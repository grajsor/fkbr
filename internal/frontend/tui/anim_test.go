package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"fkbr/internal/api"
)

func TestNameXEasing(t *testing.T) {
	const home, width, n = 1, 100, 4
	if x := nameX(animOut, 0, home, width, n); x != home {
		t.Errorf("out starts at %d", x)
	}
	if x := nameX(animOut, animOutDur, home, width, n); x > -n {
		t.Errorf("out ends at %d, still visible", x)
	}
	if x := nameX(animIn, 0, home, width, n); x < width {
		t.Errorf("in starts at %d, already visible", x)
	}
	if x := nameX(animIn, animInDur, home, width, n); x != home {
		t.Errorf("in ends at %d", x)
	}
	if x := nameX(animIdle, time.Hour, home, width, n); x != home {
		t.Errorf("idle at %d", x)
	}
	// Out: slow start, fast end. In: fast start, slow end. Both monotonic.
	steps := 20
	var out, in []int
	for i := 0; i <= steps; i++ {
		out = append(out, nameX(animOut, animOutDur*time.Duration(i)/time.Duration(steps), home, width, n))
		in = append(in, nameX(animIn, animInDur*time.Duration(i)/time.Duration(steps), home, width, n))
	}
	for i := 1; i <= steps; i++ {
		if out[i] > out[i-1] || in[i] > in[i-1] {
			t.Fatalf("not monotonic: out %v in %v", out, in)
		}
	}
	if first, last := out[0]-out[2], out[steps-2]-out[steps]; first >= last {
		t.Errorf("out does not speed up: %v", out)
	}
	if first, last := in[0]-in[2], in[steps-2]-in[steps]; first <= last {
		t.Errorf("in does not slow down: %v", in)
	}
}

// Every frame of the animation keeps the header full width and painted.
func TestHeaderAnimationFrames(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	h := newHarness(t)
	for _, ph := range []struct {
		phase animPhase
		dur   time.Duration
	}{{animOut, animOutDur}, {animGap, animGapDur}, {animIn, animInDur}} {
		for i := 0; i <= 10; i++ {
			h.m.anim, h.m.animAt = ph.phase, time.Now().Add(-ph.dur*time.Duration(i)/10)
			l := h.m.header()
			if w := lipgloss.Width(l); w != h.m.width {
				t.Fatalf("phase %d step %d: header %d wide", ph.phase, i, w)
			}
			if c := unpainted(l); c >= 0 {
				t.Fatalf("phase %d step %d: column %d unpainted", ph.phase, i, c)
			}
		}
	}
	// The name moves only inside its own " fkbr " frame; the rest of the
	// header never changes.
	h.m.anim = animIdle
	idle := stripANSI(h.m.header())
	frame := len(" fkbr ")
	for _, ph := range []struct {
		phase animPhase
		dur   time.Duration
	}{{animOut, animOutDur}, {animGap, animGapDur}, {animIn, animInDur}} {
		for i := 0; i <= 10; i++ {
			h.m.anim, h.m.animAt = ph.phase, time.Now().Add(-ph.dur*time.Duration(i)/10)
			if got := stripANSI(h.m.header()); got[frame:] != idle[frame:] {
				t.Fatalf("phase %d step %d changed the header outside the frame:\n%q\n%q", ph.phase, i, got, idle)
			}
		}
	}
	h.m.anim = animGap
	if strings.Contains(h.m.header(), "fkbr") {
		t.Error("name shown during the gap")
	}
	h.m.anim = animIdle
	if !strings.Contains(h.m.header(), "fkbr") {
		t.Error("name missing when idle")
	}
}

// The cycle advances on its own ticks and comes back to idle.
func TestAnimationCycle(t *testing.T) {
	h := newHarness(t)
	h.m.Update(animMsg{})
	if h.m.anim != animOut {
		t.Fatalf("idle tick did not start the animation: %d", h.m.anim)
	}
	h.m.animAt = time.Now().Add(-animOutDur)
	h.m.Update(animMsg{})
	if h.m.anim != animGap {
		t.Fatalf("out did not end in the gap: %d", h.m.anim)
	}
	h.m.Update(animMsg{})
	if h.m.anim != animIn {
		t.Fatalf("gap did not lead to in: %d", h.m.anim)
	}
	h.m.animAt = time.Now().Add(-animInDur)
	h.m.Update(animMsg{})
	if h.m.anim != animIdle {
		t.Fatalf("in did not end idle: %d", h.m.anim)
	}
}

func TestSpinnerSequence(t *testing.T) {
	var got []string
	for i := 0; i < 14; i++ {
		got = append(got, spinChar(i))
	}
	want := "- > ) | ( < - < ( | ) > - >"
	if strings.Join(got, " ") != want {
		t.Fatalf("spinner frames:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
}

// The spinner replaces the bottom-right corner of the jobs panel, or of
// the right pane while there are no jobs; the key bar has no spinner.
func TestSpinnerInCorner(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	h := newHarness(t)
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	corner := func() (row int, last string) {
		lines := strings.Split(h.m.View(), "\n")
		for i, l := range lines {
			if c := unpainted(l); c >= 0 {
				t.Fatalf("line %d column %d unpainted", i, c)
			}
			if w := lipgloss.Width(l); w != 80 {
				t.Fatalf("line %d is %d wide", i, w)
			}
		}
		for i := len(lines) - 1; i >= 0; i-- {
			p := []rune(stripANSI(lines[i]))
			if s := string(p[len(p)-1]); s == spinChar(h.m.spin) {
				return i, s
			}
		}
		return -1, ""
	}
	h.m.spin = 3 // "|"
	row, _ := corner()
	lines := strings.Split(stripANSI(h.m.View()), "\n")
	if row < 0 || !(strings.HasPrefix(lines[row], "└") || strings.HasPrefix(lines[row], "╚")) {
		t.Fatalf("no jobs: spinner not on the panes' bottom line (row %d)", row)
	}
	if strings.Contains(lines[len(lines)-1], "|") {
		t.Error("spinner still in the key bar")
	}
	h.send(eventMsg{api.Event{Type: api.EventJob, JobID: "j1", Job: &api.Job{ID: "j1", State: api.JobDone,
		Request: api.TransferRequest{SrcNode: "master", DstNodes: []string{"s1"}}}}})
	h.m.spin = 5 // "<"
	row, _ = corner()
	lines = strings.Split(stripANSI(h.m.View()), "\n")
	if row != len(lines)-3 { // jobs panel bottom, above status and key bar
		t.Fatalf("with jobs: spinner on row %d, want %d", row, len(lines)-3)
	}
	if !strings.Contains(h.m.View(), h.m.st.spinner.Render("<")) {
		t.Error("spinner not in its yellow style")
	}
	h.m.spin = 0
	h.m.Update(spinMsg{})
	if h.m.spin != 1 {
		t.Errorf("tick did not advance the spinner: %d", h.m.spin)
	}
}
