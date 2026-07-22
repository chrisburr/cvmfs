package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestInodeMapRoundtrip(t *testing.T) {
	m, err := openInodeMap(filepath.Join(t.TempDir(), "j"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	a := m.Ino("/repo/a")
	b := m.Ino("/repo/b")
	if a == b {
		t.Fatalf("distinct paths share inode %d", a)
	}
	if again := m.Ino("/repo/a"); again != a {
		t.Fatalf("inode not stable: %d then %d", a, again)
	}
	p, ok := m.PathOf(a)
	if !ok || p != "/repo/a" {
		t.Fatalf("PathOf(%d) = %q, %v", a, p, ok)
	}
	if _, ok := m.PathOf(99999); ok {
		t.Fatal("unknown inode resolved")
	}
}

func TestInodeMapPersistence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "j")
	m, err := openInodeMap(file)
	if err != nil {
		t.Fatal(err)
	}
	a := m.Ino("/x")
	b := m.Ino("/y/z")
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	m2, err := openInodeMap(file)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if got := m2.Ino("/x"); got != a {
		t.Fatalf("inode for /x changed across restart: %d then %d", a, got)
	}
	if p, ok := m2.PathOf(b); !ok || p != "/y/z" {
		t.Fatalf("PathOf(%d) = %q, %v after restart", b, p, ok)
	}
	if c := m2.Ino("/new"); c == a || c == b {
		t.Fatalf("new path reused inode %d", c)
	}
}

func TestInodeMapTornTail(t *testing.T) {
	file := filepath.Join(t.TempDir(), "j")
	m, err := openInodeMap(file)
	if err != nil {
		t.Fatal(err)
	}
	a := m.Ino("/kept")
	m.Close()

	// Simulate a crash mid-append: a length prefix promising more bytes than
	// are present.
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{9, 0, 0, 0, 0, 0, 0, 0, 200, 0, 0, 0, 'x'}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	m2, err := openInodeMap(file)
	if err != nil {
		t.Fatalf("torn tail not tolerated: %v", err)
	}
	defer m2.Close()
	if got := m2.Ino("/kept"); got != a {
		t.Fatalf("intact record lost: %d then %d", a, got)
	}
	if _, ok := m2.PathOf(9); ok {
		t.Fatal("torn record was resurrected")
	}
}

func TestInodeMapRejectsForeignFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "j")
	if err := os.WriteFile(file, []byte("not a journal at all"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openInodeMap(file); err == nil {
		t.Fatal("foreign file accepted as journal")
	}
}

func TestInodeMapConcurrent(t *testing.T) {
	m, err := openInodeMap(filepath.Join(t.TempDir(), "j"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	const workers = 16
	inos := make([]uint64, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Ino("/unique/" + string(rune('a'+i)))
			}
			inos[i] = m.Ino("/shared")
		}(i)
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if inos[i] != inos[0] {
			t.Fatalf("concurrent Ino(/shared) disagreed: %d vs %d", inos[0], inos[i])
		}
	}
}

func TestSplitExported(t *testing.T) {
	cases := []struct {
		abs  string
		want target
	}{
		{"/", target{isRoot: true}},
		{"/sft.cern.ch", target{isRepoRt: true, repoName: "sft.cern.ch", sub: "/"}},
		{"/sft.cern.ch/lcg", target{repoName: "sft.cern.ch", sub: "/lcg"}},
		{"/sft.cern.ch/lcg/releases", target{repoName: "sft.cern.ch", sub: "/lcg/releases"}},
	}
	for _, c := range cases {
		if got := splitExported(c.abs); got != c.want {
			t.Errorf("splitExported(%q) = %+v, want %+v", c.abs, got, c.want)
		}
	}
}

func TestFullCannotEscapeChroot(t *testing.T) {
	f := &cvmfsFS{prefix: "/sft.cern.ch"}
	for _, name := range []string{"../lhcb.cern.ch/x", "a/../../etc", "/.."} {
		got := f.full(name)
		if got != "/sft.cern.ch" && !strings.HasPrefix(got, "/sft.cern.ch/") {
			t.Errorf("full(%q) escaped chroot: %q", name, got)
		}
	}
}
