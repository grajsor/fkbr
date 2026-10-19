package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/receive"
)

// makeTree creates dir/tree with nfiles small files plus one big file.
func makeTree(t *testing.T, dir string, nfiles int, bigSize int) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "tree", "sub"), 0o755))
	for i := range nfiles {
		must(os.WriteFile(filepath.Join(dir, "tree", "sub", fmt.Sprintf("f%03d", i)), []byte(fmt.Sprintf("file %d\n", i)), 0o640))
	}
	big := make([]byte, bigSize)
	rand.Read(big)
	must(os.WriteFile(filepath.Join(dir, "tree", "big.bin"), big, 0o600))
	must(os.Symlink("big.bin", filepath.Join(dir, "tree", "link")))
	old := time.Unix(1600000000, 0)
	must(os.Chtimes(filepath.Join(dir, "tree", "big.bin"), old, old))
}

// sameTree compares content, mode, mtime and symlinks of two trees.
func sameTree(t *testing.T, a, b string) {
	t.Helper()
	n := 0
	err := filepath.WalkDir(a, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(a, p)
		q := filepath.Join(b, rel)
		ai, _ := os.Lstat(p)
		bi, err := os.Lstat(q)
		if err != nil {
			return fmt.Errorf("missing %s", rel)
		}
		if ai.Mode() != bi.Mode() {
			return fmt.Errorf("%s: mode %v vs %v", rel, ai.Mode(), bi.Mode())
		}
		switch {
		case ai.Mode()&fs.ModeSymlink != 0:
			la, _ := os.Readlink(p)
			lb, _ := os.Readlink(q)
			if la != lb {
				return fmt.Errorf("%s: link %q vs %q", rel, la, lb)
			}
		case ai.Mode().IsRegular():
			da, _ := os.ReadFile(p)
			db, _ := os.ReadFile(q)
			if !bytes.Equal(da, db) {
				return fmt.Errorf("%s: content differs", rel)
			}
			fallthrough
		case ai.IsDir():
			if !ai.ModTime().Equal(bi.ModTime()) {
				return fmt.Errorf("%s: mtime %v vs %v", rel, ai.ModTime(), bi.ModTime())
			}
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("compared only %d entries", n)
	}
}

func waitJob(t *testing.T, e *Engine, id string, timeout time.Duration) *api.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		j, err := e.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State.Terminal() {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s still %s after %s: %+v", id, j.State, timeout, j.Participants)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTransferWithBackpressure(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	slave, sroot := node(t, share, "slave1")
	makeTree(t, mroot, 50, 1<<20) // 1 MiB file vs 256 KiB job quota: must wait for acks
	runWorker(t, master)
	runWorker(t, slave)

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"slave1"}, DstRoot: "data", DstDir: "copy", Compress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	if j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "copy", "tree"))
	if j.TotalChunks < 16 {
		t.Errorf("expected the big file to span many chunks, got %d", j.TotalChunks)
	}
	if _, err := os.Stat(filepath.Join(share, "data", "master", id)); !os.IsNotExist(err) {
		t.Errorf("staged data not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sroot, receive.StagingDir)); !os.IsNotExist(err) {
		t.Errorf("receiver staging not removed: %v", err)
	}

	// Same copy again: everything exists, default policy skips it all.
	id, _ = master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"slave1"}, DstRoot: "data", DstDir: "copy",
	})
	j = waitJob(t, master, id, 20*time.Second)
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && (p.Skipped != 52 || p.State != api.JobDone) {
			t.Errorf("second copy: %+v", p)
		}
	}
}

func TestFanOutAndThirdPartyCopy(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, r1 := node(t, share, "slave1")
	s2, r2 := node(t, share, "slave2")
	makeTree(t, mroot, 10, 200<<10)
	for _, e := range []*Engine{master, s1, s2} {
		runWorker(t, e)
	}
	// Initiated on slave2, copying master -> slave1 and slave2.
	id, err := s2.Submit(context.Background(), api.TransferRequest{
		SrcNode: "master", SrcRoot: "data", Paths: []string{"."}, DstNodes: []string{"slave1", "slave2"}, DstRoot: "data", DstDir: "mirror",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, s2, id, 20*time.Second); j.State != api.JobDone || len(j.Participants) != 3 {
		t.Fatalf("job: %s %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(r1, "mirror", "tree"))
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(r2, "mirror", "tree"))
}

func TestCancelWhileTargetDown(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	slave, _ := node(t, share, "slave1")
	makeTree(t, mroot, 1, 2<<20)
	runWorker(t, master)
	stopSlave := runWorker(t, slave)
	stopSlave() // still "online" for OfflineAfter, but nobody consumes

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"slave1"}, DstRoot: "data",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the source is blocked on quota, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, _ := master.Job(context.Background(), id)
		if s := ownStatus(j, "master", api.RoleSource); s != nil && s.Chunks > 0 && s.Phase != "sending" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("source never blocked: %+v", j.Participants)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := master.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 5*time.Second); j.State != api.JobCancelled {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	if _, err := os.Stat(filepath.Join(share, "data", "master", id)); !os.IsNotExist(err) {
		t.Errorf("staged data not removed after cancel")
	}
}

func TestReceiverRestartResumes(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "slave1")
	makeTree(t, mroot, 20, 3<<20)
	runWorker(t, master)
	stop := runWorker(t, build(t, scfg))

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"slave1"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Let the receiver consume some chunks, then kill it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := master.st.Acked(context.Background(), "master", id, "slave1"); n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receiver made no progress")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	if j, _ := master.Job(context.Background(), id); j.State.Terminal() {
		t.Fatalf("job finished before restart: %s", j.State)
	}
	runWorker(t, build(t, scfg)) // restart: request redelivered from inbox cur/
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(scfg.Roots[0].Path, "in", "tree"))
}

func TestSubmitValidation(t *testing.T) {
	share := t.TempDir()
	master, _ := node(t, share, "master")
	cfg := nodeCfg(t, share, "slave1")
	cfg.Roots[0].AllowWrite = false
	runWorker(t, master)
	runWorker(t, build(t, cfg))
	ctx := context.Background()
	base := api.TransferRequest{SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"slave1"}, DstRoot: "data"}

	if _, err := master.Submit(ctx, base); !errors.Is(err, api.ErrReadOnly) {
		t.Errorf("read-only dest: got %v", err)
	}
	r := base
	r.DstNodes = []string{"ghost"}
	if _, err := master.Submit(ctx, r); !errors.Is(err, api.ErrUnknownNode) {
		t.Errorf("unknown node: got %v", err)
	}
	r = base
	r.Paths = []string{"../etc"}
	if _, err := master.Submit(ctx, r); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("bad path: got %v", err)
	}
	r = base
	r.SrcRoot = "nope"
	if _, err := master.Submit(ctx, r); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("unknown root: got %v", err)
	}
}

func TestBundleName(t *testing.T) {
	cases := [][3]string{{".", "a/b", "a/b"}, {"tree", "tree", "tree"}, {"tree", "tree/x", "tree/x"}, {"a/tree", "a/tree/x", "tree/x"}}
	for _, c := range cases {
		if got := bundleName(c[0], c[1]); got != c[2] {
			t.Errorf("bundleName(%q,%q) = %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestSenderRestartResumes(t *testing.T) {
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	slave, sroot := node(t, share, "slave1")
	mroot := mcfg.Roots[0].Path
	makeTree(t, mroot, 20, 3<<20)
	master := build(t, mcfg)
	stop := runWorker(t, master)
	runWorker(t, slave)

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"slave1"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, _ := master.Job(context.Background(), id)
		if s := ownStatus(j, "master", api.RoleSource); s != nil && s.Chunks >= 5 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sender made no progress")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	master2 := build(t, mcfg)
	runWorker(t, master2)
	j := waitJob(t, master2, id, 30*time.Second)
	if j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "in", "tree"))
}
