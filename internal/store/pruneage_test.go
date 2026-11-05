package store

import (
	"testing"
	"time"

	"fkbr/internal/api"
)

// A negative age puts the prune cutoff in the future. It is refused by all
// three prunes, and nothing goes: a finished job past the one-minute floor
// was deleted by PruneJobs(-1h) before (review 2026-11-05). Zero is still a
// policy (TestPruneFloors).
func TestPruneRefusesNegativeAge(t *testing.T) {
	share := t.TempDir()
	a := open(t, share, "a")
	j := &api.Job{ID: "000000000000000000000011", Request: api.TransferRequest{SrcNode: "a", DstNodes: []string{"a"}}}
	a.CreateJob(ctx, j)
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleSource, State: api.JobDone})
	a.PutStatus(ctx, j.ID, &api.JobStatus{Node: "a", Role: api.RoleTarget, State: api.JobDone})
	backdateJob(share, j.ID, time.Now().Add(-2*time.Minute))
	for name, err := range map[string]error{
		"Prune":      a.Prune(ctx, -time.Hour),
		"PruneJobs":  a.PruneJobs(ctx, -time.Hour),
		"PruneBlobs": a.PruneBlobs(ctx, -time.Hour),
	} {
		if err == nil {
			t.Errorf("%s accepted a negative age", name)
		}
	}
	if _, err := a.GetJob(ctx, j.ID); err != nil {
		t.Fatalf("a finished job went with a negative age: %v", err)
	}
}
