package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
)

// ScanStats summarises a scan.
type ScanStats struct {
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Errors  int64 `json:"errors"`
	Missing bool  `json:"missing"`
}

// ScanOptions configure Scan.
type ScanOptions struct {
	Checksum bool
	// Exclude is a root-relative directory left out (fkbr's staging area).
	Exclude string
	// Skip, if set, leaves out entries by their path inside base (a
	// directory with everything below it).
	Skip func(rel string, dir bool) bool
	// Progress, if set, is called about every interval.
	Progress func(ScanStats)
	Interval time.Duration
}

// Scan walks base inside root and writes a snapshot. A missing base gives
// an empty snapshot marked Missing; a base that is not a directory is an
// error. Unreadable entries become TypeError lines rather than failing.
func Scan(fsys localfs.FS, root, base string, h Header, opts ScanOptions, out io.Writer) (*ScanStats, error) {
	base, err := localfs.CleanRel(base)
	if err != nil {
		return nil, err
	}
	st := &ScanStats{}
	be, err := fsys.Stat(root, base)
	if errors.Is(err, fs.ErrNotExist) {
		st.Missing, h.Missing = true, true
		w, err := NewWriter(out, h)
		if err != nil {
			return nil, err
		}
		return st, w.Close()
	}
	if err != nil {
		return nil, err
	}
	if be.Type != api.TypeDir {
		return nil, fmt.Errorf("%w: %s is not a directory", api.ErrBadPath, base)
	}
	h.Checksum = opts.Checksum
	w, err := NewWriter(out, h)
	if err != nil {
		return nil, err
	}
	last := time.Now()
	err = fsys.Walk(root, base, func(rel string, e api.Entry, werr error) error {
		if opts.Exclude != "" && (rel == opts.Exclude || strings.HasPrefix(rel, opts.Exclude+"/")) {
			return localfs.SkipDir
		}
		p := relTo(base, rel)
		if opts.Skip != nil && werr == nil && opts.Skip(p, e.Type == api.TypeDir) {
			if e.Type == api.TypeDir {
				return localfs.SkipDir
			}
			return nil
		}
		if werr != nil {
			st.Errors++
			return w.Write(&Entry{P: p, T: TypeError, E: werr.Error()})
		}
		ent := &Entry{P: p, T: typeOf(e.Type), M: e.MtimeNs, Mo: e.Mode, U: e.UID, G: e.GID, L: e.LinkTarget}
		if e.Type == api.TypeFile {
			ent.S = e.Size
			st.Bytes += e.Size
			if opts.Checksum {
				sum, err := hashFile(fsys, root, rel)
				if err != nil {
					st.Errors++
					return w.Write(&Entry{P: p, T: TypeError, E: err.Error()})
				}
				ent.H = sum
			}
		}
		st.Entries++
		if opts.Progress != nil && time.Since(last) >= opts.Interval {
			last = time.Now()
			opts.Progress(*st)
		}
		return w.Write(ent)
	})
	if err != nil {
		return nil, err
	}
	return st, w.Close()
}

// relTo makes a root-relative walk path relative to base.
func relTo(base, rel string) string {
	if base == "." || rel == base {
		if rel == base {
			return "."
		}
		return rel
	}
	return strings.TrimPrefix(rel, base+"/")
}

func hashFile(fsys localfs.FS, root, rel string) (string, error) {
	f, err := fsys.Open(root, rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Join turns a snapshot-relative path back into a root-relative one.
func Join(base, p string) string {
	if p == "." {
		return base
	}
	return path.Join(base, p)
}
