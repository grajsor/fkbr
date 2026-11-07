package engine

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// fkbr carries no ACLs or extended attributes. The source only counts the
// files that have some, so a job can say what the copy leaves behind
// instead of dropping it silently.

// hasUncarried reports whether the open file has an extended attribute
// (an ACL is one: system.posix_acl_*; so are file capabilities and user.*
// attributes) other than the SELinux label. The label is left out: every
// file has one on an SELinux host, and the target gives a new file the
// label its own policy says, so it would make every job report it.
// A filesystem without xattrs (NFS, FAT) or an error says no.
func hasUncarried(f *os.File) bool {
	rc, err := f.SyscallConn()
	if err != nil {
		return false
	}
	var names []byte
	rc.Control(func(fd uintptr) {
		buf := make([]byte, 512)
		n, err := unix.Flistxattr(int(fd), buf)
		if err == unix.ERANGE { // a long list: ask for its size
			if n, err = unix.Flistxattr(int(fd), nil); err != nil || n <= 0 {
				return
			}
			buf = make([]byte, n)
			n, err = unix.Flistxattr(int(fd), buf)
		}
		if err == nil && n > 0 {
			names = buf[:n]
		}
	})
	for _, name := range strings.Split(string(names), "\x00") {
		if name != "" && name != "security.selinux" {
			return true
		}
	}
	return false
}

// countUncarried counts f when it has attributes that are not carried.
func (s *sender) countUncarried(f *os.File) {
	if hasUncarried(f) {
		s.uncarried.Add(1)
	}
}
