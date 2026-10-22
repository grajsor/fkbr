package engine

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync/atomic"
	"testing"

	"fkbr/internal/inventory"
	"fkbr/internal/transport"
)

type countingStore struct {
	transport.Store
	opens atomic.Int32
}

func (s *countingStore) OpenBlob(ctx context.Context, name string) (io.ReadCloser, error) {
	s.opens.Add(1)
	return s.Store.OpenBlob(ctx, name)
}

// An apply's Remove batches read the target's scan once, forward, and get
// the same entries as reading it whole for each batch.
func TestScanReadOnceAcrossBatches(t *testing.T) {
	e, _ := node(t, t.TempDir(), "n1")
	ctx := context.Background()
	var all []string
	w, err := e.st.CreateBlob(ctx, snapshotBlob("p1", "n1"))
	if err != nil {
		t.Fatal(err)
	}
	sw, err := inventory.NewWriter(w, inventory.Header{})
	if err != nil {
		t.Fatal(err)
	}
	add := func(p, typ string) {
		all = append(all, p)
		if err := sw.Write(&inventory.Entry{P: p, T: typ}); err != nil {
			t.Fatal(err)
		}
	}
	add(".", inventory.TypeDir)
	for i := range 1000 {
		d := fmt.Sprintf("m/d%04d", i)
		if i == 0 {
			add("m", inventory.TypeDir)
		}
		add(d, inventory.TypeDir)
		add(d+"/f", inventory.TypeFile)
		add(d+"-x", inventory.TypeFile) // sorts after d's subtree
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(nil); err != nil {
		t.Fatal(err)
	}
	cs := &countingStore{Store: e.st}
	e.st = cs

	// What a full read gives for tops (the old way).
	want := func(tops []string) []string {
		var out []string
		for _, p := range all {
			for _, top := range tops {
				if within(top, p) {
					out = append(out, p)
					break
				}
			}
		}
		return out
	}
	batch := func(plan string, from, to int) {
		t.Helper()
		var paths []string
		for i := from; i < to; i++ {
			paths = append(paths, fmt.Sprintf("base/m/d%04d", i), fmt.Sprintf("base/m/d%04d-x", i))
		}
		got, err := e.scannedBelow(ctx, plan, "base", paths)
		if err != nil {
			t.Fatal(err)
		}
		var tops []string
		for _, p := range paths {
			tops = append(tops, inventoryRel("base", p))
		}
		keys := slices.SortedFunc(maps.Keys(got), inventory.ComparePaths)
		if w := want(tops); !slices.Equal(keys, w) {
			t.Fatalf("batch %d-%d: %d entries, want %d", from, to, len(keys), len(w))
		}
	}
	batch("p1", 0, 400)
	batch("p1", 400, 800)
	batch("p1", 800, 1000)
	if n := cs.opens.Load(); n != 1 {
		t.Errorf("scan opened %d times for three batches, want 1", n)
	}
	batch("p1", 100, 200) // going back: read again from the top
	if n := cs.opens.Load(); n != 2 {
		t.Errorf("going back: %d opens, want 2", n)
	}
	if _, err := e.scannedBelow(ctx, "p2", "base", []string{"base/m"}); err == nil {
		t.Error("a plan without a scan: no error")
	}
	batch("p1", 0, 10) // after another plan
}
