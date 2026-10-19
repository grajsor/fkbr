package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
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

func TestPulseColour(t *testing.T) {
	// Two colours, switching every frame (2 a second).
	for i, want := range []string{pulseDark, pulseLight, pulseDark, pulseLight} {
		at := pulseFrame*time.Duration(i) + pulseFrame/2
		if got := pulseColour(at); got != want {
			t.Errorf("frame %d: %s, want %s", i, got, want)
		}
	}
	if pulseFrame != 500*time.Millisecond {
		t.Errorf("pulse frame %s, want 2 a second", pulseFrame)
	}
	for p, want := range map[termenv.Profile]bool{termenv.TrueColor: true, termenv.ANSI256: true, termenv.ANSI: false, termenv.Ascii: false} {
		if got := pulseEnabled(p); got != want {
			t.Errorf("%s: pulse %v, want %v", p.Name(), got, want)
		}
	}
}

// The pulsing cursor is drawn in the colour of the moment.
func TestCursorPulses(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	h := newHarness(t)
	h.m.pulse = true
	h.m.pulseStart = time.Now()
	h.m.pulseStart = time.Now().Add(-pulseFrame / 2)
	dark := h.m.View()
	h.m.pulseStart = time.Now().Add(-pulseFrame - pulseFrame/2)
	light := h.m.View()
	if dark == light {
		t.Fatal("cursor colour does not change over a breath")
	}
	if !strings.Contains(light, "48;2;") || unpainted(strings.Split(light, "\n")[2]) >= 0 {
		t.Error("pulsing cursor line not fully painted")
	}
}
