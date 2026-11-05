package store

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The share is reopened (a remount, or the folder made again) while an
// operation holds the old directory's descriptor number: the operation
// still resolves in the share it started in. Closing the old descriptor
// under it gave EBADF, or, once the number was reused by another folder,
// a lookup relative to that folder (review 2026-11-05).
func TestReopenDuringOperation(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	share, other := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(share, "x"), []byte("share"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "x"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openShareFS(share)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.beneath {
		t.Skip("no openat2 here")
	}
	var squatters []int
	s.afterDirfd = func() {
		s.afterDirfd = nil
		if err := s.open(); err != nil { // the share path now names a new directory
			t.Error(err)
		}
		// Take the lowest free descriptor number: the one the old
		// directory had, if it was closed.
		for range 2 { // the old os.Root's and the old directory's
			fd, err := unix.Open(other, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				t.Error(err)
			}
			squatters = append(squatters, fd)
		}
	}
	f, err := s.Open(filepath.Join(share, "x"))
	defer func() {
		for _, fd := range squatters {
			unix.Close(fd)
		}
	}()
	if err != nil {
		t.Fatalf("open during a reopen: %v", err)
	}
	defer f.Close()
	b := make([]byte, 16)
	n, _ := f.Read(b)
	if string(b[:n]) != "share" {
		t.Fatalf("read %q: the lookup left the share", b[:n])
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc")
	}
	return len(ents)
}

// A reopened share does not pile up descriptors, and Close gives back all
// it holds, more than once without harm.
func TestCloseReleasesDescriptors(t *testing.T) {
	share := t.TempDir()
	before := openFDs(t)
	s, err := openShareFS(share)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if err := s.open(); err != nil {
			t.Fatal(err)
		}
	}
	if held := openFDs(t) - before; held > 2+retiredMax*2 {
		t.Errorf("20 reopens hold %d descriptors, want at most %d", held, 2+retiredMax*2)
	}
	s.Close()
	s.Close()
	if held := openFDs(t) - before; held != 0 {
		t.Errorf("after Close %d descriptors are still open", held)
	}
}

// Dir.Close releases the share, and Open that fails after the share was
// opened (a protocol mismatch) does not keep it.
func TestDirCloseAndFailedOpen(t *testing.T) {
	share := t.TempDir()
	before := openFDs(t)
	m, err := Open(share, "n1", Options{})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	m.Close()
	if held := openFDs(t) - before; held != 0 {
		t.Errorf("after Dir.Close %d descriptors are still open", held)
	}
	if err := os.WriteFile(filepath.Join(share, "protocol-version"), []byte("999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(share, "n1", Options{}); err == nil {
		t.Fatal("a share of another protocol opened")
	}
	if held := openFDs(t) - before; held != 0 {
		t.Errorf("a refused Open left %d descriptors open", held)
	}
}
