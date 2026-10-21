package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"fkbr/internal/api"
)

// click renders first, as bubbletea does before any input arrives.
func (h *harness) click(x, y int, b tea.MouseButton) {
	h.m.View()
	h.send(tea.MouseMsg{X: x, Y: y, Button: b, Action: tea.MouseActionPress})
}

// rowY is the screen line of the row called name in pane i.
func (h *harness) rowY(i int, name string) int {
	p := h.m.panes[i]
	for k, r := range p.rows {
		if r.name == name {
			return 2 + k - p.offset
		}
	}
	h.t.Fatalf("no row %q in pane %d", name, i)
	return 0
}

// find returns where text is on the screen.
func (h *harness) find(text string) (x, y int) {
	for y, l := range strings.Split(h.m.View(), "\n") {
		if i := strings.Index(ansi.Strip(l), text); i >= 0 {
			return ansi.StringWidth(ansi.Strip(l)[:i]), y
		}
	}
	h.t.Fatalf("%q not on screen", text)
	return 0, 0
}

func TestMouse(t *testing.T) {
	h := newHarness(t)
	left, right := 10, 70
	// A click focuses the other pane and moves its cursor; a second one
	// on the same row opens it.
	h.click(right, h.rowY(1, "s1"), tea.MouseButtonLeft)
	if h.m.active != 1 || h.cursorName(1) != "s1" {
		t.Fatalf("click: active %d cursor %q", h.m.active, h.cursorName(1))
	}
	h.click(right, h.rowY(1, "s1"), tea.MouseButtonLeft)
	if p := h.m.panes[1]; p.node != "s1" || p.level == levelNodes {
		t.Fatalf("double click did not open s1: %+v", p.level)
	}
	h.click(left, h.rowY(0, "master"), tea.MouseButtonLeft)
	h.click(left, h.rowY(0, "master"), tea.MouseButtonLeft)
	if h.m.active != 0 || h.m.panes[0].node != "master" {
		t.Fatalf("left pane: active %d node %q", h.m.active, h.m.panes[0].node)
	}
	// A right click marks (or unmarks) the row under it and stays there.
	h.click(left, h.rowY(0, "b.txt"), tea.MouseButtonRight)
	if !h.m.panes[0].selected["b.txt"] || h.cursorName(0) != "b.txt" {
		t.Fatalf("right click: marks %v cursor %q", h.m.panes[0].selected, h.cursorName(0))
	}
	// The wheel moves the cursor.
	before := h.m.panes[0].cursor
	h.click(left, 5, tea.MouseButtonWheelUp)
	if h.m.panes[0].cursor >= before {
		t.Errorf("wheel up: cursor %d, was %d", h.m.panes[0].cursor, before)
	}
	// The key bar presses its keys: 7 = Mkdir.
	x, y := h.find("7Mkdir")
	h.click(x+2, y, tea.MouseButtonLeft)
	if _, ok := h.m.dlg.(*inputDialog); !ok {
		t.Fatalf("key bar F7: dialog %T", h.m.dlg)
	}
	// A click outside a dialog closes it.
	h.click(0, 5, tea.MouseButtonLeft)
	if h.m.dlg != nil {
		t.Fatalf("click outside: dialog %T still open", h.m.dlg)
	}
	// A confirm dialog takes a click on its answer.
	h.key("f8")
	x, y = h.find("y = yes")
	h.click(x+1, y, tea.MouseButtonLeft)
	if h.m.dlg != nil || len(h.f.removes) != 1 || h.f.removes[0].Paths[0] != "b.txt" {
		t.Fatalf("click on yes: dialog %T, removes %+v", h.m.dlg, h.f.removes)
	}
	h.m.panes[0].cursorTo("a.txt")
	h.key("f8")
	x, y = h.find("n / Esc = no")
	h.click(x+1, y, tea.MouseButtonLeft)
	if h.m.dlg != nil || len(h.f.removes) != 1 {
		t.Fatalf("click on no: dialog %T, removes %d", h.m.dlg, len(h.f.removes))
	}
}

func TestMouseJobs(t *testing.T) {
	h := newHarness(t)
	id := "18e04e0fadd562793713f4f8"
	h.m.jobs[id] = api.Job{ID: id, State: api.JobRunning, Request: api.TransferRequest{SrcNode: "master", Paths: []string{"a.txt"}, DstNodes: []string{"s1"}}}
	h.m.jobOrder = []string{id}
	x, y := h.find(short(id))
	h.click(x, y, tea.MouseButtonLeft)
	if !h.m.jobsFocus {
		t.Fatal("click on a job did not focus the jobs panel")
	}
	h.click(x, y, tea.MouseButtonLeft)
	if d, ok := h.m.dlg.(*textDialog); !ok || d.jobID != id {
		t.Fatalf("double click on a job: dialog %T", h.m.dlg)
	}
	h.click(x, 3, tea.MouseButtonWheelDown) // scrolls the details
	h.click(0, 0, tea.MouseButtonLeft)      // outside: closes
	if h.m.dlg != nil {
		t.Fatalf("dialog %T still open", h.m.dlg)
	}
	h.click(10, 3, tea.MouseButtonLeft)
	if h.m.jobsFocus || h.m.active != 0 {
		t.Error("click on a pane did not take the focus back")
	}
}

// Dialogs take clicks on their hints (each part presses its key), on
// their settings (each toggles) and on their fields (focus).
func TestMouseDialogs(t *testing.T) {
	h := newHarness(t)
	h.m.active = 1
	h.open("s1", "data") // the destination
	h.m.active = 0
	h.open("master", "data")
	h.m.panes[0].cursorTo("a.txt")
	h.key("f5")
	d, ok := h.m.dlg.(*transferDialog)
	if !ok {
		t.Fatalf("F5: dialog %T", h.m.dlg)
	}
	x, y := h.find("^Z zstd")
	h.click(x+1, y, tea.MouseButtonLeft)
	if !d.compress {
		t.Error("click on ^Z zstd: compression still off")
	}
	conflict := d.conflict
	x, y = h.find("if exists:")
	h.click(x+3, y, tea.MouseButtonLeft)
	if d.conflict == conflict {
		t.Error("click on if exists: policy unchanged")
	}
	x, y = h.find("zstd: on")
	h.click(x+1, y, tea.MouseButtonLeft)
	if d.compress {
		t.Error("click on zstd: on did not turn it off")
	}
	x, y = h.find("directory:")
	h.click(x+2, y, tea.MouseButtonLeft)
	if d.focus != 2 {
		t.Errorf("click on the directory field: focus %d", d.focus)
	}
	x, y = h.find("^A all nodes")
	h.click(x+1, y, tea.MouseButtonLeft)
	if d.inputs[0].Value() != "@all" {
		t.Errorf("click on ^A: to = %q", d.inputs[0].Value())
	}
	x, y = h.find("Enter start")
	h.click(x+2, y, tea.MouseButtonLeft)
	if h.m.dlg != nil || len(h.f.submits) != 1 || h.f.submits[0].Conflict == "" {
		t.Fatalf("click on Enter start: dialog %T, submits %+v, err %q", h.m.dlg, h.f.submits, d.err)
	}

	h.key("f4")
	s, ok := h.m.dlg.(*syncDialog)
	if !ok {
		t.Fatalf("F4: dialog %T", h.m.dlg)
	}
	x, y = h.find("checksum:")
	h.click(x+1, y, tea.MouseButtonLeft)
	x, y = h.find("mode:")
	h.click(x+1, y, tea.MouseButtonLeft)
	x, y = h.find("keep newer:")
	h.click(x+1, y, tea.MouseButtonLeft)
	if !s.checksum || !s.mirror || !s.keepNewer {
		t.Errorf("sync settings: checksum %v mirror %v keep newer %v", s.checksum, s.mirror, s.keepNewer)
	}
	x, y = h.find("Esc cancel")
	h.click(x+1, y, tea.MouseButtonLeft)
	if h.m.dlg != nil {
		t.Fatalf("click on Esc cancel: dialog %T", h.m.dlg)
	}

	h.key("f7")
	x, y = h.find("Esc cancel")
	h.click(x+1, y, tea.MouseButtonLeft)
	if h.m.dlg != nil {
		t.Fatalf("mkdir, click on Esc cancel: dialog %T", h.m.dlg)
	}
}

// Dragging with the right button held marks every row it passes (or
// unmarks, when the first row was marked), past the pane's edge too, and
// in a node list it marks @dest.
func TestMouseDragMarks(t *testing.T) {
	h := newHarness(t)
	var es []api.Entry
	for i := range 80 {
		es = append(es, api.Entry{Name: fmt.Sprintf("f%02d", i), Type: api.TypeFile})
	}
	h.f.files["master:data/."] = es
	left := 10
	h.click(left, h.rowY(0, "master"), tea.MouseButtonLeft)
	h.click(left, h.rowY(0, "master"), tea.MouseButtonLeft)
	move := func(x, y int) {
		h.m.View()
		h.send(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonRight, Action: tea.MouseActionMotion})
	}
	release := func() { h.send(tea.MouseMsg{X: left, Button: tea.MouseButtonRight, Action: tea.MouseActionRelease}) }
	marked := func(set map[string]bool) string { return strings.Join(slices.Sorted(maps.Keys(set)), " ") }
	sel := h.m.panes[0].selected

	// One fast move skips rows on screen; they are marked all the same.
	h.click(left, h.rowY(0, "f02"), tea.MouseButtonRight)
	move(left, h.rowY(0, "f05"))
	move(left, h.rowY(0, "f03")) // back over marked rows: they stay
	release()
	if got := marked(sel); got != "f02 f03 f04 f05" || h.cursorName(0) != "f03" {
		t.Fatalf("drag down and back: marked %q, cursor %q", got, h.cursorName(0))
	}
	// Starting on a marked row unmarks.
	h.click(left, h.rowY(0, "f04"), tea.MouseButtonRight)
	move(left, h.rowY(0, "f05"))
	release()
	if got := marked(sel); got != "f02 f03" {
		t.Fatalf("unmarking drag: marked %q", got)
	}
	// Moving without a held button after the release marks nothing.
	move(left, h.rowY(0, "f08"))
	if got := marked(sel); got != "f02 f03" {
		t.Fatalf("motion after release: marked %q", got)
	}
	// Below the pane it reaches further and the pane scrolls along.
	h.click(left, h.rowY(0, "f10"), tea.MouseButtonRight)
	move(left, 2+h.m.paneHeight()+2)
	release()
	h.m.View()
	p := h.m.panes[0]
	if p.offset == 0 || p.cursor < p.offset {
		t.Fatalf("no scroll: offset %d cursor %d", p.offset, p.cursor)
	}
	for k := 11; k <= p.cursor; k++ { // row k is f(k-1): the up row is first
		if !sel[p.rows[k].name] {
			t.Fatalf("%s not marked (cursor at %s)", p.rows[k].name, h.cursorName(0))
		}
	}
	// In a node list it marks @dest.
	right := 70
	h.click(right, h.rowY(1, "master"), tea.MouseButtonRight)
	move(right, h.rowY(1, "s1"))
	release()
	if got := marked(h.m.dest); got != "master s1" {
		t.Fatalf("node list drag: @dest %q", got)
	}
}
