package safetext

import (
	"bytes"
	"testing"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"plain.txt":                "plain.txt",
		"ünïcode 日本":               "ünïcode 日本",
		"\x1b]0;PWNED\x07title":    "?]0;PWNED?title",
		"\x1b]52;c;cHduZWQ=\x07":   "?]52;c;cHduZWQ=?",
		"cr\rback\nnl\ttab\x7fdel": "cr?back?nl?tab?del",
		"c1\u009bcsi":              "c1?csi",
		"latin1 \xe9t\xe9":         "latin1 ?t?",
		"evil‮txt.exe":             "evil?txt.exe",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Text("a\tb\nc\x1b[2J"); got != "a\tb\nc?[2J" {
		t.Errorf("Text: %q", got)
	}
	var b bytes.Buffer
	Writer(&b).Write([]byte("name\x1b]0;x\x07\n"))
	if b.String() != "name?]0;x?\n" {
		t.Errorf("Writer: %q", b.String())
	}
}

func TestScreen(t *testing.T) {
	in := "\x1b[38;2;17;106;62;48;5;22mok\x1b[0m\n\x1b[7mrev\x1b[m \x1b]0;t\x07 \x1b[2J\r"
	want := "\x1b[38;2;17;106;62;48;5;22mok\x1b[0m\n\x1b[7mrev\x1b[m ?]0;t? ?[2J?"
	if got := Screen(in); got != want {
		t.Errorf("Screen:\n got %q\nwant %q", got, want)
	}
}
