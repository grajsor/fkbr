package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

func TestLanguage(t *testing.T) {
	for name, want := range map[string]string{
		"n:files/config.xml": "xml", "n:files/pom.POM": "xml", "n:files/a.json": "json",
		"n:files/notes.txt": "", "n:files/noext": "",
	} {
		if got := language(name, "plain"); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if language("n:files/build.conf", "\n  <?xml version=\"1.0\"?><a/>") != "xml" {
		t.Error("<?xml declaration not detected")
	}
}

// classes renders segments as "class:text" for compact comparison.
func classes(lines [][]segment) string {
	names := map[int]string{clPlain: "p", clTag: "tag", clKey: "key", clString: "str", clLiteral: "lit", clComment: "com"}
	var out []string
	for _, l := range lines {
		var parts []string
		for _, s := range l {
			parts = append(parts, fmt.Sprintf("%s:%s", names[s.class], s.text))
		}
		out = append(out, strings.Join(parts, " "))
	}
	return strings.Join(out, "\n")
}

func TestTokenizeXML(t *testing.T) {
	// A tag and a comment that run over several lines.
	got := classes(tokenizeXML([]string{
		`<?xml version="1.0"?>`,
		`<job a="x"`,
		`  b='y'>text &amp; more</job> <!-- one`,
		`two --> <empty/>`,
	}))
	want := strings.Join([]string{
		`tag:<?xml p:  key:version p:= str:"1.0" tag:?>`,
		`tag:<job p:  key:a p:= str:"x"`,
		`p:   key:b p:= str:'y' tag:> p:text &amp; more tag:</job> p:  com:<!-- one`,
		`com:two --> p:  tag:<empty/>`,
	}, "\n")
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestTokenizeJSON(t *testing.T) {
	got := classes(tokenizeJSON([]string{`{"name": "fk\"br", "n": -1.5e3, "ok": true, "x": null}`}))
	want := `p:{ key:"name" p::  str:"fk\"br" p:,  key:"n" p::  lit:-1.5e3 p:,  key:"ok" p::  lit:true p:,  key:"x" p::  lit:null p:}`
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestHighlightWrapsAndPaints(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	st := newStyles()
	text := `<config>` + "\n\t" + `<name attr="` + strings.Repeat("v", 50) + `">välkommen</name>` + "\n</config>"
	out := highlight(st, "xml", text, 20)
	var plain []string
	for i, l := range strings.Split(out, "\n") {
		if w := lipgloss.Width(l); w != 20 {
			t.Errorf("line %d is %d wide", i, w)
		}
		if c := unpainted(l); c >= 0 {
			t.Errorf("line %d column %d has no background: %q", i, c, l)
		}
		plain = append(plain, strings.TrimRight(stripANSI(l), " "))
	}
	if got, want := strings.Join(plain, "\n"), wrapText(text, 20); got != want {
		t.Errorf("text changed by highlighting:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(out, st.inputLabel.Foreground(cCodeKey).Render("attr")) {
		t.Error("attribute not coloured")
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestWrapSegmentsWide(t *testing.T) {
	segs := wrapSegments([]segment{{"ab日本", clPlain}}, 3)
	for _, l := range segs {
		w := 0
		for _, s := range l {
			w += runewidth.StringWidth(s.text)
		}
		if w > 3 {
			t.Errorf("line %v is %d wide", l, w)
		}
	}
}
