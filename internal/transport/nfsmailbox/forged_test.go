package nfsmailbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/transport"
)

// Names from the share become paths; forged ones with "../" must not
// reach outside it (security audit 2026-10-22).
func TestForgedNamesStayOnTheShare(t *testing.T) {
	top := t.TempDir()
	share := filepath.Join(top, "share")
	os.Mkdir(share, 0o777)
	outside := filepath.Join(top, "outside", "x")
	os.MkdirAll(outside, 0o755)
	b := open(t, share, "b")

	// A request from "../../outside": its reply would land in outside/x.
	for _, from := range []string{"../../outside", "a/../../../outside"} {
		os.WriteFile(filepath.Join(share, "inbox", "b", "new", "x.json"),
			[]byte(`{"protocol":2,"id":"x","type":"list","from":"`+from+`","to":"b"}`), 0o644)
		got, err := b.Fetch(ctx)
		if err != nil || len(got) != 0 {
			t.Fatalf("from %q: Fetch = %v, %v; want it parked", from, got, err)
		}
	}
	if err := b.Reply(ctx, &transport.Message{ID: "x", From: "../../outside"}, &transport.Reply{Final: true}); err == nil {
		t.Error("Reply to a forged sender succeeded")
	}
	if des, _ := os.ReadDir(outside); len(des) > 0 {
		t.Errorf("written outside the share: %v", des)
	}

	// A job whose source or target is no node ID is refused (and pruned).
	for i, req := range []api.TransferRequest{
		{SrcNode: "../../outside", DstNodes: []string{"b"}},
		{SrcNode: "b", DstNodes: []string{"../../outside"}},
		{DstNodes: []string{"b"}},
	} {
		id := "00000000000000000000000" + string(rune('1'+i))
		dir := filepath.Join(share, "jobs", id)
		os.MkdirAll(dir, 0o777)
		if err := b.writeJSON(filepath.Join(dir, "job.json"), &api.Job{ID: id, Request: req}); err != nil {
			t.Fatal(err)
		}
		if _, err := b.GetJob(ctx, id); !errors.Is(err, errBadRecord) {
			t.Errorf("job %v: GetJob err = %v, want a bad record", req, err)
		}
		past := time.Now().Add(-2 * time.Hour)
		os.Chtimes(filepath.Join(dir, "job.json"), past, past)
	}
	if err := b.PruneJobs(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(filepath.Join(share, "jobs")); len(des) > 0 {
		t.Errorf("bad job records not pruned: %d left", len(des))
	}
}

func TestShareReadsAreCapped(t *testing.T) {
	share := t.TempDir()
	b := open(t, share, "b")
	defer func(n int64) { maxShareFile = n }(maxShareFile)
	maxShareFile = 1 << 10
	big := `{"protocol":2,"id":"x","type":"list","from":"a","to":"b","body":"` + strings.Repeat("a", 2<<10) + `"}`
	os.WriteFile(filepath.Join(share, "inbox", "b", "new", "x.json"), []byte(big), 0o644)
	for i := 0; i < maxReadTries; i++ {
		got, err := b.Fetch(ctx)
		if len(got) != 0 {
			t.Fatalf("an oversized request was read: %v", got)
		}
		if i == 0 && !errors.Is(err, errTooBig) {
			t.Fatalf("Fetch err = %v, want too big", err)
		}
	}
	if _, err := os.Stat(filepath.Join(share, "inbox", "b", "failed", "x.json")); err != nil {
		t.Fatalf("oversized request not parked: %v", err)
	}
}
