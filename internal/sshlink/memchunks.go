package sshlink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"

	"fkbr/internal/config"
	"fkbr/internal/transport"
)

// memChunks keeps the chunks a node sends over ssh in memory, from the
// moment it writes them until every active target has read them: no chunk
// file on any disk (docs/ssh.md, "Data"). The engine bounds what is here
// (job_quota, staging_quota, FreeBytes from MemAvailable), as it bounds a
// staging directory. A restart loses them: the source then builds them
// again from the lowest chunk a target has not acked (send.go).
type memChunks struct {
	self string
	mu   sync.Mutex
	jobs map[string]map[int]*memChunk
	// total is a job's chunk count once all are staged (SetComplete):
	// a chunk past it is never coming, so nobody waits for it.
	total map[string]int
	// staged is what published chunks hold, writing what chunks being
	// written hold so far; peak the most both ever were (tests).
	staged, writing, peak int64
}

// memChunk is one published chunk. data is never changed once published,
// so a reader keeps reading it after the chunk is deleted.
type memChunk struct {
	data [][]byte
	size int64
	meta []byte
}

// chunkSource serves the meta of chunks staged here (waitMeta).
type chunkSource interface {
	ChunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error)
	past(id string, seq int) bool
}

var _ chunkSource = (*memChunks)(nil)

func validChunk(src, id string, seq int) error {
	c := chunkRef{src, id, seq}
	return c.check()
}

func newMemChunks(self string) *memChunks {
	return &memChunks{self: self, jobs: map[string]map[int]*memChunk{}, total: map[string]int{}}
}

// A chunk being written grows by blocks, from firstBlock doubling up to
// blockSize: no buffer that holds twice the chunk while it grows.
const (
	firstBlock = 64 << 10
	blockSize  = 1 << 20
)

func notStaged(id string, seq int) error {
	return &fs.PathError{Op: "open", Path: fmt.Sprintf("chunk %s/%d", id, seq), Err: fs.ErrNotExist}
}

func (m *memChunks) CreateChunk(_ context.Context, id string, seq int) (transport.ChunkWriter, error) {
	if err := validChunk(m.self, id, seq); err != nil {
		return nil, err
	}
	return &memWriter{m: m, id: id, seq: seq}, nil
}

func (m *memChunks) ChunkMeta(_ context.Context, src, id string, seq int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.jobs[id][seq]; c != nil && src == m.self {
		return c.meta, nil
	}
	return nil, notStaged(id, seq)
}

func (m *memChunks) OpenChunk(_ context.Context, src, id string, seq int) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.jobs[id][seq]
	if c == nil || src != m.self {
		return nil, notStaged(id, seq)
	}
	rs := make([]io.Reader, len(c.data))
	for i, b := range c.data {
		rs[i] = bytes.NewReader(b)
	}
	return io.NopCloser(io.MultiReader(rs...)), nil
}

func (m *memChunks) DeleteChunk(_ context.Context, id string, seq int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.jobs[id][seq]; c != nil {
		m.staged -= c.size
		delete(m.jobs[id], seq)
	}
	return nil
}

func (m *memChunks) StagedBytes(_ context.Context, id string) (job, total int64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.jobs[id] {
		job += c.size
	}
	return job, m.staged, nil
}

// complete records that job id has n chunks, all staged.
func (m *memChunks) complete(id string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total[id] = n
}

// past reports whether chunk seq of job id is past its last one.
func (m *memChunks) past(id string, seq int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.total[id]
	return ok && seq >= n
}

// RemoveJobData drops every chunk of job id.
func (m *memChunks) RemoveJobData(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.jobs[id] {
		m.staged -= c.size
	}
	delete(m.jobs, id)
	delete(m.total, id)
	return nil
}

// RemoveAll drops every chunk.
func (m *memChunks) RemoveAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs, m.total, m.staged = map[string]map[int]*memChunk{}, map[string]int{}, 0
}

// ChunksInMemory: a restart loses what is staged here (send.go).
func (m *memChunks) ChunksInMemory() bool { return true }

// jobIDs lists the jobs with chunks here.
func (m *memChunks) jobIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id := range m.jobs {
		ids = append(ids, id)
	}
	return ids
}

// Peak is the most chunk data ever held here, published or being written.
func (m *memChunks) Peak() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

func (m *memChunks) grew(n int64) {
	m.mu.Lock()
	m.writing += n
	m.peak = max(m.peak, m.staged+m.writing)
	m.mu.Unlock()
}

// memReserve is memory left to everything else when chunks wait for room.
const memReserve = 64 << 20

// FreeBytes is the memory chunks may still take: MemAvailable less a
// reserve, so a small box waits for its targets instead of swapping.
func (m *memChunks) FreeBytes() (int64, error) {
	avail, err := config.MemInfo("MemAvailable")
	if err != nil {
		return 0, err
	}
	return max(avail-memReserve, 0), nil
}

// memWriter collects a chunk being written in blocks; Commit publishes it.
type memWriter struct {
	m      *memChunks
	id     string
	seq    int
	blocks [][]byte
	size   int64
	done   bool
}

func (w *memWriter) Write(p []byte) (int, error) {
	if w.done {
		return 0, errors.New("chunk written already")
	}
	n := len(p)
	for len(p) > 0 {
		last := len(w.blocks) - 1
		if last < 0 || len(w.blocks[last]) == cap(w.blocks[last]) {
			c := firstBlock
			if last >= 0 {
				c = min(2*cap(w.blocks[last]), blockSize)
			}
			w.blocks = append(w.blocks, make([]byte, 0, c))
			last++
		}
		b := w.blocks[last]
		k := copy(b[len(b):cap(b)], p)
		w.blocks[last] = b[:len(b)+k]
		p = p[k:]
	}
	w.size += int64(n)
	w.m.grew(int64(n))
	return n, nil
}

func (w *memWriter) Commit(meta []byte) error {
	if w.done {
		return errors.New("chunk written already")
	}
	w.done = true
	m := w.m
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writing -= w.size
	if old := m.jobs[w.id][w.seq]; old != nil {
		m.staged -= old.size
	}
	if m.jobs[w.id] == nil {
		m.jobs[w.id] = map[int]*memChunk{}
	}
	m.jobs[w.id][w.seq] = &memChunk{data: w.blocks, size: w.size, meta: append([]byte(nil), meta...)}
	m.staged += w.size
	m.peak = max(m.peak, m.staged+m.writing)
	return nil
}

func (w *memWriter) Abort() {
	if w.done {
		return
	}
	w.done = true
	w.m.grew(-w.size)
	w.blocks = nil
}
