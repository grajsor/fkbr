package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Scrollbars sit in the right edge of what scrolls: a pane's right border,
// the column right of a text dialog's text. They show only when there is
// more than fits. A click on the track above or below the thumb pages,
// dragging the thumb moves through it (mouse.go).

const thumbChar = "█"

// scrollThumb places the thumb on a track of h cells for a window of h
// lines at offset off into n lines; ok is false when everything fits.
func scrollThumb(h, n, off int) (start, size int, ok bool) {
	if h <= 0 || n <= h {
		return 0, 0, false
	}
	size = max(1, h*h/n)
	start = (off*(h-size) + (n-h)/2) / (n - h)
	return min(max(0, start), h-size), size, true
}

// scrollLen is how many lines the pane scrolls through, the entries not
// loaded yet included.
func (p *pane) scrollLen() int {
	n := len(p.rows)
	if p.level == levelDir && p.loaded < p.total {
		n += p.total - p.loaded
	}
	return n
}

// withScrollbar draws a thumb into the right border of box (made by
// frame) on the lines from+start .. from+start+size-1.
func (st *styles) withScrollbar(box string, active bool, from, start, size int) string {
	b, fg := st.border, st.borderFg
	if active {
		b, fg = st.borderActive, st.borderActiveFg
	}
	thumb := lipgloss.NewStyle().Foreground(fg).Background(cPanel).Render(thumbChar)
	lines := strings.Split(box, "\n")
	for y := from + start; y < from+start+size && y < len(lines); y++ {
		if i := strings.LastIndex(lines[y], b.Right); i >= 0 {
			lines[y] = lines[y][:i] + thumb + lines[y][i+len(b.Right):]
		}
	}
	return strings.Join(lines, "\n")
}

// bar is the scrollbar column for a text dialog's viewport of h lines.
func (st *styles) bar(h, n, off int) []string {
	track := st.dialogBar.Render("│")
	out := make([]string, h)
	for i := range out {
		out[i] = track
	}
	if start, size, ok := scrollThumb(h, n, off); ok {
		for i := start; i < start+size; i++ {
			out[i] = st.dialogBar.Render(thumbChar)
		}
	} else {
		for i := range out {
			out[i] = st.dialogBar.Render(" ")
		}
	}
	return out
}

// drag says what a held button moves: a pane's list or a dialog (left),
// or the marking in a pane (right).
type drag int

const (
	dragNone drag = iota
	dragPane0
	dragPane1
	dragDialog
	dragMark0
	dragMark1
)

// paneBarClick handles a click on pane i's scrollbar at track line; false
// if the pane has none.
func (m *model) paneBarClick(i, line int) (tea.Cmd, bool) {
	p := m.panes[i]
	ph := m.paneHeight()
	start, size, ok := scrollThumb(ph, p.scrollLen(), p.offset)
	if !ok {
		return nil, false
	}
	m.active, m.jobsFocus = i, false
	switch {
	case line < start:
		return m.key(tea.KeyMsg{Type: tea.KeyPgUp}), true
	case line >= start+size:
		return m.key(tea.KeyMsg{Type: tea.KeyPgDown}), true
	}
	m.drag = dragPane0 + drag(i)
	return nil, true
}

// textBarClick is paneBarClick for a text dialog's scrollbar.
func (m *model) textBarClick(d *textDialog, line int) {
	h := d.vp.Height
	start, size, ok := scrollThumb(h, d.vp.TotalLineCount(), d.vp.YOffset)
	switch {
	case !ok:
	case line < start:
		d.vp.SetYOffset(d.vp.YOffset - h)
	case line >= start+size:
		d.vp.SetYOffset(d.vp.YOffset + h)
	default:
		m.drag = dragDialog
	}
}

// dragTo moves what is being dragged so its thumb follows the pointer at
// screen line y.
func (m *model) dragTo(y int) tea.Cmd {
	frac := func(line, h, n int) int { // line of a track of h cells → 0..n-1
		line = max(0, min(h-1, line))
		if h <= 1 {
			return 0
		}
		return (line*(n-1) + (h-1)/2) / (h - 1)
	}
	switch m.drag {
	case dragMark0, dragMark1:
		return m.markTo(int(m.drag-dragMark0), y)
	case dragPane0, dragPane1:
		i := int(m.drag - dragPane0)
		p := m.panes[i]
		if len(p.rows) == 0 {
			return nil
		}
		p.cursor = frac(y-2, m.paneHeight(), len(p.rows))
		return m.maybeLoadMore(i)
	case dragDialog:
		d, ok := m.dlg.(*textDialog)
		if !ok {
			m.drag = dragNone
			return nil
		}
		h := d.vp.Height
		d.vp.SetYOffset(frac(y-m.dlgBox.cy-1, h, max(1, d.vp.TotalLineCount()-h+1)))
	}
	return nil
}
