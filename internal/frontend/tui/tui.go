// Package tui is the dual-pane, Norton Commander style frontend. Like
// every frontend it only talks to fkbr through api.Client; live updates
// come from the client's event stream.
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"fkbr/internal/api"
	"fkbr/internal/product"
	"fkbr/internal/safetext"
)

// Options describe how the TUI was started.
type Options struct {
	// RunsNode: this process is the node's worker (no separate daemon), so
	// quitting pauses this node's part of running jobs.
	RunsNode bool
	// Colors: auto (default), true, 256, 16 or none.
	Colors string
	// LogPath is where this node logs, shown in the help.
	LogPath string
	// Version scrolls through the header now and then (anim.go).
	Version string
	// Notice is shown in the status line at start.
	Notice string
	// NoMouse leaves the mouse to the terminal (text selection without
	// Shift).
	NoMouse bool
	// StartInDirs opens the panes in this node's directory and its local
	// root (or another node) instead of the node list.
	StartInDirs bool
}

// Run starts the TUI and blocks until the user quits.
func Run(ctx context.Context, c api.Client, opts Options) error {
	if err := setColors(opts.Colors); err != nil {
		return err
	}
	m := newModel(ctx, c)
	m.runsNode = opts.RunsNode
	m.startPanes = opts.StartInDirs
	m.logPath = opts.LogPath
	m.version = opts.Version
	if opts.Notice != "" {
		m.setStatus(opts.Notice, false)
	}
	popts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithContext(ctx)}
	if !opts.NoMouse {
		popts = append(popts, tea.WithMouseCellMotion())
	}
	_, err := tea.NewProgram(m, popts...).Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // ended from outside: the caller knows why
	}
	return err
}

const (
	viewLimit  = 64 << 10
	jobsShown  = 5 // rows in the jobs panel
	statusLife = 8 * time.Second
)

// moveInfo remembers what to delete once a move's copy has succeeded.
type moveInfo struct {
	node, root string
	paths      []string
}

type model struct {
	ctx context.Context
	c   api.Client
	// links are an ssh swarm's hosts, if this node is its home node;
	// startPanes and placed: see Options.StartInDirs (links.go).
	links []api.Link
	// lk: the client as an api.Linker if this node links hosts, asked
	// once (linkerAsked).
	lk          api.Linker
	linkerAsked bool
	startPanes  bool
	placed      bool
	st          *styles
	panes       [2]*pane
	active      int
	// lastExclude prefills the next copy or sync dialog's exclude field.
	lastExclude string
	// lastClick, dlgBox and drag serve the mouse (mouse.go, scrollbar.go);
	// markOn and markRow a right-button drag that marks rows.
	lastClick click
	dlgBox    box
	drag      drag
	markOn    bool
	markRow   int

	nodes    map[string]api.Node
	jobs     map[string]api.Job
	jobOrder []string // newest first

	width, height int
	dlg           dialog
	status        string
	statusErr     bool
	statusAt      time.Time

	runsNode bool
	logPath  string
	version  string
	// dest holds the nodes marked in the node list: "@dest" in the copy
	// and sync dialogs. It lasts as long as the TUI.
	dest map[string]bool
	// Header animation, spinner and marquee (anim.go).
	spin       int
	anim       animPhase
	animAt     time.Time
	marqueeKey string
	marqueeAt  time.Time
	jobsFocus  bool
	jobCursor  int
	moves      map[string]moveInfo
	events     <-chan api.Event
}

func newModel(ctx context.Context, c api.Client) *model {
	return &model{
		ctx: ctx, c: c, st: newStyles(),
		panes: [2]*pane{newPane(), newPane()},
		nodes: map[string]api.Node{}, jobs: map[string]api.Job{},
		moves: map[string]moveInfo{}, dest: map[string]bool{},
	}
}

// Messages produced by commands.
type (
	nodesMsg struct {
		nodes []api.Node
		links []api.Link // an ssh swarm's home node: its hosts (links.go)
	}
	listMsg struct {
		pane int
		seq  int
		res  *api.ListResult
		err  error
	}
	subscribedMsg struct{ ch <-chan api.Event }
	eventMsg      struct{ ev api.Event }
	eventsEndMsg  struct{ err error }
	resubscribe   struct{}
	statusMsg     struct {
		text    string
		err     bool
		refresh []target // panes showing these locations reload
	}
	submittedMsg struct {
		id   string
		req  api.TransferRequest
		move *moveInfo
		err  error
	}
	viewMsg struct {
		title string
		file  *viewedFile
		err   error
	}
	syncPlannedMsg struct {
		plans []*api.SyncPlan
		files *api.TransferRequest
		// entries are the first few paths of each plan's lists, by plan ID.
		entries map[string]*api.SyncEntries
		err     error
	}
	// openMsg shows a dialog that a command prepared.
	openMsg struct{ dlg dialog }
)

// target is a node:root location; panes inside it reload after changes.
type target struct{ node, root string }

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadNodes(), m.subscribe(), animTick(animEvery), spinTick()}
	if m.linker() != nil {
		cmds = append(cmds, linksTick())
	}
	return tea.Batch(cmds...)
}

func (m *model) loadNodes() tea.Cmd {
	l := m.linker() // here, not in the command: it may set the model's fields
	return func() tea.Msg {
		ns, err := m.c.Nodes(m.ctx)
		if err != nil {
			return statusMsg{text: "nodes: " + err.Error(), err: true}
		}
		return nodesMsg{ns, fetchLinks(m.ctx, l)}
	}
}

func (m *model) subscribe() tea.Cmd {
	return func() tea.Msg {
		ch, err := m.c.Subscribe(m.ctx)
		if err != nil {
			return eventsEndMsg{err}
		}
		return subscribedMsg{ch}
	}
}

func waitEvent(ch <-chan api.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsEndMsg{}
		}
		return eventMsg{ev}
	}
}

// load (re)lists pane i from offset.
func (m *model) load(i, offset int) tea.Cmd {
	p := m.panes[i]
	if p.level != levelDir {
		return nil
	}
	p.seq++
	p.loading = true
	seq, req := p.seq, api.ListRequest{Node: p.node, Root: p.root, Path: p.path, Offset: offset, Limit: pageSize}
	return func() tea.Msg {
		res, err := m.c.List(m.ctx, req)
		return listMsg{pane: i, seq: seq, res: res, err: err}
	}
}

// refreshPane reloads whatever pane i shows.
func (m *model) refreshPane(i int) tea.Cmd {
	switch m.panes[i].level {
	case levelDir:
		return m.load(i, 0)
	default:
		return m.loadNodes()
	}
}

func (m *model) refreshTargets(ts []target) tea.Cmd {
	var cmds []tea.Cmd
	for i, p := range m.panes {
		for _, t := range ts {
			if p.level == levelDir && p.node == t.node && p.root == t.root {
				cmds = append(cmds, m.load(i, 0))
				break
			}
		}
	}
	return tea.Batch(cmds...)
}

func (m *model) setStatus(text string, isErr bool) {
	m.status, m.statusErr, m.statusAt = safetext.Clean(text), isErr, time.Now()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		return m, m.mouse(msg)

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.dlg != nil {
			d := m.dlg
			done, cmd := d.update(m, msg)
			if done && m.dlg == d {
				m.dlg = nil
				if t, ok := d.(*textDialog); ok && t.back != nil {
					m.dlg = t.back // a list opened from a dialog returns to it
				}
			}
			return m, cmd
		}
		if m.jobsFocus {
			return m, m.jobsKey(msg)
		}
		return m, m.key(msg)

	case nodesMsg:
		for _, n := range msg.nodes {
			m.nodes[n.ID] = n
		}
		m.links = msg.links
		m.syncNodePanes()
		return m, m.placePanes()

	case linksTickMsg:
		return m, tea.Batch(m.loadNodes(), linksTick())

	case listMsg:
		p := m.panes[msg.pane]
		if msg.seq != p.seq {
			return m, nil // stale
		}
		p.loading = false
		if msg.err != nil {
			p.err = safetext.Clean(msg.err.Error())
			if msg.res == nil && len(p.rows) == 0 {
				p.rows = []row{{name: "..", up: true, isDir: true}}
			}
			return m, nil
		}
		p.err = ""
		p.setEntries(msg.res)
		return m, nil

	case subscribedMsg:
		m.events = msg.ch
		return m, waitEvent(msg.ch)

	case eventMsg:
		cmd := m.applyEvent(msg.ev)
		return m, tea.Batch(cmd, waitEvent(m.events))

	case eventsEndMsg:
		if m.ctx.Err() != nil {
			return m, nil
		}
		text := "live updates stopped; retrying"
		if msg.err != nil {
			text += ": " + msg.err.Error()
		}
		m.setStatus(text, true)
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return resubscribe{} })

	case resubscribe:
		return m, m.subscribe()

	case statusMsg:
		m.setStatus(msg.text, msg.err)
		return m, m.refreshTargets(msg.refresh)

	case submittedMsg:
		if msg.err != nil {
			m.setStatus("submit: "+msg.err.Error(), true)
			return m, nil
		}
		if msg.move != nil {
			m.moves[msg.id] = *msg.move
		}
		m.setStatus(fmt.Sprintf("job %s submitted → %s", short(msg.id), strings.Join(msg.req.DstNodes, ",")), false)
		m.panes[m.active].clearSelection()
		return m, nil

	case syncPlannedMsg:
		if msg.err != nil {
			m.setStatus("sync plan: "+msg.err.Error(), true)
			return m, nil
		}
		var work, entries int64
		for _, p := range msg.plans {
			work += planWork(p)
			entries += p.SrcEntries
		}
		if work == 0 && msg.files == nil {
			m.setStatus(fmt.Sprintf("in sync: nothing to do (%d entries compared)", entries), false)
			return m, nil
		}
		m.setStatus("", false)
		m.dlg = syncConfirm(m, msg.plans, msg.files, msg.entries)
		return m, nil

	case openMsg:
		m.dlg = msg.dlg
		return m, nil

	case animMsg:
		return m, m.animate()

	case spinMsg:
		m.spin = (m.spin + 1) % (2 * (len(spinFrames) - 1))
		return m, spinTick()

	case viewMsg:
		if msg.err != nil {
			m.setStatus("view: "+msg.err.Error(), true)
			return m, nil
		}
		d := newTextDialog(msg.title, renderFile(msg.file.res))
		if bytes.IndexByte(msg.file.res.Data, 0) < 0 { // not shown as hex
			d.lang = language(msg.title, d.text)
		}
		d.file = msg.file
		m.dlg = d
		return m, nil

	case editSavedMsg:
		if m.dlg == msg.d {
			m.dlg = msg.d.saved(msg)
		}
		if msg.err != nil {
			return m, nil
		}
		f := msg.d.file
		return m, m.refreshTargets([]target{{node: f.node, root: f.root}})
	}
	return m, nil
}

// applyEvent folds an event into the model and triggers follow-ups:
// refreshing panes at a finished job's destination and completing moves.
func (m *model) applyEvent(ev api.Event) tea.Cmd {
	switch ev.Type {
	case api.EventNode:
		if ev.Node != nil {
			m.nodes[ev.Node.ID] = *ev.Node
			m.syncNodePanes()
		}
	case api.EventScan:
		if s := ev.Scan; s != nil && !s.Done {
			text := fmt.Sprintf("scanning %s: %d entries, %s", s.Node, s.Entries, humanSize(s.Bytes))
			if s.Dir != "" {
				// The deepest part of the folder says the most.
				text += ", in " + fitLeft(rootPath(s.Dir), max(12, m.width-runewidth.StringWidth(text)-5))
			}
			m.setStatus(text, false)
		}
	case api.EventJobGone:
		delete(m.jobs, ev.JobID)
		m.sortJobs()
	case api.EventJob:
		if ev.Job == nil {
			return nil
		}
		prev, seen := m.jobs[ev.Job.ID]
		j := *ev.Job
		m.jobs[j.ID] = j
		m.sortJobs()
		if d, ok := m.dlg.(*textDialog); ok && d.jobID == j.ID {
			d.setText(jobDetails(&j)) // details stay live while open
		}
		if !j.State.Terminal() || (seen && prev.State == j.State) {
			return nil
		}
		var ts []target
		for _, d := range j.Request.DstNodes {
			ts = append(ts, target{d, j.Request.DstRoot})
		}
		cmds := []tea.Cmd{m.refreshTargets(ts)}
		if mv, ok := m.moves[j.ID]; ok {
			delete(m.moves, j.ID)
			cmds = append(cmds, m.finishMove(j, mv))
		}
		return tea.Batch(cmds...)
	}
	return nil
}

func (m *model) sortJobs() {
	m.jobOrder = m.jobOrder[:0]
	for id := range m.jobs {
		m.jobOrder = append(m.jobOrder, id)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(m.jobOrder))) // IDs sort by time
	m.jobCursor = min(m.jobCursor, max(0, len(m.jobOrder)-1))
}

// finishMove deletes a move's sources only if every participant finished
// cleanly: no errors, nothing skipped, nobody detached.
func (m *model) finishMove(j api.Job, mv moveInfo) tea.Cmd {
	var problems int64
	for _, p := range j.Participants {
		problems += p.Errors + p.Skipped
	}
	if j.State != api.JobDone || problems > 0 || len(j.Detached) > 0 {
		return func() tea.Msg {
			return statusMsg{text: fmt.Sprintf("move %s: copy ended %s with %d errors/skips; sources kept", short(j.ID), j.State, problems), err: true}
		}
	}
	return func() tea.Msg {
		res, err := m.c.Remove(m.ctx, api.RemoveRequest{Node: mv.node, Root: mv.root, Paths: mv.paths})
		refresh := []target{{mv.node, mv.root}}
		if err != nil {
			return statusMsg{text: "move: copied, but deleting sources failed: " + err.Error(), err: true, refresh: refresh}
		}
		if len(res.Errors) > 0 {
			return statusMsg{text: "move: copied; delete errors: " + strings.Join(res.Errors, "; "), err: true, refresh: refresh}
		}
		return statusMsg{text: fmt.Sprintf("move %s done", short(j.ID)), refresh: refresh}
	}
}

// syncNodePanes refreshes panes that list nodes or roots.
func (m *model) syncNodePanes() {
	nodes := m.sortedNodes()
	for _, p := range m.panes {
		switch p.level {
		case levelNodes:
			p.setNodes(nodes, m.links)
		case levelRoots:
			if n, ok := m.nodes[p.node]; ok {
				if r := p.current(); r != nil && p.want == "" {
					p.want = r.name // keep the cursor where it is
				}
				p.setRoots(n)
			}
		}
	}
}

func (m *model) sortedNodes() []api.Node {
	out := make([]api.Node, 0, len(m.nodes))
	for _, n := range m.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// paneHeight is the number of rows inside a pane (excluding border and
// footer).
func (m *model) paneHeight() int {
	return max(1, m.height-1-m.jobsHeight()-2-2-1-1)
}

func (m *model) jobsHeight() int {
	if len(m.jobOrder) == 0 {
		return 0
	}
	return min(jobsShown, len(m.jobOrder)) + 2
}

// key handles keys in the file panes.
func (m *model) key(msg tea.KeyMsg) tea.Cmd {
	p := m.panes[m.active]
	h := m.paneHeight()
	switch msg.String() {
	case "f10", "q":
		return m.quit()
	case "tab":
		m.active = 1 - m.active
	case "up", "k":
		p.move(-1, h)
	case "down", "j":
		p.move(1, h)
		return m.maybeLoadMore(m.active)
	case "pgup":
		p.move(-h, h)
	case "pgdown":
		p.move(h, h)
		return m.maybeLoadMore(m.active)
	case "home":
		p.move(-len(p.rows), h)
	case "end":
		p.move(len(p.rows), h)
		return m.maybeLoadMore(m.active)
	case "enter":
		return m.enter(m.active)
	case "backspace", "ctrl+h": // many terminals send ^H for Backspace
		return m.up(m.active)
	case "insert", " ":
		if r := p.current(); r != nil && p.markable(r) {
			toggle(m.marks(p), r.name)
		}
		p.move(1, h)
		m.destStatus(p)
	case "+":
		for _, r := range p.rows {
			if p.markable(&r) {
				m.marks(p)[r.name] = true
			}
		}
		m.destStatus(p)
	case "-":
		clear(m.marks(p))
		m.destStatus(p)
	case "*":
		for _, r := range p.rows {
			if p.markable(&r) {
				toggle(m.marks(p), r.name)
			}
		}
		m.destStatus(p)
	case "ctrl+r":
		return m.refreshPane(m.active)
	case "f1", "?":
		title := product.Name + " help"
		if m.version != "" {
			title = product.Name + " " + m.version + " · help"
		}
		m.dlg = newHelpDialog(title, m.helpText())
	case "f2":
		p.level, p.node, p.root, p.path, p.err = levelNodes, "", "", ".", ""
		p.clearSelection()
		p.setNodes(m.sortedNodes(), m.links)
		return m.loadNodes()
	case "f3":
		return m.view()
	case "f4":
		m.openSync()
	case "f5":
		m.openTransfer(false)
	case "f6":
		m.openTransfer(true)
	case "f7":
		if p.level == levelNodes && m.linker() != nil {
			m.openConnect()
		} else {
			m.openMkdir()
		}
	case "f8":
		if p.level == levelNodes && m.linker() != nil {
			m.openDisconnect()
		} else {
			m.openDelete()
		}
	case "f9":
		if len(m.jobOrder) > 0 {
			m.jobsFocus = true
		}
	}
	return nil
}

// quit asks first if this TUI is the node and it takes part in running
// jobs: they pause until fkbr runs here again.
func (m *model) quit() tea.Cmd {
	if !m.runsNode {
		return tea.Quit
	}
	var active []string
	for _, id := range m.jobOrder {
		j := m.jobs[id]
		if j.State.Terminal() {
			continue
		}
		if j.Request.SrcNode == m.c.Self() || slices.Contains(j.Request.DstNodes, m.c.Self()) {
			active = append(active, jobLine(&j, 70))
		}
	}
	if len(active) == 0 {
		return tea.Quit
	}
	m.dlg = &confirmDialog{
		title: fmt.Sprintf("This TUI is node %s. Quit anyway? Its part of %d running jobs pauses until %s runs here again (they resume, nothing is lost).", m.c.Self(), len(active), product.Name),
		lines: active,
		onYes: func() tea.Cmd { return tea.Quit },
	}
	return nil
}

func (m *model) maybeLoadMore(i int) tea.Cmd {
	p := m.panes[i]
	if p.level == levelDir && !p.loading && p.loaded < p.total && p.cursor > len(p.rows)-200 {
		return m.load(i, p.loaded)
	}
	return nil
}

func (m *model) enter(i int) tea.Cmd {
	p := m.panes[i]
	r := p.current()
	if r == nil {
		return nil
	}
	if r.up {
		return m.up(i)
	}
	switch p.level {
	case levelNodes:
		if r.link {
			m.setStatus(r.name+" is not linked yet: "+r.info, true)
			return nil
		}
		n := m.nodes[r.name]
		if !n.Online && !n.Self {
			m.setStatus(n.ID+" is offline: its directories cannot be listed", true)
		}
		if len(n.Roots) == 1 { // the usual case: go straight to its directory
			p.level, p.node, p.root, p.path = levelDir, n.ID, n.Roots[0].ID, "."
			p.rows, p.err, p.single = nil, "", true
			p.clearSelection()
			return m.load(i, 0)
		}
		p.level, p.node, p.single = levelRoots, n.ID, false
		p.setRoots(n)
	case levelRoots:
		p.level, p.root, p.path = levelDir, r.name, "."
		p.rows, p.err = nil, ""
		p.clearSelection()
		return m.load(i, 0)
	case levelDir:
		if !r.isDir {
			return m.view()
		}
		p.path = p.relPath(r.name)
		p.rows, p.err = nil, ""
		p.clearSelection()
		return m.load(i, 0)
	}
	return nil
}

func (m *model) up(i int) tea.Cmd {
	p := m.panes[i]
	p.clearSelection()
	p.err = ""
	switch p.level {
	case levelDir:
		if p.path == "." && p.single {
			keep := p.node
			p.level, p.node, p.root, p.single = levelNodes, "", "", false
			p.setNodes(m.sortedNodes(), m.links)
			p.cursorTo(keep)
			return nil
		}
		if p.path == "." {
			p.level, p.want = levelRoots, p.root
			p.setRoots(m.nodes[p.node])
			return nil
		}
		p.want = path.Base(p.path)
		p.path = path.Dir(p.path)
		return m.load(i, 0)
	case levelRoots:
		keep := p.node
		p.level, p.node = levelNodes, ""
		p.setNodes(m.sortedNodes(), m.links)
		p.cursorTo(keep)
	}
	return nil
}

func (m *model) openTransfer(move bool) {
	src, dst := m.panes[m.active], m.panes[1-m.active]
	names := src.selection()
	if len(names) == 0 {
		m.setStatus("nothing to copy: open a directory and select entries", true)
		return
	}
	dNodes, dRoot, dDir := "", "", "."
	if dst.level == levelDir {
		dNodes, dRoot, dDir = dst.node, dst.root, dst.path
	} else if dst.level == levelRoots {
		dNodes = dst.node
	}
	dNodes, dRoot = m.destDefault(dNodes, dRoot)
	d := newTransferDialog(move, src, names, dNodes, dRoot, dDir, m.lastExclude)
	d.dest = m.destList()
	m.dlg = d
}

func (m *model) openSync() {
	src, dst := m.panes[m.active], m.panes[1-m.active]
	if src.level != levelDir {
		m.setStatus("sync: open the source directory in this pane", true)
		return
	}
	dNodes, dRoot, dDir := "", "", "."
	if dst.level == levelDir {
		dNodes, dRoot, dDir = dst.node, dst.root, dst.path
	}
	dNodes, dRoot = m.destDefault(dNodes, dRoot)
	d := newSyncDialog(src, dNodes, dRoot, dDir, m.lastExclude)
	d.dest = m.destList()
	// Only an explicit selection counts: with none, the whole directory.
	for _, r := range src.rows {
		switch {
		case r.up || !src.selected[r.name]:
		case r.isDir:
			d.dirs = append(d.dirs, r.name)
		default:
			d.files = append(d.files, r.name)
		}
	}
	m.dlg = d
}

// marks returns the marked set a pane works on: its own selection in a
// directory, the TUI's @dest in the node list.
func (m *model) marks(p *pane) map[string]bool {
	if p.level == levelNodes {
		return m.dest
	}
	return p.selected
}

// markable: a row marks can hold. Not "../", not a root, and not an ssh
// host with no node yet (it cannot be a destination).
func (p *pane) markable(r *row) bool {
	return !r.up && !r.link && p.level != levelRoots
}

func toggle(set map[string]bool, name string) {
	if set[name] {
		delete(set, name)
	} else {
		set[name] = true
	}
}

// destList returns the marked nodes, sorted.
func (m *model) destList() []string {
	return slices.Sorted(maps.Keys(m.dest))
}

// destStatus tells what @dest holds after marking in the node list.
func (m *model) destStatus(p *pane) {
	switch {
	case p.level != levelNodes:
	case len(m.dest) == 0:
		m.setStatus("no nodes marked", false)
	default:
		m.setStatus(fmt.Sprintf("@%s = %s (F5/F4 copy or sync there)", api.GroupDest, strings.Join(m.destList(), ",")), false)
	}
}

// destDefault makes @dest the dialogs' destination while nodes are marked,
// with their shared root if they all have one of that name.
func (m *model) destDefault(nodes, root string) (string, string) {
	if len(m.dest) == 0 {
		return nodes, root
	}
	common := ""
	for i, id := range m.destList() {
		n, ok := m.nodes[id]
		if !ok || len(n.Roots) != 1 || (i > 0 && n.Roots[0].ID != common) {
			return "@" + api.GroupDest, root
		}
		common = n.Roots[0].ID
	}
	return "@" + api.GroupDest, common
}

// planSync makes a dry-run plan for each request (one per selected
// folder, or one for the whole directory); files, if any, are copied
// alongside once the plans are confirmed.
func (m *model) planSync(reqs []api.SyncRequest, files *api.TransferRequest) tea.Cmd {
	m.setStatus("planning sync: scanning source and targets…", false)
	return func() tea.Msg {
		var plans []*api.SyncPlan
		entries := map[string]*api.SyncEntries{}
		for _, r := range reqs {
			p, err := m.c.PlanSync(m.ctx, r)
			if err != nil {
				return syncPlannedMsg{err: fmt.Errorf("%s: %w", r.SrcPath, err)}
			}
			plans = append(plans, p)
			if e, err := m.c.SyncPlanEntries(m.ctx, p.ID, planPreview); err == nil {
				entries[p.ID] = e
			}
		}
		return syncPlannedMsg{plans: plans, files: files, entries: entries}
	}
}

func (m *model) submit(req api.TransferRequest, move bool) tea.Cmd {
	var mv *moveInfo
	if move {
		mv = &moveInfo{node: req.SrcNode, root: req.SrcRoot, paths: slices.Clone(req.Paths)}
	}
	req.Move = move
	return func() tea.Msg {
		id, err := m.c.Submit(m.ctx, req)
		return submittedMsg{id: id, req: req, move: mv, err: err}
	}
}

func (m *model) openMkdir() {
	p := m.panes[m.active]
	if p.level != levelDir {
		m.setStatus("open a directory first", true)
		return
	}
	node, root, dir := p.node, p.root, p.path
	m.dlg = newInputDialog(fmt.Sprintf("Make directory in %s", p.title()), "name: ", func(name string) tea.Cmd {
		p.want = strings.SplitN(name, "/", 2)[0]
		return func() tea.Msg {
			err := m.c.Mkdir(m.ctx, api.MkdirRequest{Node: node, Root: root, Path: path.Join(dir, name)})
			if err != nil {
				return statusMsg{text: "mkdir: " + err.Error(), err: true}
			}
			return statusMsg{text: "created " + name, refresh: []target{{node, root}}}
		}
	})
}

func (m *model) openDelete() {
	p := m.panes[m.active]
	names := p.selection()
	if len(names) == 0 {
		m.setStatus("nothing to delete", true)
		return
	}
	node, root := p.node, p.root
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = p.relPath(n)
	}
	m.dlg = &confirmDialog{
		title: fmt.Sprintf("Delete %d entries on %s:%s (recursively)?", len(paths), node, root),
		lines: paths,
		onYes: func() tea.Cmd {
			p.clearSelection()
			return func() tea.Msg {
				res, err := m.c.Remove(m.ctx, api.RemoveRequest{Node: node, Root: root, Paths: paths})
				refresh := []target{{node, root}}
				if err != nil {
					return statusMsg{text: "delete: " + err.Error(), err: true, refresh: refresh}
				}
				if len(res.Errors) > 0 {
					return statusMsg{text: fmt.Sprintf("deleted %d; errors: %s", res.Removed, strings.Join(res.Errors, "; ")), err: true, refresh: refresh}
				}
				return statusMsg{text: fmt.Sprintf("deleted %d entries", res.Removed), refresh: refresh}
			}
		},
	}
}

func (m *model) view() tea.Cmd {
	p := m.panes[m.active]
	r := p.current()
	if p.level != levelDir || r == nil || r.up || r.isDir {
		return nil
	}
	req := api.ReadRequest{Node: p.node, Root: p.root, Path: p.relPath(r.name), Limit: viewLimit}
	title := p.node + ":" + p.root + "/" + req.Path
	return func() tea.Msg {
		res, err := m.c.Read(m.ctx, req)
		return viewMsg{title: title, file: &viewedFile{node: req.Node, root: req.Root, path: req.Path, res: res}, err: err}
	}
}

// renderFile shows text as is, binary data as a hex dump.
func renderFile(res *api.ReadResult) string {
	data := res.Data
	var b strings.Builder
	if bytes.IndexByte(data, 0) >= 0 {
		fmt.Fprintf(&b, "binary file, %d bytes; first %d shown as hex\n\n", res.Size, min(len(data), 4096))
		data = data[:min(len(data), 4096)]
		for off := 0; off < len(data); off += 16 {
			line := data[off:min(off+16, len(data))]
			fmt.Fprintf(&b, "%08x  % x\n", off, line)
		}
		return b.String()
	}
	b.WriteString(strings.ReplaceAll(string(data), "\r\n", "\n")) // newTextDialog cleans the rest
	if !res.EOF {
		fmt.Fprintf(&b, "\n\n[… showing %d of %d bytes]", len(data), res.Size)
	}
	return b.String()
}

// jobsKey handles keys while the jobs panel has focus.
func (m *model) jobsKey(msg tea.KeyMsg) tea.Cmd {
	if len(m.jobOrder) == 0 {
		m.jobsFocus = false
		return nil
	}
	id := m.jobOrder[m.jobCursor]
	switch msg.String() {
	case "esc", "f9", "tab":
		m.jobsFocus = false
	case "f10", "q":
		return m.quit()
	case "up", "k":
		m.jobCursor = max(0, m.jobCursor-1)
	case "down", "j":
		m.jobCursor = min(len(m.jobOrder)-1, m.jobCursor+1)
	case "enter":
		j := m.jobs[id]
		d := newTextDialog("Job "+id, jobDetails(&j))
		d.jobID = id
		m.dlg = d
	case "c":
		return func() tea.Msg {
			if err := m.c.Cancel(m.ctx, id); err != nil {
				return statusMsg{text: "cancel: " + err.Error(), err: true}
			}
			return statusMsg{text: "cancellation requested for " + short(id)}
		}
	case "u":
		return func() tea.Msg {
			nid, err := m.c.CatchUp(m.ctx, id)
			if err != nil {
				return statusMsg{text: "catch-up: " + err.Error(), err: true}
			}
			return statusMsg{text: "catch-up job " + short(nid) + " submitted"}
		}
	}
	return nil
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

func jobDetails(j *api.Job) string {
	var b strings.Builder
	r := j.Request
	state := string(j.State)
	var last time.Time
	var srcBytes int64
	for _, p := range j.Participants {
		if p.Updated.After(last) {
			last = p.Updated
		}
		if p.Role == api.RoleSource {
			srcBytes = p.Bytes
		}
	}
	// From submit to the last status; while running it moves on with each
	// update of the job (no timer of its own).
	elapsed := ""
	switch {
	case j.Created.IsZero():
	case j.State.Terminal() && !last.IsZero():
		took := last.Sub(j.Created)
		elapsed = humanDuration(took)
		if took >= time.Second && srcBytes > 0 {
			elapsed += fmt.Sprintf(", %s at %s/s", humanSize(srcBytes), humanSize(int64(float64(srcBytes)/took.Seconds())))
		}
		elapsed += " (finished " + last.Local().Format("15:04:05") + ")"
	case !j.State.Terminal():
		elapsed = humanDuration(time.Since(j.Created)) + " so far"
	}
	fmt.Fprintf(&b, "state:     %s\ncreated:   %s by %s\n", state, j.Created.Local().Format("2006-01-02 15:04:05"), j.Initiator)
	if elapsed != "" {
		fmt.Fprintf(&b, "elapsed:   %s\n", elapsed)
	}
	fmt.Fprintf(&b, "from:      %s:%s  %s\nto:        %s:%s/%s\nif exists: %s   zstd: %v\n",
		r.SrcNode, r.SrcRoot, strings.Join(r.Paths, " "),
		strings.Join(r.DstNodes, ","), r.DstRoot, r.DstDir, r.Conflict, r.Compress)
	if j.CatchUpOf != "" {
		fmt.Fprintf(&b, "catch-up of %s\n", j.CatchUpOf)
	}
	if !j.State.Terminal() {
		done, total, files, counted, rate, eta := progressOf(j)
		switch {
		case counted && total > 0:
			fmt.Fprintf(&b, "progress:  %s of %s (%d%%)", humanSize(min(done, total)), humanSize(total), min(100, done*100/total))
			if rate > 0 {
				fmt.Fprintf(&b, " · %s/s · ETA %s", humanSize(int64(rate)), eta)
			}
			b.WriteString("\n")
		default:
			fmt.Fprintf(&b, "progress:  %s done · counting the total (%d files so far)\n", humanSize(done), files)
		}
	}

	// The source first, then the targets by name.
	ps := slices.Clone(j.Participants)
	slices.SortFunc(ps, func(a, c api.JobStatus) int {
		if (a.Role == api.RoleSource) != (c.Role == api.RoleSource) {
			if a.Role == api.RoleSource {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Node, c.Node)
	})
	b.WriteString("\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tROLE\tSTATE\tFILES\tBYTES\tCHUNKS\tSKIPPED\tERRORS\tNOW")
	var timed, untimed []string
	for _, p := range ps {
		now := ""
		if !p.State.Terminal() {
			now = p.Phase
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\n",
			p.Node, p.Role, p.State, p.Files, humanSize(p.Bytes), p.Chunks, p.Skipped, p.Errors, now)
		if p.Error != "" {
			untimed = append(untimed, p.Node+": error: "+safetext.Clean(p.Error))
		}
		for _, pr := range p.Problems {
			pr = safetext.Clean(pr)
			// Nodes stamp notes with the time ("15:04:05 note"); older
			// builds did not.
			if len(pr) > 9 && pr[2] == ':' && pr[5] == ':' && pr[8] == ' ' {
				timed = append(timed, pr[:8]+"  "+p.Node+": "+pr[9:])
			} else {
				untimed = append(untimed, p.Node+": "+pr)
			}
		}
	}
	tw.Flush()
	sort.Strings(timed)
	if notes := append(untimed, timed...); len(notes) > 0 {
		b.WriteString("\nNotes\n")
		for _, n := range notes {
			b.WriteString("  " + n + "\n")
		}
	}
	for _, d := range j.Detached {
		fmt.Fprintf(&b, "\ndetached %s after %d chunks: %s  (u = catch up)", d.Node, d.Chunks, safetext.Clean(d.Reason))
	}
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && p.State == api.JobFailed && !j.IsDetached(p.Node) {
			fmt.Fprintf(&b, "\nfailed %s  (u = send again)", p.Node)
		}
	}
	return b.String()
}

// progressOf returns a running job's progress: bytes done by its slowest
// active target, the source's totals so far and whether they are final,
// and the average rate and ETA (zero while unknown).
func progressOf(j *api.Job) (done, total, files int64, counted bool, rate float64, eta time.Duration) {
	done = -1
	for _, p := range j.Participants {
		switch {
		case p.Role == api.RoleSource:
			total, files, counted = p.TotalBytes, p.TotalFiles, p.Counted
		case !j.IsDetached(p.Node) && (done < 0 || p.Bytes < done):
			done = p.Bytes
		}
	}
	if done < 0 {
		done = 0
	}
	if el := time.Since(j.Created).Seconds(); el > 0 && done > 0 {
		rate = float64(done) / el
		if counted && total > done {
			eta = time.Duration(float64(total-done) / rate * float64(time.Second)).Round(time.Second)
		}
	}
	return
}

// jobLine summarises a job in one line for the jobs panel.
// humanDuration is d in h/min/s: "45 s", "1 min 12 s", "2 h 3 min".
func humanDuration(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case d < time.Second:
		return "under 1 s"
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	}
	return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
}

func jobLine(j *api.Job, width int) string {
	r := j.Request
	var src *api.JobStatus
	minChunks := -1
	for i := range j.Participants {
		p := &j.Participants[i]
		if p.Role == api.RoleSource {
			src = p
			continue
		}
		if j.IsDetached(p.Node) {
			continue
		}
		if minChunks < 0 || p.Chunks < minChunks {
			minChunks = p.Chunks
		}
	}
	progress := ""
	switch {
	case j.State.Terminal():
		if src != nil {
			progress = fmt.Sprintf("%d files %s", src.Files, humanSize(src.Bytes))
		}
	case src != nil && src.Counted && src.TotalBytes > 0:
		done, total, _, _, rate, eta := progressOf(j)
		progress = fmt.Sprintf("%d%%", min(100, done*100/total))
		if rate > 0 {
			progress += fmt.Sprintf(" · ETA %s", eta)
		}
	case src != nil && j.TotalChunks > 0 && minChunks >= 0:
		progress = fmt.Sprintf("%d/%d chunks", minChunks, j.TotalChunks)
	case src != nil:
		progress = fmt.Sprintf("%s staged", humanSize(src.Bytes))
		if src.Phase != "" {
			progress += " · " + src.Phase
		}
	}
	state := string(j.State)
	if len(j.Detached) > 0 {
		state += fmt.Sprintf("(%d det.)", len(j.Detached))
	}
	paths := strings.Join(r.Paths, ",")
	if r.SyncPlan != "" {
		paths = r.BaseDir + " (sync)"
	}
	paths = safetext.Clean(paths)
	// Progress always shows in full; paths and destination share the rest,
	// the paths giving way first.
	head := fmt.Sprintf("%s %-16s %s:", short(j.ID), state, r.SrcNode)
	dst := safetext.Clean(fmt.Sprintf("%s:%s/%s", strings.Join(r.DstNodes, ","), r.DstRoot, r.DstDir))
	room := width - runewidth.StringWidth(head) - len(" → ") - runewidth.StringWidth("  "+progress)
	pw, dw := runewidth.StringWidth(paths), runewidth.StringWidth(dst)
	if pw+dw > room {
		dw = min(dw, max(room-8, room/2))
		pw = max(room-dw, 1)
		paths, dst = runewidth.Truncate(paths, pw, "…"), runewidth.Truncate(dst, max(dw, 1), "…")
	}
	return fit(head+paths+" → "+dst+"  "+progress, width)
}

// View lays out header, panes (or a dialog), jobs, status and key bar.
func (m *model) View() string {
	if m.width < 40 || m.height < 12 {
		return "terminal too small for " + product.Name + " (need 40x12)"
	}
	var out []string
	out = append(out, m.header())
	ph := m.paneHeight()
	if m.dlg != nil {
		// The dialog takes the panes' place (ph rows + rule, footer and
		// borders); the box adds a border and padding around its content.
		area := ph + 4
		content := lipgloss.NewStyle().MaxWidth(m.width - 4).MaxHeight(area - 2).
			Render(padLines(m.dlg.view(m.st, m.width, area), m.st.inputLabel))
		framed := m.st.dialog.Render(content)
		w, h := lipgloss.Width(framed), lipgloss.Height(framed)
		x, y := placedBox(m.width, area, w, h)
		d := m.st.dialog
		m.dlgBox = box{x: x, y: 1 + y, w: w, h: h,
			cx: x + d.GetBorderLeftSize() + d.GetPaddingLeft(), cy: 1 + y + d.GetBorderTopSize() + d.GetPaddingTop(),
			lines: strings.Split(ansi.Strip(content), "\n")}
		out = append(out, lipgloss.Place(m.width, area, lipgloss.Center, lipgloss.Center, framed,
			lipgloss.WithWhitespaceBackground(cPanel)))
	} else {
		out = append(out, m.renderPanes(ph))
	}
	if jh := m.jobsHeight(); jh > 0 {
		out = append(out, m.renderJobs(jh))
	}
	status := ""
	if m.status != "" && time.Since(m.statusAt) < statusLife {
		status = m.status
	}
	st := m.st.status
	if m.statusErr {
		st = m.st.statusErr
	}
	out = append(out, st.Render(fit(status, m.width)))
	out = append(out, m.keyBar())
	// Text from elsewhere is cleaned where it is drawn; this is the net
	// for anything missed: only colour codes reach the terminal.
	return safetext.Screen(strings.Join(out, "\n"))
}

func (m *model) header() string {
	online, total, running := 0, len(m.nodes), 0
	for _, n := range m.nodes {
		if n.Online {
			online++
		}
	}
	for _, j := range m.jobs {
		if !j.State.Terminal() {
			running++
		}
	}
	name, home := product.Name, 1
	left := fmt.Sprintf(" · %s", m.c.Self())
	if m.runsNode {
		left += " (node runs in this TUI)"
	}
	right := fmt.Sprintf("nodes %d/%d online · jobs %d running ", online, total, running)
	slot := home + len(name)
	gap := max(1, m.width-slot-lipgloss.Width(left)-lipgloss.Width(right))
	// The name is laid over the line at its (possibly animated) column,
	// clipped to its own frame: the cells of " fkbr ".
	line := []rune(fit(strings.Repeat(" ", slot)+left+strings.Repeat(" ", gap)+right, m.width))
	isName := make([]bool, len(line))
	frame := min(slot+1, len(line))
	text, x := name, nameX(m.anim, time.Since(m.animAt), home, frame, len(name))
	if m.anim == animTicker {
		text, x = m.version, tickerX(time.Since(m.animAt), frame)
	}
	for i, r := range []rune(text) {
		if c := x + i; c >= 0 && c < frame {
			line[c], isName[c] = r, true
		}
	}
	var b strings.Builder
	for i := 0; i < len(line); {
		j := i
		for j < len(line) && isName[j] == isName[i] {
			j++
		}
		st := m.st.header
		if isName[i] {
			st = m.st.headerName
		}
		b.WriteString(st.Render(string(line[i:j])))
		i = j
	}
	return b.String()
}

func (m *model) renderPanes(h int) string {
	lw := m.width / 2
	widths := [2]int{lw, m.width - lw}
	var boxes [2]string
	for i, p := range m.panes {
		inner := widths[i] - 2
		active := i == m.active && !m.jobsFocus
		var marquee time.Duration
		if active {
			// The marquee starts over whenever the cursor is on another entry.
			key := fmt.Sprintf("%d|%s|%s|%s|%d", i, p.node, p.root, p.path, p.cursor)
			if key != m.marqueeKey {
				m.marqueeKey, m.marqueeAt = key, time.Now()
			}
			marquee = time.Since(m.marqueeAt)
		}
		lines := p.render(m.st, inner, h, active, marquee, m.marks(p))
		lines = append(lines, m.st.rule.Render(strings.Repeat("─", inner)), m.st.panel.Render(p.footer(inner)))
		title := p.title()
		if n, ok := m.nodes[p.node]; ok && p.node != "" && !n.Online {
			title += " (offline)"
		}
		boxes[i] = m.st.frame(active, title, strings.Join(lines, "\n"), inner)
		if start, size, ok := scrollThumb(h, p.scrollLen(), p.offset); ok {
			boxes[i] = m.st.withScrollbar(boxes[i], active, 1, start, size)
		}
		if i == 1 && m.jobsHeight() == 0 { // no jobs panel: the spinner sits here
			boxes[i] = m.st.withCorner(boxes[i], active, spinChar(m.spin))
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, boxes[0], boxes[1])
}

// rootPath shows a root-relative path as fkbr's paths read: "/a/b".
func rootPath(rel string) string {
	if rel == "." {
		return "/"
	}
	return "/" + rel
}

// fitLeft keeps the end of s (the deepest path part) if it is too long.
func fitLeft(s string, w int) string {
	r := []rune(s)
	if len(r) <= w || w < 2 {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

func (m *model) renderJobs(h int) string {
	inner := m.width - 2
	var lines []string
	n := h - 2
	start := 0
	if m.jobCursor >= n {
		start = m.jobCursor - n + 1
	}
	for i := start; i < len(m.jobOrder) && len(lines) < n; i++ {
		j := m.jobs[m.jobOrder[i]]
		if m.jobsFocus && i == m.jobCursor {
			lines = append(lines, m.st.highlight(m.st.cursor, jobLine(&j, inner)))
			continue
		}
		s := m.st.file
		switch {
		case j.State == api.JobFailed, j.State == api.JobCancelled:
			s = m.st.err
		case j.State == api.JobPartial:
			s = m.st.warn
		case j.State == api.JobDone:
			s = m.st.dim
		}
		lines = append(lines, s.Render(jobLine(&j, inner)))
	}
	for len(lines) < n {
		lines = append(lines, m.st.panel.Render(strings.Repeat(" ", inner)))
	}
	box := m.st.frame(m.jobsFocus, "jobs (F9)", strings.Join(lines, "\n"), inner)
	return m.st.withCorner(box, m.jobsFocus, spinChar(m.spin))
}

func (m *model) keyBar() string {
	keys := []struct{ n, label string }{
		{"1", "Help"}, {"2", "Nodes"}, {"3", "View"}, {"4", "Sync"}, {"5", "Copy"},
		{"6", "Move"}, {"7", "Mkdir"}, {"8", "Delete"}, {"9", "Jobs"}, {"10", "Quit"},
	}
	cell := max(4, m.width/len(keys))
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(m.st.keyNum.Render(k.n))
		b.WriteString(m.st.keyLabel.Render(fit(k.label, cell-len(k.n))))
	}
	if rest := m.width - cell*len(keys); rest > 0 {
		b.WriteString(m.st.keyNum.Render(strings.Repeat(" ", rest)))
	}
	return b.String()
}
