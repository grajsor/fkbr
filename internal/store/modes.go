package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"fkbr/internal/product"
)

// Share modes. Nodes on different hosts usually run as different users,
// and each node creates, renames and deletes entries in the others'
// directories (requests, replies, acks, pruning). So directories are open
// to everyone, without the sticky bit since nodes delete each other's
// files, and files are readable by everyone. Files are never edited in
// place, only replaced or removed, which needs write access to the
// directory alone. The modes are set explicitly so the umask cannot
// narrow them; keep the share directory itself where only your hosts can
// reach it.
const (
	shareDirMode  = 0o777
	shareFileMode = 0o644
)

// Private shares (Options.Private) are this user's alone.
const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
)

func (m *Dir) dirMode() fs.FileMode {
	if m.opts.Private {
		return privateDirMode
	}
	return shareDirMode
}

func (m *Dir) fileMode() fs.FileMode {
	if m.opts.Private {
		return privateFileMode
	}
	return shareFileMode
}

// mkdir creates dir and any missing parents with the share's mode. What
// stands in its place and is no directory (a symlink a share writer put
// there, a file) is removed first: fkbr never makes either in its layout.
func (m *Dir) mkdir(dir string) error {
	if fi, err := m.fs.Lstat(dir); err == nil {
		if fi.IsDir() {
			return nil
		}
		if err := m.fs.Remove(dir); err != nil {
			return err
		}
	}
	err := m.fs.Mkdir(dir, m.dirMode())
	switch {
	case err == nil:
		return m.fs.Chmod(dir, m.dirMode()) // Mkdir applied the umask
	case errors.Is(err, fs.ErrExist):
		return nil
	case errors.Is(err, fs.ErrNotExist) && filepath.Dir(dir) != dir:
		if err := m.mkdir(filepath.Dir(dir)); err != nil {
			return err
		}
		return m.mkdir(dir)
	}
	return err
}

// openUp gives the entries this user owns the share modes. Shares made by
// earlier versions used 0770 and 0660, which locked out nodes running as
// other users; each node fixes what it created, and leaves the rest to
// their owners. Only fkbr's own entries: anything else in the share
// directory is left as it is, and a symlink is never followed (review
// 2026-10-25: a share that was not a directory of its own had every
// file of the user's opened up).
func (m *Dir) openUp() {
	uid := uint32(os.Geteuid())
	var walk func(p string, depth int)
	walk = func(p string, depth int) {
		fi, err := m.fs.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == uid {
			want := m.fileMode()
			if fi.IsDir() {
				want = m.dirMode()
			}
			if fi.Mode().Perm() != want {
				m.fs.Chmod(p, want)
			}
		}
		if !fi.IsDir() || depth > 6 { // the layout is never deeper
			return
		}
		des, err := m.fs.ReadDir(p)
		if err != nil {
			return
		}
		for _, de := range des {
			walk(filepath.Join(p, de.Name()), depth+1)
		}
	}
	if fi, err := m.fs.Stat(m.root); err == nil && fi.Mode().Perm() != m.dirMode() { // the share itself
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == uid {
			m.fs.Chmod(m.root, m.dirMode())
		}
	}
	for _, top := range layoutNames {
		walk(m.path(top), 0)
	}
}

// layoutNames are the share directory's entries that are fkbr's.
var layoutNames = []string{"tmp", "nodes", "inbox", "reply", "jobs", "data", "blobs", "protocol-version"}

// checkShareDir refuses a share directory that is not one of its own: /,
// the home directory, the binary's directory, or one with other things in
// it that is no share yet (no protocol-version). Every node opens up and
// prunes its share (review 2026-10-25: fkbr ~ by mistake would have done
// that to the home directory). What a NAS keeps in an export's top
// (lost+found, .snapshot, #recycle, @eaDir) may be there.
func checkShareDir(root string) error {
	real := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		real = r
	}
	same := func(p string) bool {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return p == real
	}
	why := ""
	switch {
	case real == "/":
		why = "is the root of the filesystem"
	case func() bool { h, err := os.UserHomeDir(); return err == nil && same(h) }():
		why = "is your home directory"
	case func() bool { e, err := os.Executable(); return err == nil && same(filepath.Dir(e)) }():
		why = "is the directory " + product.Name + " is in"
	}
	if why == "" {
		f, err := os.Open(root)
		if err != nil {
			return nil // Open reports it
		}
		names, _ := f.Readdirnames(-1)
		f.Close()
		if slices.Contains(names, "protocol-version") {
			return nil
		}
		for _, n := range names {
			if n == "lost+found" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "#") || strings.HasPrefix(n, "@") ||
				slices.Contains(layoutNames, n) { // fkbr's own, before the first node wrote protocol-version
				continue
			}
			why = fmt.Sprintf("has other things in it (%s) and is no share yet", n)
			break
		}
	}
	if why == "" {
		return nil
	}
	return fmt.Errorf("share %s %s: give %s a directory of its own on the NFS mount (an empty one starts a share)", root, why, product.Name)
}
