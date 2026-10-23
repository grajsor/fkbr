package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The mouse does what keys do. In the panes and the jobs panel a click
// moves the cursor (and the focus) there and a double click opens (Enter);
// a right click marks or unmarks the row under it, as in Norton Commander,
// and dragging with it held does the same to every row it passes; the
// wheel moves the cursor. A click on the key bar presses that key. In
// a dialog the wheel scrolls, a click outside closes it (Esc), a click on
// a hint ("Enter start · Tab next field · Esc cancel") presses that key,
// one on a setting ("if exists:", "zstd:", ...) toggles it, and one on a
// field puts the cursor there.

const doubleClick = 400 * time.Millisecond

// click is the last left click on a row, for telling double clicks.
type click struct {
	area, row int // area: 0, 1 the panes, 2 the jobs panel
	at        time.Time
}

// box is where View drew the dialog: its frame, where its content starts,
// and the content's lines as plain text.
type box struct {
	x, y, w, h int
	cx, cy     int
	lines      []string
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	if m.width < 40 || m.height < 12 {
		return nil
	}
	switch msg.Action {
	case tea.MouseActionRelease:
		m.drag = dragNone
		return nil
	case tea.MouseActionMotion:
		if m.drag != dragNone {
			return m.dragTo(msg.Y)
		}
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.wheel(msg, -3)
	case tea.MouseButtonWheelDown:
		return m.wheel(msg, 3)
	case tea.MouseButtonLeft, tea.MouseButtonRight:
	default:
		return nil
	}
	if msg.Y == m.height-1 {
		if msg.Button == tea.MouseButtonLeft {
			return m.keyBarClick(msg.X)
		}
		return nil
	}
	if m.dlg != nil {
		return m.dialogClick(msg)
	}
	ph := m.paneHeight()
	if msg.Y >= 2 && msg.Y < 2+ph {
		i := 0
		if msg.X >= m.width/2 {
			i = 1
		}
		if bar := []int{m.width/2 - 1, m.width - 1}[i]; msg.X == bar && msg.Button == tea.MouseButtonLeft {
			if cmd, ok := m.paneBarClick(i, msg.Y-2); ok {
				return cmd
			}
		}
		return m.rowClick(i, msg.Y-2, msg.Button)
	}
	if jh := m.jobsHeight(); jh > 0 && msg.Y > 5+ph && msg.Y < 5+ph+jh-1 {
		return m.jobClick(msg.Y-6-ph, msg.Button)
	}
	return nil
}

// double records a left click on row of area and reports whether it
// completes a double click.
func (m *model) double(area, row int) bool {
	now := time.Now()
	if m.lastClick.area == area && m.lastClick.row == row && now.Sub(m.lastClick.at) < doubleClick {
		m.lastClick = click{}
		return true
	}
	m.lastClick = click{area, row, now}
	return false
}

func (m *model) rowClick(i, line int, b tea.MouseButton) tea.Cmd {
	p := m.panes[i]
	m.active, m.jobsFocus = i, false
	skip := 0
	if p.err != "" {
		skip = 1 // the error line above the rows
	}
	row := p.offset + line - skip
	if line < skip || row >= len(p.rows) {
		return nil
	}
	p.cursor = row
	if b == tea.MouseButtonRight {
		m.lastClick = click{}
		if r := p.current(); r != nil && p.markable(r) {
			set := m.marks(p)
			toggle(set, r.name)
			m.drag, m.markOn, m.markRow = dragMark0+drag(i), set[r.name], row
			m.destStatus(p)
		}
		return nil
	}
	if m.double(i, row) {
		return m.enter(i)
	}
	return nil
}

// markTo carries a right-button drag in pane i to screen line y: every row
// from the last one it reached to the one there gets the state the first
// row got. Above or below the pane it reaches that much further, and the
// pane scrolls with the cursor.
func (m *model) markTo(i, y int) tea.Cmd {
	p := m.panes[i]
	if len(p.rows) == 0 || p.level == levelRoots {
		return nil
	}
	skip := 0
	if p.err != "" {
		skip = 1 // the error line above the rows
	}
	last := min(m.markRow, len(p.rows)-1) // the pane may have reloaded
	row := max(0, min(len(p.rows)-1, p.offset+y-2-skip))
	set := m.marks(p)
	step := 1
	if row < last {
		step = -1
	}
	for k := last + step; k != row+step; k += step {
		switch r := p.rows[k]; {
		case !p.markable(&r):
		case m.markOn:
			set[r.name] = true
		default:
			delete(set, r.name)
		}
	}
	m.markRow, p.cursor = row, row
	m.destStatus(p)
	return m.maybeLoadMore(i)
}

func (m *model) jobClick(line int, b tea.MouseButton) tea.Cmd {
	n := m.jobsHeight() - 2
	start := 0
	if m.jobCursor >= n {
		start = m.jobCursor - n + 1
	}
	row := start + line
	if row >= len(m.jobOrder) {
		return nil
	}
	m.jobsFocus, m.jobCursor = true, row
	if b == tea.MouseButtonLeft && m.double(2, row) {
		return m.jobsKey(tea.KeyMsg{Type: tea.KeyEnter})
	}
	return nil
}

func (m *model) wheel(msg tea.MouseMsg, d int) tea.Cmd {
	k := tea.KeyMsg{Type: tea.KeyDown}
	if d < 0 {
		k, d = tea.KeyMsg{Type: tea.KeyUp}, -d
	}
	if m.dlg == nil {
		ph := m.paneHeight()
		switch {
		case msg.Y >= 2 && msg.Y < 2+ph:
			m.jobsFocus = false
			m.active = 0
			if msg.X >= m.width/2 {
				m.active = 1
			}
		case m.jobsHeight() > 0 && msg.Y > 4+ph && msg.Y < m.height-2:
			m.jobsFocus = true
		default:
			return nil
		}
	}
	var cmds []tea.Cmd
	for range d {
		_, cmd := m.Update(k)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

var fkeys = []tea.KeyType{tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6, tea.KeyF7, tea.KeyF8, tea.KeyF9, tea.KeyF10}

// keyBarClick presses the key whose cell is at column x (see keyBar).
func (m *model) keyBarClick(x int) tea.Cmd {
	cell := max(4, m.width/len(fkeys))
	if k := x / cell; k < len(fkeys) {
		_, cmd := m.Update(tea.KeyMsg{Type: fkeys[k]})
		return cmd
	}
	return nil
}

func (m *model) dialogClick(msg tea.MouseMsg) tea.Cmd {
	b := m.dlgBox
	if msg.X < b.x || msg.X >= b.x+b.w || msg.Y < b.y || msg.Y >= b.y+b.h {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		return cmd
	}
	row, col := msg.Y-b.cy, msg.X-b.cx
	if msg.Button != tea.MouseButtonLeft || row < 0 || row >= len(b.lines) || col < 0 {
		return nil
	}
	if d, ok := m.dlg.(*textDialog); ok && col == d.vp.Width && row >= 1 && row <= d.vp.Height {
		m.textBarClick(d, row-1)
		return nil
	}
	line := b.lines[row]
	// Hints end every dialog (the last two lines): a list of the keys it
	// takes, so names listed above them never count as keys.
	if k, ok := settingKey(line, col); ok || row >= len(b.lines)-2 && hintKey(line, col, &k) {
		_, cmd := m.Update(k)
		return cmd
	}
	if f, ok := m.dlg.(interface{ focusLine(string) bool }); ok {
		f.focusLine(line)
	}
	return nil
}

// hintKeys are the keys dialogs name in their hints and what a click on
// them presses.
var hintKeys = map[string]tea.KeyMsg{
	"Enter": {Type: tea.KeyEnter}, "Tab": {Type: tea.KeyTab}, "Esc": {Type: tea.KeyEsc},
	"^A": {Type: tea.KeyCtrlA}, "^O": {Type: tea.KeyCtrlO}, "^K": {Type: tea.KeyCtrlK}, "^Z": {Type: tea.KeyCtrlZ},
	"^N": {Type: tea.KeyCtrlN},
	"y":  {Type: tea.KeyRunes, Runes: []rune("y")}, "n": {Type: tea.KeyRunes, Runes: []rune("n")},
	"l": {Type: tea.KeyRunes, Runes: []rune("l")},
}

// hintKey finds the key of the hint at column col of a hint line
// ("Enter start · Tab next field · Esc cancel": the first word of the
// part clicked).
func hintKey(line string, col int, k *tea.KeyMsg) bool {
	const sep = " · "
	start := 0
	for _, part := range strings.Split(line, sep) {
		w := ansi.StringWidth(part)
		if col < start+w {
			f := strings.Fields(part)
			if col < start || len(f) == 0 {
				return false
			}
			var ok bool
			*k, ok = hintKeys[f[0]]
			return ok
		}
		start += w + ansi.StringWidth(sep)
	}
	return false
}

// settingKeys are the settings shown on a dialog's settings lines (each
// starts with one of them) and the keys that toggle them.
var settingKeys = []struct{ label, key string }{
	{"if exists:", "^O"}, {"mode:", "^O"}, {"keep newer:", "^N"}, {"checksum:", "^K"}, {"zstd:", "^Z"},
}

// settingKey is the key of the setting at column col: the nearest label
// at or left of it.
func settingKey(line string, col int) (tea.KeyMsg, bool) {
	starts := false
	for _, s := range settingKeys {
		starts = starts || strings.HasPrefix(line, s.label)
	}
	if !starts {
		return tea.KeyMsg{}, false
	}
	at, key := -1, ""
	for _, s := range settingKeys {
		if i := strings.Index(line, s.label); i >= 0 {
			if c := ansi.StringWidth(line[:i]); c <= col && c > at {
				at, key = c, s.key
			}
		}
	}
	k, ok := hintKeys[key]
	return k, ok
}

// placedBox works out where lipgloss.Place centres a block of w x h cells
// in an area of aw x ah: the odd cell of a gap goes right and down.
func placedBox(aw, ah, w, h int) (x, y int) {
	return max(0, aw-w) / 2, max(0, ah-h) / 2
}
