package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/product"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`
node_id: host1
nfs_root: /mnt/share/fkbr
state_dir: /srv/fkbr-state
roots:
  - id: data
    path: /srv/data
    allow_write: true
poll:
  active: 100ms
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll.Active != 100*time.Millisecond || c.Poll.Idle != time.Second || c.OfflineAfter != 30*time.Second {
		t.Fatalf("unexpected durations: %+v offline=%s", c.Poll, c.OfflineAfter)
	}
	if len(c.Roots) != 1 || !c.Roots[0].AllowWrite {
		t.Fatalf("roots: %+v", c.Roots)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":    "node_id: a\nnfs_root: /x\nstate_dir: /s\nbogus: 1\n",
		"bad node id":    "node_id: A/B\nnfs_root: /x\nstate_dir: /s\n",
		"relative nfs":   "node_id: a\nnfs_root: x\nstate_dir: /s\n",
		"duplicate root": "node_id: a\nnfs_root: /x\nstate_dir: /s\nroots:\n- {id: r, path: /a}\n- {id: r, path: /b}\n",
		"relative root":  "node_id: a\nnfs_root: /x\nstate_dir: /s\nroots:\n- {id: r, path: a}\n",
		"offline<=hb":    "node_id: a\nnfs_root: /x\nstate_dir: /s\nheartbeat_interval: 10s\noffline_after: 5s\n",
		"role dest":      "node_id: a\nnfs_root: /x\nstate_dir: /s\nroles: [dest]\n",
		"role all":       "node_id: a\nnfs_root: /x\nstate_dir: /s\nroles: [all]\n",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.HasPrefix(err.Error(), "config:") {
			t.Errorf("%s: error not prefixed: %v", name, err)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	ok := map[string]ByteSize{"1024": 1024, "512MiB": 512 << 20, "2GiB": 2 << 30, "10G": 10 << 30, "64K": 64 << 10, "3 MB": 3 << 20}
	for in, want := range ok {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1", "5X"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q): expected error", in)
		}
	}
	c, err := Parse([]byte("node_id: a\nnfs_root: /x\nstate_dir: /s\ntransfer:\n  chunk_size: 64MiB\n"))
	if err != nil || c.Transfer.ChunkSize != 64<<20 || c.Transfer.JobQuota != 1<<30 {
		t.Fatalf("transfer config: %+v, %v", c.Transfer, err)
	}
}

func TestSocketDefaults(t *testing.T) {
	c, err := Parse([]byte("node_id: a\nnfs_root: /x\nstate_dir: /srv/fkbr-state\nsocket_mode: 0660\n"))
	if err != nil || c.Socket != "/srv/fkbr-state/"+product.Name+".sock" || c.SocketMode != 0o660 {
		t.Fatalf("socket: %q %o %v", c.Socket, c.SocketMode, err)
	}
	if _, err := Parse([]byte("node_id: a\nnfs_root: /x\nstate_dir: /s\nsocket_mode: 999\n")); err == nil {
		t.Fatal("bad mode accepted")
	}
}

func TestDefaultsWithoutFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000") // must be ignored
	t.Setenv("TMPDIR", "/tmp")
	c := &Config{NodeID: "n1", NFSRoot: "/mnt/x"}
	r, err := ParseRoot("data=/srv/data")
	if err != nil || !r.AllowWrite {
		t.Fatalf("ParseRoot: %+v %v", r, err)
	}
	ro, _ := ParseRoot("logs=/var/log:ro")
	if ro.AllowWrite || ro.Path != "/var/log" {
		t.Fatalf("read-only root: %+v", ro)
	}
	c.Roots = []Root{r, ro}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/%s-%d-n1", product.Name, os.Getuid())
	if c.StateDir != want || c.Socket != want+"/"+product.Name+".sock" {
		t.Fatalf("state %q socket %q", c.StateDir, c.Socket)
	}
	if _, err := ParseRoot("nopath"); err == nil {
		t.Error("bad root accepted")
	}
}

// A home node passes its chunk size and quotas to the hosts it links, but
// only those it changed (an older fkbr there knows no such flags).
func TestLinkArgs(t *testing.T) {
	c := &Config{NodeID: "home", Home: true, StateDir: t.TempDir(), Roots: []Root{{ID: "files", Path: t.TempDir()}}}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	if a := c.LinkArgs(); len(a) != 0 {
		t.Errorf("defaults passed: %q", a)
	}
	if err := c.SetTransfer("", "256M", ""); err != nil {
		t.Fatal(err)
	}
	a := c.LinkArgs()
	if strings.Join(a, " ") != "-job-quota 268435456" {
		t.Fatalf("args %q", a)
	}
	var l Config
	if err := l.SetTransfer("", a[1], ""); err != nil || l.Transfer.JobQuota != 256<<20 {
		t.Errorf("read back: %d %v", l.Transfer.JobQuota, err)
	}
	if err := l.SetTransfer("lots", "", ""); err == nil {
		t.Error("a bad size accepted")
	}
}

// fakeMemInfo makes MemInfo read a meminfo saying total bytes of RAM.
func fakeMemInfo(t *testing.T, total int64) {
	p := filepath.Join(t.TempDir(), "meminfo")
	os.WriteFile(p, []byte(fmt.Sprintf("MemTotal:       %d kB\nMemFree:          1000 kB\nMemAvailable:   %d kB\n", total>>10, total>>11)), 0o644)
	old := MemInfoPath
	MemInfoPath = p
	t.Cleanup(func() { MemInfoPath = old })
}

func TestMemInfo(t *testing.T) {
	fakeMemInfo(t, 1740<<20)
	if n, err := MemInfo("MemTotal"); err != nil || n != 1740<<20 {
		t.Errorf("MemTotal: %d %v", n, err)
	}
	if n, err := MemInfo("MemAvailable"); err != nil || n != 870<<20 {
		t.Errorf("MemAvailable: %d %v", n, err)
	}
	if _, err := MemInfo("Mem"); err == nil {
		t.Error("a field that is not there: no error")
	}
}

// Over ssh chunks are staged in memory: smaller and fewer, on the home
// node and its linked hosts alike, so they need no flags for it; the
// staging quota is sized to each host's RAM.
func TestSSHTransferDefaults(t *testing.T) {
	fakeMemInfo(t, 1740<<20)
	for _, c := range []struct {
		cfg                 Config
		chunk, job, staging ByteSize
		description         string
	}{
		{Config{NFSRoot: t.TempDir()}, DefaultChunkSize, DefaultJobQuota, DefaultStagingQuota, "share"},
		{Config{Home: true}, SSHChunkSize, SSHJobQuota, 435 << 20, "home"},
		{Config{Link: true}, SSHChunkSize, SSHJobQuota, 435 << 20, "link"},
		{Config{Home: true, Transfer: Transfer{ChunkSize: 256 << 20}}, 256 << 20, 256 << 20, 435 << 20, "home, bigger chunks"},
		{Config{Home: true, Transfer: Transfer{JobQuota: 1 << 30}}, SSHChunkSize, 1 << 30, 1 << 30, "home, a bigger job quota"},
	} {
		cfg := c.cfg
		cfg.NodeID, cfg.StateDir, cfg.Roots = "n", t.TempDir(), []Root{{ID: "files", Path: t.TempDir()}}
		if err := cfg.Finish(); err != nil {
			t.Fatalf("%s: %v", c.description, err)
		}
		if cfg.Transfer.ChunkSize != c.chunk || cfg.Transfer.JobQuota != c.job || cfg.Transfer.StagingQuota != c.staging {
			t.Errorf("%s: chunk %d job %d staging %d, want %d %d %d", c.description,
				cfg.Transfer.ChunkSize, cfg.Transfer.JobQuota, cfg.Transfer.StagingQuota, c.chunk, c.job, c.staging)
		}
		if cfg.Home && c.chunk == SSHChunkSize && c.job == SSHJobQuota && len(cfg.LinkArgs()) != 0 {
			t.Errorf("%s: link args %q", c.description, cfg.LinkArgs())
		}
	}
}

// A linked node asks the home node for requests again at once, but its
// jobs' waits keep the usual pace: each look there is a round trip.
func TestLinkPolls(t *testing.T) {
	c := &Config{NodeID: "web01", Link: true, StateDir: t.TempDir(), Roots: []Root{{ID: "files", Path: t.TempDir()}}}
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	if c.Poll.Fetch != 10*time.Millisecond || c.Poll.Active != 200*time.Millisecond {
		t.Errorf("fetch %s, active %s", c.Poll.Fetch, c.Poll.Active)
	}
}

func TestDefaultNodeAndRoot(t *testing.T) {
	id, err := DefaultNodeID()
	if err != nil || !ValidID(id) {
		t.Fatalf("DefaultNodeID = %q, %v", id, err)
	}
	r, err := DirRoot("/srv/data", "/mnt/share/x", true)
	if err != nil || r.ID != DefaultRootID || r.Path != "/srv/data" || !r.AllowWrite {
		t.Fatalf("DirRoot = %+v, %v", r, err)
	}
	for _, bad := range [][2]string{{"/", "/mnt/s"}, {"/mnt/s/sub", "/mnt/s"}, {"/mnt/s", "/mnt/s"}} {
		if _, err := DirRoot(bad[0], bad[1], true); err == nil {
			t.Errorf("DirRoot(%q, share %q) accepted", bad[0], bad[1])
		}
	}
}

// The share may sit inside a shared directory (cd ~ && fkbr ~/box): that
// root then hides it. A root inside the share is refused, also when it is
// reached through a symlink.
func TestShareAndRoots(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	share := filepath.Join(home, "box")
	if err := os.MkdirAll(filepath.Join(share, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink(filepath.Join(share, "sub"), filepath.Join(base, "lnk"))
	os.Symlink(home, filepath.Join(base, "homelink"))
	c := &Config{NodeID: "n1", NFSRoot: share, Roots: []Root{{ID: "files", Path: filepath.Join(base, "homelink"), AllowWrite: true}}}
	if err := c.Finish(); err != nil || c.Roots[0].Hide != "box" {
		t.Fatalf("share inside the root: hide %q, %v", c.Roots[0].Hide, err)
	}
	for _, p := range []string{share, filepath.Join(share, "sub"), filepath.Join(base, "lnk")} {
		c := &Config{NodeID: "n1", NFSRoot: share, Roots: []Root{{ID: "files", Path: p}}}
		if err := c.Finish(); err == nil {
			t.Errorf("root %s inside the share accepted", p)
		}
	}
	c = &Config{NodeID: "n1", NFSRoot: share, Roots: []Root{{ID: "files", Path: filepath.Join(base, "elsewhere")}}}
	if err := c.Finish(); err != nil || c.Roots[0].Hide != "" {
		t.Fatalf("unrelated root: hide %q, %v", c.Roots[0].Hide, err)
	}
}

func TestNFSAndSSHBothRefused(t *testing.T) {
	if _, err := Parse([]byte("nfs_root: /mnt/nfs/xfer\nssh: [web01]\n")); err == nil {
		t.Error("a config with nfs_root and ssh was accepted")
	}
}

// A config file that does not parse is named in the error: the one in
// the home directory is read without being asked for.
func TestBadFileNamed(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".fkbr.conf")
	os.WriteFile(p, []byte("ssh: [web01\n"), 0o600)
	if _, err := Read(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Errorf("err %v", err)
	}
}
