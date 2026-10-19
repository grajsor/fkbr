// Package id generates unique, time-sortable identifiers for requests and
// jobs. They are used as file names on the share, so lexical order of the
// names equals creation order (per creating host clock).
package id

import (
	"fmt"
	"math/rand/v2"
	"time"
)

// New returns a 24-character lowercase hex ID: 16 digits of Unix nanos
// followed by 8 random digits.
func New() string {
	return fmt.Sprintf("%016x%08x", time.Now().UnixNano(), rand.Uint32())
}
