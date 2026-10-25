package tui

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"github.com/charmbracelet/bubbles/textinput"

	"fkbr/internal/api"
	"fkbr/internal/safetext"
)

// The editor: Ctrl+E in the file viewer. A small nano: type, move, cut
// lines with Ctrl+K and paste them with Ctrl+U, save with Ctrl+S (or
// Ctrl+O); Esc or Ctrl+X goes back to the view, asking first about changes
// not saved. Lines are numbered down the left.

// viewedFile is the file a viewer shows, as read.
type viewedFile struct {
	node, root, path string
	res              *api.ReadResult
}

// editable says why a viewed file cannot be edited, or "".
func (f *viewedFile) editable() string {
	switch {
	case !f.res.EOF:
		return fmt.Sprintf("only files up to %d KiB can be edited", viewLimit>>10)
	case bytes.IndexByte(f.res.Data, 0) >= 0:
		return "a binary file cannot be edited"
	case !utf8.Valid(f.res.Data):
		return "only UTF-8 text can be edited"
	}
	return ""
}

// editTab is how far a tab moves the text on, as in the viewer.
const editTab = 4

type editDialog struct {
	file     *viewedFile
	back     *textDialog // the viewer, shown again on Esc
	title    string
	lines    [][]rune
	crlf     bool // the file's lines end in \r\n; saved the same way
	row, col int  // cursor: line and rune in it
	top      int  // first line shown
	height   int  // lines shown at the last view, for PgUp/PgDn
	modified bool
	saving   bool
	asking   bool // Esc or Ctrl+X with changes not saved: save them?
	closing  bool // close once the save in progress is done
	note     string
	noteErr  bool
	// cut holds the lines Ctrl+K cut; consecutive Ctrl+Ks add to it, as
	// in nano, any other key ends that (cutting starts it afresh).
	cut    [][]rune
	cutRun bool
	// undo and redo hold the text as it was before each edit (Ctrl+Z,
	// Ctrl+Y); a run of typing, of Backspace, Delete or Ctrl+K is one
	// step (lastEdit), until another key comes between. savedText is the
	// text as last read or saved: the same text again is not modified.
	undo, redo []editState
	// finding is the Ctrl+W prompt while it is open; lastFind what it
	// looked for last (Enter on an empty prompt looks again).
	finding  *textinput.Model
	lastFind string
	// gutter and left are the line numbers' width and how far the cursor's
	// line scrolled sideways at the last view, for clicks.
	gutter, left int
	lastEdit     string
	edits        int // counts changes, to tell whether a key made one
	savedText    string
}

type editState struct {
	lines    [][]rune
	row, col int
}

// maxUndo bounds the steps kept; past it the oldest after the first
// are merged.
const maxUndo = 500

func newEditDialog(back *textDialog) *editDialog {
	text := string(back.file.res.Data)
	d := &editDialog{file: back.file, back: back, title: back.title}
	if strings.Contains(text, "\r\n") && strings.Count(text, "\r\n") == strings.Count(text, "\n") {
		d.crlf = true
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	for _, l := range strings.Split(text, "\n") {
		d.lines = append(d.lines, []rune(l))
	}
	d.savedText = string(d.text())
	return d
}

func (d *editDialog) state() editState {
	lines := make([][]rune, len(d.lines))
	for i, l := range d.lines {
		lines[i] = append([]rune(nil), l...)
	}
	return editState{lines, d.row, d.col}
}

func (d *editDialog) restore(st editState) {
	d.lines, d.row, d.col = st.lines, st.row, st.col
	d.modified = string(d.text()) != d.savedText
}

// undoKind is the undo step a key's edit belongs to ("": no edit).
func undoKind(key string, msg tea.KeyMsg) string {
	switch key {
	case "ctrl+k":
		return "cut"
	case "ctrl+u":
		return "paste"
	case "enter":
		return "enter"
	case "backspace", "ctrl+h":
		return "back"
	case "delete", "ctrl+d":
		return "del"
	case "tab":
		return "type"
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		return "type"
	}
	return ""
}

func (d *editDialog) text() []byte {
	nl := "\n"
	if d.crlf {
		nl = "\r\n"
	}
	parts := make([]string, len(d.lines))
	for i, l := range d.lines {
		parts[i] = string(l)
	}
	return []byte(strings.Join(parts, nl))
}

// editSavedMsg is the answer to a save.
type editSavedMsg struct {
	d   *editDialog
	res *api.WriteResult
	err error
}

func (d *editDialog) save(m *model) tea.Cmd {
	if d.saving {
		return nil
	}
	d.saving, d.note, d.noteErr = true, "saving…", false
	f := d.file
	req := api.WriteRequest{Node: f.node, Root: f.root, Path: f.path, Data: d.text(),
		Size: f.res.Size, Sum: api.ContentSum(f.res.Data), ModTime: f.res.ModTime}
	return func() tea.Msg {
		res, err := m.c.Write(m.ctx, req)
		return editSavedMsg{d: d, res: res, err: err}
	}
}

// saved takes the answer to a save; it returns the dialog to show next.
func (d *editDialog) saved(msg editSavedMsg) dialog {
	d.saving = false
	if msg.err != nil {
		d.note, d.noteErr, d.closing = "not saved: "+msg.err.Error(), true, false
		return d
	}
	data := d.text()
	d.file.res = &api.ReadResult{Data: data, Size: msg.res.Size, EOF: true, ModTime: msg.res.ModTime}
	d.back.setText(renderFile(d.file.res))
	d.savedText = string(data)
	d.modified, d.note, d.noteErr = false, fmt.Sprintf("saved, %d bytes", len(data)), false
	for _, n := range msg.res.Notes {
		d.note += "; " + n
	}
	if d.closing {
		return d.back
	}
	return d
}

func (d *editDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
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
	if d.asking {
		// Answered with Ctrl keys only: one typed on after Esc must not
		// save the file or throw the changes away (a stray y saved).
		switch msg.String() {
		case "ctrl+s", "ctrl+o":
			d.asking, d.closing = false, true
			return false, d.save(m)
		case "ctrl+x":
			m.dlg = d.back
			return false, nil
		case "esc":
			d.asking, d.note = false, ""
		}
		return false, nil
	}
	if d.saving {
		return false, nil // the text stays as sent until the answer
	}
	d.note = ""
	key := msg.String()
	if key != "ctrl+k" {
		d.cutRun = false
	}
	// One undo step for a run of edits of one kind; a new run (or a paste,
	// or Enter) starts a step with the text as it was before it.
	kind := undoKind(key, msg)
	pushed, before := false, d.edits
	if kind != "" && (kind != d.lastEdit || kind == "paste" || kind == "enter") {
		d.undo = append(d.undo, d.state())
		if len(d.undo) > maxUndo {
			// The first step (the file as opened) stays: the two after it
			// become one, so undoing always gets back to the start.
			d.undo = append(d.undo[:1], d.undo[2:]...)
		}
		pushed = true
	}
	d.lastEdit = kind
	defer func() {
		switch {
		case pushed && d.edits == before: // the key changed nothing
			d.undo = d.undo[:len(d.undo)-1]
		case d.edits != before:
			d.redo = nil
			d.modified = string(d.text()) != d.savedText
		}
	}()
	switch key {
	case "ctrl+z":
		if n := len(d.undo); n > 0 {
			d.redo = append(d.redo, d.state())
			d.restore(d.undo[n-1])
			d.undo = d.undo[:n-1]
		}
		return false, nil
	case "ctrl+y":
		if n := len(d.redo); n > 0 {
			d.undo = append(d.undo, d.state())
			d.restore(d.redo[n-1])
			d.redo = d.redo[:n-1]
		}
		return false, nil
	case "ctrl+k":
		d.cutLine()
	case "ctrl+u":
		d.paste()
	case "esc", "ctrl+x":
		if !d.modified {
			m.dlg = d.back
			return false, nil
		}
		d.asking = true
		return false, nil
	case "ctrl+w":
		d.finding = newFindInput(d.lastFind)
		return false, nil
	case "ctrl+s", "ctrl+o":
		return false, d.save(m)
	case "up":
		d.row--
	case "down":
		d.row++
	case "left":
		if d.col > 0 {
			d.col--
		} else if d.row > 0 {
			d.row--
			d.col = len(d.lines[d.row])
		}
	case "right":
		if d.col < len(d.lines[d.row]) {
			d.col++
		} else if d.row < len(d.lines)-1 {
			d.row, d.col = d.row+1, 0
		}
	case "home", "ctrl+a":
		d.col = 0
	case "end":
		d.col = len(d.lines[d.row])
	case "pgup":
		d.row -= max(1, d.height-1)
	case "pgdown":
		d.row += max(1, d.height-1)
	case "ctrl+home":
		d.row, d.col = 0, 0
	case "ctrl+end":
		d.row = len(d.lines) - 1
		d.col = len(d.lines[d.row])
	case "enter":
		d.insert([]rune{'\n'})
	case "tab":
		d.insert([]rune{'\t'})
	case "backspace", "ctrl+h":
		d.backspace()
	case "delete", "ctrl+d":
		d.delete()
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			d.insert(msg.Runes)
		}
	}
	d.row = max(0, min(d.row, len(d.lines)-1))
	d.col = max(0, min(d.col, len(d.lines[d.row])))
	return false, nil
}

// newFindInput is the Ctrl+W prompt; it names what Enter alone looks for
// again.
func newFindInput(last string) *textinput.Model {
	ti := textinput.New()
	ti.Prompt = "Find: "
	if last != "" {
		ti.Prompt = "Find [" + safetext.Clean(clip(last, 30)) + "]: "
	}
	ti.CharLimit = 1024
	ti.Focus()
	return &ti
}

// matcher finds q in a line: without capitals in q, case does not count
// (as in less and vim's smartcase).
func matcher(q string) func(line string) int {
	if strings.ToLower(q) == q {
		return func(line string) int { return strings.Index(strings.ToLower(line), q) }
	}
	return func(line string) int { return strings.Index(line, q) }
}

// find puts the cursor on the next match of q after it: on to the end,
// then from the top (saying so) up to the cursor.
func (d *editDialog) find(q string) {
	match := matcher(q)
	n := len(d.lines)
	for k := 0; k <= n; k++ {
		i := (d.row + k) % n
		line := string(d.lines[i])
		from := 0
		if k == 0 { // the cursor's line: after the cursor
			from = len(string(d.lines[i][:min(d.col+1, len(d.lines[i]))]))
		}
		at := match(line[from:])
		if at < 0 {
			continue
		}
		pos := utf8.RuneCountInString(line[:from+at])
		if k == n && pos > d.col { // round to the cursor's line again: only before it
			continue
		}
		d.note, d.noteErr = "", false
		if d.row+k >= n {
			d.note = "found from the top"
		}
		d.row, d.col = i, pos
		if d.row < d.top || d.row >= d.top+d.height { // off screen: a third of the way down
			d.top = max(0, d.row-d.height/3)
		}
		return
	}
	d.note, d.noteErr = "not found: "+q, true
}

// clickAt puts the cursor where a click in the text landed (row and col
// in the dialog's lines: the title is row 0).
func (d *editDialog) clickAt(row, col int) bool {
	i := d.top + row - 1
	if row < 1 || row > d.height || i >= len(d.lines) {
		return false
	}
	x := col - d.gutter
	if x < 0 {
		x = 0
	}
	if i == d.row {
		x += d.left
	}
	_, at := cells(d.lines[i])
	d.row, d.col = i, len(at)-1
	for j := 0; j+1 < len(at); j++ {
		if x < at[j+1] {
			d.col = j
			break
		}
	}
	d.cutRun, d.lastEdit = false, ""
	return true
}

// insert types rs at the cursor; '\n' splits the line (a paste may bring
// several, and \r\n).
func (d *editDialog) insert(rs []rune) {
	for _, r := range rs {
		if r == '\r' {
			continue
		}
		line := d.lines[d.row]
		if r == '\n' {
			rest := append([]rune(nil), line[d.col:]...)
			d.lines[d.row] = line[:d.col]
			d.lines = append(d.lines[:d.row+1], append([][]rune{rest}, d.lines[d.row+1:]...)...)
			d.row, d.col = d.row+1, 0
		} else {
			line = append(line[:d.col], append([]rune{r}, line[d.col:]...)...)
			d.lines[d.row] = line
			d.col++
		}
		d.modified = true
		d.edits++
	}
}

// cutLine cuts the cursor's line into the cut buffer (added to it
// while Ctrl+K follows Ctrl+K). The empty line after a last newline is
// not a line: cutting there does nothing.
func (d *editDialog) cutLine() {
	last := d.row == len(d.lines)-1
	if last && len(d.lines[d.row]) == 0 && len(d.lines) > 1 {
		return
	}
	if !d.cutRun {
		d.cut = nil
	}
	d.cutRun = true
	d.cut = append(d.cut, append([]rune(nil), d.lines[d.row]...))
	if last {
		d.lines[d.row] = nil // the last line: emptied, it stays
	} else {
		d.lines = append(d.lines[:d.row], d.lines[d.row+1:]...)
	}
	d.col = 0
	d.modified = true
	d.edits++
}

// paste puts the cut lines in above the cursor's line, whole, wherever
// the cursor is in it (at the cursor, as nano does, they split the line),
// as often as it is asked to.
func (d *editDialog) paste() {
	if len(d.cut) > 0 {
		d.col = 0
	}
	for _, l := range d.cut {
		d.insert(append(append([]rune(nil), l...), '\n'))
	}
}

func (d *editDialog) backspace() {
	switch {
	case d.col > 0:
		line := d.lines[d.row]
		d.lines[d.row] = append(line[:d.col-1], line[d.col:]...)
		d.col--
	case d.row > 0:
		prev := d.lines[d.row-1]
		d.col = len(prev)
		d.lines[d.row-1] = append(prev, d.lines[d.row]...)
		d.lines = append(d.lines[:d.row], d.lines[d.row+1:]...)
		d.row--
	default:
		return
	}
	d.modified = true
	d.edits++
}

func (d *editDialog) delete() {
	line := d.lines[d.row]
	switch {
	case d.col < len(line):
		d.lines[d.row] = append(line[:d.col], line[d.col+1:]...)
	case d.row < len(d.lines)-1:
		d.lines[d.row] = append(line, d.lines[d.row+1]...)
		d.lines = append(d.lines[:d.row+1], d.lines[d.row+2:]...)
	default:
		return
	}
	d.modified = true
	d.edits++
}

// cells lays a line out: what each rune shows as (tabs as spaces, control
// characters as '?') and the column it starts at.
func cells(line []rune) (shown []string, at []int) {
	x := 0
	for _, r := range line {
		s := string(r)
		switch {
		case r == '\t':
			s = strings.Repeat(" ", editTab-x%editTab)
		case unicode.IsControl(r) || runewidth.RuneWidth(r) == 0:
			s = "?"
		}
		shown = append(shown, safetext.Clean(s))
		at = append(at, x)
		x += runewidth.StringWidth(shown[len(shown)-1])
	}
	return shown, append(at, x)
}

func (d *editDialog) view(st *styles, width, height int) string {
	w, h := max(20, width-6), max(3, height-6)
	d.height = h
	if d.row < d.top {
		d.top = d.row
	}
	if d.row >= d.top+h {
		d.top = d.row - h + 1
	}
	text := st.inputLabel
	num := st.inputLabel.Foreground(cCodeComment)
	// Line numbers down the left, as wide as the last one needs.
	gutter := len(strconv.Itoa(len(d.lines))) + 1
	d.gutter, d.left = gutter, 0
	full := w
	w = max(4, full-gutter)
	var out []string
	for i := d.top; i < d.top+h; i++ {
		if i >= len(d.lines) {
			out = append(out, text.Render(strings.Repeat(" ", full)))
			continue
		}
		n := num.Render(fmt.Sprintf("%*d ", gutter-1, i+1))
		shown, at := cells(d.lines[i])
		// The cursor's line scrolls sideways to keep the cursor in sight;
		// the others show their start.
		left := 0
		if i == d.row && at[d.col] >= w {
			left = at[d.col] - w + 1
			d.left = left
		}
		var b strings.Builder
		used := 0
		for j, s := range shown {
			if at[j] < left {
				continue
			}
			sw := runewidth.StringWidth(s)
			if used+sw > w {
				break
			}
			if i == d.row && j == d.col {
				b.WriteString(st.highlight(st.inputCursor, s))
			} else {
				b.WriteString(text.Render(s))
			}
			used += sw
		}
		if i == d.row && d.col == len(shown) && used < w {
			b.WriteString(st.highlight(st.inputCursor, " "))
			used++
		}
		b.WriteString(text.Render(strings.Repeat(" ", max(0, w-used))))
		out = append(out, n+b.String())
	}
	w = full
	mark := ""
	if d.modified {
		mark = " *"
	}
	pos := fmt.Sprintf(" %d:%d", d.row+1, d.col+1)
	title := st.dialogTitle.Render(fit("edit "+d.title+mark, w-len(pos)) + pos)
	var foot string
	switch {
	case d.finding != nil:
		d.finding.Width = max(10, w-8)
		st.styleInputs(0, d.finding)
		foot = padTo(st, d.finding.View(), w) // coloured: fit would count the escapes
	case d.asking:
		foot = fit("Not saved.  ^S save and close · ^X close without saving · Esc keep editing", w)
	case d.note != "" && d.noteErr:
		foot = st.dialogErr.Render(fit(d.note, w))
	case d.note != "":
		foot = fit(d.note, w)
	default:
		foot = fit("^S save · ^X close · ^W find · ^K cut line · ^U paste · ^Z undo · ^Y redo", w)
	}
	return title + "\n" + strings.Join(out, "\n") + "\n" + foot
}
