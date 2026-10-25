package nfsmailbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/store"
)

func TestRefreshLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	r := newRefresher(share, "a")
	m, err := store.Open(share, "a", store.Options{Fresh: r.fresh})
	if err != nil {
		t.Fatal(err)
	}
	m.Register(ctx, api.NodeInfo{ID: "a"})
	m.Heartbeat(ctx, "w1 1 a")
	for i := 0; i < 5; i++ {
		if _, err := m.Nodes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Throttled: a burst refreshes each directory once.
	if n := r.seq.Load(); n < 2 || n > 3 {
		t.Errorf("refreshes in a burst of 5 Nodes calls: %d", n)
	}
	filepath.Walk(share, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(fi.Name(), ".r-") {
			t.Errorf("refresh file left behind: %s", p)
		}
		return nil
	})
}

// A directory another node has not created yet may be cached as missing in
// its parent. Refreshing it must then refresh the parent, or the client
// keeps "does not exist" for as long as the mount caches attributes.
func TestRefreshMissingDirRefreshesParent(t *testing.T) {
	share := t.TempDir()
	r := newRefresher(share, "a")
	parent := filepath.Join(share, "data", "src")
	if err := os.MkdirAll(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(parent, old, old); err != nil {
		t.Fatal(err)
	}
	touch := func(p string) error {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
		return os.Remove(p)
	}
	r.fresh(filepath.Join(parent, "job", "acks"), touch) // neither job nor acks exists
	fi, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().After(old.Add(time.Minute)) {
		t.Errorf("parent of a missing directory not refreshed (mtime %v)", fi.ModTime())
	}
	if _, err := os.Stat(filepath.Join(parent, "job")); !os.IsNotExist(err) {
		t.Errorf("refresh created the missing directory: %v", err)
	}
}
