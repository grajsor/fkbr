package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/api"
)

// Removing from a read-only folder makes it writable for the moment and
// puts its whole mode back: setgid and sticky too (review 2026-11-05).
func TestRemoveKeepsParentSetgidAndSticky(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	runWorker(t, a)
	keep := filepath.Join(aroot, "keep")
	os.MkdirAll(filepath.Join(keep, "gone"), 0o755)
	os.WriteFile(filepath.Join(keep, "gone", "f"), []byte("x"), 0o644)
	want := os.FileMode(0o555) | os.ModeSetgid | os.ModeSticky
	if err := os.Chmod(keep, want); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(keep, 0o755) })
	if fi, _ := os.Stat(keep); fi.Mode() != want|os.ModeDir {
		t.Skipf("this filesystem keeps %v of %v", fi.Mode(), want)
	}
	res, err := a.Remove(context.Background(), api.RemoveRequest{Node: "a", Root: "data", Paths: []string{"keep/gone"}})
	if err != nil || res.Removed != 1 || len(res.Errors) != 0 {
		t.Fatalf("Remove: %+v %v", res, err)
	}
	if fi, _ := os.Stat(keep); fi.Mode() != want|os.ModeDir {
		t.Errorf("mode %v, want %v", fi.Mode(), want|os.ModeDir)
	}
}
