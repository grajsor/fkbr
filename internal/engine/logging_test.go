package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/transport"
	"fkbr/internal/transport/nfsmailbox"
)

// syncBuffer is a log sink several engines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func nodeWithLog(t *testing.T, share, id string, log *syncBuffer) (*Engine, string) {
	t.Helper()
	cfg := nodeCfg(t, share, id)
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(cfg.NFSRoot, cfg.NodeID, nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	h := slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelInfo})
	e := New(cfg, fsys, mb, mb, Options{Version: "test", Logger: slog.New(h).With("node", id)})
	t.Cleanup(func() { e.Close() })
	return e, cfg.Roots[0].Path
}

// A transfer leaves enough in the logs to follow it chunk by chunk on both
// sides, so a hang can be placed.
func TestTransferLogsProgress(t *testing.T) {
	share := t.TempDir()
	var log syncBuffer
	src, sroot := nodeWithLog(t, share, "src", &log)
	a, _ := nodeWithLog(t, share, "a", &log)
	b, _ := nodeWithLog(t, share, "b", &log)
	makeTree(t, sroot, 20, 300<<10) // spans several 64 KiB chunks
	runWorker(t, src)
	runWorker(t, a)
	runWorker(t, b)
	id, err := src.Submit(context.Background(), api.TransferRequest{
		SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"a", "b"}, DstRoot: "data", DstDir: "in", Compress: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, src, id, 20*time.Second); j.State != api.JobDone {
		t.Fatalf("state %s", j.State)
	}
	out := log.String()
	for _, want := range []string{
		`msg=sending`, `to=a,b`, `zstd=true`,
		`msg="chunk staged" node=src job=` + id, `ratio=`,
		`msg="target acked"`,
		`msg=receiving`, `from=src`,
		`msg="chunk applied"`,
		`msg=installed`,
		`msg="send done"`, `rate=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(out)
	}
}

func TestWaitLog(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	w := newWaitLog(log, "waiting for chunk", time.Hour, time.Hour)
	w.tick("chunk", 3)
	if buf.String() != "" {
		t.Fatalf("logged before the threshold: %s", buf.String())
	}
	w.start = time.Now().Add(-2 * time.Hour) // long wait
	w.tick("chunk", 3)
	w.tick("chunk", 3) // not again until waitEvery has passed
	if n := strings.Count(buf.String(), "waiting for chunk"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, buf.String())
	}
	w.done("chunk", 3)
	if !strings.Contains(buf.String(), `msg="waiting for chunk: over"`) {
		t.Errorf("end of a logged wait not logged:\n%s", buf.String())
	}
}

// cancelledFetch fails its first Fetch, then waits for a request until the
// worker stops, as a linked node's Fetch waits on the home node.
type cancelledFetch struct {
	transport.Transport
	failed, waiting atomic.Bool
}

func (f *cancelledFetch) Fetch(ctx context.Context) ([]*transport.Message, error) {
	if !f.failed.Swap(true) {
		return nil, errors.New("inbox unreadable")
	}
	f.waiting.Store(true)
	<-ctx.Done()
	return nil, ctx.Err()
}

// A fetch cut off because the worker stops is no failure: over ssh one is
// always waiting when the worker stops, and its warning read like a lost
// request (lab ssh test 2026-10-22). A real failure is still logged.
func TestStopLogsNoFetchFailure(t *testing.T) {
	var log syncBuffer
	cfg := nodeCfg(t, t.TempDir(), "a")
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := nfsmailbox.Open(cfg.NFSRoot, cfg.NodeID, nfsmailbox.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter})
	if err != nil {
		t.Fatal(err)
	}
	h := slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo})
	tr := &cancelledFetch{Transport: mb}
	e := New(cfg, fsys, tr, mb, Options{Version: "test", Logger: slog.New(h)})
	t.Cleanup(func() { e.Close() })
	stop := runWorker(t, e)
	waitUntil(t, tr.waiting.Load, "a fetch waiting")
	stop()
	if n := strings.Count(log.String(), `msg="fetch failed"`); n != 1 {
		t.Errorf("%d fetch failures logged, want the real one only:\n%s", n, log.String())
	}
}

// The source counts what it will send alongside sending, so readers can
// show progress and an ETA; the totals match what was sent.
func TestSourceCountsTotals(t *testing.T) {
	share := t.TempDir()
	master, mroot := node(t, share, "master")
	s1, _ := node(t, share, "s1")
	makeTree(t, mroot, 20, 300<<10)
	runWorker(t, master)
	runWorker(t, s1)
	id, err := master.Submit(context.Background(), api.TransferRequest{SrcRoot: "data", Paths: []string{"tree"}, DstNodes: []string{"s1"}, DstRoot: "data"})
	if err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, master, id, 20*time.Second)
	src := ownStatus(j, "master", api.RoleSource)
	if src == nil || !src.Counted {
		t.Fatalf("source status not counted: %+v", src)
	}
	if src.TotalFiles != src.Files || src.TotalBytes != src.Bytes || src.TotalBytes == 0 {
		t.Errorf("totals %d files %d bytes, sent %d files %d bytes", src.TotalFiles, src.TotalBytes, src.Files, src.Bytes)
	}
}
