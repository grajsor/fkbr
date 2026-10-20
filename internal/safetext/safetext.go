// Package safetext makes text from elsewhere (file names, a peer's registry
// entry, error messages, file contents) safe to show on a terminal. Control
// characters in it could clear the screen, retitle the window or write the
// clipboard (OSC 52). Each such character, bidi control and invalid UTF-8
// byte becomes "?", as with ls -q.
package safetext

import (
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean replaces every unsafe character in s, line breaks and tabs too.
func Clean(s string) string { return clean(s, false) }

// Text is Clean but keeps line breaks and tabs.
func Text(s string) string { return clean(s, true) }

func unsafe(r rune, size int, lines bool) bool {
	switch {
	case r == utf8.RuneError && size == 1:
		return true
	case lines && (r == '\n' || r == '\t'):
		return false
	case unicode.IsControl(r):
		return true
	}
	// Bidi controls reorder what follows ("Trojan Source" names).
	return r == '؜' || r == '‎' || r == '‏' || r >= '‪' && r <= '‮' || r >= '⁦' && r <= '⁩'
}

func clean(s string, lines bool) string {
	i := 0
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if unsafe(r, n, lines) {
			break
		}
		i += n
	}
	if i == len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if unsafe(r, n, lines) {
			b.WriteByte('?')
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// Writer passes everything written to it through Text.
func Writer(w io.Writer) io.Writer { return writer{w} }

type writer struct{ w io.Writer }

func (x writer) Write(p []byte) (int, error) {
	if _, err := io.WriteString(x.w, Text(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Screen is for a rendered screen: it keeps colour codes (SGR, ESC [ ... m)
// and line breaks, and replaces every other control sequence's ESC and
// every other unsafe character.
func Screen(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if n := sgrLen(s[i:]); n > 0 {
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != '\n' && unsafe(r, n, false) {
			b.WriteByte('?')
		} else {
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// sgrLen is the length of the SGR sequence s starts with, or 0.
func sgrLen(s string) int {
	if len(s) < 3 || s[1] != '[' {
		return 0
	}
	for i := 2; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9' || c == ';' || c == ':':
		case c == 'm':
			return i + 1
		default:
			return 0
		}
	}
	return 0
}
