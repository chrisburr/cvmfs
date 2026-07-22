// cvmfs_nfs_server serves CernVM-FS repositories over NFSv3 from userspace,
// backed by libcvmfs.  It exists primarily for macOS, where the built-in NFS
// client provides kext-free, unprivileged mounts.
//
// This file contains all cgo bindings; C types must not leak into other
// files.  The library location is supplied at build time via CGO_LDFLAGS:
//
//	CGO_LDFLAGS="-L$BUILD/cvmfs -lcvmfs_client -Wl,-rpath,$BUILD/cvmfs"
package main

/*
#cgo CFLAGS: -I${SRCDIR}/../cvmfs
#include <stdlib.h>
#include "libcvmfs.h"

// Defined in logshim.c; routes libcvmfs log output to goCvmfsLog.
void install_cvmfs_log_relay(void);
*/
import "C"

import (
	"bytes"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sync/atomic"
	"syscall"
	"unsafe"
)

func init() {
	if unsafe.Sizeof(C.struct_stat{}) != unsafe.Sizeof(syscall.Stat_t{}) {
		panic("layout mismatch between C struct stat and syscall.Stat_t")
	}
}

//export goCvmfsLog
func goCvmfsLog(msg *C.char) {
	log.Printf("[libcvmfs] %s", C.GoString(msg))
}

// InstallLogRelay routes libcvmfs syslog/debug messages into the Go logger so
// that launchd captures a single stream.
func InstallLogRelay() {
	C.install_cvmfs_log_relay()
}

func setOption(opts *C.cvmfs_option_map, key, value string) {
	ck := C.CString(key)
	cv := C.CString(value)
	defer C.free(unsafe.Pointer(ck))
	defer C.free(unsafe.Pointer(cv))
	C.cvmfs_options_set(opts, ck, cv)
}

// InitGlobal initializes libcvmfs global state.  extra entries override the
// defaults parsed from the system configuration (/etc/cvmfs, if present).
func InitGlobal(cacheDir string, extra map[string]string) error {
	opts := C.cvmfs_options_init()
	setOption(opts, "CVMFS_CACHE_DIR", cacheDir)
	for k, v := range extra {
		setOption(opts, k, v)
	}
	if ret := C.cvmfs_init_v2(opts); ret != C.LIBCVMFS_ERR_OK {
		return fmt.Errorf("cvmfs_init_v2 failed (error %d)", int(ret))
	}
	return nil
}

// FiniGlobal tears down libcvmfs global state.  All repositories must be
// detached first.
func FiniGlobal() {
	C.cvmfs_fini()
}

// Repo wraps an attached libcvmfs repository context.  All methods are safe
// for concurrent use.  gen counts successful catalog reloads and is used by
// the fd and readdir caches to drop entries that predate the current catalog.
type Repo struct {
	name string
	ctx  *C.cvmfs_context
	gen  atomic.Uint64

	fds  fdCache
	dirs dirCache
}

// Attach mounts a repository.  Options are the system defaults for the fqrn
// (per cvmfs_options_parse_default) with overrides applied on top.
func Attach(fqrn string, overrides map[string]string) (*Repo, error) {
	opts := C.cvmfs_options_init()
	cfqrn := C.CString(fqrn)
	defer C.free(unsafe.Pointer(cfqrn))
	C.cvmfs_options_parse_default(opts, cfqrn)
	for k, v := range overrides {
		setOption(opts, k, v)
	}
	var ctx *C.cvmfs_context
	if ret := C.cvmfs_attach_repo_v2(cfqrn, opts, &ctx); ret != C.LIBCVMFS_ERR_OK {
		C.cvmfs_options_fini(opts)
		return nil, fmt.Errorf("cvmfs_attach_repo_v2(%s) failed (error %d)", fqrn, int(ret))
	}
	C.cvmfs_adopt_options(ctx, opts)
	C.cvmfs_enable_threaded(ctx)
	r := &Repo{name: fqrn, ctx: ctx}
	r.fds.entries = make(map[string]*fdEntry)
	r.dirs.entries = make(map[string]dirCacheEntry)
	return r, nil
}

// Detach closes all cached descriptors and unmounts the repository.
func (r *Repo) Detach() {
	r.fds.closeAll(r)
	C.cvmfs_detach_repo(r.ctx)
	r.ctx = nil
}

// norm turns billy-style paths ("", ".", "a/b", "/a/b") into the absolute
// in-repo form libcvmfs expects ("/", "/a/b").
func norm(name string) string {
	return path.Clean("/" + name)
}

func pathErr(op, name string, errno error) error {
	if errno == nil {
		errno = syscall.EIO
	}
	return &fs.PathError{Op: op, Path: name, Err: errno}
}

func goStat(cst *C.struct_stat) syscall.Stat_t {
	return *(*syscall.Stat_t)(unsafe.Pointer(cst))
}

// Lstat stats an in-repo path without following a terminal symlink.
func (r *Repo) Lstat(name string) (syscall.Stat_t, error) {
	cp := C.CString(norm(name))
	defer C.free(unsafe.Pointer(cp))
	var cst C.struct_stat
	ret, errno := C.cvmfs_lstat(r.ctx, cp, &cst)
	if ret != 0 {
		return syscall.Stat_t{}, pathErr("lstat", name, errno)
	}
	return goStat(&cst), nil
}

// Stat stats an in-repo path, following symlinks within the repository.
func (r *Repo) Stat(name string) (syscall.Stat_t, error) {
	cp := C.CString(norm(name))
	defer C.free(unsafe.Pointer(cp))
	var cst C.struct_stat
	ret, errno := C.cvmfs_stat(r.ctx, cp, &cst)
	if ret != 0 {
		return syscall.Stat_t{}, pathErr("stat", name, errno)
	}
	return goStat(&cst), nil
}

// Readlink returns the target of a symlink.  CVMFS variant symlinks
// ($(VAR) forms) are expanded by libcvmfs from the client configuration.
func (r *Repo) Readlink(name string) (string, error) {
	cp := C.CString(norm(name))
	defer C.free(unsafe.Pointer(cp))
	buf := make([]byte, 4096)
	ret, errno := C.cvmfs_readlink(r.ctx, cp, (*C.char)(unsafe.Pointer(&buf[0])),
		C.size_t(len(buf)))
	if ret != 0 {
		return "", pathErr("readlink", name, errno)
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}

// DirEntry is a single directory entry with its stat information.
type DirEntry struct {
	Name string
	Stat syscall.Stat_t
}

// listdir returns the entries of a directory, uncached.  Use Listdir.
func (r *Repo) listdir(name string) ([]DirEntry, error) {
	cp := C.CString(norm(name))
	defer C.free(unsafe.Pointer(cp))
	var buf *C.struct_cvmfs_stat_t
	var listlen, buflen C.size_t
	ret, errno := C.cvmfs_listdir_stat(r.ctx, cp, &buf, &listlen, &buflen)
	if ret != 0 {
		return nil, pathErr("readdir", name, errno)
	}
	defer C.free(unsafe.Pointer(buf))
	out := make([]DirEntry, 0, int(listlen))
	for _, e := range unsafe.Slice(buf, int(listlen)) {
		out = append(out, DirEntry{
			Name: C.GoString(e.name),
			Stat: goStat(&e.info),
		})
		C.free(unsafe.Pointer(e.name))
	}
	return out, nil
}

// openRaw opens an in-repo path and returns a libcvmfs descriptor.
// Descriptors of chunked files have bit 30 set; cvmfs_pread and cvmfs_close
// handle both kinds transparently.  Use AcquireFD for cached access.
func (r *Repo) openRaw(name string) (int, error) {
	cp := C.CString(norm(name))
	defer C.free(unsafe.Pointer(cp))
	fd, errno := C.cvmfs_open(r.ctx, cp)
	if fd < 0 {
		return -1, pathErr("open", name, errno)
	}
	return int(fd), nil
}

// Pread reads from a descriptor returned by openRaw/AcquireFD.
func (r *Repo) Pread(fd int, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n, errno := C.cvmfs_pread(r.ctx, C.int(fd), unsafe.Pointer(&p[0]),
		C.size_t(len(p)), C.off_t(off))
	if n < 0 {
		if errno == nil {
			errno = syscall.EIO
		}
		return 0, errno
	}
	return int(n), nil
}

// closeRaw releases a descriptor returned by openRaw.
func (r *Repo) closeRaw(fd int) {
	C.cvmfs_close(r.ctx, C.int(fd))
}

// Remount reloads the repository catalog if a new revision is available and
// invalidates the fd and readdir caches.
func (r *Repo) Remount() error {
	if ret := C.cvmfs_remount(r.ctx); ret != 0 {
		return fmt.Errorf("cvmfs_remount(%s) failed (error %d)", r.name, int(ret))
	}
	r.gen.Add(1)
	return nil
}
