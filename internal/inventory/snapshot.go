// Package inventory scans directory trees into snapshots and compares them
// to plan syncs. A snapshot is a zstd-compressed stream of JSON lines in walk
// order (depth-first, names sorted per directory), so a source and any
// number of targets can be merged in one streaming pass whatever their
// size: memory use does not grow with the number of files.
package inventory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
)

const snapshotVersion = 2

// Header is the first line of a snapshot.
type Header struct {
	Version  int    `json:"snapshot"`
	Node     string `json:"node"`
	Root     string `json:"root"`
	Path     string `json:"path"`
	Missing  bool   `json:"missing,omitempty"` // the base path does not exist
	Checksum bool   `json:"checksum,omitempty"`
	// What the scanned filesystem cannot store, so a planner does not send
	// files again for it: mtimes only in steps of MtimeStep seconds (FAT:
	// 2), no permission bits (FAT, SMB, NTFS).
	MtimeStep int64 `json:"mtime_step,omitempty"`
	NoModes   bool  `json:"no_modes,omitempty"`
	// MtimeMin, MtimeMax (ns, 0: none) are where the filesystem clamps
	// mtimes it cannot store (see localfs.MtimeRange); only target scans
	// probe for them.
	MtimeMin int64 `json:"mtime_min,omitempty"`
	MtimeMax int64 `json:"mtime_max,omitempty"`
	// Names: which names it refuses or folds together (FAT, exFAT,
	// casefold); only target scans set it.
	Names *api.NameRules `json:"names,omitempty"`
}

// storable is mtime m as the scanned filesystem would store it.
func (h *Header) storable(m int64) int64 {
	if h.MtimeMax != 0 && m > h.MtimeMax {
		return h.MtimeMax
	}
	if h.MtimeMin != 0 && m < h.MtimeMin {
		return h.MtimeMin
	}
	return m
}

// Entry types in snapshots. TypeError marks a path whose metadata or
// contents could not be read: nothing about it (or below it) is known.
const (
	TypeFile    = "f"
	TypeDir     = "d"
	TypeSymlink = "l"
	TypeOther   = "o"
	TypeError   = "e"
)

// Entry is one line of a snapshot. P is relative to the snapshot base
// ("." is the base itself).
type Entry struct {
	P  string `json:"p"`
	T  string `json:"t"`
	S  int64  `json:"s,omitempty"`
	M  int64  `json:"m,omitempty"` // mtime, ns
	Mo uint32 `json:"mo,omitempty"`
	U  uint32 `json:"u,omitempty"`
	G  uint32 `json:"g,omitempty"`
	L  string `json:"l,omitempty"`
	H  string `json:"h,omitempty"` // sha256, with checksum scans
	E  string `json:"e,omitempty"` // error text for TypeError

	// Unreadable is set by the Reader on a directory whose listing failed
	// (it is followed by a TypeError line for the same path).
	Unreadable bool `json:"-"`
}

func typeOf(t api.EntryType) string {
	switch t {
	case api.TypeFile:
		return TypeFile
	case api.TypeDir:
		return TypeDir
	case api.TypeSymlink:
		return TypeSymlink
	}
	return TypeOther
}

// Writer writes a snapshot.
type Writer struct {
	zw  *zstd.Encoder
	buf *bufio.Writer
	enc *json.Encoder
}

func NewWriter(w io.Writer, h Header) (*Writer, error) {
	zw, err := bundle.NewEncoder(w)
	if err != nil {
		return nil, err
	}
	buf := bufio.NewWriterSize(zw, 256<<10)
	sw := &Writer{zw: zw, buf: buf, enc: json.NewEncoder(buf)}
	h.Version = snapshotVersion
	return sw, sw.enc.Encode(h)
}

func (w *Writer) Write(e *Entry) error {
	// Names that are not valid UTF-8 would come back as other names (see
	// api.EncodeName).
	if p, l := api.EncodeName(e.P), api.EncodeName(e.L); p != e.P || l != e.L {
		c := *e
		c.P, c.L = p, l
		e = &c
	}
	return w.enc.Encode(e)
}

// Close flushes the snapshot; it does not close the underlying writer.
func (w *Writer) Close() error {
	if err := w.buf.Flush(); err != nil {
		return err
	}
	return w.zw.Close()
}

// Reader reads a snapshot, one entry at a time.
type Reader struct {
	Header Header
	zr     *bundle.Decoder
	sc     *bufio.Scanner
	peeked *Entry
	last   string
}

func NewReader(r io.Reader) (*Reader, error) {
	zr, err := bundle.NewDecoder(r)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	rd := &Reader{zr: zr, sc: sc}
	if !sc.Scan() {
		zr.Close()
		return nil, fmt.Errorf("snapshot: missing header: %v", sc.Err())
	}
	if err := json.Unmarshal(sc.Bytes(), &rd.Header); err != nil {
		zr.Close()
		return nil, fmt.Errorf("snapshot header: %w", err)
	}
	if rd.Header.Version != snapshotVersion {
		zr.Close()
		return nil, fmt.Errorf("snapshot version %d not supported", rd.Header.Version)
	}
	return rd, nil
}

// Close releases the decoder; it does not close the underlying reader.
func (r *Reader) Close() { r.zr.Close() }

func (r *Reader) raw() (*Entry, error) {
	if e := r.peeked; e != nil {
		r.peeked = nil
		return e, nil
	}
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	var e Entry
	if err := json.Unmarshal(r.sc.Bytes(), &e); err != nil {
		return nil, fmt.Errorf("snapshot line: %w", err)
	}
	e.P, e.L = api.DecodeName(e.P), api.DecodeName(e.L)
	return &e, nil
}

// Next returns the next entry or io.EOF. A directory followed by an error
// line for itself comes back once, marked Unreadable. Entries must be in
// walk order; anything else is reported as corruption.
func (r *Reader) Next() (*Entry, error) {
	e, err := r.raw()
	if err != nil {
		return nil, err
	}
	if e.T == TypeDir {
		n, err := r.raw()
		switch {
		case err == nil && n.P == e.P && n.T == TypeError:
			e.Unreadable, e.E = true, n.E
		case err == nil:
			r.peeked = n
		case !errors.Is(err, io.EOF):
			return nil, err
		}
	}
	if r.last != "" && ComparePaths(r.last, e.P) >= 0 {
		return nil, fmt.Errorf("snapshot out of order: %q after %q", e.P, r.last)
	}
	r.last = e.P
	return e, nil
}

// ComparePaths orders relative paths the way a sorted depth-first walk
// visits them: "." first, a directory before its contents, and components
// compared bytewise ("a/x" < "a-b", because "a" < "a-b"). That is bytewise
// order with '/' treated as lower than any other byte.
func ComparePaths(a, b string) int {
	if a == b {
		return 0
	}
	if a == "." {
		return -1
	}
	if b == "." {
		return 1
	}
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if ca == '/' {
			return -1
		}
		if cb == '/' {
			return 1
		}
		if ca < cb {
			return -1
		}
		return 1
	}
	return len(a) - len(b)
}

// under reports whether p is inside directory dir (both relative).
func under(p, dir string) bool {
	if dir == "." {
		return p != "."
	}
	return strings.HasPrefix(p, dir+"/")
}

// parent returns the parent of a relative path ("." for top-level).
func parent(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}
