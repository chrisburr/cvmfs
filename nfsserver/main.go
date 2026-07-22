package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	nfs "github.com/willscott/go-nfs"
)

const journalSyncInterval = 5 * time.Second

// optionList collects repeatable -cvmfs-opt KEY=VALUE flags.
type optionList map[string]string

func (o optionList) String() string { return fmt.Sprint(map[string]string(o)) }

func (o optionList) Set(kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok || k == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", kv)
	}
	o[k] = v
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("cvmfs_nfs_server: ")

	var (
		listen    = flag.String("listen", "127.0.0.1:11149", "NFS listen address (keep on loopback)")
		repos     = flag.String("repos", "", "comma-separated fully qualified repository names (required)")
		keys      = flag.String("keys", "", "public key location: a directory (CVMFS_KEYS_DIR) or colon-separated key files (CVMFS_PUBLIC_KEY)")
		serverURL = flag.String("server-url", "http://cvmfs-stratum-one.cern.ch/cvmfs/@fqrn@", "CVMFS_SERVER_URL template; @fqrn@ is replaced per repository")
		proxy     = flag.String("proxy", "DIRECT", "CVMFS_HTTP_PROXY")
		cacheDir  = flag.String("cache", "", "cache directory (required)")
		stateDir  = flag.String("state", "", "state directory for the handle journal (default: <cache>/nfs-state)")
		uid       = flag.Uint("uid", uint(os.Getuid()), "uid to report as file owner")
		gid       = flag.Uint("gid", uint(os.Getgid()), "gid to report as file group")
		remount   = flag.Duration("remount", 4*time.Minute, "catalog refresh interval (0 disables)")
		cvmfsLog  = flag.Bool("libcvmfs-log", true, "route libcvmfs log output through this process' log")
	)
	cvmfsOpts := optionList{}
	flag.Var(cvmfsOpts, "cvmfs-opt", "extra CVMFS_* option as KEY=VALUE (repeatable, applied globally and per repository)")
	flag.Parse()

	if *repos == "" {
		log.Fatal("-repos is required")
	}
	if *cacheDir == "" {
		log.Fatal("-cache is required")
	}
	if *stateDir == "" {
		*stateDir = filepath.Join(*cacheDir, "nfs-state")
	}
	for _, d := range []string{*cacheDir, *stateDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			log.Fatalf("cannot create %s: %v", d, err)
		}
	}
	repoNames := strings.Split(*repos, ",")
	for i, n := range repoNames {
		repoNames[i] = strings.TrimSpace(n)
	}

	if *cvmfsLog {
		InstallLogRelay()
	}
	if err := InitGlobal(*cacheDir, cvmfsOpts); err != nil {
		log.Fatalf("libcvmfs init failed: %v", err)
	}

	imap, err := openInodeMap(filepath.Join(*stateDir, "handles.journal"))
	if err != nil {
		log.Fatalf("cannot open handle journal: %v", err)
	}

	overrides := func(fqrn string) map[string]string {
		o := map[string]string{
			"CVMFS_SERVER_URL": *serverURL,
			"CVMFS_HTTP_PROXY": *proxy,
		}
		if *keys != "" {
			if st, err := os.Stat(*keys); err == nil && st.IsDir() {
				o["CVMFS_KEYS_DIR"] = *keys
			} else {
				o["CVMFS_PUBLIC_KEY"] = *keys
			}
		}
		for k, v := range cvmfsOpts {
			o[k] = v
		}
		return o
	}
	mgr := newRepoManager(repoNames, overrides)

	root := &cvmfsFS{
		mgr:   mgr,
		imap:  imap,
		uid:   uint32(*uid),
		gid:   uint32(*gid),
		start: time.Now(),
	}
	handler := &cvmfsHandler{root: root, imap: imap}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	log.Printf("exporting %s over NFSv3 on %s", strings.Join(mgr.Names(), ", "), listener.Addr())
	log.Printf("mount with: mount_nfs -o ro,nolocks,nosuid,vers=3,tcp,port=%d,mountport=%d localhost:/ <mountpoint>", port, port)

	serveErr := make(chan error, 1)
	go func() { serveErr <- nfs.Serve(listener, handler) }()

	tickers := []*time.Ticker{time.NewTicker(journalSyncInterval)}
	journalTick := tickers[0].C
	var remountTick <-chan time.Time
	if *remount > 0 {
		t := time.NewTicker(*remount)
		tickers = append(tickers, t)
		remountTick = t.C
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	shuttingDown := false
	for !shuttingDown {
		select {
		case <-journalTick:
			if err := imap.Sync(); err != nil {
				log.Printf("journal sync: %v", err)
			}
		case <-remountTick:
			mgr.RemountAll()
		case sig := <-sigs:
			log.Printf("received %s, shutting down", sig)
			shuttingDown = true
		case err := <-serveErr:
			log.Fatalf("NFS server failed: %v", err)
		}
	}

	for _, t := range tickers {
		t.Stop()
	}
	listener.Close()
	if err := <-serveErr; err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("NFS server: %v", err)
	}
	mgr.DetachAll()
	FiniGlobal()
	if err := imap.Close(); err != nil {
		log.Printf("closing handle journal: %v", err)
	}
	log.Print("shutdown complete")
}
