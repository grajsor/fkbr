package api

import "testing"

// Key is what the target's filesystem compares names by (review
// 2026-11-05): casefolding ext4 takes straße and STRASSE, Σ and ς for one
// name (checked on a casefold ext4); vfat lowercases letter by letter
// (checked on a vfat mount: σ and Σ are one name there, Σ and ς two).
func TestNameRulesKey(t *testing.T) {
	same := func(n NameRules, a, b string) {
		t.Helper()
		if n.Key(a) != n.Key(b) {
			t.Errorf("%s: %q (%q) and %q (%q) are different names, want one", n.FS, a, n.Key(a), b, n.Key(b))
		}
	}
	differ := func(n NameRules, a, b string) {
		t.Helper()
		if n.Key(a) == n.Key(b) {
			t.Errorf("%s: %q and %q are one name (%q), want two", n.FS, a, b, n.Key(a))
		}
	}
	cf := NameRules{FS: "casefold", Fold: true, Form: true}
	same(cf, "STRASSE", "straße")
	same(cf, "Straße", "STRASSE")
	same(cf, "Σ", "ς")
	same(cf, "σ", "ς")
	same(cf, "README", "readme")
	same(cf, "café", "CAFÉ") // NFD and NFC, any case
	differ(cf, "readme", "readme2")
	differ(cf, "ss", "s")

	// Not casefold: letter by letter, no ß to ss.
	for _, fs := range []string{"vfat", "exfat"} {
		fat := NameRules{FS: fs, Fold: true, UTF8: true}
		same(fat, "README", "readme")
		same(fat, "Σ", "σ")
		differ(fat, "Σ", "ς")
		differ(fat, "STRASSE", "straße")
		differ(fat, "a", "b")
	}

	// Without folding a name is itself.
	if k := (NameRules{}).Key("README"); k != "README" {
		t.Errorf("no folding: %q", k)
	}
}
