// Package bundle turns a stream of filesystem entries into a sequence of
// self-contained chunks and back. Every chunk is complete in itself, in a
// compact format of fkbr's own (native.go; optionally zstd-compressed), so
// chunks can be verified, retried and consumed one at a time and deleted
// once every target has them. A regular file larger than the space left in
// a chunk is split into segments carried in consecutive chunks; each
// segment records its offset and the file's total size.
//
// The package knows nothing about NFS: chunks go to a Sink.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"strconv"

	"github.com/klauspost/compress/zstd"

	"fkbr/internal/api"
)

// ChunkMeta is published alongside each chunk once it is complete.
type ChunkMeta struct {
	Seq        int    `json:"seq"`
	Size       int64  `json:"size"` // bytes as stored
	SHA256     string `json:"sha256"`
	Compressed bool   `json:"compressed"` // zstd
	Entries    int    `json:"entries"`
	// First identifies the first entry ("name@offset"); a resumed sender
	// compares it to detect that the source changed under it.
	First string `json:"first"`
	// Bad lists files whose content changed or failed while being read.
	// Their data in the bundle is unusable and receivers must drop them.
	Bad []string `json:"bad,omitempty"`
	// Raw is the chunk's size before compression (for logs; optional).
	Raw int64 `json:"raw,omitempty"`
}

// Sink stores chunks. Create is called when a chunk starts; the returned
// writer is closed before Commit publishes the chunk with its meta.
type Sink interface {
	Create(seq int) (io.WriteCloser, error)
	Commit(meta ChunkMeta) error
}

// File is what a Writer reads regular-file content from (*os.File fits).
type File interface {
	io.Reader
	io.Closer
	Stat() (fs.FileInfo, error)
}

// Item is one entry to bundle. Name is its path inside the bundle (relative
// to the destination directory). Open is only used for regular files.
type Item struct {
	Name  string
	Entry api.Entry
	Open  func() (File, error)
}

// Writer produces chunks of approximately ChunkSize uncompressed bytes.
// The first chunks are smaller (rampStart, doubling up to ChunkSize), so
// targets can start consuming within moments instead of waiting for a
// whole large chunk; the size depends only on the chunk number, so an
// interrupted sender regenerates the same chunks.
type Writer struct {
	sink      Sink
	chunkSize int64
	compress  bool
	zw        *zstd.Encoder // reused for every chunk
	seq       int
	cur       *chunk
	// Files and Bytes count entries and file content bytes added.
	Files, Bytes int64
}

func NewWriter(sink Sink, chunkSize int64, compress bool) *Writer {
	return &Writer{sink: sink, chunkSize: chunkSize, compress: compress}
}

// rampStart is the size of the first chunk; later ones double up to the
// configured chunk size.
const rampStart = 4 << 20

// limit is the size of the chunk being written.
func (w *Writer) limit() int64 {
	if w.seq >= 30 {
		return w.chunkSize
	}
	return min(w.chunkSize, int64(rampStart)<<w.seq)
}

type chunk struct {
	out  io.WriteCloser
	hash hash.Hash
	size countWriter // stored bytes
	zw   *zstd.Encoder
	ew   *nativeEntries
	raw  *countWriter // uncompressed bytes
	meta ChunkMeta
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func (w *Writer) open() error {
	if w.cur != nil {
		return nil
	}
	out, err := w.sink.Create(w.seq)
	if err != nil {
		return err
	}
	c := &chunk{out: out, hash: sha256.New(), meta: ChunkMeta{Seq: w.seq, Compressed: w.compress}}
	c.size.w = io.MultiWriter(out, c.hash)
	var dst io.Writer = &c.size
	if w.compress {
		if w.zw == nil {
			if w.zw, err = NewEncoder(dst); err != nil {
				out.Close()
				return err
			}
		} else {
			w.zw.Reset(dst)
		}
		c.zw = w.zw
		dst = c.zw
	}
	c.raw = &countWriter{w: dst}
	if c.ew, err = newNativeEntries(c.raw); err != nil {
		out.Close()
		return err
	}
	w.cur = c
	return nil
}

func (w *Writer) closeChunk() error {
	c := w.cur
	if c == nil {
		return nil
	}
	w.cur = nil
	err := c.ew.close()
	if c.zw != nil && err == nil {
		err = c.zw.Close()
	}
	if cerr := c.out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	c.meta.Size = c.size.n
	c.meta.Raw = c.raw.n
	c.meta.SHA256 = hex.EncodeToString(c.hash.Sum(nil))
	w.seq++
	return w.sink.Commit(c.meta)
}

func (w *Writer) note(name string, off int64) {
	if w.cur.meta.Entries == 0 {
		w.cur.meta.First = name + "@" + strconv.FormatInt(off, 10)
	}
	w.cur.meta.Entries++
}

// ErrUnsupported is returned by Add for entry types that cannot be bundled
// (devices, sockets, fifos). The bundle stays valid.
var ErrUnsupported = errors.New("unsupported file type")

// Add appends one entry. Errors wrapping ErrUnsupported, or an *ItemError,
// concern only this item and the bundle remains usable; any other error is
// fatal for the writer.
func (w *Writer) Add(it Item) error {
	if len(it.Name) > MaxNameLen {
		return &ItemError{it.Name[:64] + "…", fmt.Errorf("path of %d bytes is longer than %d: not copied", len(it.Name), MaxNameLen)}
	}
	if len(it.Entry.LinkTarget) > maxLinkLen {
		return &ItemError{it.Name, fmt.Errorf("symlink target of %d bytes is longer than %d: not copied", len(it.Entry.LinkTarget), maxLinkLen)}
	}
	switch it.Entry.Type {
	case api.TypeDir, api.TypeSymlink:
		if err := w.open(); err != nil {
			return err
		}
		w.note(it.Name, 0)
		if err := w.cur.ew.header(it.Name, it.Entry, 0, 0); err != nil {
			return err
		}
		w.Files++
		return w.maybeRoll()
	case api.TypeFile:
		return w.addFile(it)
	default:
		return fmt.Errorf("%s: %w", it.Name, ErrUnsupported)
	}
}

// ItemError reports that one file could not be read correctly. It has been
// recorded as Bad in the chunk meta.
type ItemError struct {
	Name string
	Err  error
}

func (e *ItemError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *ItemError) Unwrap() error { return e.Err }

func (w *Writer) addFile(it Item) error {
	f, err := it.Open()
	if err != nil {
		return &ItemError{it.Name, err} // nothing written yet: just skip it
	}
	defer f.Close()
	total := it.Entry.Size
	var itemErr error
	for off := int64(0); ; {
		if err := w.open(); err != nil {
			return err
		}
		seg := total - off
		limit := w.limit()
		remain := limit - w.cur.raw.n
		if seg > remain {
			// Split only if a worthwhile piece fits; otherwise start fresh.
			if remain < limit/16 && w.cur.meta.Entries > 0 {
				if err := w.closeChunk(); err != nil {
					return err
				}
				continue
			}
			seg = max(remain, 0)
			if seg == 0 {
				seg = min(total-off, limit)
			}
		}
		w.note(it.Name, off)
		if err := w.cur.ew.header(it.Name, it.Entry, off, seg); err != nil {
			return err
		}
		n, err := io.CopyN(w.cur.ew, f, seg)
		if err != nil {
			// Shrunk or unreadable: pad so the chunk stays valid, mark bad.
			if _, perr := io.CopyN(w.cur.ew, zeroReader{}, seg-n); perr != nil {
				return perr
			}
			if itemErr == nil {
				itemErr = &ItemError{it.Name, fmt.Errorf("read: %w", err)}
				w.cur.meta.Bad = append(w.cur.meta.Bad, it.Name)
			}
		}
		w.Bytes += seg
		off += seg
		if off >= total {
			break
		}
		if err := w.closeChunk(); err != nil { // chunk is full
			return err
		}
	}
	if itemErr == nil {
		if fi, err := f.Stat(); err != nil || fi.Size() != total || fi.ModTime().UnixNano() != it.Entry.MtimeNs {
			itemErr = &ItemError{it.Name, errors.New("changed while being read")}
			w.cur.meta.Bad = append(w.cur.meta.Bad, it.Name)
		}
	}
	w.Files++
	if err := w.maybeRoll(); err != nil {
		return err
	}
	return itemErr
}

func (w *Writer) maybeRoll() error {
	if w.cur != nil && w.cur.raw.n >= w.limit() {
		return w.closeChunk()
	}
	return nil
}

// Close publishes the last chunk and returns the number of chunks.
func (w *Writer) Close() (int, error) {
	if err := w.closeChunk(); err != nil {
		return 0, err
	}
	return w.seq, nil
}

// Abort drops the chunk in progress without publishing it.
func (w *Writer) Abort() {
	if w.cur != nil {
		if w.cur.zw != nil {
			w.cur.zw.Close()
		}
		w.cur.out.Close()
		w.cur = nil
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// Header is one entry read from a chunk. For file segments, Offset and
// Length locate the data in a file of Entry.Size bytes.
type Header struct {
	Name   string
	Entry  api.Entry
	Offset int64
	Length int64
}

// Last reports whether this header completes its file.
func (h *Header) Last() bool { return h.Offset+h.Length == h.Entry.Size }

// Read iterates over the entries of one chunk. Names are validated: no
// absolute paths and nothing climbing above the bundle root.
func Read(r io.Reader, compressed bool, fn func(h *Header, data io.Reader) error) error {
	if compressed {
		zr, err := NewDecoder(r)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}
	return readNative(r, fn)
}
