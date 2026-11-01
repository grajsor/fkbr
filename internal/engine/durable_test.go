package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
)

// ackLog records, in order, a target's staging syncs and its acks.
type ackLog struct {
	transport.Store
	mu     sync.Mutex
	events []string
}

func (a *ackLog) add(ev string) {
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
}

func (a *ackLog) SetAcked(ctx context.Context, src, id, node string, n int) error {
	a.add(fmt.Sprintf("ack %d", n))
	return a.Store.SetAcked(ctx, src, id, node, n)
}

// A target's staging is on disk before it acks a chunk: once every target
// acked it the source deletes the chunk, so staging a power cut loses
// could not be had again, and a target resuming after the cut would
// install the job without it (and say done). Commit's own sync, before
// the job is done, is in receive's TestCommitSyncs.
func TestAckAfterStagingSynced(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	acks := &ackLog{}
	s1 := buildWrapped(t, scfg, func(st transport.Store) transport.Store { acks.Store = st; return acks })
	old := syncStaging
	t.Cleanup(func() { syncStaging = old }) // after the workers stop
	syncStaging = func(r *receive.Receiver) error {
		err := old(r)
		acks.add("sync") // done: the ack may go out
		return err
	}
	makeTree(t, mroot, 10, 600<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(scfg.Roots[0].Path, "tree"))
	acks.mu.Lock()
	defer acks.mu.Unlock()
	synced, n := false, 0
	for _, ev := range acks.events {
		switch {
		case ev == "sync":
			synced = true
		case strings.HasPrefix(ev, "ack"):
			if !synced {
				t.Fatalf("%s without a sync of the staging before it: %v", ev, acks.events)
			}
			synced, n = false, n+1
		}
	}
	if n < 2 {
		t.Fatalf("only %d acks: the test needs several chunks: %v", n, acks.events)
	}
}

// A staging sync that fails (EIO: the disk may not hold what was written)
// acks nothing: the chunks since it are applied again, and the job is done
// and exact.
func TestStagingSyncFailsOnce(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	scfg := nodeCfg(t, share, "s1")
	acks := &ackLog{}
	s1 := buildWrapped(t, scfg, func(st transport.Store) transport.Store { acks.Store = st; return acks })
	old := syncStaging
	t.Cleanup(func() { syncStaging = old })
	var mu sync.Mutex
	calls := 0
	syncStaging = func(r *receive.Receiver) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 2 {
			acks.add("failed")
			return &os.PathError{Op: "syncfs", Path: "data", Err: syscall.EIO}
		}
		err := old(r)
		acks.add("sync")
		return err
	}
	makeTree(t, mroot, 10, 600<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, master, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(mroot, "tree"), filepath.Join(scfg.Roots[0].Path, "tree"))
	acks.mu.Lock()
	defer acks.mu.Unlock()
	failed := slices.Index(acks.events, "failed")
	if failed < 0 {
		t.Fatalf("fault never injected: %v", acks.events)
	}
	// The ack after the failure is for no more than what was acked before it
	// plus the chunk applied again.
	last := 0
	for _, ev := range acks.events[:failed] {
		fmt.Sscanf(ev, "ack %d", &last)
	}
	for _, ev := range acks.events[failed:] {
		var n int
		if _, err := fmt.Sscanf(ev, "ack %d", &n); err == nil {
			if n != last+1 {
				t.Fatalf("first ack after the failed sync is %d, want %d: %v", n, last+1, acks.events)
			}
			break
		}
	}
}
