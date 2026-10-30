package tui

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"fkbr/internal/api"
	"fkbr/internal/exclude"
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
	inputs   [4]textinput.Model // nodes, root, dir, exclude
	focus    int
	conflict int
	compress bool
	err      string
	dest     []string // the marked nodes, for @dest
}

// excl prefills the exclude field (space-separated patterns, see package
// exclude); the TUI keeps the last one used.
func newTransferDialog(move bool, src *pane, names []string, dstNodes, dstRoot, dstDir, excl string) *transferDialog {
	d := &transferDialog{move: move, srcNode: src.node, srcRoot: src.root}
	for _, n := range names {
		d.paths = append(d.paths, src.relPath(n))
	}
	labels := []string{"to node(s): ", "root:       ", "directory:  ", "exclude:    "}
	values := []string{dstNodes, dstRoot, dstDir, excl}
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
	ex, err := excludes(d.inputs[3].Value())
	if err != nil {
		return api.TransferRequest{}, err
	}
	if d.move && len(ex) > 0 {
		return api.TransferRequest{}, errMoveExclude
	}
	return api.TransferRequest{
		SrcNode: d.srcNode, SrcRoot: d.srcRoot, Paths: d.paths,
		DstNodes: nodes, DetachOffline: viaDest, DstRoot: root, DstDir: dir,
		Conflict: conflictPolicies[d.conflict], Compress: d.compress, Exclude: ex,
	}, nil
}

// errMoveExclude: a move deletes its sources once copied, so what an
// exclude left out would be deleted without a copy.
var errMoveExclude = errors.New("a move cannot exclude: what is left out would be deleted with the sources (copy with F5 instead)")

// excludes splits the exclude field into patterns and checks them.
func excludes(field string) ([]string, error) {
	pats := strings.Fields(field)
	_, err := exclude.New(pats)
	return pats, err
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
		m.lastExclude = d.inputs[3].Value()
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

// focusLine puts the cursor in the field shown on line (a mouse click).
func (d *transferDialog) focusLine(line string) bool {
	return focusLine(d.inputs[:], line, d.setFocus)
}

func focusLine(inputs []textinput.Model, line string, setFocus func(int)) bool {
	for i, in := range inputs {
		if strings.HasPrefix(line, strings.TrimSpace(in.Prompt)) {
			setFocus(i)
			return true
		}
	}
	return false
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
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2], &d.inputs[3])
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
	// onList, if set, offers "l = list all" (a sync plan's entries).
	onList func() tea.Cmd
}

func (d *confirmDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		return true, d.onYes()
	case "l", "L":
		if d.onList != nil {
			return false, d.onList()
		}
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
	if d.onList != nil {
		b.WriteString(" · l = list all")
	}
	return b.String()
}

// confirmHint ends a confirm dialog; a click on an answer works too.
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
	// flow word-wraps to the width (the help) instead of cutting lines.
	flow bool
	// back is the dialog to return to on close (a list opened from it).
	back dialog
	// file is the file a viewer shows: Ctrl+E edits it (editor.go).
	file *viewedFile
	st   *styles
	// Ctrl+W finds text: finding is the prompt while open, plain the
	// lines as shown (no colours), match the one found (-1: none).
	finding  *textinput.Model
	lastFind string
	plain    []string
	match    int
	note     string
}

// setText replaces the text, keeping the scroll position.
func (d *textDialog) setText(text string) {
	d.text = text
	if d.ready {
		d.setContent(d.vp.Width)
	}
}

func (d *textDialog) setContent(w int) {
	c := d.content(w)
	d.vp.SetContent(c)
	d.plain = strings.Split(ansi.Strip(c), "\n")
}

// find shows the next line with q after the one found last (or the top
// of the view), from the top again past the end.
func (d *textDialog) find(q string) {
	match := matcher(q)
	start := d.vp.YOffset
	if d.match >= 0 {
		start = d.match + 1
	}
	n := len(d.plain)
	for k := range n {
		i := (start + k) % n
		if match(d.plain[i]) < 0 {
			continue
		}
		d.match, d.note = i, ""
		if start+k >= n {
			d.note = "found from the top"
		}
		d.vp.SetYOffset(max(0, i-2))
		return
	}
	d.match, d.note = -1, "not found: "+q
}

func (d *textDialog) content(w int) string {
	if d.lang != "" && d.st != nil {
		return highlight(d.st, d.lang, d.text, w)
	}
	if d.flow {
		return flowText(d.text, w-1) // a cell between the text and the scrollbar
	}
	return wrapText(d.text, w)
}

func newTextDialog(title, text string) *textDialog {
	return &textDialog{title: safetext.Clean(title), text: safetext.Text(text), match: -1}
}

// newHelpDialog is a text dialog whose paragraphs flow with the window.
func newHelpDialog(title, text string) *textDialog {
	d := newTextDialog(title, text)
	d.flow = true
	return d
}

func (d *textDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	if d.finding != nil {
		switch msg.String() {
		case "enter":
			q := d.finding.Value()
			if q == "" {
				q = d.lastFind
			}
			d.finding = nil
			if q != "" {
				d.lastFind = q
				d.find(q)
			}
		case "esc", "ctrl+w":
			d.finding = nil
		default:
			var cmd tea.Cmd
			*d.finding, cmd = d.finding.Update(msg)
			return false, cmd
		}
		return false, nil
	}
	d.note = ""
	switch msg.String() {
	case "ctrl+w":
		d.finding = newFindInput(d.lastFind)
		return false, nil
	case "esc", "q", "f3", "f10", "enter":
		return true, nil
	case "ctrl+e":
		if d.file == nil {
			return false, nil
		}
		if why := d.file.editable(); why != "" {
			m.setStatus("edit: "+why, true)
			return false, nil
		}
		m.dlg = newEditDialog(d)
		return false, nil
	}
	var cmd tea.Cmd
	d.vp, cmd = d.vp.Update(msg)
	return false, cmd
}

func (d *textDialog) view(st *styles, width, height int) string {
	w, h := max(20, width-6), max(3, height-6)
	d.st = st
	// The text leaves the last column to the scrollbar (scrollbar.go).
	if !d.ready || d.vp.Width != w-1 || d.vp.Height != h {
		d.vp = viewport.New(w-1, h)
		d.setContent(w - 1)
		d.ready = true
	}
	pct := int(d.vp.ScrollPercent() * 100)
	lines := strings.Split(d.vp.View(), "\n")
	bar := st.bar(len(lines), d.vp.TotalLineCount(), d.vp.YOffset)
	for i, l := range lines {
		if at := d.vp.YOffset + i; at == d.match && at < len(d.plain) {
			l = st.highlight(st.inputCursor, fit(d.plain[at], w-1)) // the line found
		}
		if gap := w - 1 - lipgloss.Width(l); gap > 0 {
			l += st.inputLabel.Render(strings.Repeat(" ", gap))
		}
		lines[i] = l + bar[i]
	}
	foot := "↑↓ PgUp PgDn scroll · ^W find · Esc close"
	if d.file != nil {
		foot = "↑↓ PgUp PgDn scroll · ^W find · ^E edit · Esc close"
	}
	switch {
	case d.finding != nil:
		d.finding.Width = max(10, w-8)
		st.styleInputs(0, d.finding)
		foot = d.finding.View()
	case d.note != "":
		foot = d.note
	}
	if d.finding == nil {
		foot = fit(foot, w)
	} else {
		foot = padTo(st, foot, w) // coloured: fit would count the escapes
	}
	return st.dialogTitle.Render(fit(d.title, w-8)+fmt.Sprintf(" %5d%%", pct)) + "\n" + strings.Join(lines, "\n") + "\n" + foot
}

// padTo pads a coloured line to w cells with the dialog's background.
func padTo(st *styles, s string, w int) string {
	return s + st.inputLabel.Render(strings.Repeat(" ", max(0, w-lipgloss.Width(s))))
}

// flowText word-wraps s to w cells for the help: a paragraph fills the
// width with its indent kept; a key row ("  Enter          open ...")
// continues under its description.
func flowText(s string, w int) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		body := strings.TrimLeft(line, " ")
		hang := len(line) - len(body)
		if i := strings.Index(body, "  "); i > 0 && hang+i < 20 {
			gap := len(body[i:]) - len(strings.TrimLeft(body[i:], " "))
			hang += i + gap
		}
		prefix, rest := line[:hang], line[hang:] // hang is in bytes ("↑ ↓")
		indent := min(runewidth.StringWidth(prefix), w/2)
		cont := strings.Repeat(" ", indent)
		cur := prefix
		for _, word := range strings.Fields(rest) {
			ww := runewidth.StringWidth(word)
			switch {
			case cur == prefix:
			case runewidth.StringWidth(cur)+1+ww <= w:
				cur += " "
			default:
				out = append(out, cur)
				cur = cont
			}
			for runewidth.StringWidth(cur)+ww > w && w-runewidth.StringWidth(cur) > 0 { // a word longer than the line
				cut := runewidth.Truncate(word, w-runewidth.StringWidth(cur), "")
				out = append(out, cur+cut)
				word = word[len(cut):]
				ww = runewidth.StringWidth(word)
				cur = cont
			}
			cur += word
		}
		out = append(out, cur)
	}
	return strings.Join(out, "\n")
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

// sshHelp is the help for linking hosts, on an ssh swarm's home node.
func (m *model) sshHelp() string {
	if m.linker() == nil {
		return ""
	}
	return product.Expand(`

SSH HOSTS
  In the node list (F2), hosts this node reaches with ssh:
  F7             link one: web01, or web01:/srv/www to share that
  F8             unlink the one under the cursor
  www=web01:/srv/www names its node www (else it is named after the host; two directories on one host need a name each).
  A linked host is kept in the config file ($FKBR_CONFIG, {name}.yaml beside {name}, else ~/.{name}.conf) and linked again whenever {name} starts without a share. The host needs {name} too, and ssh must log in without a password (a key or the agent).`)
}

// helpText is the F1 screen. Lines stay under 72 columns so it reads well
// in an 80-column terminal.
func (m *model) helpText() string {
	// Paragraphs are one line each: flowText wraps them to the window.
	jobs := `  Copies run on the nodes, not in this window. A daemon keeps going after you quit.`
	if m.runsNode {
		jobs = `  This window is node ` + m.c.Self() + ` (no daemon runs it). Quitting asks first if it has running jobs; its part pauses and resumes the next time ` + product.Name + ` runs here. Nothing is lost.`
	}
	text := product.Expand(`
MOVING AROUND
  ↑ ↓  k j       move
  PgUp PgDn      a page up or down
  Home End       first or last entry
  Enter          open a folder, node or root; on a file: view it
  Backspace      up a folder; from the top of this host's shared folder on up to / (root local), then the node list
  Tab            switch pane
  F2             go to the node list
  Ctrl+R         reload this pane

  Names end in / for a folder and @ for a link; ../ is one up.

  In an ssh swarm ({name} ssh:host1,host2), a node whose link is down says why in the node list.

MARKING
  Insert, Space  mark and move down
  + - *          mark all, none, invert
  In a node list, the marked nodes are @dest: the destination that F5 and F4 offer until you unmark them.

MOUSE
  click          move there (in either pane or the jobs)
  double-click   open, like Enter
  right-click    mark or unmark; hold and drag for more rows
  wheel          move up or down; scroll a dialog
  scrollbar      click above or below the thumb: a page; drag it
  key bar        a click presses that key
  in a dialog    click a key it names (Enter, Esc, ^O) or a setting
  click outside  close a dialog
  Shift+drag     select text (-mouse=false: no Shift needed)

FILES
  F3             view (64 KiB); XML, JSON, YAML, shell, Python coloured
  F5             copy the marked entries, or the one under the cursor
  F6             move: copy, then delete here once every copy succeeded
  F7             new folder
  F8             delete (asks first)

  In the F5/F6 dialog, "to" is the other pane, a node, n1,n2, @dest, @all or @role. "exclude" leaves out what matches, patterns separated by spaces: *.tmp or .git (a name anywhere), build/out (a path from the top of what you copy), cache/ (folders only). The last ones you used are offered again.
  Ctrl+A         to every node (@all)
  Ctrl+O         if a file exists: skip, overwrite if older, overwrite
  Ctrl+Z         compress on the way (zstd)

VIEW AND EDIT (F3)
  The viewer shows the first 64 KiB of a file from any node, in colour where it knows the language. Ctrl+E edits a whole text file up to 64 KiB there; a save checks first that nobody changed the file meanwhile.
  Ctrl+W         find (Enter alone: the next); also in this help
  Ctrl+E         edit the file
  In the editor:
  Ctrl+S, Ctrl+O save
  Ctrl+X, Esc    close (asks if not saved)
  Ctrl+K         cut the line; again: the next one too
  Ctrl+U         paste the cut lines above this one
  Ctrl+Z, Ctrl+Y undo, redo
  Ctrl+W         find
  click          put the cursor there

SYNC (F4)
  Makes the other side match this pane. With nothing marked, the whole folder is synced; marked folders each go to a folder of the same name, marked files are copied. Incremental (the default) sends new and changed files and keeps extra ones; mirror (Ctrl+O) also deletes what the source does not have, but only inside what you sync. You always see a dry run first and confirm it.
  Ctrl+O         incremental or mirror
  Ctrl+K         compare by checksum, not size and time
  Ctrl+N         keep files that are newer on the destination
  l              in the plan: list everything it copies and deletes

  Excluded entries are neither copied nor deleted, on either side.

JOBS (F9)
  ↑ ↓            choose a job
  Enter          details: time taken, progress, ETA, every node, notes
  c              cancel
  u              send again to targets that were offline or failed
  Esc            back

`) + jobs + m.sshHelp() + `

SETTINGS (F11)
  The settings people change most: colours and theme, mouse, what the copy dialogs start with, this node's name, share, roles and limits. Kept in ~/.{name}.conf, or in {name}.conf beside the binary if there is one; only what you change is written. Colours, theme and mouse show as you pick them (^X on leaving puts them back); the TUI's other settings apply when saved, the node's when it starts again.
  ↑ ↓            choose a setting
  Enter, ← →     change it: the next choice, or type a value
  d              back to the default
  Ctrl+S         save
  o              also opens it, where the terminal takes F11

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
	inputs                     [4]textinput.Model // nodes, root, dir, exclude
	focus                      int
	mirror, checksum, compress bool
	keepNewer                  bool // keep target files newer than the source's
	err                        string
	dest                       []string // the marked nodes, for @dest
	// dirs and files are the selected entries; none means the whole
	// directory. Each folder syncs to one of the same name; files are copied.
	dirs, files []string
}

func newSyncDialog(src *pane, dstNodes, dstRoot, dstDir, excl string) *syncDialog {
	d := &syncDialog{srcNode: src.node, srcRoot: src.root, srcPath: src.path}
	labels := []string{"to node(s): ", "root:       ", "directory:  ", "exclude:    "}
	values := []string{dstNodes, dstRoot, dstDir, excl}
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

func (d *syncDialog) setFocus(i int) {
	d.inputs[d.focus].Blur()
	d.focus = i
	d.inputs[d.focus].Focus()
}

// focusLine puts the cursor in the field shown on line (a mouse click).
func (d *syncDialog) focusLine(line string) bool {
	return focusLine(d.inputs[:], line, d.setFocus)
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
	ex, err := excludes(d.inputs[3].Value())
	if err != nil {
		return nil, nil, err
	}
	base := api.SyncRequest{SrcNode: d.srcNode, SrcRoot: d.srcRoot, SrcPath: d.srcPath,
		DstNodes: nodes, DetachOffline: viaDest, DstRoot: root, DstPath: dir, Mode: mode, Checksum: d.checksum, Compress: d.compress,
		Exclude: ex, KeepNewer: d.keepNewer}
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
		d.setFocus((d.focus + 1) % len(d.inputs))
		return false, nil
	case "shift+tab", "up":
		d.setFocus((d.focus + len(d.inputs) - 1) % len(d.inputs))
		return false, nil
	case "ctrl+o":
		d.mirror = !d.mirror
		return false, nil
	case "ctrl+k":
		d.checksum = !d.checksum
		return false, nil
	case "ctrl+n":
		d.keepNewer = !d.keepNewer
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
		m.lastExclude = d.inputs[3].Value()
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
	st.styleInputs(d.focus, &d.inputs[0], &d.inputs[1], &d.inputs[2], &d.inputs[3])
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
	keep := "off (newer files there are replaced)"
	if d.keepNewer {
		keep = "on (newer files there are kept)"
	}
	fmt.Fprintf(&b, "keep newer: %s\n", keep)
	if d.err != "" {
		b.WriteString("\n" + st.dialogErr.Render("! "+d.err) + "\n")
	}
	b.WriteString("\nEnter plan (dry run first) · Tab next field · Esc cancel\n^A all nodes · ^O mode · ^K checksum · ^Z zstd · ^N keep newer")
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
// planPreview is how many entries of each list a sync's confirm shows.
const planPreview = 5

func syncConfirm(m *model, plans []*api.SyncPlan, files *api.TransferRequest, previews map[string]*api.SyncEntries) dialog {
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
		if len(p.Request.Exclude) > 0 {
			lines = append(lines, indent+"excluding: "+strings.Join(p.Request.Exclude, " "))
		}
		for _, t := range p.Targets {
			l := fmt.Sprintf("%s%-12s copy %d (%s)  replace %d  delete %d  unchanged %d",
				indent, t.Node, t.Copy, humanSize(t.CopyBytes), t.Replace, t.Delete, t.Unchanged)
			if t.Kept > 0 {
				l += fmt.Sprintf("  kept newer %d", t.Kept)
			}
			if t.OverLimit {
				l += fmt.Sprintf("  ⚠ over the delete limit %d", p.MaxDeletes)
				over = true
			}
			lines = append(lines, l)
			if e := previews[p.ID]; e != nil {
				for _, d := range e.Deletes {
					if d.Node == t.Node && len(d.Paths) > 0 {
						l := indent + "  delete: " + strings.Join(d.Paths, ", ")
						if d.More > 0 {
							l += fmt.Sprintf(" … and %d more", d.More)
						}
						lines = append(lines, l)
					}
				}
			}
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
	c := &confirmDialog{title: title, lines: lines}
	c.onList = func() tea.Cmd {
		return func() tea.Msg {
			var b strings.Builder
			for _, p := range plans {
				e, err := m.c.SyncPlanEntries(m.ctx, p.ID, 0)
				if err != nil {
					return statusMsg{text: "sync plan: " + err.Error(), err: true}
				}
				if len(plans) > 1 {
					b.WriteString(path.Base(p.Request.SrcPath) + "/\n")
				}
				writeEntries(&b, e)
			}
			t := newTextDialog("What the sync does (Esc: back to the plan)", b.String())
			t.back = c
			return openMsg{t}
		}
	}
	c.onYes = func() tea.Cmd {
		return func() tea.Msg {
			jobs, notSent, why := 0, 0, ""
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
				if n := len(ap.Groups) - len(ap.Jobs); n > 0 {
					// Not submitted (a target went offline since the
					// plan): ApplySync says why after the plan's own notes.
					notSent += n
					if extra := ap.Problems[min(len(p.Problems), len(ap.Problems)):]; len(extra) > 0 && why == "" {
						why = extra[0]
					}
				}
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
			if notSent > 0 {
				return statusMsg{text: fmt.Sprintf("%s; %d not: %s", text, notSent, why), err: true}
			}
			if len(problems) > 0 {
				return statusMsg{text: fmt.Sprintf("%s; %d deletions failed: %s", text, len(problems), problems[0]), err: true}
			}
			return statusMsg{text: text, refresh: ts}
		}
	}
	return c
}

// writeEntries writes a plan's lists, one path per line.
func writeEntries(b *strings.Builder, e *api.SyncEntries) {
	show := func(title string, l api.SyncEntryList) {
		fmt.Fprintf(b, "%s (%d):\n", title, int64(len(l.Paths))+l.More)
		for _, p := range l.Paths {
			b.WriteString("  " + p + "\n")
		}
		b.WriteString("\n")
	}
	for _, d := range e.Deletes {
		show("delete on "+d.Node, d)
	}
	for _, g := range e.Copies {
		show("copy to "+strings.Join(g.Targets, ","), g)
	}
}
