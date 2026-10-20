package main

import (
	"flag"
	"slices"
	"testing"
)

// Global flags work after the command too; the command keeps its own
// flags (prune -keep), its arguments, and everything after "--".
func TestGlobalFlagsAfterCommand(t *testing.T) {
	fs := flag.NewFlagSet("fkbr", flag.ContinueOnError)
	fs.String("node", "", "")
	fs.String("state", "", "")
	fs.Bool("ro", false, "")
	fs.Duration("keep", 0, "")
	for _, tc := range []struct {
		cmd          string
		args         []string
		global, rest []string
	}{
		{"daemon", []string{"-node", "a"}, []string{"-node", "a"}, nil},
		{"daemon", []string{"--node=a", "-ro"}, []string{"--node=a", "-ro"}, nil},
		{"tui", []string{"-ro", "-state", "/tmp/s"}, []string{"-ro", "-state", "/tmp/s"}, nil},
		{"cp", []string{"-z", "/a", "-node", "x", "b:/c"}, []string{"-node", "x"}, []string{"-z", "/a", "b:/c"}},
		{"cp", []string{"/a", "--", "-node"}, nil, []string{"/a", "--", "-node"}},
		{"prune", []string{"-keep", "5m", "-node", "x"}, []string{"-node", "x"}, []string{"-keep", "5m"}},
		{"ls", []string{"-", "-json"}, nil, []string{"-", "-json"}},
	} {
		global, rest := globalFlags(fs, tc.cmd, tc.args)
		if !slices.Equal(global, tc.global) || !slices.Equal(rest, tc.rest) {
			t.Errorf("%s %q: global %q rest %q, want %q %q", tc.cmd, tc.args, global, rest, tc.global, tc.rest)
		}
	}
}
