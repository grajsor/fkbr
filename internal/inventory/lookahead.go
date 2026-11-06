package inventory

import (
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"strings"
)

// A target that folds names together (FAT, exFAT, casefold ext4) takes
// README and readme for one. The merge sees the entries of a folder in the
// order of their bytes, so a target's readme that sorts after the source's
// README is not known when README is planned: it would be sent, and the
// receiver would find the name taken, every run. Before the merge, a pass
// over the target's snapshot and one over the source's find the source
// names that fold onto a different spelling the target keeps, and
// cannotStore leaves those out (review 2026-11-05). Memory: a few words
// per entry of a folding target, nothing for the others.

// foldClash is, for one target, the hashes of the source paths to leave
// out, and the hash function that made them.
type foldClash struct {
	out  map[uint64]bool
	hash func(string) uint64
}

func (p *planner) lookahead() error {
	p.clash = make([]*foldClash, len(p.targets))
	for i, t := range p.targets {
		n := t.Snap.Header.Names
		if n == nil || !n.Fold || t.Again == nil || p.opts.SrcAgain == nil {
			continue
		}
		seed := maphash.MakeSeed()
		hash := func(s string) uint64 { return maphash.String(seed, s) }
		folded := func(path string) uint64 {
			return hash(parent(path) + "\x00" + n.Key(path[strings.LastIndexByte(path, '/')+1:]))
		}
		// What the target holds: folded name -> the spelling it has.
		held := map[uint64]uint64{}
		if err := eachEntry(t.Again, func(e *Entry) {
			if e.P != "." {
				if k := folded(e.P); held[k] == 0 {
					held[k] = hash(e.P) | 1 // never 0
				}
			}
		}); err != nil {
			return fmt.Errorf("%s: %w", t.Node, err)
		}
		// The source names that fold onto another spelling, and which of
		// the target's spellings the source has too (a mirror keeps those).
		kept := map[uint64]bool{}
		other := map[uint64]uint64{} // source path -> folded name
		if err := eachEntry(p.opts.SrcAgain, func(e *Entry) {
			if e.P == "." || e.T != TypeFile && e.T != TypeDir && e.T != TypeSymlink {
				return
			}
			k := folded(e.P)
			switch h, ok := held[k]; {
			case !ok:
			case h == hash(e.P)|1:
				kept[k] = true
			default:
				other[hash(e.P)|1] = k
			}
		}); err != nil {
			return err
		}
		c := &foldClash{out: map[uint64]bool{}, hash: func(s string) uint64 { return hash(s) | 1 }}
		for path, k := range other {
			// A mirror removes the target's spelling unless the source has
			// it too; otherwise it stays, and the two would clash.
			if !p.opts.Mirror || kept[k] {
				c.out[path] = true
			}
		}
		if len(c.out) > 0 {
			p.clash[i] = c
		}
	}
	return nil
}

// eachEntry reads every entry of the snapshot open() returns.
func eachEntry(open func() (*Reader, io.Closer, error), fn func(*Entry)) error {
	r, c, err := open()
	if err != nil {
		return err
	}
	defer c.Close()
	defer r.Close()
	for {
		e, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		fn(e)
	}
}
