package nfsmailbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

var ctx = context.Background()

func open(t *testing.T, share, self string) *Mailbox {
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

func TestRefreshLeavesNoTrace(t *testing.T) {
	share := t.TempDir()
	m, err := Open(share, "a", Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	m.Register(ctx, api.NodeInfo{ID: "a"})
	m.Heartbeat(ctx, "w1 1 a")
	for i := 0; i < 5; i++ {
		if _, err := m.Nodes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Throttled: a burst refreshes each directory once.
	if n := m.refreshSeq.Load(); n < 2 || n > 3 {
		t.Errorf("refreshes in a burst of 5 Nodes calls: %d", n)
	}
	filepath.Walk(share, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(fi.Name(), ".r-") {
			t.Errorf("refresh file left behind: %s", p)
		}
		return nil
	})
}

// A directory another node has not created yet may be cached as missing in
// its parent. Refreshing it must then refresh the parent, or the client
// keeps "does not exist" for as long as the mount caches attributes.
func TestRefreshMissingDirRefreshesParent(t *testing.T) {
	share := t.TempDir()
	m, err := Open(share, "a", Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	parent := m.path("data", "src")
	if err := os.MkdirAll(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(parent, old, old); err != nil {
		t.Fatal(err)
	}
	m.fresh(filepath.Join(parent, "job", "acks")) // neither job nor acks exists
	fi, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().After(old.Add(time.Minute)) {
		t.Errorf("parent of a missing directory not refreshed (mtime %v)", fi.ModTime())
	}
	if _, err := os.Stat(filepath.Join(parent, "job")); !os.IsNotExist(err) {
		t.Errorf("refresh created the missing directory: %v", err)
	}
}

func TestPruneLegacyAndUnstartedJobs(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	legacy := filepath.Join(share, "inbox", "a", "done")
	os.MkdirAll(legacy, 0o770)
	os.WriteFile(filepath.Join(legacy, "x.json"), nil, 0o644)

	old := &api.Job{ID: "000000000000000000000001"}
	young := &api.Job{ID: "000000000000000000000002"}
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
	os.Chtimes(filepath.Join(share, "jobs", j.ID, "job.json"), past, past)
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
	os.Chtimes(filepath.Join(share, "jobs", j.ID, "job.json"), past, past)
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
