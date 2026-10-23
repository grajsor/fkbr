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
	"fkbr/internal/id"
	"fkbr/internal/transport"
)

// withLocal gives cfg a local-only root "local" with a file in it.
func withLocal(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local")
	os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	os.WriteFile(filepath.Join(dir, "etc", "secret"), []byte("not for peers"), 0o600)
	os.WriteFile(filepath.Join(dir, "mine"), []byte("mine to send"), 0o644)
	cfg.Roots = append(cfg.Roots, config.Root{ID: "local", Path: dir, AllowWrite: true, Local: true})
	return dir
}

func rootIDs(n api.Node) []string {
	var ids []string
	for _, r := range n.Roots {
		ids = append(ids, r.ID)
	}
	return ids
}

// Other nodes neither see a local root nor reach it.
func TestLocalRootUnseenByPeers(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	withLocal(t, acfg)
	a := build(t, acfg)
	b, _ := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	for _, view := range []*Engine{a, b} {
		ns, _ := view.Nodes(ctx)
		for _, n := range ns {
			if n.ID != "a" {
				continue
			}
			got := strings.Join(rootIDs(n), ",")
			want := "data"
			if view == a {
				want = "data,local" // a's own TUI and CLI see it
			}
			if got != want {
				t.Errorf("a's roots as %s sees them: %s, want %s", view.Self(), got, want)
			}
		}
	}
	if _, err := b.List(ctx, api.ListRequest{Node: "a", Root: "local", Path: "etc"}); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("list: %v", err)
	}
	if _, err := b.Read(ctx, api.ReadRequest{Node: "a", Root: "local", Path: "etc/secret"}); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("read: %v", err)
	}
	if err := b.Mkdir(ctx, api.MkdirRequest{Node: "a", Root: "local", Path: "planted"}); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("mkdir: %v", err)
	}
	if _, err := b.Remove(ctx, api.RemoveRequest{Node: "a", Root: "local", Paths: []string{"etc"}}); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("remove: %v", err)
	}
	if _, err := b.Submit(ctx, api.TransferRequest{SrcNode: "a", SrcRoot: "local", Paths: []string{"etc"}, DstNodes: []string{"b"}, DstRoot: "data"}); !errors.Is(err, api.ErrUnknownRoot) {
		t.Errorf("copy out of it, asked by b: %v", err)
	}
	if _, err := a.List(ctx, api.ListRequest{Root: "local", Path: "etc"}); err != nil {
		t.Errorf("a's own listing: %v", err)
	}
	if _, err := a.PlanSync(ctx, api.SyncRequest{SrcRoot: "local", SrcPath: "etc", DstNodes: []string{"b"}, DstRoot: "data", DstPath: "x"}); !errors.Is(err, api.ErrNotAllowed) {
		t.Errorf("sync from it: %v", err)
	}
}

// A copy this node starts reads from its local root; a job record made or
// changed by anyone else is refused, however it names this node.
func TestLocalRootOnlyOwnJobs(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	withLocal(t, acfg)
	a := build(t, acfg)
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	jid, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "local", Paths: []string{"mine"}, DstNodes: []string{"b"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, jid, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("own copy: %s %+v", j.State, j.Participants)
	}
	if b, _ := os.ReadFile(filepath.Join(broot, "mine")); string(b) != "mine to send" {
		t.Fatalf("copied: %q", b)
	}

	forge := func(jobID string, paths []string) {
		t.Helper()
		j := &api.Job{ID: jobID, Initiator: "a", Created: time.Now(), Request: api.TransferRequest{
			SrcNode: "a", SrcRoot: "local", Paths: paths, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "loot", Conflict: api.DefaultConflictPolicy,
		}}
		if err := b.st.CreateJob(ctx, j); err != nil { // b writes it, claiming a started it
			t.Fatal(err)
		}
		msg, _ := b.newMessage("a", transport.TypeSend, transport.JobRef{ID: jobID})
		msg.Notify = true
		if err := b.tr.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(jobID, why string) {
		t.Helper()
		var st *api.JobStatus
		waitUntil(t, func() bool {
			j, err := a.Job(ctx, jobID)
			if err == nil {
				st = ownStatus(j, "a", api.RoleSource)
			}
			return st != nil && st.State.Terminal()
		}, "a's answer to job "+jobID)
		if st.State != api.JobFailed || !strings.Contains(st.Error, why) {
			t.Errorf("job %s: %s %q, want failed: %s", jobID, st.State, st.Error, why)
		}
	}
	// A job b made up.
	made := id.New()
	forge(made, []string{"etc"})
	refused(made, "no record of starting it")
	// A job a submitted (its record kept), changed on the share before a
	// ran it, to send something else.
	changed := id.New()
	if err := a.keepOwn(&api.Job{ID: changed, Request: api.TransferRequest{
		SrcNode: "a", SrcRoot: "local", Paths: []string{"mine"}, DstNodes: []string{"b"}, DstRoot: "data", DstDir: "loot", Conflict: api.DefaultConflictPolicy,
	}}); err != nil {
		t.Fatal(err)
	}
	forge(changed, []string{"etc"})
	refused(changed, "differs")
	if _, err := os.Stat(filepath.Join(broot, "loot")); !os.IsNotExist(err) {
		t.Fatalf("a forged job sent data: %v", err)
	}
}

// Into a local root only where no one else can write the share.
func TestLocalRootWritesNeedPrivateShare(t *testing.T) {
	share := t.TempDir()
	acfg := nodeCfg(t, share, "a")
	ldir := withLocal(t, acfg)
	a := build(t, acfg)
	b, broot := node(t, share, "b")
	os.WriteFile(filepath.Join(broot, "f"), []byte("from b"), 0o644)
	runWorker(t, a)
	runWorker(t, b)
	ctx := context.Background()
	req := api.TransferRequest{SrcNode: "b", SrcRoot: "data", Paths: []string{"f"}, DstNodes: []string{"a"}, DstRoot: "local", DstDir: "in"}
	if _, err := a.Submit(ctx, req); !errors.Is(err, api.ErrNotAllowed) {
		t.Fatalf("into a local root over an open share: %v", err)
	}
	acfg.Home = true // a private share: an ssh swarm's records and links
	jid, err := a.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, jid, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("%s %+v", j.State, j.Participants)
	}
	if got, _ := os.ReadFile(filepath.Join(ldir, "in", "f")); string(got) != "from b" {
		t.Fatalf("arrived: %q", got)
	}
}

// A move on one node from a root into another root that holds it would
// delete what it copied.
func TestMoveAcrossOverlappingRoots(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	outer := filepath.Dir(cfg.Roots[0].Path) // holds root "data"
	cfg.Roots = append(cfg.Roots, config.Root{ID: "outer", Path: outer, AllowWrite: true})
	a := build(t, cfg)
	runWorker(t, a)
	makeTree(t, cfg.Roots[0].Path, 3, 1<<10)
	ctx := context.Background()
	for _, dst := range []string{"tree/inside", "tree", "."} {
		_, err := a.Submit(ctx, api.TransferRequest{SrcRoot: "outer", Paths: []string{"data/tree"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: dst, Move: true})
		if !errors.Is(err, api.ErrBadPath) {
			t.Errorf("move outer:data/tree into data:%s: %v", dst, err)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Roots[0].Path, "tree")); err != nil {
		t.Fatal(err)
	}
}
