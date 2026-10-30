package tui

import (
	"path"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// Light syntax highlighting for the file viewer (F3): XML, JSON, YAML,
// shell and Python, by file extension, an <?xml declaration or a #! line.
// Colours are for the light dialog background; tags take the theme's
// (palette.codeTag).
var (
	cCodeKey     = lipgloss.CompleteColor{TrueColor: "#4a52b6", ANSI256: "61", ANSI: "4"}  // XML attributes, JSON keys, shell variables
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
	case ".sh", ".bash":
		return "sh"
	case ".py", ".pyw":
		return "py"
	case ".yaml", ".yml":
		return "yaml"
	}
	if strings.HasPrefix(strings.TrimLeft(text, " \t\r\n\ufeff"), "<?xml") {
		return "xml"
	}
	if shebang, ok := strings.CutPrefix(text, "#!"); ok {
		first, _, _ := strings.Cut(shebang, "\n")
		f := strings.Fields(first)
		if len(f) > 1 && path.Base(f[0]) == "env" {
			f = f[1:]
		}
		if len(f) > 0 {
			switch b := path.Base(f[0]); {
			case b == "sh" || b == "bash" || b == "dash" || b == "ksh" || b == "zsh":
				return "sh"
			case strings.HasPrefix(b, "python"):
				return "py"
			}
		}
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
		clTag:     base.Foreground(st.pal.codeTag).Bold(true),
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
	case "sh":
		lines = tokenizeShell(strings.Split(text, "\n"))
	case "py":
		lines = tokenizePython(strings.Split(text, "\n"))
	case "yaml":
		lines = tokenizeYAML(strings.Split(text, "\n"))
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

// shellKeywords are coloured where a word may start a command.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"for": true, "while": true, "until": true, "do": true, "done": true,
	"case": true, "esac": true, "in": true, "select": true, "function": true,
	"return": true, "local": true, "export": true, "readonly": true, "declare": true,
}

// tokenizeShell classifies sh and bash lightly: comments, quoted strings
// (which may run over several lines), keywords and $variables, also
// inside double quotes. Here-documents are left plain.
func tokenizeShell(lines []string) [][]segment {
	quote := rune(0) // the quote a string from an earlier line is still in
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
		// boundary: where a word or a comment may start or end.
		boundary := func(i int) bool {
			return i < 0 || i >= len(rs) || strings.ContainsRune(" \t;&|()`{}", rs[i])
		}
		// variable returns the end of a $variable at i, or i.
		variable := func(i int) int {
			if i+1 >= len(rs) {
				return i
			}
			switch r := rs[i+1]; {
			case r == '{':
				if end := strings.IndexRune(string(rs[i:]), '}'); end > 0 {
					return i + len([]rune(string(rs[i:])[:end])) + 1
				}
			case r == '_' || unicode.IsLetter(r):
				j := i + 1
				for j < len(rs) && (rs[j] == '_' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
					j++
				}
				return j
			case unicode.IsDigit(r) || strings.ContainsRune("@*#?$!-", r):
				return i + 2
			}
			return i
		}
		for i := 0; i < len(rs); {
			r := rs[i]
			switch {
			case quote == '\'': // nothing is special up to the closing quote
				j := i
				for j < len(rs) && rs[j] != '\'' {
					j++
				}
				if j < len(rs) {
					j++
					quote = 0
				}
				emit(string(rs[i:j]), clString)
				i = j
			case quote == '"':
				switch {
				case r == '\\' && i+1 < len(rs):
					emit(string(rs[i:i+2]), clString)
					i += 2
				case r == '$' && variable(i) > i:
					j := variable(i)
					emit(string(rs[i:j]), clKey)
					i = j
				default:
					if r == '"' {
						quote = 0
					}
					emit(string(r), clString)
					i++
				}
			case r == '\\':
				emit(string(rs[i:min(i+2, len(rs))]), clPlain)
				i += 2
			case r == '\'' || r == '"':
				quote = r
				emit(string(r), clString)
				i++
			case r == '#' && boundary(i-1):
				emit(string(rs[i:]), clComment)
				i = len(rs)
			case r == '$' && variable(i) > i:
				j := variable(i)
				emit(string(rs[i:j]), clKey)
				i = j
			case unicode.IsLetter(r) || r == '_':
				j := i
				for j < len(rs) && (rs[j] == '_' || rs[j] == '-' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
					j++
				}
				c := clPlain
				if shellKeywords[string(rs[i:j])] && boundary(i-1) && boundary(j) {
					c = clTag
				}
				emit(string(rs[i:j]), c)
				i = j
			default:
				emit(string(r), clPlain)
				i++
			}
		}
		out[li] = segs
	}
	return out
}

// segments collects a line's pieces, joining neighbours of one class.
type segments []segment

func (s *segments) add(text string, c int) {
	if text == "" {
		return
	}
	if n := len(*s); n > 0 && (*s)[n-1].class == c {
		(*s)[n-1].text += text
		return
	}
	*s = append(*s, segment{text, c})
}

var pythonKeywords = map[string]int{
	"and": clTag, "as": clTag, "assert": clTag, "async": clTag, "await": clTag,
	"break": clTag, "class": clTag, "continue": clTag, "def": clTag, "del": clTag,
	"elif": clTag, "else": clTag, "except": clTag, "finally": clTag, "for": clTag,
	"from": clTag, "global": clTag, "if": clTag, "import": clTag, "in": clTag,
	"is": clTag, "lambda": clTag, "nonlocal": clTag, "not": clTag, "or": clTag,
	"pass": clTag, "raise": clTag, "return": clTag, "try": clTag, "while": clTag,
	"with": clTag, "yield": clTag,
	"True": clLiteral, "False": clLiteral, "None": clLiteral,
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// tokenizePython classifies Python lightly: comments, strings (triple
// quoted ones over several lines), keywords, numbers and decorators.
func tokenizePython(lines []string) [][]segment {
	triple := "" // the triple quote a string from an earlier line is in
	out := make([][]segment, len(lines))
	for li, line := range lines {
		var segs segments
		rs := []rune(line)
		// str returns the end of a string whose quote q starts at i (or,
		// for a triple quote, continues there), and whether it is closed.
		str := func(i int, q string) (int, bool) {
			for j := i; j < len(rs); j++ {
				if rs[j] == '\\' {
					j++
					continue
				}
				if strings.HasPrefix(string(rs[j:]), q) {
					return j + len([]rune(q)), true
				}
			}
			return len(rs), false
		}
		i := 0
		if triple != "" {
			j, closed := str(0, triple)
			segs.add(string(rs[:j]), clString)
			if closed {
				triple = ""
			}
			i = j
		}
		for i < len(rs) {
			r := rs[i]
			switch {
			case r == '#':
				segs.add(string(rs[i:]), clComment)
				i = len(rs)
			case r == '"' || r == '\'':
				q := string(r)
				if strings.HasPrefix(string(rs[i:]), q+q+q) {
					q = q + q + q
				}
				j, closed := str(i+len([]rune(q)), q)
				if !closed && len(q) == 3 {
					triple = q
				}
				segs.add(string(rs[i:j]), clString)
				i = j
			case r == '@' && strings.TrimSpace(string(rs[:i])) == "":
				j := i + 1
				for j < len(rs) && (isWordRune(rs[j]) || rs[j] == '.') {
					j++
				}
				segs.add(string(rs[i:j]), clKey)
				i = j
			case unicode.IsDigit(r):
				j := i
				for j < len(rs) && (isWordRune(rs[j]) || rs[j] == '.') {
					j++
				}
				segs.add(string(rs[i:j]), clLiteral)
				i = j
			case isWordRune(r):
				j := i
				for j < len(rs) && isWordRune(rs[j]) {
					j++
				}
				c, ok := pythonKeywords[string(rs[i:j])]
				if !ok {
					c = clPlain
				}
				segs.add(string(rs[i:j]), c)
				i = j
			default:
				segs.add(string(r), clPlain)
				i++
			}
		}
		out[li] = segs
	}
	return out
}

// tokenizeYAML classifies YAML a line at a time: document markers, keys,
// quoted strings, anchors, aliases and tags, values that are one literal
// (a number, true, null, …) and comments.
func tokenizeYAML(lines []string) [][]segment {
	out := make([][]segment, len(lines))
	for li, line := range lines {
		var segs segments
		rs := []rune(line)
		if t := strings.TrimRight(line, " \t"); t == "---" || t == "..." {
			segs.add(line, clTag)
			out[li] = segs
			continue
		}
		// The indent and any "- " in front of a sequence item.
		i := 0
		for {
			for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
				i++
			}
			if i < len(rs) && rs[i] == '-' && (i+1 == len(rs) || rs[i+1] == ' ') {
				i++
				continue
			}
			break
		}
		segs.add(string(rs[:i]), clPlain)
		// A key: text up to ": " (or a ':' that ends the line), before
		// any comment and not quoted or a flow collection.
		if i < len(rs) && !strings.ContainsRune("#\"'[{&*!|>", rs[i]) {
			for j := i; j < len(rs); j++ {
				if rs[j] == '#' && (rs[j-1] == ' ' || rs[j-1] == '\t') {
					break
				}
				if rs[j] == ':' && (j+1 == len(rs) || rs[j+1] == ' ' || rs[j+1] == '\t') {
					segs.add(string(rs[i:j]), clKey)
					segs.add(":", clPlain)
					i = j + 1
					break
				}
			}
		}
		yamlValue(&segs, rs[i:])
		out[li] = segs
	}
	return out
}

// yamlValue classifies the rest of a YAML line after any key.
func yamlValue(segs *segments, rs []rune) {
	// Where a comment starts: a '#' at the start or after a blank, outside
	// quotes.
	end, quote := len(rs), rune(0)
	for j, r := range rs {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && (j == 0 || rs[j-1] == ' ' || rs[j-1] == '\t'):
			end = j
		}
		if end < len(rs) {
			break
		}
	}
	core := string(rs[:end])
	if t := strings.TrimSpace(core); yamlLiteral(t) {
		lead := core[:strings.Index(core, t)]
		segs.add(lead, clPlain)
		segs.add(t, clLiteral)
		segs.add(core[len(lead)+len(t):], clPlain)
	} else {
		v := []rune(core)
		for i := 0; i < len(v); {
			r := v[i]
			switch {
			case r == '"' || r == '\'':
				j := i + 1
				for j < len(v) && v[j] != r {
					if r == '"' && v[j] == '\\' {
						j++
					}
					j++
				}
				j = min(j+1, len(v))
				segs.add(string(v[i:j]), clString)
				i = j
			case (r == '&' || r == '*' || r == '!') && (i == 0 || v[i-1] == ' '):
				j := i + 1
				for j < len(v) && v[j] != ' ' && v[j] != ',' && v[j] != ']' && v[j] != '}' {
					j++
				}
				segs.add(string(v[i:j]), clTag)
				i = j
			default:
				segs.add(string(r), clPlain)
				i++
			}
		}
	}
	segs.add(string(rs[end:]), clComment)
}

// yamlLiteral reports whether a whole value is a number or a keyword.
func yamlLiteral(t string) bool {
	switch strings.ToLower(t) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	case "":
		return false
	}
	digits := 0
	for i, r := range t {
		switch {
		case unicode.IsDigit(r):
			digits++
		case (r == '-' || r == '+') && i == 0, r == '.', r == '_', r == 'e', r == 'E':
		default:
			return false
		}
	}
	return digits > 0
}
