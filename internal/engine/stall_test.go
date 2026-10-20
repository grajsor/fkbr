package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/transport"
	"fkbr/internal/transport/nfsmailbox"
)

// lossyInbox drops the first n receive notifies, as if they were lost.
type lossyInbox struct {
	*nfsmailbox.Mailbox
	mu   sync.Mutex
	drop int
}

func (l *lossyInbox) Fetch(ctx context.Context) ([]*transport.Message, error) {
	msgs, err := l.Mailbox.Fetch(ctx)
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*transport.Message
	for _, m := range msgs {
		if m.Type == transport.TypeReceive && l.drop != 0 {
			l.drop--
			l.Mailbox.Finish(ctx, m, false)
			continue
		}
		out = append(out, m)
	}
	return out, err
}

func buildLossy(t *testing.T, share, id string, drop int) (*Engine, string) {
	t.Helper()
	cfg := nodeCfg(t, share, id)
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(share, id, nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, fsys, &lossyInbox{Mailbox: mb, drop: drop}, mb, Options{Version: "test"})
	t.Cleanup(func() { e.Close() })
	return e, cfg.Roots[0].Path
}

func shortRenotify(t *testing.T) {
	t.Helper()
	old := renotifyEvery
	renotifyEvery = 100 * time.Millisecond
	t.Cleanup(func() { renotifyEvery = old })
}

// A lost receive notify is sent again, so the job still finishes.
func TestLostNotifyResent(t *testing.T) {
	shortRenotify(t)
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := buildLossy(t, share, "b", 2)
	makeTree(t, aroot, 3, 100<<10)
	runWorker(t, a)
	runWorker(t, b)
	id, err := a.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, aroot+"/tree", broot+"/in/tree")
}

// A single target that was forgotten fails the job instead of leaving the
// source waiting forever.
func TestForgottenTargetFailsJob(t *testing.T) {
	shortRenotify(t)
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, _ := buildLossy(t, share, "b", -1) // never starts
	makeTree(t, aroot, 5, 3<<20)          // beyond the job quota: the source blocks
	runWorker(t, a)
	stopB := runWorker(t, b)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	stopB()
	waitUntil(t, func() bool { return a.Forget(ctx, "b") == nil }, "forget b")
	j := waitJob(t, a, id, 20*time.Second)
	if j.State != api.JobFailed || !strings.Contains(roleError(j, api.RoleSource), "no longer registered") {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
}

// A target whose source was forgotten mid-transfer fails instead of
// waiting forever for the next chunk.
func TestForgottenSourceFailsTarget(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	a := build(t, acfg)
	b, _ := node(t, share, "b")
	makeTree(t, acfg.Roots[0].Path, 5, 3<<20)
	stopA := runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool { n, _ := a.st.Acked(ctx, "a", id, "b"); return n >= 1 }, "b acks a chunk")
	stopA()
	waitUntil(t, func() bool { return b.Forget(ctx, "a") == nil }, "forget a")
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := b.st.GetJob(ctx, id)
		if err == nil && roleState(j, api.RoleTarget, "b") == api.JobFailed {
			if msg := roleError(j, api.RoleTarget); !strings.Contains(msg, "no longer registered") {
				t.Fatalf("target error: %s", msg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("target still waiting: %+v", j)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func roleError(j *api.Job, role api.Role) string {
	for _, p := range j.Participants {
		if p.Role == role {
			return p.Error
		}
	}
	return ""
}

// A target interrupted after its last ack, but before installing, finds
// the source done and its data (acks included) removed. It must install
// what it staged instead of waiting for chunk 0 forever.
func TestTargetResumesAfterSourceFinished(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	bcfg := nodeCfg(t, share, "b")
	makeTree(t, aroot, 3, 200<<10)
	runWorker(t, a)
	ctx := context.Background()
	// Stop b's worker just after it acks the last chunk, before Commit.
	b := build(t, bcfg)
	stopB := runWorker(t, b)
	id, err := a.Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s", j.State)
	}
	stopB()
	// Rewind b's status to "received everything, not installed" and run
	// the receive again, as after a restart at that moment.
	j, _ := a.st.GetJob(ctx, id)
	st := *ownStatus(j, "b", api.RoleTarget)
	st.State, st.Phase = api.JobRunning, "receiving"
	b2 := build(t, bcfg)
	if err := b2.st.PutStatus(ctx, id, &st); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- b2.runReceive(ctx, id) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("target waits for chunks the finished source already deleted")
	}
	j, _ = a.st.GetJob(ctx, id)
	if s := roleState(j, api.RoleTarget, "b"); s != api.JobDone {
		t.Fatalf("target state %s", s)
	}
}

// A target whose job slots are all busy publishes "queued", and if it is
// dropped from a fan-out for that, the reason says so.
func TestQueuedTargetVisible(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	acfg.Transfer.DetachAfter = 300 * time.Millisecond
	a := build(t, acfg)
	bcfg := nodeCfg(t, share, "b")
	bcfg.Transfer.MaxJobs = 1
	b := build(t, bcfg)
	c, _ := node(t, share, "c")
	makeTree(t, acfg.Roots[0].Path, 5, 3<<20) // beyond the job quota: the source blocks on b
	runWorker(t, a)
	runWorker(t, b)
	runWorker(t, c)
	b.recvSem <- struct{}{} // b's only slot is taken
	ctx := context.Background()
	id, err := a.Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"b", "c"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		j, err := a.st.GetJob(ctx, id)
		return err == nil && roleState(j, api.RoleTarget, "b") == api.JobQueued
	}, "b reports queued")
	j := waitJob(t, a, id, 20*time.Second)
	<-b.recvSem
	if j.State != api.JobPartial || len(j.Detached) != 1 || !strings.Contains(j.Detached[0].Reason, "queued on the target") {
		t.Fatalf("state %s detached %+v", j.State, j.Detached)
	}
}
