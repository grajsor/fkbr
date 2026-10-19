package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"fkbr/internal/api"
	"fkbr/internal/product"
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
	nodes := strings.FieldsFunc(d.inputs[0].Value(), func(r rune) bool { return r == ',' || r == ' ' })
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
		DstNodes: nodes, DstRoot: root, DstDir: dir,
		Conflict: conflictPolicies[d.conflict], Compress: d.compress,
	}, nil
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
	b.WriteString(st.dialogTitle.Render(fmt.Sprintf("%s %s from %s:%s", verb, clip(what, w-20), d.srcNode, d.srcRoot)) + "\n\n")
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2])
	for i := range d.inputs {
		d.inputs[i].Width = w - 14
		b.WriteString(d.inputs[i].View() + "\n")
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
	b.WriteString(st.dialogTitle.Render(d.title) + "\n\n")
	for i, l := range d.lines {
		if i == max(3, height-12) {
			fmt.Fprintf(&b, "  … and %d more\n", len(d.lines)-i)
			break
		}
		b.WriteString("  " + fit(l, w-2) + "\n")
	}
	b.WriteString("\ny = yes · n / Esc = no")
	return b.String()
}

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
	return &textDialog{title: title, text: text}
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

var helpText = product.Expand(`{name} — keys

Navigation
  ↑ ↓ PgUp PgDn Home End   move
  Enter                    open directory / node / root
  Backspace                up (directory → nodes)
  Tab                      switch pane
  F2                       pick node (go to the node list)
  Ctrl+R                   refresh pane

Selection
  Insert / Space           toggle and move down
  +  -  *                  select all, none, invert

Files
  F3   view file (first 64 KiB; XML and JSON highlighted)
  F4   sync: make the other pane's directory match this one
       (dry-run plan first; incremental or mirror; works for n1,n2 / @role)
  F5   copy selection to the other pane, or to n1,n2 or every node
       (Ctrl+A in the dialog = @all)
  F6   move: copy, then delete the source if the copy fully succeeded
  F7   make directory
  F8   delete selection (asks first)

Jobs
  F9   focus the jobs panel: ↑↓ select, Enter details,
       c cancel, u catch up detached targets, Esc back

  F1 help · F10 or Ctrl+C quit

Jobs run in the node daemons and keep running after you quit.`)

// syncDialog sets up a sync of the active pane's directory.
type syncDialog struct {
	srcNode, srcRoot, srcPath  string
	inputs                     [3]textinput.Model // nodes, root, dir
	focus                      int
	mirror, checksum, compress bool
	err                        string
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

func (d *syncDialog) request() (api.SyncRequest, error) {
	nodes := strings.FieldsFunc(d.inputs[0].Value(), func(r rune) bool { return r == ',' || r == ' ' })
	root := strings.TrimSpace(d.inputs[1].Value())
	if len(nodes) == 0 || root == "" {
		return api.SyncRequest{}, fmt.Errorf("enter destination node(s) and root")
	}
	dir := strings.Trim(strings.TrimSpace(d.inputs[2].Value()), "/")
	if dir == "" {
		dir = "."
	}
	mode := api.SyncIncremental
	if d.mirror {
		mode = api.SyncMirror
	}
	return api.SyncRequest{SrcNode: d.srcNode, SrcRoot: d.srcRoot, SrcPath: d.srcPath,
		DstNodes: nodes, DstRoot: root, DstPath: dir, Mode: mode, Checksum: d.checksum, Compress: d.compress}, nil
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
		req, err := d.request()
		if err != nil {
			d.err = err.Error()
			return false, nil
		}
		return true, m.planSync(req)
	}
	var cmd tea.Cmd
	d.inputs[d.focus], cmd = d.inputs[d.focus].Update(msg)
	return false, cmd
}

func (d *syncDialog) view(st *styles, width, _ int) string {
	w := min(width-6, 76)
	var b strings.Builder
	b.WriteString(st.dialogTitle.Render(fmt.Sprintf("Sync %s → (make the destination match it)", clip(d.srcNode+":"+d.srcRoot+"/"+d.srcPath, w-40))) + "\n\n")
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2])
	for i := range d.inputs {
		d.inputs[i].Width = w - 14
		b.WriteString(d.inputs[i].View() + "\n")
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

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// syncConfirm shows a plan; y applies it.
func syncConfirm(m *model, p *api.SyncPlan) dialog {
	var lines []string
	over := false
	for _, t := range p.Targets {
		l := fmt.Sprintf("%-12s copy %d (%s)  replace %d  delete %d  unchanged %d",
			t.Node, t.Copy, humanSize(t.CopyBytes), t.Replace, t.Delete, t.Unchanged)
		if t.OverLimit {
			l += fmt.Sprintf("  ⚠ over the delete limit %d", p.MaxDeletes)
			over = true
		}
		lines = append(lines, l)
	}
	for _, s := range p.Skipped {
		lines = append(lines, "skipped "+s+" (offline)")
	}
	for _, pr := range p.Problems {
		lines = append(lines, "problem: "+pr)
	}
	title := fmt.Sprintf("Sync plan (%s, source %d entries): apply?", p.Request.Mode, p.SrcEntries)
	if over {
		title += " Deletions exceed the limit; y applies anyway."
	}
	id := p.ID
	return &confirmDialog{title: title, lines: lines, onYes: func() tea.Cmd {
		return func() tea.Msg {
			ap, err := m.c.ApplySync(m.ctx, id, over)
			if err != nil {
				return statusMsg{text: "sync: " + err.Error(), err: true}
			}
			text := fmt.Sprintf("sync: %d transfer jobs submitted", len(ap.Jobs))
			if len(ap.DeleteErrors) > 0 {
				return statusMsg{text: fmt.Sprintf("%s; %d deletions failed: %s", text, len(ap.DeleteErrors), ap.DeleteErrors[0]), err: true}
			}
			var ts []target
			for _, t := range ap.Targets {
				ts = append(ts, target{t.Node, ap.Request.DstRoot})
			}
			return statusMsg{text: text, refresh: ts}
		}
	}}
}
