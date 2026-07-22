// Persistent inode/path map backing NFS file handles.
//
// NFS clients expect file handles to remain valid indefinitely: across
// catalog reloads, daemon restarts and client reconnects (e.g. after laptop
// sleep).  Handles therefore encode a small integer inode whose mapping to a
// path is persisted in an append-only journal, in the spirit of the FUSE
// client's nfs_maps.  Mappings are write-once: a path keeps its inode
// forever, and an inode never changes meaning.  Paths that disappear from the
// repository simply yield ENOENT when their handle is used.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

var journalMagic = []byte("CVMFSNFSHANDLES\x01")

const maxJournalPath = 64 * 1024

type inodeMap struct {
	mu     sync.RWMutex
	byPath map[string]uint64
	byIno  map[uint64]string
	next   uint64

	journal    *os.File
	journalErr bool // logged once; the map degrades to in-memory only
}

// openInodeMap loads (or creates) the journal at file and replays it.  A torn
// final record — from a crash mid-append — is discarded and truncated away.
func openInodeMap(file string) (*inodeMap, error) {
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	m := &inodeMap{
		byPath:  make(map[string]uint64),
		byIno:   make(map[uint64]string),
		next:    1,
		journal: f,
	}
	good, err := m.replay()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("handle journal %s corrupt: %w", file, err)
	}
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return m, nil
}

// replay loads all complete records and returns the offset of the last one.
// An empty file gets the magic header written; a file with a wrong header is
// an error (never silently discard someone's journal).
func (m *inodeMap) replay() (int64, error) {
	hdr := make([]byte, len(journalMagic))
	n, err := io.ReadFull(m.journal, hdr)
	if err == io.EOF && n == 0 {
		if _, err := m.journal.Write(journalMagic); err != nil {
			return 0, err
		}
		return int64(len(journalMagic)), nil
	}
	if err != nil || string(hdr) != string(journalMagic) {
		return 0, fmt.Errorf("bad journal header")
	}
	good := int64(len(journalMagic))
	rec := make([]byte, 12)
	for {
		if _, err := io.ReadFull(m.journal, rec); err != nil {
			return good, nil // EOF or torn length prefix
		}
		ino := binary.LittleEndian.Uint64(rec[0:8])
		plen := binary.LittleEndian.Uint32(rec[8:12])
		if plen > maxJournalPath {
			return 0, fmt.Errorf("implausible path length %d at offset %d", plen, good)
		}
		p := make([]byte, plen)
		if _, err := io.ReadFull(m.journal, p); err != nil {
			return good, nil // torn payload
		}
		m.byPath[string(p)] = ino
		m.byIno[ino] = string(p)
		if ino >= m.next {
			m.next = ino + 1
		}
		good += int64(12 + int(plen))
	}
}

// Ino returns the stable inode for an absolute path, allocating and
// journaling a new one on first sight.  Journal write failures are logged
// once and do not fail the lookup: handles then only last until restart,
// which degrades gracefully rather than taking the mount down.
func (m *inodeMap) Ino(path string) uint64 {
	m.mu.RLock()
	ino, ok := m.byPath[path]
	m.mu.RUnlock()
	if ok {
		return ino
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if ino, ok = m.byPath[path]; ok {
		return ino
	}
	ino = m.next
	m.next++
	m.byPath[path] = ino
	m.byIno[ino] = path

	rec := make([]byte, 12+len(path))
	binary.LittleEndian.PutUint64(rec[0:8], ino)
	binary.LittleEndian.PutUint32(rec[8:12], uint32(len(path)))
	copy(rec[12:], path)
	if _, err := m.journal.Write(rec); err != nil && !m.journalErr {
		m.journalErr = true
		log.Printf("handle journal write failed, handles will not survive restart: %v", err)
	}
	return ino
}

// PathOf resolves an inode back to its path.
func (m *inodeMap) PathOf(ino uint64) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.byIno[ino]
	return p, ok
}

// Sync flushes the journal to disk.  Called periodically and on shutdown; a
// crash in between at worst loses handles allocated since the last sync.
func (m *inodeMap) Sync() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.journal.Sync()
}

func (m *inodeMap) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal.Sync()
	return m.journal.Close()
}
