package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/api"
)

// Eighth review pass (2026-11-07, testplan.md 26): store findings.

// A COMPLETE marker is the count of chunks the source made. One that is not
// a plain positive number is no marker (0: not complete, targets wait); it
// used to be read as 0 or negative with the error thrown away, and a huge
// one sent a restarted source past the end.
func TestCompleteMarkerMustBeACount(t *testing.T) {
	share := t.TempDir()
	m, err := Open(share, "a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := m.Register(ctx, api.NodeInfo{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateJob(ctx, &api.Job{ID: "j1", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		marker string
		want   int
	}{
		{"7\n", 7},
		{"-1\n", 0},
		{"garbage", 0},
		{"9223372036854775807", 0},
		{"2147483648", 0},
		{"", 0},
	} {
		os.WriteFile(filepath.Join(share, "jobs", "j1", "COMPLETE"), []byte(c.marker), 0o666)
		j, err := m.GetJob(ctx, "j1")
		if err != nil {
			t.Fatalf("marker %q: %v", c.marker, err)
		}
		if j.TotalChunks != c.want {
			t.Errorf("marker %q: TotalChunks %d, want %d", c.marker, j.TotalChunks, c.want)
		}
	}
}
