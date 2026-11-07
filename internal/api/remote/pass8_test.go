package remote

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Eighth review pass (2026-11-07, testplan.md 26).

// A daemon that crashed leaves its socket file behind. The next one
// replaces it, with the mode asked for, whatever the umask.
func TestListenReplacesStaleSocket(t *testing.T) {
	dir, _ := os.MkdirTemp("", "fkbr-sock")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "api.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	old.Close() // the file stays, nobody listens: a crashed daemon
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal(err)
	}
	prev := syscall.Umask(0)
	defer syscall.Umask(prev)
	l, err := Listen(sock, 0o600)
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}
	defer l.Close()
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v (%v), want 0600 under umask 0", fi.Mode(), err)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("the new socket does not answer: %v", err)
	}
	c.Close()
	// a live one is refused, and stays
	if _, err := Listen(sock, 0o600); err == nil {
		t.Error("a second Listen on a live socket succeeded")
	}
	if c, err := net.Dial("unix", sock); err != nil {
		t.Errorf("the live socket was disturbed: %v", err)
	} else {
		c.Close()
	}
}
