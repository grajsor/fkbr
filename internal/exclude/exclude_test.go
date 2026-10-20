package exclude

import "testing"

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
