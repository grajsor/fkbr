package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"fkbr/internal/api"
)

func TestScrollThumb(t *testing.T) {
	for _, c := range []struct{ h, n, off, start, size int }{
		{10, 100, 0, 0, 1}, {10, 100, 90, 9, 1}, {10, 20, 10, 5, 5}, {10, 20, 0, 0, 5}, {10, 11, 1, 1, 9},
	} {
		start, size, ok := scrollThumb(c.h, c.n, c.off)
		if !ok || start != c.start || size != c.size {
			t.Errorf("scrollThumb(%d, %d, %d) = %d, %d, %v; want %d, %d", c.h, c.n, c.off, start, size, ok, c.start, c.size)
		}
	}
	if _, _, ok := scrollThumb(10, 10, 0); ok {
		t.Error("a list that fits has a scrollbar")
	}
}

// column returns the cell at x of every screen line.
func (h *harness) column(x int) []string {
	var out []string
	for _, l := range strings.Split(h.m.View(), "\n") {
		out = append(out, ansi.Cut(l, x, x+1))
	}
	return out
}

func thumbLines(col []string) []int {
	var ys []int
	for y, c := range col {
		if ansi.Strip(c) == thumbChar {
			ys = append(ys, y)
		}
	}
	return ys
}

// Long lists and texts get a scrollbar in their right edge; clicks on its
// track page, dragging the thumb moves through them.
func TestScrollbars(t *testing.T) {
	h := newHarness(t)
	var es []api.Entry
	for i := range 200 {
		es = append(es, api.Entry{Name: fmt.Sprintf("f%03d", i), Type: api.TypeFile})
	}
	h.f.files["master:data/."] = es
	h.open("master", "data")
	bar := h.m.width/2 - 1 // the left pane's right border
	ys := thumbLines(h.column(bar))
	if len(ys) == 0 || ys[0] != 2 {
		t.Fatalf("no thumb at the top of the left pane: %v", ys)
	}
	if len(thumbLines(h.column(h.m.width-1))) != 0 {
		t.Error("the right pane (a node list that fits) has a thumb")
	}
	ph := h.m.paneHeight()
	h.click(bar, 2+ph-1, tea.MouseButtonLeft) // the track's bottom: a page down
	if c := h.m.panes[0].cursor; c != ph {
		t.Errorf("track click: cursor %d, want %d", c, ph)
	}
	ys = thumbLines(h.column(bar))
	h.click(bar, ys[0], tea.MouseButtonLeft) // on the thumb: drag
	h.m.View()
	h.send(tea.MouseMsg{X: bar, Y: 2 + ph - 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if c := h.m.panes[0].cursor; c != len(h.m.panes[0].rows)-1 {
		t.Errorf("dragged to the bottom: cursor %d of %d", c, len(h.m.panes[0].rows))
	}
	h.send(tea.MouseMsg{X: bar, Y: 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if c := h.m.panes[0].cursor; c != 0 {
		t.Errorf("dragged to the top: cursor %d", c)
	}
	h.send(tea.MouseMsg{X: bar, Y: 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	h.send(tea.MouseMsg{X: bar, Y: 2 + ph - 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if c := h.m.panes[0].cursor; c != 0 {
		t.Errorf("motion after release still drags: cursor %d", c)
	}

	// The viewer: 300 lines.
	var text strings.Builder
	for i := range 300 {
		fmt.Fprintf(&text, "line %d\n", i)
	}
	h.f.read = []byte(text.String())
	h.m.panes[0].cursorTo("f000")
	h.key("f3")
	d, ok := h.m.dlg.(*textDialog)
	if !ok {
		t.Fatalf("F3: dialog %T", h.m.dlg)
	}
	h.m.View()
	b := h.m.dlgBox
	x := b.cx + d.vp.Width // the bar, right of the text
	ys = thumbLines(h.column(x))
	if len(ys) == 0 || ys[0] != b.cy+1 {
		t.Fatalf("viewer: no thumb at the top of column %d: %v", x, ys)
	}
	h.click(x, b.cy+d.vp.Height, tea.MouseButtonLeft) // track bottom
	if d.vp.YOffset != d.vp.Height {
		t.Errorf("viewer track click: offset %d, want %d", d.vp.YOffset, d.vp.Height)
	}
	ys = thumbLines(h.column(x))
	h.click(x, ys[0], tea.MouseButtonLeft)
	h.m.View()
	h.send(tea.MouseMsg{X: x, Y: b.cy + d.vp.Height, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if !d.vp.AtBottom() {
		t.Errorf("viewer dragged to the bottom: offset %d", d.vp.YOffset)
	}
	h.send(tea.MouseMsg{X: x, Y: b.cy + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion})
	if d.vp.YOffset != 0 {
		t.Errorf("viewer dragged to the top: offset %d", d.vp.YOffset)
	}
	h.send(tea.MouseMsg{X: x, Y: b.cy + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease})
	if h.m.dlg == nil {
		t.Error("scrollbar clicks closed the viewer")
	}
}
