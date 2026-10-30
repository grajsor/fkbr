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
	for name, want := range map[string]string{"n:files/run.sh": "sh", "n:files/x.BASH": "sh",
		"n:files/a.py": "py", "n:files/c.yaml": "yaml", "n:files/c.YML": "yaml"} {
		if got := language(name, "plain"); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	for text, want := range map[string]string{
		"#!/bin/bash\necho": "sh", "#!/bin/sh -e\n": "sh", "#!/usr/bin/env bash\n": "sh",
		"#! /bin/dash": "sh", "#!/usr/bin/python3\n": "py", "#!/usr/bin/env python\n": "py", "# just a comment": "",
	} {
		if got := language("n:files/noext", text); got != want {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
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

func TestTokenizeShell(t *testing.T) {
	got := classes(tokenizeShell([]string{
		`#!/bin/bash`,
		`if [ "$HOME" != '' ]; then  # check`,
		`  echo "${n:-1} \"x\" items" $# $1 a#b \#x $(date)`,
		`x='one`,
		`two # not a comment' ; fi`,
		`fiction done-x for`,
	}))
	want := strings.Join([]string{
		`com:#!/bin/bash`,
		`tag:if p: [  str:" key:$HOME str:" p: !=  str:'' p: ];  tag:then p:   com:# check`,
		`p:  echo  str:" key:${n:-1} str: \"x\" items" p:  key:$# p:  key:$1 p: a#b \#x $(date)`,
		`p:x= str:'one`,
		`str:two # not a comment' p: ;  tag:fi`,
		`p:fiction done-x  tag:for`,
	}, "\n")
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestTokenizePython(t *testing.T) {
	got := classes(tokenizePython([]string{
		`@dataclass`,
		`def f(x=1.5, s='a#b'):  # note`,
		`    """doc`,
		`    still doc"""`,
		`    return None if x else "y\"z"`,
		`definitely = True`,
	}))
	want := strings.Join([]string{
		`key:@dataclass`,
		`tag:def p: f(x= lit:1.5 p:, s= str:'a#b' p:):   com:# note`,
		`p:     str:"""doc`,
		`str:    still doc"""`,
		`p:     tag:return p:  lit:None p:  tag:if p: x  tag:else p:  str:"y\"z"`,
		`p:definitely =  lit:True`,
	}, "\n")
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestTokenizeYAML(t *testing.T) {
	got := classes(tokenizeYAML([]string{
		`---`,
		`# settings`,
		`name: "fk#br"   # quoted`,
		`  - port: 8080`,
		`    url: http://x/#frag`,
		`ok: true`,
		`base: &b {a: 1}`,
		`list: [one, 'two']`,
		`plain text: no: colon`,
	}))
	want := strings.Join([]string{
		`tag:---`,
		`com:# settings`,
		`key:name p::  str:"fk#br" p:    com:# quoted`,
		`p:  -  key:port p::  lit:8080`,
		`p:     key:url p:: http://x/#frag`,
		`key:ok p::  lit:true`,
		`key:base p::  tag:&b p: {a: 1}`,
		`key:list p:: [one,  str:'two' p:]`,
		`key:plain text p:: no: colon`,
	}, "\n")
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestHighlightWrapsAndPaints(t *testing.T) {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)
	st := newStyles("")
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
