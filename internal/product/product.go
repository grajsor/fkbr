// Package product holds the product's name. Everything a user or another
// node sees that is derived from the name (messages, the binary, file and
// directory names, environment variables) comes from here, so a rename
// changes these constants plus the module path, cmd/ directory and docs.
package product

import "strings"

// Name is the binary and product name.
const Name = "fkbr"

// Release is the version number; raise it for a release.
const Release = "1.0"

// Version is what a build calls itself: the release and the commit it was
// built from, e.g. "v1.0 5cc1bcd" (just "v1.0" without a commit).
func Version(commit string) string {
	if commit == "" || commit == "dev" {
		return "v" + Release
	}
	return "v" + Release + " " + commit
}

// EnvPrefix starts the environment variables: EnvPrefix+"CONFIG" etc.
const EnvPrefix = "FKBR_"

var expander = strings.NewReplacer("{name}", Name, "{ENV}", EnvPrefix)

// Expand fills in {name} and {ENV} in longer help texts.
func Expand(s string) string { return expander.Replace(s) }
