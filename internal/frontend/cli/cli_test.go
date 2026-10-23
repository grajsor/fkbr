package cli

import (
	"context"
	"errors"
	"slices"
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
	dirs  []string // folders in the shared roots, as "root/path"
}

func (c rootsClient) Self() string                              { return "a" }
func (c rootsClient) Nodes(context.Context) ([]api.Node, error) { return c.nodes, nil }
func (c rootsClient) List(_ context.Context, req api.ListRequest) (*api.ListResult, error) {
	if slices.Contains(c.dirs, req.Root+"/"+req.Path) {
		return &api.ListResult{}, nil
	}
	return nil, api.ErrNotFound
}

func TestResolveRoot(t *testing.T) {
	one := func(id string) api.Node {
		return api.Node{NodeInfo: api.NodeInfo{ID: id, Roots: []api.Root{{ID: "files"}}}}
	}
	multi := api.Node{NodeInfo: api.NodeInfo{ID: "m", Roots: []api.Root{{ID: "data"}, {ID: "logs"}}}}
	self := one("a") // also root "local", which only it sees
	self.Roots = append(self.Roots, api.Root{ID: "local", Local: true})
	c := rootsClient{nodes: []api.Node{self, one("b"), one("c"), multi}}
	cases := []struct{ nodes, root, rel, wantRoot, wantRel string }{
		{"", "sub", "dir", "files", "sub/dir"}, // a:/sub/dir
		{"b", "", "", "files", ""},             // b:/
		{"b,c", "in", "", "files", "in"},       // b,c:/in
		{"@all", "x", "", "x", ""},             // includes m: no shorthand
		{"b", "files", "x", "files", "x"},      // explicit root kept
		{"m", "logs", "app", "logs", "app"},    // multi-root node, explicit
		{"m", "nope", "", "nope", ""},          // unknown root reported later
		{"", "", "", "files", ""},              // cp b:/x / : this node's shared root
		{"", "local", "etc", "local", "etc"},   // local root, explicit
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
	// cd /usr && fkbr: local/bin is not quietly /bin when files/local is there.
	c.dirs = []string{"files/local"}
	if r, p, err := resolveRoot(context.Background(), c, "", "local", "bin"); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("local/bin with a folder local in the shared root: %q %q %v", r, p, err)
	}
	if r, p, err := resolveRoot(context.Background(), c, "", "files", "local/bin"); err != nil || r != "files" || p != "local/bin" {
		t.Errorf("files/local/bin: %q %q %v", r, p, err)
	}
	if r, p, err := resolveRoot(context.Background(), c, "b", "local", "bin"); err != nil || r != "files" || p != "local/bin" {
		t.Errorf("b:/local/bin (b has no root local): %q %q %v", r, p, err)
	}
	// The same in-process, with this node never registered (no share,
	// no node running): its own roots count.
	in := unregistered{rootsClient{nodes: c.nodes[1:], dirs: c.dirs}, self.Roots}
	if r, p, err := resolveRoot(context.Background(), in, "", "local", "bin"); !errors.Is(err, api.ErrBadPath) {
		t.Errorf("unregistered: local/bin with a folder local: %q %q %v", r, p, err)
	}
	if r, p, err := resolveRoot(context.Background(), in, "", "sub", "dir"); err != nil || r != "files" || p != "sub/dir" {
		t.Errorf("unregistered: /sub/dir: %q %q %v", r, p, err)
	}
}

// unregistered is a client in-process whose node is not in Nodes.
type unregistered struct {
	rootsClient
	roots []api.Root
}

func (c unregistered) LocalRoots() []api.Root { return c.roots }
