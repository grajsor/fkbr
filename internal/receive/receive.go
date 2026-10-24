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
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

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
	// Hide is the share's path inside the root, if it is there; nothing is
	// written into it (see config.Root.Hide).
	Hide string
	// NoModes: the destination filesystem has no permission bits (FAT,
	// SMB); modes are not set (vfat refuses some, e.g. sticky folders).
	NoModes bool
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
	// setidStripped counts entries whose setuid/setgid bits were left off
	// because allow_setid is off.
	setidStripped int64
	// dirsNotChowned counts directories left owned by us on purpose.
	dirsNotChowned int64
	// mtimeLost counts files whose mtime the filesystem did not store
	// (beyond its range: XFS without bigtime stops at 2038), one example.
	mtimeLost    int64
	mtimeExample string
	// dir is the staging directory files were last written to, kept open:
	// files arrive grouped by directory, and a name relative to an open
	// directory resolves in one step instead of one per path component.
	dir     *os.Root
	dirPath string
	// destNames caches destination directory listings during a commit
	// (see listed).
	destNames map[string]map[string]bool
}

// stagingDir returns dir (inside the staging tree) open, creating it.
func (r *Receiver) stagingDir(dir string) (*os.Root, error) {
	if r.dir != nil && r.dirPath == dir {
		return r.dir, nil
	}
	r.closeDir()
	if err := r.root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	d, err := r.root.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	r.dir, r.dirPath = d, dir
	return d, nil
}

func (r *Receiver) closeDir() {
	if r.dir != nil {
		r.dir.Close()
		r.dir, r.dirPath = nil, ""
	}
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
	if opts.Hide != "" && (dst == opts.Hide || strings.HasPrefix(dst, opts.Hide+"/")) {
		return nil, fmt.Errorf("%w: cannot receive into %s, the share", api.ErrBadPath, opts.Hide)
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
	r.closeDir() // what it removes may be the open directory
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

// Apply stages one bundle entry. An entry this destination cannot hold is
// refused (see entryError) and the rest goes on.
func (r *Receiver) Apply(h *bundle.Header, data io.Reader) error {
	if r.skip(h.Name) {
		return nil
	}
	err := r.apply(h, data)
	// exFAT answers EPERM to every call with a name that is not valid
	// UTF-8 (create, stat, unlink): it cannot hold that name.
	if err != nil && errors.Is(err, syscall.EPERM) && !utf8.ValidString(path.Base(h.Name)) {
		err = entryError{err}
	}
	if err != nil && isEntryError(err) {
		return r.refuse(h.Name, err)
	}
	return err
}

// skip reports whether name, or a directory above it, was dropped.
func (r *Receiver) skip(name string) bool {
	if len(r.bad) == 0 {
		return false
	}
	for n := name; ; n = path.Dir(n) {
		if r.bad[n] {
			return true
		}
		if n == "." || n == "/" {
			return false
		}
	}
}

// entryError is an error that concerns only one entry: the destination
// cannot hold it (a name FAT refuses, a file over the size limit, a
// symlink where there are none, a name that clashes with another).
type entryError struct{ err error }

func (e entryError) Error() string { return e.err.Error() }
func (e entryError) Unwrap() error { return e.err }

func isEntryError(err error) bool {
	var ee entryError
	return errors.As(err, &ee) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, syscall.EFBIG) || errors.Is(err, syscall.EILSEQ)
}

// refuse drops an entry the destination cannot hold, for good (a resume
// skips it too), and records why for the commit's report. What it left in
// staging is removed, unless its name clashes: that removal would hit the
// other entry.
func (r *Receiver) refuse(name string, cause error) error {
	r.closeDir()
	r.bad[name] = true
	if err := r.appendLine("bad", []byte(name)); err != nil {
		return err
	}
	line, _ := json.Marshal(refusal{name, cause.Error()})
	if err := r.appendLine("refused", line); err != nil {
		return err
	}
	if errors.Is(cause, errClashed) {
		return nil
	}
	p := path.Join(r.tree, name)
	if errors.Is(cause, errNameAltered) {
		// What was created is the shortened name; it is ours (a staged
		// entry of that name would have been a clash already).
		if base := strings.TrimRight(path.Base(p), ". "); base != "" { // never the folder itself ("...")
			if err := removeStaged(r.root, path.Join(path.Dir(p), base)); err != nil {
				return err
			}
		}
	}
	if err := removeStaged(r.root, p); err != nil {
		// exFAT answers EPERM for a name it cannot hold: then nothing by
		// that name was staged, and the entry is just refused.
		if _, lerr := r.root.Lstat(p); lerr != nil {
			return nil
		}
		return err
	}
	return nil
}

// Staging operations tests replace to act like FAT and exFAT.
var (
	removeStaged = (*os.Root).RemoveAll
	mkdirStaged  = (*os.Root).MkdirAll
)

// altered reports whether creating name in dir (in staging) stored another
// name: FAT and exFAT drop a trailing dot (FAT also trailing spaces)
// without an error, so "dir." would land as "dir", merged with a real
// "dir". Only names ending in a dot or space are checked (a listing per
// such name).
func (r *Receiver) altered(dir, name string) bool {
	if !strings.HasSuffix(name, ".") && !strings.HasSuffix(name, " ") {
		return false
	}
	return !r.hasName(dir, name)
}

// errNameAltered: the filesystem stored the name without its trailing dots
// or spaces.
var errNameAltered = errors.New("this filesystem stores the name without its trailing dot or space")

func errAltered(name string) error { return entryError{fmt.Errorf("%s: %w", name, errNameAltered)} }

type refusal struct {
	Name string `json:"name"`
	Err  string `json:"err"`
}

// listed is hasName for destination directories during a commit, with
// each directory read once (only where a name already exists).
func (r *Receiver) listed(dir, name string) bool {
	names, ok := r.destNames[dir]
	if !ok {
		names = map[string]bool{}
		if f, err := r.root.Open(dir); err == nil {
			ns, err := f.Readdirnames(-1)
			f.Close()
			if err != nil {
				return true // cannot tell; let the operation itself decide
			}
			for _, n := range ns {
				names[n] = true
			}
		}
		if r.destNames == nil {
			r.destNames = map[string]map[string]bool{}
		}
		r.destNames[dir] = names
	}
	return names[name]
}

// hasName reports whether directory dir lists exactly name. Where a name
// exists but is not listed, the filesystem matched another spelling
// (README for readme on FAT).
func (r *Receiver) hasName(dir, name string) bool {
	f, err := r.root.Open(dir)
	if err != nil {
		return true // cannot tell; let the operation itself decide
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	return err != nil || slices.Contains(names, name)
}

var errClashed = errors.New("clashes with another name here (a case-insensitive filesystem?)")

func errClash(name string) error { return entryError{fmt.Errorf("%s: %w", name, errClashed)} }

func (r *Receiver) apply(h *bundle.Header, data io.Reader) error {
	p := path.Join(r.tree, h.Name)
	if h.Entry.Type == api.TypeFile {
		d, err := r.stagingDir(path.Dir(p))
		if err != nil {
			return err
		}
		if _, err := d.Lstat(path.Base(p)); err == nil && h.Offset == 0 && !r.hasName(path.Dir(p), path.Base(p)) {
			return errClash(h.Name)
		}
		if err := r.applyFile(d, path.Base(p), h, data); err != nil {
			return err
		}
		if h.Offset == 0 && r.altered(path.Dir(p), path.Base(p)) {
			return errAltered(h.Name) // refuse removes what it became
		}
		return nil
	}
	if h.Name != "." {
		if err := r.root.MkdirAll(path.Dir(p), 0o700); err != nil {
			return err
		}
	}
	switch h.Entry.Type {
	case api.TypeDir:
		if _, err := r.root.Lstat(p); err == nil && h.Name != "." && !r.hasName(path.Dir(p), path.Base(p)) {
			return errClash(h.Name)
		}
		if err := mkdirStaged(r.root, p, 0o700); err != nil {
			return err
		}
		if h.Name != "." && r.altered(path.Dir(p), path.Base(p)) {
			return errAltered(h.Name)
		}
		line, _ := json.Marshal(dirRecord{api.EncodeName(h.Name), h.Entry})
		return r.appendLine("dirs", line)
	case api.TypeSymlink:
		if err := r.root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.root.Symlink(h.Entry.LinkTarget, p); err != nil {
			return entryError{err} // FAT has no symlinks
		}
		r.linkTime(p, h.Entry.MtimeNs) // before chown: after it only root may
		r.chown(p, h.Entry, true)
		return nil
	}
	return fmt.Errorf("%s: unsupported type %q", h.Name, h.Entry.Type)
}

// openStaged opens a staged file; tests replace it to fail.
var openStaged = (*os.Root).OpenFile

// truncateStaged sets a staged file's size; tests replace it to count.
var truncateStaged = (*os.File).Truncate

// applyFile stages one file (segment) as name in the open staging dir d.
func (r *Receiver) applyFile(d *os.Root, name string, h *bundle.Header, data io.Reader) error {
	// A previous run may have left a non-file here; replace it.
	if fi, err := d.Lstat(name); err == nil && !fi.Mode().IsRegular() {
		if err := d.RemoveAll(name); err != nil {
			return err
		}
	}
	f, err := openStaged(d, name, os.O_WRONLY|os.O_CREATE, 0o600)
	if errors.Is(err, fs.ErrPermission) {
		// Replay of a file already completed and given to its owner: we
		// may not write to it any more, but the staging dir is ours.
		if rerr := d.Remove(name); rerr == nil {
			f, err = openStaged(d, name, os.O_WRONLY|os.O_CREATE, 0o600)
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
		return fmt.Errorf("%s: wrote %d of %d bytes: %w", h.Name, n, h.Length, err)
	}
	if h.Last() {
		// Only a file without its size yet (a hole at the end, or longer
		// from an earlier run) is truncated: on XFS a truncate writes the
		// file out at once, which made every small file a disk write.
		if fi, err := f.Stat(); err != nil || fi.Size() != h.Entry.Size {
			if err := truncateStaged(f, h.Entry.Size); err != nil {
				f.Close()
				return err
			}
		}
		// Through the open file: each path-based call would resolve the
		// whole staging path again, one component at a time.
		if err := r.setFileMeta(f, h.Name, h.Entry); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// setFileMeta is setMeta for a regular file that is open for writing, in
// the same order: mode, mtime, owner, setuid/setgid again after chown.
func (r *Receiver) setFileMeta(f *os.File, name string, e api.Entry) error {
	m := r.mode(e.Mode)
	if err := r.chmod(func() error { return f.Chmod(m) }); err != nil {
		return err
	}
	if err := futimes(f, e.MtimeNs); err != nil {
		return err
	}
	if fi, err := f.Stat(); err == nil {
		// More than FAT's 2-second steps: clamped or wrapped.
		if d := fi.ModTime().UnixNano() - e.MtimeNs; d > 2e9 || d < -2e9 {
			if r.mtimeLost++; r.mtimeExample == "" {
				r.mtimeExample = fmt.Sprintf("%s: %s stored as %s", name,
					time.Unix(0, e.MtimeNs).UTC().Format(time.DateOnly), fi.ModTime().UTC().Format(time.DateOnly))
			}
		}
	}
	ok := false
	if r.opts.Chown {
		ok = f.Chown(int(e.UID), int(e.GID)) == nil
	} else {
		ok = e.UID == uint32(os.Getuid()) && e.GID == uint32(os.Getgid())
	}
	if !ok {
		r.ownerFails++
	}
	if ok && r.opts.Chown && m&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		if err := f.Chmod(m); err != nil {
			r.setidLost++
		}
	}
	return nil
}

// futimes sets an open file's access and modification times (utimensat
// with a NULL path, Linux 2.6.22+).
func futimes(f *os.File, ns int64) error {
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(ns), syscall.NsecToTimespec(ns)}
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, f.Fd(), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if errno != 0 {
		return &os.PathError{Op: "futimens", Path: f.Name(), Err: errno}
	}
	return nil
}

// linkTime sets a symlink's own mtime (utimensat with AT_SYMLINK_NOFOLLOW,
// Linux 2.6.22+), as rsync -a does. Best effort: nothing reads it, and
// sync does not compare it.
func (r *Receiver) linkTime(p string, ns int64) {
	d, err := r.root.Open(path.Dir(p))
	if err != nil {
		return
	}
	defer d.Close()
	name, err := syscall.BytePtrFromString(path.Base(p))
	if err != nil {
		return
	}
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(ns), syscall.NsecToTimespec(ns)}
	syscall.Syscall6(syscall.SYS_UTIMENSAT, d.Fd(), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&ts[0])),
		atSymlinkNofollow, 0, 0)
}

const atSymlinkNofollow = 0x100 // AT_SYMLINK_NOFOLLOW, the same on amd64 and arm64

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
	if err := r.chmod(func() error { return r.root.Chmod(p, r.mode(e.Mode)) }); err != nil {
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
	if err := r.chmod(func() error { return r.root.Chmod(p, m) }); err != nil {
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

// chmod runs a mode change, unless the filesystem has no modes to set.
func (r *Receiver) chmod(do func() error) error {
	if r.opts.NoModes {
		return nil
	}
	return do()
}

// mode converts Unix mode bits to a FileMode for Chmod, stripping
// setuid/setgid unless allowed.
func (r *Receiver) mode(u uint32) fs.FileMode {
	m := fs.FileMode(u & 0o777)
	if u&0o6000 != 0 && !r.opts.AllowSetID && !r.opts.NoModes {
		r.setidStripped++
	}
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
	r.closeDir()
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
	if err := r.reportRefused(res); err != nil {
		return res, err
	}
	res.OwnerNotPreserved = r.ownerFails
	if r.dirsNotChowned > 0 {
		res.problem("ownership of %d directories not preserved (needs root or CAP_DAC_OVERRIDE, else later copies into them would fail)", r.dirsNotChowned)
	}
	if r.setidLost > 0 {
		res.problem("setuid/setgid bits not restored on %d files (needs root or CAP_FOWNER)", r.setidLost)
	}
	if r.setidStripped > 0 {
		res.problem("setuid/setgid bits left off on %d entries (allow_setid is off)", r.setidStripped)
	}
	if r.mtimeLost > 0 {
		res.problem("mtime not stored on %d files: outside what this filesystem can hold (e.g. %s)", r.mtimeLost, r.mtimeExample)
	}
	if err := r.root.RemoveAll(r.base); err != nil {
		return res, err
	}
	r.root.Remove(StagingDir) // only succeeds when no other job is staging
	return res, nil
}

// reportRefused counts the entries refused while staging as errors.
func (r *Receiver) reportRefused(res *Result) error {
	f, err := r.root.Open(path.Join(r.base, "refused"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var x refusal
		if json.Unmarshal(sc.Bytes(), &x) == nil {
			res.Errors++
			res.problem("%s: not placed: %s", x.Name, x.Err)
		}
	}
	return sc.Err()
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
		if h := r.opts.Hide; h != "" && (dst == h || strings.HasPrefix(dst, h+"/")) {
			res.Errors++
			res.problem("%s: that is the share here, not written", child)
			continue
		}
		if sfi.IsDir() {
			_, err := r.root.Lstat(dst)
			if errors.Is(err, fs.ErrNotExist) {
				// Nothing there yet: everything below is new, so the
				// whole staged directory moves into place at once.
				if err := r.moveNewDir(src, dst, res, created); err == nil {
					continue
				}
			} else if err == nil && !r.listed(path.Dir(dst), path.Base(dst)) {
				res.Errors++
				res.problem("%s: %v", child, errClashed)
				continue
			}
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

// moveNewDir renames a staged directory to dst, which does not exist, and
// counts what it holds; its directories count as created, so their
// metadata is applied.
func (r *Receiver) moveNewDir(src, dst string, res *Result, created map[string]bool) error {
	var n Result
	var dirs []string
	err := fs.WalkDir(r.root.FS(), src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		switch {
		case d.IsDir():
			n.Dirs++
			dirs = append(dirs, path.Join(dst, rel))
		case d.Type()&fs.ModeSymlink != 0:
			n.Symlinks++
		default:
			fi, err := d.Info()
			if err != nil {
				return err
			}
			n.Files++
			n.Bytes += fi.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := r.writable(path.Dir(dst), func() error { return renameEntry(r.root, src, dst) }); err != nil {
		return err
	}
	res.Dirs, res.Files, res.Symlinks, res.Bytes = res.Dirs+n.Dirs, res.Files+n.Files, res.Symlinks+n.Symlinks, res.Bytes+n.Bytes
	for _, d := range dirs {
		created[d] = true
	}
	return nil
}

func (r *Receiver) install(name, src, dst string, sfi fs.FileInfo, res *Result) error {
	dfi, err := r.root.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case !r.listed(path.Dir(dst), path.Base(dst)):
		// Another spelling (README for readme): replacing it would lose it.
		res.Errors++
		res.problem("%s: %v", name, errClashed)
		return nil
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
			recs[api.DecodeName(d.Name)] = d.Entry
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
	r.closeDir()
	err := r.root.RemoveAll(r.base)
	r.root.Remove(StagingDir)
	return err
}
