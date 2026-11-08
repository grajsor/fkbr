package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// A file read ahead is read for the size the walk saw, plus one byte to
// show that it grew: one that was small at the walk and is big when
// opened (replaced, or growing) must not be held in memory whole, eight
// at a time.
func TestPrefetchReadsNoMoreThanItsSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grown")
	if err := os.WriteFile(path, make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newPrefetcher()
	pf := p.start(10, func() (*os.File, error) { return os.Open(path) }, make(chan struct{}))
	<-pf.done
	if pf.read != nil || pf.open != nil {
		t.Fatalf("read %v, open %v", pf.read, pf.open)
	}
	if len(pf.data) != 11 {
		t.Errorf("held %d bytes of a file the walk saw as 10 bytes, want 11 (size plus one)", len(pf.data))
	}
	if pf.fi == nil || pf.fi.Size() != 3<<20 {
		t.Errorf("the file's real size must show, so that the bundle marks it changed: %v", pf.fi)
	}
	// A file that did not change is read whole, to its last byte.
	ok := filepath.Join(filepath.Dir(path), "same")
	if err := os.WriteFile(ok, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf = p.start(10, func() (*os.File, error) { return os.Open(ok) }, make(chan struct{}))
	<-pf.done
	if string(pf.data) != "0123456789" {
		t.Errorf("got %q", pf.data)
	}
}
