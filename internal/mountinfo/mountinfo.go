// Package mountinfo reads /proc/self/mountinfo, to find which filesystem
// and mount options a path is on.
package mountinfo

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Mount struct {
	Point  string
	FSType string
	Opts   map[string]string
}

// Parse reads mountinfo lines.
func Parse(r io.Reader) []Mount {
	var out []Mount
	if r == nil {
		return nil
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i, x := range f {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(f) < sep+4 {
			continue
		}
		m := Mount{Point: unescape(f[4]), FSType: f[sep+1], Opts: map[string]string{}}
		for _, o := range strings.Split(f[5]+","+f[sep+3], ",") {
			k, v, _ := strings.Cut(o, "=")
			m.Opts[k] = v
		}
		out = append(out, m)
	}
	return out
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// For returns the mount containing path (longest mount point prefix).
func For(mounts []Mount, path string) *Mount {
	var best *Mount
	for i := range mounts {
		m := &mounts[i]
		if path == m.Point || strings.HasPrefix(path, strings.TrimSuffix(m.Point, "/")+"/") || m.Point == "/" {
			// >=: of mounts at the same point the later one is on top
			// (autofs lists its trigger first, then the real mount).
			if best == nil || len(m.Point) >= len(best.Point) {
				best = m
			}
		}
	}
	return best
}

// Of returns the mount of path on this system (symlinks resolved), or nil.
func Of(path string) *Mount {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	if p, err := filepath.EvalSymlinks(path); err == nil {
		path = p
	}
	return For(Parse(f), path)
}

func (m *Mount) IsNFS() bool { return strings.HasPrefix(m.FSType, "nfs") }

// CachesAttributes reports whether an NFS mount caches attributes or
// lookups long enough that other clients' changes can stay invisible:
// attribute timeouts above 3 s, or negative lookups cached (lookupcache
// other than positive/none).
func (m *Mount) CachesAttributes() bool {
	if !m.IsNFS() {
		return false
	}
	if _, ok := m.Opts["noac"]; ok {
		return false
	}
	for _, k := range []string{"acdirmin", "acregmin"} {
		if n, err := strconv.Atoi(m.Opts[k]); err == nil && n > 3 {
			return true
		}
	}
	switch m.Opts["lookupcache"] {
	case "pos", "positive", "none":
		return false
	}
	return true
}
