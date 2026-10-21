package inventory

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Target is one destination snapshot plus what it can preserve, so that
// differences it cannot fix anyway (ownership) do not cause re-copies.
type Target struct {
	Node string
	Snap *Reader
	// Chown: the target preserves file ownership; ChownDirs: directory
	// ownership too.
	Chown, ChownDirs bool
	// Holders are the target's folders (paths inside the base) that hold
	// excluded entries: a mirror deletes what else is in them, never them.
	Holders map[string]bool
}

type PlanOptions struct {
	Mirror   bool
	Checksum bool
	// KeepNewer keeps a target's file or symlink when it is newer than the
	// source's (rsync -u); a type change still applies.
	KeepNewer bool
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
	Kept                                        int64 // newer on the target (KeepNewer)
	// Refused counts entries the target's filesystem cannot store (see
	// api.NameRules), left out of the plan; what is below a refused
	// folder is not counted.
	Refused int64
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
	// names: per target with folding name rules, the names this folder
	// will hold there, by api.NameRules.Key.
	names []map[string]string
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
	// refusedDir: per target, the top-most folder it cannot store; refusals
	// keeps a few examples per target.
	refusedDir []string
	refusals   [][]string
}

// Plan merges the source snapshot with every target snapshot in one pass.
func Plan(src *Reader, targets []Target, opts PlanOptions, sink Sink) (*PlanStats, error) {
	if len(targets) == 0 || len(targets) > 64 {
		return nil, fmt.Errorf("plan: need 1 to 64 targets, got %d", len(targets))
	}
	p := &planner{
		targets: targets, opts: opts, sink: sink,
		stats:      &PlanStats{Targets: make([]TargetStats, len(targets))},
		cur:        make([]*Entry, len(targets)),
		srcRd:      src,
		deleted:    make([]string, len(targets)),
		refusedDir: make([]string, len(targets)),
		refusals:   make([][]string, len(targets)),
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
	var lines []string
	for i, t := range targets {
		if n := p.stats.Targets[i].Refused; n > 0 {
			lines = append(lines, fmt.Sprintf("%s: %d entries cannot be stored on its %s and are left out, e.g. %s",
				t.Node, n, t.Snap.Header.Names.FS, strings.Join(p.refusals[i], "; ")))
		}
	}
	p.stats.Problems = append(lines, p.stats.Problems...)
	return p.stats, nil
}

// cannotStore says why target i cannot hold source entry path, which it
// does not have yet: a name its filesystem refuses, or one it would take
// for a name already there (README and readme on FAT).
func (p *planner) cannotStore(i int, path string, s *Entry) string {
	r := p.targets[i].Snap.Header.Names
	if r == nil || path == "." {
		return ""
	}
	if r.NoSymlinks && s.T == TypeSymlink {
		return "no symlinks on " + r.FS
	}
	base := path[strings.LastIndexByte(path, '/')+1:]
	if why := r.Refuses(base); why != "" {
		return why
	}
	if m := p.namesIn(i, parent(path), false); m != nil {
		if other, ok := m[r.Key(base)]; ok && other != base {
			return fmt.Sprintf("the same name as %q there", other)
		}
	}
	return ""
}

// remember records that target i will hold path (see cannotStore).
func (p *planner) remember(i int, path string) {
	r := p.targets[i].Snap.Header.Names
	if r == nil || !r.Fold || path == "." {
		return
	}
	if m := p.namesIn(i, parent(path), true); m != nil {
		base := path[strings.LastIndexByte(path, '/')+1:]
		if _, ok := m[r.Key(base)]; !ok {
			m[r.Key(base)] = base
		}
	}
}

// namesIn returns target i's name set of source folder dir, if it is the
// folder being merged.
func (p *planner) namesIn(i int, dir string, create bool) map[string]string {
	if len(p.dirs) == 0 {
		return nil
	}
	top := &p.dirs[len(p.dirs)-1]
	if top.e.P != dir {
		return nil
	}
	if top.names == nil {
		if !create {
			return nil
		}
		top.names = make([]map[string]string, len(p.targets))
	}
	if top.names[i] == nil && create {
		top.names[i] = map[string]string{}
	}
	return top.names[i]
}

func (p *planner) refuse(i int, path string, s *Entry, why string) {
	p.stats.Targets[i].Refused++
	if len(p.refusals[i]) < 5 {
		p.refusals[i] = append(p.refusals[i], fmt.Sprintf("%q (%s)", path, why))
	}
	if s.T == TypeDir {
		p.refusedDir[i] = path
	}
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
		if p.refusedDir[i] != "" {
			if under(path, p.refusedDir[i]) {
				continue // inside a folder the target cannot store
			}
			p.refusedDir[i] = ""
		}
		why := ""
		if s != nil && d == nil {
			why = p.cannotStore(i, path, s)
		}
		// What the folder will hold on the target: the source's entry, or
		// a target entry the plan leaves there.
		if s != nil && why == "" || d != nil && (s != nil || !p.opts.Mirror || srcUnknown || d.T == TypeError || d.T == TypeDir && t.Holders[path]) {
			p.remember(i, path)
		}
		ts := &p.stats.Targets[i]
		switch {
		case why != "":
			p.refuse(i, path, s, why)
			continue
		case s == nil && d == nil:
			// unreadable/special source entry absent on target
		case s == nil:
			if srcUnknown || !p.opts.Mirror || d.T == TypeError || d.T == TypeDir && t.Holders[path] {
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
		case d.T != s.T && d.T == TypeDir && t.Holders[path]:
			p.stats.problem("%s: %s stays a folder: it holds excluded entries", t.Node, path)
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
		case p.opts.KeepNewer && s.T != TypeDir && newerThere(s, d, t) && p.differs(s, d, t):
			ts.Kept++
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

// newerThere reports whether target entry d is newer than source entry s,
// by more than what the target's filesystem can tell apart.
func newerThere(s, d *Entry, t Target) bool {
	sm := t.Snap.Header.storable(s.M)
	if step := t.Snap.Header.MtimeStep; step > 1 {
		return d.M-sm >= step*1e9
	}
	return sec(d.M) > sec(sm)
}

// sec truncates an mtime to whole seconds, the precision compared.
func sec(ns int64) int64 { return ns / 1e9 }

// differs reports whether a same-typed target entry must be re-sent, within
// what the target's filesystem can store.
func (p *planner) differs(s, d *Entry, t Target) bool {
	h := &t.Snap.Header
	modeDiff := !h.NoModes && s.Mo&0o1777 != d.Mo&0o1777 // setuid/setgid depend on target policy
	sm := h.storable(s.M)                                // what the target can hold at best
	timeDiff := sec(sm) != sec(d.M)
	if h.MtimeStep > 1 {
		timeDiff = max(sm-d.M, d.M-sm) >= h.MtimeStep*1e9
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
