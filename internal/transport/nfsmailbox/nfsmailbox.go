// Package nfsmailbox opens a share on an NFS mount that every node reaches:
// the store (package store) plus what NFS needs, the refresh trick for
// mounts that cache attributes and lookups for long.
package nfsmailbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fkbr/internal/store"
)

type Options struct {
	// Poll is how often AwaitReplies checks for new reply parts.
	Poll time.Duration
	// OfflineAfter is the heartbeat age after which a node counts as offline.
	OfflineAfter time.Duration
	// Refresh compensates for NFS mounts that cache attributes and lookups
	// for long (e.g. actimeo=1800): before reading a directory, or a file
	// in it, that other nodes change, a tiny file is created and removed
	// in that directory. The client then revalidates the directory and its
	// entries, so renamed-in files and new names become visible at once.
	Refresh bool
}

// Open opens the share at root (the NFS mount, or a directory on it) for
// node self.
func Open(root, self string, opts Options) (*store.Dir, error) {
	so := store.Options{Poll: opts.Poll, OfflineAfter: opts.OfflineAfter}
	if opts.Refresh {
		so.Fresh = newRefresher(root, self).fresh
	}
	return store.Open(root, self, so)
}

// refreshEvery throttles directory refreshes per directory.
const refreshEvery = 100 * time.Millisecond

type refresher struct {
	root, self string
	mu         sync.Mutex
	at         map[string]time.Time
	seq        atomic.Uint64
}

func newRefresher(root, self string) *refresher {
	return &refresher{root: root, self: self, at: map[string]time.Time{}}
}

// fresh makes other clients' changes in dir visible (see Options.Refresh).
// The probe files are named ".r-<node>-<n>"; the store skips such names.
func (r *refresher) fresh(dir string) {
	now := time.Now()
	r.mu.Lock()
	if now.Sub(r.at[dir]) < refreshEvery {
		r.mu.Unlock()
		return
	}
	r.at[dir] = now
	r.mu.Unlock()
	p := filepath.Join(dir, fmt.Sprintf(".r-%s-%d", r.self, r.seq.Add(1)))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
		os.Remove(p)
		return
	}
	// dir may exist on the server while this client still caches it as
	// missing (looked up before another node created it). Refresh the
	// parent so that cached "does not exist" is dropped.
	if parent := filepath.Dir(dir); errors.Is(err, fs.ErrNotExist) && parent != dir && strings.HasPrefix(parent, r.root) {
		r.fresh(parent)
	}
}
