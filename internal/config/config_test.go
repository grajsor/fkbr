package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"fkbr/internal/product"
)

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`
node_id: rhel9-a
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
