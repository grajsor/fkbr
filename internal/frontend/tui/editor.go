package tui

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"

	"fkbr/internal/api"
	"fkbr/internal/safetext"
)

// The editor: Ctrl+E in the file viewer (not in the help yet). A small
// nano: type, move, save with Ctrl+S (or Ctrl+O); Esc goes back to the
// view, asking first about changes not saved.

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
	asking   bool // Esc with changes not saved: save them?
	closing  bool // close once the save in progress is done
	note     string
	noteErr  bool
}

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
	return d
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
	req := api.WriteRequest{Node: f.node, Root: f.root, Path: f.path, Data: d.text(), Size: f.res.Size, ModTime: f.res.ModTime}
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
	d.modified, d.note, d.noteErr = false, fmt.Sprintf("saved, %d bytes", len(data)), false
	if d.closing {
		return d.back
	}
	return d
}

func (d *editDialog) update(m *model, msg tea.KeyMsg) (bool, tea.Cmd) {
	if d.asking {
		switch msg.String() {
		case "y", "Y":
			d.asking, d.closing = false, true
			return false, d.save(m)
		case "n", "N":
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
	switch msg.String() {
	case "esc":
		if !d.modified {
			m.dlg = d.back
			return false, nil
		}
		d.asking = true
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
	var out []string
	for i := d.top; i < d.top+h; i++ {
		if i >= len(d.lines) {
			out = append(out, text.Render(strings.Repeat(" ", w)))
			continue
		}
		shown, at := cells(d.lines[i])
		// The cursor's line scrolls sideways to keep the cursor in sight;
		// the others show their start.
		left := 0
		if i == d.row && at[d.col] >= w {
			left = at[d.col] - w + 1
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
		out = append(out, b.String())
	}
	mark := ""
	if d.modified {
		mark = " *"
	}
	pos := fmt.Sprintf(" %d:%d", d.row+1, d.col+1)
	title := st.dialogTitle.Render(fit("edit "+d.title+mark, w-len(pos)) + pos)
	hint := "Ctrl+S save · Esc back to the view"
	switch {
	case d.asking:
		hint = "Save the changes? y = save · n = throw them away · Esc = keep editing"
	case d.note != "":
		hint = d.note
	}
	foot := fit(hint, w)
	if d.noteErr && !d.asking {
		foot = st.dialogErr.Render(fit(hint, w))
	}
	return title + "\n" + strings.Join(out, "\n") + "\n" + foot
}
