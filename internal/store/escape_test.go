package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
)

// outsideTree is a folder outside the share with old files and a
// subfolder, as a node user's home might be.
func outsideTree(t *testing.T) (dir string, check func()) {
	t.Helper()
	dir = t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	files := []string{"notes.txt", "sub/keep.txt", "heartbeat", "info.json"}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
	}
	os.Chmod(dir, 0o755)
	os.Chtimes(filepath.Join(dir, "sub"), old, old)
	return dir, func() {
		t.Helper()
		for _, f := range files {
			p := filepath.Join(dir, f)
			fi, err := os.Stat(p)
			if err != nil {
				t.Errorf("outside the share: %s gone: %v", f, err)
				continue
			}
			if b, _ := os.ReadFile(p); string(b) != "mine" || fi.Mode().Perm() != 0o600 {
				t.Errorf("outside the share: %s changed: %q %v", f, b, fi.Mode().Perm())
			}
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o755 {
			t.Errorf("outside the share: the folder's mode changed to %v", fi.Mode().Perm())
		}
		ents, _ := os.ReadDir(dir)
		if len(ents) != 4 {
			t.Errorf("outside the share: %d entries now, want 4", len(ents))
		}
	}
}

// A share writer who puts a symlink to a folder outside the share in
// place of one of its layout folders reaches nothing through it: pruning,
// heartbeats, chunks, blobs and replies stay in the share, and the share
// heals (review 2026-10-25: pruning deleted and chmodded through such a
// link, recursively for reply/<node>).
func TestLayoutSymlinkStaysInside(t *testing.T) {
	for _, slow := range []bool{false, true} { // openat2, os.Root
		t.Run(map[bool]string{false: "openat2", true: "os.Root"}[slow], func(t *testing.T) {
			old := noOpenat2
			noOpenat2 = slow
			t.Cleanup(func() { noOpenat2 = old })
			layoutSymlinks(t)
		})
	}
}

func layoutSymlinks(t *testing.T) {
	for _, layout := range []string{"tmp", "reply/a", "inbox/a/failed", "inbox/a/cur", "data/a", "jobs", "blobs", "nodes/a", "nodes"} {
		t.Run(layout, func(t *testing.T) {
			share := t.TempDir()
			m := open(t, share, "a")
			if err := m.CreateJob(ctx, &api.Job{ID: "18dfe2ce5f5b9560f9aa6dfc", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"b"}}}); err != nil {
				t.Fatal(err)
			}
			out, check := outsideTree(t)
			p := filepath.Join(share, layout)
			os.MkdirAll(p, 0o777)
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(out, p); err != nil {
				t.Fatal(err)
			}
			m.Prune(ctx, 0)
			m.PruneJobs(ctx, 0)
			m.PruneBlobs(ctx, 0)
			m.openUp()
			m.Heartbeat(ctx, "w1 token")
			if w, err := m.CreateChunk(ctx, "18dfe2ce5f5b9560f9aa6dfc", 0); err == nil {
				w.Write([]byte("chunk"))
				w.Commit([]byte(`{}`))
			}
			m.RemoveJobData(ctx, "18dfe2ce5f5b9560f9aa6dfc")
			if w, err := m.CreateBlob(ctx, "plans/x"); err == nil {
				w.Write([]byte("blob"))
				w.Commit(nil)
			}
			m.Unregister(ctx, "a")
			check()
		})
	}
}

// A layout folder replaced by a symlink is made again: the node goes on
// working in the share.
func TestLayoutSymlinkHealed(t *testing.T) {
	share := t.TempDir()
	m := open(t, share, "a")
	out, check := outsideTree(t)
	p := filepath.Join(share, "tmp")
	os.RemoveAll(p)
	os.Symlink(out, p)
	if _, err := m.Heartbeat(ctx, "w1 token"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
		t.Fatalf("tmp not made again: %v %v", fi, err)
	}
	check()
}

// A share must be a directory of its own: not /, the home directory or
// the binary's, nor one with other things in it that is no share yet
// (review 2026-10-25: every start opened up and pruned whatever it was).
func TestShareDirRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe, _ := os.Executable()
	foreign := t.TempDir()
	os.WriteFile(filepath.Join(foreign, "report.pdf"), []byte("x"), 0o600)
	for _, dir := range []string{"/", home, filepath.Dir(exe), foreign} {
		if _, err := Open(dir, "a", Options{}); err == nil {
			t.Errorf("%s accepted as a share", dir)
		}
	}
	if fi, _ := os.Stat(filepath.Join(foreign, "report.pdf")); fi.Mode().Perm() != 0o600 {
		t.Errorf("a refused share was changed: %v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(foreign); len(ents) != 1 {
		t.Errorf("a refused share got entries: %v", ents)
	}
	// Empty, an export's top with what a NAS keeps there, or a share.
	nas := t.TempDir()
	for _, n := range []string{"lost+found", ".snapshot", "#recycle", "@eaDir"} {
		os.Mkdir(filepath.Join(nas, n), 0o755)
	}
	share := t.TempDir()
	open(t, share, "b")
	os.WriteFile(filepath.Join(share, "left-by-someone"), []byte("x"), 0o600)
	for _, dir := range []string{t.TempDir(), nas, share} {
		if _, err := Open(dir, "a", Options{}); err != nil {
			t.Errorf("%s refused: %v", dir, err)
		}
	}
}

// Opening up the share (modes for other users) touches fkbr's own entries
// only, and never through a symlink.
func TestOpenUpOnlyLayout(t *testing.T) {
	share := t.TempDir()
	m := open(t, share, "a")
	mine := filepath.Join(share, "my-notes.txt")
	os.WriteFile(mine, []byte("x"), 0o600)
	out, check := outsideTree(t)
	os.Symlink(out, filepath.Join(share, "jobs", "link"))
	os.Symlink(filepath.Join(out, "notes.txt"), filepath.Join(share, "tmp", "f"))
	m.openUp()
	if fi, _ := os.Stat(mine); fi.Mode().Perm() != 0o600 {
		t.Errorf("a file not fkbr's was opened up: %v", fi.Mode().Perm())
	}
	check()
}
