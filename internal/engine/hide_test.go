package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
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

// A node whose shared directory is deleted stops, saying why, instead of
// failing every transfer into a directory that is gone.
func TestWorkerStopsWhenRootDeleted(t *testing.T) {
	oldCheck := rootCheckInterval // restored after the workers stop
	t.Cleanup(func() { rootCheckInterval = oldCheck })
	rootCheckInterval = 20 * time.Millisecond
	a, aroot := node(t, t.TempDir(), "a")
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case <-a.Ready():
	case err := <-done:
		t.Fatal(err)
	}
	os.RemoveAll(aroot)
	select {
	case err := <-done:
		if !errors.Is(err, localfs.ErrRootGone) || !strings.Contains(err.Error(), "was deleted") {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker still running with its shared directory deleted")
	}
}

// A link inside the root that leads to the share is the share: it is not
// listed, read, sent, written into or removed through (review 2026-11-05:
// the Hide check looked at the path as written, not where it leads).
func TestShareThroughSymlink(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	aroot := acfg.Roots[0].Path
	acfg.Roots[0].Hide = "box"
	os.MkdirAll(filepath.Join(aroot, "box/nodes"), 0o755)
	os.WriteFile(filepath.Join(aroot, "box/nodes/info.json"), []byte("share"), 0o644)
	os.Symlink("box", filepath.Join(aroot, "link"))      // to the share
	os.Symlink("box/nodes", filepath.Join(aroot, "ln2")) // into it
	os.MkdirAll(filepath.Join(aroot, "d"), 0o755)
	os.Symlink("../box", filepath.Join(aroot, "d/up")) // from a folder
	a := build(t, acfg)
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()

	for _, p := range []string{"link", "link/nodes", "ln2", "d/up", "d/up/nodes", "d/../link/nodes"} {
		if _, err := a.List(ctx, api.ListRequest{Node: "a", Root: "data", Path: p}); err == nil {
			t.Errorf("List of %s reached the share", p)
		}
	}
	for _, p := range []string{"link/nodes/info.json", "ln2/info.json", "d/up/nodes/info.json", "link/../link/nodes/info.json"} {
		if _, err := a.Read(ctx, api.ReadRequest{Node: "a", Root: "data", Path: p}); err == nil {
			t.Errorf("Read of %s reached the share", p)
		}
	}
	for _, p := range []string{"link/new", "link/nodes/new", "ln2/new", "d/up/new"} {
		if err := b.Mkdir(ctx, api.MkdirRequest{Node: "a", Root: "data", Path: p}); err == nil {
			t.Errorf("Mkdir of %s reached the share", p)
		}
	}
	for _, p := range []string{"link/nodes", "link/nodes/info.json", "d/up/nodes"} {
		res, err := b.Remove(ctx, api.RemoveRequest{Node: "a", Root: "data", Paths: []string{p}})
		if err != nil || res.Removed != 0 {
			t.Errorf("Remove of %s: %+v %v", p, res, err)
		}
	}
	// A copy into a folder that is the share through a link is refused.
	// The source exists first: a missing one would fail the Submit and the
	// receive refusal would never be reached (review 2026-11-06).
	if err := os.WriteFile(filepath.Join(broot, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := b.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: "link/nodes"})
	if err != nil {
		t.Fatalf("Submit: %v (the refusal belongs to the target)", err)
	}
	if j := waitJob(t, b, id, 10*time.Second); j.State == api.JobDone {
		t.Error("a copy into the share through a link was done")
	}
	if _, err := os.Stat(filepath.Join(aroot, "box/nodes/x")); err == nil {
		t.Error("the copy landed in the share")
	}
	// A copy of a folder holding a link to the share sends the link, and
	// nothing of the share.
	id, err = a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"d"}, DstNodes: []string{"b"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, a, id, 10*time.Second)
	if _, err := os.Stat(filepath.Join(broot, "d/up/nodes")); err == nil {
		t.Error("the share arrived through a link")
	}
	if b, _ := os.ReadFile(filepath.Join(aroot, "box/nodes/info.json")); string(b) != "share" {
		t.Errorf("the share was changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(aroot, "box/nodes/new")); err == nil {
		t.Error("something was made in the share")
	}
}
