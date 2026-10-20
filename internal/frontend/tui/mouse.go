package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The mouse does what keys do. In the panes and the jobs panel a click
// moves the cursor (and the focus) there and a double click opens (Enter);
// a right click marks or unmarks the row under it, as in Norton Commander;
// the wheel moves the cursor. A click on the key bar presses that key. In
// a dialog the wheel scrolls, a click outside closes it (Esc), and a
// confirm dialog takes a click on its answers.

const doubleClick = 400 * time.Millisecond

// click is the last left click on a row, for telling double clicks.
type click struct {
	area, row int // area: 0, 1 the panes, 2 the jobs panel
	at        time.Time
}

// box is where View drew the dialog: its frame, and where its content
// starts and how many lines it has.
type box struct {
	x, y, w, h       int
	cx, cy, contentH int
}

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	if msg.Action != tea.MouseActionPress || m.width < 40 || m.height < 12 {
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
		if r := p.current(); r != nil && !r.up && p.level != levelRoots {
			toggle(m.marks(p), r.name)
			m.destStatus(p)
		}
		return nil
	}
	if m.double(i, row) {
		return m.enter(i)
	}
	return nil
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
	if _, ok := m.dlg.(*confirmDialog); ok && msg.Button == tea.MouseButtonLeft && msg.Y == b.cy+b.contentH-1 {
		// The last line is confirmHint: "y = yes · n / Esc = no".
		answer := "n"
		if x := msg.X - b.cx; x >= 0 && x < len("y = yes") {
			answer = "y"
		} else if x < 0 || x >= len([]rune(confirmHint)) {
			return nil
		}
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(answer)})
		return cmd
	}
	return nil
}

// placedBox works out where lipgloss.Place centres a block of w x h cells
// in an area of aw x ah: the odd cell of a gap goes right and down.
func placedBox(aw, ah, w, h int) (x, y int) {
	return max(0, aw-w) / 2, max(0, ah-h) / 2
}

func lineCount(s string) int { return strings.Count(s, "\n") + 1 }
