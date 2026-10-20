package product

import (
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
