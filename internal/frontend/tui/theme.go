package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// The palette follows forsakringskassan.se: its dark header green fills the
// panels the way blue did in Norton Commander. Each colour carries 256- and
// 16-colour fallbacks for terminals without true colour.
var (
	cGreen     = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "23", ANSI: "2"}
	cGreenInk  = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "23", ANSI: "0"} // as text on light colours
	cMidGreen  = lipgloss.CompleteColor{TrueColor: "#35805b", ANSI256: "65", ANSI: "0"}
	cSoftGreen = lipgloss.CompleteColor{TrueColor: "#77ad82", ANSI256: "108", ANSI: "0"}
	cDimGreen  = lipgloss.CompleteColor{TrueColor: "#88b49f", ANSI256: "109", ANSI: "7"}
	cPaleGreen = lipgloss.CompleteColor{TrueColor: "#afcfb5", ANSI256: "151", ANSI: "6"}
	cSurface   = lipgloss.CompleteColor{TrueColor: "#dbe9e2", ANSI256: "254", ANSI: "7"}
	cText      = lipgloss.CompleteColor{TrueColor: "#e7f0e9", ANSI256: "255", ANSI: "15"}
	cWhite     = lipgloss.CompleteColor{TrueColor: "#ffffff", ANSI256: "231", ANSI: "15"}
	cInk       = lipgloss.CompleteColor{TrueColor: "#1b1e23", ANSI256: "234", ANSI: "0"}
	cYellow    = lipgloss.CompleteColor{TrueColor: "#ffbe10", ANSI256: "214", ANSI: "11"}
	cSoftRed   = lipgloss.CompleteColor{TrueColor: "#e48a8a", ANSI256: "174", ANSI: "9"}
	cRed       = lipgloss.CompleteColor{TrueColor: "#ca1515", ANSI256: "160", ANSI: "1"}
	cAccent    = lipgloss.CompleteColor{TrueColor: "#4a52b6", ANSI256: "61", ANSI: "4"}
)

type styles struct {
	// ascii: no colours (NO_COLOR, dumb terminal); see highlight.
	ascii bool
	// On the green panels.
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
		panel:        on(cGreen).Foreground(cText),
		file:         on(cGreen).Foreground(cText),
		dir:          on(cGreen).Foreground(cWhite).Bold(true),
		marked:       on(cGreen).Foreground(cYellow).Bold(true),
		dim:          on(cGreen).Foreground(cDimGreen),
		warn:         on(cGreen).Foreground(cYellow),
		err:          on(cGreen).Foreground(cSoftRed),
		cursor:       on(cPaleGreen).Foreground(cInk),
		cursorMarked: on(cYellow).Foreground(cInk).Bold(true),
		rule:         on(cGreen).Foreground(cSoftGreen),
		title:        on(cGreen).Foreground(cText).Bold(true),
		titleActive:  on(cPaleGreen).Foreground(cGreenInk).Bold(true),
		border:       lipgloss.NormalBorder(),
		borderActive: lipgloss.DoubleBorder(),

		borderFg:       cSoftGreen,
		borderActiveFg: cWhite,

		header:     on(cMidGreen).Foreground(cText),
		headerName: on(cMidGreen).Foreground(cWhite).Bold(true),
		keyNum:     on(cInk).Foreground(cWhite),
		keyLabel:   on(cPaleGreen).Foreground(cInk),
		status:     on(cInk).Foreground(cYellow),
		statusErr:  on(cInk).Foreground(cSoftRed),

		dialog: on(cSurface).Foreground(cInk).Padding(0, 1).
			Border(lipgloss.DoubleBorder()).BorderForeground(cGreen).BorderBackground(cSurface),
		dialogTitle: on(cSurface).Foreground(cGreenInk).Bold(true),
		dialogErr:   on(cSurface).Foreground(cRed).Bold(true),
		inputLabel:  on(cSurface).Foreground(cInk),
		inputFocus:  on(cAccent).Foreground(cWhite).Bold(true),
		inputCursor: on(cAccent).Foreground(cWhite),
	}
	st.ascii = lipgloss.ColorProfile() == termenv.Ascii
	return st
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
	line := lipgloss.NewStyle().Foreground(fg).Background(cGreen)
	t := " " + fitLeft(title, max(1, inner-4)) + " "
	if active {
		t = st.highlight(ts, t)
	} else {
		t = ts.Render(t)
	}
	fill := max(0, inner-1-lipgloss.Width(t))
	top := line.Render(b.TopLeft+b.Top) + t + line.Render(strings.Repeat(b.Top, fill)+b.TopRight)
	body := st.panel.Border(b, false, true, true, true).
		BorderForeground(fg).BorderBackground(cGreen).Width(inner).Render(content)
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
