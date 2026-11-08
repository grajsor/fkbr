package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A retried remove makes directories that lack the owner's access
// writable; the ones it leaves behind keep their setgid and sticky bits.
func TestOpenDirsKeepsSpecialBits(t *testing.T) {
	dir := t.TempDir()
	top := filepath.Join(dir, "top")
	sub := filepath.Join(top, "sub")
	plain := filepath.Join(top, "plain")
	for _, d := range []string{sub, plain} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// os.Chmod takes the special bits as FileMode flags.
	if err := os.Chmod(sub, 0o550|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o755|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	openDirs(root, "top")
	fi, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o700 != 0o700 {
		t.Errorf("sub is %v: the owner still lacks access", fi.Mode())
	}
	if fi.Mode()&os.ModeSetgid == 0 || fi.Mode()&os.ModeSticky == 0 {
		t.Errorf("sub is %v: setgid and sticky must stay", fi.Mode())
	}
	if fi, _ := os.Lstat(plain); fi.Mode()&os.ModeSetgid == 0 {
		t.Errorf("a directory that needed nothing was changed: %v", fi.Mode())
	}
}
