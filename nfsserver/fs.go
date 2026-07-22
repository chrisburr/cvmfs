// Read-only billy.Filesystem over the full set of configured repositories.
//
// The root directory lists every configured repository; repository subtrees
// delegate to libcvmfs.  Looking up or statting a repository's top-level
// directory serves a synthetic entry without attaching, so `ls -l /cvmfs`
// does not trigger attach storms — repositories attach on first descent, like
// autofs on Linux.
//
// The inode presented to clients (and used in file handles) is always the
// persistent one from inodeMap, never the catalog inode: catalog inodes
// change with reloads, persistent ones never do.  Ownership is reported as
// the configured claim uid/gid, mirroring the FUSE client's behavior of not
// trusting catalog uids on foreign machines.
package main

import (
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
)

var errReadOnly error = syscall.EROFS

// debugOps enables per-operation logging via CVMFSNFS_DEBUG=1.
var debugOps = os.Getenv("CVMFSNFS_DEBUG") != ""

func dbg(format string, args ...interface{}) {
	if debugOps {
		log.Printf("op: "+format, args...)
	}
}

type cvmfsFS struct {
	mgr   *repoManager
	imap  *inodeMap
	uid   uint32
	gid   uint32
	start time.Time
	// prefix restricts the view to a subtree ("" for the full root); it is
	// always absolute and clean, e.g. "/sft.cern.ch".
	prefix string
}

var (
	_ billy.Filesystem = (*cvmfsFS)(nil)
	_ billy.Capable    = (*cvmfsFS)(nil)
)

// full maps a billy path to the absolute exported path.  Cleaning inside the
// absolute namespace first means no name can escape the chroot prefix.
func (f *cvmfsFS) full(name string) string {
	abs := path.Clean("/" + name)
	if f.prefix == "" {
		return abs
	}
	if abs == "/" {
		return f.prefix
	}
	return f.prefix + abs
}

// chrootView returns the filesystem restricted to dir, validating that it
// exists.  Used by MOUNT so single-repository exports work.
func (f *cvmfsFS) chrootView(dir string) (*cvmfsFS, error) {
	abs := path.Clean("/" + strings.Trim(dir, "/"))
	if abs == "/" {
		return f, nil
	}
	view := &cvmfsFS{mgr: f.mgr, imap: f.imap, uid: f.uid, gid: f.gid,
		start: f.start, prefix: abs}
	if _, err := view.Lstat(""); err != nil {
		return nil, err
	}
	return view, nil
}

// target is the result of resolving an exported path.
type target struct {
	isRoot   bool   // the export root itself
	isRepoRt bool   // a repository's top-level directory
	repoName string // set unless isRoot
	sub      string // in-repo path ("/..." with isRepoRt meaning "/")
}

func splitExported(abs string) target {
	if abs == "/" {
		return target{isRoot: true}
	}
	rest := abs[1:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return target{repoName: rest[:i], sub: rest[i:]}
	}
	return target{isRepoRt: true, repoName: rest, sub: "/"}
}

// repoFor resolves the repository of a target, attaching it on first use.
func (f *cvmfsFS) repoFor(t target, op, name string) (*Repo, error) {
	if !f.mgr.Configured(t.repoName) {
		return nil, pathErr(op, name, syscall.ENOENT)
	}
	repo, err := f.mgr.Get(t.repoName)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

// syntheticDir fabricates the stat of the export root and of unattached
// repository directories.  The mtime moves on daemon restart, which at worst
// refreshes client directory caches.
func (f *cvmfsFS) syntheticDir(abs string) syscall.Stat_t {
	return syscall.Stat_t{
		Ino:       f.imap.Ino(abs),
		Mode:      syscall.S_IFDIR | 0555,
		Nlink:     2,
		Uid:       f.uid,
		Gid:       f.gid,
		Size:      4096,
		Mtimespec: syscall.NsecToTimespec(f.start.UnixNano()),
		Atimespec: syscall.NsecToTimespec(f.start.UnixNano()),
		Ctimespec: syscall.NsecToTimespec(f.start.UnixNano()),
	}
}

// present rewrites a catalog stat for export: persistent inode, claimed
// ownership.
func (f *cvmfsFS) present(abs string, st syscall.Stat_t) syscall.Stat_t {
	st.Ino = f.imap.Ino(abs)
	st.Uid = f.uid
	st.Gid = f.gid
	return st
}

func (f *cvmfsFS) statAt(name string, follow bool) (fi os.FileInfo, err error) {
	defer func() { dbg("statAt(%q, follow=%v) err=%v", name, follow, err) }()
	abs := f.full(name)
	t := splitExported(abs)
	base := path.Base(abs)
	switch {
	case t.isRoot:
		return &fileInfo{name: "/", st: f.syntheticDir("/")}, nil
	case t.isRepoRt:
		if !f.mgr.Configured(t.repoName) {
			return nil, pathErr("stat", name, syscall.ENOENT)
		}
		if repo := f.mgr.Peek(t.repoName); repo != nil {
			if st, err := repo.Lstat("/"); err == nil {
				return &fileInfo{name: base, st: f.present(abs, st)}, nil
			}
		}
		return &fileInfo{name: base, st: f.syntheticDir(abs)}, nil
	default:
		repo, err := f.repoFor(t, "stat", name)
		if err != nil {
			return nil, err
		}
		var st syscall.Stat_t
		if follow {
			st, err = repo.Stat(t.sub)
		} else {
			st, err = repo.Lstat(t.sub)
		}
		if err != nil {
			return nil, err
		}
		return &fileInfo{name: base, st: f.present(abs, st)}, nil
	}
}

func (f *cvmfsFS) Stat(name string) (os.FileInfo, error)  { return f.statAt(name, true) }
func (f *cvmfsFS) Lstat(name string) (os.FileInfo, error) { return f.statAt(name, false) }

func (f *cvmfsFS) ReadDir(name string) (infos []os.FileInfo, err error) {
	defer func() { dbg("ReadDir(%q) n=%d err=%v", name, len(infos), err) }()
	abs := f.full(name)
	t := splitExported(abs)
	if t.isRoot {
		names := f.mgr.Names()
		infos := make([]os.FileInfo, 0, len(names))
		for _, n := range names {
			infos = append(infos, &fileInfo{name: n, st: f.syntheticDir("/" + n)})
		}
		return infos, nil
	}
	repo, err := f.repoFor(t, "readdir", name)
	if err != nil {
		return nil, err
	}
	entries, err := repo.Listdir(t.sub)
	if err != nil {
		return nil, err
	}
	dir := abs
	if dir == "/" {
		dir = ""
	}
	infos = make([]os.FileInfo, len(entries))
	for i, e := range entries {
		infos[i] = &fileInfo{name: e.Name, st: f.present(dir+"/"+e.Name, e.Stat)}
	}
	return infos, nil
}

func (f *cvmfsFS) Readlink(link string) (string, error) {
	abs := f.full(link)
	t := splitExported(abs)
	if t.isRoot || t.isRepoRt {
		return "", pathErr("readlink", link, syscall.EINVAL)
	}
	repo, err := f.repoFor(t, "readlink", link)
	if err != nil {
		return "", err
	}
	return repo.Readlink(t.sub)
}

func (f *cvmfsFS) Open(name string) (billy.File, error) {
	return f.OpenFile(name, os.O_RDONLY, 0)
}

func (f *cvmfsFS) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_CREATE|os.O_TRUNC) != 0 {
		return nil, errReadOnly
	}
	abs := f.full(name)
	t := splitExported(abs)
	if t.isRoot || t.isRepoRt {
		return nil, pathErr("open", name, syscall.EISDIR)
	}
	repo, err := f.repoFor(t, "open", name)
	if err != nil {
		return nil, err
	}
	entry, err := repo.AcquireFD(t.sub)
	if err != nil {
		return nil, err
	}
	return &cvmfsFile{repo: repo, name: name, sub: t.sub, entry: entry}, nil
}

func (f *cvmfsFS) Join(elem ...string) string { return path.Join(elem...) }

func (f *cvmfsFS) Chroot(p string) (billy.Filesystem, error) { return f.chrootView(f.full(p)) }

func (f *cvmfsFS) Root() string {
	if f.prefix == "" {
		return "/"
	}
	return f.prefix
}

func (f *cvmfsFS) Capabilities() billy.Capability {
	return billy.ReadCapability | billy.SeekCapability
}

func (f *cvmfsFS) Create(string) (billy.File, error)           { return nil, errReadOnly }
func (f *cvmfsFS) Rename(string, string) error                 { return errReadOnly }
func (f *cvmfsFS) Remove(string) error                         { return errReadOnly }
func (f *cvmfsFS) TempFile(string, string) (billy.File, error) { return nil, errReadOnly }
func (f *cvmfsFS) MkdirAll(string, os.FileMode) error          { return errReadOnly }
func (f *cvmfsFS) Symlink(string, string) error                { return errReadOnly }

type cvmfsFile struct {
	repo  *Repo
	name  string
	sub   string
	entry *fdEntry

	mu     sync.Mutex
	off    int64
	closed bool
}

var _ billy.File = (*cvmfsFile)(nil)

func (f *cvmfsFile) Name() string { return f.name }

func (f *cvmfsFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *cvmfsFile) ReadAt(p []byte, off int64) (int, error) {
	return f.readAt(p, off)
}

// readAt fills p completely unless EOF is hit, per the io.ReaderAt contract.
func (f *cvmfsFile) readAt(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		n, err := f.repo.Pread(f.entry.fd, p[total:], off+int64(total))
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.EOF
		}
		total += n
	}
	return total, nil
}

func (f *cvmfsFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	case io.SeekEnd:
		st, err := f.repo.Stat(f.sub)
		if err != nil {
			return f.off, err
		}
		f.off = st.Size + offset
	default:
		return f.off, syscall.EINVAL
	}
	return f.off, nil
}

func (f *cvmfsFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.repo.ReleaseFD(f.entry)
	return nil
}

func (f *cvmfsFile) Write([]byte) (int, error) { return 0, errReadOnly }
func (f *cvmfsFile) Truncate(int64) error      { return errReadOnly }
func (f *cvmfsFile) Lock() error               { return nil }
func (f *cvmfsFile) Unlock() error             { return nil }

// fileInfo adapts a syscall.Stat_t to os.FileInfo.  Sys() exposes the Stat_t
// so go-nfs propagates the persistent inode, ownership and link count.
type fileInfo struct {
	name string
	st   syscall.Stat_t
}

var _ os.FileInfo = (*fileInfo)(nil)

func (fi *fileInfo) Name() string { return fi.name }
func (fi *fileInfo) Size() int64  { return fi.st.Size }

func (fi *fileInfo) Mode() os.FileMode {
	mode := os.FileMode(fi.st.Mode & 0777)
	switch fi.st.Mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	case syscall.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	}
	return mode
}

func (fi *fileInfo) ModTime() time.Time {
	return time.Unix(fi.st.Mtimespec.Sec, fi.st.Mtimespec.Nsec)
}

func (fi *fileInfo) IsDir() bool      { return fi.Mode().IsDir() }
func (fi *fileInfo) Sys() interface{} { return &fi.st }
