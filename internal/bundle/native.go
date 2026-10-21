package bundle

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
)

// The chunk format: a 512-byte tar header plus a PAX record for
// nanosecond mtimes, with data padded to 512 bytes, cost about 1.5 KiB per
// entry; this format writes each entry as a few varints and its name,
// sharing the leading bytes with the previous name (entries of one
// directory arrive together), followed by the data unpadded: about 10
// bytes plus the differing part of the name.
//
// Chunk layout (all integers unsigned LEB128 varints except mtime,
// which is zigzag-encoded):
//
//	"FKB2"                                  magic
//	entries, each:
//	  kind byte                             'D' dir, 'L' symlink, 'F' file (segment)
//	  shared, suffixLen, suffix             name = previous[:shared] + suffix
//	  mode, uid, gid, mtimeNs (zigzag)
//	  'L': targetLen, target
//	  'F': size, offset, length, then length bytes of data
//	0 byte                                  end of chunk
//
// The reader trusts nothing: every length is bounded, names are cleaned
// like tar names (no absolute paths, no climbing above the bundle root, no
// NUL), segments must lie inside their file, and a chunk without its end
// byte is an error.
const (
	nativeMagic = "FKB2"
	// MaxNameLen bounds an entry's path inside a chunk. Paths longer than
	// PATH_MAX (4096) are legal (made one directory at a time), so the
	// bound is far above it; Writer.Add refuses longer ones per item.
	MaxNameLen  = 64 << 10
	maxLinkLen  = 4096
	maxFileSize = 1 << 62
	kindEnd     = 0
	kindDir     = 'D'
	kindSymlink = 'L'
	kindFile    = 'F'
)

// nativeEntries writes entries in the native format.
type nativeEntries struct {
	w    io.Writer
	prev string
	buf  []byte
}

func newNativeEntries(w io.Writer) (*nativeEntries, error) {
	if _, err := io.WriteString(w, nativeMagic); err != nil {
		return nil, err
	}
	return &nativeEntries{w: w, buf: make([]byte, 0, 256)}, nil
}

func (n *nativeEntries) header(name string, e api.Entry, off, seg int64) error {
	b := n.buf[:0]
	switch e.Type {
	case api.TypeDir:
		b = append(b, kindDir)
	case api.TypeSymlink:
		b = append(b, kindSymlink)
	default:
		b = append(b, kindFile)
	}
	shared := 0
	for shared < len(name) && shared < len(n.prev) && name[shared] == n.prev[shared] {
		shared++
	}
	b = binary.AppendUvarint(b, uint64(shared))
	b = binary.AppendUvarint(b, uint64(len(name)-shared))
	b = append(b, name[shared:]...)
	b = binary.AppendUvarint(b, uint64(e.Mode&0o7777))
	b = binary.AppendUvarint(b, uint64(e.UID))
	b = binary.AppendUvarint(b, uint64(e.GID))
	b = binary.AppendVarint(b, e.MtimeNs)
	switch e.Type {
	case api.TypeSymlink:
		b = binary.AppendUvarint(b, uint64(len(e.LinkTarget)))
		b = append(b, e.LinkTarget...)
	case api.TypeFile:
		b = binary.AppendUvarint(b, uint64(e.Size))
		b = binary.AppendUvarint(b, uint64(off))
		b = binary.AppendUvarint(b, uint64(seg))
	}
	n.prev, n.buf = name, b
	_, err := n.w.Write(b)
	return err
}

func (n *nativeEntries) Write(p []byte) (int, error) { return n.w.Write(p) }

func (n *nativeEntries) close() error {
	_, err := n.w.Write([]byte{kindEnd})
	return err
}

// readNative iterates over the entries of a native chunk.
func readNative(r io.Reader, fn func(h *Header, data io.Reader) error) error {
	br := bufio.NewReaderSize(r, 64<<10)
	magic := make([]byte, len(nativeMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != nativeMagic {
		return errors.New("not an fkbr chunk")
	}
	prev := ""
	for {
		kind, err := br.ReadByte()
		if err != nil {
			return fmt.Errorf("chunk truncated: %w", unexpected(err))
		}
		if kind == kindEnd {
			if _, err := br.ReadByte(); err != io.EOF {
				return errors.New("data after the end of the chunk")
			}
			return nil
		}
		if kind != kindDir && kind != kindSymlink && kind != kindFile {
			return fmt.Errorf("unknown entry kind %#x", kind)
		}
		raw, err := readName(br, prev)
		if err != nil {
			return err
		}
		prev = raw
		h, err := readEntry(br, kind, raw)
		if err != nil {
			return err
		}
		if kind != kindFile {
			if err := fn(h, strings.NewReader("")); err != nil {
				return err
			}
			continue
		}
		data := &io.LimitedReader{R: br, N: h.Length}
		if err := fn(h, data); err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, data); err != nil {
			return err
		}
		if data.N != 0 {
			return fmt.Errorf("%s: chunk truncated in its data", h.Name)
		}
	}
}

func readName(br *bufio.Reader, prev string) (string, error) {
	shared, err := uvarint(br, uint64(len(prev)), "name prefix")
	if err != nil {
		return "", err
	}
	n, err := uvarint(br, MaxNameLen-shared, "name length")
	if err != nil {
		return "", err
	}
	suffix := make([]byte, n)
	if _, err := io.ReadFull(br, suffix); err != nil {
		return "", fmt.Errorf("chunk truncated in a name: %w", unexpected(err))
	}
	name := prev[:shared] + string(suffix)
	if name == "" || strings.IndexByte(name, 0) >= 0 {
		return "", fmt.Errorf("invalid name %q", name)
	}
	return name, nil
}

func readEntry(br *bufio.Reader, kind byte, raw string) (*Header, error) {
	name, err := localfs.CleanRel(raw)
	if err != nil {
		return nil, err
	}
	mode, err := uvarint(br, 0o7777, "mode")
	if err != nil {
		return nil, err
	}
	uid, err := uvarint(br, math.MaxUint32, "uid")
	if err != nil {
		return nil, err
	}
	gid, err := uvarint(br, math.MaxUint32, "gid")
	if err != nil {
		return nil, err
	}
	mtime, err := binary.ReadVarint(br)
	if err != nil {
		return nil, fmt.Errorf("%s: bad mtime: %w", name, unexpected(err))
	}
	h := &Header{Name: name, Entry: api.Entry{Name: name, Mode: uint32(mode), UID: uint32(uid), GID: uint32(gid), MtimeNs: mtime}}
	switch kind {
	case kindDir:
		h.Entry.Type = api.TypeDir
	case kindSymlink:
		h.Entry.Type = api.TypeSymlink
		n, err := uvarint(br, maxLinkLen, "link length")
		if err != nil {
			return nil, err
		}
		target := make([]byte, n)
		if _, err := io.ReadFull(br, target); err != nil {
			return nil, fmt.Errorf("chunk truncated in a link: %w", unexpected(err))
		}
		if n == 0 || strings.IndexByte(string(target), 0) >= 0 {
			return nil, fmt.Errorf("%s: invalid link target", name)
		}
		h.Entry.LinkTarget = string(target)
	case kindFile:
		h.Entry.Type = api.TypeFile
		size, err := uvarint(br, maxFileSize, "size")
		if err != nil {
			return nil, err
		}
		off, err := uvarint(br, size, "offset")
		if err != nil {
			return nil, err
		}
		length, err := uvarint(br, size-off, "length")
		if err != nil {
			return nil, err
		}
		h.Entry.Size, h.Offset, h.Length = int64(size), int64(off), int64(length)
	}
	if name == "." && h.Entry.Type != api.TypeDir {
		return nil, fmt.Errorf("bundle root must be a directory")
	}
	return h, nil
}

// uvarint reads an unsigned varint no larger than max.
func uvarint(br *bufio.Reader, max uint64, what string) (uint64, error) {
	v, err := binary.ReadUvarint(br)
	if err != nil {
		return 0, fmt.Errorf("bad %s: %w", what, unexpected(err))
	}
	if v > max {
		return 0, fmt.Errorf("%s %d out of range", what, v)
	}
	return v, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
