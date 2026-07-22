// Repository lifecycle and per-repository caches.
//
// NFSv3 is stateless: every READ arrives as (handle, offset) with no open
// state, and go-nfs opens the file for each RPC.  The fd cache keeps libcvmfs
// descriptors alive across those calls so a sequential read of a large file
// does not pay a catalog lookup per 32k block.  The readdir cache similarly
// absorbs the per-cookie re-listing the protocol requires on large
// directories.  Both are invalidated by catalog generation, so a remount
// makes new content visible no later than the client's attribute cache
// timeout.
package main

import (
	"log"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	fdCacheCap     = 128
	dirCacheCap    = 64
	dirCacheTTL    = 2 * time.Second
	attachBackoff  = 30 * time.Second
	remountJitterN = 16 // spread remounts of many repos inside one tick
)

// fdEntry is a cached libcvmfs descriptor.  refs counts open billy files;
// entries are only evicted or invalidated once unreferenced.
type fdEntry struct {
	path    string
	fd      int
	gen     uint64
	refs    int
	lastUse time.Time
	dead    bool // evicted or stale; close when refs drops to zero
}

type fdCache struct {
	mu      sync.Mutex
	entries map[string]*fdEntry
}

// AcquireFD returns a cached descriptor for path, opening one if needed.
// Release with ReleaseFD.
func (r *Repo) AcquireFD(path string) (*fdEntry, error) {
	gen := r.gen.Load()
	r.fds.mu.Lock()
	if e, ok := r.fds.entries[path]; ok && e.gen == gen {
		e.refs++
		e.lastUse = time.Now()
		r.fds.mu.Unlock()
		return e, nil
	}
	r.fds.mu.Unlock()

	fd, err := r.openRaw(path) // potentially slow: network fetch
	if err != nil {
		return nil, err
	}

	r.fds.mu.Lock()
	defer r.fds.mu.Unlock()
	if e, ok := r.fds.entries[path]; ok {
		if e.gen == gen {
			// Lost a race with another opener; keep theirs.
			e.refs++
			e.lastUse = time.Now()
			r.closeRaw(fd)
			return e, nil
		}
		r.dropLocked(e)
	}
	e := &fdEntry{path: path, fd: fd, gen: gen, refs: 1, lastUse: time.Now()}
	r.fds.entries[path] = e
	r.evictLocked()
	return e, nil
}

// ReleaseFD undoes AcquireFD.  The descriptor stays cached for reuse unless
// it was marked dead while referenced.
func (r *Repo) ReleaseFD(e *fdEntry) {
	r.fds.mu.Lock()
	defer r.fds.mu.Unlock()
	e.refs--
	e.lastUse = time.Now()
	if e.dead && e.refs == 0 {
		r.closeRaw(e.fd)
	}
}

// dropLocked removes an entry from the cache, closing it now or, if still
// referenced, when the last reference is released.
func (r *Repo) dropLocked(e *fdEntry) {
	delete(r.fds.entries, e.path)
	if e.refs == 0 {
		r.closeRaw(e.fd)
	} else {
		e.dead = true
	}
}

// evictLocked bounds the cache to fdCacheCap unreferenced-oldest-first.
// Referenced entries are never evicted, so the cache may transiently exceed
// the cap under heavy parallel load.
func (r *Repo) evictLocked() {
	for len(r.fds.entries) > fdCacheCap {
		var oldest *fdEntry
		for _, e := range r.fds.entries {
			if e.refs == 0 && (oldest == nil || e.lastUse.Before(oldest.lastUse)) {
				oldest = e
			}
		}
		if oldest == nil {
			return
		}
		r.dropLocked(oldest)
	}
}

func (c *fdCache) closeAll(r *Repo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		r.closeRaw(e.fd)
	}
	c.entries = make(map[string]*fdEntry)
}

type dirCacheEntry struct {
	infos   []DirEntry
	gen     uint64
	expires time.Time
}

type dirCache struct {
	mu      sync.Mutex
	entries map[string]dirCacheEntry
}

// Listdir returns directory entries, served from a short-lived cache.
func (r *Repo) Listdir(path string) ([]DirEntry, error) {
	gen := r.gen.Load()
	now := time.Now()
	r.dirs.mu.Lock()
	if e, ok := r.dirs.entries[path]; ok && e.gen == gen && now.Before(e.expires) {
		r.dirs.mu.Unlock()
		return e.infos, nil
	}
	r.dirs.mu.Unlock()

	infos, err := r.listdir(path)
	if err != nil {
		return nil, err
	}

	r.dirs.mu.Lock()
	defer r.dirs.mu.Unlock()
	if len(r.dirs.entries) >= dirCacheCap {
		for k, e := range r.dirs.entries {
			if e.gen != gen || now.After(e.expires) {
				delete(r.dirs.entries, k)
			}
		}
		if len(r.dirs.entries) >= dirCacheCap {
			// Still full of live entries: drop an arbitrary one.
			for k := range r.dirs.entries {
				delete(r.dirs.entries, k)
				break
			}
		}
	}
	r.dirs.entries[path] = dirCacheEntry{infos: infos, gen: gen, expires: now.Add(dirCacheTTL)}
	return infos, nil
}

// managedRepo tracks attach state for one configured repository.  Attach
// failures are cached with a backoff so a broken network does not turn every
// NFS lookup into a fresh multi-second attach attempt.
type managedRepo struct {
	mu      sync.Mutex
	repo    *Repo
	lastErr error
	nextTry time.Time
}

type repoManager struct {
	repos     map[string]*managedRepo
	overrides func(fqrn string) map[string]string
}

func newRepoManager(names []string, overrides func(fqrn string) map[string]string) *repoManager {
	m := &repoManager{
		repos:     make(map[string]*managedRepo, len(names)),
		overrides: overrides,
	}
	for _, n := range names {
		m.repos[n] = &managedRepo{}
	}
	return m
}

// Names returns the configured repository names, sorted.
func (m *repoManager) Names() []string {
	names := make([]string, 0, len(m.repos))
	for n := range m.repos {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Configured reports whether name is a known repository.
func (m *repoManager) Configured(name string) bool {
	_, ok := m.repos[name]
	return ok
}

// Peek returns the repository if it is already attached, without attaching.
func (m *repoManager) Peek(name string) *Repo {
	mr, ok := m.repos[name]
	if !ok {
		return nil
	}
	mr.mu.Lock()
	defer mr.mu.Unlock()
	return mr.repo
}

// Get returns the repository, attaching it on first use.  Concurrent callers
// for the same repository serialize on the attach.
func (m *repoManager) Get(name string) (*Repo, error) {
	mr, ok := m.repos[name]
	if !ok {
		return nil, pathErr("attach", name, syscall.ENOENT)
	}
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if mr.repo != nil {
		return mr.repo, nil
	}
	if time.Now().Before(mr.nextTry) {
		return nil, mr.lastErr
	}
	log.Printf("attaching %s", name)
	repo, err := Attach(name, m.overrides(name))
	if err != nil {
		mr.lastErr = pathErr("attach", name, syscall.EIO)
		mr.nextTry = time.Now().Add(attachBackoff)
		log.Printf("attach %s failed (retry in %s): %v", name, attachBackoff, err)
		return nil, mr.lastErr
	}
	log.Printf("attached %s", name)
	mr.repo = repo
	mr.lastErr = nil
	return repo, nil
}

// RemountAll refreshes catalogs of all attached repositories.
func (m *repoManager) RemountAll() {
	for _, name := range m.Names() {
		if repo := m.Peek(name); repo != nil {
			if err := repo.Remount(); err != nil {
				log.Printf("remount: %v", err)
			}
		}
	}
}

// DetachAll unmounts everything; the manager must not be used afterwards.
func (m *repoManager) DetachAll() {
	for _, name := range m.Names() {
		mr := m.repos[name]
		mr.mu.Lock()
		if mr.repo != nil {
			mr.repo.Detach()
			mr.repo = nil
		}
		mr.mu.Unlock()
	}
}
