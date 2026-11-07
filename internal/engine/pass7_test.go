package engine

import (
	"context"
	"testing"
	"time"

	"fkbr/internal/api"
)

// A node that leaves the registry (forgotten, pruned) is announced, so a
// frontend that keeps its own list does not show it for good.
func TestForgottenNodeBroadcastsRemoval(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := build(t, nodeCfg(t, share, "b"))
	if err := b.tr.Register(ctx, api.NodeInfo{ID: "b"}); err != nil {
		t.Fatal(err)
	}
	ch, err := a.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// the snapshot lists b; wait for it, then for the poller to go quiet
	sawB := false
drain:
	for deadline := time.After(3 * time.Second); ; {
		select {
		case ev := <-ch:
			if ev.Type == api.EventNode && ev.Node.ID == "b" {
				sawB = true
			}
		case <-time.After(300 * time.Millisecond):
			if sawB {
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !sawB {
		t.Fatal("node b never listed")
	}
	if err := a.tr.Unregister(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * eventInterval)
	for {
		select {
		case ev := <-ch:
			if ev.Type == api.EventNodeGone && ev.NodeID == "b" {
				return
			}
		case <-timeout:
			t.Fatal("b left the registry and no node_gone event followed")
		}
	}
}
