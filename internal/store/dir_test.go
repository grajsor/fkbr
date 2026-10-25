package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

var ctx = context.Background()

func open(t *testing.T, share, self string) *Dir {
	t.Helper()
	m, err := Open(share, self, Options{Poll: 5 * time.Millisecond, OfflineAfter: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(ctx, api.NodeInfo{ID: self}); err != nil {
		t.Fatal(err)
	}
	return m
}

// A job folder CreateJob has just made, with nothing in it yet, is not
// an orphan: a prune at that moment removed it, and Submit failed with
// "chmod jobs/<id>: no such file or directory" (or lost a job it had just
// written).
func TestPruneKeepsJobBeingCreated(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	dir := filepath.Join(share, "jobs", "000000000000000000000009")
	os.MkdirAll(dir, 0o777)
	a.PruneJobs(ctx, 0)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("an empty job folder made a moment ago was pruned")
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(dir, old, old)
	a.PruneJobs(ctx, 0)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("an old empty job folder was never pruned")
	}
}

func TestProtocolMismatch(t *testing.T) {
	share := t.TempDir()
	open(t, share, "a")
	if err := os.WriteFile(filepath.Join(share, "protocol-version"), []byte("99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(share, "b", Options{}); err == nil {
		t.Fatal("expected protocol mismatch error")
	}
}

func TestNodesLiveness(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	if _, err := b.Heartbeat(ctx, "w1 1 b"); err != nil {
		t.Fatal(err)
	}
	ns, err := a.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]api.Node{}
	for _, n := range ns {
		byID[n.ID] = n
	}
	if !byID["b"].Online || byID["b"].Self {
		t.Errorf("b should be online and not self: %+v", byID["b"])
	}
	if byID["a"].Online || byID["a"].LastSeen >= 0 || !byID["a"].Self {
		t.Errorf("a never heartbeat, should be offline self: %+v", byID["a"])
	}

	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(filepath.Join(share, "nodes", "b", "heartbeat"), old, old); err != nil {
		t.Fatal(err)
	}
	ns, _ = a.Nodes(ctx)
	for _, n := range ns {
		if n.ID == "b" && (n.Online || n.LastSeen < time.Minute) {
			t.Errorf("stale b should be offline: %+v", n)
		}
	}
}

func TestRequestReplyRoundTrip(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	msg := &transport.Message{Protocol: 1, ID: "0001", Type: "list", From: "a", To: "b"}
	if err := a.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, err := b.Fetch(ctx)
	if err != nil || len(got) != 1 || got[0].ID != "0001" {
		t.Fatalf("Fetch = %v, %v", got, err)
	}
	// Reply out of order; the reader must still deliver in Seq order.
	for _, seq := range []int{1, 0, 2} {
		if err := b.Reply(ctx, got[0], &transport.Reply{Seq: seq, Final: seq == 2}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(ctx, got[0], false); err != nil {
		t.Fatal(err)
	}
	var seqs []int
	if err := a.AwaitReplies(ctx, "0001", func(r *transport.Reply) error {
		seqs = append(seqs, r.Seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 3 || seqs[0] != 0 || seqs[2] != 2 {
		t.Fatalf("seqs = %v", seqs)
	}
	for _, st := range []string{"cur", "done", "failed"} {
		if _, err := os.Stat(filepath.Join(share, "inbox", "b", st, "0001.json")); !os.IsNotExist(err) {
			t.Errorf("processed request left in %s/: %v", st, err)
		}
	}
	// Reply directory is cleaned up, so late replies are refused.
	if err := b.Reply(ctx, got[0], &transport.Reply{Seq: 3}); !errors.Is(err, ErrRequesterGone) {
		t.Errorf("late reply: want ErrRequesterGone, got %v", err)
	}
}

func TestErrorReplyAndTimeout(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	msg := &transport.Message{Protocol: 1, ID: "0002", From: "a", To: "b"}
	if err := a.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Fetch(ctx)
	b.Reply(ctx, got[0], &transport.Reply{Final: true, Error: "unknown root: x", Code: api.ErrorCode(api.ErrUnknownRoot)})
	err := a.AwaitReplies(ctx, "0002", func(*transport.Reply) error { return nil })
	if !errors.Is(err, api.ErrUnknownRoot) || err.Error() != "b: unknown root: x" {
		t.Fatalf("want coded unknown-root error from b, got %v", err)
	}

	msg.ID = "0003"
	if err := a.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := a.AwaitReplies(tctx, "0003", func(*transport.Reply) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
}

func TestSendUnknownNode(t *testing.T) {
	a := open(t, t.TempDir(), "a")
	err := a.Send(ctx, &transport.Message{ID: "1", From: "a", To: "ghost"})
	if !errors.Is(err, api.ErrUnknownNode) {
		t.Fatalf("want ErrUnknownNode, got %v", err)
	}
}

func TestPruneStaleTmp(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	old, fresh := filepath.Join(share, "tmp", ".crashed.tmp"), filepath.Join(share, "tmp", ".writing.tmp")
	os.WriteFile(old, []byte("x"), 0o644)
	os.WriteFile(fresh, []byte("x"), 0o644)
	past := time.Now().Add(-2 * StaleTmp)
	os.Chtimes(old, past, past)
	if err := a.Prune(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale temp file not pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("in-progress temp file pruned")
	}
}

func TestFetchSkipsGarbage(t *testing.T) {
	share := t.TempDir()
	open(t, share, "a")
	b := open(t, share, "b")
	os.WriteFile(filepath.Join(share, "inbox", "b", "new", "0001.json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(share, "inbox", "b", "new", "0002.json"), []byte(`{"id":"other"}`), 0o644)
	got, err := b.Fetch(ctx)
	if err != nil || len(got) != 0 {
		t.Fatalf("Fetch = %v, %v", got, err)
	}
	for _, n := range []string{"0001.json", "0002.json"} {
		if _, err := os.Stat(filepath.Join(share, "inbox", "b", "failed", n)); err != nil {
			t.Errorf("%s not parked in failed/: %v", n, err)
		}
	}
}

func TestFetchRecoversClaimed(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	a.Send(ctx, &transport.Message{Protocol: 1, ID: "0004", From: "a", To: "b"})
	if got, _ := b.Fetch(ctx); len(got) != 1 {
		t.Fatal("expected one message")
	}
	// Simulate a crash: a new worker instance must see the claimed request.
	b2, err := Open(share, "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b2.Fetch(ctx)
	if err != nil || len(got) != 1 || got[0].ID != "0004" {
		t.Fatalf("recovery Fetch = %v, %v", got, err)
	}
	if got, _ := b2.Fetch(ctx); len(got) != 0 {
		t.Fatalf("second Fetch should be empty, got %d", len(got))
	}
}

func TestPruneLegacyAndUnstartedJobs(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	legacy := filepath.Join(share, "inbox", "a", "done")
	os.MkdirAll(legacy, 0o770)
	os.WriteFile(filepath.Join(legacy, "x.json"), nil, 0o644)

	old := &api.Job{ID: "000000000000000000000001", Request: api.TransferRequest{SrcNode: "a"}}
	young := &api.Job{ID: "000000000000000000000002", Request: api.TransferRequest{SrcNode: "a"}}
	for j, age := range map[*api.Job]time.Duration{old: 25 * time.Hour, young: 2 * time.Hour} {
		if err := a.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-age)
		os.Chtimes(filepath.Join(share, "jobs", j.ID, "job.json"), past, past)
	}
	if err := a.Prune(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneJobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy done/ directory not removed")
	}
	if _, err := a.GetJob(ctx, old.ID); err == nil {
		t.Error("job never started for over a day not pruned")
	}
	if _, err := a.GetJob(ctx, young.ID); err != nil {
		t.Errorf("unstarted job pruned too early: %v", err)
	}
}

// A request that cannot be read must not hold up or drop the others, and
// is parked in failed/ after maxReadTries.
func TestFetchKeepsGoodRequestsPastABadOne(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	// A directory named like a request: the rename works, reading fails.
	if err := os.Mkdir(filepath.Join(share, "inbox", "b", "new", "0001.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	a.Send(ctx, &transport.Message{Protocol: 1, ID: "0005", From: "a", To: "b", Notify: true})
	got, err := b.Fetch(ctx)
	if err == nil || len(got) != 1 || got[0].ID != "0005" {
		t.Fatalf("Fetch = %v, %v; want 0005 and an error", got, err)
	}
	a.Send(ctx, &transport.Message{Protocol: 1, ID: "0006", From: "a", To: "b", Notify: true})
	got, _ = b.Fetch(ctx)
	if len(got) != 1 || got[0].ID != "0006" {
		t.Fatalf("second Fetch = %v", got)
	}
	for i := 0; i < maxReadTries; i++ {
		b.Fetch(ctx)
	}
	if _, err := os.Stat(filepath.Join(share, "inbox", "b", "failed", "0001.json")); err != nil {
		t.Fatalf("bad request not parked: %v", err)
	}
	if _, err := b.Fetch(ctx); err != nil {
		t.Fatalf("Fetch after parking: %v", err)
	}
}

func TestPruneOrphanData(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	live := &api.Job{ID: "000000000000000000000003"}
	if err := a.CreateJob(ctx, live); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	for _, id := range []string{live.ID, "000000000000000000000004"} {
		d := filepath.Join(share, "data", "a", id)
		os.MkdirAll(d, 0o777)
		os.Chtimes(d, past, past)
	}
	if err := a.Prune(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "data", "a", live.ID)); err != nil {
		t.Errorf("data of a live job pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share, "data", "a", "000000000000000000000004")); !os.IsNotExist(err) {
		t.Errorf("orphan data not pruned: %v", err)
	}
}

// A target that was detached mid-transfer and never came back leaves its
// last status ("running") behind; the finished job must still be pruned.
func TestPruneJobWithVanishedDetachedTarget(t *testing.T) {
	share := t.TempDir()
	a, b, c := open(t, share, "a"), open(t, share, "b"), open(t, share, "c")
	j := &api.Job{ID: "000000000000000000000005", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"b", "c"}}}
	if err := a.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobDone})
	b.PutStatus(ctx, j.ID, &api.JobStatus{Node: "b", Role: api.RoleTarget, State: api.JobRunning})
	c.PutStatus(ctx, j.ID, &api.JobStatus{Node: "c", Role: api.RoleTarget, State: api.JobDone})
	if err := a.Detach(ctx, j.ID, api.Detached{Node: "b", Reason: "offline"}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	backdateJob(share, j.ID, past)
	if err := a.PruneJobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob(ctx, j.ID); err == nil {
		t.Fatal("finished job kept because a detached target's stale status says running")
	}
}

// A source that died mid-job and was forgotten leaves "running" behind too.
func TestPruneJobWithForgottenSource(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	j := &api.Job{ID: "000000000000000000000006", Request: api.TransferRequest{SrcNode: "b", DstNodes: []string{"a"}}}
	if err := a.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	b.PutStatus(ctx, j.ID, &api.JobStatus{Node: "b", Role: api.RoleSource, State: api.JobRunning})
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleTarget, State: api.JobFailed})
	past := time.Now().Add(-2 * time.Hour)
	backdateJob(share, j.ID, past)
	if err := a.PruneJobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob(ctx, j.ID); err != nil {
		t.Fatalf("job pruned while its source is still registered: %v", err)
	}
	if err := a.Unregister(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if err := a.PruneJobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob(ctx, j.ID); err == nil {
		t.Fatal("job of a forgotten source never pruned")
	}
}

// One damaged registry entry must not break listing the others.
func TestNodesSkipsDamagedEntries(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	for dir, content := range map[string]string{"broken": "{not json", "wrongid": `{"id":"other"}`} {
		os.MkdirAll(filepath.Join(share, "nodes", dir), 0o777)
		os.WriteFile(filepath.Join(share, "nodes", dir, "info.json"), []byte(content), 0o644)
	}
	ns, err := a.Nodes(ctx)
	if err != nil || len(ns) != 1 || ns[0].ID != "a" {
		t.Fatalf("Nodes = %+v, %v", ns, err)
	}
}

// Share directories removed under a running node come back on their own.
func TestLayoutHeals(t *testing.T) {
	share := t.TempDir()
	a, b := open(t, share, "a"), open(t, share, "b")
	os.RemoveAll(filepath.Join(share, "tmp"))
	if _, err := a.Heartbeat(ctx, "w1 1 a"); err != nil {
		t.Fatalf("heartbeat without tmp/: %v", err)
	}
	if _, err := a.Nodes(ctx); err != nil {
		t.Fatalf("probe without tmp/: %v", err)
	}
	os.RemoveAll(filepath.Join(share, "inbox", "b"))
	b.Fetch(ctx) // notices and repairs
	if err := a.Send(ctx, &transport.Message{Protocol: 1, ID: "0007", From: "a", To: "b", Notify: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Fetch(ctx); err != nil || len(got) != 1 {
		t.Fatalf("Fetch after the inbox was removed: %v %v", got, err)
	}
	os.RemoveAll(filepath.Join(share, "nodes", "a"))
	if _, err := a.Heartbeat(ctx, "w1 1 a"); !errors.Is(err, transport.ErrNotRegistered) {
		t.Fatalf("heartbeat with the entry removed: %v", err)
	}
}

// backdateJob makes a job's record and statuses look written at t.
func backdateJob(share, id string, t time.Time) {
	dir := filepath.Join(share, "jobs", id)
	os.Chtimes(filepath.Join(dir, "job.json"), t, t)
	des, _ := os.ReadDir(filepath.Join(dir, "status"))
	for _, de := range des {
		os.Chtimes(filepath.Join(dir, "status", de.Name()), t, t)
	}
	os.Chtimes(filepath.Join(dir, "status"), t, t)
}

// A job that just ended stays a little even with -keep 0, so the CLI that
// follows it can read how it ended; one without its record goes later.
func TestPruneFloors(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	j := &api.Job{ID: "000000000000000000000007", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}
	a.CreateJob(ctx, j)
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobDone})
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleTarget, State: api.JobDone})
	if err := a.PruneJobs(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob(ctx, j.ID); err != nil {
		t.Fatalf("a job that ended a moment ago was pruned: %v", err)
	}
	backdateJob(share, j.ID, time.Now().Add(-2*time.Minute))
	if err := a.PruneJobs(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetJob(ctx, j.ID); err == nil {
		t.Fatal("a job that ended two minutes ago survived -keep 0")
	}
	// half pruned: a status landed after the record went
	orphan := filepath.Join(share, "jobs", "000000000000000000000008")
	os.MkdirAll(filepath.Join(orphan, "status"), 0o777)
	a.PruneJobs(ctx, 0)
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("a fresh folder without its record was pruned at once")
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(orphan, "status"), old, old)
	os.Chtimes(orphan, old, old)
	a.PruneJobs(ctx, 0)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("an old job folder without its record was never pruned")
	}
}

// A sync's path lists stay while a job that reads them runs, however old.
func TestPruneKeepsPlansInUse(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	for _, id := range []string{"plan1", "plan2"} {
		w, _ := a.CreateBlob(ctx, "plans/"+id+"/copy-1")
		w.Write([]byte("\"x\"\n"))
		w.Commit(nil)
		old := time.Now().Add(-3 * time.Hour)
		os.Chtimes(filepath.Join(share, "blobs", "plans", id, "copy-1"), old, old)
		os.Chtimes(filepath.Join(share, "blobs", "plans", id), old, old)
	}
	j := &api.Job{ID: "000000000000000000000009", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}, SyncPlan: "plan1"}}
	a.CreateJob(ctx, j)
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobRunning})
	if err := a.PruneBlobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "blobs", "plans", "plan1")); err != nil {
		t.Errorf("the plan of a running job was pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share, "blobs", "plans", "plan2")); !os.IsNotExist(err) {
		t.Errorf("an old unused plan survived: %v", err)
	}
}

// A plan's scans are kept as long as the plan: applying it a while after
// the dry run refreshes the plan, and its deletes still need the scans.
func TestPruneKeepsScansWithTheirPlan(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	blob := func(name string, age time.Duration) {
		w, _ := a.CreateBlob(ctx, name)
		w.Write([]byte("x"))
		w.Commit(nil)
		old := time.Now().Add(-age)
		p := filepath.Join(share, "blobs", name)
		os.Chtimes(p, old, old)
		os.Chtimes(filepath.Dir(p), old, old)
	}
	blob("scans/plan1/a.snap", 3*time.Hour) // planned long ago
	blob("plans/plan1/plan.json", 0)        // applied just now
	blob("scans/plan2/a.snap", 3*time.Hour) // planned long ago, never applied
	blob("plans/plan2/plan.json", 3*time.Hour)
	if err := a.PruneBlobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "blobs", "scans", "plan1")); err != nil {
		t.Errorf("the scans of a fresh plan were pruned: %v", err)
	}
	for _, d := range []string{"scans/plan2", "plans/plan2"} {
		if _, err := os.Stat(filepath.Join(share, "blobs", d)); !os.IsNotExist(err) {
			t.Errorf("%s: old and unused, but kept: %v", d, err)
		}
	}
}

// A source killed while writing a chunk leaves its temp file in tmp/; the
// restarted source removes it when it writes that chunk again or when the
// job ends, not an hour later (cloud run 2026-10-24: 312 MiB left).
func TestKilledSourceTempChunksRemoved(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	const id, other = "0123456789abcdef01234567", "0123456789abcdef01234568"
	left := func(seq int, job string) string {
		p := filepath.Join(share, "tmp", "."+job+"-"+chunkName(seq, ".chunk")+".1234")
		if err := os.WriteFile(p, []byte("half"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	same, later, theirs := left(6, id), left(7, id), left(6, other)
	w, err := a.CreateChunk(ctx, id, 6)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Commit([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(same); err == nil {
		t.Error("the chunk written again left its old temp file")
	}
	if _, err := os.Stat(later); err != nil {
		t.Error("a temp file of another chunk was removed while the job runs")
	}
	if err := a.RemoveJobData(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(later); err == nil {
		t.Error("the job ended and left a temp chunk")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Error("another job's temp chunk was removed")
	}
}

// StagedBytes counts this node's chunks as it writes and deletes them,
// and lists them only every stagedRescan: before, every chunk listed and
// stat'ed all staged chunks (review 2026-10-25). What changes behind its
// back shows at the next scan.
func TestStagedBytesCounted(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	const id, other = "0123456789abcdef01234567", "0123456789abcdef01234568"
	put := func(job string, seq, n int) {
		t.Helper()
		w, err := a.CreateChunk(ctx, job, seq)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(make([]byte, n)); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit([]byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	want := func(what string, job, total int64) {
		t.Helper()
		j, tot, err := a.StagedBytes(ctx, id)
		if err != nil || j != job || tot != total {
			t.Errorf("%s: %d of %d staged (%v), want %d of %d", what, j, tot, err, job, total)
		}
	}
	put(id, 0, 100) // before the first scan
	want("first scan", 100, 100)
	put(id, 1, 10)
	put(other, 0, 1000)
	want("written", 110, 1110)
	put(id, 1, 20) // written again (a resumed source)
	want("rewritten", 120, 1120)
	if err := a.DeleteChunk(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	want("deleted", 20, 1020)
	if err := a.RemoveJobData(ctx, other); err != nil {
		t.Fatal(err)
	}
	want("job removed", 20, 20)

	// Behind its back: trusted until the next scan.
	os.Remove(filepath.Join(share, "data", "a", id, chunkName(1, ".chunk")))
	want("removed by hand, no scan yet", 20, 20)
	a.staged.at = time.Now().Add(-stagedRescan)
	want("after a scan", 0, 0)
}
