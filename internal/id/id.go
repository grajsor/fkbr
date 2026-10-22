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
func New() string { return At(time.Now()) }

// At is New for a given creation time (the share's clock: see
// store.Dir.Now), so IDs from a host whose clock is off still
// sort with everyone else's.
func At(t time.Time) string {
	return fmt.Sprintf("%016x%08x", t.UnixNano(), rand.Uint32())
}
