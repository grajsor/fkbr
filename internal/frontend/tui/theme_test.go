package tui

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"fkbr/internal/api"
)

// unpainted returns the column of the first visible cell in line drawn
// without a background colour (where the terminal's own background would
// show through the theme), or -1.
func unpainted(line string) int {
	bg, col := false, 0
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				bg = sgrBackground(line[i+2:j], bg)
			}
			i = j + 1
			continue
		}
		_, n := utf8.DecodeRuneInString(line[i:])
		if !bg {
			return col
		}
		col++
		i += n
	}
	return -1
}

// sgrBackground applies one SGR sequence's parameters to "a background
// colour is set".
func sgrBackground(params string, bg bool) bool {
	ps := strings.Split(params, ";")
	for k := 0; k < len(ps); k++ {
		n, _ := strconv.Atoi(ps[k])
		switch {
		case ps[k] == "" || n == 0 || n == 49:
			bg = false
		case n == 48:
			bg = true
			if k+1 < len(ps) && ps[k+1] == "5" {
				k += 2
			} else {
				k += 4
			}
		case n == 38:
			if k+1 < len(ps) && ps[k+1] == "5" {
				k += 2
			} else {
				k += 4
			}
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			bg = true
		}
	}
	return bg
}

func TestUnpaintedDetector(t *testing.T) {
	for in, want := range map[string]int{
		"plain":                              0,
		"\x1b[48;2;1;2;3mab\x1b[0m":          -1,
		"\x1b[48;2;1;2;3mab\x1b[0mc":         2,
		"\x1b[42;1mab\x1b[38;5;3mc\x1b[49mx": 3,
		"\x1b[48;5;22;38;2;1;2;3mabc":        -1,
	} {
		if got := unpainted(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

// TestThemePaintsEveryCell renders each screen in true colour and checks
// that every line fills the terminal exactly and every cell has a
// background from the theme.
func TestThemePaintsEveryCell(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	h := newHarness(t)
	h.open("master", "data")
	h.key("tab")
	h.open("s1", "data")
	h.key("tab")
	h.key("insert") // a marked entry
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &api.Job{ID: "j1", State: api.JobFailed,
		Request: api.TransferRequest{SrcNode: "master", Paths: []string{"x"}, DstNodes: []string{"s1"}}}}})

	check := func(name string) {
		t.Helper()
		lines := strings.Split(h.m.View(), "\n")
		if len(lines) != h.m.height {
			t.Errorf("%s: %d lines, want %d", name, len(lines), h.m.height)
		}
		for i, l := range lines {
			if w := lipgloss.Width(l); w != h.m.width {
				t.Errorf("%s: line %d is %d wide, want %d", name, i, w, h.m.width)
			}
			if c := unpainted(l); c >= 0 {
				t.Errorf("%s: line %d, column %d has no background:\n%q", name, i, c, l)
			}
		}
	}
	for _, size := range [][2]int{{120, 40}, {80, 24}, {41, 13}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		check("panes")
		h.key("f9")
		check("jobs focus")
		h.key("esc")
		for _, k := range []string{"f1", "f5", "f4", "f7", "f8"} {
			h.key(k)
			if h.m.dlg == nil {
				t.Fatalf("%s opened no dialog", k)
			}
			check(k + " dialog")
			h.key("esc")
		}
		h.m.status, h.m.statusErr = "something failed", true
		check("error status")
		h.m.status = ""
	}
}

// Without colours (NO_COLOR, dumb terminals) the cursor must stay visible.
func TestThemeWithoutColour(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(prev)
	h := newHarness(t)
	h.open("master", "data")
	lines := strings.Split(h.m.View(), "\n")
	if !strings.Contains(lines[1], "\x1b[7m master:/ ") {
		t.Errorf("active title not highlighted: %q", lines[1])
	}
	if !strings.Contains(lines[2], "\x1b[7m/..") {
		t.Errorf("cursor row not highlighted: %q", lines[2])
	}
	if strings.Contains(h.m.View(), "\x1b[3") || strings.Contains(h.m.View(), "\x1b[4") {
		t.Error("colours emitted without a colour profile")
	}
}
