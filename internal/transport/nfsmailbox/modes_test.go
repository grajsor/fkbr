package nfsmailbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

// checkModes fails for any directory under root that other users cannot
// write to, or any file they cannot read.
func checkModes(t *testing.T, root string) (dirs, files int) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := d.Info()
		switch {
		case d.IsDir():
			dirs++
			if fi.Mode().Perm() != shareDirMode {
				t.Errorf("dir %s: %v, want %v", p, fi.Mode().Perm(), os.FileMode(shareDirMode))
			}
		default:
			files++
			if fi.Mode().Perm() != shareFileMode {
				t.Errorf("file %s: %v, want %v", p, fi.Mode().Perm(), os.FileMode(shareFileMode))
			}
		}
		return nil
	})
	return dirs, files
}

// Nodes on different hosts usually run as different users, so everything
// fkbr creates on the share must be usable by them whatever the umask.
func TestShareModesIgnoreUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	root := filepath.Join(t.TempDir(), "mb")
	if err := os.Mkdir(root, 0o755); err != nil { // as a user would: 0700 under this umask
		t.Fatal(err)
	}
	a, b := open(t, root, "a"), open(t, root, "b")
	a.Heartbeat(ctx, "w1 1 a")

	msg := &transport.Message{Protocol: 1, ID: "0001", Type: "list", From: "a", To: "b"}
	if err := a.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	job := &api.Job{ID: "18dfe2ce5f5b9560f9aa6dfc", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"b"}}}
	if err := a.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := b.PutStatus(ctx, job.ID, &api.JobStatus{Node: "b", Role: api.RoleTarget}); err != nil {
		t.Fatal(err)
	}
	cw, err := a.CreateChunk(ctx, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	cw.Write([]byte("chunk"))
	if err := cw.Commit([]byte(`{"seq":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.SetAcked(ctx, "a", job.ID, "b", 1); err != nil {
		t.Fatal(err)
	}
	bw, err := a.CreateBlob(ctx, "plans/p1/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	bw.Write([]byte("{}"))
	if err := bw.Commit(nil); err != nil {
		t.Fatal(err)
	}

	dirs, files := checkModes(t, root)
	if dirs < 15 || files < 8 {
		t.Errorf("walked only %d dirs and %d files; is the test still exercising the share?", dirs, files)
	}
}

// A share made by an older version (0770/0660) is opened up by the nodes
// that own its entries.
func TestPruneOpensUpOwnEntries(t *testing.T) {
	root := t.TempDir()
	a := open(t, root, "a")
	old := filepath.Join(root, "jobs", "18dfe2ce5f5b9560f9aa6dfc")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "job.json"), []byte(`{"id":"18dfe2ce5f5b9560f9aa6dfc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "jobs"), old, filepath.Join(root, "inbox")} {
		os.Chmod(p, 0o770)
	}
	os.Chmod(old, 0o700)
	if err := a.Prune(ctx, 0); err != nil {
		t.Fatal(err)
	}
	checkModes(t, root)
}

func TestReadListed(t *testing.T) {
	m := open(t, t.TempDir(), "a")
	dir := m.path("data", "src", "job")
	if _, err := m.readListed(dir, "x.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
	if err := mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.readListed(dir, "x.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if ok, err := m.listed(dir, "x.json"); ok || err != nil {
		t.Fatalf("listed missing file: %v %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.json"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := m.readListed(dir, "x.json"); err != nil || string(b) != "hi" {
		t.Fatalf("listed file: %q %v", b, err)
	}
}
