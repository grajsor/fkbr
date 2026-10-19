package tui

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"fkbr/internal/api"
)

// fakeClient serves a fixed tree per node and records mutating calls.
type fakeClient struct {
	mu      sync.Mutex
	nodes   []api.Node
	files   map[string][]api.Entry // "node:root/dir" -> entries
	submits []api.TransferRequest
	removes []api.RemoveRequest
	mkdirs  []api.MkdirRequest
	syncs   []api.SyncRequest
	applied []string
	events  chan api.Event
}

func newFake() *fakeClient {
	f := &fakeClient{
		nodes: []api.Node{
			{NodeInfo: api.NodeInfo{ID: "master", Roots: []api.Root{{ID: "data", Writable: true}}}, Self: true, Online: true},
			{NodeInfo: api.NodeInfo{ID: "s1", Roles: []string{"slave"}, Roots: []api.Root{{ID: "data", Writable: true}, {ID: "logs"}}}, Online: true},
		},
		files:  map[string][]api.Entry{},
		events: make(chan api.Event, 16),
	}
	f.files["master:data/."] = []api.Entry{
		{Name: "b.txt", Type: api.TypeFile, Size: 5, Mode: 0o644},
		{Name: "a.txt", Type: api.TypeFile, Size: 3, Mode: 0o644},
		{Name: "proj", Type: api.TypeDir, Mode: 0o755},
	}
	f.files["master:data/proj"] = []api.Entry{{Name: "x", Type: api.TypeFile, Size: 1}}
	f.files["s1:data/."] = []api.Entry{{Name: "in", Type: api.TypeDir}}
	f.files["s1:data/in"] = nil
	return f
}

func (f *fakeClient) Self() string                                  { return "master" }
func (f *fakeClient) Close() error                                  { return nil }
func (f *fakeClient) Nodes(context.Context) ([]api.Node, error)     { return f.nodes, nil }
func (f *fakeClient) Forget(context.Context, string) error          { return nil }
func (f *fakeClient) Prune(context.Context, time.Duration) error    { return nil }
func (f *fakeClient) Jobs(context.Context) ([]api.Job, error)       { return nil, nil }
func (f *fakeClient) Job(context.Context, string) (*api.Job, error) { return nil, api.ErrUnknownJob }
func (f *fakeClient) Cancel(context.Context, string) error          { return nil }
func (f *fakeClient) CatchUp(context.Context, string) (string, error) {
	return "", fmt.Errorf("none")
}
func (f *fakeClient) Subscribe(context.Context) (<-chan api.Event, error) { return f.events, nil }

func (f *fakeClient) PlanSync(_ context.Context, r api.SyncRequest) (*api.SyncPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, r)
	return &api.SyncPlan{ID: "plan1", Request: r, SrcEntries: 4, MaxDeletes: 1,
		Targets: []api.SyncTarget{{Node: "s1", Copy: 2, Delete: 3, OverLimit: true}}}, nil
}

func (f *fakeClient) SyncPlan(context.Context, string) (*api.SyncPlan, error) {
	return nil, api.ErrUnknownJob
}

func (f *fakeClient) ApplySync(_ context.Context, id string, force bool) (*api.SyncPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, fmt.Sprintf("%s force=%v", id, force))
	return &api.SyncPlan{ID: id, Applied: true, Jobs: []string{"j"}}, nil
}

func (f *fakeClient) List(_ context.Context, r api.ListRequest) (*api.ListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	es, ok := f.files[r.Node+":"+r.Root+"/"+r.Path]
	if !ok {
		return nil, fmt.Errorf("no such dir %s", r.Path)
	}
	es = append([]api.Entry(nil), es...)
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	return &api.ListResult{Node: r.Node, Root: r.Root, Path: r.Path, Total: len(es), Entries: es}, nil
}

func (f *fakeClient) Submit(_ context.Context, r api.TransferRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submits = append(f.submits, r)
	return fmt.Sprintf("job%016d", len(f.submits)), nil
}

func (f *fakeClient) Mkdir(_ context.Context, r api.MkdirRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirs = append(f.mkdirs, r)
	key := r.Node + ":" + r.Root + "/" + path.Dir(r.Path)
	f.files[key] = append(f.files[key], api.Entry{Name: path.Base(r.Path), Type: api.TypeDir})
	return nil
}

func (f *fakeClient) Remove(_ context.Context, r api.RemoveRequest) (*api.RemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, r)
	return &api.RemoveResult{Removed: len(r.Paths)}, nil
}

func (f *fakeClient) Read(_ context.Context, r api.ReadRequest) (*api.ReadResult, error) {
	return &api.ReadResult{Data: []byte("hello\x00world"), Size: 11, EOF: true}, nil
}

// harness drives the model like bubbletea does, running commands and
// feeding their messages back. Commands that block (event waits) are
// abandoned after a short timeout.
type harness struct {
	t *testing.T
	m *model
	f *fakeClient
}

func newHarness(t *testing.T) *harness {
	f := newFake()
	h := &harness{t: t, m: newModel(context.Background(), f), f: f}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 40})
	h.run(h.m.loadNodes())
	return h
}

func (h *harness) send(msg tea.Msg) {
	_, cmd := h.m.Update(msg)
	h.run(cmd)
}

func (h *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(100 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			h.run(c)
		}
	case tea.QuitMsg:
	default:
		h.send(msg)
	}
}

func (h *harness) key(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "ctrl+h": // what terminals that send ^H for Backspace deliver
			msg = tea.KeyMsg{Type: tea.KeyCtrlH}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "insert":
			msg = tea.KeyMsg{Type: tea.KeyInsert}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "ctrl+o":
			msg = tea.KeyMsg{Type: tea.KeyCtrlO}
		case "f4":
			msg = tea.KeyMsg{Type: tea.KeyF4}
		case "f3", "f5", "f6", "f7", "f8", "f9":
			msg = tea.KeyMsg{Type: map[string]tea.KeyType{"f3": tea.KeyF3, "f5": tea.KeyF5, "f6": tea.KeyF6, "f7": tea.KeyF7, "f8": tea.KeyF8, "f9": tea.KeyF9}[k]}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		h.send(msg)
	}
}

func (h *harness) cursorName(i int) string {
	if r := h.m.panes[i].current(); r != nil {
		return r.name
	}
	return ""
}

// open navigates the active pane from the node list into node:root (a
// single-root node opens its root directly).
func (h *harness) open(node, root string) {
	p := h.m.panes[h.m.active]
	p.cursorTo(node)
	h.key("enter")
	if p.level == levelRoots {
		p.cursorTo(root)
		h.key("enter")
	}
}

func TestNavigation(t *testing.T) {
	h := newHarness(t)
	p := h.m.panes[0]
	if p.level != levelNodes || len(p.rows) != 2 || p.rows[0].name != "master" {
		t.Fatalf("start: level %d rows %+v", p.level, p.rows)
	}
	h.key("enter") // master has one root: straight into it
	if p.level != levelDir || p.title() != "master:/" {
		t.Fatalf("dir: level %d title %s", p.level, p.title())
	}
	var names []string
	for _, r := range p.rows {
		names = append(names, r.name)
	}
	if strings.Join(names, ",") != "..,proj,a.txt,b.txt" {
		t.Fatalf("rows (dirs first): %v", names)
	}
	h.key("down", "enter") // into proj
	if p.path != "proj" || len(p.rows) != 2 {
		t.Fatalf("proj: path %q rows %d", p.path, len(p.rows))
	}
	if p.title() != "master:/proj" {
		t.Errorf("title %q", p.title())
	}
	h.key("ctrl+h") // back to root, cursor on proj (Backspace sent as ^H)
	if p.path != "." || h.cursorName(0) != "proj" {
		t.Fatalf("back: path %q cursor %q", p.path, h.cursorName(0))
	}
	h.key("backspace") // single root: straight back to the nodes
	if p.level != levelNodes || h.cursorName(0) != "master" {
		t.Fatalf("up to nodes: %d %q", p.level, h.cursorName(0))
	}
	// A node with several roots still shows them.
	p.cursorTo("s1")
	h.key("enter")
	if p.level != levelRoots || p.rows[1].name != "data" {
		t.Fatalf("multi-root node: level %d rows %+v", p.level, p.rows)
	}
	if !strings.Contains(h.m.View(), "nodes 2/2 online") {
		t.Error("header missing node count")
	}
}

func TestCopySelectionToOtherPane(t *testing.T) {
	h := newHarness(t)
	h.open("master", "data")
	h.key("tab")
	h.open("s1", "data")
	h.m.panes[1].cursorTo("in")
	h.key("enter", "tab") // right pane into s1:data/in, back to left

	p := h.m.panes[0]
	p.cursorTo("a.txt")
	h.key("insert", "insert") // select a.txt and b.txt
	if n, _ := p.selectedBytes(); n != 2 {
		t.Fatalf("selected %d", n)
	}
	h.key("f5")
	d, ok := h.m.dlg.(*transferDialog)
	if !ok {
		t.Fatalf("no transfer dialog: %T", h.m.dlg)
	}
	if d.inputs[0].Value() != "s1" || d.inputs[1].Value() != "data" || d.inputs[2].Value() != "in" {
		t.Fatalf("defaults: %q %q %q", d.inputs[0].Value(), d.inputs[1].Value(), d.inputs[2].Value())
	}
	h.key("ctrl+o") // skip -> overwrite-if-older
	// Fan out: replace the node field with a group.
	d.inputs[0].SetValue("@slave,master")
	h.key("enter")
	if h.m.dlg != nil || len(h.f.submits) != 1 {
		t.Fatalf("dialog %T submits %d", h.m.dlg, len(h.f.submits))
	}
	got := h.f.submits[0]
	want := api.TransferRequest{SrcNode: "master", SrcRoot: "data", Paths: []string{"a.txt", "b.txt"},
		DstNodes: []string{"@slave", "master"}, DstRoot: "data", DstDir: "in", Conflict: api.ConflictOverwriteOlder}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("submit:\n got %+v\nwant %+v", got, want)
	}
	if len(p.selected) != 0 {
		t.Error("selection not cleared after submit")
	}
}

func TestMoveDeletesSourcesOnlyAfterCleanSuccess(t *testing.T) {
	h := newHarness(t)
	h.open("master", "data")
	h.key("tab")
	h.open("s1", "data")
	h.key("tab")
	h.m.panes[0].cursorTo("proj")
	h.key("f6", "enter")
	if len(h.f.submits) != 1 {
		t.Fatal("move not submitted")
	}
	id := "job0000000000000001"
	ev := func(state api.JobState, skipped int64) {
		h.send(eventMsg{api.Event{Type: api.EventJob, JobID: id, Job: &api.Job{ID: id, State: state,
			Request:      h.f.submits[0],
			Participants: []api.JobStatus{{Node: "s1", Role: api.RoleTarget, State: state, Skipped: skipped}}}}})
	}
	ev(api.JobRunning, 0)
	if len(h.f.removes) != 0 {
		t.Fatal("removed while running")
	}
	ev(api.JobDone, 0)
	if len(h.f.removes) != 1 || h.f.removes[0].Paths[0] != "proj" || h.f.removes[0].Node != "master" {
		t.Fatalf("removes: %+v", h.f.removes)
	}

	// A move whose copy skipped something keeps the sources.
	h.key("f6", "enter")
	id = "job0000000000000002"
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: id, Job: &api.Job{ID: id, State: api.JobDone, Request: h.f.submits[1],
		Participants: []api.JobStatus{{Node: "s1", Role: api.RoleTarget, State: api.JobDone, Skipped: 1}}}}})
	if len(h.f.removes) != 1 {
		t.Fatalf("sources removed despite skips: %+v", h.f.removes)
	}
	if !strings.Contains(h.m.status, "sources kept") {
		t.Errorf("status: %q", h.m.status)
	}
}

func TestMkdirDeleteView(t *testing.T) {
	h := newHarness(t)
	h.open("s1", "data")
	h.key("f7")
	for _, r := range "newdir" {
		h.key(string(r))
	}
	h.key("enter")
	if len(h.f.mkdirs) != 1 || h.f.mkdirs[0] != (api.MkdirRequest{Node: "s1", Root: "data", Path: "newdir"}) {
		t.Fatalf("mkdirs: %+v", h.f.mkdirs)
	}
	if h.cursorName(0) != "newdir" {
		t.Errorf("cursor after mkdir: %q", h.cursorName(0))
	}

	h.key("f8")
	if _, ok := h.m.dlg.(*confirmDialog); !ok {
		t.Fatalf("no confirm: %T", h.m.dlg)
	}
	h.key("n")
	if len(h.f.removes) != 0 {
		t.Fatal("deleted after 'n'")
	}
	h.key("f8", "y")
	if len(h.f.removes) != 1 || h.f.removes[0].Paths[0] != "newdir" {
		t.Fatalf("removes: %+v", h.f.removes)
	}

	h.key("backspace", "backspace")
	h.open("master", "data")
	h.m.panes[0].cursorTo("a.txt")
	h.key("f3")
	d, ok := h.m.dlg.(*textDialog)
	if !ok || !strings.Contains(d.text, "binary file") {
		t.Fatalf("viewer: %T %+v", h.m.dlg, d)
	}
	h.key("esc")
	if h.m.dlg != nil {
		t.Fatal("viewer did not close")
	}
}

func TestJobsPanelAndRefreshOnDone(t *testing.T) {
	h := newHarness(t)
	h.key("tab")
	h.open("s1", "data")
	h.f.files["s1:data/."] = append(h.f.files["s1:data/."], api.Entry{Name: "arrived", Type: api.TypeFile})
	job := &api.Job{ID: "j1", State: api.JobRunning, Request: api.TransferRequest{SrcNode: "master", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "."}}
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: job}})
	if len(h.m.panes[1].rows) != 2 {
		t.Fatal("pane refreshed before the job finished")
	}
	done := *job
	done.State = api.JobDone
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &done}})
	if len(h.m.panes[1].rows) != 3 {
		t.Fatalf("destination pane not refreshed on job done: %d rows", len(h.m.panes[1].rows))
	}
	view := h.m.View()
	if !strings.Contains(view, "jobs (F9)") || !strings.Contains(view, "done") {
		t.Errorf("jobs panel missing:\n%s", view)
	}
	h.key("f9", "enter")
	if d, ok := h.m.dlg.(*textDialog); !ok || !strings.Contains(d.text, "state:     done") {
		t.Fatalf("job details: %T", h.m.dlg)
	}
}

func TestViewFitsTerminal(t *testing.T) {
	h := newHarness(t)
	h.open("master", "data")
	for _, size := range [][2]int{{120, 40}, {80, 24}, {41, 13}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		lines := strings.Split(h.m.View(), "\n")
		if len(lines) != size[1] {
			t.Errorf("%dx%d: %d lines", size[0], size[1], len(lines))
		}
		for i, l := range lines {
			if w := lipglossWidth(l); w > size[0] {
				t.Errorf("%dx%d: line %d is %d wide", size[0], size[1], i, w)
			}
		}
	}
}

func lipglossWidth(s string) int { return lipgloss.Width(s) }

func TestSyncFromPanes(t *testing.T) {
	h := newHarness(t)
	h.open("master", "data")
	h.m.panes[0].cursorTo("proj")
	h.key("enter", "tab")
	h.open("s1", "data")
	h.key("tab", "f4")
	_, ok := h.m.dlg.(*syncDialog)
	if !ok {
		t.Fatalf("no sync dialog: %T", h.m.dlg)
	}
	h.key("ctrl+o") // mirror
	h.key("enter")
	if len(h.f.syncs) != 1 {
		t.Fatal("no plan requested")
	}
	want := api.SyncRequest{SrcNode: "master", SrcRoot: "data", SrcPath: "proj", DstNodes: []string{"s1"}, DstRoot: "data", DstPath: ".", Mode: api.SyncMirror}
	if fmt.Sprint(h.f.syncs[0]) != fmt.Sprint(want) {
		t.Fatalf("plan request:\n got %+v\nwant %+v", h.f.syncs[0], want)
	}
	c, ok := h.m.dlg.(*confirmDialog)
	if !ok || !strings.Contains(c.title, "exceed the limit") {
		t.Fatalf("confirm: %T %+v", h.m.dlg, c)
	}
	h.key("y")
	if len(h.f.applied) != 1 || h.f.applied[0] != "plan1 force=true" {
		t.Fatalf("applied: %v", h.f.applied)
	}
}

func TestQuitAsksWhenRunningNodeWithActiveJobs(t *testing.T) {
	h := newHarness(t)
	h.m.runsNode = true
	_, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyF10})
	if cmd == nil || h.m.dlg != nil {
		t.Fatal("no active jobs: should quit at once")
	}
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &api.Job{ID: "j1", State: api.JobRunning,
		Request: api.TransferRequest{SrcNode: "master", DstNodes: []string{"s1"}}}}})
	h.m.Update(tea.KeyMsg{Type: tea.KeyF10})
	if _, ok := h.m.dlg.(*confirmDialog); !ok {
		t.Fatalf("active job on this node: expected a confirmation, got %T", h.m.dlg)
	}
	h.key("n")
	if h.m.dlg != nil {
		t.Fatal("dialog stayed open")
	}
}

func TestJobDetailsLiveUpdate(t *testing.T) {
	h := newHarness(t)
	job := &api.Job{ID: "j1", State: api.JobRunning, Request: api.TransferRequest{SrcNode: "master", SrcRoot: "data", Paths: []string{"x"}, DstNodes: []string{"s1"}, DstRoot: "data", DstDir: "."}}
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: job}})
	h.key("f9", "enter")
	d, ok := h.m.dlg.(*textDialog)
	if !ok || !strings.Contains(d.text, "state:     running") {
		t.Fatalf("job details: %T", h.m.dlg)
	}
	done := *job
	done.State = api.JobDone
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &done}})
	if !strings.Contains(d.text, "state:     done") {
		t.Fatalf("details not updated while open:\n%s", d.text)
	}
	if !strings.Contains(h.m.View(), "done") {
		t.Error("view still shows the old state")
	}
	// Events for other jobs leave it alone.
	other := *job
	other.ID = "j2"
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j2", Job: &other}})
	if !strings.Contains(d.text, "Job j1") && !strings.Contains(d.title, "j1") {
		t.Error("details switched to another job")
	}
	if !strings.Contains(d.text, "state:     done") {
		t.Error("another job's event replaced the details")
	}
}

func TestHelpShowsLogPath(t *testing.T) {
	h := newHarness(t)
	h.m.logPath = "/tmp/fkbr-1000-master/node.log"
	h.send(tea.KeyMsg{Type: tea.KeyF1})
	d, ok := h.m.dlg.(*textDialog)
	if !ok || !strings.Contains(d.text, "This node's log: /tmp/fkbr-1000-master/node.log") {
		t.Fatalf("help without the log path: %T", h.m.dlg)
	}
}
