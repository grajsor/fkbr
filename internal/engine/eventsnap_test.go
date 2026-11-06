package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"fkbr/internal/api"
)

// A subscriber gets the whole snapshot first, however many nodes and jobs
// it holds: events past the channel's size used to be dropped and, being
// counted as seen, never sent (review 2026-11-05: 300 jobs gave 256).
func TestSubscribeSnapshotIsComplete(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const jobs = 300
	for i := range jobs {
		j := &api.Job{ID: fmt.Sprintf("%024d", i+1), Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}
		if err := a.st.CreateJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := a.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < jobs {
		select {
		case ev := <-ch:
			if ev.Type == api.EventJob {
				seen[ev.JobID] = true
			}
		case <-deadline:
			t.Fatalf("%d of %d jobs in the snapshot", len(seen), jobs)
		}
	}
}

// The last subscriber leaving while a new one reads its snapshot stopped
// the poller, and the newcomer, who had seen a subscriber on arrival, did
// not start it again: no event ever reached it (review 2026-11-06).
func TestSubscribeAfterLastLeavesDuringSnapshot(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctxA, cancelA := context.WithCancel(ctx)
	if _, err := a.Subscribe(ctxA); err != nil {
		t.Fatal(err)
	}
	afterSnapshot = func() {
		cancelA()
		waitUntil(t, func() bool {
			a.hub.mu.Lock()
			defer a.hub.mu.Unlock()
			return len(a.hub.subs) == 0
		}, "first subscriber gone")
	}
	defer func() { afterSnapshot = func() {} }()
	ch, err := a.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterSnapshot = func() {}
	for len(ch) > 0 { // the snapshot
		<-ch
	}
	j := &api.Job{ID: fmt.Sprintf("%024d", 1), Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}
	if err := a.st.CreateJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == api.EventJob && ev.JobID == j.ID {
				return
			}
		case <-deadline:
			t.Fatal("no event after the first subscriber left during the snapshot")
		}
	}
}
