// Package bundle turns a stream of filesystem entries into a sequence of
// self-contained chunks and back. Every chunk is a complete tar archive
// (optionally zstd-compressed), so chunks can be verified, retried and consumed one
// at a time and deleted once every target has them. A regular file larger
// than the space left in a chunk is split into segments carried in
// consecutive chunks; PAX records give each segment's offset and the
// file's total size.
//
// The package knows nothing about NFS: chunks go to a Sink.
package bundle

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
)

// PAX record names for file segments, e.g. "FKBR.off".
var (
	paxOffset = strings.ToUpper(product.Name) + ".off"
	paxTotal  = strings.ToUpper(product.Name) + ".total"
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

type chunk struct {
	out  io.WriteCloser
	hash hash.Hash
	size countWriter // stored bytes
	zw   *zstd.Encoder
	tw   *tar.Writer
	raw  *countWriter // uncompressed tar bytes
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
	c.tw = tar.NewWriter(c.raw)
	w.cur = c
	return nil
}

func (w *Writer) closeChunk() error {
	c := w.cur
	if c == nil {
		return nil
	}
	w.cur = nil
	err := c.tw.Close()
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

func (w *Writer) header(name string, e api.Entry, size int64) *tar.Header {
	h := &tar.Header{
		Name:    name,
		Mode:    int64(e.Mode),
		Uid:     int(e.UID),
		Gid:     int(e.GID),
		ModTime: time.Unix(0, e.MtimeNs),
		Size:    size,
		Format:  tar.FormatPAX,
	}
	switch e.Type {
	case api.TypeDir:
		h.Typeflag = tar.TypeDir
	case api.TypeSymlink:
		h.Typeflag = tar.TypeSymlink
		h.Linkname = e.LinkTarget
	default:
		h.Typeflag = tar.TypeReg
	}
	return h
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
	switch it.Entry.Type {
	case api.TypeDir, api.TypeSymlink:
		if err := w.open(); err != nil {
			return err
		}
		w.note(it.Name, 0)
		if err := w.cur.tw.WriteHeader(w.header(it.Name, it.Entry, 0)); err != nil {
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
		remain := w.chunkSize - w.cur.raw.n
		if seg > remain {
			// Split only if a worthwhile piece fits; otherwise start fresh.
			if remain < w.chunkSize/16 && w.cur.meta.Entries > 0 {
				if err := w.closeChunk(); err != nil {
					return err
				}
				continue
			}
			seg = max(remain, 0)
			if seg == 0 {
				seg = min(total-off, w.chunkSize)
			}
		}
		h := w.header(it.Name, it.Entry, seg)
		if seg != total {
			h.PAXRecords = map[string]string{
				paxOffset: strconv.FormatInt(off, 10),
				paxTotal:  strconv.FormatInt(total, 10),
			}
		}
		w.note(it.Name, off)
		if err := w.cur.tw.WriteHeader(h); err != nil {
			return err
		}
		n, err := io.CopyN(w.cur.tw, f, seg)
		if err != nil {
			// Shrunk or unreadable: pad so the tar stays valid, mark bad.
			if _, perr := io.CopyN(w.cur.tw, zeroReader{}, seg-n); perr != nil {
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
	if w.cur != nil && w.cur.raw.n >= w.chunkSize {
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
	tr := tar.NewReader(r)
	for {
		th, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		h, err := fromTar(th)
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

func fromTar(th *tar.Header) (*Header, error) {
	name, err := localfs.CleanRel(th.Name)
	if err != nil {
		return nil, err
	}
	h := &Header{
		Name: name,
		Entry: api.Entry{
			Name:    name,
			Mode:    uint32(th.Mode) & 0o7777,
			UID:     uint32(th.Uid),
			GID:     uint32(th.Gid),
			MtimeNs: th.ModTime.UnixNano(),
		},
	}
	switch th.Typeflag {
	case tar.TypeDir:
		h.Entry.Type = api.TypeDir
	case tar.TypeSymlink:
		h.Entry.Type = api.TypeSymlink
		h.Entry.LinkTarget = th.Linkname
	case tar.TypeReg:
		h.Entry.Type = api.TypeFile
		h.Entry.Size, h.Length = th.Size, th.Size
		if v, ok := th.PAXRecords[paxTotal]; ok {
			if h.Entry.Size, err = strconv.ParseInt(v, 10, 64); err != nil {
				return nil, fmt.Errorf("%s: bad %s", name, paxTotal)
			}
			if h.Offset, err = strconv.ParseInt(th.PAXRecords[paxOffset], 10, 64); err != nil {
				return nil, fmt.Errorf("%s: bad %s", name, paxOffset)
			}
		}
		if h.Offset < 0 || h.Length < 0 || h.Offset+h.Length > h.Entry.Size {
			return nil, fmt.Errorf("%s: segment out of range", name)
		}
	default:
		return nil, fmt.Errorf("%s: unsupported tar entry type %q", name, th.Typeflag)
	}
	if name == "." && h.Entry.Type != api.TypeDir {
		return nil, fmt.Errorf("bundle root must be a directory")
	}
	return h, nil
}
