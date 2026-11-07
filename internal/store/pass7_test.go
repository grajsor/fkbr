package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fkbr/internal/api"
)

// Seventh review pass (2026-11-07, testplan.md 25): store findings.

// touchProbe is the NFS refresh trick without the removal: it bumps the
// directory's mtime the way a read does.
func touchProbe(dir string, touch func(string) error) {
	_ = touch(filepath.Join(dir, ".r-probe"))
}

func putOldBlob(t *testing.T, m *Dir, share, name string, age time.Duration) string {
	t.Helper()
	w, err := m.CreateBlob(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("blob"))
	if err := w.Commit(nil); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(share, "blobs", filepath.FromSlash(name))
	old := time.Now().Add(-age)
	os.Chtimes(p, old, old)
	os.Chtimes(filepath.Dir(p), old, old)
	return filepath.Dir(p)
}

// Reading a blob refreshes its folder (Options.Fresh); that must not keep
// an old plan alive: freshness is judged by the entries.
func TestPruneBlobsIgnoresReadRefresh(t *testing.T) {
	share := t.TempDir()
	m, err := Open(share, "a", Options{Fresh: touchProbe})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(context.Background(), api.NodeInfo{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	dir := putOldBlob(t, m, share, "plans/old/x", 3*time.Hour)
	rc, err := m.OpenBlob(context.Background(), "plans/old/x")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if err := m.PruneBlobs(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("a 3h-old unused plan was kept because a read refreshed its folder")
	}
}

// A blob folder is made before its file is renamed in. A fresh empty one
// is being written and stays; an old empty one goes.
func TestPruneBlobsKeepsFreshEmptyFolder(t *testing.T) {
	share := t.TempDir()
	m, err := Open(share, "a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(context.Background(), api.NodeInfo{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	fresh, stale := filepath.Join(share, "blobs", "plans", "fresh"), filepath.Join(share, "blobs", "plans", "stale")
	for _, d := range []string{fresh, stale} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(stale, old, old)
	if err := m.PruneBlobs(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a folder being written was pruned: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("an old empty folder was kept")
	}
}

// A request that cannot be parked (failed/ is gone, or the rename fails
// for a moment) stays claimed; the error is reported, the layout healed,
// and the next Fetch parks it.
func TestFetchKeepsClaimWhenParkFails(t *testing.T) {
	share := t.TempDir()
	b := open(t, share, "b")
	os.WriteFile(filepath.Join(share, "inbox", "b", "new", "0001.json"),
		[]byte(`{"protocol":2,"id":"other","type":"list","from":"a","to":"b"}`), 0o644)
	os.RemoveAll(filepath.Join(share, "inbox", "b", "failed"))
	ctx := context.Background()
	if _, err := b.Fetch(ctx); err == nil {
		t.Error("the failed park was not reported")
	}
	if _, kept := b.claimed["0001.json"]; !kept {
		t.Fatal("the claim was dropped with the request still in cur/")
	}
	for i := 0; i < 2; i++ {
		b.Fetch(ctx)
	}
	if _, err := os.Stat(filepath.Join(share, "inbox", "b", "failed", "0001.json")); err != nil {
		t.Errorf("the request was never parked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(share, "inbox", "b", "cur", "0001.json")); err == nil {
		t.Error("the request is still in cur/")
	}
	if _, kept := b.claimed["0001.json"]; kept {
		t.Error("the claim outlived the parked request")
	}
}
