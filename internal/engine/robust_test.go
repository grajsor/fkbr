package engine

// Regression tests for what the 2026-10-21 lab robust-test found
// (~/fkbr-lab/robust/runs/2026-10-21/findings.md, F1..F17).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
	"fkbr/internal/transport/nfsmailbox"
)

// deepPath creates top/<20 directories of 240 bytes>/end.txt: a path of
// about 4.9 KB, longer than PATH_MAX, made one directory at a time.
func deepPath(t *testing.T, top string) []string {
	t.Helper()
	var parts []string
	r, err := os.OpenRoot(top)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		n := strings.Repeat(string(rune('a'+i)), 240)
		if err := r.Mkdir(n, 0o755); err != nil {
			t.Fatal(err)
		}
		sub, err := r.OpenRoot(n)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		r, parts = sub, append(parts, n)
	}
	defer r.Close()
	if err := r.WriteFile("end.txt", []byte("bottom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(parts, "end.txt")
}

// readDeep reads top/parts... through nested roots (a plain open of the
// whole path would fail with ENAMETOOLONG).
func readDeep(top string, parts []string) (string, error) {
	r, err := os.OpenRoot(top)
	if err != nil {
		return "", err
	}
	for _, p := range parts[:len(parts)-1] {
		sub, err := r.OpenRoot(p)
		r.Close()
		if err != nil {
			return "", err
		}
		r = sub
	}
	defer r.Close()
	b, err := r.ReadFile(parts[len(parts)-1])
	return string(b), err
}

// F1: one path longer than 4096 bytes failed the whole copy on every target.
func TestPathLongerThanPathMax(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	os.MkdirAll(aroot+"/deep", 0o755)
	os.WriteFile(aroot+"/deep/short.txt", []byte("short\n"), 0o644)
	parts := deepPath(t, aroot+"/deep")
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"deep"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("copy: %s %+v", j.State, j.Participants)
	}
	if got, err := readDeep(broot+"/in/deep", parts); err != nil || got != "bottom\n" {
		t.Fatalf("deep file after cp: %q %v", got, err)
	}
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "deep", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "synced", Mode: api.SyncMirror}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	if got, err := readDeep(broot+"/synced", parts); err != nil || got != "bottom\n" {
		t.Fatalf("deep file after sync: %q %v", got, err)
	}
	nothingToDo(t, mustPlan(t, a, req))
}

// F2: sync mangled names that are not valid UTF-8 (snapshots and requests
// are JSON): two such names failed the plan, one was "vanished since it was
// planned", and a mirror deletion of one silently did nothing.
func TestNamesNotUTF8(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	src := aroot + "/l1"
	names := []string{"caf\xe9", "na\xefve", "\xff\xfe", "dir\xe9/inner\xe9", "ok"}
	for _, n := range names {
		os.MkdirAll(src+"/"+n[:strings.LastIndexByte("/"+n, '/')], 0o755)
		if err := os.WriteFile(src+"/"+n, []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Symlink("caf\xe9", src+"/link\xe9")
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "l1", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "synced", Mode: api.SyncMirror}
	applyAndWait(t, a, mustPlan(t, a, req), false)
	sameTree(t, src, broot+"/synced")
	nothingToDo(t, mustPlan(t, a, req))
	// a mirror deletes such a name on the target
	os.Remove(src + "/caf\xe9")
	applyAndWait(t, a, mustPlan(t, a, req), false)
	if _, err := os.Lstat(broot + "/synced/caf\xe9"); !os.IsNotExist(err) {
		t.Fatalf("caf\\xe9 not deleted by the mirror: %v", err)
	}
	sameTree(t, src, broot+"/synced")
	// listing, cp of a named path, mkdir and rm through the API
	ctx := context.Background()
	l, err := a.List(ctx, api.ListRequest{Node: "b", Root: "data", Path: "synced"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range l.Entries {
		found = found || e.Name == "na\xefve"
	}
	if !found {
		t.Fatalf("remote listing lost na\\xefve: %+v", l.Entries)
	}
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"l1/na\xefve"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in\xe9"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("cp: %s %+v", j.State, j.Participants)
	}
	if got, err := os.ReadFile(broot + "/in\xe9/na\xefve"); err != nil || string(got) != "na\xefve" {
		t.Fatalf("cp of a named non-UTF-8 path: %q %v", got, err)
	}
	if err := a.Mkdir(ctx, api.MkdirRequest{Node: "b", Root: "data", Path: "new\xe9"}); err != nil {
		t.Fatal(err)
	}
	if res, err := a.Remove(ctx, api.RemoveRequest{Node: "b", Root: "data", Paths: []string{"new\xe9", "in\xe9"}}); err != nil || res.Removed != 2 {
		t.Fatalf("rm: %+v %v", res, err)
	}
	for _, p := range []string{"new\xe9", "in\xe9"} {
		if _, err := os.Lstat(broot + "/" + p); !os.IsNotExist(err) {
			t.Errorf("%q still there", p)
		}
	}
}

// F3: a target in two target sets got two jobs; the later one installed
// into folders whose mtime the first had already set, and the next sync
// was not empty.
func TestSyncTwoGroupsFolderMtimes(t *testing.T) {
	share := t.TempDir()
	s, sroot := node(t, share, "s")
	acfg := nodeCfg(t, share, "a")
	acfg.Transfer.MaxJobs = 1 // its two jobs run one after the other: the race, every time
	a := build(t, acfg)
	b, broot := node(t, share, "b")
	for _, e := range []*Engine{s, a, b} {
		runWorker(t, e)
	}
	for round := range 5 {
		src := fmt.Sprintf("%s/t%d", sroot, round)
		dst := fmt.Sprintf("t%d", round)
		for d := range 6 {
			for f := range 20 {
				p := fmt.Sprintf("%s/d%d/f%02d", src, d, f)
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte(p), 0o644)
			}
		}
		old := time.Unix(1500000000, 0)
		filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
			if de.IsDir() {
				defer os.Chtimes(p, old, old)
			}
			return nil
		})
		// b already holds every other file (same mtime and size), a nothing:
		// a needs entries of two target sets ({a,b} and {a}).
		filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
			rel, _ := filepath.Rel(src, p)
			if !de.IsDir() && rel[len(rel)-1]%2 == 0 {
				q := filepath.Join(broot, dst, rel)
				os.MkdirAll(filepath.Dir(q), 0o755)
				b, _ := os.ReadFile(p)
				os.WriteFile(q, b, 0o644)
				fi, _ := os.Stat(p)
				os.Chtimes(q, fi.ModTime(), fi.ModTime())
			}
			return nil
		})
		req := api.SyncRequest{SrcRoot: "data", SrcPath: dst, DstNodes: []string{"a", "b"}, DstRoot: "data", DstPath: dst, Mode: api.SyncMirror}
		plan := mustPlan(t, s, req)
		if len(plan.Groups) < 2 {
			t.Fatalf("round %d: want two target sets, got %+v", round, plan.Groups)
		}
		// Whatever order the jobs run in, each one must restore the folders
		// it installs into.
		ents, err := s.SyncPlanEntries(context.Background(), plan.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range ents.Copies {
			in := map[string]bool{}
			for _, p := range g.Paths {
				in[strings.TrimSuffix(p, "/")] = true
			}
			for _, p := range g.Paths {
				if par := filepath.Dir(p); p != "./" && !in[par] {
					t.Fatalf("round %d: group %v installs %s but does not carry its folder %s", round, g.Targets, p, par)
				}
			}
		}
		applyAndWait(t, s, plan, false)
		nothingToDo(t, mustPlan(t, s, req))
	}
}

// F6: a restart right after SIGTERM failed while the old worker was still
// pausing its jobs ("another worker is already running"): now the new one
// waits for a stopping worker, and only for a stopping one.
func TestRestartWaitsForStoppingWorker(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	l1, _, err := lockWorker(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lockWorker(dir, log); !errors.Is(err, ErrWorkerRunning) {
		t.Fatalf("a running worker's lock: %v", err)
	}
	l1.state("stopping")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Remove(dir + "/worker.lock") // what an exiting process does
		l1.release()
	}()
	start := time.Now()
	l2, waited, err := lockWorker(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.release()
	if waited != os.Getpid() || time.Since(start) < 250*time.Millisecond {
		t.Errorf("waited for %d after %s", waited, time.Since(start))
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dir+"/worker.lock", &st); err != nil || st.Ino != l2.ino {
		t.Errorf("the new lock is not on the lock path: %v", err)
	}
	if _, _, err := lockWorker(dir, log); !errors.Is(err, ErrWorkerRunning) {
		t.Fatalf("after the hand-over: %v", err)
	}
}

// F7: a stopping target applied every chunk that was already staged (9 x
// 128 MiB in the lab, until kill -9) before it let go.
func TestStoppingTargetLetsGoBetweenChunks(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	acfg.Transfer.JobQuota, acfg.Transfer.StagingQuota = 8<<20, 16<<20 // stage far ahead
	a := build(t, acfg)
	aroot := acfg.Roots[0].Path
	bcfg := nodeCfg(t, share, "b")
	b, bf := buildFaulty(t, bcfg)
	bf.openDelay = 100 * time.Millisecond
	runWorker(t, a)
	stopB := runWorker(t, b)
	makeTree(t, aroot, 2, 6<<20) // ~100 chunks of 64 KiB
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		j, _ := a.Job(ctx, id)
		return j != nil && targetChunks(j, "b") >= 3
	}, "target b applying chunks")
	start := time.Now()
	stopB()
	if d := time.Since(start); d > time.Second {
		t.Errorf("stopping the target took %s", d)
	}
	b2, _ := buildFaulty(t, bcfg)
	runWorker(t, b2)
	if j := waitJob(t, a, id, 60*time.Second); j.State != api.JobDone {
		t.Fatalf("after the restart: %s %+v", j.State, j.Participants)
	}
	sameTree(t, aroot+"/tree", bcfg.Roots[0].Path+"/in/tree")
}

func targetChunks(j *api.Job, node string) int {
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && p.Node == node {
			return p.Chunks
		}
	}
	return 0
}

// F14: a job waited forever for its only target once that went offline for
// good, holding the source's job slot (every later job from it queued).
func TestOfflineLastTargetDetached(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	acfg.Transfer.DetachAfter = 2 * time.Second
	acfg.Transfer.MaxJobs = 1
	a := build(t, acfg)
	aroot := acfg.Roots[0].Path
	bcfg := nodeCfg(t, share, "b")
	b, bf := buildFaulty(t, bcfg)
	bf.openDelay = 200 * time.Millisecond
	c, croot := node(t, share, "c")
	runWorker(t, a)
	stopB := runWorker(t, b)
	runWorker(t, c)
	makeTree(t, aroot, 2, 3<<20)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { j, _ := a.Job(ctx, id); return j != nil && targetChunks(j, "b") >= 1 }, "b receiving")
	stopB() // and it does not come back
	j := waitJob(t, a, id, 30*time.Second)
	if j.State != api.JobPartial || !j.IsDetached("b") {
		t.Fatalf("job with its only target gone: %s, detached %+v", j.State, j.Detached)
	}
	// the source's one job slot is free again
	id2, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"c"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id2, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("next job: %s", j.State)
	}
	sameTree(t, aroot+"/tree", croot+"/in/tree")
	// back again: a catch-up delivers
	b2 := build(t, bcfg)
	runWorker(t, b2)
	cu, err := a.CatchUp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, cu, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("catch-up: %s %+v", j.State, j.Participants)
	}
	sameTree(t, aroot+"/tree", bcfg.Roots[0].Path+"/in/tree")
}

// F14: a target that died after its last ack (the source finished) and
// never came back left the job "running" forever, forgotten or not.
func TestTargetGoneAfterSourceFinished(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, _ := node(t, share, "b")
	runWorker(t, a)
	stopB := runWorker(t, b)
	ctx := context.Background()
	j := &api.Job{ID: "18e0000000000000aaaaaaaa", Initiator: "a", Request: api.TransferRequest{SrcNode: "a", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"b"}, DstRoot: "data"}}
	if err := a.st.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	a.st.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobDone})
	b.st.PutStatus(ctx, j.ID, &api.JobStatus{Node: "b", Role: api.RoleTarget, State: api.JobRunning, Chunks: 1})
	stopB()
	if got, _ := a.Job(ctx, j.ID); got.State != api.JobRunning {
		t.Fatalf("while b may still come back: %s", got.State)
	}
	waitUntil(t, func() bool {
		ns, _ := a.Nodes(ctx)
		for _, n := range ns {
			if n.ID == "b" && !n.Online {
				return true
			}
		}
		return false
	}, "b offline")
	if err := a.Forget(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Job(ctx, j.ID)
	if got.State != api.JobPartial || !got.IsDetached("b") {
		t.Fatalf("after b was forgotten: %s %+v", got.State, got.Detached)
	}
	js, _ := a.Jobs(ctx)
	if len(js) != 1 || js[0].State != api.JobPartial {
		t.Fatalf("listing: %+v", js)
	}
}

// Robust test 2, F4: a job whose source was forgotten before it wrote done
// stayed running forever, though every target was done.
func TestSourceGoneAfterTargetsFinished(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, _ := node(t, share, "b")
	c, _ := node(t, share, "c")
	stopA := runWorker(t, a)
	runWorker(t, b)
	runWorker(t, c)
	ctx := context.Background()
	mk := func(id string, cState api.JobState) {
		j := &api.Job{ID: id, Initiator: "a", Request: api.TransferRequest{SrcNode: "a", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"b", "c"}, DstRoot: "data"}}
		if err := a.st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		a.st.PutStatus(ctx, id, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobRunning, Chunks: 1})
		b.st.PutStatus(ctx, id, &api.JobStatus{Node: "b", Role: api.RoleTarget, State: api.JobDone, Chunks: 1})
		c.st.PutStatus(ctx, id, &api.JobStatus{Node: "c", Role: api.RoleTarget, State: cState, Chunks: 1})
	}
	mk("18e0000000000000aaaaaaa1", api.JobDone)
	mk("18e0000000000000aaaaaaa2", api.JobFailed)
	mk("18e0000000000000aaaaaaa3", api.JobRunning)
	stopA()
	if got, _ := b.Job(ctx, "18e0000000000000aaaaaaa1"); got.State != api.JobRunning {
		t.Fatalf("while a may still come back: %s", got.State)
	}
	waitUntil(t, func() bool {
		ns, _ := b.Nodes(ctx)
		for _, n := range ns {
			if n.ID == "a" && !n.Online {
				return true
			}
		}
		return false
	}, "a offline")
	if err := b.Forget(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]api.JobState{
		"18e0000000000000aaaaaaa1": api.JobDone,
		"18e0000000000000aaaaaaa2": api.JobFailed,
		// c has not ended: c fails the job itself once it misses a
		"18e0000000000000aaaaaaa3": api.JobRunning,
	} {
		got, _ := b.Job(ctx, id)
		if got.State != want {
			t.Errorf("%s: %s, want %s (%+v)", id, got.State, want, got.Participants)
		}
	}
	js, _ := b.Jobs(ctx)
	for _, j := range js {
		if j.ID == "18e0000000000000aaaaaaa1" && j.State != api.JobDone {
			t.Errorf("listing: %s", j.State)
		}
	}
}

// F15: a node whose info.json was damaged stayed invisible for good (it kept
// heartbeating), and such an entry could not be forgotten.
func TestDamagedRegistryEntry(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, _ := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	junk := share + "/nodes/b/.junk"
	os.WriteFile(junk, []byte("\x00\xffnot json"), 0o644)
	os.Rename(junk, share+"/nodes/b/info.json")
	ctx := context.Background()
	waitUntil(t, func() bool {
		ns, _ := a.Nodes(ctx)
		for _, n := range ns {
			if n.ID == "b" && n.Online {
				return true
			}
		}
		return false
	}, "b registered again")
	// a dead node with a damaged entry can be forgotten
	os.MkdirAll(share+"/nodes/zz", 0o777)
	os.WriteFile(share+"/nodes/zz/info.json", []byte("{junk"), 0o644)
	os.WriteFile(share+"/nodes/zz/heartbeat", []byte("w1 1 x\n"), 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(share+"/nodes/zz/heartbeat", old, old)
	if err := a.Forget(ctx, "zz"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(share + "/nodes/zz"); !os.IsNotExist(err) {
		t.Fatalf("damaged entry still there: %v", err)
	}
}

// failSend refuses to deliver requests to one node until a deadline (its
// inbox unwritable for a while).
type failSend struct {
	transport.Transport
	to    string
	until time.Time
}

func (f *failSend) Send(ctx context.Context, m *transport.Message) error {
	if m.To == f.to && time.Now().Before(f.until) {
		return &os.PathError{Op: "mkdir", Path: "inbox/" + f.to + "/new", Err: syscall.EACCES}
	}
	return f.Transport.Send(ctx, m)
}

// F17: one target's inbox unwritable for a moment failed the whole fan-out
// at once, the healthy target included.
func TestNotifyFailureIsNotFatal(t *testing.T) {
	old := renotifyEvery
	renotifyEvery = 300 * time.Millisecond
	t.Cleanup(func() { renotifyEvery = old })
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	fsys, err := localfs.Open(acfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(share, "a", nfsmailbox.Options{Poll: acfg.Poll.Active, OfflineAfter: acfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	a := New(acfg, fsys, &failSend{Transport: mb, to: "b", until: time.Now().Add(2 * time.Second)}, mb, Options{Version: "test"})
	t.Cleanup(func() { a.Close() })
	aroot := acfg.Roots[0].Path
	b, broot := node(t, share, "b")
	c, croot := node(t, share, "c")
	runWorker(t, a)
	runWorker(t, b)
	runWorker(t, c)
	makeTree(t, aroot, 3, 100<<10)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b", "c"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("%s %+v", j.State, j.Participants)
	}
	sameTree(t, aroot+"/tree", broot+"/in/tree")
	sameTree(t, aroot+"/tree", croot+"/in/tree")
}

func (f *faultStore) Acked(ctx context.Context, src, id, node string) (int, error) {
	if node == f.hideAcks {
		return 0, nil
	}
	return f.Mailbox.Acked(ctx, src, id, node)
}

// Robust test 2, F1: a source back from a stall on the share read no acks
// from a target that had already installed everything and said done, and
// detached it for "no progress": the job ended partial.
func TestDoneTargetNotDetached(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	acfg.Transfer.DetachAfter = 3 * time.Second
	a, af := buildFaulty(t, acfg)
	af.hideAcks = "b"
	aroot := acfg.Roots[0].Path
	b, broot := node(t, share, "b")
	c, croot := node(t, share, "c")
	runWorker(t, a)
	runWorker(t, b)
	runWorker(t, c)
	// small: b is done long before detach_after, only its acks are not seen
	makeTree(t, aroot, 1, 1<<10)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b", "c"}, DstRoot: "data", DstDir: "in"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, a, id, 30*time.Second)
	if j.State != api.JobDone || len(j.Detached) > 0 {
		t.Fatalf("job: %s, detached %+v, participants %+v", j.State, j.Detached, j.Participants)
	}
	sameTree(t, aroot+"/tree", broot+"/in/tree")
	sameTree(t, aroot+"/tree", croot+"/in/tree")
}

// Robust test 2, F3: a target restarted mid-receive and then detached kept
// the job's half-received staging in its root for good.
func TestStaleStagingSwept(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, broot := node(t, share, "b")
	ctx := context.Background()
	mkJob := func(id string, bState api.JobState) {
		j := &api.Job{ID: id, Initiator: "a", Request: api.TransferRequest{SrcNode: "a", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"b"}, DstRoot: "data"}}
		if err := a.st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		if bState != "" {
			b.st.PutStatus(ctx, id, &api.JobStatus{Node: "b", Role: api.RoleTarget, State: bState, Chunks: 1})
		}
	}
	const (
		detached = "18e0000000000000bbbbbbb1"
		goneOld  = "18e0000000000000bbbbbbb2"
		goneNew  = "18e0000000000000bbbbbbb3"
		running  = "18e0000000000000bbbbbbb4"
		ended    = "18e0000000000000bbbbbbb5"
	)
	mkJob(detached, api.JobRunning)
	a.st.Detach(ctx, detached, api.Detached{Node: "b", Reason: "offline"})
	mkJob(running, api.JobRunning)
	mkJob(ended, api.JobFailed)
	staging := broot + "/" + receive.StagingDir
	for _, id := range []string{detached, goneOld, goneNew, running, ended} {
		os.MkdirAll(staging+"/"+id+"/tree/d", 0o755)
		os.WriteFile(staging+"/"+id+"/tree/d/f", []byte("half"), 0o644)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(staging+"/"+goneOld, old, old)
	b.sweepStaging(ctx)
	for id, keep := range map[string]bool{detached: false, goneOld: false, goneNew: true, running: true, ended: false} {
		_, err := os.Stat(staging + "/" + id)
		if kept := err == nil; kept != keep {
			t.Errorf("%s: kept=%v, want %v", id, kept, keep)
		}
	}
}

// lostEntry reports the registry entry missing on the first heartbeat, as
// if info.json was damaged right after Register.
type lostEntry struct {
	transport.Transport
	hits atomic.Int32
}

func (l *lostEntry) Heartbeat(ctx context.Context, token string) (string, error) {
	if l.hits.Add(1) == 1 {
		return "", transport.ErrNotRegistered
	}
	return l.Transport.Heartbeat(ctx, token)
}

// A worker whose entry was damaged between Register and its first
// heartbeat stopped ("heartbeat: registry entry missing", seen once under
// -race in TestDamagedRegistryEntry); it registers again instead.
func TestEntryLostAtStart(t *testing.T) {
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
	lost := &lostEntry{Transport: mb}
	a := New(cfg, fsys, lost, mb, Options{Version: "test"})
	t.Cleanup(func() { a.Close() })
	runWorker(t, a)
	if lost.hits.Load() < 2 {
		t.Fatalf("heartbeats: %d", lost.hits.Load())
	}
	select {
	case <-a.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("worker never ready")
	}
}
