package store

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// shareFS does the store's file operations through an os.Root of the
// share, so none of them can leave it: not through a symlink a share
// writer put in place of one of its folders (tmp, reply/<node>, data/...),
// which made pruning delete and chmod outside the share (review
// 2026-10-25), and not through a forged record's path. Paths are given as
// the store builds them, under the share's root; one outside it is
// refused.
//
// The share stays the directory opened at the start. If the path comes to
// name another one (the share remounted, or removed and made again),
// check opens that.
//
// Where the kernel has openat2 (Linux 5.6, backported to RHEL 8), the
// frequent operations (open, stat, remove, rename) resolve a path with it,
// RESOLVE_BENEATH: one lookup like any path's. os.Root opens every folder
// on the way instead, and on NFS each open asks the server for the
// folder's attributes (close-to-open): three times the GETATTRs on the
// lab, a small-files copy 8% slower. Elsewhere, and for the rare
// operations, os.Root.
type shareFS struct {
	root    string
	mu      sync.RWMutex
	r       *os.Root
	dir     *os.File // the same directory, for openat2
	beneath bool     // openat2 works here
}

var errOutsideShare = errors.New("not in the share")

func openShareFS(root string) (*shareFS, error) {
	s := &shareFS{root: root}
	if err := s.open(); err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(int(s.dir.Fd()), ".", &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH})
	if err == nil {
		unix.Close(fd)
		s.beneath = !noOpenat2
	}
	return s, nil
}

// noOpenat2: tests take the os.Root way everywhere.
var noOpenat2 bool

func (s *shareFS) open() error {
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	dir, err := os.Open(s.root)
	if err != nil {
		r.Close()
		return err
	}
	s.mu.Lock()
	oldR, oldDir := s.r, s.dir
	s.r, s.dir = r, dir
	s.mu.Unlock()
	if oldR != nil {
		oldR.Close()
		oldDir.Close()
	}
	return nil
}

// at is the open root and p relative to it.
func (s *shareFS) at(p string) (*os.Root, string, error) {
	s.mu.RLock()
	r := s.r
	s.mu.RUnlock()
	if p == s.root {
		return r, ".", nil
	}
	if rel, ok := strings.CutPrefix(p, s.root+string(filepath.Separator)); ok {
		return r, rel, nil
	}
	return nil, "", &fs.PathError{Op: "open", Path: p, Err: errOutsideShare}
}

// openBeneath opens p with openat2, refusing any way out of the share.
func (s *shareFS) openBeneath(op, p string, flags int, perm uint32) (int, error) {
	_, rel, err := s.at(p)
	if err != nil {
		return -1, err
	}
	s.mu.RLock()
	dirfd := int(s.dir.Fd())
	s.mu.RUnlock()
	if flags&(unix.O_CREAT|unix.O_TMPFILE) == 0 {
		perm = 0 // openat2 takes a mode only when it creates
	}
	how := &unix.OpenHow{Flags: uint64(flags | unix.O_CLOEXEC), Mode: uint64(perm),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS}
	for range 8 {
		fd, err := unix.Openat2(dirfd, rel, how)
		switch {
		case err == nil:
			return fd, nil
		case err == unix.EAGAIN || err == unix.EINTR: // a rename raced the lookup
			continue
		case err == unix.EXDEV:
			err = errOutsideShare
		}
		return -1, &fs.PathError{Op: op, Path: p, Err: err}
	}
	return -1, &fs.PathError{Op: op, Path: p, Err: unix.EAGAIN}
}

// parentBeneath opens the folder p is in (O_PATH) and returns p's name.
func (s *shareFS) parentBeneath(op, p string) (int, string, error) {
	base := filepath.Base(p)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return -1, "", &fs.PathError{Op: op, Path: p, Err: unix.EINVAL}
	}
	fd, err := s.openBeneath(op, filepath.Dir(p), unix.O_PATH|unix.O_DIRECTORY, 0)
	return fd, base, err
}

// check reopens the share if its path now leads to another directory than
// the one open.
func (s *shareFS) check() {
	byPath, err := os.Stat(s.root)
	if err != nil {
		return
	}
	s.mu.RLock()
	open, err := s.dir.Stat()
	s.mu.RUnlock()
	if err == nil && os.SameFile(byPath, open) {
		return
	}
	s.open()
}

func (s *shareFS) Open(p string) (*os.File, error) {
	if s.beneath {
		return s.OpenFile(p, os.O_RDONLY, 0)
	}
	r, rel, err := s.at(p)
	if err != nil {
		return nil, err
	}
	return r.Open(rel)
}

func (s *shareFS) OpenFile(p string, flag int, perm os.FileMode) (*os.File, error) {
	if s.beneath {
		fd, err := s.openBeneath("open", p, flag, uint32(perm.Perm()))
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), p), nil
	}
	r, rel, err := s.at(p)
	if err != nil {
		return nil, err
	}
	return r.OpenFile(rel, flag, perm)
}

func (s *shareFS) ReadDir(p string) ([]os.DirEntry, error) {
	f, err := s.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	des, err := f.ReadDir(-1)
	slices.SortFunc(des, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return des, err
}

// readNames lists a directory's names, unsorted.
func (s *shareFS) readNames(p string) ([]string, error) {
	f, err := s.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// statBeneath stats p through an O_PATH descriptor (nofollow: the link).
func (s *shareFS) statBeneath(op, p string, nofollow bool) (os.FileInfo, error) {
	flags := unix.O_PATH
	if nofollow {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := s.openBeneath(op, p, flags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	return f.Stat()
}

func (s *shareFS) Stat(p string) (os.FileInfo, error) {
	if s.beneath {
		return s.statBeneath("stat", p, false)
	}
	r, rel, err := s.at(p)
	if err != nil {
		return nil, err
	}
	return r.Stat(rel)
}

func (s *shareFS) Lstat(p string) (os.FileInfo, error) {
	if s.beneath {
		return s.statBeneath("lstat", p, true)
	}
	r, rel, err := s.at(p)
	if err != nil {
		return nil, err
	}
	return r.Lstat(rel)
}

func (s *shareFS) Mkdir(p string, perm os.FileMode) error {
	r, rel, err := s.at(p)
	if err != nil {
		return err
	}
	return r.Mkdir(rel, perm)
}

func (s *shareFS) Chmod(p string, mode os.FileMode) error {
	r, rel, err := s.at(p)
	if err != nil {
		return err
	}
	return r.Chmod(rel, mode)
}

// Remove removes a file or an empty directory; a symlink itself, never
// what it points to.
func (s *shareFS) Remove(p string) error {
	if s.beneath {
		dir, name, err := s.parentBeneath("remove", p)
		if err != nil {
			return err
		}
		defer unix.Close(dir)
		err = unix.Unlinkat(dir, name, 0)
		if err == unix.EISDIR || err == unix.EPERM {
			if derr := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); derr == nil || err == unix.EISDIR {
				err = derr
			}
		}
		if err != nil {
			return &fs.PathError{Op: "remove", Path: p, Err: err}
		}
		return nil
	}
	r, rel, err := s.at(p)
	if err != nil {
		return err
	}
	return r.Remove(rel)
}

// RemoveAll removes a tree below the share, never following a symlink.
func (s *shareFS) RemoveAll(p string) error {
	r, rel, err := s.at(p)
	if err != nil {
		return err
	}
	if rel == "." {
		return &fs.PathError{Op: "removeall", Path: p, Err: syscall.EINVAL}
	}
	return r.RemoveAll(rel)
}

func (s *shareFS) Rename(from, to string) error {
	if s.beneath {
		fd, fname, err := s.parentBeneath("rename", from)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		td, tname, err := s.parentBeneath("rename", to)
		if err != nil {
			return err
		}
		defer unix.Close(td)
		if err := unix.Renameat(fd, fname, td, tname); err != nil {
			return &os.LinkError{Op: "rename", Old: from, New: to, Err: err}
		}
		return nil
	}
	r, a, err := s.at(from)
	if err != nil {
		return err
	}
	_, b, err := s.at(to)
	if err != nil {
		return err
	}
	return r.Rename(a, b)
}

// CreateTemp is os.CreateTemp in the share: a new file in dir named after
// pattern, its last "*" replaced by a random number.
func (s *shareFS) CreateTemp(dir, pattern string) (*os.File, error) {
	prefix, suffix := pattern, ""
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for range 10000 {
		name := prefix + strconv.FormatUint(uint64(rand.Uint32()), 10) + suffix
		f, err := s.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, pattern), Err: fs.ErrExist}
}

// writeAtomic publishes dest with data: written in tmpDir, synced, closed,
// then renamed into place (see package atomicfile).
func (s *shareFS) writeAtomic(tmpDir, dest string, data []byte, perm os.FileMode) (err error) {
	f, err := s.CreateTemp(tmpDir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			s.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return s.Rename(f.Name(), dest)
}

// touch creates and removes an empty file at p (the NFS refresh trick).
func (s *shareFS) touch(p string) error {
	f, err := s.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return s.Remove(p)
}

func (s *shareFS) String() string { return fmt.Sprintf("share %s", s.root) }
