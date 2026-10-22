package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/exclude"
	"fkbr/internal/id"
	"fkbr/internal/inventory"
	"fkbr/internal/localfs"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
)

const (
	// scanIdleFactor: a remote scan fails if no progress arrives for this
	// many request timeouts (progress is sent every scanProgressInterval).
	scanIdleFactor       = 4
	scanProgressInterval = 2 * time.Second
	removeBatch          = 1000
)

type scanRequest struct {
	PlanID   string   `json:"plan_id"`
	Root     string   `json:"root"`
	Path     string   `json:"path"`
	Checksum bool     `json:"checksum"`
	Exclude  []string `json:"exclude,omitempty"`
	// Target: the scan is of a destination, which may probe what its
	// filesystem can store (it writes a temporary file).
	Target bool `json:"target,omitempty"`
}

func snapshotBlob(planID, node string) string { return "scans/" + planID + "/" + node + ".snap" }

// holdersBlob lists the folders of a scan that hold excluded entries.
func holdersBlob(planID, node string) string { return "scans/" + planID + "/" + node + ".holders" }
func planBlob(planID string) string          { return "plans/" + planID + "/plan.json" }

// scanLocal snapshots root/path into the share.
func (e *Engine) scanLocal(ctx context.Context, req scanRequest, progress func(inventory.ScanStats)) (*inventory.ScanStats, error) {
	w, err := e.st.CreateBlob(ctx, snapshotBlob(req.PlanID, e.Self()))
	if err != nil {
		return nil, err
	}
	defer w.Abort()
	rel, err := localfs.CleanRel(req.Path)
	if err != nil {
		return nil, err
	}
	h := inventory.Header{Node: e.Self(), Root: req.Root, Path: req.Path}
	for _, r := range e.fs.Roots() {
		if r.ID == req.Root {
			// The base, not the root: a USB stick may be mounted below it.
			// The probes below write there, so it must really be inside
			// the root, not reached through a symlink.
			base := insideRoot(r.Path, rel)
			h.MtimeStep, h.NoModes = localfs.Limits(base)
			if req.Target && r.Writable {
				h.MtimeMin, h.MtimeMax = localfs.MtimeRange(base)
			}
			if n := localfs.Names(base); req.Target && n != (api.NameRules{}) {
				h.Names = &n
			}
		}
	}
	ex, err := exclude.New(req.Exclude)
	if err != nil {
		return nil, err
	}
	if h.Names != nil && h.Names.Fold {
		ex = ex.WithKey(h.Names.Key) // what is excluded here, whatever its case
	}
	// The folders above what is left out: a mirror must not delete them
	// whole (they would take the excluded entries along).
	holders := map[string]bool{}
	skip := func(rel string, dir bool) bool {
		if !ex.Match(rel, dir) {
			return false
		}
		for q := path.Dir(rel); q != "."; q = path.Dir(q) {
			holders[q] = true
		}
		return true
	}
	st, err := inventory.Scan(e.fs, req.Root, req.Path, h, inventory.ScanOptions{
		Checksum: req.Checksum, Exclude: receive.StagingDir, Skip: skip,
		Progress: progress, Interval: scanProgressInterval,
	}, w)
	if err != nil {
		return nil, err
	}
	if len(holders) > 0 {
		hw, err := e.st.CreateBlob(ctx, holdersBlob(req.PlanID, e.Self()))
		if err != nil {
			return nil, err
		}
		defer hw.Abort()
		bw := bufio.NewWriter(hw)
		for q := range holders {
			fmt.Fprintln(bw, strconv.Quote(q))
		}
		if err := bw.Flush(); err != nil {
			return nil, err
		}
		if err := hw.Commit(nil); err != nil {
			return nil, err
		}
	}
	return st, w.Commit(nil)
}

// insideRoot is root/rel with symlinks resolved, or root itself when that
// does not exist or lies outside root.
func insideRoot(root, rel string) string {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return root
	}
	if r, err := filepath.Rel(realRoot, p); err != nil || r == ".." || strings.HasPrefix(r, "../") {
		return root
	}
	return p
}

// readHolders reads a scan's holders list (none if it has none).
func (e *Engine) readHolders(ctx context.Context, planID, node string) (map[string]bool, error) {
	rc, err := e.st.OpenBlob(ctx, holdersBlob(planID, node))
	if err != nil {
		return nil, nil // nothing excluded there
	}
	defer rc.Close()
	keep := map[string]bool{}
	return keep, readPathList(rc, func(p string) error { keep[p] = true; return nil })
}

// serveScan runs a peer's scan request, reporting progress as reply parts.
func (e *Engine) serveScan(ctx context.Context, m *transport.Message) error {
	var req scanRequest
	if err := json.Unmarshal(m.Body, &req); err != nil {
		return err
	}
	req.Path = api.DecodeName(req.Path)
	seq := 0
	reply := func(st inventory.ScanStats, final bool) error {
		b, _ := json.Marshal(st)
		r := &transport.Reply{Seq: seq, Final: final, Body: b}
		seq++
		return e.tr.Reply(ctx, m, r)
	}
	start := time.Now()
	st, err := e.scanLocal(ctx, req, func(st inventory.ScanStats) { reply(st, false) })
	if err != nil {
		return err
	}
	e.log.Info("scanned for a sync", "plan", req.PlanID, "for", m.From, "path", req.Root+"/"+req.Path,
		"entries", st.Entries, "bytes", size(st.Bytes), "took", time.Since(start).Round(time.Millisecond))
	return reply(*st, true)
}

// scan snapshots a directory on any node, broadcasting progress events.
func (e *Engine) scan(ctx context.Context, node string, req scanRequest) (*inventory.ScanStats, error) {
	report := func(st inventory.ScanStats, done bool) {
		e.broadcast(api.Event{Type: api.EventScan, Time: time.Now().UTC(), Scan: &api.ScanProgress{
			PlanID: req.PlanID, Node: node, Entries: st.Entries, Bytes: st.Bytes, Done: done, Dir: st.Dir}})
	}
	if node == e.Self() {
		st, err := e.scanLocal(ctx, req, func(st inventory.ScanStats) { report(st, false) })
		if err == nil {
			report(*st, true)
		}
		return st, err
	}
	wire := req
	wire.Path = api.EncodeName(req.Path)
	msg, err := e.newMessage(node, transport.TypeScan, wire)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := e.cfg.RequestTimeout * scanIdleFactor
	var stalled atomic.Bool
	timer := time.AfterFunc(idle, func() { stalled.Store(true); cancel() })
	defer timer.Stop()
	if err := e.tr.Send(ctx, msg); err != nil {
		return nil, err
	}
	var final inventory.ScanStats
	err = e.tr.AwaitReplies(ctx, msg.ID, func(r *transport.Reply) error {
		timer.Reset(idle)
		var st inventory.ScanStats
		if err := json.Unmarshal(r.Body, &st); err != nil {
			return err
		}
		report(st, r.Final)
		final = st
		return nil
	})
	if stalled.Load() {
		return nil, fmt.Errorf("scan on %s: no progress for %s (is its worker running?)", node, idle)
	}
	if err != nil {
		return nil, err
	}
	return &final, nil
}

// PlanSync scans source and targets and compares them.
func (e *Engine) PlanSync(ctx context.Context, req api.SyncRequest) (*api.SyncPlan, error) {
	if req.SrcNode == "" {
		req.SrcNode = e.Self()
	}
	if req.Mode == "" {
		req.Mode = api.SyncIncremental
	}
	if req.Mode != api.SyncIncremental && req.Mode != api.SyncMirror {
		return nil, fmt.Errorf("invalid sync mode %q", req.Mode)
	}
	var err error
	if req.SrcPath, err = localfs.CleanRel(req.SrcPath); err != nil {
		return nil, err
	}
	if req.DstPath, err = localfs.CleanRel(req.DstPath); err != nil {
		return nil, err
	}
	if _, err := exclude.New(req.Exclude); err != nil {
		return nil, err
	}
	nodes, err := e.tr.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]api.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	src, err := onlineNode(byID, req.SrcNode)
	if err != nil {
		return nil, err
	}
	if _, ok := findRoot(src, req.SrcRoot); !ok {
		return nil, fmt.Errorf("%w: %s:%s", api.ErrUnknownRoot, req.SrcNode, req.SrcRoot)
	}
	targets, offline, err := expandTargets(byID, req.DstNodes, req.SrcNode, req.DetachOffline)
	if err != nil {
		return nil, err
	}
	if err := e.checkTargets(byID, targets, req.DstRoot); err != nil {
		return nil, err
	}
	if len(req.Exclude) > 0 { // every side scans with them
		scanned := []api.Node{src}
		for _, t := range targets {
			scanned = append(scanned, byID[t])
		}
		if err := needFeature(api.FeatureExclude, "excludes", scanned...); err != nil {
			return nil, err
		}
	}
	for _, t := range targets {
		if t == req.SrcNode && req.DstRoot == req.SrcRoot && overlaps(req.SrcPath, req.DstPath) {
			return nil, fmt.Errorf("%w: source and destination overlap on %s:%s", api.ErrBadPath, t, req.SrcRoot)
		}
	}
	maxDel := req.MaxDeletes
	if maxDel == 0 {
		maxDel = e.cfg.Sync.MaxDeletes
	}

	plan := &api.SyncPlan{
		ID: id.At(e.now()), Created: e.now(), Initiator: e.Self(), Request: req,
		MaxDeletes: maxDel, Skipped: offline,
	}
	// Scan everything in parallel.
	nodesToScan := append([]string{req.SrcNode}, targets...)
	stats := make([]*inventory.ScanStats, len(nodesToScan))
	errs := make([]error, len(nodesToScan))
	var wg sync.WaitGroup
	for i, n := range nodesToScan {
		root, path := req.DstRoot, req.DstPath
		if i == 0 {
			root, path = req.SrcRoot, req.SrcPath
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats[i], errs[i] = e.scan(ctx, n, scanRequest{PlanID: plan.ID, Root: root, Path: path, Checksum: req.Checksum, Exclude: req.Exclude, Target: i > 0})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("scan on %s: %w", nodesToScan[i], err)
		}
	}
	if stats[0].Missing {
		return nil, fmt.Errorf("%w: source %s:%s/%s does not exist", api.ErrBadPath, req.SrcNode, req.SrcRoot, req.SrcPath)
	}

	open := func(node string) (*inventory.Reader, io.Closer, error) {
		rc, err := e.st.OpenBlob(ctx, snapshotBlob(plan.ID, node))
		if err != nil {
			return nil, nil, err
		}
		r, err := inventory.NewReader(bufio.NewReaderSize(rc, 256<<10))
		if err != nil {
			rc.Close()
			return nil, nil, err
		}
		return r, rc, nil
	}
	srcRd, c, err := open(req.SrcNode)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	defer srcRd.Close()
	var its []inventory.Target
	for _, t := range targets {
		r, c, err := open(t)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		defer r.Close()
		n := byID[t]
		holders, err := e.readHolders(ctx, plan.ID, t)
		if err != nil {
			return nil, err
		}
		its = append(its, inventory.Target{Node: t, Snap: r, Chown: n.ChownCapable, ChownDirs: n.ChownCapable && n.DACOverride, Holders: holders})
	}
	sink := &planSink{e: e, ctx: ctx, plan: plan, targets: targets, groups: map[uint64]*listWriter{}, deletes: map[int]*listWriter{}}
	ps, err := inventory.Plan(srcRd, its, inventory.PlanOptions{Mirror: req.Mode == api.SyncMirror, Checksum: req.Checksum, KeepNewer: req.KeepNewer}, sink)
	if err != nil {
		sink.abort()
		return nil, err
	}
	if err := sink.commit(); err != nil {
		return nil, err
	}
	plan.SrcEntries, plan.SrcBytes, plan.Problems = ps.SrcEntries, ps.SrcBytes, ps.Problems
	var todo []string
	for i, t := range targets {
		ts := ps.Targets[i]
		plan.Targets = append(plan.Targets, api.SyncTarget{
			Node: t, Copy: ts.Copy, CopyBytes: ts.CopyBytes, Replace: ts.Replace,
			Delete: ts.Delete, Unchanged: ts.Unchanged, Kept: ts.Kept, Refused: ts.Refused,
			OverLimit: maxDel >= 0 && ts.Delete+ts.Replace > int64(maxDel),
		})
		todo = append(todo, fmt.Sprintf("%s:copy=%d,delete=%d", t, ts.Copy, ts.Delete))
	}
	e.log.Info("sync planned", "plan", plan.ID, "mode", req.Mode, "source", req.SrcNode+":"+req.SrcRoot+"/"+req.SrcPath,
		"entries", ps.SrcEntries, "targets", summary(todo, 4), "problems", len(ps.Problems))
	return plan, e.savePlan(ctx, plan)
}

// checkTargets validates destination root and write policy for targets.
func (e *Engine) checkTargets(byID map[string]api.Node, targets []string, root string) error {
	initiator := e.info
	if n, ok := byID[e.Self()]; ok {
		initiator = n.NodeInfo
	}
	for _, d := range targets {
		n := byID[d]
		r, ok := findRoot(n, root)
		if !ok {
			return fmt.Errorf("%w: %s:%s", api.ErrUnknownRoot, d, root)
		}
		if !r.Writable {
			return fmt.Errorf("%w: %s:%s", api.ErrReadOnly, d, root)
		}
		if !n.AcceptsWritesFrom(&initiator) {
			return fmt.Errorf("%w: %s only accepts writes from %v", api.ErrNotAllowed, d, n.AcceptWritesFrom)
		}
	}
	return nil
}

func overlaps(a, b string) bool {
	return a == b || a == "." || b == "." || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func (e *Engine) savePlan(ctx context.Context, plan *api.SyncPlan) error {
	w, err := e.st.CreateBlob(ctx, planBlob(plan.ID))
	if err != nil {
		return err
	}
	defer w.Abort()
	if err := json.NewEncoder(w).Encode(plan); err != nil {
		return err
	}
	return w.Commit(nil)
}

func (e *Engine) SyncPlan(ctx context.Context, planID string) (*api.SyncPlan, error) {
	rc, err := e.st.OpenBlob(ctx, planBlob(planID))
	if err != nil {
		return nil, fmt.Errorf("%w: sync plan %s", api.ErrUnknownJob, planID)
	}
	defer rc.Close()
	var p api.SyncPlan
	return &p, json.NewDecoder(rc).Decode(&p)
}

// ApplySync deletes first (mirror removals and type changes), then submits
// one transfer per group of targets needing the same entries.
func (e *Engine) ApplySync(ctx context.Context, planID string, force bool) (*api.SyncPlan, error) {
	plan, err := e.SyncPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	if plan.Applied {
		return nil, fmt.Errorf("sync plan %s was already applied", planID)
	}
	if !force {
		for _, t := range plan.Targets {
			if t.OverLimit {
				return nil, fmt.Errorf("%w: plan removes %d entries on %s, more than the limit of %d; apply with force to proceed",
					api.ErrNotAllowed, t.Delete+t.Replace, t.Node, plan.MaxDeletes)
			}
		}
	}
	req := plan.Request
	plan.Applied = true // even partially: a plan is never applied twice
	for _, t := range plan.Targets {
		if t.Delete+t.Replace == 0 {
			continue
		}
		errs := e.applyDeletes(ctx, plan.ID, t.Node, req.DstRoot, req.Exclude, req.DstPath)
		plan.DeleteErrors = append(plan.DeleteErrors, errs...)
	}
	for _, g := range plan.Groups {
		jid, err := e.Submit(ctx, api.TransferRequest{
			SrcNode: req.SrcNode, SrcRoot: req.SrcRoot,
			PathsBlob: g.Blob, PathsCount: g.Entries, BaseDir: req.SrcPath, NoRecurse: true,
			DstNodes: g.Targets, DstRoot: req.DstRoot, DstDir: req.DstPath,
			Conflict: api.ConflictOverwrite, Compress: req.Compress, SyncPlan: plan.ID,
		})
		if err != nil {
			plan.Problems = append(plan.Problems, fmt.Sprintf("transfer to %s: %v", strings.Join(g.Targets, ","), err))
			continue
		}
		plan.Jobs = append(plan.Jobs, jid)
	}
	e.log.Info("sync applied", "plan", plan.ID, "jobs", summary(plan.Jobs, 4), "delete_errors", len(plan.DeleteErrors),
		"problems", len(plan.Problems))
	return plan, e.savePlan(ctx, plan)
}

// applyDeletes removes a plan's deletions on node; what the sync excludes
// (patterns relative to base) stays, with the folders holding it.
func (e *Engine) applyDeletes(ctx context.Context, planID, node, root string, excl []string, base string) []string {
	var errs []string
	fail := func(err error) []string { return append(errs, fmt.Sprintf("%s: %v", node, err)) }
	rc, err := e.st.OpenBlob(ctx, "plans/"+planID+"/delete-"+node)
	if err != nil {
		return fail(err)
	}
	defer rc.Close()
	var batch []string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		res, err := e.Remove(ctx, api.RemoveRequest{Node: node, Root: root, Paths: batch, Exclude: excl, Base: base})
		if err != nil {
			errs = fail(err)
		} else {
			for _, m := range res.Errors {
				if len(errs) < 100 {
					errs = append(errs, node+": "+m)
				}
			}
		}
		batch = batch[:0]
	}
	err = readPathList(rc, func(p string) error {
		batch = append(batch, p)
		if len(batch) == removeBatch {
			flush()
		}
		return nil
	})
	flush()
	if err != nil {
		errs = fail(err)
	}
	return errs
}

// Path lists hold one JSON-quoted path per line, so any file name survives.

func readPathList(r io.Reader, fn func(string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		p, err := strconv.Unquote(sc.Text())
		if err != nil {
			return fmt.Errorf("path list: %w", err)
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return sc.Err()
}

type listWriter struct {
	w       transport.ChunkWriter
	buf     *bufio.Writer
	blob    string
	mask    uint64
	entries int64
	bytes   int64
	// dirs are the directories in this list; parents those its entries are
	// installed into (see planSink.commit).
	dirs, parents map[string]bool
}

func (l *listWriter) add(p string) error {
	l.entries++
	_, err := l.buf.WriteString(strconv.Quote(p) + "\n")
	return err
}

// planSink writes the plan's path lists to the share as they are found.
type planSink struct {
	e       *Engine
	ctx     context.Context
	plan    *api.SyncPlan
	targets []string
	groups  map[uint64]*listWriter
	deletes map[int]*listWriter
}

func (s *planSink) open(name string) (*listWriter, error) {
	blob := "plans/" + s.plan.ID + "/" + name
	w, err := s.e.st.CreateBlob(s.ctx, blob)
	if err != nil {
		return nil, err
	}
	return &listWriter{w: w, buf: bufio.NewWriterSize(w, 64<<10), blob: blob}, nil
}

func (s *planSink) Copy(mask uint64, ent *inventory.Entry) error {
	g := s.groups[mask]
	if g == nil {
		var err error
		if g, err = s.open(fmt.Sprintf("copy-%x", mask)); err != nil {
			return err
		}
		g.mask = mask
		g.dirs, g.parents = map[string]bool{}, map[string]bool{}
		s.groups[mask] = g
	}
	g.bytes += ent.S
	if ent.T == inventory.TypeDir {
		g.dirs[ent.P] = true
	}
	if ent.P != "." {
		g.parents[path.Dir(ent.P)] = true
	}
	return g.add(inventory.Join(s.plan.Request.SrcPath, ent.P))
}

func (s *planSink) Delete(i int, p string) error {
	d := s.deletes[i]
	if d == nil {
		var err error
		if d, err = s.open("delete-" + s.targets[i]); err != nil {
			return err
		}
		s.deletes[i] = d
	}
	return d.add(inventory.Join(s.plan.Request.DstPath, p))
}

func (s *planSink) commit() error {
	// A target that needs entries of several target sets gets one job per
	// set, and the jobs install into the same folders at the same time.
	// A folder's mtime is set after a job's own installs, so every job that
	// installs into a folder also carries that folder: whichever finishes
	// last leaves the source's mtime (else the next sync is not empty).
	groupsOf := make([]int, len(s.targets))
	for m := range s.groups {
		for i := range s.targets {
			if m&(1<<uint(i)) != 0 {
				groupsOf[i]++
			}
		}
	}
	var errs []error
	for m, g := range s.groups {
		shared := false
		for i := range s.targets {
			shared = shared || m&(1<<uint(i)) != 0 && groupsOf[i] > 1
		}
		if !shared {
			continue
		}
		// Up to the top: a job may create a missing folder on the way (in
		// staging), which changes the folder above it too.
		var add []string
		for p := range g.parents {
			for ; !g.dirs[p]; p = path.Dir(p) {
				g.dirs[p] = true
				add = append(add, p)
				if p == "." {
					break
				}
			}
		}
		slices.Sort(add)
		for _, p := range add {
			errs = append(errs, g.add(inventory.Join(s.plan.Request.SrcPath, p)))
		}
	}
	for _, l := range s.all() {
		if err := l.buf.Flush(); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, l.w.Commit(nil))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	masks := make([]uint64, 0, len(s.groups))
	for m := range s.groups {
		masks = append(masks, m)
	}
	slices.Sort(masks)
	for _, m := range masks {
		g := s.groups[m]
		var ts []string
		for i, t := range s.targets {
			if m&(1<<uint(i)) != 0 {
				ts = append(ts, t)
			}
		}
		s.plan.Groups = append(s.plan.Groups, api.SyncGroup{Targets: ts, Entries: g.entries, Bytes: g.bytes, Blob: g.blob})
	}
	return nil
}

func (s *planSink) abort() {
	for _, l := range s.all() {
		l.w.Abort()
	}
}

func (s *planSink) all() []*listWriter {
	var out []*listWriter
	for _, g := range s.groups {
		out = append(out, g)
	}
	for _, d := range s.deletes {
		out = append(out, d)
	}
	return out
}

// SyncPlanEntries reads a plan's path lists back from the share.
func (e *Engine) SyncPlanEntries(ctx context.Context, planID string, limit int) (*api.SyncEntries, error) {
	plan, err := e.SyncPlan(ctx, planID)
	if err != nil {
		return nil, err
	}
	read := func(blob, base string, l *api.SyncEntryList) error {
		rc, err := e.st.OpenBlob(ctx, blob)
		if err != nil {
			return err
		}
		defer rc.Close()
		return readPathList(rc, func(p string) error {
			if limit > 0 && len(l.Paths) >= limit {
				l.More++
				return nil
			}
			rel := inventoryRel(base, p)
			if rel == "." {
				rel = "./" // the synced folder itself (its time or mode)
			}
			l.Paths = append(l.Paths, rel)
			return nil
		})
	}
	out := &api.SyncEntries{}
	for _, g := range plan.Groups {
		l := api.SyncEntryList{Targets: g.Targets}
		if err := read(g.Blob, plan.Request.SrcPath, &l); err != nil {
			return nil, err
		}
		out.Copies = append(out.Copies, l)
	}
	for _, t := range plan.Targets {
		if t.Delete+t.Replace == 0 {
			continue
		}
		l := api.SyncEntryList{Node: t.Node}
		if err := read("plans/"+planID+"/delete-"+t.Node, plan.Request.DstPath, &l); err != nil {
			return nil, err
		}
		out.Deletes = append(out.Deletes, l)
	}
	return out, nil
}

// inventoryRel makes a root-relative path relative to base.
func inventoryRel(base, p string) string {
	switch {
	case base == "." || base == "":
		return p
	case p == base:
		return "."
	}
	return strings.TrimPrefix(p, base+"/")
}
