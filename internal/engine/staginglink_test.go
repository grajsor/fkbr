package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/receive"
)

// A link in a root that leads to the staging area is the staging area, also
// where the root holds no share: nothing is made, removed or received
// through it (review 2026-11-06: the check looked at the path as written
// unless the root had a Hide).
func TestStagingThroughLink(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	stage := filepath.Join(aroot, receive.StagingDir)
	if err := os.MkdirAll(filepath.Join(stage, "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"link", "d/up"} {
		os.MkdirAll(filepath.Join(aroot, filepath.Dir(l)), 0o755)
	}
	if err := os.Symlink(receive.StagingDir, filepath.Join(aroot, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+receive.StagingDir, filepath.Join(aroot, "d/up")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(broot, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{"link/other", "d/up/other"} {
		id, err := b.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: dst})
		if err != nil {
			t.Fatalf("Submit to %s: %v (the refusal belongs to the target)", dst, err)
		}
		if j := waitJob(t, b, id, 10*time.Second); j.State == api.JobDone {
			t.Errorf("a copy into %s was done", dst)
		}
	}
	for _, p := range []string{"link/new", "link/other/new", "d/up/new"} {
		if err := b.Mkdir(ctx, api.MkdirRequest{Node: "a", Root: "data", Path: p}); err == nil {
			t.Errorf("Mkdir of %s accepted", p)
		}
	}
	for _, p := range []string{"link/other", "d/up/other"} {
		res, err := b.Remove(ctx, api.RemoveRequest{Node: "a", Root: "data", Paths: []string{p}})
		if err != nil || res.Removed != 0 {
			t.Errorf("Remove of %s: %+v %v", p, res, err)
		}
	}
	for _, p := range []string{"new", "other/new", "other/x"} {
		if _, err := os.Lstat(filepath.Join(stage, p)); err == nil {
			t.Errorf("%s/%s was made through a link", receive.StagingDir, p)
		}
	}
	if _, err := os.Stat(filepath.Join(stage, "other")); err != nil {
		t.Errorf("a folder in the staging area was removed: %v", err)
	}
}
