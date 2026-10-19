package nfsmailbox

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

// mkdir creates dir and any missing parents with shareDirMode.
func mkdir(dir string) error {
	err := os.Mkdir(dir, shareDirMode)
	switch {
	case err == nil:
		return os.Chmod(dir, shareDirMode) // Mkdir applied the umask
	case errors.Is(err, fs.ErrExist):
		return nil
	case errors.Is(err, fs.ErrNotExist) && filepath.Dir(dir) != dir:
		if err := mkdir(filepath.Dir(dir)); err != nil {
			return err
		}
		return mkdir(dir)
	}
	return err
}

// openUp gives the entries this user owns the share modes. Shares made by
// earlier versions used 0770 and 0660, which locked out nodes running as
// other users; each node fixes what it created, and leaves the rest to
// their owners.
func (m *Mailbox) openUp() {
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
		want := fs.FileMode(shareFileMode)
		if d.IsDir() {
			want = shareDirMode
		}
		if fi.Mode().Perm() != want {
			os.Chmod(p, want)
		}
		return nil
	})
}
