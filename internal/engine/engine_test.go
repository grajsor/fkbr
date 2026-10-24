package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
	"fkbr/internal/sshlink"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
	"fkbr/internal/transport"
	"fkbr/internal/transport/nfsmailbox"
)

// nodeCfg makes a config with its own local root and state dir on a shared
// "NFS" temp dir.
func nodeCfg(t *testing.T, share, id string) *config.Config {
	t.Helper()
	base := t.TempDir()
	rootDir := filepath.Join(base, "data")
	if err := os.Mkdir(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		NodeID:            id,
		NFSRoot:           share,
		StateDir:          filepath.Join(base, "state"),
		Roots:             []config.Root{{ID: "data", Path: rootDir, AllowWrite: true}},
		Poll:              config.Poll{Active: 5 * time.Millisecond, Idle: 5 * time.Millisecond, IdleAfter: time.Second},
		HeartbeatInterval: 20 * time.Millisecond,
		OfflineAfter:      time.Second,
		RequestTimeout:    2 * time.Second,
		KeepDone:          time.Hour,
		ForgetAfter:       -1,
		Transfer: config.Transfer{
			ChunkSize:    64 << 10,
			StagingQuota: 1 << 20,
			JobQuota:     256 << 10,
			MaxJobs:      2,
			DetachAfter:  time.Minute,
		},
		Sync: config.Sync{MaxDeletes: 1000},
	}
}

// build creates an engine for cfg; several engines may share one cfg to
// simulate restarts.
func build(t *testing.T, cfg *config.Config) *Engine {
	t.Helper()
	return buildWrapped(t, cfg, nil)
}

// buildWrapped is build with the node's store wrapped (nil: as it is).
func buildWrapped(t *testing.T, cfg *config.Config, wrap func(transport.Store) transport.Store) *Engine {
	t.Helper()
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	if wrap == nil {
		wrap = func(st transport.Store) transport.Store { return st }
	}
	// Refresh on: exercises the NFS-compensation paths everywhere (on a
	// local filesystem they change nothing observable).
	opts := nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter, Refresh: true}
	if linkTest {
		tr, st := linked(t, cfg, opts)
		e := New(cfg, fsys, tr, wrap(st), Options{Version: "test"})
		t.Cleanup(func() { e.Close() })
		return e
	}
	mb, err := nfsmailbox.Open(cfg.NFSRoot, cfg.NodeID, opts)
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, fsys, mb, wrap(mb), Options{Version: "test"})
	t.Cleanup(func() { e.Close() })
	return e
}

// With FKBR_TEST_TRANSPORT=ssh, the first node built on a share is an ssh
// swarm's home node, with the share as its private directory, and every
// other node is linked to it over an in-process pipe (docs/ssh.md).
var linkTest = os.Getenv("FKBR_TEST_TRANSPORT") == "ssh"

type testSwarm struct {
	mu    sync.Mutex
	home  *sshlink.Home
	id    string
	links map[string]*sshlink.Remote
}

var swarms sync.Map // share -> *testSwarm

func linked(t *testing.T, cfg *config.Config, nfs nfsmailbox.Options) (transport.Transport, transport.Store) {
	t.Helper()
	v, loaded := swarms.LoadOrStore(cfg.NFSRoot, &testSwarm{id: cfg.NodeID, links: map[string]*sshlink.Remote{}})
	sw := v.(*testSwarm)
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if !loaded {
		sw.home = sshlink.NewHome(cfg.NFSRoot, cfg.NodeID, store.Options{Poll: nfs.Poll, OfflineAfter: nfs.OfflineAfter}, nil)
	}
	if cfg.NodeID == sw.id {
		n, err := sw.home.Open()
		if err != nil {
			t.Fatal(err)
		}
		return n, n
	}
	if old := sw.links[cfg.NodeID]; old != nil { // a restart: the old process is gone
		old.Close()
		waitUntil(t, func() bool { return !linkedNode(sw.home, cfg.NodeID) }, "old link gone")
	}
	a, b := net.Pipe()
	// Pings often, but a link is given up only after seconds: a -race run
	// beside other load can stall the whole process for half a second, and
	// a pipe link here never comes back (cloud run 2026-10-24).
	fo := frame.Options{Ping: 100 * time.Millisecond, Dead: 5 * time.Second}
	served := make(chan struct{})
	go func() {
		sw.home.ServeConn(context.Background(), a, cfg.NodeID+"-host", fo)
		close(served)
	}()
	r, err := sshlink.Dial(context.Background(), b, cfg.NodeID, "test", filepath.Join(cfg.StateDir, "chunks"), fo, nil)
	if err != nil {
		t.Fatal(err)
	}
	sw.links[cfg.NodeID] = r
	t.Cleanup(func() {
		r.Close()
		// The home node's calls for this node end before the share is
		// removed: one still writing left it "not empty".
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Errorf("home node still serving %s", cfg.NodeID)
		}
	})
	return r, r
}

// nfsOnly skips a test that builds a node on the share itself, to inject
// faults into the share's files: in an ssh swarm no node does.
func nfsOnly(t *testing.T) {
	t.Helper()
	if linkTest {
		t.Skip("injects faults into the share's files; NFS only")
	}
}

func linkedNode(h *sshlink.Home, node string) bool {
	for _, l := range h.Links() {
		if l.Node == node && l.State == "connected" {
			return true
		}
	}
	return false
}

func node(t *testing.T, share, id string) (*Engine, string) {
	t.Helper()
	cfg := nodeCfg(t, share, id)
	return build(t, cfg), cfg.Roots[0].Path
}

// waitPruned waits until each engine's worker has done its first prune.
func waitPruned(t *testing.T, es ...*Engine) {
	t.Helper()
	for _, e := range es {
		select {
		case <-e.pruned:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no prune at start", e.Self())
		}
	}
}

func runWorker(t *testing.T, e *Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("worker %s: %v", e.Self(), err)
			}
		})
	}
	t.Cleanup(stop)
	// Wait until the node is registered and heartbeating.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ns, _ := e.Nodes(context.Background())
		for _, n := range ns {
			if n.ID == e.Self() && n.Online {
				return stop
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("worker %s never came online", e.Self())
	return nil
}

func TestRemoteListAcrossNodes(t *testing.T) {
	share := t.TempDir()
	master, _ := node(t, share, "master")
	slave, slaveRoot := node(t, share, "slave1")
	const n = 4500 // spans several reply parts
	for i := range n {
		if err := os.WriteFile(filepath.Join(slaveRoot, fmt.Sprintf("f%05d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runWorker(t, slave)

	ctx := context.Background()
	res, err := master.List(ctx, api.ListRequest{Node: "slave1", Root: "data", Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != n || len(res.Entries) != n || res.Node != "slave1" {
		t.Fatalf("total=%d entries=%d node=%s", res.Total, len(res.Entries), res.Node)
	}
	if res.Entries[0].Name != "f00000" || res.Entries[n-1].Name != fmt.Sprintf("f%05d", n-1) {
		t.Fatalf("order: first=%s last=%s", res.Entries[0].Name, res.Entries[n-1].Name)
	}

	page, err := master.List(ctx, api.ListRequest{Node: "slave1", Root: "data", Offset: 4490, Limit: 100})
	if err != nil || len(page.Entries) != 10 || page.Offset != 4490 {
		t.Fatalf("paging: %+v, %v", page, err)
	}

	// Errors on the executing node come back to the requester.
	if _, err := master.List(ctx, api.ListRequest{Node: "slave1", Root: "nope"}); err == nil {
		t.Fatal("expected error for unknown remote root")
	}
	if _, err := master.List(ctx, api.ListRequest{Node: "slave1", Root: "data", Path: "../x"}); err == nil {
		t.Fatal("expected error for escaping path")
	}
}

func TestListOfflineAndUnknown(t *testing.T) {
	share := t.TempDir()
	master, _ := node(t, share, "master")
	slave, _ := node(t, share, "slave1")
	runWorker(t, slave)
	ctx := context.Background()
	if _, err := master.List(ctx, api.ListRequest{Node: "ghost", Root: "data"}); !errors.Is(err, api.ErrUnknownNode) {
		t.Fatalf("want ErrUnknownNode, got %v", err)
	}
	old := time.Now().Add(-time.Hour)
	// Registered but stale heartbeat: fail fast instead of waiting for timeout.
	node(t, share, "slave2")
	os.MkdirAll(filepath.Join(share, "nodes", "slave2"), 0o755)
	os.WriteFile(filepath.Join(share, "nodes", "slave2", "info.json"), []byte(`{"id":"slave2"}`), 0o644)
	os.WriteFile(filepath.Join(share, "nodes", "slave2", "heartbeat"), nil, 0o644)
	os.Chtimes(filepath.Join(share, "nodes", "slave2", "heartbeat"), old, old)
	if _, err := master.List(ctx, api.ListRequest{Node: "slave2", Root: "data"}); !errors.Is(err, api.ErrNodeOffline) {
		t.Fatalf("want ErrNodeOffline, got %v", err)
	}
}

func TestSingleWorkerPerNode(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	if err := a.Run(context.Background()); err == nil {
		t.Fatal("second worker for the same node must be refused")
	}
}
