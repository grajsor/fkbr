package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
)

// A share inside a node's root (cd ~ && fkbr ~/box) is not sent, not
// written, and neither it nor a folder holding it can be deleted.
func TestShareInsideRoot(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	aroot := acfg.Roots[0].Path
	acfg.Roots[0].Hide = "x/box"
	for _, f := range []string{"x/box/nodes/info.json", "x/other.txt"} {
		os.MkdirAll(filepath.Join(aroot, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(aroot, f), []byte("x"), 0o644)
	}
	a := build(t, acfg)
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"b"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 10*time.Second); j.State != api.JobDone {
		t.Fatalf("copy: %s", j.State)
	}
	if _, err := os.Stat(filepath.Join(broot, "x/other.txt")); err != nil {
		t.Errorf("other.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(broot, "x/box")); !os.IsNotExist(err) {
		t.Errorf("the share was sent: %v", err)
	}
	if err := b.Mkdir(ctx, api.MkdirRequest{Node: "a", Root: "data", Path: "x/box/new"}); err == nil {
		t.Error("mkdir in the share accepted")
	}
	res, err := b.Remove(ctx, api.RemoveRequest{Node: "a", Root: "data", Paths: []string{"x", "x/box"}})
	if err != nil || res.Removed != 0 {
		t.Errorf("remove of the share or its parent: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(aroot, "x/box/nodes/info.json")); err != nil {
		t.Errorf("share damaged: %v", err)
	}
}
