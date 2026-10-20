// Package localfs gives the engine access to this node's configured roots.
// Every operation is confined to a root with os.Root, so neither ".." nor
// symlinks can reach outside it, whatever a peer puts in a request.
package localfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"fkbr/internal/api"
	"fkbr/internal/config"
)

// FS is what the engine needs from the local filesystem. Tests may fake it.
type FS interface {
	Roots() []api.Root
	// List returns all entries of a directory, sorted by name.
	List(rootID, rel string) ([]api.Entry, error)
	Stat(rootID, rel string) (api.Entry, error)
	// Walk visits rel and, if it is a directory, everything below it in
	// lexical depth-first order (a directory before its children). The
	// order is deterministic so an interrupted sender can regenerate the
	// same stream. fn gets slash paths relative to the root.
	Walk(rootID, rel string, fn WalkFunc) error
	// Open opens a file for reading inside a root.
	Open(rootID, rel string) (*os.File, error)
	// WritableRoot returns the os.Root handle of a root that allows writes.
	WritableRoot(rootID string) (*os.Root, error)
	Close() error
}

// WalkFunc is called for every entry. Returning SkipDir for a directory
// skips its children. A non-nil err means rel could not be read (fn may
// return nil to continue).
type WalkFunc func(rel string, e api.Entry, err error) error

// SkipDir tells Walk not to descend into a directory.
var SkipDir = fs.SkipDir

// CleanRel normalises a root-relative slash path. It rejects absolute paths
// and anything that climbs above the root; "" means the root itself (".").
func CleanRel(rel string) (string, error) {
	if rel == "" {
		return ".", nil
	}
	if path.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: %q", api.ErrBadPath, rel)
	}
	c := path.Clean(rel)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: %q escapes root", api.ErrBadPath, rel)
	}
	return c, nil
}

type root struct {
	cfg config.Root
	r   *os.Root
}

// Local is the real FS over the configured roots.
type Local struct {
	roots map[string]*root
	order []api.Root
}

// Open opens every configured root. A missing root directory is an error so
// misconfiguration surfaces at startup rather than on the first request.
func Open(roots []config.Root) (*Local, error) {
	l := &Local{roots: map[string]*root{}}
	for _, rc := range roots {
		r, err := os.OpenRoot(rc.Path)
		if err != nil {
			l.Close()
			return nil, fmt.Errorf("root %q: %w", rc.ID, err)
		}
		l.roots[rc.ID] = &root{cfg: rc, r: r}
		l.order = append(l.order, api.Root{ID: rc.ID, Path: rc.Path, Writable: rc.AllowWrite})
	}
	return l, nil
}

func (l *Local) Close() error {
	var errs []error
	for _, r := range l.roots {
		errs = append(errs, r.r.Close())
	}
	return errors.Join(errs...)
}

func (l *Local) Roots() []api.Root { return append([]api.Root(nil), l.order...) }

func (l *Local) lookup(rootID, rel string) (*root, string, error) {
	r, ok := l.roots[rootID]
	if !ok {
		return nil, "", fmt.Errorf("%w: %q", api.ErrUnknownRoot, rootID)
	}
	c, err := CleanRel(rel)
	if err != nil {
		return nil, "", err
	}
	if r.hidden(c) {
		return nil, "", &fs.PathError{Op: "open", Path: c, Err: fs.ErrNotExist}
	}
	return r, c, nil
}

// Hidden reports whether rel is the share, or inside it, in a root that
// holds the share (see config.Root.Hide).
func (l *Local) Hidden(rootID, rel string) bool {
	r, ok := l.roots[rootID]
	return ok && r.hidden(rel)
}

func (r *root) hidden(rel string) bool {
	h := r.cfg.Hide
	return h != "" && (rel == h || strings.HasPrefix(rel, h+"/"))
}

func (l *Local) List(rootID, rel string) ([]api.Entry, error) {
	r, rel, err := l.lookup(rootID, rel)
	if err != nil {
		return nil, err
	}
	f, err := r.r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []api.Entry
	for {
		des, err := f.ReadDir(4096)
		for _, de := range des {
			if r.hidden(path.Join(rel, de.Name())) {
				continue
			}
			e, err := r.entry(path.Join(rel, de.Name()))
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed while listing
			}
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (l *Local) Stat(rootID, rel string) (api.Entry, error) {
	r, rel, err := l.lookup(rootID, rel)
	if err != nil {
		return api.Entry{}, err
	}
	return r.entry(rel)
}

func (l *Local) Open(rootID, rel string) (*os.File, error) {
	r, rel, err := l.lookup(rootID, rel)
	if err != nil {
		return nil, err
	}
	return r.r.Open(rel)
}

func (l *Local) WritableRoot(rootID string) (*os.Root, error) {
	r, ok := l.roots[rootID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", api.ErrUnknownRoot, rootID)
	}
	if !r.cfg.AllowWrite {
		return nil, fmt.Errorf("%w: %q", api.ErrReadOnly, rootID)
	}
	return r.r, nil
}

func (l *Local) Walk(rootID, rel string, fn WalkFunc) error {
	r, rel, err := l.lookup(rootID, rel)
	if err != nil {
		return err
	}
	e, err := r.entry(rel)
	if err != nil {
		return fn(rel, api.Entry{}, err)
	}
	err = r.walk(rel, e, fn)
	if err == SkipDir {
		return nil
	}
	return err
}

func (r *root) walk(rel string, e api.Entry, fn WalkFunc) error {
	if err := fn(rel, e, nil); err != nil || e.Type != api.TypeDir {
		return err
	}
	names, err := r.readNames(rel)
	if err != nil {
		if err := fn(rel, e, err); err != nil && err != SkipDir {
			return err
		}
		return nil
	}
	for _, name := range names {
		child := path.Join(rel, name)
		if r.hidden(child) {
			continue
		}
		ce, err := r.entry(child)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			if err := fn(child, ce, err); err != nil && err != SkipDir {
				return err
			}
			continue
		}
		if err := r.walk(child, ce, fn); err != nil && err != SkipDir {
			return err
		}
	}
	return nil
}

// readNames returns the sorted names in a directory without stat'ing them.
func (r *root) readNames(rel string) ([]string, error) {
	f, err := r.r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// entry lstats rel inside the root and converts it to an api.Entry.
func (r *root) entry(rel string) (api.Entry, error) {
	fi, err := r.r.Lstat(rel)
	if err != nil {
		return api.Entry{}, err
	}
	e := FromFileInfo(fi)
	if rel == "." {
		e.Name = "."
	}
	if e.Type == api.TypeSymlink {
		if e.LinkTarget, err = r.r.Readlink(rel); err != nil {
			return api.Entry{}, err
		}
	}
	return e, nil
}

// FromFileInfo converts an lstat result; LinkTarget is left for the caller.
func FromFileInfo(fi fs.FileInfo) api.Entry {
	e := api.Entry{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Mode:    UnixMode(fi.Mode()),
		MtimeNs: fi.ModTime().UnixNano(),
	}
	switch m := fi.Mode(); {
	case m.IsRegular():
		e.Type = api.TypeFile
	case m.IsDir():
		e.Type = api.TypeDir
		e.Size = 0
	case m&fs.ModeSymlink != 0:
		e.Type = api.TypeSymlink
	default:
		e.Type = api.TypeOther
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		e.UID, e.GID = st.Uid, st.Gid
	}
	return e
}

// UnixMode converts Go's FileMode to Unix permission + special bits.
func UnixMode(m fs.FileMode) uint32 {
	u := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		u |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		u |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		u |= 0o1000
	}
	return u
}

// ProbeDACOverride reports whether this process may write into
// directories it has no write permission on (root or CAP_DAC_OVERRIDE).
// Without it, giving a directory away to its original owner would make
// later transfers into it fail.
func ProbeDACOverride(dir string) bool {
	if os.Geteuid() == 0 {
		return true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	d, err := os.MkdirTemp(dir, ".dac-probe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(d)
	defer os.Chmod(d, 0o700)
	if err := os.Chmod(d, 0o500); err != nil {
		return false
	}
	f, err := os.Create(filepath.Join(d, "x"))
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// ProbeChown reports whether this process may give files away to other
// users, which is needed to preserve ownership on receive. It tests for real
// on a scratch file in dir rather than trusting euid alone, so CAP_CHOWN on a
// service account is detected too.
func ProbeChown(dir string) bool {
	if os.Geteuid() == 0 {
		return true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".chown-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	// Giving a file to root is never allowed without CAP_CHOWN.
	return os.Lchown(filepath.Clean(name), 0, -1) == nil
}

// Limits says what the filesystem holding path cannot store: mtimes only
// in steps of mtimeStep seconds, or no permission bits. A sync compares
// within these, so it does not send files again for what never sticks.
func Limits(path string) (mtimeStep int64, noModes bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(path, &st) != nil {
		return 0, false
	}
	switch uint32(st.Type) {
	case 0x4d44, 0x2011bab0: // vfat/msdos, exfat
		return 2, true
	case 0xff534d42, 0xfe534d42, 0x517b, 0x7366746e: // cifs, smb2, smbfs, ntfs3
		return 0, true
	}
	return 0, false
}
