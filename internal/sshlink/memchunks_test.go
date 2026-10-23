package sshlink

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
)

func TestMemChunks(t *testing.T) {
	ctx := context.Background()
	m := newMemChunks("a")
	const id = "18e12c793b6678e8eb4d4709"
	put := func(seq int, data []byte) {
		t.Helper()
		w, err := m.CreateChunk(ctx, id, seq)
		if err != nil {
			t.Fatal(err)
		}
		for p := data; len(p) > 0; p = p[min(len(p), 7000):] { // odd writes across blocks
			w.Write(p[:min(len(p), 7000)])
		}
		if err := w.Commit([]byte(`{"seq":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 300<<10>>4+3) // over several blocks
	put(0, []byte("small"))
	put(1, big)
	if job, total, _ := m.StagedBytes(ctx, id); job != int64(5+len(big)) || total != job {
		t.Errorf("staged %d %d", job, total)
	}
	if meta, err := m.ChunkMeta(ctx, "a", id, 1); err != nil || string(meta) != `{"seq":1}` {
		t.Errorf("meta %q %v", meta, err)
	}
	r, err := m.OpenChunk(ctx, "a", id, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.DeleteChunk(ctx, id, 1) // a reader keeps what it opened
	if got, _ := io.ReadAll(r); !bytes.Equal(got, big) {
		t.Errorf("read back %d bytes, want %d", len(got), len(big))
	}
	if _, err := m.OpenChunk(ctx, "a", id, 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("deleted chunk: %v", err)
	}
	if _, err := m.ChunkMeta(ctx, "b", id, 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("another node's chunk: %v", err)
	}
	if job, total, _ := m.StagedBytes(ctx, id); job != 5 || total != 5 {
		t.Errorf("staged after delete %d %d", job, total)
	}
	// A chunk being written counts in the peak; an aborted one leaves nothing.
	w, _ := m.CreateChunk(ctx, id, 2)
	w.Write(big)
	w.Abort()
	if p := m.Peak(); p < int64(len(big))+5 {
		t.Errorf("peak %d", p)
	}
	if _, err := m.ChunkMeta(ctx, "a", id, 2); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("aborted chunk: %v", err)
	}
	m.RemoveJobData(ctx, id)
	if _, total, _ := m.StagedBytes(ctx, id); total != 0 || m.writing != 0 {
		t.Errorf("after removing the job: staged %d, writing %d", total, m.writing)
	}
	for _, bad := range []string{"", "../x", "a/b", "x y"} {
		if _, err := m.CreateChunk(ctx, bad, 0); err == nil {
			t.Errorf("job id %q accepted", bad)
		}
	}
	if _, err := m.CreateChunk(ctx, id, -1); err == nil {
		t.Error("seq -1 accepted")
	}
}

func TestMemFreeBytes(t *testing.T) {
	n, err := newMemChunks("a").FreeBytes()
	if err != nil || n < 0 {
		t.Errorf("free %d %v", n, err)
	}
}
