package bundle

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

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
		sum := sha256.Sum256(c)
		if hex.EncodeToString(sum[:]) != s.metas[i].SHA256 || int64(len(c)) != s.metas[i].Size {
			t.Fatalf("chunk %d: meta mismatch", i)
		}
		err := Read(bytes.NewReader(c), s.metas[i].Compressed, func(h *Header, r io.Reader) error {
			if h.Entry.Type != api.TypeFile {
				files[h.Name+"/"] = nil
				return nil
			}
			b, _ := io.ReadAll(r)
			buf := files[h.Name]
			if int64(len(buf)) != h.Offset {
				t.Fatalf("%s: segment at %d, have %d bytes", h.Name, h.Offset, len(buf))
			}
			files[h.Name] = append(buf, b...)
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

func TestReadRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "/etc/passwd", "a/../../b"} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
		tw.Close()
		err := Read(&buf, false, func(*Header, io.Reader) error { return nil })
		if !errors.Is(err, api.ErrBadPath) {
			t.Errorf("%q: want ErrBadPath, got %v", name, err)
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o644})
	tw.Close()
	if err := Read(&buf, false, func(*Header, io.Reader) error { return nil }); err == nil {
		t.Error("device entry must be rejected")
	}
}

func TestUnsupportedType(t *testing.T) {
	w := NewWriter(&memSink{}, 1<<20, false)
	if err := w.Add(Item{Name: "fifo", Entry: api.Entry{Type: api.TypeOther}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
