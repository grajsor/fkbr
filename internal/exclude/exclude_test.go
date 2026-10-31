package exclude

import (
	"path"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	s, err := New([]string{"*.tmp", ".git", "node_modules/", "build/out", "/top.txt", "logs/*.gz", "  ", "cache/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rel  string
		dir  bool
		want bool
	}{
		{"a.tmp", false, true},
		{"deep/er/a.tmp", false, true},
		{"a.tmpx", false, false},
		{".git", true, true},
		{"sub/.git", true, true},
		{"sub/.github", true, false},
		{"node_modules", true, true},
		{"x/node_modules", true, true},
		{"node_modules", false, false}, // only directories
		{"build/out", true, true},
		{"build/out", false, true},
		{"x/build/out", true, false}, // a path pattern is anchored
		{"top.txt", false, true},
		{"sub/top.txt", false, false},
		{"logs/a.gz", false, true},
		{"logs/sub/a.gz", false, false}, // * does not cross /
		{"src/main.go", false, false},
		{".", true, false},
	} {
		if got := s.Match(c.rel, c.dir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v", c.rel, c.dir, got, c.want)
		}
	}
	var none *Set
	if none.Match("a.tmp", false) {
		t.Error("nil set matches")
	}
	if _, err := New([]string{"[a-"}); err == nil {
		t.Error("bad pattern accepted")
	}
	if s, _ := New(nil); s != nil {
		t.Error("no patterns should give a nil set")
	}
}

func FuzzExclude(f *testing.F) {
	for _, s := range []string{"*.tmp", ".git", "cache/", "/top", "a/b/*.gz", "[a-", "**", "\\", "a\x00b"} {
		f.Add(s, "a/b/c.tmp", true)
	}
	f.Fuzz(func(t *testing.T, pat, rel string, dir bool) {
		s, err := New([]string{pat})
		if err != nil || s == nil {
			return
		}
		got := s.Match(rel, dir)
		if s.Match(".", true) {
			t.Fatalf("%q excludes the top", pat)
		}
		// A plain name pattern matches by the last name only: the same
		// name under another folder gives the same answer. Plain as New
		// reads it: "*/ /" is the path "*/ " (a name of one space one
		// folder down), not the name "*".
		core := strings.TrimRight(strings.TrimSpace(pat), "/")
		if !strings.Contains(core, "/") && !strings.HasPrefix(strings.TrimSpace(pat), "/") && rel != "" && !strings.Contains(rel, "\\") {
			name := path.Base(rel)
			if s.Match("other/"+name, dir) != got && name != "." && name != "/" && !strings.HasPrefix(rel, "/") && path.Clean(rel) == rel {
				t.Fatalf("%q: %q=%v but other/%s=%v", pat, rel, got, name, !got)
			}
		}
	})
}

func TestWithKey(t *testing.T) {
	s, _ := New([]string{"cache/", "*.TMP", "Sub/x"})
	f := s.WithKey(strings.ToLower)
	for _, c := range []struct {
		p   string
		dir bool
	}{{"CACHE", true}, {"a/Cache", true}, {"x.tmp", false}, {"SUB/X", false}} {
		if !f.Match(c.p, c.dir) {
			t.Errorf("%s not matched case-insensitively", c.p)
		}
		if s.Match(c.p, c.dir) && c.p != "x.tmp" {
			t.Errorf("%s matched by the exact set", c.p)
		}
	}
	if f.Match("CACHE", false) {
		t.Error("cache/ is for folders only")
	}
}
