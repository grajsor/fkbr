package bundle

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"fkbr/internal/api"
)

// The format's point: tiny files cost little more than their data (tar
// needed 3.6 MB for these 2 MB).
func TestChunksAreCompact(t *testing.T) {
	s := &memSink{}
	w := NewWriter(s, 64<<20, false)
	for i := 0; i < 1000; i++ {
		must(t, w.Add(fileItem(fmt.Sprintf("dir%02d/file%04d", i/100, i), make([]byte, 2000))))
	}
	_, err := w.Close()
	must(t, err)
	var n int64
	for _, m := range s.metas {
		n += m.Size
	}
	if n > 2000*1000*102/100 {
		t.Errorf("%d bytes for 2,000,000 bytes of data", n)
	}
}

// nativeChunk builds a native chunk by hand.
func nativeChunk(parts ...[]byte) []byte {
	b := []byte(nativeMagic)
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// entry encodes one entry like nativeEntries does, with free choice of
// every field (including invalid ones).
func entry(kind byte, shared int, suffix string, mode, uid, gid uint64, extra ...uint64) []byte {
	b := []byte{kind}
	b = binary.AppendUvarint(b, uint64(shared))
	b = binary.AppendUvarint(b, uint64(len(suffix)))
	b = append(b, suffix...)
	b = binary.AppendUvarint(b, mode)
	b = binary.AppendUvarint(b, uid)
	b = binary.AppendUvarint(b, gid)
	b = binary.AppendVarint(b, 1700000000000000000)
	for _, x := range extra {
		b = binary.AppendUvarint(b, x)
	}
	return b
}

func readChunk(b []byte) error {
	return Read(bytes.NewReader(b), false, func(h *Header, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
}

func TestNativeRejectsHostileChunks(t *testing.T) {
	end := []byte{kindEnd}
	ok := nativeChunk(entry(kindDir, 0, "d", 0o755, 0, 0), entry(kindFile, 0, "d/f", 0o644, 0, 0, 3, 0, 3), []byte("abc"), end)
	if err := readChunk(ok); err != nil {
		t.Fatalf("valid chunk: %v", err)
	}
	cases := map[string][]byte{
		"climbs out":          nativeChunk(entry(kindFile, 0, "../evil", 0o644, 0, 0, 0, 0, 0), end),
		"absolute":            nativeChunk(entry(kindFile, 0, "/etc/passwd", 0o644, 0, 0, 0, 0, 0), end),
		"climbs via prefix":   nativeChunk(entry(kindDir, 0, "a/..", 0o755, 0, 0), entry(kindFile, 4, "/../x", 0o644, 0, 0, 0, 0, 0), end),
		"prefix too long":     nativeChunk(entry(kindDir, 0, "d", 0o755, 0, 0), entry(kindFile, 9, "x", 0o644, 0, 0, 0, 0, 0), end),
		"NUL in name":         nativeChunk(entry(kindFile, 0, "a\x00b", 0o644, 0, 0, 0, 0, 0), end),
		"empty name":          nativeChunk(entry(kindFile, 0, "", 0o644, 0, 0, 0, 0, 0), end),
		"name too long":       nativeChunk(entry(kindFile, 0, strings.Repeat("x", MaxNameLen+1), 0o644, 0, 0, 0, 0, 0), end),
		"mode out of range":   nativeChunk(entry(kindFile, 0, "f", 0o170777, 0, 0, 0, 0, 0), end),
		"uid out of range":    nativeChunk(entry(kindFile, 0, "f", 0o644, 1<<33, 0, 0, 0, 0), end),
		"offset past end":     nativeChunk(entry(kindFile, 0, "f", 0o644, 0, 0, 10, 11, 0), end),
		"segment past end":    nativeChunk(entry(kindFile, 0, "f", 0o644, 0, 0, 10, 5, 6), end),
		"size too large":      nativeChunk(entry(kindFile, 0, "f", 0o644, 0, 0, 1<<63, 0, 0), end),
		"data truncated":      nativeChunk(entry(kindFile, 0, "f", 0o644, 0, 0, 10, 0, 10), []byte("abc")),
		"no end byte":         nativeChunk(entry(kindDir, 0, "d", 0o755, 0, 0)),
		"data after end":      nativeChunk(entry(kindDir, 0, "d", 0o755, 0, 0), end, []byte("x")),
		"unknown kind":        nativeChunk([]byte{'Z'}, end),
		"root not a dir":      nativeChunk(entry(kindFile, 0, ".", 0o644, 0, 0, 0, 0, 0), end),
		"empty link":          nativeChunk(entry(kindSymlink, 0, "l", 0o777, 0, 0, 0), end),
		"link too long":       nativeChunk(entry(kindSymlink, 0, "l", 0o777, 0, 0, maxLinkLen+1), end),
		"wrong magic":         append([]byte("FKB3"), end...),
		"varint overflow":     nativeChunk([]byte{kindFile, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, end),
		"truncated in a name": nativeChunk([]byte{kindFile, 0, 10, 'a'}),
	}
	for name, b := range cases {
		if err := readChunk(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	err := readChunk(cases["climbs out"])
	if !errors.Is(err, api.ErrBadPath) {
		t.Errorf("escape: want ErrBadPath, got %v", err)
	}
}

// Whatever bytes arrive, the reader returns (an error or not) without
// panicking or hanging, and never hands out a name that escapes.
func FuzzReadNative(f *testing.F) {
	s := &memSink{}
	w := NewWriter(s, 1<<20, false)
	must(f, w.Add(Item{Name: "dir", Entry: api.Entry{Type: api.TypeDir, Mode: 0o755}}))
	must(f, w.Add(fileItem("dir/a", []byte("hello"))))
	must(f, w.Add(Item{Name: "dir/l", Entry: api.Entry{Type: api.TypeSymlink, LinkTarget: "a"}}))
	_, err := w.Close()
	must(f, err)
	zs := &memSink{}
	zw := NewWriter(zs, 1<<20, true)
	must(f, zw.Add(fileItem("dir/a", bytes.Repeat([]byte("hello"), 100))))
	_, err = zw.Close()
	must(f, err)
	f.Add(s.chunks[0], false)
	f.Add([]byte(nativeMagic), false)
	// The compressed path too: a real frame, a cut one, and garbage.
	f.Add(zs.chunks[0], true)
	f.Add(zs.chunks[0][:len(zs.chunks[0])/2], true)
	f.Add([]byte("not zstd"), true)
	f.Fuzz(func(t *testing.T, b []byte, compressed bool) {
		Read(bytes.NewReader(b), compressed, func(h *Header, r io.Reader) error {
			if strings.HasPrefix(h.Name, "/") || h.Name == ".." || strings.HasPrefix(h.Name, "../") || strings.Contains(h.Name, "/../") {
				t.Fatalf("escaping name %q", h.Name)
			}
			if h.Offset < 0 || h.Length < 0 || h.Offset+h.Length > h.Entry.Size {
				t.Fatalf("bad segment %+v", h)
			}
			_, err := io.Copy(io.Discard, r)
			return err
		})
	})
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// A read that fails before the magic is that error (a share I/O error is
// retried by the engine), not the verdict "not an fkbr chunk"; a short or
// foreign chunk still is.
func TestMagicReadErrorKept(t *testing.T) {
	want := errors.New("stale file handle")
	err := Read(failingReader{want}, false, func(*Header, io.Reader) error { return nil })
	if !errors.Is(err, want) {
		t.Errorf("got %v, want it to wrap %v", err, want)
	}
	for _, in := range []string{"", "FK", "XXXXXXXX"} {
		err := Read(strings.NewReader(in), false, func(*Header, io.Reader) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "not an fkbr chunk") {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// blipReader serves data, then one error, then end of file.
type blipReader struct {
	data []byte
	err  error
	done bool
}

func (r *blipReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.done {
		r.done = true
		return 0, r.err
	}
	return 0, io.EOF
}

// A share I/O error on the byte after the end of a whole chunk is that
// error (the engine retries it), not the verdict "data after the end"; a
// real extra byte still is.
func TestErrorAfterTheEndKept(t *testing.T) {
	want := syscall.EIO
	chunk := nativeChunk([]byte{kindEnd})
	err := Read(&blipReader{data: chunk, err: want}, false, func(*Header, io.Reader) error { return nil })
	if !errors.Is(err, want) {
		t.Errorf("got %v, want it to wrap %v", err, want)
	}
	err = Read(bytes.NewReader(append(chunk, 'x')), false, func(*Header, io.Reader) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "data after the end") {
		t.Errorf("an extra byte: %v", err)
	}
	if err := Read(bytes.NewReader(chunk), false, func(*Header, io.Reader) error { return nil }); err != nil {
		t.Errorf("a whole chunk: %v", err)
	}
}
