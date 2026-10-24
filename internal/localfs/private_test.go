package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The default state dir has a name others can guess; one they made
// first must be refused (security audit 2026-10-22).
func TestPrivateDir(t *testing.T) {
	top := t.TempDir()
	d := filepath.Join(top, "new")
	if err := PrivateDir(d); err != nil {
		t.Fatalf("new dir: %v", err)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Errorf("new dir mode %v, want 0700", fi.Mode().Perm())
	}
	if err := PrivateDir(d); err != nil {
		t.Fatalf("existing own dir: %v", err)
	}

	open := filepath.Join(top, "open")
	os.Mkdir(open, 0o700)
	os.Chmod(open, 0o777)
	link := filepath.Join(top, "link")
	os.Symlink(d, link)
	file := filepath.Join(top, "file")
	os.WriteFile(file, nil, 0o600)
	for _, p := range []string{open, link, file} {
		if err := CheckPrivateDir(p); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("%s: err = %v, want ErrNotPrivate", filepath.Base(p), err)
		}
	}
	for _, p := range []string{open, link} {
		if err := PrivateDir(p); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("PrivateDir(%s): err = %v, want ErrNotPrivate", filepath.Base(p), err)
		}
	}
	if os.Geteuid() == 0 {
		theirs := filepath.Join(top, "theirs")
		os.Mkdir(theirs, 0o700)
		os.Chown(theirs, 65534, 65534)
		if err := PrivateDir(theirs); !errors.Is(err, ErrNotPrivate) {
			t.Errorf("someone else's dir: err = %v, want ErrNotPrivate", err)
		}
	}
}

// The socket's check names the socket and says to move it.
func TestCheckSocketDir(t *testing.T) {
	top := t.TempDir()
	open := filepath.Join(top, "open")
	os.Mkdir(open, 0o700)
	os.Chmod(open, 0o777)
	sock := filepath.Join(open, "s.sock")
	err := CheckSocketDir(sock)
	if !errors.Is(err, ErrNotPrivate) || !strings.Contains(err.Error(), sock) || strings.Contains(err.Error(), "-state") {
		t.Errorf("open folder: err = %v", err)
	}
	if err := CheckSocketDir(filepath.Join(top, "s.sock")); err != nil {
		t.Errorf("own folder: %v", err)
	}
	if err := CheckSocketDir(filepath.Join(top, "gone", "s.sock")); err == nil || errors.Is(err, ErrNotPrivate) {
		t.Errorf("missing folder: err = %v, want a plain lookup error", err)
	}
}
