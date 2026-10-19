package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/config"
	"fkbr/internal/mountinfo"
)

const nfsMountinfo = `22 1 8:2 / / rw,relatime shared:1 - ext4 /dev/sda2 rw
95 22 0:50 / /mnt/transfer rw,relatime shared:60 - nfs4 srv:/export rw,vers=4.2,rsize=1048576,soft,acregmin=3,acregmax=60,acdirmin=30,acdirmax=60,hard,proto=tcp
96 22 0:51 / /mnt/good\040dir rw,relatime - nfs4 srv:/x rw,vers=4.1,hard,acdirmin=1,lookupcache=pos
`

func TestMountOptions(t *testing.T) {
	ms := mountinfo.Parse(strings.NewReader(nfsMountinfo))
	if m := mountinfo.For(ms, "/mnt/transfer/fkbr"); m == nil || m.FSType != "nfs4" {
		t.Fatalf("mountFor: %+v", m)
	}
	if m := mountinfo.For(ms, "/mnt/good dir/fkbr"); m == nil || m.Opts["lookupcache"] != "pos" {
		t.Fatalf("escaped mount point: %+v", m)
	}
	if m := mountinfo.For(ms, "/home/x"); m == nil || m.Point != "/" {
		t.Fatalf("fallback to /: %+v", m)
	}
	c := &checker{}
	c.mountOptions(mountinfo.For(ms, "/mnt/transfer/fkbr"))
	var warns []string
	for _, r := range c.res {
		if r.Level == Warn {
			warns = append(warns, r.Msg)
		}
	}
	all := strings.Join(warns, "|")
	for _, want := range []string{"soft", "acdirmin=30", "lookupcache"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing warning about %s: %v", want, warns)
		}
	}
	c = &checker{}
	c.mountOptions(mountinfo.For(ms, "/mnt/good dir/x"))
	for _, r := range c.res {
		if r.Level >= Warn {
			t.Errorf("good mount warned: %s", r.Msg)
		}
	}
}

func TestRunLocal(t *testing.T) {
	base := t.TempDir()
	share, root, ro := filepath.Join(base, "share"), filepath.Join(base, "data"), filepath.Join(base, "ro")
	for _, d := range []string{share, root, ro} {
		os.Mkdir(d, 0o755)
	}
	cfg := &config.Config{
		NodeID: "n1", NFSRoot: share, StateDir: filepath.Join(base, "state"), Socket: filepath.Join(base, "s.sock"),
		OfflineAfter: 30 * time.Second,
		Roots:        []config.Root{{ID: "data", Path: root, AllowWrite: true}, {ID: "ro", Path: ro}, {ID: "gone", Path: filepath.Join(base, "missing")}},
	}
	res := Run(context.Background(), cfg, strings.NewReader("22 1 8:2 / / rw - ext4 /dev/sda2 rw\n"))
	byArea := map[string]Level{}
	for _, r := range res {
		if r.Level > byArea[r.Area] {
			byArea[r.Area] = r.Level
		}
	}
	want := map[string]Level{"root data": OK, "root ro": OK, "root gone": Fail, "share": Info, "mount": Warn, "state_dir": OK, "registry": Warn}
	for area, lvl := range want {
		if byArea[area] != lvl {
			t.Errorf("%s: got %s want %s (%+v)", area, byArea[area], lvl, res)
		}
	}
}
