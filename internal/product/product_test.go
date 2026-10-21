package product

import (
	"os"
	"strings"
	"testing"
)

func TestEnvPrefixMatchesName(t *testing.T) {
	if EnvPrefix != strings.ToUpper(Name)+"_" {
		t.Fatalf("EnvPrefix %q does not match Name %q", EnvPrefix, Name)
	}
}

func TestExpand(t *testing.T) {
	if got, want := Expand("{name} uses ${ENV}SHARE"), Name+" uses $"+EnvPrefix+"SHARE"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersion(t *testing.T) {
	if got, want := Version("5cc1bcd"), "v"+Release+" 5cc1bcd"; got != want {
		t.Errorf("Version = %q, want %q", got, want)
	}
	if got, want := Version("dev"), "v"+Release; got != want {
		t.Errorf("Version(dev) = %q, want %q", got, want)
	}
}

// In a checkout commit.txt holds the placeholder, which must not count.
// In a zip or tar.gz from git archive it holds the commit instead.
func TestArchivedCommitInCheckout(t *testing.T) {
	raw, _ := os.ReadFile("commit.txt")
	if !strings.HasPrefix(string(raw), "$Format") {
		if c := ArchivedCommit(); len(c) < 7 {
			t.Fatalf("archive with commit.txt %q: ArchivedCommit = %q", raw, c)
		}
		t.Skipf("not a checkout: commit.txt holds %q", strings.TrimSpace(string(raw)))
	}
	if c := ArchivedCommit(); c != "" {
		t.Fatalf("ArchivedCommit in a checkout = %q", c)
	}
}
