package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/exclude"
	"fkbr/internal/id"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
	"fkbr/internal/transport"
)

// Submit validates a transfer against the node registry, records the job on
// the share and asks the source node's worker to start producing chunks.
func (e *Engine) Submit(ctx context.Context, req api.TransferRequest) (string, error) {
	return e.submit(ctx, req, "")
}

func (e *Engine) submit(ctx context.Context, req api.TransferRequest, catchUpOf string) (string, error) {
	if req.SrcNode == "" {
		req.SrcNode = e.Self()
	}
	if req.Conflict == "" {
		req.Conflict = api.DefaultConflictPolicy
	}
	if !req.Conflict.Valid() {
		return "", fmt.Errorf("invalid conflict policy %q", req.Conflict)
	}
	if len(req.Paths) == 0 && req.PathsBlob == "" {
		return "", errors.New("no source paths")
	}
	if req.BaseDir != "" {
		b, err := localfs.CleanRel(req.BaseDir)
		if err != nil {
			return "", err
		}
		req.BaseDir = b
	}
	for i, p := range req.Paths {
		c, err := localfs.CleanRel(p)
		if err != nil {
			return "", err
		}
		req.Paths[i] = c
	}
	dst, err := localfs.CleanRel(req.DstDir)
	if err != nil {
		return "", err
	}
	req.DstDir = dst
	if _, err := exclude.New(req.Exclude); err != nil {
		return "", err
	}

	nodes, err := e.Nodes(ctx) // with this node's local roots
	if err != nil {
		return "", err
	}
	byID := map[string]api.Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	src, err := onlineNode(byID, req.SrcNode)
	if err != nil {
		return "", err
	}
	if _, ok := findRoot(src, req.SrcRoot); !ok {
		return "", fmt.Errorf("%w: %s:%s", api.ErrUnknownRoot, req.SrcNode, req.SrcRoot)
	}
	if len(req.Exclude) > 0 {
		if err := needFeature(api.FeatureExclude, "excludes", src); err != nil {
			return "", err
		}
	}
	targets, offline, err := expandTargets(byID, req.DstNodes, req.SrcNode, req.DetachOffline)
	if err != nil {
		return "", err
	}
	if req.Move && sameRootTarget(req, append(append([]string(nil), targets...), offline...)) {
		if err := moveOverlap(req, req.Paths, req.DstDir); err != nil {
			return "", err
		}
		if err := needFeature(api.FeatureMove, "safe moves onto itself", src); err != nil {
			return "", err
		}
	}
	initiator := e.info
	if n, ok := byID[e.Self()]; ok {
		initiator = n.NodeInfo
	}
	for _, d := range append(append([]string(nil), targets...), offline...) {
		n := byID[d]
		r, ok := findRoot(n, req.DstRoot)
		if !ok {
			return "", fmt.Errorf("%w: %s:%s", api.ErrUnknownRoot, d, req.DstRoot)
		}
		if !r.Writable {
			return "", fmt.Errorf("%w: %s:%s", api.ErrReadOnly, d, req.DstRoot)
		}
		if !n.AcceptsWritesFrom(&initiator) {
			return "", fmt.Errorf("%w: %s only accepts writes from %v", api.ErrNotAllowed, d, n.AcceptWritesFrom)
		}
	}
	req.DstNodes = append(targets, offline...)
	slices.Sort(req.DstNodes)
	if err := e.checkLocalSubmit(&req); err != nil {
		return "", err
	}
	if req.SrcNode == e.Self() {
		// Here, a missing source path is refused before a job exists; on
		// another node the source fails the job (missingPaths in send.go).
		if err := e.missingPaths(req); err != nil {
			return "", err
		}
	}

	now := e.now()
	job := &api.Job{ID: id.At(now), Initiator: e.Self(), Created: now, Request: req, CatchUpOf: catchUpOf}
	for _, d := range offline {
		job.Detached = append(job.Detached, api.Detached{Node: d, Reason: "offline at submit", At: now})
	}
	if err := e.keepOwn(job); err != nil {
		return "", err
	}
	if err := e.st.CreateJob(ctx, job); err != nil {
		return "", err
	}
	msg, err := e.newMessage(req.SrcNode, transport.TypeSend, transport.JobRef{ID: job.ID})
	if err != nil {
		return "", err
	}
	msg.Notify = true
	if err := e.tr.Send(ctx, msg); err != nil {
		return "", err
	}
	return job.ID, nil
}

// needFeature refuses a request a node's build cannot carry out (it would
// ignore what it does not know, and do something else than asked).
func needFeature(f, what string, nodes ...api.Node) error {
	for _, n := range nodes {
		if !n.Has(f) {
			return fmt.Errorf("%s (version %s) runs an older %s without %s; update it first", n.ID, n.Version, product.Name, what)
		}
	}
	return nil
}

// expandTargets resolves destination entries. A node ID must be online,
// unless detachOffline. "@all" means every other registered node, and
// "@role" every registered node with that role, in both cases except the
// source; members that are offline are returned separately so the job can
// record them as detached. At least one target must be online.
func expandTargets(byID map[string]api.Node, dsts []string, src string, detachOffline bool) (online, offline []string, err error) {
	seen := map[string]bool{}
	add := func(n api.Node) {
		if seen[n.ID] {
			return
		}
		seen[n.ID] = true
		if n.Online {
			online = append(online, n.ID)
		} else {
			offline = append(offline, n.ID)
		}
	}
	for _, d := range dsts {
		role, isGroup := strings.CutPrefix(d, "@")
		if !isGroup {
			n, err := onlineNode(byID, d)
			if err != nil && !(detachOffline && errors.Is(err, api.ErrNodeOffline)) {
				return nil, nil, err
			}
			add(n)
			continue
		}
		if role == api.GroupDest {
			return nil, nil, fmt.Errorf("%w: @%s is the TUI's marked nodes; name the nodes instead (n1,n2)", api.ErrUnknownNode, role)
		}
		var members []api.Node
		for _, n := range byID {
			if n.ID != src && (role == api.GroupAll || n.HasRole(role)) {
				members = append(members, n)
			}
		}
		if len(members) == 0 && role == api.GroupAll {
			return nil, nil, fmt.Errorf("%w: no other nodes are registered on this share", api.ErrUnknownNode)
		}
		if len(members) == 0 {
			return nil, nil, fmt.Errorf("%w: no nodes with role %q", api.ErrUnknownNode, role)
		}
		slices.SortFunc(members, func(a, b api.Node) int { return strings.Compare(a.ID, b.ID) })
		for _, n := range members {
			add(n)
		}
	}
	if len(online) == 0 {
		if len(offline) > 0 {
			return nil, nil, fmt.Errorf("%w: all targets are offline (%s)", api.ErrNodeOffline, strings.Join(offline, ", "))
		}
		return nil, nil, errors.New("no destination node")
	}
	return online, offline, nil
}

// CatchUp re-sends a job to its targets that missed it and are online
// now: detached ones, and ones that failed (a link down for longer than
// transfers wait for it, say).
func (e *Engine) CatchUp(ctx context.Context, id string) (string, error) {
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		return "", err
	}
	missed := missedTargets(j)
	if len(missed) == 0 {
		return "", fmt.Errorf("job %s has no detached or failed targets", id)
	}
	nodes, err := e.tr.Nodes(ctx)
	if err != nil {
		return "", err
	}
	online := map[string]bool{}
	for _, n := range nodes {
		online[n.ID] = n.Online
	}
	req := j.Request
	req.DstNodes = nil
	var down []string
	for _, n := range missed {
		if online[n] {
			req.DstNodes = append(req.DstNodes, n)
		} else {
			down = append(down, n)
		}
	}
	if len(req.DstNodes) == 0 {
		return "", fmt.Errorf("%w: targets still offline: %s", api.ErrNodeOffline, strings.Join(down, ", "))
	}
	return e.submit(ctx, req, id)
}

// missedTargets are a job's targets that did not get it: detached, or
// failed.
func missedTargets(j *api.Job) []string {
	var out []string
	for _, d := range j.Detached {
		out = append(out, d.Node)
	}
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && p.State == api.JobFailed && !slices.Contains(out, p.Node) {
			out = append(out, p.Node)
		}
	}
	return out
}

func onlineNode(byID map[string]api.Node, id string) (api.Node, error) {
	n, ok := byID[id]
	if !ok {
		return n, fmt.Errorf("%w: %q", api.ErrUnknownNode, id)
	}
	if !n.Online {
		return n, fmt.Errorf("%w: %s (is its worker running?)", api.ErrNodeOffline, id)
	}
	return n, nil
}

func findRoot(n api.Node, id string) (api.Root, bool) {
	for _, r := range n.Roots {
		if r.ID == id {
			return r, true
		}
	}
	return api.Root{}, false
}

func (e *Engine) Job(ctx context.Context, id string) (*api.Job, error) {
	j, err := e.st.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	normalize(j)
	e.goneNodes(ctx, []*api.Job{j}, nil)
	return j, nil
}

func (e *Engine) Jobs(ctx context.Context) ([]api.Job, error) {
	js, err := e.st.ListJobs(ctx)
	var ps []*api.Job
	for i := range js {
		normalize(&js[i])
		ps = append(ps, &js[i])
	}
	e.goneNodes(ctx, ps, nil)
	return js, err
}

// goneNodes ends jobs in the view that a gone node would never end. A gone
// node is forgotten, or offline for longer than detach_after.
//
// A target is shown as detached when the source is done but the target
// never said so and is gone (it died after its last ack). If it comes
// back, it installs and the job is done.
//
// A source is shown as finished when every target has ended but the
// source never said so and is gone (it died, or was forgotten, before it
// wrote its last status): done if every target is done, failed otherwise.
// Prune ignores the statuses of forgotten nodes in the same way.
func (e *Engine) goneNodes(ctx context.Context, js []*api.Job, nodes []api.Node) {
	gone := func(node string) string {
		i := slices.IndexFunc(nodes, func(n api.Node) bool { return n.ID == node })
		switch {
		case i < 0:
			return "forgotten"
		case !nodes[i].Online && nodes[i].LastSeen > e.cfg.Transfer.DetachAfter:
			return fmt.Sprintf("offline for %s", nodes[i].LastSeen.Round(time.Second))
		}
		return ""
	}
	for _, j := range js {
		if j.State.Terminal() {
			continue
		}
		src := roleState(j, api.RoleSource, "")
		if src != api.JobDone && !(src != "" && !src.Terminal() && targetsEnded(j)) {
			continue
		}
		if nodes == nil {
			var err error
			if nodes, err = e.tr.Nodes(ctx); err != nil {
				return
			}
		}
		if src != api.JobDone {
			why := gone(j.Request.SrcNode)
			if why == "" {
				continue
			}
			for i := range j.Participants {
				p := &j.Participants[i]
				if p.Role != api.RoleSource {
					continue
				}
				p.State, p.Phase = api.JobDone, ""
				if !targetsDone(j) {
					p.State = api.JobFailed
					p.Error = "source " + why + " before it finished"
				}
				p.Problems = append(p.Problems, "source "+why+" before it wrote its last status; every target had ended")
			}
			normalize(j)
			continue
		}
		for _, p := range j.Participants {
			if p.Role != api.RoleTarget || p.State.Terminal() || j.IsDetached(p.Node) {
				continue
			}
			switch why := gone(p.Node); {
			case why == "forgotten":
				j.Detached = append(j.Detached, api.Detached{Node: p.Node, Reason: "forgotten before it finished", Chunks: p.Chunks})
			case why != "":
				j.Detached = append(j.Detached, api.Detached{Node: p.Node, Chunks: p.Chunks, Reason: why + " after the source finished"})
			}
		}
		normalize(j)
	}
}

// targetsEnded: every target that is not detached has a final status.
func targetsEnded(j *api.Job) bool {
	n := 0
	for _, p := range j.Participants {
		if p.Role != api.RoleTarget || j.IsDetached(p.Node) {
			continue
		}
		if !p.State.Terminal() {
			return false
		}
		n++
	}
	return n == len(j.Request.DstNodes)-len(j.Detached)
}

// targetsDone: every target that is not detached is done.
func targetsDone(j *api.Job) bool {
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && !j.IsDetached(p.Node) && p.State != api.JobDone {
			return false
		}
	}
	return true
}

func (e *Engine) Cancel(ctx context.Context, id string) error {
	return e.st.RequestCancel(ctx, id)
}

// normalize derives the job state and shows detached targets as such even
// if their last status is stale (e.g. the node died mid-transfer).
func normalize(j *api.Job) {
	j.State = deriveState(j)
	for i := range j.Participants {
		p := &j.Participants[i]
		if p.Role == api.RoleTarget && j.IsDetached(p.Node) && !p.State.Terminal() {
			p.State, p.Phase = api.JobDetached, ""
		}
	}
}

// deriveState combines participant statuses into the job's overall state.
// Detached targets do not count, except that their presence turns a
// completed job into a partial one. A failed target is only final once
// the source has stopped: while it runs it may still detach the target and
// carry on with the others.
func deriveState(j *api.Job) api.JobState {
	want := 1 + len(j.Request.DstNodes) - len(j.Detached)
	done, counted, terminal := 0, 0, 0
	srcState := api.JobState("")
	targetFailed := false
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && j.IsDetached(p.Node) {
			continue
		}
		counted++
		if p.Role == api.RoleSource {
			srcState = p.State
		} else if p.State == api.JobFailed {
			targetFailed = true
		}
		if p.State == api.JobDone {
			done++
		}
		if p.State.Terminal() {
			terminal++
		}
	}
	switch {
	case srcState == api.JobFailed, targetFailed && srcState.Terminal():
		return api.JobFailed
	case done == want && len(j.Detached) > 0:
		return api.JobPartial
	case done == want:
		return api.JobDone
	case j.Cancelled && terminal == counted:
		return api.JobCancelled
	case counted == 0:
		return api.JobQueued
	}
	return api.JobRunning
}

// startJob runs a send or receive request in the background. The request
// stays claimed (inbox cur/) until the job ends, so a crashed worker picks
// it up again on restart and resumes from the state on the share.
func (e *Engine) startJob(ctx context.Context, m *transport.Message) {
	var ref transport.JobRef
	if err := json.Unmarshal(m.Body, &ref); err != nil || ref.ID == "" {
		e.log.Warn("bad job request", "req", m.ID, "err", err)
		e.tr.Finish(ctx, m, true)
		return
	}
	key := m.Type + "/" + ref.ID
	e.jobsMu.Lock()
	dup := e.running[key]
	e.running[key] = true
	e.jobsMu.Unlock()
	if dup {
		e.tr.Finish(ctx, m, false)
		return
	}
	sem, run, role := e.sendSem, e.runSend, api.RoleSource
	if m.Type == transport.TypeReceive {
		sem, run, role = e.recvSem, e.runReceive, api.RoleTarget
	}
	e.jobsWG.Add(1)
	go func() {
		defer e.jobsWG.Done()
		defer func() {
			e.jobsMu.Lock()
			delete(e.running, key)
			e.jobsMu.Unlock()
		}()
		select {
		case sem <- struct{}{}:
		default:
			e.markQueued(ctx, ref.ID, role)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
		defer func() { <-sem }()
		log := e.log.With("job", ref.ID, "role", m.Type)
		log.Info("job started")
		err := run(ctx, ref.ID)
		if ctx.Err() != nil {
			log.Info("job interrupted; will resume on restart")
			return
		}
		if err != nil {
			log.Warn("job failed", "err", err)
		} else {
			log.Info("job finished")
		}
		if ferr := e.tr.Finish(context.WithoutCancel(ctx), m, err != nil); ferr != nil {
			log.Warn("finish failed", "err", ferr)
		}
	}()
}

// statusWriter publishes a participant's status, throttled.
type statusWriter struct {
	e    *Engine
	job  string
	st   api.JobStatus
	last time.Time
	// before, if set, updates st just before each publication.
	before func(*api.JobStatus)
}

func (e *Engine) newStatus(job string, role api.Role) *statusWriter {
	return &statusWriter{e: e, job: job, st: api.JobStatus{Node: e.Self(), Role: role, State: api.JobRunning}}
}

// problem records an error affecting one entry.
func (w *statusWriter) problem(format string, args ...any) {
	w.st.Errors++
	w.note(format, args...)
}

// note records a message that is not an error, stamped with the time
// ("15:04:05 message") so readers can put notes from all nodes in order.
func (w *statusWriter) note(format string, args ...any) {
	if len(w.st.Problems) < 200 {
		w.st.Problems = append(w.st.Problems, time.Now().Format("15:04:05")+" "+fmt.Sprintf(format, args...))
	}
}

// put publishes the status; unless force, at most every 500ms.
func (w *statusWriter) put(ctx context.Context, force bool) {
	if !force && time.Since(w.last) < 500*time.Millisecond {
		return
	}
	w.last = time.Now()
	w.st.Updated = w.e.now()
	if w.before != nil {
		w.before(&w.st)
	}
	if err := w.e.st.PutStatus(context.WithoutCancel(ctx), w.job, &w.st); err != nil {
		w.e.log.Warn("status update failed", "job", w.job, "err", err)
	}
}

func (w *statusWriter) finish(ctx context.Context, state api.JobState, err error) error {
	w.st.State = state
	w.st.Phase = ""
	if err != nil {
		w.st.Error = err.Error()
	}
	w.put(ctx, true)
	return err
}

// markQueued publishes that this node has the job but waits for a free
// slot (transfer.max_jobs), so nobody takes the wait for a stall. A job
// that already has a status (resumed after a restart) keeps it.
func (e *Engine) markQueued(ctx context.Context, id string, role api.Role) {
	j, err := e.st.GetJob(ctx, id)
	if err != nil || ownStatus(j, e.Self(), role) != nil {
		return
	}
	w := e.newStatus(id, role)
	w.st.State = api.JobQueued
	w.st.Phase = fmt.Sprintf("waiting for a free slot (%d jobs running)", e.cfg.Transfer.MaxJobs)
	w.put(ctx, true)
	e.log.Info("job queued: all slots busy", "job", id, "role", role, "max_jobs", e.cfg.Transfer.MaxJobs)
}

// ownStatus returns this node's previously published status for a job.
func ownStatus(j *api.Job, self string, role api.Role) *api.JobStatus {
	for i := range j.Participants {
		if p := &j.Participants[i]; p.Node == self && p.Role == role {
			return p
		}
	}
	return nil
}

func roleState(j *api.Job, role api.Role, node string) api.JobState {
	for _, p := range j.Participants {
		if p.Role == role && (node == "" || p.Node == node) {
			return p.State
		}
	}
	return ""
}

var (
	errCancelled = errors.New("cancelled")
	errDetached  = errors.New("detached by the source")
)

// sleep waits one poll interval or until ctx ends.
func (e *Engine) sleep(ctx context.Context) error {
	t := time.NewTimer(e.cfg.Poll.Active)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
