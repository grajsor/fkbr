package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
)

func writeFiles(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
		if err := os.WriteFile(filepath.Join(root, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(root, p string) bool {
	_, err := os.Lstat(filepath.Join(root, p))
	return err == nil
}

// Excluded entries are not sent by a copy; in a sync they are neither
// copied nor deleted, on either side.
func TestExclude(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	writeFiles(t, aroot, "p/keep.txt", "p/x.tmp", "p/sub/y.tmp", "p/cache/big", "p/sub/cache", "p/build/out/o", "p/sub/build/out/o")
	ex := []string{"*.tmp", "cache/", "build/out"}

	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"p"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "c", Exclude: ex})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 10*time.Second); j.State != api.JobDone {
		t.Fatalf("copy: %s", j.State)
	}
	for p, want := range map[string]bool{"c/p/keep.txt": true, "c/p/x.tmp": false, "c/p/sub/y.tmp": false, "c/p/cache": false,
		"c/p/sub/cache": true /* a file: cache/ is for directories */, "c/p/build/out": false, "c/p/sub/build/out/o": true} {
		if exists(broot, p) != want {
			t.Errorf("copy: %s there = %v, want %v", p, !want, want)
		}
	}

	// Mirror: an excluded file only the target has is kept; excluded
	// source files are not sent.
	writeFiles(t, broot, "m/local.tmp", "m/stale.txt")
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "p", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror, Exclude: ex}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	for p, want := range map[string]bool{"m/keep.txt": true, "m/local.tmp": true, "m/stale.txt": false, "m/x.tmp": false, "m/cache": false, "m/build/out": false} {
		if exists(broot, p) != want {
			t.Errorf("mirror: %s there = %v, want %v", p, !want, want)
		}
	}
	nothingToDo(t, mustPlan(t, a, req))

	if _, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"p"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "c", Exclude: []string{"[x-"}}); err == nil {
		t.Error("bad pattern accepted")
	}
}

// A node on a build without excludes would send everything: refuse.
func TestExcludeNeedsSupport(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	old, _ := node(t, share, "old")
	old.info.Features = nil // as published by an older build
	runWorker(t, a)
	runWorker(t, old)
	ctx := context.Background()
	_, err := a.Submit(ctx, api.TransferRequest{SrcNode: "old", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: "c", Exclude: []string{"*.tmp"}})
	if err == nil || !strings.Contains(err.Error(), "old") || !strings.Contains(err.Error(), "update") {
		t.Errorf("copy from an old node with excludes: %v", err)
	}
	_, err = a.PlanSync(ctx, api.SyncRequest{SrcRoot: "data", SrcPath: ".", DstNodes: []string{"old"}, DstRoot: "data", DstPath: "m", Exclude: []string{"*.tmp"}})
	if err == nil || !strings.Contains(err.Error(), "old") {
		t.Errorf("sync to an old node with excludes: %v", err)
	}
}

// A plan lists what it would copy and delete, entry by entry.
func TestSyncPlanEntries(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	writeFiles(t, aroot, "p/new1.txt", "p/new2.txt", "p/sub/new3.txt")
	writeFiles(t, broot, "m/old1.txt", "m/old2.txt", "m/olddir/x")
	plan := mustPlan(t, a, api.SyncRequest{SrcRoot: "data", SrcPath: "p", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror})
	l, err := a.SyncPlanEntries(context.Background(), plan.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Deletes) != 1 || l.Deletes[0].Node != "b" || strings.Join(l.Deletes[0].Paths, " ") != "old1.txt old2.txt olddir" {
		t.Errorf("deletes: %+v", l.Deletes)
	}
	if len(l.Copies) != 1 || !slices.Contains(l.Copies[0].Paths, "sub/new3.txt") || l.Copies[0].Targets[0] != "b" {
		t.Errorf("copies: %+v", l.Copies)
	}
	l, err = a.SyncPlanEntries(context.Background(), plan.ID, 2)
	if err != nil || len(l.Deletes[0].Paths) != 2 || l.Deletes[0].More != 1 {
		t.Errorf("limited to 2: %+v %v", l.Deletes, err)
	}
}

// A sync with KeepNewer leaves a target file that was changed after the
// source's alone.
func TestSyncKeepNewer(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	writeFiles(t, aroot, "p/edited-there.txt", "p/plain.txt")
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "p", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "m", KeepNewer: true}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	later := time.Now().Add(time.Hour)
	os.WriteFile(filepath.Join(broot, "m/edited-there.txt"), []byte("mine"), 0o644)
	os.Chtimes(filepath.Join(broot, "m/edited-there.txt"), later, later)
	os.WriteFile(filepath.Join(aroot, "p/plain.txt"), []byte("new"), 0o644)
	os.Chtimes(filepath.Join(aroot, "p/plain.txt"), later, later)
	plan := mustPlan(t, a, req)
	if tg := target(plan, "b"); tg.Kept != 1 || tg.Copy < 1 {
		t.Fatalf("plan: %+v", tg)
	}
	applyAndWait(t, a, plan, false)
	if b, _ := os.ReadFile(filepath.Join(broot, "m/edited-there.txt")); string(b) != "mine" {
		t.Errorf("newer target file replaced: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(broot, "m/plain.txt")); string(b) != "new" {
		t.Errorf("older target file not updated: %q", b)
	}
}
