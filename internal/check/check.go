// Package check validates a node's environment before (and while) running
// fkbr: configuration, local roots, the shared NFS mount and its options,
// atomic rename on the share, clock skew and the node registry.
package check

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/atomicfile"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
	"fkbr/internal/mountinfo"
	"fkbr/internal/product"
	"fkbr/internal/transport/nfsmailbox"
)

type Level int

const (
	OK Level = iota
	Info
	Warn
	Fail
)

func (l Level) String() string { return [...]string{"OK", "INFO", "WARN", "FAIL"}[l] }

type Result struct {
	Level Level
	Area  string
	Msg   string
}

type checker struct {
	cfg *config.Config
	res []Result
}

func (c *checker) add(l Level, area, format string, args ...any) {
	c.res = append(c.res, Result{l, area, fmt.Sprintf(format, args...)})
}

// Run performs all checks. mountinfo is /proc/self/mountinfo (injectable
// for tests).
func Run(ctx context.Context, cfg *config.Config, mountInfo io.Reader) []Result {
	c := &checker{cfg: cfg}
	c.add(OK, "config", "node %q, %d roots, share %s", cfg.NodeID, len(cfg.Roots), cfg.NFSRoot)
	c.roots()
	c.stateDir()
	mounts := mountinfo.Parse(mountInfo)
	c.share(mounts)
	c.registry(ctx)
	c.capabilities()
	c.socket()
	return c.res
}

func (c *checker) roots() {
	for _, r := range c.cfg.Roots {
		fi, err := os.Stat(r.Path)
		if err != nil || !fi.IsDir() {
			c.add(Fail, "root "+r.ID, "%s: not a usable directory: %v", r.Path, err)
			continue
		}
		if _, err := os.ReadDir(r.Path); err != nil {
			c.add(Fail, "root "+r.ID, "%s: cannot list: %v", r.Path, err)
			continue
		}
		if !r.AllowWrite {
			c.add(OK, "root "+r.ID, "%s readable (read-only)", r.Path)
			continue
		}
		f, err := os.CreateTemp(r.Path, "."+product.Name+"-check-*")
		if err != nil {
			c.add(Fail, "root "+r.ID, "%s: allow_write is set but writing fails: %v", r.Path, err)
			continue
		}
		f.Close()
		os.Remove(f.Name())
		c.add(OK, "root "+r.ID, "%s readable and writable", r.Path)
	}
}

const nfsSuperMagic = 0x6969

func (c *checker) stateDir() {
	if err := os.MkdirAll(c.cfg.StateDir, 0o700); err != nil {
		c.add(Fail, "state_dir", "%s: %v", c.cfg.StateDir, err)
		return
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(c.cfg.StateDir, &st); err == nil && st.Type == nfsSuperMagic {
		c.add(Fail, "state_dir", "%s is on NFS; it must be local (worker lock, socket)", c.cfg.StateDir)
		return
	}
	c.add(OK, "state_dir", "%s on local storage", c.cfg.StateDir)
}

func (c *checker) share(mounts []mountinfo.Mount) {
	root, err := filepath.EvalSymlinks(c.cfg.NFSRoot)
	if err != nil {
		c.add(Fail, "share", "%s: %v (is the NFS export mounted?)", c.cfg.NFSRoot, err)
		return
	}
	if m := mountinfo.For(mounts, root); m != nil {
		c.mountOptions(m)
	}
	// Atomic publish: write, rename over an existing name, read back.
	tmp := filepath.Join(root, "tmp")
	os.MkdirAll(tmp, 0o770)
	dest := filepath.Join(tmp, ".check-"+c.cfg.NodeID)
	defer os.Remove(dest)
	for i, content := range []string{"first", "second"} {
		if err := atomicfile.Write(tmp, dest, []byte(content), 0o660); err != nil {
			c.add(Fail, "share", "cannot write to %s: %v", tmp, err)
			return
		}
		if got, err := os.ReadFile(dest); err != nil || string(got) != content {
			c.add(Fail, "share", "rename-over not visible (write %d read %q, %v)", i, got, err)
			return
		}
	}
	c.add(OK, "share", "writable; atomic rename-over works")

	if b, err := os.ReadFile(filepath.Join(root, "protocol-version")); err == nil {
		if v := strings.TrimSpace(string(b)); v != strconv.Itoa(api.ProtocolVersion) {
			c.add(Fail, "share", "share uses protocol %s, this binary speaks %d", v, api.ProtocolVersion)
		} else {
			c.add(OK, "share", "protocol version %s", v)
		}
	} else {
		c.add(Info, "share", "no protocol-version yet (the first daemon creates it)")
	}

	// Clock skew against the server: fkbr does not depend on it (liveness
	// uses server mtimes), but skew confuses logs and job timestamps.
	f, err := os.CreateTemp(tmp, ".skew-*")
	if err == nil {
		f.Write([]byte("x"))
		fi, _ := f.Stat()
		f.Close()
		os.Remove(f.Name())
		skew := time.Since(fi.ModTime()).Round(time.Second)
		lvl := OK
		if skew > time.Minute || skew < -time.Minute {
			lvl = Warn
		}
		c.add(lvl, "clock", "local clock vs NFS server: %s (%s tolerates skew; keep NTP running for sane timestamps)", skew, product.Name)
	}
}

func (c *checker) mountOptions(m *mountinfo.Mount) {
	if !strings.HasPrefix(m.FSType, "nfs") {
		c.add(Warn, "mount", "%s is %s, not NFS (fine for testing on one machine)", m.Point, m.FSType)
		return
	}
	c.add(OK, "mount", "%s is %s (vers=%s)", m.Point, m.FSType, m.Opts["vers"])
	if _, soft := m.Opts["soft"]; soft {
		c.add(Warn, "mount", "mounted soft: I/O can fail silently on server hiccups; use hard")
	}
	if v, ok := m.Opts["acdirmin"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 3 {
			c.add(Warn, "mount", "acdirmin=%d: new requests may take up to %ds to be noticed; mount with actimeo=1", n, n)
		}
	}
	if v := m.Opts["lookupcache"]; v != "pos" && v != "positive" && v != "none" {
		c.add(Warn, "mount", "lookupcache not set to positive: replies may be seen late; add lookupcache=positive")
	}
	if m.CachesAttributes() {
		c.add(Info, "mount", product.Name+" compensates by refreshing directories before reading them (nfs_refresh: auto); the mount options above would still be faster")
	}
}

func (c *checker) registry(ctx context.Context) {
	mb, err := nfsmailbox.Open(c.cfg.NFSRoot, c.cfg.NodeID, nfsmailbox.Options{OfflineAfter: c.cfg.OfflineAfter, Refresh: c.cfg.RefreshNFS()})
	if err != nil {
		c.add(Fail, "registry", "%v", err)
		return
	}
	nodes, err := mb.Nodes(ctx)
	if err != nil {
		c.add(Fail, "registry", "%v", err)
		return
	}
	host, _ := os.Hostname()
	var others []string
	self := false
	for _, n := range nodes {
		if n.Self {
			self = true
			switch {
			case n.Online && n.Hostname != host:
				c.add(Fail, "registry", "node_id %q is in use by host %s (online); node IDs must be unique", n.ID, n.Hostname)
			case n.Online:
				c.add(OK, "registry", "this node is registered and its daemon is heartbeating")
			default:
				c.add(Warn, "registry", "%s", product.Expand("this node's daemon is not heartbeating (start {name} tui or {name} daemon)"))
			}
			continue
		}
		state := "offline"
		if n.Online {
			state = "online"
		}
		if n.Protocol != api.ProtocolVersion {
			c.add(Warn, "registry", "node %s speaks protocol %d", n.ID, n.Protocol)
		}
		others = append(others, n.ID+" "+state)
	}
	if !self {
		c.add(Warn, "registry", "%s", product.Expand("this node is not registered yet (start {name} tui or {name} daemon)"))
	}
	if len(others) == 0 {
		c.add(Info, "registry", "no other nodes registered yet")
	} else {
		c.add(Info, "registry", "peers: %s", strings.Join(others, ", "))
	}
}

func (c *checker) capabilities() {
	chown := localfs.ProbeChown(c.cfg.StateDir)
	dac := localfs.ProbeDACOverride(c.cfg.StateDir)
	switch {
	case chown && dac:
		c.add(OK, "privileges", "ownership fully preserved on received data")
	case chown:
		c.add(Info, "privileges", "file ownership preserved; directories stay the daemon user's (add CAP_DAC_OVERRIDE for full)")
	default:
		c.add(Info, "privileges", "received data is owned by the daemon user (CAP_CHOWN preserves ownership)")
	}
}

func (c *checker) socket() {
	conn, err := net.DialTimeout("unix", c.cfg.Socket, time.Second)
	if err != nil {
		c.add(Info, "socket", "%s not reachable: commands will run in-process", c.cfg.Socket)
		return
	}
	conn.Close()
	c.add(OK, "socket", "daemon API reachable at %s", c.cfg.Socket)
}
