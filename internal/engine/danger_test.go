package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/frontend/cli"
)

// A mirror deletes a folder the source lacks, but never the excluded
// files inside it (rsync without --delete-excluded): those and their
// folders stay.
func TestMirrorKeepsExcludedInsideDeletedFolder(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	writeFiles(t, aroot, "p/keep.txt")
	writeFiles(t, broot, "m/gone/x.tmp", "m/gone/y.txt", "m/gone/deep/z.tmp", "m/gone/deep/w.txt", "m/all-gone/q.txt")
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "p", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror, Exclude: []string{"*.tmp"}}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	for p, want := range map[string]bool{"m/gone/x.tmp": true, "m/gone/deep/z.tmp": true, "m/gone/y.txt": false, "m/gone/deep/w.txt": false, "m/all-gone": false, "m/keep.txt": true} {
		if exists(broot, p) != want {
			t.Errorf("%s there = %v, want %v", p, !want, want)
		}
	}
}

// A move whose destination is a source, or inside one, would delete what
// it just copied: it must not start.
func TestMoveOntoOrIntoItselfRefused(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	runWorker(t, a)
	writeFiles(t, aroot, "d/f.txt", "f.txt")
	os.Symlink("d", filepath.Join(aroot, "link-to-d"))
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		paths []string
		dst   string
	}{
		{"into a subfolder of itself", []string{"d"}, "d/sub"},
		{"into itself", []string{"d"}, "d"},
		{"onto itself (same folder)", []string{"f.txt"}, "."},
		{"into itself through a symlink", []string{"d"}, "link-to-d"},
	} {
		id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: tc.paths, DstNodes: []string{"a"}, DstRoot: "data", DstDir: tc.dst,
			Conflict: api.ConflictOverwrite, Move: true})
		if err == nil {
			// Refused at the source, at the latest: the job must not end done.
			j := waitJob(t, a, id, 10*time.Second)
			if j.State == api.JobDone {
				t.Errorf("%s: move job done (its sources would now be deleted)", tc.name)
			}
			var why string
			for _, p := range j.Participants {
				why += p.Error
			}
			if !strings.Contains(why, "would delete what it copies") {
				t.Errorf("%s: failed, but not by the move check: %q", tc.name, why)
			}
		}
	}
	// A plain copy into a subfolder of itself stays allowed.
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"d"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: "d/copy"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 10*time.Second); j.State != api.JobDone {
		t.Errorf("copy into own subfolder: %s", j.State)
	}
}

// rm of a symlink to a folder removes the link, not the folder.
func TestRemoveSymlinkKeepsTarget(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	runWorker(t, a)
	writeFiles(t, aroot, "real/f.txt")
	os.Symlink("real", filepath.Join(aroot, "link"))
	os.Symlink(filepath.Join(aroot, "real"), filepath.Join(aroot, "abslink"))
	res, err := a.Remove(context.Background(), api.RemoveRequest{Root: "data", Paths: []string{"link", "abslink"}})
	if err != nil || res.Removed != 2 {
		t.Fatalf("remove: %+v %v", res, err)
	}
	if !exists(aroot, "real/f.txt") || exists(aroot, "link") || exists(aroot, "abslink") {
		t.Error("removing a symlink touched its target")
	}
}

// Odd arguments to rm, mkdir and mv never touch what they should not:
// the root, the staging area, a share inside the root, anything outside.
func TestCLIOddArguments(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	aroot := cfg.Roots[0].Path
	cfg.Roots[0].Hide = "box"
	a := build(t, cfg)
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	outside := t.TempDir()
	writeFiles(t, outside, "precious")
	writeFiles(t, aroot, "keep/f", "box/nodes/info.json", ".fkbr-tmp/job/x", "file")
	os.Symlink(outside, filepath.Join(aroot, "out-link"))
	snap := func() map[string]bool {
		m := map[string]bool{}
		for _, root := range []string{aroot, outside} {
			filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				m[p] = true
				return nil
			})
		}
		return m
	}
	before := snap()
	ctx := context.Background()
	for _, args := range [][]string{
		{"rm", "/"}, {"rm", "-r", "/"}, {"rm", "-r", "."}, {"rm", "-r", "/.."}, {"rm", "-r", "/../x"},
		{"rm", "-r", "/.fkbr-tmp"}, {"rm", "-r", "/.fkbr-tmp/job"}, {"rm", "-r", "/box"}, {"rm", "-r", "/box/nodes"},
		{"rm", "-r", "//"}, {"rm", "-r", "/keep/../.."}, {"rm", "-r", "/out-link/precious"}, {"rm", "-r", "a:/"},
		{"rm", "/file/"}, {"rm", "-r", "/nope", "/keep"}, // one bad path stops all
		{"mkdir", "/../escape"}, {"mkdir", "/.fkbr-tmp/new"}, {"mkdir", "/box/new"}, {"mkdir", "/out-link/new"},
		{"mv", "/", "b:/whole"}, {"mv", "/box", "b:/x"}, {"mv", "/.fkbr-tmp", "b:/x"},
	} {
		var out bytes.Buffer
		cli.Run(ctx, a, args[0], args[1:], &out)
		if now := snap(); len(now) != len(before) {
			for p := range before {
				if !now[p] {
					t.Errorf("%v removed %s", args, p)
				}
			}
			for p := range now {
				if !before[p] {
					t.Errorf("%v created %s", args, p)
				}
			}
			before = now
		}
	}
	if exists(broot, "whole") {
		t.Error("mv of the whole root copied it before failing")
	}
}
