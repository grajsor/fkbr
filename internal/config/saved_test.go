package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSavedHosts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fkbr.yaml")
	if hs, err := SavedHosts(p); err != nil || hs != nil {
		t.Fatalf("no file: %v %v", hs, err)
	}
	for _, h := range []string{"web01:/srv/www", "db01", "web01:/srv/www"} {
		if err := SaveHost(p, h); err != nil {
			t.Fatal(err)
		}
	}
	if hs, _ := SavedHosts(p); !slices.Equal(hs, []string{"web01:/srv/www", "db01"}) {
		t.Fatalf("saved: %v", hs)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// A comment and another key the owner added stay, and so does the
	// file's mode.
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte("# mine\nnode_id: here\n"+string(b)), 0o640)
	os.Chmod(p, 0o640)
	if err := ForgetHost(p, "web01:/srv/www"); err != nil {
		t.Fatal(err)
	}
	if hs, _ := SavedHosts(p); !slices.Equal(hs, []string{"db01"}) {
		t.Fatalf("after forget: %v", hs)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", fi.Mode().Perm())
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "# mine") || !strings.Contains(string(b), "node_id: here") {
		t.Errorf("lost the rest of the file:\n%s", b)
	}
	// Never written through a symlink.
	os.Remove(p)
	other := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(other, []byte("keep: me\n"), 0o600)
	os.Symlink(other, p)
	if err := SaveHost(p, "x"); err == nil {
		t.Error("wrote through a symlink")
	}
	if b, _ := os.ReadFile(other); string(b) != "keep: me\n" {
		t.Errorf("victim changed: %q", b)
	}
}

// A host linked by `fkbr ssh:web01` is not saved into a config file that
// names a share: the file could not be read any more.
func TestSaveHostBesideShare(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fkbr.yaml")
	os.WriteFile(p, []byte("nfs_root: /mnt/nfs\n"), 0o600)
	if err := SaveHost(p, "web01"); err == nil {
		t.Error("saved beside nfs_root")
	}
	if _, err := Read(p); err != nil {
		t.Errorf("the file no longer reads: %v", err)
	}
}

// The config file, also where linked hosts are kept: -config, else
// $FKBR_CONFIG, else fkbr.yaml beside the binary if it is there, else
// ~/.fkbr.conf.
func TestConfigPathOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("FKBR_CONFIG", "")
	dot := filepath.Join(home, ".fkbr.conf")
	if p := WritePath(""); p != dot {
		t.Errorf("nothing there: write %q, want %q", p, dot)
	}
	if p := DefaultPath(); p != "" {
		t.Errorf("nothing there: read %q", p)
	}
	os.WriteFile(dot, []byte("ssh: [web01]\n"), 0o600)
	if p := DefaultPath(); p != dot {
		t.Errorf("read %q, want %q", p, dot)
	}
	exe, _ := os.Executable()
	beside := filepath.Join(filepath.Dir(exe), "fkbr.yaml")
	if err := os.WriteFile(beside, []byte("{}\n"), 0o600); err != nil {
		t.Skip("cannot write beside the test binary:", err)
	}
	defer os.Remove(beside)
	if p, q := WritePath(""), DefaultPath(); p != beside || q != beside {
		t.Errorf("beside the binary: write %q, read %q, want %q", p, q, beside)
	}
	t.Setenv("FKBR_CONFIG", "/etc/elsewhere.yaml")
	if p, q := WritePath(""), DefaultPath(); p != "/etc/elsewhere.yaml" || q != p {
		t.Errorf("$FKBR_CONFIG: write %q, read %q", p, q)
	}
	if p := WritePath("/given.yaml"); p != "/given.yaml" {
		t.Errorf("-config: %q", p)
	}
}
