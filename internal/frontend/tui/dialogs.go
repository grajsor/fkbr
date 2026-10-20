package tui

import (
	"fmt"
	"path"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"fkbr/internal/api"
	"fkbr/internal/product"
	"fkbr/internal/safetext"
)

// dialog is a modal box. update returns done=true to close it.
type dialog interface {
	update(m *model, msg tea.KeyMsg) (done bool, cmd tea.Cmd)
	view(st *styles, width, height int) string
}

var conflictPolicies = []api.ConflictPolicy{api.ConflictSkip, api.ConflictOverwriteOlder, api.ConflictOverwrite}

// transferDialog asks where to copy or move the selection. The node field
// takes "node", "n1,n2" or "@role" for fan-out.
type transferDialog struct {
	move     bool
	srcNode  string
	srcRoot  string
	paths    []string
	inputs   [3]textinput.Model // nodes, root, dir
	focus    int
	conflict int
	compress bool
	err      string
	dest     []string // the marked nodes, for @dest
}

func newTransferDialog(move bool, src *pane, names []string, dstNodes, dstRoot, dstDir string) *transferDialog {
	d := &transferDialog{move: move, srcNode: src.node, srcRoot: src.root}
	for _, n := range names {
		d.paths = append(d.paths, src.relPath(n))
	}
	labels := []string{"to node(s): ", "root:       ", "directory:  "}
	values := []string{dstNodes, dstRoot, dstDir}
	for i := range d.inputs {
		ti := textinput.New()
		ti.Prompt = labels[i]
		ti.SetValue(values[i])
		ti.CharLimit = 4096
		d.inputs[i] = ti
	}
	d.inputs[0].Focus()
	return d
}

func (d *transferDialog) request() (api.TransferRequest, error) {
	nodes, viaDest, err := expandDest(d.inputs[0].Value(), d.dest)
	if err != nil {
		return api.TransferRequest{}, err
	}
	if len(nodes) == 0 {
		return api.TransferRequest{}, fmt.Errorf("enter a destination node, n1,n2 or @role")
	}
	root := strings.TrimSpace(d.inputs[1].Value())
	if root == "" {
		return api.TransferRequest{}, fmt.Errorf("enter a destination root")
	}
	dir := strings.Trim(strings.TrimSpace(d.inputs[2].Value()), "/")
	if dir == "" {
		dir = "."
	}
	return api.TransferRequest{
		SrcNode: d.srcNode, SrcRoot: d.srcRoot, Paths: d.paths,
		DstNodes: nodes, DetachOffline: viaDest, DstRoot: root, DstDir: dir,
		Conflict: conflictPolicies[d.conflict], Compress: d.compress,
	}, nil
}

// expandDest splits the node field and replaces @dest by the marked nodes.
// viaDest tells the engine to detach marked nodes that are offline (catch
// them up later) rather than refuse the request, as it does for @all.
func expandDest(field string, dest []string) (nodes []string, viaDest bool, err error) {
	for _, n := range strings.FieldsFunc(field, func(r rune) bool { return r == ',' || r == ' ' }) {
		if n != "@"+api.GroupDest {
			nodes = append(nodes, n)
			continue
		}
		if len(dest) == 0 {
			return nil, false, fmt.Errorf("@%s is empty: mark nodes with Insert in the node list", api.GroupDest)
		}
		nodes, viaDest = append(nodes, dest...), true
	}
	return nodes, viaDest, nil
}

// destHint shows what @dest stands for while the node field uses it.
func destHint(field string, dest []string) string {
	if !strings.Contains(field, "@"+api.GroupDest) || len(dest) == 0 {
		return ""
	}
	return fmt.Sprintf("            (@%s = %s)\n", api.GroupDest, strings.Join(dest, ","))
}

func (d *transferDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return true, nil
	case "tab", "down":
		d.setFocus((d.focus + 1) % len(d.inputs))
		return false, nil
	case "shift+tab", "up":
		d.setFocus((d.focus + len(d.inputs) - 1) % len(d.inputs))
		return false, nil
	case "ctrl+o":
		d.conflict = (d.conflict + 1) % len(conflictPolicies)
		return false, nil
	case "ctrl+z":
		d.compress = !d.compress
		return false, nil
	case "ctrl+a":
		d.inputs[0].SetValue("@" + api.GroupAll)
		return false, nil
	case "enter":
		req, err := d.request()
		if err != nil {
			d.err = err.Error()
			return false, nil
		}
		return true, m.submit(req, d.move)
	}
	var cmd tea.Cmd
	d.inputs[d.focus], cmd = d.inputs[d.focus].Update(msg)
	return false, cmd
}

func (d *transferDialog) setFocus(i int) {
	d.inputs[d.focus].Blur()
	d.focus = i
	d.inputs[d.focus].Focus()
}

func (d *transferDialog) view(st *styles, width, _ int) string {
	verb := "Copy"
	if d.move {
		verb = "Move"
	}
	w := min(width-6, 76)
	what := fmt.Sprintf("%d entries", len(d.paths))
	if len(d.paths) == 1 {
		what = d.paths[0]
	}
	var b strings.Builder
	b.WriteString(st.dialogTitle.Render(fmt.Sprintf("%s %s from %s:%s", verb, clip(safetext.Clean(what), w-20), d.srcNode, d.srcRoot)) + "\n\n")
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2])
	for i := range d.inputs {
		d.inputs[i].Width = w - 14
		b.WriteString(d.inputs[i].View() + "\n")
		if i == 0 {
			b.WriteString(destHint(d.inputs[0].Value(), d.dest))
		}
	}
	zs := "off"
	if d.compress {
		zs = "on"
	}
	fmt.Fprintf(&b, "\nif exists: %-20s zstd: %s\n", conflictPolicies[d.conflict], zs)
	if d.err != "" {
		b.WriteString("\n" + st.dialogErr.Render("! "+d.err) + "\n")
	}
	b.WriteString("\nEnter start · Tab next field · Esc cancel\n^A all nodes · ^O if-exists · ^Z zstd")
	return b.String()
}

// inputDialog asks for one line of text.
type inputDialog struct {
	title    string
	input    textinput.Model
	onSubmit func(string) tea.Cmd
}

func newInputDialog(title, prompt string, onSubmit func(string) tea.Cmd) *inputDialog {
	ti := textinput.New()
	ti.Prompt = prompt
	ti.CharLimit = 4096
	ti.Focus()
	return &inputDialog{title: title, input: ti, onSubmit: onSubmit}
}

func (d *inputDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return true, nil
	case "enter":
		v := strings.TrimSpace(d.input.Value())
		if v == "" {
			return true, nil
		}
		return true, d.onSubmit(v)
	}
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(msg)
	return false, cmd
}

func (d *inputDialog) view(st *styles, width, _ int) string {
	d.input.Width = min(width-10, 60)
	st.styleInputs(0, &d.input)
	return st.dialogTitle.Render(d.title) + "\n\n" + d.input.View() + "\n\nEnter ok · Esc cancel"
}

// confirmDialog asks yes/no; only "y" confirms.
type confirmDialog struct {
	title string
	lines []string
	onYes func() tea.Cmd
}

func (d *confirmDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		return true, d.onYes()
	case "n", "N", "esc", "enter":
		return true, nil
	}
	return false, nil
}

func (d *confirmDialog) view(st *styles, width, height int) string {
	w := min(width-6, 76)
	var b strings.Builder
	b.WriteString(st.dialogTitle.Render(safetext.Clean(d.title)) + "\n\n")
	for i, l := range d.lines {
		l = safetext.Clean(l)
		if i == max(3, height-12) {
			fmt.Fprintf(&b, "  … and %d more\n", len(d.lines)-i)
			break
		}
		b.WriteString("  " + fit(l, w-2) + "\n")
	}
	b.WriteString("\n" + confirmHint)
	return b.String()
}

// confirmHint ends a confirm dialog. A click on its answers works too
// (dialogClick): it must start with "y = yes".
const confirmHint = "y = yes · n / Esc = no"

// textDialog shows scrollable text (file viewer, help, job details).
type textDialog struct {
	title string
	vp    viewport.Model
	text  string
	ready bool
	// jobID is set for job details, which follow the job's events.
	jobID string
	// lang ("xml", "json" or "") selects syntax highlighting.
	lang string
	st   *styles
}

// setText replaces the text, keeping the scroll position.
func (d *textDialog) setText(text string) {
	d.text = text
	if d.ready {
		d.vp.SetContent(d.content(d.vp.Width))
	}
}

func (d *textDialog) content(w int) string {
	if d.lang != "" && d.st != nil {
		return highlight(d.st, d.lang, d.text, w)
	}
	return wrapText(d.text, w)
}

func newTextDialog(title, text string) *textDialog {
	return &textDialog{title: safetext.Clean(title), text: safetext.Text(text)}
}

func (d *textDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "f3", "f10", "enter":
		return true, nil
	}
	var cmd tea.Cmd
	d.vp, cmd = d.vp.Update(msg)
	return false, cmd
}

func (d *textDialog) view(st *styles, width, height int) string {
	w, h := max(20, width-6), max(3, height-6)
	d.st = st
	if !d.ready || d.vp.Width != w || d.vp.Height != h {
		d.vp = viewport.New(w, h)
		d.vp.SetContent(d.content(w))
		d.ready = true
	}
	pct := int(d.vp.ScrollPercent() * 100)
	return st.dialogTitle.Render(fit(d.title, w-8)+fmt.Sprintf(" %5d%%", pct)) + "\n" + d.vp.View() + "\n" + fit("↑↓ PgUp PgDn scroll · Esc close", w)
}

// wrapText hard-wraps long lines to w cells and expands tabs.
func wrapText(s string, w int) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(s, "\t", "    "), "\n") {
		for len([]rune(line)) > w {
			r := []rune(line)
			out.WriteString(string(r[:w]) + "\n")
			line = string(r[w:])
		}
		out.WriteString(line + "\n")
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// helpText is the F1 screen. Lines stay under 72 columns so it reads well
// in an 80-column terminal.
func (m *model) helpText() string {
	jobs := `  Copies run on the nodes, not in this window. A daemon keeps going
  after you quit.`
	if m.runsNode {
		jobs = `  This window is node ` + m.c.Self() + ` (no daemon runs it). Quitting asks
  first if it has running jobs; its part pauses and resumes the next
  time ` + product.Name + ` runs here. Nothing is lost.`
	}
	text := product.Expand(`
MOVING AROUND
  ↑ ↓  k j       move
  PgUp PgDn      a page up or down
  Home End       first or last entry
  Enter          open a folder, node or root; on a file: view it
  Backspace      up: folder, root, then the node list
  Tab            switch pane
  F2             go to the node list
  Ctrl+R         reload this pane

  Names end in / for a folder and @ for a link; ../ is one up.

MARKING
  Insert, Space  mark and move down
  + - *          mark all, none, invert
  In a node list, the marked nodes are @dest: the destination
  that F5 and F4 offer until you unmark them.

MOUSE
  click          move there (in either pane or the jobs)
  double-click   open, like Enter
  right-click    mark or unmark
  wheel          move up or down; scroll a dialog
  key bar        a click presses that key
  click outside  close a dialog
  Shift+drag     select text (-mouse=false: no Shift needed)

FILES
  F3             view (first 64 KiB; XML and JSON coloured)
  F5             copy the marked entries, or the one under the cursor
  F6             move: copy, then delete here once every copy succeeded
  F7             new folder
  F8             delete (asks first)

  In the F5/F6 dialog, "to" is the other pane, a node, n1,n2,
  @dest, @all or @role.
  Ctrl+A         to every node (@all)
  Ctrl+O         if a file exists: skip, overwrite if older, overwrite
  Ctrl+Z         compress on the way (zstd)

SYNC (F4)
  Makes the other side match this pane. With nothing marked, the
  whole folder is synced; marked folders each go to a folder of the
  same name, marked files are copied. Incremental (the default) sends
  new and changed files and keeps extra ones; mirror (Ctrl+O) also
  deletes what the source does not have, but only inside what you
  sync. You always see a dry run first and confirm it.
  Ctrl+O         incremental or mirror
  Ctrl+K         compare by checksum, not size and time

JOBS (F9)
  ↑ ↓            choose a job
  Enter          details: time taken, progress, ETA, every node, notes
  c              cancel
  u              catch up nodes that were offline
  Esc            back

`) + jobs + `

QUIT AND HELP
  F1, ?          this help
  F10, q, Ctrl+C quit`
	if m.logPath != "" {
		text += "\n\nThis node's log: " + m.logPath
	}
	return text
}

// syncDialog sets up a sync of the active pane's directory.
type syncDialog struct {
	srcNode, srcRoot, srcPath  string
	inputs                     [3]textinput.Model // nodes, root, dir
	focus                      int
	mirror, checksum, compress bool
	err                        string
	dest                       []string // the marked nodes, for @dest
	// dirs and files are the selected entries; none means the whole
	// directory. Each folder syncs to one of the same name; files are copied.
	dirs, files []string
}

func newSyncDialog(src *pane, dstNodes, dstRoot, dstDir string) *syncDialog {
	d := &syncDialog{srcNode: src.node, srcRoot: src.root, srcPath: src.path}
	labels := []string{"to node(s): ", "root:       ", "directory:  "}
	values := []string{dstNodes, dstRoot, dstDir}
	for i := range d.inputs {
		ti := textinput.New()
		ti.Prompt = labels[i]
		ti.SetValue(values[i])
		ti.CharLimit = 4096
		d.inputs[i] = ti
	}
	d.inputs[0].Focus()
	return d
}

// requests returns one sync per selected folder (into a folder of the same
// name), or one for the whole directory, and a copy of the selected files.
func (d *syncDialog) requests() ([]api.SyncRequest, *api.TransferRequest, error) {
	nodes, viaDest, err := expandDest(d.inputs[0].Value(), d.dest)
	if err != nil {
		return nil, nil, err
	}
	root := strings.TrimSpace(d.inputs[1].Value())
	if len(nodes) == 0 || root == "" {
		return nil, nil, fmt.Errorf("enter destination node(s) and root")
	}
	dir := strings.Trim(strings.TrimSpace(d.inputs[2].Value()), "/")
	if dir == "" {
		dir = "."
	}
	mode := api.SyncIncremental
	if d.mirror {
		mode = api.SyncMirror
	}
	base := api.SyncRequest{SrcNode: d.srcNode, SrcRoot: d.srcRoot, SrcPath: d.srcPath,
		DstNodes: nodes, DetachOffline: viaDest, DstRoot: root, DstPath: dir, Mode: mode, Checksum: d.checksum, Compress: d.compress}
	if len(d.dirs) == 0 && len(d.files) == 0 {
		return []api.SyncRequest{base}, nil, nil
	}
	var reqs []api.SyncRequest
	for _, name := range d.dirs {
		r := base
		r.SrcPath, r.DstPath = path.Join(d.srcPath, name), path.Join(dir, name)
		reqs = append(reqs, r)
	}
	var files *api.TransferRequest
	if len(d.files) > 0 {
		files = &api.TransferRequest{SrcNode: d.srcNode, SrcRoot: d.srcRoot,
			DstNodes: nodes, DetachOffline: viaDest, DstRoot: root, DstDir: dir,
			Conflict: api.ConflictOverwrite, Compress: d.compress}
		for _, name := range d.files {
			files.Paths = append(files.Paths, path.Join(d.srcPath, name))
		}
	}
	return reqs, files, nil
}

func (d *syncDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return true, nil
	case "tab", "down":
		d.inputs[d.focus].Blur()
		d.focus = (d.focus + 1) % len(d.inputs)
		d.inputs[d.focus].Focus()
		return false, nil
	case "shift+tab", "up":
		d.inputs[d.focus].Blur()
		d.focus = (d.focus + len(d.inputs) - 1) % len(d.inputs)
		d.inputs[d.focus].Focus()
		return false, nil
	case "ctrl+o":
		d.mirror = !d.mirror
		return false, nil
	case "ctrl+k":
		d.checksum = !d.checksum
		return false, nil
	case "ctrl+a":
		d.inputs[0].SetValue("@" + api.GroupAll)
		return false, nil
	case "ctrl+z":
		d.compress = !d.compress
		return false, nil
	case "enter":
		reqs, files, err := d.requests()
		if err != nil {
			d.err = err.Error()
			return false, nil
		}
		return true, m.planSync(reqs, files)
	}
	var cmd tea.Cmd
	d.inputs[d.focus], cmd = d.inputs[d.focus].Update(msg)
	return false, cmd
}

func (d *syncDialog) view(st *styles, width, _ int) string {
	w := min(width-6, 76)
	var b strings.Builder
	b.WriteString(st.dialogTitle.Render(fmt.Sprintf("Sync %s → (make the destination match it)", clip(safetext.Clean(d.what()), w-40))) + "\n\n")
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2])
	for i := range d.inputs {
		d.inputs[i].Width = w - 14
		b.WriteString(d.inputs[i].View() + "\n")
		if i == 0 {
			b.WriteString(destHint(d.inputs[0].Value(), d.dest))
		}
	}
	mode := "incremental (extra files kept)"
	if d.mirror {
		mode = "MIRROR (extra files deleted)"
	}
	fmt.Fprintf(&b, "\nmode: %-32s checksum: %-4v zstd: %v\n", mode, onOff(d.checksum), onOff(d.compress))
	if d.err != "" {
		b.WriteString("\n" + st.dialogErr.Render("! "+d.err) + "\n")
	}
	b.WriteString("\nEnter plan (dry run first) · Tab next field · Esc cancel\n^A all nodes · ^O mode · ^K checksum · ^Z zstd")
	return b.String()
}

// what names the source: the directory, or the selected entries in it.
func (d *syncDialog) what() string {
	where := d.srcNode + ":" + d.srcRoot + "/" + d.srcPath
	if len(d.dirs) == 0 && len(d.files) == 0 {
		return where
	}
	var parts []string
	for _, n := range d.dirs {
		parts = append(parts, n+"/")
	}
	if len(d.files) == 1 {
		parts = append(parts, d.files[0])
	} else if len(d.files) > 1 {
		parts = append(parts, fmt.Sprintf("%d files", len(d.files)))
	}
	return strings.Join(parts, ", ") + " in " + where
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// syncConfirm shows a plan; y applies it.
// planWork is what applying a plan would change.
func planWork(p *api.SyncPlan) int64 {
	var n int64
	for _, t := range p.Targets {
		n += t.Copy + t.Delete + t.Replace
	}
	return n
}

// syncConfirm shows the dry-run plans (one per selected folder, or one for
// the whole directory) and the files to copy, and applies them all on yes.
func syncConfirm(m *model, plans []*api.SyncPlan, files *api.TransferRequest) dialog {
	var lines []string
	over := false
	var entries int64
	mode := api.SyncIncremental
	for _, p := range plans {
		entries += p.SrcEntries
		mode = p.Request.Mode
		indent := ""
		if len(plans) > 1 || files != nil {
			lines = append(lines, path.Base(p.Request.SrcPath)+"/")
			indent = "  "
		}
		for _, t := range p.Targets {
			l := fmt.Sprintf("%s%-12s copy %d (%s)  replace %d  delete %d  unchanged %d",
				indent, t.Node, t.Copy, humanSize(t.CopyBytes), t.Replace, t.Delete, t.Unchanged)
			if t.OverLimit {
				l += fmt.Sprintf("  ⚠ over the delete limit %d", p.MaxDeletes)
				over = true
			}
			lines = append(lines, l)
		}
		for _, s := range p.Skipped {
			lines = append(lines, indent+"skipped "+s+" (offline)")
		}
		for _, pr := range p.Problems {
			lines = append(lines, indent+"problem: "+pr)
		}
	}
	if files != nil {
		lines = append(lines, fmt.Sprintf("files: copy %d to %s (replacing what is there)", len(files.Paths), strings.Join(files.DstNodes, ",")))
	}
	title := fmt.Sprintf("Sync plan (%s, source %d entries): apply?", mode, entries)
	if over {
		title += " Deletions exceed the limit; y applies anyway."
	}
	return &confirmDialog{title: title, lines: lines, onYes: func() tea.Cmd {
		return func() tea.Msg {
			jobs := 0
			var ts []target
			var problems []string
			for _, p := range plans {
				if planWork(p) == 0 {
					continue
				}
				ap, err := m.c.ApplySync(m.ctx, p.ID, over)
				if err != nil {
					return statusMsg{text: "sync: " + err.Error(), err: true}
				}
				jobs += len(ap.Jobs)
				problems = append(problems, ap.DeleteErrors...)
				for _, t := range ap.Targets {
					ts = append(ts, target{t.Node, ap.Request.DstRoot})
				}
			}
			if files != nil {
				if _, err := m.c.Submit(m.ctx, *files); err != nil {
					return statusMsg{text: "sync: copying files: " + err.Error(), err: true}
				}
				jobs++
			}
			text := fmt.Sprintf("sync: %d transfer jobs submitted", jobs)
			if len(problems) > 0 {
				return statusMsg{text: fmt.Sprintf("%s; %d deletions failed: %s", text, len(problems), problems[0]), err: true}
			}
			return statusMsg{text: text, refresh: ts}
		}
	}}
}
