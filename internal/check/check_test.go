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
	levels := func(c *checker) (warn, info string) {
		var w, i []string
		for _, r := range c.res {
			switch r.Level {
			case Warn:
				w = append(w, r.Msg)
			case Info:
				i = append(i, r.Msg)
			}
		}
		return strings.Join(w, "|"), strings.Join(i, "|")
	}
	// Caching that fkbr compensates for is information; soft stays a warning.
	c := &checker{}
	c.mountOptions(mountinfo.For(ms, "/mnt/transfer/fkbr"))
	warn, info := levels(c)
	if !strings.Contains(warn, "soft") {
		t.Errorf("no warning about soft: %v", warn)
	}
	for _, want := range []string{"acdirmin=30", "lookupcache"} {
		if strings.Contains(warn, want) || !strings.Contains(info, want) {
			t.Errorf("%s should be info while compensated: warn %q info %q", want, warn, info)
		}
	}
	// With the refresh turned off they are warnings again.
	c = &checker{cfg: &config.Config{NFSRefresh: "never"}}
	c.mountOptions(mountinfo.For(ms, "/mnt/transfer/fkbr"))
	warn, _ = levels(c)
	for _, want := range []string{"soft", "acdirmin=30", "lookupcache"} {
		if !strings.Contains(warn, want) {
			t.Errorf("nfs_refresh never: missing warning about %s: %v", want, warn)
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

func TestShareModes(t *testing.T) {
	root := t.TempDir()
	os.Chmod(root, 0o777)
	for _, d := range []string{"inbox/a", "jobs"} {
		os.MkdirAll(filepath.Join(root, d), 0o777)
		os.Chmod(filepath.Join(root, d), 0o777)
	}
	os.Chmod(filepath.Join(root, "inbox"), 0o777)
	c := &checker{}
	c.shareModes(root)
	if len(c.res) != 1 || c.res[0].Level != OK {
		t.Fatalf("open share: %+v", c.res)
	}
	os.Chmod(filepath.Join(root, "inbox", "a"), 0o770)
	c = &checker{}
	c.shareModes(root)
	if len(c.res) != 1 || c.res[0].Level != Warn || !strings.Contains(c.res[0].Msg, "inbox/a") {
		t.Fatalf("closed dir not reported: %+v", c.res)
	}
}

// A state dir on tmpfs loses its records at a reboot: the ssh home node's
// jobs and the own jobs of root local. That is a warning where the swarm
// keeps its records there, information elsewhere, and nothing on disk.
func TestStateDirOnTmpfs(t *testing.T) {
	level := func(cfg *config.Config, fsType int64) (Level, string) {
		c := &checker{cfg: cfg}
		c.stateFS(fsType)
		for _, r := range c.res {
			if r.Area == "state_dir" {
				return r.Level, r.Msg
			}
		}
		return -1, ""
	}
	plain := &config.Config{StateDir: "/tmp/fkbr-x"}
	home := &config.Config{StateDir: "/tmp/fkbr-x", Home: true}
	linked := &config.Config{StateDir: "/tmp/fkbr-x", SSH: []string{"h1"}}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		fs   int64
		want Level
	}{
		{"disk", plain, 0xEF53, OK},
		{"disk, ssh home", home, 0xEF53, OK},
		{"tmpfs, share node", plain, tmpfsMagic, Info},
		{"ramfs, share node", plain, ramfsMagic, Info},
		{"tmpfs, ssh home", home, tmpfsMagic, Warn},
		{"tmpfs, hosts linked", linked, tmpfsMagic, Warn},
	} {
		got, msg := level(tc.cfg, tc.fs)
		if got != tc.want {
			t.Errorf("%s: %s, want %s (%q)", tc.name, got, tc.want, msg)
		}
	}
	if _, msg := level(home, tmpfsMagic); !strings.Contains(msg, "state") || !strings.Contains(msg, "reboot") {
		t.Errorf("the warning should say what is lost at a reboot: %q", msg)
	}
}

// The share is writable by everyone: a symlink where tmp/ or
// protocol-version stands must not take check out of it (it wrote, read
// and deleted files there, and reported "writable").
func TestShareCheckStaysInTheShare(t *testing.T) {
	base := t.TempDir()
	share, outside := filepath.Join(base, "share"), filepath.Join(base, "outside")
	for _, d := range []string{share, outside} {
		os.Mkdir(d, 0o755)
	}
	secret := filepath.Join(outside, "secret")
	os.WriteFile(secret, []byte("99\n"), 0o600)
	keep := filepath.Join(outside, ".check-n1")
	os.WriteFile(keep, []byte("mine"), 0o600)
	if err := os.Symlink(outside, filepath.Join(share, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(share, "protocol-version")); err != nil {
		t.Fatal(err)
	}
	c := &checker{cfg: &config.Config{NodeID: "n1", NFSRoot: share}}
	c.share(nil)
	if b, err := os.ReadFile(keep); err != nil || string(b) != "mine" {
		t.Errorf("a file outside the share was touched: %q, %v", b, err)
	}
	des, _ := os.ReadDir(outside)
	if len(des) != 2 {
		t.Errorf("files appeared outside the share: %v", des)
	}
	for _, r := range c.res {
		if r.Level == OK && strings.Contains(r.Msg, "writable") {
			t.Errorf("reported writable through a link: %+v", c.res)
		}
		if strings.Contains(r.Msg, "99") {
			t.Errorf("printed a file from outside the share: %+v", r)
		}
	}
}

// A healthy share still passes, and check leaves nothing behind.
func TestShareCheckWritable(t *testing.T) {
	share := t.TempDir()
	c := &checker{cfg: &config.Config{NodeID: "n1", NFSRoot: share}}
	c.share(nil)
	ok := false
	for _, r := range c.res {
		if r.Level == Fail {
			t.Errorf("fail on a healthy share: %+v", r)
		}
		ok = ok || strings.Contains(r.Msg, "atomic rename-over works")
	}
	if !ok {
		t.Errorf("no writable result: %+v", c.res)
	}
	if des, _ := os.ReadDir(filepath.Join(share, "tmp")); len(des) != 0 {
		t.Errorf("left behind: %v", des)
	}
}
