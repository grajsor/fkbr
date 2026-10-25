package bundle

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/zeebo/xxh3"

	"fkbr/internal/api"
)

type memSink struct {
	chunks [][]byte
	metas  []ChunkMeta
	cur    *bytes.Buffer
}

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }

func (s *memSink) Create(seq int) (io.WriteCloser, error) {
	s.cur = &bytes.Buffer{}
	return nopCloser{s.cur}, nil
}

func (s *memSink) Commit(m ChunkMeta) error {
	s.chunks = append(s.chunks, s.cur.Bytes())
	s.metas = append(s.metas, m)
	return nil
}

type memFile struct {
	*bytes.Reader
	size  int64
	mtime time.Time
}

func (f memFile) Close() error               { return nil }
func (f memFile) Stat() (fs.FileInfo, error) { return fakeInfo{f.size, f.mtime}, nil }

type fakeInfo struct {
	size  int64
	mtime time.Time
}

func (i fakeInfo) Name() string       { return "f" }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return i.mtime }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }

var mtime = time.Unix(1700000000, 123456789)

func fileItem(name string, data []byte) Item {
	return Item{
		Name:  name,
		Entry: api.Entry{Type: api.TypeFile, Size: int64(len(data)), Mode: 0o640, MtimeNs: mtime.UnixNano(), UID: 7, GID: 8},
		Open: func() (File, error) {
			return memFile{bytes.NewReader(data), int64(len(data)), mtime}, nil
		},
	}
}

func readAll(t *testing.T, s *memSink) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for i, c := range s.chunks {
		sum := xxh3.Hash128(c).Bytes()
		if hex.EncodeToString(sum[:]) != s.metas[i].Sum || int64(len(c)) != s.metas[i].Size {
			t.Fatalf("chunk %d: meta mismatch", i)
		}
		err := Read(bytes.NewReader(c), s.metas[i].Compressed, func(h *Header, r io.Reader) error {
			if h.Entry.Type != api.TypeFile {
				files[h.Name+"/"] = nil
				return nil
			}
			b, _ := io.ReadAll(r)
			buf := files[h.Name]
			if int64(len(buf)) > h.Offset {
				t.Fatalf("%s: segment at %d, have %d bytes", h.Name, h.Offset, len(buf))
			}
			buf = append(buf, make([]byte, h.Offset-int64(len(buf)))...) // zeros left out
			buf = append(buf, b...)
			if h.Last() {
				buf = buf[:h.Entry.Size]
			}
			files[h.Name] = buf
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestRoundTripSplitting(t *testing.T) {
	for _, compress := range []bool{false, true} {
		s := &memSink{}
		w := NewWriter(s, 64<<10, compress)
		big := bytes.Repeat([]byte("0123456789abcdef"), 20000) // 320 KiB: spans ~5 chunks
		must(t, w.Add(Item{Name: "dir", Entry: api.Entry{Type: api.TypeDir, Mode: 0o755}}))
		must(t, w.Add(fileItem("dir/small", []byte("hi"))))
		must(t, w.Add(fileItem("dir/big", big)))
		must(t, w.Add(fileItem("dir/empty", nil)))
		must(t, w.Add(Item{Name: "dir/link", Entry: api.Entry{Type: api.TypeSymlink, LinkTarget: "small"}}))
		n, err := w.Close()
		must(t, err)
		if n < 5 || n != len(s.chunks) {
			t.Fatalf("compress=%v: %d chunks (sink has %d)", compress, n, len(s.chunks))
		}
		files := readAll(t, s)
		if !bytes.Equal(files["dir/big"], big) || string(files["dir/small"]) != "hi" {
			t.Fatalf("compress=%v: content mismatch", compress)
		}
		if _, ok := files["dir/empty"]; !ok {
			t.Fatal("empty file missing")
		}
		if s.metas[0].First != "dir@0" || !strings.HasPrefix(s.metas[1].First, "dir/big@") {
			t.Fatalf("First: %q, %q", s.metas[0].First, s.metas[1].First)
		}
		if w.Files != 5 || w.Bytes != int64(len(big)+2) {
			t.Fatalf("stats: files=%d bytes=%d", w.Files, w.Bytes)
		}
	}
}

func TestCorruptCompressedChunk(t *testing.T) {
	s := &memSink{}
	w := NewWriter(s, 1<<20, true)
	must(t, w.Add(fileItem("f", bytes.Repeat([]byte("data"), 1000))))
	_, err := w.Close()
	must(t, err)
	bad := bytes.Clone(s.chunks[0])
	bad[len(bad)/2] ^= 0xff
	if err := Read(bytes.NewReader(bad), true, func(*Header, io.Reader) error { return nil }); err == nil {
		t.Fatal("corrupt chunk read without error")
	}
	if err := Read(bytes.NewReader([]byte("not zstd")), true, func(*Header, io.Reader) error { return nil }); err == nil {
		t.Fatal("garbage read without error")
	}
	// The pooled decoders still work afterwards.
	if files := readAll(t, s); len(files["f"]) != 4000 {
		t.Fatalf("got %d bytes", len(files["f"]))
	}
}

func TestChangedFileMarkedBad(t *testing.T) {
	s := &memSink{}
	w := NewWriter(s, 1<<20, false)
	it := fileItem("f", []byte("abc"))
	it.Entry.Size = 10 // file "shrank" since it was listed
	var ie *ItemError
	if err := w.Add(it); !errors.As(err, &ie) {
		t.Fatalf("want ItemError, got %v", err)
	}
	it = fileItem("g", []byte("abc"))
	it.Entry.MtimeNs++ // modified since listed
	if err := w.Add(it); !errors.As(err, &ie) {
		t.Fatalf("want ItemError, got %v", err)
	}
	must(t, w.Add(fileItem("ok", []byte("fine"))))
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.metas[0].Bad, ",") != "f,g" {
		t.Fatalf("Bad = %v", s.metas[0].Bad)
	}
	if string(readAll(t, s)["ok"]) != "fine" {
		t.Fatal("bundle unusable after bad item")
	}
}

func TestUnsupportedType(t *testing.T) {
	w := NewWriter(&memSink{}, 1<<20, false)
	if err := w.Add(Item{Name: "fifo", Entry: api.Entry{Type: api.TypeOther}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The first chunks are small and double up to the chunk size, so targets
// start early; content still round-trips.
func TestChunkSizesRamp(t *testing.T) {
	s := &memSink{}
	w := NewWriter(s, 32<<20, false)
	data := bytes.Repeat([]byte("x"), 100<<10) // not zeros: those are left out
	want := map[string][]byte{}
	for i := 0; i < 900; i++ { // ~88 MiB
		name := fmt.Sprintf("f%03d", i)
		data[0] = byte(i)
		b := append([]byte(nil), data...)
		want[name] = b
		must(t, w.Add(fileItem(name, b)))
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	limits := []int64{4 << 20, 8 << 20, 16 << 20, 32 << 20, 32 << 20}
	for i, m := range s.metas {
		lim := limits[min(i, len(limits)-1)]
		if m.Raw > lim+(200<<10) || (i < len(s.metas)-1 && m.Raw < lim-(200<<10)) {
			t.Errorf("chunk %d: %d bytes, want about %d", i, m.Raw, lim)
		}
	}
	got := readAll(t, s)
	for name, b := range want {
		if !bytes.Equal(got[name], b) {
			t.Fatalf("%s differs", name)
		}
	}
}

// sparseItem is a file of size bytes whose data is only in exts (zeros
// elsewhere); its reader yields just those bytes, as the sender's does.
func sparseItem(name string, size int64, exts []Extent) (Item, []byte) {
	full := make([]byte, size)
	var data []byte
	for i, e := range exts {
		for j := e.Off; j < e.Off+e.Len; j++ {
			full[j] = byte('a' + i)
		}
		data = append(data, full[e.Off:e.Off+e.Len]...)
	}
	return Item{
		Name:  name,
		Entry: api.Entry{Type: api.TypeFile, Size: size, Mode: 0o644, MtimeNs: mtime.UnixNano()},
		Open: func() (File, error) {
			return sparseFile{memFile{bytes.NewReader(data), size, mtime}, exts}, nil
		},
	}, full
}

type sparseFile struct {
	memFile
	exts []Extent
}

func (f sparseFile) Extents() []Extent { return f.exts }

// Holes are not sent: a sparse file's chunks carry only its data, in
// segments at their offsets, with a first segment at 0 and a last one
// that ends at the file's size (where a receiver sets size and metadata).
func TestSparseExtents(t *testing.T) {
	s := &memSink{}
	w := NewWriter(s, 64<<10, false)
	a, afull := sparseItem("holes", 4<<20, []Extent{{100 << 10, 150 << 10}, {3 << 20, 10 << 10}})
	b, bfull := sparseItem("allhole", 1<<30, nil)
	c, cfull := sparseItem("dataend", 1<<20, []Extent{{1<<20 - 4096, 4096}})
	for _, it := range []Item{a, b, c} {
		must(t, w.Add(it))
	}
	_, err := w.Close()
	must(t, err)
	got := map[string][]byte{}
	var segs []string
	var stored int64
	for i, ch := range s.chunks {
		stored += int64(len(ch))
		must(t, Read(bytes.NewReader(ch), s.metas[i].Compressed, func(h *Header, r io.Reader) error {
			buf, ok := got[h.Name]
			if !ok {
				if h.Offset != 0 {
					t.Errorf("%s: first segment at %d", h.Name, h.Offset)
				}
				buf = make([]byte, h.Entry.Size)
				got[h.Name] = buf
			}
			if _, err := io.ReadFull(r, buf[h.Offset:h.Offset+h.Length]); err != nil {
				return err
			}
			if h.Last() {
				segs = append(segs, h.Name)
			}
			return nil
		}))
	}
	for name, want := range map[string][]byte{"holes": afull, "allhole": bfull, "dataend": cfull} {
		if !bytes.Equal(got[name], want) {
			t.Errorf("%s: content differs", name)
		}
	}
	if strings.Join(segs, ",") != "holes,allhole,dataend" {
		t.Errorf("last segments: %v", segs)
	}
	// Data goes in whole blocks of zeroBlock: 3 + 1 + 1 of them here.
	if stored > 5*zeroBlock+16<<10 {
		t.Errorf("%d bytes in chunks for 164 KiB of data", stored)
	}
	if want := int64(4<<20 + 1<<30 + 1<<20); w.Bytes != want {
		t.Errorf("Bytes %d, want %d (holes count as done)", w.Bytes, want)
	}
}

// What is sent depends on the content only: the same file gives the same
// chunks whether the disk reports its holes, reports zeros as data (a
// layout changed by writeback, preallocation or a defrag), or nothing.
func TestSparseOutputFollowsContent(t *testing.T) {
	const size = 9 << 20
	full := make([]byte, size)
	for i := 3 << 20; i < 3<<20+300<<10; i++ { // data in the middle
		full[i] = byte(i)
	}
	full[size-1] = 1 // and at the very end
	layouts := map[string][]Extent{
		"exact":    {{3 << 20, 300 << 10}, {size - 1, 1}},
		"coarse":   {{2 << 20, 4 << 20}, {size - 64<<10, 64 << 10}}, // zeros reported as data
		"dense":    nil,                                             // no Sparse at all
		"one hole": {{2 << 20, size - 2<<20}},
	}
	var first []byte
	for name, exts := range layouts {
		s := &memSink{}
		w := NewWriter(s, 4<<20, false)
		it := Item{Name: "img", Entry: api.Entry{Type: api.TypeFile, Size: size, Mode: 0o644, MtimeNs: mtime.UnixNano()}}
		it.Open = func() (File, error) {
			if exts == nil {
				return memFile{bytes.NewReader(full), size, mtime}, nil
			}
			var data []byte
			for _, e := range exts {
				data = append(data, full[e.Off:e.Off+e.Len]...)
			}
			return sparseFile{memFile{bytes.NewReader(data), size, mtime}, exts}, nil
		}
		must(t, w.Add(it))
		_, err := w.Close()
		must(t, err)
		all := bytes.Join(s.chunks, nil)
		if got := readAll(t, s)["img"]; !bytes.Equal(got, full) {
			t.Fatalf("%s: content differs", name)
		}
		if first == nil {
			first = all
		} else if !bytes.Equal(all, first) {
			t.Errorf("%s: other chunks than another layout of the same content", name)
		}
		if len(all) > 600<<10 {
			t.Errorf("%s: %d bytes sent for 300 KiB of data", name, len(all))
		}
	}
}

// A chunk from the share is bounded, whoever wrote it: what it unpacks
// to, its entries, and the size a file in it may claim (review
// 2026-10-25: a small zstd frame or a header claiming exabytes could fill
// a target from almost nothing).
func TestChunkBudgets(t *testing.T) {
	oldBytes, oldEntries := maxChunkBytes, maxEntries
	t.Cleanup(func() { maxChunkBytes, maxEntries = oldBytes, oldEntries })
	maxChunkBytes, maxEntries = 1<<20, 100

	// 8 MiB of one file, compressed to almost nothing.
	s := &memSink{}
	w := NewWriter(s, 64<<20, true)
	big := bytes.Repeat([]byte("ab"), 4<<20)
	must(t, w.Add(fileItem("bomb", big)))
	_, err := w.Close()
	must(t, err)
	if len(s.chunks[0]) > 64<<10 {
		t.Fatalf("not a bomb: %d bytes", len(s.chunks[0]))
	}
	err = Read(bytes.NewReader(s.chunks[0]), true, func(h *Header, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Errorf("a chunk unpacking past the limit: %v", err)
	}

	// More entries than allowed: the writer starts a new chunk before,
	// a chunk with more is refused.
	s = &memSink{}
	w = NewWriter(s, 64<<20, false)
	for i := range 150 {
		must(t, w.Add(fileItem(fmt.Sprintf("f%03d", i), nil)))
	}
	_, err = w.Close()
	must(t, err)
	if len(s.chunks) < 2 {
		t.Errorf("150 empty files in %d chunk", len(s.chunks))
	}
	if files := readAll(t, s); len(files) != 150 {
		t.Errorf("%d files read back", len(files))
	}
	maxEntries = 1000
	s = &memSink{}
	w = NewWriter(s, 64<<20, false)
	for i := range 150 {
		must(t, w.Add(fileItem(fmt.Sprintf("f%03d", i), nil)))
	}
	w.Close()
	maxEntries = 100
	err = Read(bytes.NewReader(s.chunks[0]), false, func(*Header, io.Reader) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
		t.Errorf("150 entries: %v", err)
	}

	// A file claiming more than any disk holds.
	var buf bytes.Buffer
	ne, _ := newNativeEntries(&buf)
	ne.header("huge", api.Entry{Type: api.TypeFile, Size: 1 << 50}, 1<<50, 0)
	ne.close()
	err = Read(bytes.NewReader(buf.Bytes()), false, func(*Header, io.Reader) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("a file of 1 PiB: %v", err)
	}
}
