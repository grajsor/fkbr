package tui

import (
	"strings"
	"testing"
)

// matcher answers with an offset into the line it was given, also where
// lower case takes more bytes than upper case (U+023A: 2 bytes, its lower
// case 3) or fewer (U+0130: 2 bytes, lower case i 1). The editor slices
// the line at that offset: a lower-cased index there ran past the end and
// stopped the program.
func TestMatcherOffsetsAreInTheLine(t *testing.T) {
	for _, c := range []struct{ line, q, want string }{
		{"ȺȺX", "x", "X"},
		{strings.Repeat("Ⱥ", 10) + "X", "x", "X"},
		{"ȾȺ-Needle", "needle", "Needle"},
		{"İx", "x", "x"},
		{"Hello World", "o w", "o W"},
		{"Hello", "ll", "ll"},
		{"Ⱥ", "x", ""},
	} {
		at := matcher(c.q)(c.line)
		if c.want == "" {
			if at != -1 {
				t.Errorf("%q in %q: %d, want no match", c.q, c.line, at)
			}
			continue
		}
		if at < 0 || at > len(c.line) || !strings.HasPrefix(c.line[at:], c.want) {
			t.Errorf("%q in %q: offset %d, want the start of %q", c.q, c.line, at, c.want)
		}
	}
	// With a capital in the query the case counts, as before.
	if at := matcher("X")("ȺȺx X"); at != strings.Index("ȺȺx X", "X") {
		t.Errorf("exact case: %d", at)
	}
}

func TestEditorFindWithWideLowerCase(t *testing.T) {
	d := &editDialog{lines: [][]rune{[]rune("ȺȺX"), []rune("İ and x")}, height: 10}
	d.find("x")
	if d.row != 0 || d.col != 2 {
		t.Fatalf("cursor at %d,%d, want 0,2", d.row, d.col)
	}
	d.find("x")
	if d.row != 1 || d.col != 6 {
		t.Fatalf("cursor at %d,%d, want 1,6", d.row, d.col)
	}
}
