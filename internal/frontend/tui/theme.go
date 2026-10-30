package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"fkbr/internal/config"
)

// A palette names its colours by role. The default is built around a dark
// green, which fills the panels the way blue did in Norton Commander. The
// 16-colour fallback is a different scheme: that palette has no dark green
// (the terminal's "green" is often glaring), so there the panels are black
// with green text, like a green phosphor screen. The other themes keep the
// same roles: the panels, a light cursor bar, light dialogs.
type palette struct {
	panel, file, dir, dim, marked, err, border, borderActive, title lipgloss.CompleteColor
	bar, barText, barTitle                                          lipgloss.CompleteColor // cursor and key labels; an active title on bar
	markedText                                                      lipgloss.CompleteColor // the cursor on a marked row
	header, headerText, headerName                                  lipgloss.CompleteColor
	ink, inkText, statusText, statusErr                             lipgloss.CompleteColor // status and key-number bar
	surface, surfaceText, surfaceTitle, surfaceErr                  lipgloss.CompleteColor // dialogs
	accent, accentText                                              lipgloss.CompleteColor // focused input
	codeTag                                                         lipgloss.CompleteColor // XML tags, shell keywords in the viewer
}

func cc(tc, c256, c16 string) lipgloss.CompleteColor {
	return lipgloss.CompleteColor{TrueColor: tc, ANSI256: c256, ANSI: c16}
}

// themes are what -theme and tui.theme take; config.TUIThemes lists the
// names, the first the default.
var themes = map[string]palette{
	"green": {
		panel: cc("#116a3e", "22", "0"), file: cc("#e7f0e9", "255", "2"), dir: cc("#ffffff", "231", "10"),
		dim: cc("#88b49f", "109", "8"), marked: cc("#ffbe10", "214", "11"), err: cc("#e48a8a", "174", "9"),
		border: cc("#77ad82", "108", "2"), borderActive: cc("#ffffff", "231", "10"), title: cc("#e7f0e9", "255", "10"),
		bar: cc("#afcfb5", "151", "2"), barText: cc("#1b1e23", "234", "0"), barTitle: cc("#116a3e", "22", "0"),
		markedText: cc("#1b1e23", "234", "0"),
		header:     cc("#35805b", "65", "2"), headerText: cc("#e7f0e9", "255", "0"), headerName: cc("#ffffff", "231", "0"),
		ink: cc("#1b1e23", "234", "0"), inkText: cc("#ffffff", "231", "15"), statusText: cc("#ffbe10", "214", "11"), statusErr: cc("#e48a8a", "174", "9"),
		surface: cc("#dbe9e2", "254", "7"), surfaceText: cc("#1b1e23", "234", "0"),
		surfaceTitle: cc("#116a3e", "22", "0"), surfaceErr: cc("#ca1515", "160", "1"),
		accent: cc("#4a52b6", "61", "4"), accentText: cc("#ffffff", "231", "15"),
		codeTag: cc("#116a3e", "22", "2"),
	},
	// Dark blue; with 16 colours the terminal's blue, as Norton Commander.
	"blue": {
		panel: cc("#0f2a5c", "17", "4"), file: cc("#e2e9f5", "255", "7"), dir: cc("#ffffff", "231", "15"),
		dim: cc("#8ea3c8", "110", "6"), marked: cc("#ffbe10", "214", "11"), err: cc("#ff9a9a", "210", "9"),
		border: cc("#6f8fc4", "68", "6"), borderActive: cc("#ffffff", "231", "15"), title: cc("#e2e9f5", "255", "15"),
		bar: cc("#a9c1e8", "146", "6"), barText: cc("#10151d", "233", "0"), barTitle: cc("#0f2a5c", "17", "4"),
		markedText: cc("#10151d", "233", "0"),
		header:     cc("#2b4c86", "25", "6"), headerText: cc("#e2e9f5", "255", "0"), headerName: cc("#ffffff", "231", "0"),
		ink: cc("#10151d", "233", "0"), inkText: cc("#ffffff", "231", "15"), statusText: cc("#ffbe10", "214", "11"), statusErr: cc("#ff9a9a", "210", "9"),
		surface: cc("#dde5f2", "254", "7"), surfaceText: cc("#10151d", "233", "0"),
		surfaceTitle: cc("#0f2a5c", "17", "4"), surfaceErr: cc("#ca1515", "160", "1"),
		accent: cc("#1d7a62", "29", "2"), accentText: cc("#ffffff", "231", "15"),
		codeTag: cc("#0f2a5c", "17", "4"),
	},
	// Dark red; errors on the panels in orange, as red would not show.
	"red": {
		panel: cc("#6e1414", "52", "1"), file: cc("#f4e3e3", "254", "7"), dir: cc("#ffffff", "231", "15"),
		dim: cc("#c99494", "174", "0"), marked: cc("#ffbe10", "214", "11"), err: cc("#ffb080", "216", "14"),
		border: cc("#b87676", "138", "7"), borderActive: cc("#ffffff", "231", "15"), title: cc("#f4e3e3", "254", "15"),
		bar: cc("#e6b3b3", "181", "7"), barText: cc("#1e1919", "234", "0"), barTitle: cc("#6e1414", "52", "1"),
		markedText: cc("#1e1919", "234", "0"),
		header:     cc("#8f3333", "88", "9"), headerText: cc("#f4e3e3", "254", "0"), headerName: cc("#ffffff", "231", "0"),
		ink: cc("#1e1919", "234", "0"), inkText: cc("#ffffff", "231", "15"), statusText: cc("#ffbe10", "214", "11"), statusErr: cc("#ffb080", "216", "9"),
		surface: cc("#f2e0e0", "254", "7"), surfaceText: cc("#1e1919", "234", "0"),
		surfaceTitle: cc("#6e1414", "52", "1"), surfaceErr: cc("#b80000", "124", "9"),
		accent: cc("#2f5e9e", "25", "4"), accentText: cc("#ffffff", "231", "15"),
		codeTag: cc("#6e1414", "52", "1"),
	},
	// Gold panels with dark text: the one light theme, so marked rows are
	// blue instead of yellow.
	"yellow": {
		panel: cc("#d4a017", "178", "3"), file: cc("#1f1a0a", "234", "0"), dir: cc("#000000", "16", "0"),
		dim: cc("#5c4810", "58", "8"), marked: cc("#1d3f9e", "18", "4"), err: cc("#8a0e0e", "88", "1"),
		border: cc("#5c4810", "58", "8"), borderActive: cc("#000000", "16", "0"), title: cc("#1f1a0a", "234", "0"),
		bar: cc("#fff4cc", "230", "7"), barText: cc("#1f1a0a", "234", "0"), barTitle: cc("#7a5c00", "94", "4"),
		markedText: cc("#ffffff", "231", "15"),
		header:     cc("#b08410", "136", "11"), headerText: cc("#1f1a0a", "234", "0"), headerName: cc("#000000", "16", "0"),
		ink: cc("#1f1a0a", "234", "0"), inkText: cc("#ffffff", "231", "15"), statusText: cc("#ffd75e", "221", "11"), statusErr: cc("#ff8a7a", "210", "9"),
		surface: cc("#fbf3d9", "230", "7"), surfaceText: cc("#1f1a0a", "234", "0"),
		surfaceTitle: cc("#7a5c00", "94", "4"), surfaceErr: cc("#ca1515", "160", "1"),
		accent: cc("#1d3f9e", "18", "4"), accentText: cc("#ffffff", "231", "15"),
		codeTag: cc("#7a5c00", "94", "4"),
	},
	// Matte black, greys and one amber for what is marked.
	"black": {
		panel: cc("#1c1c1c", "234", "0"), file: cc("#d4d4d4", "188", "7"), dir: cc("#ffffff", "231", "15"),
		dim: cc("#808080", "244", "8"), marked: cc("#ffbe10", "214", "11"), err: cc("#e48a8a", "174", "9"),
		border: cc("#666666", "241", "8"), borderActive: cc("#e8e8e8", "255", "15"), title: cc("#d4d4d4", "188", "15"),
		bar: cc("#bdbdbd", "250", "7"), barText: cc("#121212", "233", "0"), barTitle: cc("#1c1c1c", "234", "0"),
		markedText: cc("#121212", "233", "0"),
		header:     cc("#303030", "236", "8"), headerText: cc("#d4d4d4", "188", "15"), headerName: cc("#ffffff", "231", "15"),
		ink: cc("#0e0e0e", "232", "0"), inkText: cc("#ffffff", "231", "15"), statusText: cc("#ffbe10", "214", "11"), statusErr: cc("#e48a8a", "174", "9"),
		surface: cc("#d6d6d6", "188", "7"), surfaceText: cc("#121212", "233", "0"),
		surfaceTitle: cc("#262626", "235", "0"), surfaceErr: cc("#b31212", "124", "1"),
		accent: cc("#4f5b66", "240", "4"), accentText: cc("#ffffff", "231", "15"),
		codeTag: cc("#262626", "235", "0"),
	},
}

// themeNamed returns the palette called name ("": the default).
func themeNamed(name string) (palette, error) {
	if name == "" {
		name = config.TUIThemes[0]
	}
	if p, ok := themes[name]; ok {
		return p, nil
	}
	return palette{}, fmt.Errorf("theme %q: want %s", name, strings.Join(config.TUIThemes, ", "))
}

type styles struct {
	// ascii: no colours (NO_COLOR, dumb terminal); see highlight.
	ascii bool
	pal   palette
	// On the panels.
	panel, file, dir, marked, dim, warn, err lipgloss.Style
	cursor, cursorMarked                     lipgloss.Style
	rule, title, titleActive                 lipgloss.Style
	border, borderActive                     lipgloss.Border
	borderFg, borderActiveFg                 lipgloss.TerminalColor
	// Screen chrome.
	header, headerName, keyNum, keyLabel, status, statusErr, spinner lipgloss.Style
	// Dialogs.
	dialog, dialogTitle, dialogErr, inputLabel, inputFocus, inputCursor lipgloss.Style
	dialogBar                                                           lipgloss.Style
}

// newStyles paints with the theme called theme, the default if there is
// none such (Run checks the name first).
func newStyles(theme string) *styles {
	p, err := themeNamed(theme)
	if err != nil {
		p, _ = themeNamed("")
	}
	on := func(bg lipgloss.TerminalColor) lipgloss.Style { return lipgloss.NewStyle().Background(bg) }
	st := &styles{
		pal:          p,
		panel:        on(p.panel).Foreground(p.file),
		file:         on(p.panel).Foreground(p.file),
		dir:          on(p.panel).Foreground(p.dir).Bold(true),
		marked:       on(p.panel).Foreground(p.marked).Bold(true),
		dim:          on(p.panel).Foreground(p.dim),
		warn:         on(p.panel).Foreground(p.marked),
		err:          on(p.panel).Foreground(p.err),
		cursor:       on(p.bar).Foreground(p.barText),
		cursorMarked: on(p.marked).Foreground(p.markedText).Bold(true),
		rule:         on(p.panel).Foreground(p.border),
		title:        on(p.panel).Foreground(p.title).Bold(true),
		titleActive:  on(p.bar).Foreground(p.barTitle).Bold(true),
		border:       lipgloss.NormalBorder(),
		borderActive: lipgloss.DoubleBorder(),

		borderFg:       p.border,
		borderActiveFg: p.borderActive,

		header:     on(p.header).Foreground(p.headerText),
		headerName: on(p.header).Foreground(p.headerName).Bold(true),
		keyNum:     on(p.ink).Foreground(p.inkText),
		keyLabel:   on(p.bar).Foreground(p.barText),
		status:     on(p.ink).Foreground(p.statusText),
		statusErr:  on(p.ink).Foreground(p.statusErr),
		spinner:    on(p.panel).Foreground(p.marked).Bold(true),

		dialog: on(p.surface).Foreground(p.surfaceText).Padding(0, 1).
			Border(lipgloss.DoubleBorder()).BorderForeground(p.surfaceTitle).BorderBackground(p.surface),
		dialogTitle: on(p.surface).Foreground(p.surfaceTitle).Bold(true),
		dialogErr:   on(p.surface).Foreground(p.surfaceErr).Bold(true),
		inputLabel:  on(p.surface).Foreground(p.surfaceText),
		dialogBar:   on(p.surface).Foreground(p.surfaceTitle),
		inputFocus:  on(p.accent).Foreground(p.accentText).Bold(true),
		inputCursor: on(p.accent).Foreground(p.accentText),
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
		return checkColors(mode)
	}
	return nil
}

// checkColors refuses a colour depth setColors does not know.
func checkColors(mode string) error {
	switch mode {
	case "", "auto", "true", "truecolor", "24bit", "256", "16", "none":
		return nil
	}
	return fmt.Errorf("colors %q: want auto, true, 256, 16 or none", mode)
}

// CheckOptions refuses colours or a theme the TUI does not know, before a
// node starts (and joins a share) for it.
func CheckOptions(o Options) error {
	if err := checkColors(o.Colors); err != nil {
		return err
	}
	_, err := themeNamed(o.Theme)
	return err
}

// sgrParams is the most parameters one SGR sequence gets. Some terminals
// keep 10 and let an 11th overwrite the 10th: bold with a 24-bit
// foreground and background is 11, and the background lost its blue
// (the blue theme's folders on green, the yellow theme's on red).
const sgrParams = 5

// shortSGR splits each SGR sequence in s longer than sgrParams into
// several, a colour's 38;2;r;g;b or 48;5;n kept in one. Applied in order
// they set the same attributes.
func shortSGR(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/8)
	for {
		i := strings.Index(s, "\x1b[")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i:]
		j := 2
		for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';') {
			j++
		}
		if j == len(s) || s[j] != 'm' || strings.Count(s[2:j], ";") < sgrParams {
			n := min(j+1, len(s))
			b.WriteString(s[:n])
			s = s[n:]
			continue
		}
		ps := strings.Split(s[2:j], ";")
		var group []string
		flush := func() {
			if len(group) > 0 {
				b.WriteString("\x1b[" + strings.Join(group, ";") + "m")
				group = group[:0]
			}
		}
		for k := 0; k < len(ps); {
			n := 1
			if (ps[k] == "38" || ps[k] == "48" || ps[k] == "58") && k+1 < len(ps) {
				switch ps[k+1] {
				case "2":
					n = 5
				case "5":
					n = 3
				}
			}
			n = min(n, len(ps)-k)
			if len(group)+n > sgrParams {
				flush()
			}
			group = append(group, ps[k:k+n]...)
			k += n
		}
		flush()
		s = s[j+1:]
	}
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
	line := lipgloss.NewStyle().Foreground(fg).Background(st.pal.panel)
	t := " " + fitLeft(title, max(1, inner-4)) + " "
	if active {
		t = st.highlight(ts, t)
	} else {
		t = ts.Render(t)
	}
	fill := max(0, inner-1-lipgloss.Width(t))
	top := line.Render(b.TopLeft+b.Top) + t + line.Render(strings.Repeat(b.Top, fill)+b.TopRight)
	body := st.panel.Border(b, false, true, true, true).
		BorderForeground(fg).BorderBackground(st.pal.panel).Width(inner).Render(content)
	return top + "\n" + body
}

// withCorner replaces the bottom-right corner of a box made by frame with
// s in the spinner style.
func (st *styles) withCorner(box string, active bool, s string) string {
	b := st.border
	if active {
		b = st.borderActive
	}
	lines := strings.Split(box, "\n")
	last := lines[len(lines)-1]
	if i := strings.LastIndex(last, b.BottomRight); i >= 0 {
		lines[len(lines)-1] = last[:i] + st.spinner.Render(s) + last[i+len(b.BottomRight):]
	}
	return strings.Join(lines, "\n")
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
