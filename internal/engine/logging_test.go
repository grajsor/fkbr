package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
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
