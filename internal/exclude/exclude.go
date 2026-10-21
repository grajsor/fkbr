// Package exclude matches the patterns that leave entries out of a copy or
// sync, like rsync's --exclude (in a simple form).
//
// A pattern without a slash ("*.tmp", ".git") matches a name at any depth;
// one with a slash ("build/out", "logs/*.gz") matches the path inside what
// is copied or synced, from its top; a leading slash anchors a plain name
// to the top too ("/notes.txt"). A trailing slash matches directories
// only. Wildcards are those of path.Match: * and ? never cross a slash.
// An excluded directory leaves out everything below it.
package exclude

import (
	"fmt"
	"path"
	"strings"
)

// Set is a parsed list of patterns. A nil *Set matches nothing.
type Set struct {
	pats []pattern
	// key, if set, is applied to names before matching (see WithKey).
	key func(string) string
}

// WithKey returns s matching names as key(name) against patterns made with
// key: a case-insensitive filesystem holds CACHE where the pattern says
// cache, and to it they are one name.
func (s *Set) WithKey(key func(string) string) *Set {
	if s == nil {
		return nil
	}
	c := &Set{key: key}
	for _, p := range s.pats {
		p.glob = key(p.glob)
		c.pats = append(c.pats, p)
	}
	return c
}

type pattern struct {
	glob     string
	anchored bool // matched against the whole path, not the last name
	dirOnly  bool
}

// New parses patterns, ignoring empty ones; with none it returns nil.
func New(patterns []string) (*Set, error) {
	var s Set
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		var pt pattern
		if strings.HasSuffix(p, "/") {
			pt.dirOnly, p = true, strings.TrimRight(p, "/")
		}
		if strings.HasPrefix(p, "/") {
			pt.anchored, p = true, strings.TrimLeft(p, "/")
		}
		pt.anchored = pt.anchored || strings.Contains(p, "/")
		if p == "" {
			return nil, fmt.Errorf("exclude %q: no name", raw)
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("exclude %q: %w", raw, err)
		}
		pt.glob = p
		s.pats = append(s.pats, pt)
	}
	if len(s.pats) == 0 {
		return nil, nil
	}
	return &s, nil
}

// Match reports whether rel (a slash path inside what is copied, "." for
// its top) is left out; dir says whether it is a directory.
func (s *Set) Match(rel string, dir bool) bool {
	if s == nil || rel == "." || rel == "" {
		return false
	}
	if s.key != nil {
		rel = s.key(rel)
	}
	base := path.Base(rel)
	for _, p := range s.pats {
		if p.dirOnly && !dir {
			continue
		}
		name := base
		if p.anchored {
			name = rel
		}
		if ok, _ := path.Match(p.glob, name); ok {
			return true
		}
	}
	return false
}
