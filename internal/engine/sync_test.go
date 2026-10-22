package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
)

func applyAndWait(t *testing.T, e *Engine, plan *api.SyncPlan, force bool) *api.SyncPlan {
	t.Helper()
	p, err := e.ApplySync(context.Background(), plan.ID, force)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.DeleteErrors) > 0 || len(p.Problems) > 0 {
		t.Fatalf("apply problems: %v %v", p.DeleteErrors, p.Problems)
	}
	for _, id := range p.Jobs {
		if j := waitJob(t, e, id, 30*time.Second); j.State != api.JobDone {
			t.Fatalf("job %s: %s %+v", id, j.State, j.Participants)
		}
	}
	return p
}

func target(p *api.SyncPlan, node string) api.SyncTarget {
	for _, t := range p.Targets {
		if t.Node == node {
			return t
		}
	}
	return api.SyncTarget{}
}

func nothingToDo(t *testing.T, p *api.SyncPlan) {
	t.Helper()
	for _, tg := range p.Targets {
		if tg.Copy+tg.Delete+tg.Replace != 0 {
			t.Fatalf("plan not converged on %s: %+v", tg.Node, tg)
		}
	}
	if len(p.Groups) != 0 {
		t.Fatalf("groups on a converged plan: %+v", p.Groups)
	}
}

func TestSyncIncrementalConverges(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	makeTree(t, mroot, 30, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	ctx := context.Background()
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "mirror"}

	plan, err := master.PlanSync(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if tg := target(plan, "s1"); tg.Copy != 34 || plan.SrcEntries != 34 { // 2 dirs + 30 + big + link
		t.Fatalf("first plan: src %d target %+v", plan.SrcEntries, tg)
	}
	applyAndWait(t, master, plan, false)
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "mirror"))

	plan, err = master.PlanSync(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	nothingToDo(t, plan)

	// Change one file, add one, chmod one; an extra file on the target stays.
	os.WriteFile(filepath.Join(mroot, "tree/sub/f003"), []byte("changed content\n"), 0o640)
	os.WriteFile(filepath.Join(mroot, "tree/new.txt"), []byte("new"), 0o644)
	os.Chmod(filepath.Join(mroot, "tree/sub/f007"), 0o600)
	os.WriteFile(filepath.Join(sroot, "mirror/extra"), []byte("x"), 0o644)
	plan, _ = master.PlanSync(ctx, req)
	tg := target(plan, "s1")
	// f003, f007, new.txt, plus "." (new entry) and sub (f003 replaced in it).
	if tg.Copy != 5 || tg.Delete != 0 {
		t.Fatalf("incremental plan: %+v", tg)
	}
	applyAndWait(t, master, plan, false)
	if _, err := os.Stat(filepath.Join(sroot, "mirror/extra")); err != nil {
		t.Error("incremental sync removed an extra file")
	}
	// sameTree walks the source side only, so the extra file is fine.
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "mirror"))
	nothingToDo(t, mustPlan(t, master, req))
}

func TestSyncMirrorDeletesWithLimit(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	makeTree(t, mroot, 5, 10<<10)
	runWorker(t, master)
	runWorker(t, s1)
	ctx := context.Background()
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror, MaxDeletes: 2}
	applyAndWait(t, master, mustPlan(t, master, req), false)

	// Three extras (one a directory tree) and a type change.
	os.MkdirAll(filepath.Join(sroot, "m/olddir/deep"), 0o755)
	os.WriteFile(filepath.Join(sroot, "m/olddir/deep/x"), nil, 0o644)
	os.WriteFile(filepath.Join(sroot, "m/stale1"), nil, 0o644)
	os.WriteFile(filepath.Join(sroot, "m/stale2"), nil, 0o644)
	os.Remove(filepath.Join(mroot, "tree/sub/f001"))
	os.Mkdir(filepath.Join(mroot, "tree/sub/f001"), 0o755) // file -> dir

	plan := mustPlan(t, master, req)
	tg := target(plan, "s1")
	if tg.Delete != 3 || tg.Replace != 1 || !tg.OverLimit {
		t.Fatalf("mirror plan: %+v", tg)
	}
	if _, err := master.ApplySync(ctx, plan.ID, false); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("over-limit apply without force: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sroot, "m/stale1")); err != nil {
		t.Fatal("refused apply must not delete anything")
	}
	applyAndWait(t, master, plan, true)
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "m"))
	for _, p := range []string{"olddir", "stale1", "stale2"} {
		if _, err := os.Stat(filepath.Join(sroot, "m", p)); !os.IsNotExist(err) {
			t.Errorf("%s not deleted", p)
		}
	}
	if _, err := master.ApplySync(ctx, plan.ID, true); err == nil {
		t.Error("a plan must not apply twice")
	}
	nothingToDo(t, mustPlan(t, master, req))
}

// The TUI and a CLI apply the same plan through one node at once: it runs
// once. Another node applying it afterwards is refused too.
func TestSyncPlanAppliesOnceAtATime(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	makeTree(t, mroot, 5, 10<<10)
	runWorker(t, master)
	runWorker(t, s1)
	ctx := context.Background()
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror}
	applyAndWait(t, master, mustPlan(t, master, req), false)
	os.WriteFile(filepath.Join(sroot, "m/stale"), nil, 0o644)
	os.WriteFile(filepath.Join(mroot, "tree/new"), []byte("n"), 0o644)
	plan := mustPlan(t, master, req)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var applied []*api.SyncPlan
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := master.ApplySync(ctx, plan.ID, false); err == nil {
				mu.Lock()
				applied = append(applied, p)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(applied) != 1 || len(applied[0].Jobs) != 1 {
		t.Fatalf("%d applies succeeded, want 1", len(applied))
	}
	if _, err := s1.ApplySync(ctx, plan.ID, false); err == nil {
		t.Error("another node applied the plan again")
	}
	if j := waitJob(t, master, applied[0].Jobs[0], 30*time.Second); j.State != api.JobDone {
		t.Fatalf("job: %s", j.State)
	}
	if jobs, _ := master.Jobs(ctx); len(jobs) != 2 { // the first sync and this one
		t.Errorf("%d jobs on the share, want 2", len(jobs))
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "m"))
}

// A type change deletes the target's entry first; if the source entry is
// gone by the time it is sent, the job says so and the next sync settles.
func TestSyncSourceGoneAfterDeletes(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, sroot := node(t, share, "s1")
	makeTree(t, mroot, 5, 10<<10)
	runWorker(t, master)
	runWorker(t, s1)
	ctx := context.Background()
	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "m", Mode: api.SyncMirror}
	applyAndWait(t, master, mustPlan(t, master, req), false)
	os.Remove(filepath.Join(mroot, "tree/sub/f001"))
	os.MkdirAll(filepath.Join(mroot, "tree/sub/f001"), 0o755) // file -> dir
	os.WriteFile(filepath.Join(mroot, "tree/sub/f001/in"), []byte("i"), 0o644)
	plan := mustPlan(t, master, req)
	if tg := target(plan, "s1"); tg.Replace != 1 {
		t.Fatalf("plan: %+v", tg)
	}
	os.RemoveAll(filepath.Join(mroot, "tree/sub/f001"))

	p, err := master.ApplySync(ctx, plan.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	for _, id := range p.Jobs {
		j := waitJob(t, master, id, 30*time.Second)
		for _, pt := range j.Participants {
			problems = append(problems, pt.Problems...)
		}
	}
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "f001: vanished") {
		t.Errorf("job problems: %q", problems)
	}
	if _, err := os.Lstat(filepath.Join(sroot, "m/sub/f001")); !os.IsNotExist(err) {
		t.Errorf("target f001: %v", err)
	}
	nothingToDo(t, mustPlan(t, master, req))
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(sroot, "m"))
}

func TestSyncFanOutGroups(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	c1, c2 := withRoles(nodeCfg(t, share, "s1"), "slave"), withRoles(nodeCfg(t, share, "s2"), "slave")
	makeTree(t, mroot, 10, 50<<10)
	runWorker(t, master)
	runWorker(t, build(t, c1))
	runWorker(t, build(t, c2))
	// s1 gets the tree first.
	applyAndWait(t, master, mustPlan(t, master, api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: "t"}), false)

	req := api.SyncRequest{SrcRoot: "data", SrcPath: "tree", DstNodes: []string{"@slave"}, DstRoot: "data", DstPath: "t"}
	plan := mustPlan(t, master, req)
	if target(plan, "s1").Copy != 0 || target(plan, "s2").Copy != 14 {
		t.Fatalf("targets: %+v", plan.Targets)
	}
	if len(plan.Groups) != 1 || len(plan.Groups[0].Targets) != 1 || plan.Groups[0].Targets[0] != "s2" {
		t.Fatalf("groups: %+v", plan.Groups)
	}
	applyAndWait(t, master, plan, false)
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(c2.Roots[0].Path, "t"))

	// A change needed by both is bundled once for both.
	os.WriteFile(filepath.Join(mroot, "tree/both"), []byte("b"), 0o644)
	plan = mustPlan(t, master, req)
	if len(plan.Groups) != 1 || len(plan.Groups[0].Targets) != 2 {
		t.Fatalf("shared change groups: %+v", plan.Groups)
	}
	applyAndWait(t, master, plan, false)
	nothingToDo(t, mustPlan(t, master, req))
}

func TestSyncValidation(t *testing.T) {
	share := t.TempDir()
	master, _ := node(t, share, "master")
	runWorker(t, master)
	ctx := context.Background()
	if _, err := master.PlanSync(ctx, api.SyncRequest{SrcRoot: "data", SrcPath: "a", DstNodes: []string{"master"}, DstRoot: "data", DstPath: "a/b"}); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("overlap: %v", err)
	}
	if _, err := master.PlanSync(ctx, api.SyncRequest{SrcRoot: "data", SrcPath: "missing", DstNodes: []string{"master"}, DstRoot: "data", DstPath: "x"}); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("missing source: %v", err)
	}
	if _, err := master.PlanSync(ctx, api.SyncRequest{SrcRoot: "data", DstNodes: []string{"master"}, DstRoot: "data", DstPath: "x", Mode: "bogus"}); err == nil {
		t.Error("bad mode accepted")
	}
}

func mustPlan(t *testing.T, e *Engine, req api.SyncRequest) *api.SyncPlan {
	t.Helper()
	p, err := e.PlanSync(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
