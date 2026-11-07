package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	return m
}

func TestWritePublishesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "x.json")
	for _, content := range []string{"first", "second, longer"} {
		if err := Write(dir, dest, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != content {
			t.Fatalf("read %q, %v; want %q", got, err, content)
		}
		fi, _ := os.Stat(dest)
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("mode %v, want 0640 whatever the umask", fi.Mode().Perm())
		}
		if l := leftovers(t, dir); len(l) != 0 {
			t.Errorf("temp files left: %v", l)
		}
	}
}

// A failed write leaves the old file as it was and no temp file behind.
func TestWriteFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "x")
	os.WriteFile(dest, []byte("old"), 0o644)

	// dest is a directory: the rename fails after the temp file was written.
	d := filepath.Join(dir, "d")
	os.Mkdir(d, 0o755)
	os.WriteFile(filepath.Join(d, "keep"), []byte("k"), 0o644)
	if err := Write(dir, d, []byte("new"), 0o644); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp file left after a failed rename: %v", l)
	}

	// the temp dir is missing: nothing can be created, dest is untouched.
	if err := Write(filepath.Join(dir, "missing"), dest, []byte("new"), 0o644); err == nil {
		t.Fatal("a missing temp directory was accepted")
	}
	if b, _ := os.ReadFile(dest); string(b) != "old" {
		t.Errorf("dest changed by a failed write: %q", b)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
}
