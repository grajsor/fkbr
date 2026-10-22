package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
	"fkbr/internal/store"
	"fkbr/internal/transport"
	"fkbr/internal/transport/nfsmailbox"
)

// faultStore injects storage faults into the share.
type faultStore struct {
	*store.Dir
	mu         sync.Mutex
	noSpaceAt  int // chunk seq whose first write fails with ENOSPC (-1: never)
	noSpaceHit bool
	corruptSeq int // chunk seq returned with one byte flipped (-1: never)
	// corruptOnce limits the corruption to the first read (transient).
	corruptOnce, corrupted bool
	// Soft-mount style I/O errors: the write of chunk eioWriteAt fails once;
	// reading chunk eioReadSeq fails eioReads times (-1: always).
	eioWriteAt, eioReadSeq, eioReads int
	eioWriteHit                      bool
	eioReadHits                      int
	// openDelay makes every chunk slow to open (a slow share).
	openDelay time.Duration
	// hideAcks: this target's acks read as 0 (a stale listing).
	hideAcks string
}

type eioWriter struct{ transport.ChunkWriter }

func (w eioWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "chunk", Err: syscall.EIO}
}

type noSpaceWriter struct{ transport.ChunkWriter }

func (w noSpaceWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "chunk", Err: syscall.ENOSPC}
}

func (f *faultStore) CreateChunk(ctx context.Context, id string, seq int) (transport.ChunkWriter, error) {
	w, err := f.Dir.CreateChunk(ctx, id, seq)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && seq == f.noSpaceAt && !f.noSpaceHit {
		f.noSpaceHit = true
		return noSpaceWriter{w}, nil
	}
	if err == nil && seq == f.eioWriteAt && !f.eioWriteHit {
		f.eioWriteHit = true
		return eioWriter{w}, nil
	}
	return w, err
}

type flipReader struct {
	io.ReadCloser
	off int64
}

func (r *flipReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.off <= 100 && r.off+int64(n) > 100 {
		p[100-r.off] ^= 0xff
	}
	r.off += int64(n)
	return n, err
}

func (f *faultStore) OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	time.Sleep(f.openDelay)
	f.mu.Lock()
	if seq == f.eioReadSeq && (f.eioReads < 0 || f.eioReadHits < f.eioReads) {
		f.eioReadHits++
		f.mu.Unlock()
		return nil, &os.PathError{Op: "open", Path: "chunk", Err: syscall.EIO}
	}
	f.mu.Unlock()
	rc, err := f.Dir.OpenChunk(ctx, src, id, seq)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && seq == f.corruptSeq && !(f.corruptOnce && f.corrupted) {
		f.corrupted = true
		return &flipReader{ReadCloser: rc}, nil
	}
	return rc, err
}

func buildFaulty(t *testing.T, cfg *config.Config) (*Engine, *faultStore) {
	t.Helper()
	nfsOnly(t)
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(cfg.NFSRoot, cfg.NodeID, nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	fs := &faultStore{Dir: mb, noSpaceAt: -1, corruptSeq: -1, eioWriteAt: -1, eioReadSeq: -1}
	e := New(cfg, fsys, mb, fs, Options{Version: "test"})
	t.Cleanup(func() { e.Close() })
	return e, fs
}

func TestShareFullRetries(t *testing.T) {
	oldWait := noSpaceWait // restored after the workers stop (t.Cleanup runs after defers)
	t.Cleanup(func() { noSpaceWait = oldWait })
	noSpaceWait = 50 * time.Millisecond
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.noSpaceAt = 2
	s1, sroot := node(t, share, "s1")
	makeTree(t, mcfg.Roots[0].Path, 10, 600<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	if !fs.noSpaceHit {
		t.Fatal("fault never injected")
	}
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(sroot, "tree"))
}

func TestCorruptChunkFailsLoudly(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	s1, fs := buildFaulty(t, scfg)
	fs.corruptSeq = 1
	makeTree(t, mroot, 5, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, _ := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	j := waitJob(t, master, id, 30*time.Second)
	if j.State != api.JobFailed {
		t.Fatalf("corrupt chunk: state %s", j.State)
	}
	if st := ownStatus(j, "s1", api.RoleTarget); st == nil || !strings.Contains(st.Error, "checksum mismatch") {
		t.Fatalf("target status: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(scfg.Roots[0].Path, "tree")); !os.IsNotExist(err) {
		t.Error("data from a corrupt transfer was installed")
	}
}

func TestShareHiccupRecovers(t *testing.T) {
	share := t.TempDir()
	master, _ := node(t, share, "master")
	s1, _ := node(t, share, "s1")
	runWorker(t, master)
	runWorker(t, s1)
	online := func() bool {
		ns, _ := master.Nodes(context.Background())
		for _, n := range ns {
			if n.ID == "s1" {
				return n.Online
			}
		}
		return false
	}
	waitFor := func(want bool) {
		deadline := time.Now().Add(5 * time.Second)
		for online() != want {
			if time.Now().After(deadline) {
				t.Fatalf("s1 online never became %v", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// s1 can no longer write its heartbeat: it must drop offline...
	dir := filepath.Join(share, "nodes", "s1")
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o770)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	waitFor(false)
	// ...and come back by itself once the share recovers.
	os.Chmod(dir, 0o770)
	waitFor(true)
	if _, err := master.List(context.Background(), api.ListRequest{Node: "s1", Root: "data"}); err != nil {
		t.Fatalf("list after recovery: %v", err)
	}
}

func TestHostileFileNames(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	runWorker(t, master)
	runWorker(t, s1)
	names := []string{
		"new\nline", "tab\there", "spaces and ümlauts ✓", "-leading-dash", `back\slash`,
		"quote\"s and 'single'", strings.Repeat("long", 50), "dir with space/inner\nnewline",
	}
	src := filepath.Join(mroot, "odd")
	for _, n := range names {
		p := filepath.Join(src, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	id, err := master.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"odd"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "copied"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("copy: %s %+v", j.State, j.Participants)
	}
	sameTree(t, src, filepath.Join(sroot, "copied", "odd"))

	req := api.SyncRequest{SrcRoot: "data", SrcPath: "odd", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "synced", Mode: api.SyncMirror}
	applyAndWait(t, master, mustPlan(t, master, req), false)
	sameTree(t, src, filepath.Join(sroot, "synced"))
	os.Remove(filepath.Join(src, "new\nline"))
	plan := mustPlan(t, master, req)
	if target(plan, "s1").Delete != 1 {
		t.Fatalf("mirror delete of an odd name: %+v", plan.Targets)
	}
	applyAndWait(t, master, plan, false)
	if _, err := os.Stat(filepath.Join(sroot, "synced", "new\nline")); !os.IsNotExist(err) {
		t.Error("odd-named file not deleted by mirror")
	}
	nothingToDo(t, mustPlan(t, master, req))
}

func TestTransientCorruptionRetried(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	s1, fs := buildFaulty(t, scfg)
	fs.corruptSeq, fs.corruptOnce = 1, true
	makeTree(t, mroot, 5, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, _ := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("transient corruption not recovered: %s %+v", j.State, j.Participants)
	}
	if !fs.corrupted {
		t.Fatal("fault never injected")
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(scfg.Roots[0].Path, "tree"))
}

func TestLockSelfHealsAndStaysFresh(t *testing.T) {
	oldCheck, oldTouch := lockCheckInterval, lockTouchInterval // restored after the worker stops
	t.Cleanup(func() { lockCheckInterval, lockTouchInterval = oldCheck, oldTouch })
	lockCheckInterval, lockTouchInterval = 20*time.Millisecond, 50*time.Millisecond
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	e := build(t, cfg)
	runWorker(t, e)
	lock := filepath.Join(cfg.StateDir, "worker.lock")

	// Aged by a tmpfiles-style cleaner's standards: gets refreshed.
	old := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(lock, old, old)
	waitUntil(t, func() bool {
		fi, err := os.Stat(lock)
		return err == nil && time.Since(fi.ModTime()) < time.Minute
	}, "lock mtime refreshed")

	// Deleted: taken back, so no second worker can start.
	os.Remove(lock)
	waitUntil(t, func() bool { _, err := os.Stat(lock); return err == nil }, "lock recreated")
	if err := build(t, cfg).Run(context.Background()); !errors.Is(err, ErrWorkerRunning) {
		t.Fatalf("second worker after re-take: %v", err)
	}
}

func TestLockLostStopsWorker(t *testing.T) {
	oldCheck := lockCheckInterval // restored after the worker stops
	t.Cleanup(func() { lockCheckInterval = oldCheck })
	lockCheckInterval = 50 * time.Millisecond
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	e := build(t, cfg)
	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background()) }()
	lock := filepath.Join(cfg.StateDir, "worker.lock")
	waitUntil(t, func() bool { _, err := os.Stat(lock); return err == nil }, "lock created")
	// Someone deletes the lock and another process grabs the path first.
	os.Remove(lock)
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrLockLost) {
			t.Fatalf("worker ended with %v, want ErrLockLost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker kept running without its lock")
	}
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPruneAtStartup(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	a := build(t, cfg)
	// A finished job from long ago, left by an earlier run.
	old := &api.Job{ID: "000000000000000000000009", Created: time.Now().Add(-48 * time.Hour),
		Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}
	if err := a.st.CreateJob(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour) // pruning goes by the share's mtimes
	os.Chtimes(filepath.Join(share, "jobs", old.ID, "job.json"), past, past)
	for _, role := range []api.Role{api.RoleSource, api.RoleTarget} {
		a.st.PutStatus(context.Background(), old.ID, &api.JobStatus{Node: "a", Role: role, State: api.JobDone})
		os.Chtimes(filepath.Join(share, "jobs", old.ID, "status", "a."+string(role)+".json"), past, past)
	}
	os.Chtimes(filepath.Join(share, "jobs", old.ID, "status"), past, past)
	runWorker(t, a) // returns once the node is online; the prune runs right after
	waitUntil(t, func() bool {
		_, err := a.Job(context.Background(), old.ID)
		return errors.Is(err, api.ErrUnknownJob)
	}, "old job pruned at startup")
}

// shortTransient makes I/O error retries fast for a test.
func shortTransient(t *testing.T, budget time.Duration) {
	t.Helper()
	w, b := transientWait, transientBudget
	transientWait, transientBudget = 20*time.Millisecond, budget
	t.Cleanup(func() { transientWait, transientBudget = w, b })
}

// A soft-mounted share returns EIO while the server is away. The source
// pauses and resumes instead of failing the job.
func TestSourceSurvivesIOError(t *testing.T) {
	shortTransient(t, 10*time.Second)
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	master, fs := buildFaulty(t, mcfg)
	fs.eioWriteAt = 2
	s1, sroot := node(t, share, "s1")
	makeTree(t, mcfg.Roots[0].Path, 10, 600<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	if !fs.eioWriteHit {
		t.Fatal("fault never injected")
	}
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(sroot, "tree"))
}

// The target waits and reads the chunk again.
func TestTargetSurvivesIOErrors(t *testing.T) {
	shortTransient(t, 10*time.Second)
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	s1, fs := buildFaulty(t, scfg)
	fs.eioReadSeq, fs.eioReads = 1, 3
	makeTree(t, mroot, 5, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, _ := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	if fs.eioReadHits != 3 {
		t.Fatalf("faults injected: %d", fs.eioReadHits)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(scfg.Roots[0].Path, "tree"))
}

// A share that stays broken fails the job once the budget is spent.
func TestIOErrorsGiveUp(t *testing.T) {
	shortTransient(t, 300*time.Millisecond)
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	s1, fs := buildFaulty(t, scfg)
	fs.eioReadSeq, fs.eioReads = 0, -1
	makeTree(t, mroot, 3, 100<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, _ := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	j := waitJob(t, master, id, 30*time.Second)
	if j.State != api.JobFailed {
		t.Fatalf("state %s", j.State)
	}
	if st := ownStatus(j, "s1", api.RoleTarget); st == nil || !strings.Contains(st.Error, "input/output error") {
		t.Fatalf("target status: %+v", st)
	}
}

// stuckInbox blocks Fetch until released, like a slow request or a share
// that hangs while listing the inbox.
type stuckInbox struct {
	*store.Dir
	release chan struct{}
}

func (s *stuckInbox) Fetch(ctx context.Context) ([]*transport.Message, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return s.Dir.Fetch(ctx)
}

// A busy inbox loop must not make the node look offline.
func TestHeartbeatWhileInboxBusy(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(share, "a", nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	stuck := &stuckInbox{Dir: mb, release: make(chan struct{})}
	a := New(cfg, fsys, stuck, mb, Options{Version: "test"})
	t.Cleanup(func() { a.Close() })
	defer close(stuck.release)
	runWorker(t, a)
	b, _ := node(t, share, "b")
	time.Sleep(3 * cfg.OfflineAfter)
	ns, err := b.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.ID == "a" && !n.Online {
			t.Fatalf("a looks offline while its inbox loop is busy (last seen %s ago)", n.LastSeen)
		}
	}
}
