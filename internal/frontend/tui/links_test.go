package tui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"fkbr/internal/api"
)

// fakeHome is the fake client as an ssh swarm's home node.
type fakeHome struct {
	*fakeClient
	lmu     sync.Mutex
	links   []api.Link
	added   []string
	removed []string
}

func (f *fakeHome) Links(context.Context) ([]api.Link, error) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	return slices.Clone(f.links), nil
}

func (f *fakeHome) AddLink(_ context.Context, host string, save bool) error {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	if !save {
		panic("links from the TUI are saved")
	}
	f.added = append(f.added, host)
	f.links = append(f.links, api.Link{Host: host, State: "connecting", Saved: true})
	return nil
}

func (f *fakeHome) RemoveLink(_ context.Context, host string) error {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	f.removed = append(f.removed, host)
	f.links = slices.DeleteFunc(f.links, func(l api.Link) bool { return l.Host == host })
	return nil
}

func newHomeHarness(t *testing.T, start bool) (*harness, *fakeHome) {
	f := &fakeHome{fakeClient: newFake()}
	f.nodes[0].Roots = append(f.nodes[0].Roots, api.Root{ID: "local", Path: "/", Writable: true, Local: true})
	f.files["master:local/."] = []api.Entry{{Name: "etc", Type: api.TypeDir}}
	f.links = []api.Link{
		{Host: "s1-host", Node: "s1", State: "connected", Saved: true},
		{Host: "db01:/var/backups", State: "retrying in 4s", Error: "Permission denied (publickey)", Saved: true},
	}
	m := newModel(context.Background(), f)
	m.startPanes = start
	h := &harness{t: t, m: m, f: f.fakeClient}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	h.run(h.m.loadNodes())
	return h, f
}

// The panes open in this node's directory and in another online node; a
// host with no node yet shows in the node list, with why.
func TestStartPanesAndLinkRows(t *testing.T) {
	h, _ := newHomeHarness(t, true)
	l, r := h.m.panes[0], h.m.panes[1]
	if l.level != levelDir || l.node != "master" || l.root != "data" {
		t.Errorf("left pane: %v %s:%s", l.level, l.node, l.root)
	}
	if r.level != levelDir || r.node != "s1" {
		t.Errorf("right pane: %v %s:%s", r.level, r.node, r.root)
	}
	h.key("f2")
	var row *row
	for i := range l.rows {
		if l.rows[i].name == "db01:/var/backups" {
			row = &l.rows[i]
		}
	}
	if row == nil || !row.link || !strings.Contains(row.info, "Permission denied") {
		t.Fatalf("no row for the host that is not up: %+v", l.rows)
	}
	for _, rw := range l.rows {
		if rw.name == "s1-host" {
			t.Error("a linked host's node shows twice")
		}
	}

	// Alone (no other node online), the right pane opens the local root.
	h2, f2 := newHomeHarness(t, false)
	f2.nodes[1].Online = false
	h2.m.startPanes = true
	h2.run(h2.m.loadNodes())
	if r := h2.m.panes[1]; r.level != levelDir || r.node != "master" || r.root != "local" {
		t.Errorf("right pane alone: %v %s:%s", r.level, r.node, r.root)
	}
}

// F7 in a node list links a host (saved); F8 unlinks it.
func TestLinkAndUnlinkFromTheNodeList(t *testing.T) {
	h, f := newHomeHarness(t, false)
	h.key("f7")
	if _, ok := h.m.dlg.(*inputDialog); !ok {
		t.Fatalf("F7 in the node list opened %T", h.m.dlg)
	}
	h.key("w", "e", "b", "0", "1", ":", "/", "s", "r", "v", "enter")
	if !slices.Equal(f.added, []string{"web01:/srv"}) {
		t.Fatalf("added %v", f.added)
	}
	h.run(h.m.loadNodes())
	p := h.m.panes[0]
	for p.current() != nil && p.current().name != "web01:/srv" {
		h.key("down")
		if p.cursor == len(p.rows)-1 && p.current().name != "web01:/srv" {
			t.Fatalf("no row for web01:/srv: %+v", p.rows)
		}
	}
	h.key("f8")
	if _, ok := h.m.dlg.(*confirmDialog); !ok {
		t.Fatalf("F8 opened %T", h.m.dlg)
	}
	h.key("y")
	if !slices.Equal(f.removed, []string{"web01:/srv"}) {
		t.Fatalf("removed %v", f.removed)
	}
	// F8 on a node linked over ssh unlinks its host.
	h.key("f2")
	for p.current() != nil && p.current().name != "s1" {
		h.key("down")
	}
	h.key("f8", "y")
	if !slices.Contains(f.removed, "s1-host") {
		t.Errorf("unlinking node s1: removed %v", f.removed)
	}
	// The help says how.
	if !strings.Contains(h.m.helpText(), "SSH HOSTS") {
		t.Error("help has no ssh section")
	}
}

// On a share, F7 and F8 in the node list do what they did.
func TestNoLinkKeysOnAShare(t *testing.T) {
	h := newHarness(t)
	h.key("f7")
	if h.m.dlg != nil {
		t.Errorf("F7 in a node list on a share opened %T", h.m.dlg)
	}
	if strings.Contains(h.m.helpText(), "SSH HOSTS") {
		t.Error("help talks about ssh hosts on a share")
	}
}
