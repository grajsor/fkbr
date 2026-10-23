package engine

import "sync"

// spaceBook keeps the space (disk, or memory over ssh) that chunks being
// written may still take, so senders running at once do not each count
// the same free space
// (two jobs on a host with /tmp in memory both started a chunk that fit
// alone, and filled it).
type spaceBook struct {
	mu   sync.Mutex
	held int64
}

// take holds need bytes if they fit in free beside what other chunks
// hold, or if none of this node's chunks are staged or being written:
// then nothing here would free space, and the write finds out. nil: wait.
func (b *spaceBook) take(free, need, staged int64) *spaceHold {
	b.mu.Lock()
	defer b.mu.Unlock()
	if free-b.held < need && (staged > 0 || b.held > 0) {
		return nil
	}
	b.held += need
	return &spaceHold{b: b, left: need}
}

// spaceHold is one chunk's share of the book. What the chunk writes is in
// the free space from then on, so it gives that much back as it goes.
type spaceHold struct {
	b    *spaceBook
	mu   sync.Mutex
	left int64
}

func (h *spaceHold) wrote(n int64) {
	h.mu.Lock()
	n = min(n, h.left)
	h.left -= n
	h.mu.Unlock()
	h.b.give(n)
}

// release gives back the rest once the chunk is published or dropped.
func (h *spaceHold) release() {
	if h == nil {
		return
	}
	h.mu.Lock()
	n := h.left
	h.left = 0
	h.mu.Unlock()
	h.b.give(n)
}

func (b *spaceBook) give(n int64) {
	b.mu.Lock()
	b.held -= n
	b.mu.Unlock()
}

// heldWriter counts what a chunk writes against its hold.
type heldWriter struct {
	w interface{ Write([]byte) (int, error) }
	h *spaceHold
}

func (w heldWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.h.wrote(int64(n))
	return n, err
}
