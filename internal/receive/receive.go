// Package receive installs bundle entries into a destination root safely.
//
// Entries are first extracted into a staging tree inside the destination
// root (<root>/.fkbr-tmp/<job>/tree), so every path operation goes through
// os.Root and nothing can escape the root via ".." or symlinks. Commit then
// renames entries into place under the conflict policy and finally applies
// directory metadata, deepest first. Staging survives restarts: re-applying
// the same chunks and re-running Commit is safe.
package receive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
)

// StagingDir is the per-root directory holding in-progress receives.
// Senders exclude it from their walks.
const StagingDir = "." + product.Name + "-tmp"

const maxProblems = 200

type Options struct {
	Conflict api.ConflictPolicy
	// Chown tries to preserve UID/GID (needs root or CAP_CHOWN).
	Chown bool
	// ChownDirs also gives directories to their owners. Only safe with root
	// or CAP_DAC_OVERRIDE: otherwise we could not write into them later.
	ChownDirs bool
	// AllowSetID keeps setuid/setgid bits instead of stripping them.
	AllowSetID bool
}

// Result summarises a commit.
type Result struct {
	Files, Dirs, Symlinks int64
	Bytes                 int64
	Skipped               int64
	Errors                int64
	// OwnerNotPreserved counts entries whose UID/GID could not be applied.
	OwnerNotPreserved int64
	Problems          []string
}

func (r *Result) problem(format string, args ...any) {
	if len(r.Problems) < maxProblems {
		r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
	}
}

type Receiver struct {
	root   *os.Root
	base   string // .fkbr-tmp/<job>
	tree   string // .fkbr-tmp/<job>/tree
	dstDir string
	opts   Options
	bad    map[string]bool
	// ownerFails counts staged entries whose ownership could not be applied
	// and setidLost those whose setuid/setgid bits did not survive chown
	// (in this process; a restart may undercount).
	ownerFails, setidLost int64
	// dirsNotChowned counts directories left owned by us on purpose.
	dirsNotChowned int64
}

// New opens (or resumes) staging for job, installing into dstDir (relative
// to root).
func New(root *os.Root, job, dstDir string, opts Options) (*Receiver, error) {
	dst, err := localfs.CleanRel(dstDir)
	if err != nil {
		return nil, err
	}
	if dst == StagingDir || strings.HasPrefix(dst, StagingDir+"/") {
		return nil, fmt.Errorf("%w: cannot receive into %s", api.ErrBadPath, StagingDir)
	}
	if !opts.Conflict.Valid() {
		return nil, fmt.Errorf("invalid conflict policy %q", opts.Conflict)
	}
	r := &Receiver{
		root:   root,
		base:   path.Join(StagingDir, job),
		tree:   path.Join(StagingDir, job, "tree"),
		dstDir: dst,
		opts:   opts,
		bad:    map[string]bool{},
	}
	if err := root.MkdirAll(r.tree, 0o700); err != nil {
		return nil, err
	}
	if err := r.loadBad(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Receiver) loadBad() error {
	f, err := r.root.Open(path.Join(r.base, "bad"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		r.bad[sc.Text()] = true
	}
	return sc.Err()
}

// appendLine appends to a journal file in the staging base directory.
func (r *Receiver) appendLine(name string, line []byte) error {
	f, err := r.root.OpenFile(path.Join(r.base, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// MarkBad drops files the source reported as unreadable or changed.
func (r *Receiver) MarkBad(names []string) error {
	for _, n := range names {
		n, err := localfs.CleanRel(n)
		if err != nil || r.bad[n] {
			continue
		}
		r.bad[n] = true
		if err := r.appendLine("bad", []byte(n)); err != nil {
			return err
		}
		if err := r.root.RemoveAll(path.Join(r.tree, n)); err != nil {
			return err
		}
	}
	return nil
}

type dirRecord struct {
	Name  string    `json:"name"`
	Entry api.Entry `json:"entry"`
}

// Apply stages one bundle entry.
func (r *Receiver) Apply(h *bundle.Header, data io.Reader) error {
	if r.bad[h.Name] {
		return nil
	}
	p := path.Join(r.tree, h.Name)
	if h.Name != "." {
		if err := r.root.MkdirAll(path.Dir(p), 0o700); err != nil {
			return err
		}
	}
	switch h.Entry.Type {
	case api.TypeDir:
		if err := r.root.MkdirAll(p, 0o700); err != nil {
			return err
		}
		line, _ := json.Marshal(dirRecord{h.Name, h.Entry})
		return r.appendLine("dirs", line)
	case api.TypeSymlink:
		if err := r.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.root.Symlink(h.Entry.LinkTarget, p); err != nil {
			return err
		}
		r.chown(p, h.Entry, true)
		return nil
	case api.TypeFile:
		return r.applyFile(p, h, data)
	}
	return fmt.Errorf("%s: unsupported type %q", h.Name, h.Entry.Type)
}

func (r *Receiver) applyFile(p string, h *bundle.Header, data io.Reader) error {
	// A previous run may have left a non-file here; replace it.
	if fi, err := r.root.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		if err := r.root.RemoveAll(p); err != nil {
			return err
		}
	}
	f, err := r.root.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
	if errors.Is(err, fs.ErrPermission) {
		// Replay of a file already completed and given to its owner: we
		// may not write to it any more, but the staging dir is ours.
		if err = r.root.Remove(p); err == nil {
			f, err = r.root.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
		}
	}
	if err != nil {
		return err
	}
	if _, err := f.Seek(h.Offset, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	if n, err := writeSparse(f, data, h.Offset); err != nil || n != h.Length {
		f.Close()
		return fmt.Errorf("%s: wrote %d of %d bytes: %v", h.Name, n, h.Length, err)
	}
	if h.Last() {
		if err := f.Truncate(h.Entry.Size); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if h.Last() {
		return r.setMeta(p, h.Entry)
	}
	return nil
}

// writeSparse copies data into f at offset (where f is positioned). An
// all-zero block beyond the current end of f is skipped, leaving a hole
// that reads as zeros, so sparse files stay sparse (as with cp
// --sparse=auto); the final Truncate gives the file its size. Below the
// end every block is written, so a replayed chunk never leaves old data.
func writeSparse(f *os.File, data io.Reader, offset int64) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	end := fi.Size()
	buf := make([]byte, sparseBlock)
	var n int64
	for {
		k, rerr := io.ReadFull(data, buf)
		if k > 0 {
			b := buf[:k]
			if offset+n >= end && allZero(b) {
				if _, err := f.Seek(int64(k), io.SeekCurrent); err != nil {
					return n, err
				}
			} else if _, err := f.Write(b); err != nil {
				return n, err
			}
			n += int64(k)
		}
		switch {
		case rerr == io.EOF || rerr == io.ErrUnexpectedEOF:
			return n, nil
		case rerr != nil:
			return n, rerr
		}
	}
}

const sparseBlock = 64 << 10

var zeros = make([]byte, sparseBlock)

func allZero(b []byte) bool { return bytes.Equal(b, zeros[:len(b)]) }

// setDirMeta is setMeta for directories, keeping them ours unless we may
// still write into them after giving them away.
func (r *Receiver) setDirMeta(p string, e api.Entry) error {
	if r.opts.ChownDirs || !r.opts.Chown {
		return r.setMeta(p, e)
	}
	if err := r.root.Chmod(p, r.mode(e.Mode)); err != nil {
		return err
	}
	t := time.Unix(0, e.MtimeNs)
	if err := r.root.Chtimes(p, t, t); err != nil {
		return err
	}
	if e.UID != uint32(os.Getuid()) || e.GID != uint32(os.Getgid()) {
		r.dirsNotChowned++
	}
	return nil
}

// setMeta applies mode, mtime and ownership in an order that works for a
// non-root account holding only CAP_CHOWN: once a file belongs to someone
// else we can no longer chmod or chtimes it, so ownership goes last.
// chown clears setuid/setgid, so those bits are re-applied afterwards,
// which needs root or CAP_FOWNER.
func (r *Receiver) setMeta(p string, e api.Entry) error {
	m := r.mode(e.Mode)
	if err := r.root.Chmod(p, m); err != nil {
		return err
	}
	t := time.Unix(0, e.MtimeNs)
	if err := r.root.Chtimes(p, t, t); err != nil {
		return err
	}
	if r.chown(p, e, false) && r.opts.Chown && m&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		if err := r.root.Chmod(p, m); err != nil {
			r.setidLost++
		}
	}
	return nil
}

// mode converts Unix mode bits to a FileMode for Chmod, stripping
// setuid/setgid unless allowed.
func (r *Receiver) mode(u uint32) fs.FileMode {
	m := fs.FileMode(u & 0o777)
	if u&0o4000 != 0 && r.opts.AllowSetID {
		m |= fs.ModeSetuid
	}
	if u&0o2000 != 0 && r.opts.AllowSetID {
		m |= fs.ModeSetgid
	}
	if u&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// chown applies ownership best-effort, counting entries where it fails,
// and reports whether ownership holds. Without chown rights it only holds
// if the source used our IDs.
func (r *Receiver) chown(p string, e api.Entry, link bool) bool {
	ok := false
	if r.opts.Chown {
		var err error
		if link {
			err = r.root.Lchown(p, int(e.UID), int(e.GID))
		} else {
			err = r.root.Chown(p, int(e.UID), int(e.GID))
		}
		ok = err == nil
	} else {
		ok = e.UID == uint32(os.Getuid()) && e.GID == uint32(os.Getgid())
	}
	if !ok {
		r.ownerFails++
	}
	return ok
}

// Commit moves staged entries into place and removes the staging area.
func (r *Receiver) Commit() (*Result, error) {
	res := &Result{}
	created := map[string]bool{}
	if err := r.ensureDir(r.dstDir, created); err != nil {
		return res, err
	}
	if err := r.commitDir(".", res, created); err != nil {
		return res, err
	}
	if err := r.applyDirMeta(res, created); err != nil {
		return res, err
	}
	res.OwnerNotPreserved = r.ownerFails
	if r.dirsNotChowned > 0 {
		res.problem("ownership of %d directories not preserved (needs root or CAP_DAC_OVERRIDE, else later copies into them would fail)", r.dirsNotChowned)
	}
	if r.setidLost > 0 {
		res.problem("setuid/setgid bits not restored on %d files (needs root or CAP_FOWNER)", r.setidLost)
	}
	if err := r.root.RemoveAll(r.base); err != nil {
		return res, err
	}
	r.root.Remove(StagingDir) // only succeeds when no other job is staging
	return res, nil
}

// ensureDir creates dir (and parents) in the destination, recording which
// ones did not exist so their metadata may be set.
func (r *Receiver) ensureDir(dir string, created map[string]bool) error {
	if dir == "." {
		return nil
	}
	fi, err := r.root.Lstat(dir)
	if err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", dir)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.ensureDir(path.Dir(dir), created); err != nil {
		return err
	}
	err = r.writable(path.Dir(dir), func() error { return r.root.Mkdir(dir, 0o755) })
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	created[dir] = true
	return nil
}

// renameEntry moves an entry into place; tests replace it to fail.
var renameEntry = (*os.Root).Rename

// writable runs op, which changes entries in dir. If that fails for lack
// of permission because dir is read-only for its owner (a directory that
// arrived read-only in an earlier copy, say), dir is made writable for the
// moment, op tried again, and dir's mode put back, as rsync does.
func (r *Receiver) writable(dir string, op func() error) error {
	err := op()
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	fi, serr := r.root.Lstat(dir)
	if serr != nil || !fi.IsDir() || fi.Mode().Perm()&0o200 != 0 {
		return err
	}
	if r.root.Chmod(dir, fi.Mode().Perm()|0o200) != nil {
		return err // not ours
	}
	defer r.root.Chmod(dir, fi.Mode().Perm())
	return op()
}

func (r *Receiver) commitDir(rel string, res *Result, created map[string]bool) error {
	f, err := r.root.Open(path.Join(r.tree, rel))
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		child := path.Join(rel, name)
		src := path.Join(r.tree, child)
		dst := path.Join(r.dstDir, child)
		sfi, err := r.root.Lstat(src)
		if err != nil {
			return err
		}
		if sfi.IsDir() {
			if err := r.ensureDir(dst, created); err != nil {
				res.Errors++
				res.problem("%s: %v", child, err)
				continue
			}
			res.Dirs++
			if err := r.commitDir(child, res, created); err != nil {
				return err
			}
			continue
		}
		if err := r.install(child, src, dst, sfi, res); err != nil {
			return err
		}
	}
	return nil
}

func (r *Receiver) install(name, src, dst string, sfi fs.FileInfo, res *Result) error {
	dfi, err := r.root.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case dfi.IsDir():
		res.Errors++
		res.problem("%s: destination is a directory", name)
		return nil
	case r.opts.Conflict == api.ConflictSkip:
		res.Skipped++
		res.problem("%s: exists, skipped", name)
		return nil
	case r.opts.Conflict == api.ConflictOverwriteOlder && !dfi.ModTime().Before(sfi.ModTime()):
		res.Skipped++
		res.problem("%s: destination not older, skipped", name)
		return nil
	}
	if err := r.writable(path.Dir(dst), func() error { return renameEntry(r.root, src, dst) }); err != nil {
		// One unwritable directory must not stop the rest of the install.
		res.Errors++
		res.problem("%s: %v", name, err)
		return nil
	}
	if sfi.Mode()&fs.ModeSymlink != 0 {
		res.Symlinks++
	} else {
		res.Files++
		res.Bytes += sfi.Size()
	}
	return nil
}

func (r *Receiver) applyDirMeta(res *Result, created map[string]bool) error {
	f, err := r.root.Open(path.Join(r.base, "dirs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	recs := map[string]api.Entry{}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var d dirRecord
		if json.Unmarshal(sc.Bytes(), &d) == nil {
			recs[d.Name] = d.Entry
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	names := make([]string, 0, len(recs))
	for n := range recs {
		names = append(names, n)
	}
	// Children before parents, so setting a parent's mtime sticks.
	sort.Slice(names, func(i, j int) bool {
		di, dj := strings.Count(names[i], "/"), strings.Count(names[j], "/")
		if names[i] == "." {
			di = -1
		}
		if names[j] == "." {
			dj = -1
		}
		if di != dj {
			return di > dj
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		dst := path.Join(r.dstDir, n)
		if !created[dst] && r.opts.Conflict != api.ConflictOverwrite {
			continue // leave pre-existing directories alone
		}
		if err := r.setDirMeta(dst, recs[n]); err != nil {
			res.Errors++
			res.problem("%s: %v", n, err)
		}
	}
	return nil
}

// Abort discards the staging area.
func (r *Receiver) Abort() error {
	err := r.root.RemoveAll(r.base)
	r.root.Remove(StagingDir)
	return err
}
