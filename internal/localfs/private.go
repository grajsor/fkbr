package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	why, fix, err := privateDir(dir)
	if why == "" {
		return err
	}
	if fix != "" {
		fix += ", or "
	}
	return fmt.Errorf("%w: %s; %spass -state DIR", ErrNotPrivate, why, fix)
}

// CheckSocketDir is CheckPrivateDir for the folder of the API socket: a
// socket where others may put their own is not surely our node's. The
// advice is to move the socket, not the state dir.
func CheckSocketDir(sock string) error {
	why, _, err := privateDir(filepath.Dir(sock))
	if why == "" {
		return err
	}
	return fmt.Errorf("%w: socket %s: %s; set socket: to a path in a folder of yours (the default is in the state dir)",
		ErrNotPrivate, sock, why)
}

// privateDir says why dir is not private ("" if it is, or if it cannot be
// looked at: err) and how to fix it.
func privateDir(dir string) (why, fix string, err error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return dir + " is a symlink", "remove it", nil
	case !fi.IsDir():
		return dir + " is not a directory", "remove it", nil
	case !ok:
		return "cannot tell who owns " + dir, "", nil
	case st.Uid != uint32(os.Geteuid()):
		return fmt.Sprintf("%s belongs to uid %d, not to this user (%d): someone else made it", dir, st.Uid, os.Geteuid()), "remove it", nil
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Sprintf("others may write to %s (mode %04o)", dir, fi.Mode().Perm()), "chmod go-w it", nil
	}
	return "", "", nil
}
