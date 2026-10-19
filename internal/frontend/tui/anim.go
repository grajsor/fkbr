package tui

import (
	"math"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// The name in the header slides out to the left every animEvery and back
// in from the right two seconds later, within its own " fkbr " frame, so a
// glance shows the TUI is alive.
// It needs no goroutine of its own: one timer message between runs, and
// about 30 header redraws a second during the two seconds of motion.

type animPhase int

const (
	animIdle animPhase = iota
	animOut            // leaving to the left: slow start, fast end
	animGap            // off screen
	animIn             // entering from the right: fast start, slow stop
)

var (
	animEvery  = 30 * time.Second
	animOutDur = 800 * time.Millisecond
	animGapDur = 2 * time.Second
	animInDur  = 1200 * time.Millisecond
	animFrame  = 33 * time.Millisecond
)

type animMsg struct{}

func animTick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return animMsg{} })
}

// animate advances the cycle on each of its ticks and schedules the next.
func (m *model) animate() tea.Cmd {
	now := time.Now()
	elapsed := now.Sub(m.animAt)
	switch m.anim {
	case animIdle:
		m.anim, m.animAt = animOut, now
	case animOut:
		if elapsed >= animOutDur {
			m.anim, m.animAt = animGap, now
			return animTick(animGapDur)
		}
	case animGap:
		m.anim, m.animAt = animIn, now
	case animIn:
		if elapsed >= animInDur {
			m.anim = animIdle
			return animTick(animEvery)
		}
	}
	return animTick(animFrame)
}

// nameX is the column of a name of n cells at elapsed time into phase, in
// a frame of width cells where it normally sits at home; columns outside
// the frame are clipped by the caller.
func nameX(phase animPhase, elapsed time.Duration, home, width, n int) int {
	switch phase {
	case animOut:
		t := min(1, float64(elapsed)/float64(animOutDur))
		return home - int(math.Round(float64(home+n)*t*t*t)) // ease in
	case animGap:
		return width
	case animIn:
		t := min(1, float64(elapsed)/float64(animInDur))
		u := 1 - t
		return home + int(math.Round(float64(width-home)*u*u*u)) // ease out
	}
	return home
}

// A small spinner replaces the bottom-right corner of the jobs panel (of
// the right pane while there are no jobs) and turns all the time, so the
// screen looks alive even when nothing moves: - > ) | ( < - and back,
// 4 times a second.
var (
	spinFrames = []string{"-", ">", ")", "|", "(", "<", "-"}
	spinEvery  = 250 * time.Millisecond
)

type spinMsg struct{}

func spinTick() tea.Cmd {
	return tea.Tick(spinEvery, func(time.Time) tea.Msg { return spinMsg{} })
}

// spinChar is the spinner at step i: through the frames and back again.
func spinChar(i int) string {
	n := len(spinFrames)
	k := i % (2 * (n - 1))
	if k >= n {
		k = 2*(n-1) - k
	}
	return spinFrames[k]
}

// A name too long for its column scrolls on the cursor row: it rests at
// the start, moves a character per marqueeStep until its end shows, rests
// there and starts over. It needs no timer of its own: marqueeStep matches
// the spinner, whose redraws carry it.
var (
	marqueeRest = 1500 * time.Millisecond
	marqueeStep = spinEvery
)

// marqueeOffset is how many characters a name that is overflow cells too
// long has scrolled at elapsed time since the cursor came to it.
func marqueeOffset(elapsed time.Duration, overflow int) int {
	if overflow <= 0 {
		return 0
	}
	run := time.Duration(overflow) * marqueeStep
	t := elapsed % (2*marqueeRest + run)
	switch {
	case t < marqueeRest:
		return 0
	case t < marqueeRest+run:
		return int((t - marqueeRest) / marqueeStep)
	}
	return overflow
}

// scrolled drops the first characters of name to show it scrolled within
// width cells at elapsed time.
func scrolled(name string, width int, elapsed time.Duration) string {
	if off := marqueeOffset(elapsed, runewidth.StringWidth(name)-width); off > 0 {
		return string([]rune(name)[off:])
	}
	return name
}
