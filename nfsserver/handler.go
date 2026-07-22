// nfs.Handler implementation with persistent file handles.
//
// Unlike go-nfs's CachingHandler (an in-memory LRU whose evictions and
// restarts invalidate handles), handles here are 9 bytes — a version byte and
// the inode from the persistent inodeMap — so they stay valid across daemon
// restarts, catalog reloads and cache pressure.  This is what makes client
// reconnects after sleep/wake or a server upgrade safe.
package main

import (
	"context"
	"encoding/binary"
	"math"
	"net"
	"os"
	"strings"

	"github.com/go-git/go-billy/v5"
	nfs "github.com/willscott/go-nfs"
)

const handleVersion = 1

type cvmfsHandler struct {
	root *cvmfsFS
	imap *inodeMap
}

var _ nfs.Handler = (*cvmfsHandler)(nil)

// Mount honors the requested export path so that both `mount localhost:/`
// (all repositories) and `mount localhost:/repo.name` (a single repository)
// work.
func (h *cvmfsHandler) Mount(_ context.Context, _ net.Conn, req nfs.MountRequest) (
	nfs.MountStatus, billy.Filesystem, []nfs.AuthFlavor) {
	view, err := h.root.chrootView(string(req.Dirpath))
	if err != nil {
		return nfs.MountStatusErrNoEnt, nil, nil
	}
	return nfs.MountStatusOk, view, []nfs.AuthFlavor{nfs.AuthFlavorNull}
}

// Change returns nil: the filesystem is read-only and all setattr-class
// operations fail with ROFS.
func (h *cvmfsHandler) Change(billy.Filesystem) billy.Change { return nil }

func (h *cvmfsHandler) FSStat(_ context.Context, _ billy.Filesystem, s *nfs.FSStat) error {
	s.TotalSize = 1 << 60
	s.FreeSize = 0
	s.AvailableSize = 0
	s.TotalFiles = 1 << 40
	s.FreeFiles = 0
	s.AvailableFiles = 0
	return nil
}

// Handles are 16 bytes: a version byte, 7 reserved bytes, and the persistent
// inode.  The length is fixed and 4-byte aligned; reserved bytes leave room
// for e.g. a generation counter without a wire-format break.
func (h *cvmfsHandler) ToHandle(f billy.Filesystem, path []string) []byte {
	view, ok := f.(*cvmfsFS)
	if !ok {
		return nil
	}
	abs := view.full(strings.Join(path, "/"))
	fh := make([]byte, 16)
	fh[0] = handleVersion
	binary.BigEndian.PutUint64(fh[8:], h.imap.Ino(abs))
	return fh
}

// FromHandle always resolves against the root view: handles encode absolute
// paths, independent of which subtree the client mounted.
func (h *cvmfsHandler) FromHandle(fh []byte) (billy.Filesystem, []string, error) {
	if len(fh) != 16 || fh[0] != handleVersion {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale}
	}
	p, ok := h.imap.PathOf(binary.BigEndian.Uint64(fh[8:]))
	if !ok {
		return nil, nil, &nfs.NFSStatusError{NFSStatus: nfs.NFSStatusStale, WrappedErr: os.ErrNotExist}
	}
	if p == "/" {
		return h.root, []string{}, nil
	}
	return h.root, strings.Split(strings.TrimPrefix(p, "/"), "/"), nil
}

// InvalidateHandle is a no-op: mappings are write-once and remain valid; a
// vanished path is reported as ENOENT/ESTALE when the handle is used.
func (h *cvmfsHandler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

func (h *cvmfsHandler) HandleLimit() int { return math.MaxInt32 }
