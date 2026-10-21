package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/engine"
	"fkbr/internal/localfs"
	"fkbr/internal/receive"
	"fkbr/internal/transport/nfsmailbox"
)

func newEngine(t *testing.T, share, id string) (*engine.Engine, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "data")
	os.Mkdir(root, 0o755)
	cfg := &config.Config{
		NodeID: id, NFSRoot: share, StateDir: filepath.Join(base, "state"),
		Roots:             []config.Root{{ID: "data", Path: root, AllowWrite: true}},
		Poll:              config.Poll{Active: 5 * time.Millisecond, Idle: 5 * time.Millisecond, IdleAfter: time.Second},
		HeartbeatInterval: 20 * time.Millisecond, OfflineAfter: time.Second, RequestTimeout: 2 * time.Second, KeepDone: time.Hour,
		Transfer: config.Transfer{ChunkSize: 64 << 10, StagingQuota: 1 << 20, JobQuota: 256 << 10, MaxJobs: 2, DetachAfter: time.Minute},
	}
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(share, id, nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(cfg, fsys, mb, mb, engine.Options{Version: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; e.Close() })
	for i := 0; i < 200; i++ {
		ns, _ := e.Nodes(context.Background())
		for _, n := range ns {
			if n.ID == id && n.Online {
				return e, root
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never came online", id)
	return nil, ""
}

// serve exposes svc on a socket and returns a connected client.
func serve(t *testing.T, svc api.Service) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "fkbr-sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "api.sock")
	l, err := Listen(sock, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Serve(ctx, l, svc, nil); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", fi.Mode(), err)
	}
	if _, err := Listen(sock, 0o600); err == nil {
		t.Fatal("second Listen on a live socket must fail")
	}
	c, err := Dial(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// contract runs the same checks against any api.Client.
func contract(t *testing.T, c api.Client, slaveRoot string) {
	ctx := context.Background()
	if c.Self() != "master" {
		t.Fatalf("Self = %q", c.Self())
	}
	ns, err := c.Nodes(ctx)
	if err != nil || len(ns) != 2 {
		t.Fatalf("Nodes = %d, %v", len(ns), err)
	}
	os.WriteFile(filepath.Join(slaveRoot, "hello"), []byte("hi"), 0o644)
	res, err := c.List(ctx, api.ListRequest{Node: "slave", Root: "data"})
	if err != nil || res.Total < 1 || res.Entries[0].Name != "hello" { // later subtests add entries
		t.Fatalf("List = %+v, %v", res, err)
	}
	// Sentinel errors survive the transport.
	if _, err := c.List(ctx, api.ListRequest{Node: "ghost", Root: "data"}); !errors.Is(err, api.ErrUnknownNode) {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := c.List(ctx, api.ListRequest{Root: "data", Path: "../x"}); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("bad path: %v", err)
	}
	if err := c.Prune(ctx, time.Hour); err != nil {
		t.Errorf("prune: %v", err)
	}
	if err := c.Forget(ctx, "ghost"); !errors.Is(err, api.ErrUnknownNode) {
		t.Errorf("forget unknown: %v", err)
	}
	if err := c.Forget(ctx, "slave"); !errors.Is(err, api.ErrNotAllowed) {
		t.Errorf("forget online node: %v", err)
	}
	if _, err := c.Job(ctx, "nosuchjob"); !errors.Is(err, api.ErrUnknownJob) {
		t.Errorf("unknown job: %v", err)
	}

	// File operations on the remote node.
	dir := "made-by-" + c.Self()
	if err := c.Mkdir(ctx, api.MkdirRequest{Node: "slave", Root: "data", Path: dir + "/sub"}); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(slaveRoot, dir, "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("mkdir result: %v", err)
	}
	rd, err := c.Read(ctx, api.ReadRequest{Node: "slave", Root: "data", Path: "hello", Offset: 1, Limit: 10})
	if err != nil || string(rd.Data) != "i" || rd.Size != 2 || !rd.EOF {
		t.Fatalf("Read = %+v, %v", rd, err)
	}
	if _, err := c.Read(ctx, api.ReadRequest{Node: "slave", Root: "data", Path: dir}); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("read of a dir: %v", err)
	}
	rm, err := c.Remove(ctx, api.RemoveRequest{Node: "slave", Root: "data", Paths: []string{dir, ".", receive.StagingDir}})
	if err != nil || rm.Removed != 1 || len(rm.Errors) != 2 {
		t.Fatalf("Remove = %+v, %v", rm, err)
	}
	if _, err := os.Stat(filepath.Join(slaveRoot, dir)); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}

	// Sync: plan (dry run), read back, apply.
	os.MkdirAll(filepath.Join(slaveRoot, "syncsrc"), 0o755)
	os.WriteFile(filepath.Join(slaveRoot, "syncsrc", "f"), []byte("data"), 0o644)
	plan, err := c.PlanSync(ctx, api.SyncRequest{SrcNode: "slave", SrcRoot: "data", SrcPath: "syncsrc", DstNodes: []string{"master"}, DstRoot: "data", DstPath: "synced-" + c.Self()})
	if err != nil || len(plan.Targets) != 1 || plan.Targets[0].Copy != 2 {
		t.Fatalf("PlanSync = %+v, %v", plan, err)
	}
	if got, err := c.SyncPlan(ctx, plan.ID); err != nil || got.ID != plan.ID {
		t.Fatalf("SyncPlan = %+v, %v", got, err)
	}
	applied, err := c.ApplySync(ctx, plan.ID, false)
	if err != nil || !applied.Applied || len(applied.Jobs) != 1 {
		t.Fatalf("ApplySync = %+v, %v", applied, err)
	}
	if _, err := c.SyncPlan(ctx, "nosuchplan"); !errors.Is(err, api.ErrUnknownJob) {
		t.Errorf("unknown plan: %v", err)
	}

	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := c.Subscribe(sub)
	if err != nil {
		t.Fatal(err)
	}
	// Initial snapshot: both nodes.
	nodesSeen := 0
	for nodesSeen < 2 {
		select {
		case ev := <-events:
			if ev.Type == api.EventNode {
				nodesSeen++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no initial node events")
		}
	}

	id, err := c.Submit(ctx, api.TransferRequest{SrcNode: "slave", SrcRoot: "data", Paths: []string{"hello"}, DstNodes: []string{"master"}, DstRoot: "data", DstDir: "in-" + c.Self()})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event stream closed")
			}
			if ev.Type == api.EventJob && ev.JobID == id && ev.Job.State == api.JobDone {
				done = true
			}
		case <-deadline:
			t.Fatal("no job-done event")
		}
	}
	j, err := c.Job(ctx, id)
	if err != nil || j.State != api.JobDone {
		t.Fatalf("Job = %+v, %v", j, err)
	}
	js, err := c.Jobs(ctx)
	if err != nil || len(js) == 0 {
		t.Fatalf("Jobs = %d, %v", len(js), err)
	}
	if err := c.Cancel(ctx, id); err != nil {
		t.Errorf("cancel finished job: %v", err)
	}
	if _, err := c.CatchUp(ctx, id); err == nil {
		t.Error("catch-up without detached targets must fail")
	}
	cancel()
	for range events { // closes after cancel
	}
}

func TestContract(t *testing.T) {
	share := t.TempDir()
	master, _ := newEngine(t, share, "master")
	_, slaveRoot := newEngine(t, share, "slave")
	t.Run("local", func(t *testing.T) { contract(t, master, slaveRoot) })
	t.Run("remote", func(t *testing.T) { contract(t, serve(t, master), slaveRoot) })
}

func TestSocketSelfHeals(t *testing.T) {
	oldCheck := SocketCheckInterval // restored after the server stops
	t.Cleanup(func() { SocketCheckInterval = oldCheck })
	SocketCheckInterval = 20 * time.Millisecond
	share := t.TempDir()
	master, _ := newEngine(t, share, "master")
	dir, _ := os.MkdirTemp("", "fkbr-sock")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "api.sock")
	l, err := Listen(sock, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ServeSocket(ctx, l, sock, 0o600, master, nil); close(done) }()
	defer func() { cancel(); <-done }()

	os.Remove(sock) // e.g. a /tmp cleaner
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, err := Dial(context.Background(), sock); err == nil {
			if c.Self() != "master" {
				t.Fatalf("self = %q", c.Self())
			}
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket never came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fi, err := os.Stat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("recreated socket: %v %v", fi, err)
	}
}
