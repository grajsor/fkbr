package engine

import (
	"context"
	"errors"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/inventory"
	"fkbr/internal/transport"
)

// recBlobs is a store whose blobs cannot be published, and which notes
// the writers that were given up.
type recBlobs struct {
	transport.Store
	writers []*recBlob
}

type recBlob struct {
	aborted bool
}

func (r *recBlobs) CreateBlob(context.Context, string) (transport.ChunkWriter, error) {
	w := &recBlob{}
	r.writers = append(r.writers, w)
	return w, nil
}

func (b *recBlob) Write(p []byte) (int, error) { return len(p), nil }
func (b *recBlob) Commit([]byte) error         { return errors.New("share full") }
func (b *recBlob) Abort()                      { b.aborted = true }

// When a plan's lists cannot be published, every writer is given up, not
// left with its temp file (or, over ssh, its call to the home node) open
// (review 2026-11-05: only a failed Plan aborted them).
func TestPlanSinkCommitFailureAbortsWriters(t *testing.T) {
	st := &recBlobs{}
	s := &planSink{
		e: &Engine{st: st}, ctx: context.Background(),
		plan:    &api.SyncPlan{ID: "p1", Request: api.SyncRequest{SrcPath: "/s", DstPath: "/d"}},
		targets: []string{"t1", "t2"}, groups: map[uint64]*listWriter{}, deletes: map[int]*listWriter{},
	}
	if err := s.Copy(1, &inventory.Entry{P: "a", T: inventory.TypeFile, S: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Copy(3, &inventory.Entry{P: "b", T: inventory.TypeFile, S: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(0, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.commit(); err == nil {
		t.Fatal("commit of lists that cannot be published succeeded")
	}
	if len(st.writers) != 3 {
		t.Fatalf("%d writers, want 3", len(st.writers))
	}
	for i, w := range st.writers {
		if !w.aborted {
			t.Errorf("writer %d was left open", i)
		}
	}
}
