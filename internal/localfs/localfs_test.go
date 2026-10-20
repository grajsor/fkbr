package localfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/config"
)

func TestCleanRel(t *testing.T) {
	ok := map[string]string{"": ".", ".": ".", "a/b/": "a/b", "a/../b": "b", "./a": "a"}
	for in, want := range ok {
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Errorf("CleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/etc", "..", "../x", "a/../../x", "a\x00b"} {
		if _, err := CleanRel(in); !errors.Is(err, api.ErrBadPath) {
			t.Errorf("CleanRel(%q): want ErrBadPath, got %v", in, err)
		}
	}
}

func setup(t *testing.T) (*Local, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, filepath.Join(root, "sub"), outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	must(t, os.WriteFile(filepath.Join(root, "b.txt"), []byte("hello"), 0o640))
	must(t, os.Symlink("b.txt", filepath.Join(root, "a-link")))
	must(t, os.Symlink(outside, filepath.Join(root, "escape")))
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600))
	l, err := Open([]config.Root{{ID: "r", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, root
}

func TestList(t *testing.T) {
	l, _ := setup(t)
	es, err := l.List("r", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name)
	}
	want := []string{"a-link", "b.txt", "escape", "sub"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
	if es[0].Type != api.TypeSymlink || es[0].LinkTarget != "b.txt" {
		t.Errorf("a-link: %+v", es[0])
	}
	if es[1].Type != api.TypeFile || es[1].Size != 5 || es[1].Mode != 0o640 {
		t.Errorf("b.txt: %+v", es[1])
	}
	if es[3].Type != api.TypeDir {
		t.Errorf("sub: %+v", es[3])
	}
}

func TestListConfinement(t *testing.T) {
	l, _ := setup(t)
	if _, err := l.List("r", "escape"); err == nil {
		t.Fatal("listing through a symlink to outside the root must fail")
	}
	if _, err := l.List("r", "../outside"); !errors.Is(err, api.ErrBadPath) {
		t.Fatalf("want ErrBadPath, got %v", err)
	}
	if _, err := l.List("nope", ""); !errors.Is(err, api.ErrUnknownRoot) {
		t.Fatalf("want ErrUnknownRoot, got %v", err)
	}
	// Lstat of the escaping link itself is fine: it is an entry in the root.
	e, err := l.Stat("r", "escape")
	if err != nil || e.Type != api.TypeSymlink {
		t.Fatalf("Stat(escape) = %+v, %v", e, err)
	}
}

func TestUnixMode(t *testing.T) {
	if got := UnixMode(os.ModeSetuid | os.ModeSticky | 0o755); got != 0o5755 {
		t.Fatalf("got %o", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWalk(t *testing.T) {
	l, root := setup(t)
	must(t, os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "sub", "deep", "z"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(root, "sub", "a"), nil, 0o644))
	var got []string
	err := l.Walk("r", "", func(rel string, e api.Entry, err error) error {
		if err != nil {
			return err
		}
		got = append(got, rel)
		return nil
	})
	must(t, err)
	want := []string{".", "a-link", "b.txt", "escape", "sub", "sub/a", "sub/deep", "sub/deep/z"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("walk order:\n got %v\nwant %v", got, want)
	}
	got = nil
	l.Walk("r", "sub", func(rel string, e api.Entry, err error) error {
		got = append(got, rel)
		if rel == "sub/deep" {
			return SkipDir
		}
		return nil
	})
	if strings.Join(got, ",") != "sub,sub/a,sub/deep" {
		t.Fatalf("SkipDir: %v", got)
	}
}

func TestWritableRoot(t *testing.T) {
	l, _ := setup(t)
	if _, err := l.WritableRoot("r"); !errors.Is(err, api.ErrReadOnly) {
		t.Fatalf("want ErrReadOnly, got %v", err)
	}
}

// A share inside a root is not part of it: not listed, walked, read.
func TestHiddenShare(t *testing.T) {
	base := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(base, "box", "nodes"), 0o755))
	must(t, os.WriteFile(filepath.Join(base, "box", "nodes", "x"), []byte("x"), 0o644))
	must(t, os.Mkdir(filepath.Join(base, "boxer"), 0o755))
	l, err := Open([]config.Root{{ID: "files", Path: base, Hide: "box"}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	es, err := l.List("files", ".")
	if err != nil || len(es) != 1 || es[0].Name != "boxer" {
		t.Fatalf("List: %+v %v", es, err)
	}
	for _, p := range []string{"box", "box/nodes", "box/nodes/x"} {
		if _, err := l.Stat("files", p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Stat %s: %v", p, err)
		}
		if f, err := l.Open("files", p); !errors.Is(err, os.ErrNotExist) {
			if f != nil {
				f.Close()
			}
			t.Errorf("Open %s: %v", p, err)
		}
		if !l.Hidden("files", p) {
			t.Errorf("Hidden %s = false", p)
		}
	}
	var seen []string
	l.Walk("files", ".", func(rel string, e api.Entry, err error) error {
		seen = append(seen, rel)
		return nil
	})
	if strings.Join(seen, " ") != ". boxer" {
		t.Errorf("Walk saw %q", seen)
	}
	if l.Hidden("files", "boxer") || l.Hidden("files", ".") {
		t.Error("Hidden too wide")
	}
}

// CheckRoots notices a shared directory that was deleted, moved, or
// replaced by another one: the open root then leads nowhere visible.
func TestCheckRoots(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		do         func(dir string)
	}{
		{"deleted", "was deleted", func(dir string) { os.RemoveAll(dir) }},
		{"moved", "was moved", func(dir string) { os.Rename(dir, dir+".old") }},
		{"replaced", "was replaced", func(dir string) { os.Rename(dir, dir+".old"); os.Mkdir(dir, 0o755) }},
		{"recreated", "was deleted", func(dir string) { os.RemoveAll(dir); os.Mkdir(dir, 0o755) }},
	} {
		dir := filepath.Join(t.TempDir(), "share-me")
		must(t, os.Mkdir(dir, 0o755))
		l, err := Open([]config.Root{{ID: "files", Path: dir, AllowWrite: true}})
		if err != nil {
			t.Fatal(err)
		}
		if err := l.CheckRoots(); err != nil {
			t.Fatalf("%s: before: %v", tc.name, err)
		}
		tc.do(dir)
		err = l.CheckRoots()
		if !errors.Is(err, ErrRootGone) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), dir) {
			t.Errorf("%s: %v", tc.name, err)
		}
		l.Close()
	}
}
