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
	// A comment and another key added by hand stay, and so does the
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

// A config file of comments only keeps them when a host is saved.
func TestSaveHostKeepsComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fkbr.yaml")
	os.WriteFile(p, []byte("# mine\n# and this\n"), 0o600)
	if err := SaveHost(p, "web01"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# mine\n# and this") {
		t.Errorf("comments lost:\n%s", b)
	}
	if hs, _ := SavedHosts(p); !slices.Equal(hs, []string{"web01"}) {
		t.Errorf("saved: %v", hs)
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

// The config file, also where settings and linked hosts are kept:
// -config, else $FKBR_CONFIG, else fkbr.conf beside the binary if it is
// there (or fkbr.yaml, its older name), else ~/.fkbr.conf.
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
	old := filepath.Join(filepath.Dir(exe), "fkbr.yaml")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Skip("cannot write beside the test binary:", err)
	}
	defer os.Remove(old)
	if p, q := WritePath(""), DefaultPath(); p != old || q != old {
		t.Errorf("fkbr.yaml beside the binary: write %q, read %q, want %q", p, q, old)
	}
	beside := filepath.Join(filepath.Dir(exe), "fkbr.conf")
	if err := os.WriteFile(beside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
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

// The settings dialog changes keys one by one: comments, the ssh list
// and other keys stay; a key set back to its default goes, and so does a
// section left empty.
func TestSaveSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fkbr.yaml")
	orig := "# my notes\nssh: [web01]\ntransfer:\n  # two at most\n  max_jobs: 2\nroles: [web]\n"
	if err := os.WriteFile(p, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	err := SaveSettings(p, []Setting{
		{"transfer.max_jobs", 4}, {"transfer.chunk_size", "64MiB"}, {"tui.colors", "256"},
		{"roles", []string{"web", "edge"}}, {"tui.mouse", false},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, want := range []string{"# my notes", "ssh: [web01]", "# two at most", "max_jobs: 4", "chunk_size: 64MiB", "colors: \"256\"", "roles: [web, edge]", "mouse: false"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %q in:\n%s", want, b)
		}
	}
	c, err := Read(p)
	if err != nil || c.Transfer.MaxJobs != 4 || c.Transfer.ChunkSize != 64<<20 || c.TUI.Colors != "256" || *c.TUI.Mouse {
		t.Fatalf("read back: %+v %v", c, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	// Back to the defaults: the keys go, and the empty tui section.
	if err := SaveSettings(p, []Setting{{"tui.colors", nil}, {"tui.mouse", nil}, {"transfer.chunk_size", nil}}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "tui") || strings.Contains(string(b), "chunk_size") || !strings.Contains(string(b), "max_jobs: 4") {
		t.Errorf("after removing:\n%s", b)
	}
}

// What a node could not start with is refused, and the file stays.
func TestSaveSettingsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fkbr.yaml")
	os.WriteFile(p, []byte("ssh: [web01]\n"), 0o600)
	for _, s := range []Setting{
		{"transfer.max_jobs", -1}, {"transfer.job_quota", "1MiB"}, // below chunk_size
		{"nfs_root", "/mnt/x"}, // with an ssh list
		{"tui.colors", "pink"}, {"tui.if_exists", "maybe"}, {"tui.exclude", "a["},
		{"node_id", "Bad Name"}, {"roles", []string{"all"}}, {"local_root", "relative"},
		{"sync.max_deletes", -2}, {"nonsense", 1},
	} {
		if err := SaveSettings(p, []Setting{s}); err == nil {
			t.Errorf("%s = %v accepted", s.Key, s.Value)
		}
	}
	if b, _ := os.ReadFile(p); string(b) != "ssh: [web01]\n" {
		t.Errorf("file changed: %q", b)
	}
	// A new file gets a comment saying what it is, and mode 0600.
	q := filepath.Join(t.TempDir(), "new.conf")
	if err := SaveSettings(q, []Setting{{"transfer.max_jobs", 3}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(q)
	if fi, _ := os.Stat(q); !strings.HasPrefix(string(b), "# fkbr config") || fi.Mode().Perm() != 0o600 {
		t.Errorf("new file %v:\n%s", fi.Mode().Perm(), b)
	}
}

func TestByteSizeString(t *testing.T) {
	for v, want := range map[ByteSize]string{128 << 20: "128MiB", 1 << 30: "1GiB", 1500 << 10: "1500KiB", 1000: "1000", 0: "0"} {
		if got := v.String(); got != want {
			t.Errorf("%d: %q, want %q", int64(v), got, want)
		}
		if back, err := ParseByteSize(v.String()); err != nil || back != v {
			t.Errorf("%s reads back as %d (%v)", v, back, err)
		}
	}
}
