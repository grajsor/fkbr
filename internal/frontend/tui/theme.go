package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The palette follows forsakringskassan.se: its dark header green fills the
// panels the way blue did in Norton Commander. Colours are named by role,
// since the 16-colour fallback is a different scheme: that palette has no
// dark green (the terminal's "green" is often glaring), so there the
// panels are black with green text, like a green phosphor screen.
var (
	cPanel        = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "22", ANSI: "0"}
	cFile         = lipgloss.CompleteColor{TrueColor: "#e7f0e9", ANSI256: "255", ANSI: "2"}
	cDir          = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "10"}
	cDim          = lipgloss.CompleteColor{TrueColor: "#88b49f", ANSI256: "109", ANSI: "8"}
	cMarked       = lipgloss.CompleteColor{TrueColor: "#ffbe10", ANSI256: "214", ANSI: "11"}
	cErr          = lipgloss.CompleteColor{TrueColor: "#e48a8a", ANSI256: "174", ANSI: "9"}
	cBorder       = lipgloss.CompleteColor{TrueColor: "#77ad82", ANSI256: "108", ANSI: "2"}
	cBorderActive = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "10"}
	cTitle        = lipgloss.CompleteColor{TrueColor: "#e7f0e9", ANSI256: "255", ANSI: "10"}
	cBar          = lipgloss.CompleteColor{TrueColor: "#afcfb5", ANSI256: "151", ANSI: "2"} // cursor, key labels
	cBarText      = lipgloss.CompleteColor{TrueColor: "#1b1e23", ANSI256: "234", ANSI: "0"}
	cBarTitle     = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "22", ANSI: "0"} // active title on cBar
	cHeader       = lipgloss.CompleteColor{TrueColor: "#35805b", ANSI256: "65", ANSI: "2"}
	cHeaderText   = lipgloss.CompleteColor{TrueColor: "#e7f0e9", ANSI256: "255", ANSI: "0"}
	cHeaderName   = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "0"}
	cInk          = lipgloss.CompleteColor{TrueColor: "#1b1e23", ANSI256: "234", ANSI: "0"} // status and key-number bar
	cInkText      = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "15"}
	cSurface      = lipgloss.CompleteColor{TrueColor: "#dbe9e2", ANSI256: "254", ANSI: "7"} // dialogs
	cSurfaceText  = lipgloss.CompleteColor{TrueColor: "#1b1e23", ANSI256: "234", ANSI: "0"}
	cSurfaceTitle = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "22", ANSI: "0"}
	cSurfaceErr   = lipgloss.CompleteColor{TrueColor: "#ca1515", ANSI256: "160", ANSI: "1"}
	cAccent       = lipgloss.CompleteColor{TrueColor: "#4a52b6", ANSI256: "61", ANSI: "4"} // focused input
	cAccentText   = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "15"}
)

type styles struct {
	// ascii: no colours (NO_COLOR, dumb terminal); see highlight.
	ascii bool
	// On the panels.
	panel, file, dir, marked, dim, warn, err lipgloss.Style
	cursor, cursorMarked                     lipgloss.Style
	rule, title, titleActive                 lipgloss.Style
	border, borderActive                     lipgloss.Border
	borderFg, borderActiveFg                 lipgloss.TerminalColor
	// Screen chrome.
	header, headerName, keyNum, keyLabel, status, statusErr lipgloss.Style
	// Dialogs.
	dialog, dialogTitle, dialogErr, inputLabel, inputFocus, inputCursor lipgloss.Style
}

func newStyles() *styles {
	on := func(bg lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Background(bg) }
	st := &styles{
		panel:        on(cPanel).Foreground(cFile),
		file:         on(cPanel).Foreground(cFile),
		dir:          on(cPanel).Foreground(cDir).Bold(true),
		marked:       on(cPanel).Foreground(cMarked).Bold(true),
		dim:          on(cPanel).Foreground(cDim),
		warn:         on(cPanel).Foreground(cMarked),
		err:          on(cPanel).Foreground(cErr),
		cursor:       on(cBar).Foreground(cBarText),
		cursorMarked: on(cMarked).Foreground(cBarText).Bold(true),
		rule:         on(cPanel).Foreground(cBorder),
		title:        on(cPanel).Foreground(cTitle).Bold(true),
		titleActive:  on(cBar).Foreground(cBarTitle).Bold(true),
		border:       lipgloss.NormalBorder(),
		borderActive: lipgloss.DoubleBorder(),

		borderFg:       cBorder,
		borderActiveFg: cBorderActive,

		header:     on(cHeader).Foreground(cHeaderText),
		headerName: on(cHeader).Foreground(cHeaderName).Bold(true),
		keyNum:     on(cInk).Foreground(cInkText),
		keyLabel:   on(cBar).Foreground(cBarText),
		status:     on(cInk).Foreground(cMarked),
		statusErr:  on(cInk).Foreground(cErr),

		dialog: on(cSurface).Foreground(cSurfaceText).Padding(0, 1).
			Border(lipgloss.DoubleBorder()).BorderForeground(cSurfaceTitle).BorderBackground(cSurface),
		dialogTitle: on(cSurface).Foreground(cSurfaceTitle).Bold(true),
		dialogErr:   on(cSurface).Foreground(cSurfaceErr).Bold(true),
		inputLabel:  on(cSurface).Foreground(cSurfaceText),
		inputFocus:  on(cAccent).Foreground(cAccentText).Bold(true),
		inputCursor: on(cAccent).Foreground(cAccentText),
	}
	st.ascii = lipgloss.ColorProfile() == termenv.Ascii
	return st
}

// setColors picks the colour depth: auto, true, 256, 16 or none. Auto
// trusts the environment, except that xterm-like terminals that claim only
// 16 colours get 256: nearly all of them support it (PuTTY, MobaXterm,
// Windows Terminal, GNOME, iTerm), and it keeps the panels dark green.
func setColors(mode string) error {
	switch mode {
	case "", "auto":
		if lipgloss.ColorProfile() == termenv.ANSI && likely256(os.Getenv("TERM")) {
			lipgloss.SetColorProfile(termenv.ANSI256)
		}
	case "true", "truecolor", "24bit":
		lipgloss.SetColorProfile(termenv.TrueColor)
	case "256":
		lipgloss.SetColorProfile(termenv.ANSI256)
	case "16":
		lipgloss.SetColorProfile(termenv.ANSI)
	case "none":
		lipgloss.SetColorProfile(termenv.Ascii)
	default:
		return fmt.Errorf("colors %q: want auto, true, 256, 16 or none", mode)
	}
	return nil
}

func likely256(term string) bool {
	for _, p := range []string{"xterm", "screen", "tmux", "rxvt", "putty", "konsole", "gnome", "alacritty", "kitty", "foot", "wezterm"} {
		if strings.HasPrefix(term, p) {
			return true
		}
	}
	return false
}

// highlight renders the cursor or another highlight with s. Without
// colours lipgloss drops every attribute, so plain reverse video is
// written instead: NO_COLOR forbids colour, not reverse.
func (st *styles) highlight(s lipgloss.Style, text string) string {
	if st.ascii {
		return "\x1b[7m" + text + "\x1b[0m"
	}
	return s.Render(text)
}

// frame draws a box of inner width around content, with title set into
// the top border. The active box gets a double white border and its title
// a highlight, as in Norton Commander.
func (st *styles) frame(active bool, title, content string, inner int) string {
	b, fg, ts := st.border, st.borderFg, st.title
	if active {
		b, fg, ts = st.borderActive, st.borderActiveFg, st.titleActive
	}
	line := lipgloss.NewStyle().Foreground(fg).Background(cPanel)
	t := " " + fitLeft(title, max(1, inner-4)) + " "
	if active {
		t = st.highlight(ts, t)
	} else {
		t = ts.Render(t)
	}
	fill := max(0, inner-1-lipgloss.Width(t))
	top := line.Render(b.TopLeft+b.Top) + t + line.Render(strings.Repeat(b.Top, fill)+b.TopRight)
	body := st.panel.Border(b, false, true, true, true).
		BorderForeground(fg).BorderBackground(cPanel).Width(inner).Render(content)
	return top + "\n" + body
}

// padLines pads every line of s to the widest one with fill. lipgloss's
// own padding would follow the last inner reset and lose the background.
func padLines(s string, fill lipgloss.Style) string {
	lines := strings.Split(s, "\n")
	w := lipgloss.Width(s)
	for i, l := range lines {
		if gap := w - lipgloss.Width(l); gap > 0 {
			lines[i] = l + fill.Render(strings.Repeat(" ", gap))
		}
	}
	return strings.Join(lines, "\n")
}

// styleInputs paints text inputs for a dialog; the focused one's label
// carries the accent colour.
func (st *styles) styleInputs(focus int, inputs ...*textinput.Model) {
	for i, in := range inputs {
		in.PromptStyle = st.inputLabel
		if i == focus {
			in.PromptStyle = st.inputFocus
		}
		in.TextStyle = st.inputLabel
		in.PlaceholderStyle = st.inputLabel
		in.Cursor.Style = st.inputCursor
		in.Cursor.TextStyle = st.inputLabel
	}
}
