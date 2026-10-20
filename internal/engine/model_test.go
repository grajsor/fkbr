package engine

import (
	"context"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/exclude"
)

// The sync model test: random trees on both sides, random options, and an
// independent model of what the target must look like afterwards.

type mEntry struct {
	dir     bool
	content string
	mtime   time.Time
}

type mTree map[string]mEntry // slash path inside the synced folder -> entry

var mNames = []string{"a", "b", "c.tmp", "d.log", "cache", "sub", "x", "top.log"}
var mTimes = []time.Duration{0, 10 * time.Second, 100 * time.Second}

func mGen(rng *rand.Rand, t mTree, dir string, depth int) {
	for _, n := range mNames {
		if rng.Intn(3) == 0 {
			continue
		}
		p := path.Join(dir, n)
		if depth < 2 && rng.Intn(3) == 0 {
			t[p] = mEntry{dir: true}
			mGen(rng, t, p, depth+1)
			continue
		}
		t[p] = mEntry{content: fmt.Sprintf("%s-%d", n, rng.Intn(3)), mtime: mT0.Add(mTimes[rng.Intn(len(mTimes))])}
	}
}

var mT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// mTarget derives a target from the source: shared entries, changed ones
// (content, time, type) and entries only the target has.
func mTarget(rng *rand.Rand, src mTree) mTree {
	t := mTree{}
	for _, p := range mSorted(src) { // sorted: a seed gives the same trees every run
		e := src[p]
		if !mParentIsDir(t, p) {
			continue
		}
		switch r := rng.Intn(10); {
		case r < 3: // missing on the target
		case r < 6:
			t[p] = e
		case r < 8 && !e.dir: // other content (maybe the same length), some time
			t[p] = mEntry{content: e.content[:len(e.content)-1] + fmt.Sprint(rng.Intn(10)), mtime: mT0.Add(mTimes[rng.Intn(len(mTimes))])}
		case r < 9 && !e.dir: // a folder where the source has a file
			t[p] = mEntry{dir: true}
			t[path.Join(p, "inner")] = mEntry{content: "inner", mtime: mT0}
		default:
			if e.dir {
				t[p] = e
			} else {
				t[p] = mEntry{content: e.content, mtime: e.mtime}
			}
		}
	}
	extra := mTree{}
	mGen(rng, extra, "", 0)
	for _, p := range mSorted(extra) { // entries only the target has
		e := extra[p]
		if _, ok := src[p]; !ok && mParentIsDir(t, p) {
			if _, ok := t[p]; !ok {
				t[p] = e
			}
		}
	}
	// Drop what lost its parent folder.
	for changed := true; changed; {
		changed = false
		for p := range t {
			if !mParentIsDir(t, p) {
				delete(t, p)
				changed = true
			}
		}
	}
	return t
}

func mSorted(t mTree) []string {
	ps := make([]string, 0, len(t))
	for p := range t {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return ps
}

func mParentIsDir(t mTree, p string) bool {
	d := path.Dir(p)
	if d == "." {
		return true
	}
	e, ok := t[d]
	return ok && e.dir
}

// mExcluded applies the exclude rule: p or a folder above it matches.
func mExcluded(ex *exclude.Set, t mTree, p string) bool {
	for q := p; q != "."; q = path.Dir(q) {
		if ex.Match(q, t[q].dir) {
			return true
		}
	}
	return false
}

// mHolds reports whether something excluded lies below p.
func mHolds(ex *exclude.Set, t mTree, p string) bool {
	for q := range t {
		if strings.HasPrefix(q, p+"/") && mExcluded(ex, t, q) {
			return true
		}
	}
	return false
}

func mWrite(t *testing.T, top string, tr mTree) {
	t.Helper()
	ps := make([]string, 0, len(tr))
	for p := range tr {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	for _, p := range ps {
		full := filepath.Join(top, p)
		if tr[p].dir {
			os.MkdirAll(full, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(tr[p].content), 0o644)
		os.Chtimes(full, tr[p].mtime, tr[p].mtime)
	}
}

func mRead(t *testing.T, top string) mTree {
	t.Helper()
	tr := mTree{}
	filepath.WalkDir(top, func(full string, d fs.DirEntry, err error) error {
		if err != nil || full == top {
			return nil
		}
		rel, _ := filepath.Rel(top, full)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".fkbr-tmp") {
			return fs.SkipDir
		}
		if d.IsDir() {
			tr[rel] = mEntry{dir: true}
			return nil
		}
		b, _ := os.ReadFile(full)
		fi, _ := d.Info()
		tr[rel] = mEntry{content: string(b), mtime: fi.ModTime()}
		return nil
	})
	return tr
}

// mExpect is the model: what the target holds after the sync.
func mExpect(src, dst mTree, ex *exclude.Set, mirror, keepNewer bool) mTree {
	out := mTree{}
	for p, e := range dst {
		out[p] = e
	}
	// Sources first, top-down.
	ps := make([]string, 0, len(src))
	for p := range src {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	for _, p := range ps {
		s := src[p]
		if mExcluded(ex, src, p) {
			continue
		}
		d, there := out[p]
		switch {
		case !there:
		case d.dir && s.dir:
			continue
		case !d.dir && !s.dir:
			same := len(d.content) == len(s.content) && d.mtime.Unix() == s.mtime.Unix() // the quick check
			if same || keepNewer && d.mtime.Unix() > s.mtime.Unix() {
				continue
			}
		case d.dir && mHolds(ex, dst, p):
			continue // a folder holding excluded entries stays (a plan problem)
		default: // type change: what was there goes, with everything below
			for q := range out {
				if q == p || strings.HasPrefix(q, p+"/") {
					delete(out, q)
				}
			}
		}
		out[p] = s
	}
	if mirror {
		for p := range dst {
			if _, ok := src[p]; ok || mExcluded(ex, dst, p) {
				continue
			}
			if _, ok := out[p]; !ok {
				continue // already replaced
			}
			if !mHolds(ex, dst, p) { // kept only as the folder of something excluded
				delete(out, p)
			}
		}
	}
	return out
}

func TestSyncModel(t *testing.T) {
	share := t.TempDir()
	a, aroot := node(t, share, "a")
	b, broot := node(t, share, "b")
	runWorker(t, a)
	runWorker(t, b)
	patterns := [][]string{nil, {"*.tmp"}, {"*.tmp", "cache"}, {"sub/x", "/top.log"}, {"*.log", "x"}}
	rounds := 40 // FKBR_MODEL_ROUNDS=400 for a longer search
	if n, err := strconv.Atoi(os.Getenv("FKBR_MODEL_ROUNDS")); err == nil {
		rounds = n
	} else if testing.Short() {
		rounds = 8
	}
	skipped := 0
	defer func() { t.Logf("%d of %d rounds run", rounds-skipped, rounds) }()
	for round := range rounds {
		seed := int64(round*7919 + 1)
		rng := rand.New(rand.NewSource(seed))
		src := mTree{}
		mGen(rng, src, "", 0)
		dst := mTarget(rng, src)
		pats := patterns[rng.Intn(len(patterns))]
		ex, _ := exclude.New(pats)
		mirror, keepNewer := rng.Intn(2) == 0, rng.Intn(2) == 0
		sp, dp := fmt.Sprintf("s%d", round), fmt.Sprintf("d%d", round)
		mWrite(t, filepath.Join(aroot, sp), src)
		os.MkdirAll(filepath.Join(aroot, sp), 0o755)
		mWrite(t, filepath.Join(broot, dp), dst)
		os.MkdirAll(filepath.Join(broot, dp), 0o755)
		mode := api.SyncIncremental
		if mirror {
			mode = api.SyncMirror
		}
		req := api.SyncRequest{SrcRoot: "data", SrcPath: sp, DstNodes: []string{"b"}, DstRoot: "data", DstPath: dp,
			Mode: mode, Exclude: pats, KeepNewer: keepNewer, MaxDeletes: -1}
		what := fmt.Sprintf("round %d (seed %d, mirror %v, keep newer %v, exclude %q)", round, seed, mirror, keepNewer, pats)
		plan := mustPlan(t, a, req)
		if planWork(plan) > 0 {
			mApply(t, a, plan, what)
		}
		want := mExpect(src, dst, ex, mirror, keepNewer)
		got := mRead(t, filepath.Join(broot, dp))
		var diffs []string
		for p := range want {
			w, g := want[p], got[p]
			if _, ok := got[p]; !ok {
				diffs = append(diffs, p+": missing")
			} else if w.dir != g.dir || w.content != g.content {
				diffs = append(diffs, fmt.Sprintf("%s: got dir=%v %q, want dir=%v %q", p, g.dir, g.content, w.dir, w.content))
			}
		}
		for p := range got {
			if _, ok := want[p]; !ok {
				diffs = append(diffs, p+": should not be there")
			}
		}
		sort.Strings(diffs)
		if len(diffs) > 0 {
			t.Errorf("%s:\n  %s", what, strings.Join(diffs, "\n  "))
		}
		if gotSrc := mRead(t, filepath.Join(aroot, sp)); len(gotSrc) != len(src) {
			t.Errorf("%s: the source changed (%d entries, was %d)", what, len(gotSrc), len(src))
		}
		// And a second plan finds nothing more to do (but what the quick
		// check or keep newer leave alone on purpose).
		if again := mustPlan(t, a, req); planWork(again) > 0 && !keepNewer {
			t.Errorf("%s: a second sync still has work: %+v", what, again.Targets)
		}
	}
}

// planWork is what a plan would change (copies, replacements, deletions).
func planWork(p *api.SyncPlan) int64 {
	var n int64
	for _, t := range p.Targets {
		n += t.Copy + t.Replace + t.Delete
	}
	return n
}

// mApply applies a plan; the one expected problem is a folder that holds
// excluded entries where the source has a file.
func mApply(t *testing.T, e *Engine, plan *api.SyncPlan, what string) {
	t.Helper()
	p, err := e.ApplySync(context.Background(), plan.ID, true)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for _, pr := range p.Problems {
		if !strings.Contains(pr, "stays a folder: it holds excluded entries") {
			t.Errorf("%s: problem %s", what, pr)
		}
	}
	if len(p.DeleteErrors) > 0 {
		t.Errorf("%s: delete errors %v", what, p.DeleteErrors)
	}
	for _, id := range p.Jobs {
		if j := waitJob(t, e, id, 30*time.Second); j.State != api.JobDone {
			t.Errorf("%s: job %s %+v", what, j.State, j.Participants)
		}
	}
}
