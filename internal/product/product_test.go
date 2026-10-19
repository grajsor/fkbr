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
