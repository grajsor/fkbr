package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
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

// mkdir creates dir and any missing parents with the share's mode.
func (m *Dir) mkdir(dir string) error {
	err := os.Mkdir(dir, m.dirMode())
	switch {
	case err == nil:
		return os.Chmod(dir, m.dirMode()) // Mkdir applied the umask
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
// their owners.
func (m *Dir) openUp() {
	uid := uint32(os.Geteuid())
	filepath.WalkDir(m.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // vanished or not ours to read: skip
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			return nil
		}
		want := m.fileMode()
		if d.IsDir() {
			want = m.dirMode()
		}
		if fi.Mode().Perm() != want {
			os.Chmod(p, want)
		}
		return nil
	})
}
