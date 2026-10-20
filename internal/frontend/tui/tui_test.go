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
	"github.com/mattn/go-runewidth"

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
	read    []byte // what Read returns, if set
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

func (f *fakeClient) SyncPlanEntries(_ context.Context, id string, limit int) (*api.SyncEntries, error) {
	return &api.SyncEntries{Deletes: []api.SyncEntryList{{Node: "s1", Paths: []string{"old/a", "old/b", "stale.txt"}}},
		Copies: []api.SyncEntryList{{Targets: []string{"s1"}, Paths: []string{"a.txt", "b.txt"}}}}, nil
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
	if f.read != nil {
		return &api.ReadResult{Data: f.read, Size: int64(len(f.read)), EOF: true}, nil
	}
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

func TestQQuits(t *testing.T) {
	h := newHarness(t)
	if _, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q in the panes did not quit")
	}
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &api.Job{ID: "j1", State: api.JobDone,
		Request: api.TransferRequest{SrcNode: "master", DstNodes: []string{"s1"}}}}})
	h.key("f9")
	if _, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}); cmd == nil {
		t.Fatal("q in the jobs panel did not quit")
	}
	// In a dialog, q keeps its meaning: it closes the viewer.
	h.m.jobsFocus = false
	h.key("f1")
	h.key("q")
	if h.m.dlg != nil {
		t.Fatal("q did not close the help")
	}
}

func TestJobDetailsLayout(t *testing.T) {
	created := time.Date(2026, 10, 19, 16, 27, 57, 0, time.Local)
	j := &api.Job{ID: "j1", State: api.JobDone, Created: created,
		Request: api.TransferRequest{SrcNode: "jenkins1", SrcRoot: "files", Paths: []string{"plugins"}, DstNodes: []string{"ci5", "ci4"}, DstRoot: "files", DstDir: "userContent"},
		Participants: []api.JobStatus{ // as listed from the share: targets first
			{Node: "ci5", Role: api.RoleTarget, State: api.JobDone, Files: 6721, Bytes: 2 << 30, Chunks: 19,
				Problems: []string{"16:29:10 ownership not preserved on 6721 entries (needs root or CAP_CHOWN)"}, Updated: created.Add(72 * time.Second)},
			{Node: "jenkins1", Role: api.RoleSource, State: api.JobDone, Files: 6721, Bytes: 2 << 30, Chunks: 19, Updated: created.Add(70 * time.Second)},
			{Node: "ci4", Role: api.RoleTarget, State: api.JobRunning, Phase: "receiving", Problems: []string{"old note without a time"}},
		}}
	d := jobDetails(j)
	table := d[strings.Index(d, "NODE"):]
	src, t4, t5 := strings.Index(table, "jenkins1  source"), strings.Index(table, "ci4 "), strings.Index(table, "ci5 ")
	if src < 0 || t4 < 0 || t5 < 0 || !(src < t4 && t4 < t5) {
		t.Fatalf("participants not ordered source, then targets by name:\n%s", d)
	}
	notes := strings.Index(d, "Notes")
	if notes < 0 || notes < strings.Index(d, "NODE")+t5 {
		t.Fatalf("notes not below the table:\n%s", d)
	}
	if !strings.Contains(d[notes:], "16:29:10  ci5: ownership not preserved") || !strings.Contains(d[notes:], "ci4: old note without a time") {
		t.Errorf("notes lack node or time:\n%s", d)
	}
	if strings.Index(d[notes:], "ci4: old note") > strings.Index(d[notes:], "16:29:10") {
		t.Errorf("untimed (older) notes should come first:\n%s", d)
	}
	if !strings.Contains(d, "receiving") {
		t.Errorf("running node's phase missing:\n%s", d)
	}
	if !strings.Contains(d, "state:     done\n") || !strings.Contains(d, "elapsed:   1 min 12 s, 2.0G at 28M/s (finished 16:29:09)\n") {
		t.Errorf("elapsed time missing:\n%s", d)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "under 1 s", 400 * time.Millisecond: "under 1 s", 1600 * time.Millisecond: "2 s", 45 * time.Second: "45 s", 72 * time.Second: "1 min 12 s",
		time.Hour: "1 h 0 min", 2*time.Hour + 3*time.Minute + 50*time.Second: "2 h 3 min", 50 * time.Hour: "50 h 0 min",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestJobProgressAndETA(t *testing.T) {
	created := time.Now().Add(-10 * time.Second)
	job := func(counted bool, targetBytes int64) *api.Job {
		return &api.Job{ID: "j1", State: api.JobRunning, Created: created,
			Request: api.TransferRequest{SrcNode: "src", DstNodes: []string{"a", "b"}},
			Participants: []api.JobStatus{
				{Node: "src", Role: api.RoleSource, State: api.JobRunning, Bytes: 600 << 20, Files: 30,
					TotalBytes: 1000 << 20, TotalFiles: 50, Counted: counted},
				{Node: "a", Role: api.RoleTarget, State: api.JobRunning, Bytes: 500 << 20},
				{Node: "b", Role: api.RoleTarget, State: api.JobRunning, Bytes: targetBytes},
			}}
	}
	// The slowest target decides: b has 250 MiB of 1000 MiB after 10 s.
	d := jobDetails(job(true, 250<<20))
	if !strings.Contains(d, "elapsed:   10 s so far\n") {
		t.Errorf("elapsed while running:\n%s", d)
	}
	if !strings.Contains(d, "progress:  250M of 1.0G (25%)") || !strings.Contains(d, "ETA 30s") {
		t.Errorf("progress line:\n%s", d)
	}
	if l := jobLine(job(true, 250<<20), 200); !strings.Contains(l, "25%") || !strings.Contains(l, "ETA 30s") {
		t.Errorf("jobs panel line: %q", l)
	}
	// Before the count is done there is no ETA, but it says so.
	d = jobDetails(job(false, 250<<20))
	if !strings.Contains(d, "progress:  250M done · counting the total (50 files so far)") || strings.Contains(d, "ETA") {
		t.Errorf("progress while counting:\n%s", d)
	}
}

// Long path lists give way, so a running job's progress stays visible.
func TestJobLineKeepsProgress(t *testing.T) {
	j := &api.Job{ID: "18dff52d10a5bd3789a5584c", State: api.JobRunning, Created: time.Now().Add(-10 * time.Second),
		Request: api.TransferRequest{SrcNode: "db01", Paths: []string{
			"dumps/orders-2026-10-17.sql.zst", "dumps/orders-2026-10-18.sql.zst", "dumps/orders-2026-10-19.sql.zst"},
			DstNodes: []string{"backup01", "build01"}, DstRoot: "files", DstDir: "incoming"},
		Participants: []api.JobStatus{
			{Node: "db01", Role: api.RoleSource, State: api.JobRunning, TotalBytes: 1000, Counted: true},
			{Node: "backup01", Role: api.RoleTarget, State: api.JobRunning, Bytes: 250},
			{Node: "build01", Role: api.RoleTarget, State: api.JobRunning, Bytes: 300},
		}}
	for _, w := range []int{116, 80} {
		l := jobLine(j, w)
		if runewidth.StringWidth(l) != w {
			t.Errorf("width %d: line is %d wide", w, runewidth.StringWidth(l))
		}
		if !strings.Contains(l, "25%") || !strings.Contains(l, "ETA") || !strings.Contains(l, "→ backup01,build01") {
			t.Errorf("width %d: progress or destination cut off: %q", w, l)
		}
	}
}

// Folders read as name/, the way up as ../ and symlinks as name@ (ls -F).
func TestRowNameMarkers(t *testing.T) {
	p := &pane{level: levelDir}
	for _, c := range []struct {
		r    row
		want string
	}{
		{row{up: true}, "../"},
		{row{name: "proj", isDir: true, entry: api.Entry{Type: api.TypeDir}}, "proj/"},
		{row{name: "current", entry: api.Entry{Type: api.TypeSymlink}}, "current@"},
		{row{name: "a.txt", entry: api.Entry{Type: api.TypeFile}}, "a.txt "},
	} {
		if got := p.formatRow(c.r, 60, false, 0); !strings.HasPrefix(got, c.want) {
			t.Errorf("%+v: row %q, want it to start with %q", c.r, got, c.want)
		}
	}
}

// Nodes marked in the node list are @dest: the copy dialog's default
// destination, expanded to the marked IDs on submit, with offline ones to
// be detached instead of failing the copy.
func TestDestFromMarkedNodes(t *testing.T) {
	h := newHarness(t)
	data := []api.Root{{ID: "data", Writable: true}}
	h.f.nodes = append(h.f.nodes,
		api.Node{NodeInfo: api.NodeInfo{ID: "s2", Roots: data}, Online: true},
		api.Node{NodeInfo: api.NodeInfo{ID: "s3", Roots: data}}) // offline
	h.run(h.m.loadNodes())
	h.key("tab")
	right := h.m.panes[1]
	right.cursorTo("s2")
	h.key("insert", "insert") // s2, then s3
	if got := h.m.destList(); fmt.Sprint(got) != "[s2 s3]" {
		t.Fatalf("marked %v", got)
	}
	if !strings.Contains(h.m.status, "@dest = s2,s3") {
		t.Errorf("status %q", h.m.status)
	}
	h.key("tab")
	h.open("master", "data")
	h.key("backspace") // back to the node list and in again: marks stay
	h.open("master", "data")
	h.m.panes[0].cursorTo("a.txt")
	h.key("f5")
	d, ok := h.m.dlg.(*transferDialog)
	if !ok {
		t.Fatalf("no transfer dialog: %T", h.m.dlg)
	}
	if d.inputs[0].Value() != "@dest" || d.inputs[1].Value() != "data" {
		t.Fatalf("defaults: %q %q", d.inputs[0].Value(), d.inputs[1].Value())
	}
	if !strings.Contains(d.view(h.m.st, 120, 40), "(@dest = s2,s3)") {
		t.Error("dialog does not show what @dest is")
	}
	h.key("enter")
	if len(h.f.submits) != 1 {
		t.Fatalf("submits %d, dialog %T", len(h.f.submits), h.m.dlg)
	}
	got := h.f.submits[0]
	if fmt.Sprint(got.DstNodes) != "[s2 s3]" || !got.DetachOffline || got.DstRoot != "data" {
		t.Fatalf("submit %+v", got)
	}
	// "-" in the node list clears @dest; F5 then falls back to the other pane.
	h.key("tab", "-", "tab")
	h.key("f5")
	if d := h.m.dlg.(*transferDialog); d.inputs[0].Value() == "@dest" {
		t.Error("@dest still the default after clearing the marks")
	}
}

func TestExpandDest(t *testing.T) {
	if _, _, err := expandDest("@dest", nil); err == nil {
		t.Error("empty @dest accepted")
	}
	nodes, via, err := expandDest("n1, @dest", []string{"a", "b"})
	if err != nil || !via || fmt.Sprint(nodes) != "[n1 a b]" {
		t.Errorf("got %v %v %v", nodes, via, err)
	}
	if _, via, _ := expandDest("n1,@all", []string{"a"}); via {
		t.Error("detach_offline set without @dest")
	}
}

// With entries selected, F4 syncs each selected folder into a folder of
// the same name on the other side, copies the selected files, and asks
// once for all of it.
func TestSyncSelectedFolders(t *testing.T) {
	h := newHarness(t)
	h.f.files["master:data/."] = append(h.f.files["master:data/."], api.Entry{Name: "docs", Type: api.TypeDir, Mode: 0o755})
	h.open("master", "data")
	h.key("tab")
	h.open("s1", "data")
	h.m.panes[1].cursorTo("in")
	h.key("enter", "tab") // other side: s1:data/in
	p := h.m.panes[0]
	for _, n := range []string{"docs", "proj", "a.txt"} {
		p.selected[n] = true
	}
	h.key("f4")
	d, ok := h.m.dlg.(*syncDialog)
	if !ok {
		t.Fatalf("no sync dialog: %T", h.m.dlg)
	}
	if !strings.Contains(d.view(h.m.st, 120, 40), "docs/, proj/, a.txt in master:data/.") {
		t.Errorf("title does not name the selection:\n%s", d.view(h.m.st, 120, 40))
	}
	h.key("ctrl+o", "enter") // mirror
	if len(h.f.syncs) != 2 {
		t.Fatalf("plans: %+v", h.f.syncs)
	}
	for i, n := range []string{"docs", "proj"} {
		if s := h.f.syncs[i]; s.SrcPath != n || s.DstPath != "in/"+n || s.Mode != api.SyncMirror {
			t.Errorf("plan %d: %+v", i, s)
		}
	}
	c, ok := h.m.dlg.(*confirmDialog)
	if !ok {
		t.Fatalf("no confirmation: %T", h.m.dlg)
	}
	all := strings.Join(c.lines, "\n")
	for _, want := range []string{"docs/", "proj/", "files: copy 1 to s1"} {
		if !strings.Contains(all, want) {
			t.Errorf("confirmation lacks %q:\n%s", want, all)
		}
	}
	h.key("y")
	if len(h.f.applied) != 2 {
		t.Fatalf("applied: %v", h.f.applied)
	}
	if len(h.f.submits) != 1 || fmt.Sprint(h.f.submits[0].Paths) != "[a.txt]" || h.f.submits[0].DstDir != "in" ||
		h.f.submits[0].Conflict != api.ConflictOverwrite {
		t.Fatalf("files copy: %+v", h.f.submits)
	}
}

// Names, a peer's registry fields and file contents can hold terminal
// control sequences; the screen shows them as "?" and keeps its layout,
// while actions still use the real names.
func TestControlCharactersNeverReachTheTerminal(t *testing.T) {
	h := newHarness(t)
	evil := []string{"\x1b]0;PWNED\x07title", "\x1b[2Jclear", "\x1b]52;c;cHduZWQ=\x07clip", "cr\rback", "nl\nname"}
	var es []api.Entry
	for _, n := range evil {
		es = append(es, api.Entry{Name: n, Type: api.TypeFile, Size: 1})
	}
	es = append(es, api.Entry{Name: "link", Type: api.TypeSymlink, LinkTarget: "\x1b]0;LINKPWN\x07"})
	h.f.files["s1:data/."] = es
	h.f.nodes[1].OS = "\x1b]0;OSPWN\x07Evil"
	h.f.nodes[1].Hostname = "host\x1b[2J"
	h.f.read = []byte("log line \x1b[31mred\x1b[0m \x1b]0;VIEWPWN\x07\r\nnext\n")
	h.run(h.m.loadNodes())
	check := func(what string) {
		t.Helper()
		v := h.m.View()
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\r", "\x1b[31m"} {
			if strings.Contains(v, bad) {
				t.Errorf("%s: screen holds %q", what, bad)
			}
		}
		for i, l := range strings.Split(v, "\n") {
			if w := lipgloss.Width(l); w != 120 {
				t.Errorf("%s: line %d is %d wide: %q", what, i, w, l)
				break
			}
		}
	}
	check("node list")
	h.open("s1", "data")
	check("listing")
	h.m.panes[h.m.active].cursorTo("link")
	check("symlink footer")
	if !strings.Contains(h.m.View(), "?]0;PWNED?title") {
		t.Error("cleaned name not shown")
	}
	h.key("f3")
	check("viewer")
	h.key("esc")
	h.m.panes[h.m.active].cursorTo(evil[0])
	h.key("insert", "f8")
	check("delete dialog")
	h.key("y")
	if len(h.f.removes) != 1 || h.f.removes[0].Paths[0] != evil[0] {
		t.Errorf("delete used %+v, want the real name", h.f.removes)
	}
	h.m.panes[h.m.active].cursorTo(evil[1])
	h.key("f5")
	check("copy dialog")
	h.key("esc")
	dir := "\x1b]0;DIRPWN\x07dir"
	h.f.files["s1:data/."] = append(h.f.files["s1:data/."], api.Entry{Name: dir, Type: api.TypeDir})
	h.f.files["s1:data/"+dir] = nil
	h.run(h.m.refreshPane(h.m.active))
	h.m.panes[h.m.active].cursorTo(dir)
	h.key("enter")
	check("inside an evil directory")
}

// The help flows with the window: paragraphs fill the width, key rows
// wrap under their description.
func TestHelpFlows(t *testing.T) {
	para := "  " + strings.Repeat("word ", 30)
	row := "  Enter          " + strings.Repeat("open ", 20)
	got := flowText(para+"\n"+row, 40)
	for i, l := range strings.Split(got, "\n") {
		if runewidth.StringWidth(l) > 40 {
			t.Errorf("line %d too wide: %q", i, l)
		}
	}
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[1], "  word") || strings.HasPrefix(lines[1], "   ") {
		t.Errorf("paragraph continuation: %q", lines[1])
	}
	var cont string
	for i, l := range lines {
		if strings.HasPrefix(l, "  Enter") {
			cont = lines[i+1]
		}
	}
	if !strings.HasPrefix(cont, strings.Repeat(" ", 17)+"open") {
		t.Errorf("key row continuation not under its description: %q", cont)
	}
	if wide := flowText(para, 200); strings.Count(wide, "\n") != 0 {
		t.Errorf("a paragraph on a wide screen is not one line: %q", wide)
	}
	// Every help line fits 80 columns when flowed for the dialog there.
	h := newHarness(t)
	for _, l := range strings.Split(flowText(h.m.helpText(), 80-7), "\n") {
		if runewidth.StringWidth(l) > 80-7 {
			t.Errorf("help line too wide: %q", l)
		}
	}
}

// The exclude field goes into the request and is offered again next time;
// a sync's confirm shows what it deletes and lists everything on l.
func TestExcludeAndPlanList(t *testing.T) {
	h := newHarness(t)
	h.open("master", "data")
	h.m.panes[0].cursorTo("proj")
	h.key("enter", "tab")
	h.open("s1", "data")
	h.key("tab", "f4")
	d, ok := h.m.dlg.(*syncDialog)
	if !ok {
		t.Fatalf("no sync dialog: %T", h.m.dlg)
	}
	d.setFocus(3)
	h.key("*.tmp .git")
	h.key("enter")
	if len(h.f.syncs) != 1 || strings.Join(h.f.syncs[0].Exclude, " ") != "*.tmp .git" {
		t.Fatalf("plan request exclude: %+v", h.f.syncs)
	}
	c, ok := h.m.dlg.(*confirmDialog)
	if !ok {
		t.Fatalf("confirm: %T", h.m.dlg)
	}
	text := strings.Join(c.lines, "\n")
	if !strings.Contains(text, "delete: old/a, old/b, stale.txt") || !strings.Contains(text, "excluding: *.tmp .git") {
		t.Errorf("confirm lines:\n%s", text)
	}
	h.key("l")
	td, ok := h.m.dlg.(*textDialog)
	if !ok || !strings.Contains(td.text, "delete on s1 (3)") || !strings.Contains(td.text, "copy to s1 (2)") || !strings.Contains(td.text, "stale.txt") {
		t.Fatalf("list: %T %+v", h.m.dlg, td)
	}
	h.key("esc")
	if h.m.dlg != c {
		t.Fatalf("Esc in the list should go back to the plan: %T", h.m.dlg)
	}
	h.key("y")
	if len(h.f.applied) != 1 {
		t.Fatalf("applied: %v", h.f.applied)
	}
	h.key("f4")
	if d, ok := h.m.dlg.(*syncDialog); !ok || d.inputs[3].Value() != "*.tmp .git" {
		t.Errorf("exclude not offered again: %T", h.m.dlg)
	}
}
