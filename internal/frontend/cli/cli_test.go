package cli

import (
	"context"
	"testing"

	"fkbr/internal/api"
)

func TestParseTarget(t *testing.T) {
	cases := []struct{ in, node, root, rel string }{
		{"data", "", "data", ""},
		{"data/a/b", "", "data", "a/b"},
		{"slave1:data/a", "slave1", "data", "a"},
		{"slave1:", "slave1", "", ""},
		{"slave1:/data", "slave1", "data", ""},
		{"data/a:b", "", "data", "a:b"},
		{"@web,s1:www/site", "@web,s1", "www", "site"},
	}
	for _, c := range cases {
		n, r, p := ParseTarget(c.in)
		if n != c.node || r != c.root || p != c.rel {
			t.Errorf("ParseTarget(%q) = %q,%q,%q; want %q,%q,%q", c.in, n, r, p, c.node, c.root, c.rel)
		}
	}
}

func TestModeString(t *testing.T) {
	if got := ModeString(api.Entry{Type: api.TypeDir, Mode: 0o755}); got != "drwxr-xr-x" {
		t.Errorf("dir: %s", got)
	}
	if got := ModeString(api.Entry{Type: api.TypeFile, Mode: 0o4755}); got != "-rwsr-xr-x" {
		t.Errorf("setuid: %s", got)
	}
}

func TestModeStringSpecial(t *testing.T) {
	cases := map[uint32]string{0o2750: "-rwxr-s---", 0o1777: "-rwxrwxrwt", 0o4644: "-rwSr--r--"}
	for mode, want := range cases {
		if got := ModeString(api.Entry{Type: api.TypeFile, Mode: mode}); got != want {
			t.Errorf("%o: got %s want %s", mode, got, want)
		}
	}
}

// rootsClient answers only what resolveRoot needs.
type rootsClient struct {
	api.Client
	nodes []api.Node
}

func (c rootsClient) Self() string                              { return "a" }
func (c rootsClient) Nodes(context.Context) ([]api.Node, error) { return c.nodes, nil }

func TestResolveRoot(t *testing.T) {
	one := func(id string) api.Node {
		return api.Node{NodeInfo: api.NodeInfo{ID: id, Roots: []api.Root{{ID: "files"}}}}
	}
	multi := api.Node{NodeInfo: api.NodeInfo{ID: "m", Roots: []api.Root{{ID: "data"}, {ID: "logs"}}}}
	c := rootsClient{nodes: []api.Node{one("a"), one("b"), one("c"), multi}}
	cases := []struct{ nodes, root, rel, wantRoot, wantRel string }{
		{"", "sub", "dir", "files", "sub/dir"}, // a:/sub/dir
		{"b", "", "", "files", ""},             // b:/
		{"b,c", "in", "", "files", "in"},       // b,c:/in
		{"@all", "x", "", "x", ""},             // includes m: no shorthand
		{"b", "files", "x", "files", "x"},      // explicit root kept
		{"m", "logs", "app", "logs", "app"},    // multi-root node, explicit
		{"m", "nope", "", "nope", ""},          // unknown root reported later
	}
	for _, tc := range cases {
		r, p, err := resolveRoot(context.Background(), c, tc.nodes, tc.root, tc.rel)
		if err != nil || r != tc.wantRoot || p != tc.wantRel {
			t.Errorf("resolveRoot(%q,%q,%q) = %q,%q,%v; want %q,%q", tc.nodes, tc.root, tc.rel, r, p, err, tc.wantRoot, tc.wantRel)
		}
	}
	c.nodes = c.nodes[:3]
	if r, p, _ := resolveRoot(context.Background(), c, "@all", "release", ""); r != "files" || p != "release" {
		t.Errorf("@all over single-root nodes: %q %q", r, p)
	}
}
