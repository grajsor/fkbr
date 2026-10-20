package inventory

import (
	"errors"
	"fmt"
	"io"
)

// Target is one destination snapshot plus what it can preserve, so that
// differences it cannot fix anyway (ownership) do not cause re-copies.
type Target struct {
	Node string
	Snap *Reader
	// Chown: the target preserves file ownership; ChownDirs: directory
	// ownership too.
	Chown, ChownDirs bool
}

type PlanOptions struct {
	Mirror   bool
	Checksum bool
}

// Sink receives the plan as it is computed. Copy is called once per entry
// with the set of targets (bit i = targets[i]) that need it; Delete once
// per top-most entry to remove from a target.
type Sink interface {
	Copy(mask uint64, e *Entry) error
	Delete(target int, p string) error
}

// TargetStats counts what the plan does on one target.
type TargetStats struct {
	Copy, CopyBytes, Replace, Delete, Unchanged int64
}

type PlanStats struct {
	SrcEntries, SrcBytes int64
	Targets              []TargetStats
	Problems             []string
}

const maxProblems = 100

func (s *PlanStats) problem(format string, args ...any) {
	if len(s.Problems) < maxProblems {
		s.Problems = append(s.Problems, fmt.Sprintf(format, args...))
	}
}

// openDir tracks a source directory whose subtree is being merged: which
// targets need its entry re-sent because its contents change there (which
// changes its mtime), and which already get it.
type openDir struct {
	e             *Entry
	changed, sent uint64
}

type planner struct {
	targets []Target
	opts    PlanOptions
	sink    Sink
	stats   *PlanStats
	cur     []*Entry // current entry per target
	src     *Entry
	srcRd   *Reader
	dirs    []openDir
	// srcUnknown: source subtrees that could not be read; nothing below
	// them is deleted on targets.
	srcUnknown []string
	// deleted: per target, the top-most directory being removed.
	deleted []string
}

// Plan merges the source snapshot with every target snapshot in one pass.
func Plan(src *Reader, targets []Target, opts PlanOptions, sink Sink) (*PlanStats, error) {
	if len(targets) == 0 || len(targets) > 64 {
		return nil, fmt.Errorf("plan: need 1 to 64 targets, got %d", len(targets))
	}
	p := &planner{
		targets: targets, opts: opts, sink: sink,
		stats:   &PlanStats{Targets: make([]TargetStats, len(targets))},
		cur:     make([]*Entry, len(targets)),
		srcRd:   src,
		deleted: make([]string, len(targets)),
	}
	var err error
	if p.src, err = next(src); err != nil {
		return nil, err
	}
	for i, t := range targets {
		if p.cur[i], err = next(t.Snap); err != nil {
			return nil, fmt.Errorf("%s: %w", t.Node, err)
		}
	}
	for {
		path, ok := p.minPath()
		if !ok {
			break
		}
		if err := p.step(path); err != nil {
			return nil, err
		}
	}
	if err := p.closeDirs(""); err != nil {
		return nil, err
	}
	return p.stats, nil
}

func next(r *Reader) (*Entry, error) {
	e, err := r.Next()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	return e, err
}

func (p *planner) minPath() (string, bool) {
	min, ok := "", false
	consider := func(e *Entry) {
		if e != nil && (!ok || ComparePaths(e.P, min) < 0) {
			min, ok = e.P, true
		}
	}
	consider(p.src)
	for _, e := range p.cur {
		consider(e)
	}
	return min, ok
}

func bit(i int) uint64 { return 1 << uint(i) }

// closeDirs pops open directories that do not contain path ("" pops all),
// emitting their entries where their contents changed.
func (p *planner) closeDirs(path string) error {
	for len(p.dirs) > 0 {
		top := p.dirs[len(p.dirs)-1]
		if path != "" && under(path, top.e.P) {
			return nil
		}
		p.dirs = p.dirs[:len(p.dirs)-1]
		if need := top.changed &^ top.sent; need != 0 {
			if err := p.sink.Copy(need, top.e); err != nil {
				return err
			}
			for i := range p.targets {
				if need&bit(i) != 0 {
					p.stats.Targets[i].Copy++
				}
			}
		}
	}
	return nil
}

// touchParent records that path's parent directory changes on target i.
func (p *planner) touchParent(path string, i int) {
	if path == "." {
		return
	}
	par := parent(path)
	for k := len(p.dirs) - 1; k >= 0; k-- {
		if p.dirs[k].e.P == par {
			p.dirs[k].changed |= bit(i)
			return
		}
	}
}

func (p *planner) underSrcUnknown(path string) bool {
	for len(p.srcUnknown) > 0 && !under(path, p.srcUnknown[len(p.srcUnknown)-1]) {
		p.srcUnknown = p.srcUnknown[:len(p.srcUnknown)-1]
	}
	return len(p.srcUnknown) > 0
}

func (p *planner) step(path string) (err error) {
	if err := p.closeDirs(path); err != nil {
		return err
	}
	var s *Entry
	if p.src != nil && p.src.P == path {
		s = p.src
		if p.src, err = next(p.srcRd); err != nil {
			return err
		}
	}
	srcUnknown := p.underSrcUnknown(path)
	if s != nil {
		switch {
		case s.T == TypeError:
			p.stats.problem("source %s unreadable (%s): not copied, nothing deleted there", path, s.E)
			p.srcUnknown = append(p.srcUnknown, path)
			s = nil
			srcUnknown = true
		case s.T == TypeOther:
			p.stats.problem("source %s is a special file: skipped", path)
			s = nil
		default:
			p.stats.SrcEntries++
			p.stats.SrcBytes += s.S
		}
	}

	var copyMask, sentMask uint64
	for i, t := range p.targets {
		var d *Entry
		if p.cur[i] != nil && p.cur[i].P == path {
			d = p.cur[i]
			if p.cur[i], err = next(t.Snap); err != nil {
				return fmt.Errorf("%s: %w", t.Node, err)
			}
		}
		if p.deleted[i] != "" {
			if under(path, p.deleted[i]) {
				continue // inside a directory being removed
			}
			p.deleted[i] = ""
		}
		ts := &p.stats.Targets[i]
		switch {
		case s == nil && d == nil:
			// unreadable/special source entry absent on target
		case s == nil:
			if srcUnknown || !p.opts.Mirror || d.T == TypeError {
				continue
			}
			if err := p.sink.Delete(i, path); err != nil {
				return err
			}
			ts.Delete++
			if d.T == TypeDir {
				p.deleted[i] = path
			}
			p.touchParent(path, i)
		case d == nil:
			copyMask |= bit(i)
			ts.Copy++
			ts.CopyBytes += s.S
			p.touchParent(path, i)
		case d.T == TypeError:
			p.stats.problem("%s: %s unreadable on target (%s)", t.Node, path, d.E)
		case d.T != s.T:
			if err := p.sink.Delete(i, path); err != nil {
				return err
			}
			if d.T == TypeDir {
				p.deleted[i] = path
			}
			copyMask |= bit(i)
			ts.Replace++
			ts.CopyBytes += s.S
			p.touchParent(path, i)
		case p.differs(s, d, t):
			copyMask |= bit(i)
			ts.Copy++
			ts.CopyBytes += s.S
			if s.T != TypeDir {
				p.touchParent(path, i) // installing replaces the entry
			}
		default:
			ts.Unchanged++
		}
		if d != nil && d.Unreadable && s != nil && s.T == TypeDir {
			p.stats.problem("%s: directory %s unreadable on target; its contents were not compared", t.Node, path)
		}
	}
	if s != nil && s.T == TypeDir {
		if s.Unreadable {
			p.stats.problem("source directory %s unreadable (%s): contents not copied, nothing deleted there", path, s.E)
			p.srcUnknown = append(p.srcUnknown, path)
		}
		sentMask = copyMask
		p.dirs = append(p.dirs, openDir{e: s, sent: sentMask})
	}
	if copyMask != 0 {
		return p.sink.Copy(copyMask, s)
	}
	return nil
}

// sec truncates an mtime to whole seconds, the precision compared.
func sec(ns int64) int64 { return ns / 1e9 }

// differs reports whether a same-typed target entry must be re-sent, within
// what the target's filesystem can store.
func (p *planner) differs(s, d *Entry, t Target) bool {
	h := &t.Snap.Header
	modeDiff := !h.NoModes && s.Mo&0o1777 != d.Mo&0o1777 // setuid/setgid depend on target policy
	timeDiff := sec(s.M) != sec(d.M)
	if h.MtimeStep > 1 {
		timeDiff = max(s.M-d.M, d.M-s.M) >= h.MtimeStep*1e9
	}
	switch s.T {
	case TypeFile:
		if s.S != d.S || timeDiff || modeDiff {
			return true
		}
		if p.opts.Checksum && s.H != "" && d.H != "" && s.H != d.H {
			return true
		}
		return t.Chown && (s.U != d.U || s.G != d.G)
	case TypeSymlink:
		return s.L != d.L || (t.Chown && (s.U != d.U || s.G != d.G))
	case TypeDir:
		return modeDiff || timeDiff || (t.ChownDirs && (s.U != d.U || s.G != d.G))
	}
	return false
}
