package engine

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/transport"
)

// Over ssh chunks go from the source's memory to its targets (docs/ssh.md,
// "Data"). These tests run as an ssh swarm whatever FKBR_TEST_TRANSPORT
// says.

// sshSwarm makes the nodes the test builds an ssh swarm: the first one
// built is the home node, the others are linked to it.
func sshSwarm(t *testing.T) {
	old := linkTest
	linkTest = true
	t.Cleanup(func() { linkTest = old })
}

// slowChunks reads each chunk only after a pause: a slow target that
// holds the window.
type slowChunks struct {
	transport.Store
	pause time.Duration
}

func (s *slowChunks) OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	select {
	case <-time.After(s.pause):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Store.OpenChunk(ctx, src, id, seq)
}

func (s *slowChunks) ChunksInMemory() bool {
	m, ok := s.Store.(interface{ ChunksInMemory() bool })
	return ok && m.ChunksInMemory()
}

func slow(pause time.Duration) func(transport.Store) transport.Store {
	return func(st transport.Store) transport.Store { return &slowChunks{st, pause} }
}

// chunkFiles lists the chunk files under dirs (a store writes chunks as
// NNNNNNNN.chunk, in tmp/ while being written).
func chunkFiles(dirs ...string) []string {
	var out []string
	for _, dir := range dirs {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.Contains(d.Name(), ".chunk") {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

// No chunk file is written on any disk over ssh: not by the home node as a
// source, not by a linked node, and not on the way between linked nodes.
func TestSSHNoChunkFiles(t *testing.T) {
	sshSwarm(t)
	share := t.TempDir()
	hcfg, acfg, bcfg := nodeCfg(t, share, "home"), nodeCfg(t, share, "a"), nodeCfg(t, share, "b")
	home := buildWrapped(t, hcfg, slow(20*time.Millisecond))
	a := buildWrapped(t, acfg, slow(20*time.Millisecond))
	b := buildWrapped(t, bcfg, slow(20*time.Millisecond))
	for _, e := range []*Engine{home, a, b} {
		runWorker(t, e)
	}
	makeTree(t, hcfg.Roots[0].Path, 20, 2<<20)
	makeTree(t, acfg.Roots[0].Path, 20, 2<<20)
	dirs := []string{share, hcfg.StateDir, acfg.StateDir, bcfg.StateDir}

	var mu sync.Mutex
	var seen []string
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if f := chunkFiles(dirs...); len(f) > 0 {
				mu.Lock()
				seen = append(seen, f...)
				mu.Unlock()
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	req := api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstRoot: "data", DstDir: "in"}
	jobs := map[*Engine]string{}
	for _, c := range []struct {
		src *Engine
		dst []string
	}{{home, []string{"a", "b"}}, {a, []string{"home", "b"}}} {
		r := req
		r.DstNodes = c.dst
		id, err := c.src.Submit(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		jobs[c.src] = id
	}
	for src, id := range jobs {
		if j := waitJob(t, src, id, 60*time.Second); j.State != api.JobDone {
			t.Fatalf("state %s: %+v", j.State, j.Participants)
		}
	}
	close(stop)
	<-done
	if len(seen) > 0 {
		t.Errorf("chunk files on disk: %q", seen[:min(len(seen), 5)])
	}
	sameTree(t, filepath.Join(hcfg.Roots[0].Path, "tree"), filepath.Join(acfg.Roots[0].Path, "in", "tree"))
	sameTree(t, filepath.Join(acfg.Roots[0].Path, "tree"), filepath.Join(hcfg.Roots[0].Path, "in", "tree"))
}

// sshRestart runs a copy from src to dst in an ssh swarm of home and a
// (linked), restarts one of them once the target acked some chunks, and
// wants equal trees.
func sshRestart(t *testing.T, src, dst, restart string) {
	sshSwarm(t)
	share := t.TempDir()
	cfgs := map[string]*config.Config{"home": nodeCfg(t, share, "home"), "a": nodeCfg(t, share, "a")}
	engines := map[string]*Engine{}
	stops := map[string]func(){}
	for _, n := range []string{"home", "a"} { // home first: the home node
		wrap := slow(0)
		if n == dst {
			wrap = slow(30 * time.Millisecond) // the window stays full
		}
		engines[n] = buildWrapped(t, cfgs[n], wrap)
		stops[n] = runWorker(t, engines[n])
	}
	sroot, droot := cfgs[src].Roots[0].Path, cfgs[dst].Roots[0].Path
	makeTree(t, sroot, 30, 3<<20)
	ctx := context.Background()
	id, err := engines[src].Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{dst}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		n, _ := engines[src].st.Acked(ctx, src, id, dst)
		return n >= 3
	}, "target acked 3 chunks")
	stops[restart]()
	if j, _ := engines[src].Job(ctx, id); j.State.Terminal() {
		t.Fatalf("job finished before the restart: %s", j.State)
	}
	wrap := slow(0)
	if restart == dst {
		wrap = slow(30 * time.Millisecond)
	}
	engines[restart] = buildWrapped(t, cfgs[restart], wrap)
	runWorker(t, engines[restart])
	if j := waitJob(t, engines[src], id, 60*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(sroot, "tree"), filepath.Join(droot, "in", "tree"))
}

// A source restarted mid-job has lost the chunks it held in memory: it
// builds them again from the lowest chunk a target has not acked.
func TestSSHSourceRestart(t *testing.T) {
	t.Run("home", func(t *testing.T) { sshRestart(t, "home", "a", "home") })
	t.Run("linked", func(t *testing.T) { sshRestart(t, "a", "home", "a") })
}

// A target restarted mid-job asks for its first unacked chunk, which its
// source still holds.
func TestSSHTargetRestart(t *testing.T) {
	t.Run("home", func(t *testing.T) { sshRestart(t, "a", "home", "home") })
	t.Run("linked", func(t *testing.T) { sshRestart(t, "home", "a", "a") })
}

// A restarted source whose files changed since the chunks a target
// applied stops, instead of sending chunks that do not follow on.
func TestSSHSourceChangedOnRestart(t *testing.T) {
	sshSwarm(t)
	share := t.TempDir()
	hcfg, acfg := nodeCfg(t, share, "home"), nodeCfg(t, share, "a")
	home := buildWrapped(t, hcfg, slow(0))
	a := buildWrapped(t, acfg, slow(30*time.Millisecond))
	stop := runWorker(t, home)
	runWorker(t, a)
	root := hcfg.Roots[0].Path
	makeTree(t, root, 30, 3<<20)
	ctx := context.Background()
	id, err := home.Submit(ctx, api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, func() bool {
		j, _ := home.Job(ctx, id)
		s := ownStatus(j, "a", api.RoleTarget)
		return s != nil && s.Chunks >= 3 && s.ChunkSHA != ""
	}, "target applied 3 chunks")
	stop()
	// Same size and mtime, other bytes: only the chunks tell.
	big := filepath.Join(root, "tree", "big.bin")
	st, _ := os.Stat(big)
	b, _ := os.ReadFile(big)
	for i := range b {
		b[i] ^= 0xff
	}
	os.WriteFile(big, b, 0o600)
	os.Chtimes(big, st.ModTime(), st.ModTime())
	home = buildWrapped(t, hcfg, slow(0))
	runWorker(t, home)
	j := waitJob(t, home, id, 60*time.Second)
	if s := ownStatus(j, "home", api.RoleSource); j.State != api.JobFailed || s == nil || !strings.Contains(s.Error, "source changed") {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
}

// vanishing loses chunk 3 once, between its meta and its bytes: the source
// restarted in between.
type vanishing struct {
	transport.Store
	mu   sync.Mutex
	gone bool
}

func (v *vanishing) OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if seq == 3 && !v.gone {
		v.gone = true
		return nil, &fs.PathError{Op: "open", Path: "chunk 3", Err: fs.ErrNotExist}
	}
	return v.Store.OpenChunk(ctx, src, id, seq)
}

func (v *vanishing) ChunksInMemory() bool { return true }

// A chunk that is gone after its meta was read is waited for again, not
// a failure: its source restarted and builds it again.
func TestSSHChunkGoneAfterMeta(t *testing.T) {
	sshSwarm(t)
	share := t.TempDir()
	hcfg, acfg := nodeCfg(t, share, "home"), nodeCfg(t, share, "a")
	home := build(t, hcfg)
	a := buildWrapped(t, acfg, func(st transport.Store) transport.Store { return &vanishing{Store: st} })
	runWorker(t, home)
	runWorker(t, a)
	makeTree(t, hcfg.Roots[0].Path, 20, 1<<20)
	id, err := home.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"a"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, home, id, 30*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(hcfg.Roots[0].Path, "tree"), filepath.Join(acfg.Roots[0].Path, "in", "tree"))
}

// What a source holds in memory stays within job_quota while a slow
// target holds the window.
func TestSSHMemoryWithinQuota(t *testing.T) {
	sshSwarm(t)
	share := t.TempDir()
	hcfg, acfg := nodeCfg(t, share, "home"), nodeCfg(t, share, "a")
	home := buildWrapped(t, hcfg, slow(30*time.Millisecond))
	a := build(t, acfg)
	runWorker(t, home)
	runWorker(t, a)
	makeTree(t, acfg.Roots[0].Path, 20, 4<<20) // 16 times the job quota
	id, err := a.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"home"}, DstRoot: "data", DstDir: "in",
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, a, id, 60*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	sameTree(t, filepath.Join(acfg.Roots[0].Path, "tree"), filepath.Join(hcfg.Roots[0].Path, "in", "tree"))
	q := int64(acfg.Transfer.JobQuota)
	c := int64(acfg.Transfer.ChunkSize)
	// A chunk may pass its limit by its last entry's header.
	most := q + c/64 + 64<<10
	peak := a.st.(interface{ ChunkPeak() int64 }).ChunkPeak()
	if peak == 0 || peak > most {
		t.Errorf("chunks held up to %d bytes in memory; job_quota %d", peak, q)
	}
	if peak < q/2 {
		t.Errorf("chunks held only %d bytes: the target did not hold the window", peak)
	}
}
