package cli

import (
	"flag"
	"fmt"
	"testing"
)

func TestParseFlagsAnywhere(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string // mirror, max-deletes, arguments
	}{
		{[]string{"-mirror", "-max-deletes", "-1", "/a", "b:/c"}, "true -1 [/a b:/c]"},
		{[]string{"/a", "b:/c", "-mirror", "-max-deletes", "-1"}, "true -1 [/a b:/c]"},
		{[]string{"/a", "-mirror", "b:/c"}, "true 0 [/a b:/c]"},
		{[]string{"/a", "b:/c"}, "false 0 [/a b:/c]"},
		{[]string{"/a", "--", "-dash", "-mirror"}, "false 0 [/a -dash -mirror]"},
	} {
		fl := flag.NewFlagSet("t", flag.ContinueOnError)
		mirror := fl.Bool("mirror", false, "")
		del := fl.Int("max-deletes", 0, "")
		if err := parseFlags(fl, tc.args); err != nil {
			t.Fatalf("%q: %v", tc.args, err)
		}
		if got := fmt.Sprint(*mirror, " ", *del, " ", fl.Args()); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.args, got, tc.want)
		}
	}
}
