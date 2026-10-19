package tui

import (
	"path"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Light syntax highlighting for the file viewer (F3): XML and JSON, by file
// extension or an <?xml declaration. Colours are for the light dialog
// background.
var (
	cCodeTag     = lipgloss.CompleteColor{TrueColor: "#116a3e", ANSI256: "22", ANSI: "2"}  // XML tags
	cCodeKey     = lipgloss.CompleteColor{TrueColor: "#4a52b6", ANSI256: "61", ANSI: "4"}  // XML attributes, JSON keys
	cCodeString  = lipgloss.CompleteColor{TrueColor: "#a8321f", ANSI256: "124", ANSI: "1"} // quoted values
	cCodeLiteral = lipgloss.CompleteColor{TrueColor: "#8a5a00", ANSI256: "94", ANSI: "5"}  // numbers, true/false/null
	cCodeComment = lipgloss.CompleteColor{TrueColor: "#5f6165", ANSI256: "241", ANSI: "0"}
)

// language picks a highlighter for a viewed file, or "".
func language(name, text string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".xml", ".xsd", ".xsl", ".xslt", ".svg", ".pom", ".plist", ".xhtml", ".wsdl", ".jelly":
		return "xml"
	case ".json", ".geojson", ".jsonl", ".ndjson":
		return "json"
	}
	if strings.HasPrefix(strings.TrimLeft(text, " \t\r\n\ufeff"), "<?xml") {
		return "xml"
	}
	return ""
}

// segment is a run of text in one token class.
type segment struct {
	text  string
	class int
}

const (
	clPlain = iota
	clTag
	clKey
	clString
	clLiteral
	clComment
)

// highlight renders text in lang, hard-wrapped to width cells, each piece
// on the dialog background.
func highlight(st *styles, lang, text string, width int) string {
	base := st.inputLabel
	classes := map[int]lipgloss.Style{
		clPlain:   base,
		clTag:     base.Foreground(cCodeTag).Bold(true),
		clKey:     base.Foreground(cCodeKey),
		clString:  base.Foreground(cCodeString),
		clLiteral: base.Foreground(cCodeLiteral),
		clComment: base.Foreground(cCodeComment).Italic(true),
	}
	var lines [][]segment
	text = strings.ReplaceAll(text, "\t", "    ")
	switch lang {
	case "xml":
		lines = tokenizeXML(strings.Split(text, "\n"))
	case "json":
		lines = tokenizeJSON(strings.Split(text, "\n"))
	}
	var out []string
	for _, line := range lines {
		for _, w := range wrapSegments(line, width) {
			var b strings.Builder
			used := 0
			for _, s := range w {
				b.WriteString(classes[s.class].Render(s.text))
				used += runewidth.StringWidth(s.text)
			}
			// Pad here: the viewport's own padding follows the last reset
			// and would show the terminal's background.
			if used < width {
				b.WriteString(base.Render(strings.Repeat(" ", width-used)))
			}
			out = append(out, b.String())
		}
	}
	return strings.Join(out, "\n")
}

// wrapSegments cuts a line of segments into lines of at most width cells,
// keeping each piece's class.
func wrapSegments(line []segment, width int) [][]segment {
	out := [][]segment{nil}
	used := 0
	for _, s := range line {
		for s.text != "" {
			room := width - used
			if room <= 0 {
				out = append(out, nil)
				used, room = 0, width
			}
			head := runewidth.Truncate(s.text, room, "")
			if head == "" { // a wide rune that does not fit: next line
				out = append(out, nil)
				used = 0
				continue
			}
			out[len(out)-1] = append(out[len(out)-1], segment{head, s.class})
			used += runewidth.StringWidth(head)
			s.text = s.text[len(head):]
		}
	}
	return out
}

// tokenizeXML classifies XML; tags, attribute values and comments may run
// over several lines.
func tokenizeXML(lines []string) [][]segment {
	const (
		text = iota
		inTag
		inComment
	)
	state, quote := text, rune(0)
	out := make([][]segment, len(lines))
	for li, line := range lines {
		var segs []segment
		emit := func(s string, c int) {
			if s == "" {
				return
			}
			if n := len(segs); n > 0 && segs[n-1].class == c {
				segs[n-1].text += s
				return
			}
			segs = append(segs, segment{s, c})
		}
		rs := []rune(line)
		for i := 0; i < len(rs); {
			switch {
			case state == inComment:
				end := strings.Index(string(rs[i:]), "-->")
				if end < 0 {
					emit(string(rs[i:]), clComment)
					i = len(rs)
					continue
				}
				n := len([]rune(string(rs[i:])[:end])) + 3
				emit(string(rs[i:i+n]), clComment)
				i += n
				state = text
			case quote != 0: // attribute value, possibly continued
				j := i
				for j < len(rs) && rs[j] != quote {
					j++
				}
				if j < len(rs) {
					j++
					quote = 0
				}
				emit(string(rs[i:j]), clString)
				i = j
			case state == inTag:
				r := rs[i]
				switch {
				case r == '"' || r == '\'':
					quote = r
					emit(string(r), clString)
					i++
				case r == '>' || (r == '/' || r == '?') && i+1 < len(rs) && rs[i+1] == '>':
					n := 1
					if r != '>' {
						n = 2
					}
					emit(string(rs[i:i+n]), clTag)
					i += n
					state = text
				case isNameRune(r):
					j := i
					for j < len(rs) && isNameRune(rs[j]) {
						j++
					}
					emit(string(rs[i:j]), clKey)
					i = j
				default:
					emit(string(r), clPlain)
					i++
				}
			case strings.HasPrefix(string(rs[i:]), "<!--"):
				emit("<!--", clComment)
				i += 4
				state = inComment
			case rs[i] == '<':
				j := i + 1
				for j < len(rs) && (rs[j] == '/' || rs[j] == '?' || rs[j] == '!') {
					j++
				}
				for j < len(rs) && isNameRune(rs[j]) {
					j++
				}
				emit(string(rs[i:j]), clTag)
				i = j
				state = inTag
			default:
				j := i
				for j < len(rs) && rs[j] != '<' {
					j++
				}
				emit(string(rs[i:j]), clPlain)
				i = j
			}
		}
		out[li] = segs
	}
	return out
}

func isNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == ':' || r == '-' || r == '_' || r == '.'
}

// tokenizeJSON classifies JSON, telling keys (strings followed by ':')
// from values. Strings do not span lines in JSON.
func tokenizeJSON(lines []string) [][]segment {
	out := make([][]segment, len(lines))
	for li, line := range lines {
		var segs []segment
		rs := []rune(line)
		for i := 0; i < len(rs); {
			r := rs[i]
			switch {
			case r == '"':
				j := i + 1
				for j < len(rs) && rs[j] != '"' {
					if rs[j] == '\\' {
						j++
					}
					j++
				}
				j = min(j+1, len(rs))
				k := j
				for k < len(rs) && rs[k] == ' ' {
					k++
				}
				c := clString
				if k < len(rs) && rs[k] == ':' {
					c = clKey
				}
				segs = append(segs, segment{string(rs[i:j]), c})
				i = j
			case r == '-' || unicode.IsDigit(r):
				j := i + 1
				for j < len(rs) && strings.ContainsRune("0123456789.eE+-", rs[j]) {
					j++
				}
				segs = append(segs, segment{string(rs[i:j]), clLiteral})
				i = j
			case unicode.IsLetter(r):
				j := i
				for j < len(rs) && unicode.IsLetter(rs[j]) {
					j++
				}
				c := clPlain
				switch string(rs[i:j]) {
				case "true", "false", "null":
					c = clLiteral
				}
				segs = append(segs, segment{string(rs[i:j]), c})
				i = j
			default:
				j := i
				for j < len(rs) && !strings.ContainsRune("\"-0123456789", rs[j]) && !unicode.IsLetter(rs[j]) {
					j++
				}
				segs = append(segs, segment{string(rs[i:j]), clPlain})
				i = j
			}
		}
		out[li] = segs
	}
	return out
}
