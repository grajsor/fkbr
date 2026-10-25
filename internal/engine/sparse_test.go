package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

// stagedCounter counts the bytes a source stages.
type stagedCounter struct {
	transport.Store
	n atomic.Int64
}

func (c *stagedCounter) CreateChunk(ctx context.Context, id string, seq int) (transport.ChunkWriter, error) {
	w, err := c.Store.CreateChunk(ctx, id, seq)
	if err != nil {
		return nil, err
	}
	return countingWriter{w, &c.n}, nil
}

type countingWriter struct {
	transport.ChunkWriter
	n *atomic.Int64
}

func (w countingWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return w.ChunkWriter.Write(p)
}

// A sparse file's holes do not cross the share: only its data is staged,
// and the target's copy is sparse and equal.
func TestSparseFileHolesNotSent(t *testing.T) {
	share := t.TempDir()
	mcfg := nodeCfg(t, share, "master")
	mcfg.Transfer.ChunkSize, mcfg.Transfer.JobQuota, mcfg.Transfer.StagingQuota = 1<<20, 8<<20, 16<<20
	counter := &stagedCounter{}
	master := buildWrapped(t, mcfg, func(st transport.Store) transport.Store {
		counter.Store = st
		return counter
	})
	s1, sroot := node(t, share, "s1")
	src := filepath.Join(mcfg.Roots[0].Path, "disk.img")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	// A hole first, 1 MiB of data at 8 MiB, 2 MiB at 40 MiB, a hole to
	// 64 MiB.
	for _, at := range []struct {
		off int64
		n   int
	}{{8 << 20, 1 << 20}, {40 << 20, 2 << 20}} {
		if _, err := f.WriteAt(bytes.Repeat([]byte{0xab}, at.n), at.off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if onDisk(t, src) >= 32<<20 {
		t.Skip("this filesystem does not keep holes")
	}
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"disk.img"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 30*time.Second)
	if j.State != api.JobDone {
		t.Fatalf("state %s: %+v", j.State, j.Participants)
	}
	if src := ownStatus(j, "master", api.RoleSource); src.Bytes != 64<<20 {
		t.Errorf("source counted %d bytes, want the whole 64 MiB", src.Bytes)
	}
	dst := filepath.Join(sroot, "disk.img")
	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if !bytes.Equal(a, b) {
		t.Fatal("the copy differs")
	}
	if n := counter.n.Load(); n > 4<<20 {
		t.Errorf("staged %d bytes for 3 MiB of data", n)
	}
	if n := onDisk(t, dst); n > 8<<20 {
		t.Errorf("the copy takes %d bytes on disk", n)
	}
}

// onDisk is what a file takes on disk.
func onDisk(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

// bigMeta makes every chunk's meta claim 3 GiB, as a forged one could.
type bigMeta struct{ transport.Store }

func (b bigMeta) ChunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error) {
	raw, err := b.Store.ChunkMeta(ctx, src, id, seq)
	if err != nil {
		return raw, err
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw, nil
	}
	m["size"] = 3 << 30
	return json.Marshal(m)
}

// A chunk meta claiming more than any chunk holds fails the target before
// it reads anything (review 2026-10-25).
func TestChunkMetaOverBudget(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1 := buildWrapped(t, nodeCfg(t, share, "s1"), func(st transport.Store) transport.Store { return bigMeta{st} })
	makeTree(t, mroot, 2, 100<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 30*time.Second)
	st := ownStatus(j, "s1", api.RoleTarget)
	if j.State != api.JobFailed || st == nil || !strings.Contains(st.Error, "no chunk holds over") {
		t.Fatalf("job %s, target %+v", j.State, st)
	}
}
