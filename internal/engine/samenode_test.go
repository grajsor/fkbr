package engine

import (
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/config"
)

// A sync from a node to itself (a backup of one folder into another, or
// between two roots of one node) used to plan nothing: the source's scan
// and the target's were written to one blob, and the planner read that
// one as both (review 2026-11-05). It copies, mirrors and settles.
func TestSyncOnOneNode(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	other := t.TempDir()
	cfg.Roots = append(cfg.Roots, config.Root{ID: "other", Path: other, AllowWrite: true})
	a := build(t, cfg)
	root := cfg.Roots[0].Path
	makeTree(t, root, 6, 40<<10)
	runWorker(t, a)

	for _, c := range []struct {
		name   string
		dstTop string // the destination root's directory
		dstID  string
	}{{"same root", root, "data"}, {"other root", other, "other"}} {
		t.Run(c.name, func(t *testing.T) {
			req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"a"}, DstRoot: c.dstID, DstPath: "copy", Mode: api.SyncMirror}
			plan := mustPlan(t, a, req)
			if tg := target(plan, "a"); tg.Copy == 0 {
				t.Fatalf("a sync of a tree to a missing folder plans no copy: %+v", tg)
			}
			applyAndWait(t, a, plan, false)
			sameTree(t, filepath.Join(root, "tree"), filepath.Join(c.dstTop, "copy"))
			nothingToDo(t, mustPlan(t, a, req))

			// Mirror: an extra file on the target goes (the delete is checked
			// against the target's own scan), a changed source file comes.
			extra := filepath.Join(c.dstTop, "copy", "extra.txt")
			if err := os.WriteFile(extra, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "tree", "new.txt"), []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			plan = mustPlan(t, a, req)
			if tg := target(plan, "a"); tg.Delete != 1 || tg.Copy == 0 {
				t.Fatalf("mirror plan: %+v, want one delete and a copy", tg)
			}
			applyAndWait(t, a, plan, false)
			if _, err := os.Stat(extra); !os.IsNotExist(err) {
				t.Errorf("the mirror left the extra file: %v", err)
			}
			sameTree(t, filepath.Join(root, "tree"), filepath.Join(c.dstTop, "copy"))
			nothingToDo(t, mustPlan(t, a, req))
			os.Remove(filepath.Join(root, "tree", "new.txt"))
			os.RemoveAll(filepath.Join(c.dstTop, "copy"))
		})
	}
}

// With excludes, the source's list of folders holding excluded entries is
// not the target's.
func TestSyncOnOneNodeWithExcludes(t *testing.T) {
	share := t.TempDir()
	a, root := node(t, share, "a")
	makeTree(t, root, 4, 10<<10)
	os.WriteFile(filepath.Join(root, "tree", "skip.tmp"), []byte("s"), 0o644)
	runWorker(t, a)
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"a"}, DstRoot: "data", DstPath: "copy", Mode: api.SyncMirror, Exclude: []string{"*.tmp"}}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	if _, err := os.Stat(filepath.Join(root, "copy", "skip.tmp")); !os.IsNotExist(err) {
		t.Errorf("an excluded file was copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "copy", "big.bin")); err != nil {
		t.Errorf("nothing was copied: %v", err)
	}
	nothingToDo(t, mustPlan(t, a, req))
}

// The source node among several targets: it and the others all converge.
func TestSyncOnOneNodeAndAnother(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	makeTree(t, aroot, 5, 20<<10)
	runWorker(t, a)
	runWorker(t, b)
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"a", "b"}, DstRoot: "data", DstPath: "copy", Mode: api.SyncMirror}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	sameTree(t, filepath.Join(aroot, "tree"), filepath.Join(aroot, "copy"))
	sameTree(t, filepath.Join(aroot, "tree"), filepath.Join(broot, "copy"))
	nothingToDo(t, mustPlan(t, a, req))
}
