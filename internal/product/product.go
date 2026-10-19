// Package product holds the product's name. Everything a user or another
// node sees that is derived from the name (messages, the binary, file and
// directory names, environment variables) comes from here, so a rename
// changes these constants plus the module path, cmd/ directory and docs.
package product

import "strings"

// Name is the binary and product name.
const Name = "fkbr"

// EnvPrefix starts the environment variables: EnvPrefix+"CONFIG" etc.
const EnvPrefix = "FKBR_"

var expander = strings.NewReplacer("{name}", Name, "{ENV}", EnvPrefix)

// Expand fills in {name} and {ENV} in longer help texts.
func Expand(s string) string { return expander.Replace(s) }
