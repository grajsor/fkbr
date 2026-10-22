package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// ErrNotPrivate: a state directory is not safe to use (see CheckPrivateDir).
var ErrNotPrivate = errors.New("not a private directory")

// PrivateDir creates dir (mode 0700) if it is missing and checks it with
// CheckPrivateDir. The default state dir has a name anyone can guess in
// /tmp: another local user could make it first and so get our lock, log
// and socket into a directory of theirs (security audit 2026-10-22).
func PrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return CheckPrivateDir(dir)
}

// CheckPrivateDir reports whether dir is a directory (not a symlink),
// owned by this user, that nobody else can write to.
func CheckPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symlink; remove it, or pass -state DIR", ErrNotPrivate, dir)
	case !fi.IsDir():
		return fmt.Errorf("%w: %s is not a directory; remove it, or pass -state DIR", ErrNotPrivate, dir)
	case !ok:
		return fmt.Errorf("%w: cannot tell who owns %s", ErrNotPrivate, dir)
	case st.Uid != uint32(os.Geteuid()):
		return fmt.Errorf("%w: %s belongs to uid %d, not to this user (%d): someone else made it; remove it, or pass -state DIR",
			ErrNotPrivate, dir, st.Uid, os.Geteuid())
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%w: others may write to %s (mode %04o); chmod go-w it, or pass -state DIR",
			ErrNotPrivate, dir, fi.Mode().Perm())
	}
	return nil
}
