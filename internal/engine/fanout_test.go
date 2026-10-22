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
	"fkbr/internal/config"
	"fkbr/internal/receive"
	"fkbr/internal/transport/nfsmailbox"
)

func withRoles(cfg *config.Config, roles ...string) *config.Config {
	cfg.Roles = roles
	return cfg
}

func targetState(j *api.Job, node string) api.JobState {
	return roleState(j, api.RoleTarget, node)
}

func TestGroupWithOfflineMemberAndCatchUp(t *testing.T) {
	share := t.TempDir()
	mcfg := withRoles(nodeCfg(t, share, "master"), "master", "slave") // own role is excluded
	c1 := withRoles(nodeCfg(t, share, "s1"), "slave")
	c2 := withRoles(nodeCfg(t, share, "s2"), "slave")
	c3 := withRoles(nodeCfg(t, share, "s3"), "slave")
	master := build(t, mcfg)
	makeTree(t, mcfg.Roots[0].Path, 5, 100<<10)
	runWorker(t, master)
	runWorker(t, build(t, c1))
	runWorker(t, build(t, c2))
	stop3 := runWorker(t, build(t, c3))
	stop3()
	time.Sleep(mcfg.OfflineAfter + 100*time.Millisecond) // s3 now offline

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"@slave"}, DstRoot: "data", DstDir: "g",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	if j.State != api.JobPartial || strings.Join(j.Request.DstNodes, ",") != "s1,s2,s3" {
		t.Fatalf("state %s targets %v: %+v", j.State, j.Request.DstNodes, j.Participants)
	}
	if len(j.Detached) != 1 || j.Detached[0].Node != "s3" || j.Detached[0].Reason != "offline at submit" {
		t.Fatalf("detached: %+v", j.Detached)
	}
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(c1.Roots[0].Path, "g", "tree"))
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(c2.Roots[0].Path, "g", "tree"))

	if _, err := master.CatchUp(context.Background(), id); !errors.Is(err, api.ErrNodeOffline) {
		t.Fatalf("catch-up with s3 offline: %v", err)
	}
	s3 := build(t, c3)
	runWorker(t, s3)
	cid, err := master.CatchUp(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	cj := waitJob(t, master, cid, 20*time.Second)
	if cj.State != api.JobDone || cj.CatchUpOf != id || strings.Join(cj.Request.DstNodes, ",") != "s3" {
		t.Fatalf("catch-up: %s %+v", cj.State, cj)
	}
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(c3.Roots[0].Path, "g", "tree"))
	// s3's original receive request is redelivered on restart and must
	// conclude as detached without touching anything.
	j, _ = master.Job(context.Background(), id)
	if st := targetState(j, "s3"); st != "" && st != api.JobDetached {
		t.Errorf("s3 state on original job: %s", st)
	}
}

func TestOfflineTargetDetachedMidTransfer(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, r1 := node(t, share, "s1")
	c2 := nodeCfg(t, share, "s2")
	makeTree(t, mroot, 5, 3<<20) // far beyond the 256 KiB job quota
	runWorker(t, master)
	runWorker(t, s1)
	stop2 := runWorker(t, build(t, c2))

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1", "s2"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Let s2 take part, then kill it. It goes offline and must be dropped.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := master.st.Acked(context.Background(), "master", id, "s2"); n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("s2 made no progress")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop2()
	j := waitJob(t, master, id, 30*time.Second)
	if j.State != api.JobPartial || len(j.Detached) != 1 || j.Detached[0].Reason != "offline" || j.Detached[0].Chunks < 2 {
		t.Fatalf("state %s detached %+v", j.State, j.Detached)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(r1, "in", "tree"))

	// s2 comes back: it learns it was dropped and cleans its staging.
	// Poll the raw status: Job() already shows s2's stale status as
	// detached, before s2 itself has acted.
	runWorker(t, build(t, c2))
	deadline = time.Now().Add(10 * time.Second)
	for {
		raw, _ := master.st.GetJob(context.Background(), id)
		if targetState(raw, "s2") == api.JobDetached {
			j, _ = master.Job(context.Background(), id)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("s2 never reported detached: %+v", j.Participants)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j.State != api.JobPartial {
		t.Errorf("job state after s2 returned: %s", j.State)
	}
	if _, err := os.Stat(filepath.Join(c2.Roots[0].Path, receive.StagingDir)); !os.IsNotExist(err) {
		t.Errorf("s2 staging not cleaned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c2.Roots[0].Path, "in")); !os.IsNotExist(err) {
		t.Errorf("detached s2 must not install a partial copy")
	}
}

func TestStalledTargetDetached(t *testing.T) {
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	mcfg.Transfer.DetachAfter = 300 * time.Millisecond
	master := build(t, mcfg)
	s1, r1 := node(t, share, "s1")
	makeTree(t, mcfg.Roots[0].Path, 5, 1<<20)
	runWorker(t, master)
	runWorker(t, s1)

	// s2 looks alive (registered, heartbeating) but never consumes.
	mb, err := nfsmailbox.Open(share, "s2", nfsmailbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mb.Register(context.Background(), api.NodeInfo{ID: "s2", Roots: []api.Root{{ID: "data", Writable: true}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			mb.Heartbeat(ctx, "")
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)

	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1", "s2"}, DstRoot: "data",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	if j.State != api.JobPartial || len(j.Detached) != 1 || !strings.HasPrefix(j.Detached[0].Reason, "no progress") {
		t.Fatalf("state %s detached %+v", j.State, j.Detached)
	}
	sameTree(t, filepath.Join(mcfg.Roots[0].Path, "tree"), filepath.Join(r1, "tree"))
}

func TestFailedTargetDetached(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, r1 := node(t, share, "s1")
	s2, r2 := node(t, share, "s2")
	makeTree(t, mroot, 5, 200<<10)
	os.Chmod(r2, 0o555) // configured writable, but the directory is not
	defer os.Chmod(r2, 0o755)
	for _, e := range []*Engine{master, s1, s2} {
		runWorker(t, e)
	}
	id, err := master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1", "s2"}, DstRoot: "data",
	})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	if j.State != api.JobPartial || len(j.Detached) != 1 || !strings.HasPrefix(j.Detached[0].Reason, "failed:") {
		t.Fatalf("state %s detached %+v participants %+v", j.State, j.Detached, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(r1, "tree"))

	// With a single target, a failure fails the job.
	id, _ = master.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s2"}, DstRoot: "data",
	})
	if j := waitJob(t, master, id, 20*time.Second); j.State != api.JobFailed {
		t.Fatalf("single failing target: %s", j.State)
	}
}

func TestAcceptWritesFrom(t *testing.T) {
	share := t.TempDir()
	mcfg := withRoles(nodeCfg(t, share, "master"), "master")
	master := build(t, mcfg)
	c1 := nodeCfg(t, share, "s1")
	c1.AcceptWritesFrom = []string{"@master"}
	s1 := build(t, c1)
	s2, _ := node(t, share, "s2")
	for _, e := range []*Engine{master, s1, s2} {
		runWorker(t, e)
	}
	ctx := context.Background()
	req := api.TransferRequest{SrcNode: "master", SrcRoot: "data", Paths: []string{"."}, DstNodes: []string{"s1"}, DstRoot: "data"}
	if _, err := s2.Submit(ctx, req); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("s2 initiating a write to s1: %v", err)
	}
	if _, err := master.Submit(ctx, req); err != nil {
		t.Fatalf("master initiating: %v", err)
	}
	req.SrcNode = "s2"
	if _, err := s1.Submit(ctx, req); err != nil {
		t.Fatalf("s1 pulling into itself: %v", err)
	}
	// The receiver enforces the policy even if the initiator skipped it.
	if err := s1.acceptWrite(ctx, "s2"); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("receiver check: %v", err)
	}
	if err := s1.acceptWrite(ctx, "master"); err != nil {
		t.Fatalf("receiver check for master: %v", err)
	}
}

func TestDeriveState(t *testing.T) {
	st := func(node string, role api.Role, s api.JobState) api.JobStatus {
		return api.JobStatus{Node: node, Role: role, State: s}
	}
	j := &api.Job{Request: api.TransferRequest{DstNodes: []string{"a", "b"}}}
	if deriveState(j) != api.JobQueued {
		t.Error("no participants: queued")
	}
	j.Participants = []api.JobStatus{st("m", api.RoleSource, api.JobDone), st("a", api.RoleTarget, api.JobDone), st("b", api.RoleTarget, api.JobRunning)}
	if deriveState(j) != api.JobRunning {
		t.Error("b running: running")
	}
	j.Participants[0].State = api.JobRunning
	j.Participants[2].State = api.JobFailed
	if deriveState(j) != api.JobRunning {
		t.Error("b failed while source runs (may detach it): running")
	}
	j.Participants[0].State = api.JobFailed
	if deriveState(j) != api.JobFailed {
		t.Error("source gave up: failed")
	}
	j.Participants[0].State = api.JobDone
	j.Detached = []api.Detached{{Node: "b"}}
	if deriveState(j) != api.JobPartial {
		t.Errorf("b failed and detached: partial, got %s", deriveState(j))
	}
	j.Participants[2].State = api.JobDone
	j.Detached = nil
	if deriveState(j) != api.JobDone {
		t.Error("all done")
	}
}

func TestGroupAll(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	c, croot := node(t, share, "c")
	makeTree(t, mroot, 3, 10<<10)
	for _, e := range []*Engine{master, b, c} {
		runWorker(t, e)
	}
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"@all"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	if j.State != api.JobDone || strings.Join(j.Request.DstNodes, ",") != "b,c" {
		t.Fatalf("@all: %s %v", j.State, j.Request.DstNodes)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(broot, "tree"))
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(croot, "tree"))
}

func TestNodeIDCollisionRefused(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	// Another host claims the same ID while "a" is online.
	impostor := build(t, nodeCfg(t, share, "a"))
	impostor.info.Hostname = "some-other-host"
	if err := impostor.Run(context.Background()); !errors.Is(err, ErrNodeIDTaken) {
		t.Fatalf("collision: %v", err)
	}
}

func TestForget(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	b, _ := node(t, share, "b") // an online peer for @all
	c := nodeCfg(t, share, "c")
	runWorker(t, a)
	runWorker(t, b)
	stopC := runWorker(t, build(t, c))
	ctx := context.Background()
	if err := a.Forget(ctx, "c"); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("forget online node: %v", err)
	}
	if err := a.Forget(ctx, "a"); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("forget self: %v", err)
	}
	if err := a.Forget(ctx, "ghost"); !errors.Is(err, api.ErrUnknownNode) {
		t.Fatalf("forget unknown: %v", err)
	}
	stopC()
	waitUntil(t, func() bool {
		ns, _ := a.Nodes(ctx)
		for _, n := range ns {
			if n.ID == "c" {
				return !n.Online
			}
		}
		return false
	}, "c offline")
	if err := a.Forget(ctx, "c"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"nodes", "inbox", "reply"} {
		if _, err := os.Stat(filepath.Join(share, d, "c")); !os.IsNotExist(err) {
			t.Errorf("%s/c still there", d)
		}
	}
	// @all no longer includes it, so the job is done, not partial.
	makeTree(t, a.cfg.Roots[0].Path, 2, 1<<10)
	id, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"@all"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 20*time.Second); j.State != api.JobDone || strings.Join(j.Request.DstNodes, ",") != "b" {
		t.Fatalf("after forget: %s %v", j.State, j.Request.DstNodes)
	}
	// Started again, it simply re-registers.
	runWorker(t, build(t, c))
}

func registered(t *testing.T, e *Engine, id string) (api.Node, bool) {
	t.Helper()
	ns, err := e.Nodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.ID == id {
			return n, true
		}
	}
	return api.Node{}, false
}

func TestLeaveOnStop(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	visitor, _ := node(t, share, "visitor")
	visitor.LeaveOnStop()
	runWorker(t, visitor)()
	if _, ok := registered(t, a, "visitor"); ok {
		t.Fatal("visitor still registered after a clean stop")
	}
	for _, d := range []string{"nodes", "inbox", "reply"} {
		if _, err := os.Stat(filepath.Join(share, d, "visitor")); !os.IsNotExist(err) {
			t.Errorf("%s/visitor still there", d)
		}
	}
	// Without the flag (a daemon) the node stays, offline.
	mcfg := nodeCfg(t, share, "member")
	runWorker(t, build(t, mcfg))()
	if n, ok := registered(t, a, "member"); !ok || n.Transient {
		t.Fatalf("daemon entry: registered=%v transient=%v", ok, n.Transient)
	}
	// A TUI on a machine a daemon registered keeps that membership.
	tui := build(t, mcfg)
	tui.LeaveOnStop()
	stop := runWorker(t, tui)
	if n, _ := registered(t, a, "member"); n.Transient {
		t.Error("TUI turned a daemon's entry transient")
	}
	stop()
	if _, ok := registered(t, a, "member"); !ok {
		t.Fatal("TUI removed a node a daemon registered")
	}
	// A visitor's entry is marked while it runs; one left behind by a TUI
	// that died is taken over and removed by the next TUI.
	vcfg := nodeCfg(t, share, "visitor2")
	v := build(t, vcfg)
	v.LeaveOnStop()
	stopV := runWorker(t, v)
	if n, _ := registered(t, a, "visitor2"); !n.Transient {
		t.Error("visitor not marked transient")
	}
	v.leaveOnStop.Store(false) // dies without leaving
	stopV()
	v2 := build(t, vcfg)
	v2.LeaveOnStop()
	runWorker(t, v2)()
	if _, ok := registered(t, a, "visitor2"); ok {
		t.Error("TUI kept a stale TUI entry")
	}
}

func TestLeaveKeepsNodeNeededByJobs(t *testing.T) {
	self := "n"
	job := func(src string, dst []string, detached []string, st api.JobState) api.Job {
		j := api.Job{ID: "j1", Request: api.TransferRequest{SrcNode: src, DstNodes: dst}, State: st}
		for _, d := range detached {
			j.Detached = append(j.Detached, api.Detached{Node: d})
		}
		return j
	}
	for _, tc := range []struct {
		name string
		job  api.Job
		want bool
	}{
		{"source of running job", job(self, []string{"x"}, nil, api.JobRunning), true},
		{"target of queued job", job("x", []string{"y", self}, nil, api.JobQueued), true},
		{"detached target", job("x", []string{self}, []string{self}, api.JobRunning), false},
		{"finished job", job(self, []string{"x"}, nil, api.JobDone), false},
		{"not involved", job("x", []string{"y"}, nil, api.JobRunning), false},
	} {
		if got := neededBy([]api.Job{tc.job}, self); (got != "") != tc.want {
			t.Errorf("%s: neededBy = %q, want needed=%v", tc.name, got, tc.want)
		}
	}
}

func TestAutoForget(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	cfg.ForgetAfter = time.Hour
	a := build(t, cfg)
	runWorker(t, a)
	for _, id := range []string{"old", "recent"} {
		runWorker(t, build(t, nodeCfg(t, share, id)))()
	}
	long := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(share, "nodes", "old", "heartbeat"), long, long); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Prune(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := registered(t, a, "old"); ok {
		t.Error("node offline for 2h not forgotten")
	}
	if _, ok := registered(t, a, "recent"); !ok {
		t.Error("recently seen node forgotten")
	}
	if _, ok := registered(t, a, "a"); !ok {
		t.Error("self forgotten")
	}
	// Negative turns it off.
	cfg.ForgetAfter = -1
	if err := os.Chtimes(filepath.Join(share, "nodes", "recent", "heartbeat"), long, long); err != nil {
		t.Fatal(err)
	}
	if err := a.Prune(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, ok := registered(t, a, "recent"); !ok {
		t.Error("forgotten with forget_after off")
	}
}

// Named targets: an offline one fails the request, unless DetachOffline
// (the TUI's @dest), which records it as detached like an @role member.
func TestDetachOfflineNamedTargets(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	c := nodeCfg(t, share, "c")
	makeTree(t, aroot, 2, 1<<10) // into b and c
	runWorker(t, a)
	runWorker(t, b)
	stopC := runWorker(t, build(t, c))
	stopC()
	waitUntil(t, func() bool { n, _ := registered(t, a, "c"); return !n.Online }, "c offline")
	ctx := context.Background()
	req := api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b", "c"}, DstRoot: "data", DstDir: "d"}
	if _, err := a.Submit(ctx, req); !errors.Is(err, api.ErrNodeOffline) {
		t.Fatalf("offline named target without DetachOffline: %v", err)
	}
	req.DetachOffline = true
	id, err := a.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, a, id, 20*time.Second)
	if j.State != api.JobPartial || len(j.Detached) != 1 || j.Detached[0].Node != "c" {
		t.Fatalf("state %s detached %+v", j.State, j.Detached)
	}
	sameTree(t, filepath.Join(aroot, "tree"), filepath.Join(broot, "d", "tree"))
	req.DstNodes = []string{"@dest"}
	if _, err := a.Submit(ctx, req); err == nil || !strings.Contains(err.Error(), "marked nodes") {
		t.Fatalf("@dest outside the TUI: %v", err)
	}
}

// A target that failed (a link down for longer than transfers wait, say)
// is caught up like a detached one: catchup re-sends the job to it.
func TestCatchUpFailedTarget(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	c1 := nodeCfg(t, share, "s1")
	s1 := build(t, c1)
	makeTree(t, mroot, 5, 100<<10)
	runWorker(t, master)
	runWorker(t, s1)
	ctx := context.Background()
	id, err := master.Submit(ctx, api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s", j.State)
	}
	if _, err := master.CatchUp(ctx, id); err == nil {
		t.Fatal("catch-up of a job every target got")
	}
	// As if s1 had failed: its status says so, and nothing arrived.
	if err := s1.st.PutStatus(ctx, id, &api.JobStatus{Node: "s1", Role: api.RoleTarget, State: api.JobFailed, Error: "chunk 6: ssh link down"}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(c1.Roots[0].Path, "g"))
	cid, err := master.CatchUp(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	cj := waitJob(t, master, cid, 20*time.Second)
	if cj.State != api.JobDone || cj.CatchUpOf != id || strings.Join(cj.Request.DstNodes, ",") != "s1" {
		t.Fatalf("catch-up: %s %+v", cj.State, cj)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(c1.Roots[0].Path, "g", "tree"))
}
