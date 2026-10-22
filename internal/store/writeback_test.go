package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

// Bulk writes sync on the way (after syncMin at first), which measures the
// rate the share takes data at; the bound follows it within limits.
func TestBulkWritesSyncOnTheWay(t *testing.T) {
	writeRate.Store(0)
	if syncEvery() != syncMin {
		t.Fatalf("unmeasured: sync every %d, want %d", syncEvery(), syncMin)
	}
	m, err := Open(t.TempDir(), "a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := m.CreateChunk(context.Background(), "0123456789abcdef01234567", 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for range 9 {
		if _, err := w.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if writeRate.Load() <= 0 {
		t.Fatal("no sync happened within 9 MiB")
	}
	writeRate.Store(1 << 40)
	if syncEvery() != syncMax {
		t.Errorf("fast share: sync every %d, want the cap %d", syncEvery(), syncMax)
	}
	writeRate.Store(0)
}

// Now follows the share's clock (file mtimes the server sets), not this
// host's. On a local filesystem both are the same clock.
func TestNowIsTheSharesClock(t *testing.T) {
	m, err := Open(t.TempDir(), "a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(context.Background(), api.NodeInfo{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	m.offset.Store(int64(2 * time.Hour))
	if _, err := m.Heartbeat(context.Background(), "w1 1 x"); err != nil && err != transport.ErrNotRegistered {
		t.Fatal(err)
	}
	if d := time.Until(m.Now()); d > time.Second || d < -time.Second {
		t.Errorf("after a heartbeat Now is %s off the share's clock", d)
	}
}

// A client that only looks (a CLI command run where no node runs) leaves
// no folders of its node ID on the share, even after that node was
// forgotten.
func TestVisitorLeavesNoFolders(t *testing.T) {
	share := t.TempDir()
	b, err := Open(share, "b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	b.Register(context.Background(), api.NodeInfo{ID: "b"})
	v, err := Open(share, "visitor", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Nodes(context.Background()); err != nil {
		t.Fatal(err)
	}
	v.Prune(context.Background(), time.Hour)
	for _, d := range []string{"nodes", "inbox", "reply", "data"} {
		if _, err := os.Stat(filepath.Join(share, d, "visitor")); !os.IsNotExist(err) {
			t.Errorf("%s/visitor exists (%v)", d, err)
		}
	}
}
