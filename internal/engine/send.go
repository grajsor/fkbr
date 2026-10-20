package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
	"fkbr/internal/localfs"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
)

// sender is the source side of one job.
type sender struct {
	e      *Engine
	job    *api.Job
	status *statusWriter
	// resumeFrom is the number of chunks a previous run already published;
	// they are regenerated (the walk is deterministic) but not re-staged.
	resumeFrom int
	// purged is the number of leading chunks already deleted from the share.
	purged    int
	lastCheck time.Time

	// Fan-out bookkeeping: dropped targets, last seen ack counts and when
	// each target last made progress, and node liveness (refreshed lazily).
	detached  map[string]bool
	acks      map[string]int
	progress  map[string]time.Time
	online    map[string]bool
	lastNodes time.Time
	// gone is when each target was first seen missing from the registry;
	// tstate is each target's last published state.
	gone       map[string]time.Time
	tstate     map[string]api.JobState
	lastNotify time.Time

	log     *slog.Logger
	totals  totals
	start   time.Time
	stored  int64 // bytes staged on the share by this run
	waiting *waitLog
}

func (e *Engine) runSend(ctx context.Context, id string) error {
	job, err := e.st.GetJob(ctx, id)
	if err != nil {
		return err
	}
	s := &sender{
		e: e, job: job, status: e.newStatus(id, api.RoleSource),
		detached: map[string]bool{}, acks: map[string]int{}, progress: map[string]time.Time{}, gone: map[string]time.Time{},
		tstate: map[string]api.JobState{},
		log:    e.log.With("job", id), start: time.Now(),
	}
	s.status.before = s.totals.apply
	now := time.Now()
	for _, d := range job.Detached {
		s.detached[d.Node] = true
	}
	for _, d := range job.Request.DstNodes {
		s.progress[d] = now
	}
	if prev := ownStatus(job, e.Self(), api.RoleSource); prev != nil {
		if prev.State.Terminal() {
			return nil // duplicate request for a finished job
		}
		s.status.st = *prev
		s.status.st.State, s.status.st.Error = api.JobRunning, "" // was queued, or interrupted
		s.resumeFrom = prev.Chunks
	}
	if job.Request.SrcNode != e.Self() {
		return s.status.finish(ctx, api.JobFailed, fmt.Errorf("job source is %s, not %s", job.Request.SrcNode, e.Self()))
	}
	req := job.Request
	what := summary(req.Paths, 3)
	if req.SyncPlan != "" {
		what = fmt.Sprintf("%s (sync, %d entries)", req.BaseDir, req.PathsCount)
	}
	s.log.Info("sending", "paths", what, "from", req.SrcRoot, "to", strings.Join(req.DstNodes, ","),
		"dest", req.DstRoot+"/"+req.DstDir, "zstd", req.Compress, "resume_after", s.resumeFrom)
	err = s.run(ctx)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errCancelled):
		e.st.RemoveJobData(ctx, id)
		return s.status.finish(ctx, api.JobCancelled, nil)
	case err != nil:
		e.st.RemoveJobData(ctx, id)
		return s.status.finish(ctx, api.JobFailed, err)
	}
	if err := e.st.RemoveJobData(ctx, id); err != nil {
		e.log.Warn("removing staged data", "job", id, "err", err)
	}
	took := time.Since(s.start)
	st := s.status.st
	s.log.Info("send done", "files", st.Files, "bytes", size(st.Bytes), "chunks", st.Chunks, "stored", size(s.stored),
		"targets", summary(s.active(), 8), "detached", len(s.detached), "took", took.Round(time.Millisecond), "rate", rate(st.Bytes, took))
	return s.status.finish(ctx, api.JobDone, nil)
}

// active returns the targets still part of the job.
func (s *sender) active() []string {
	var out []string
	for _, d := range s.job.Request.DstNodes {
		if !s.detached[d] {
			out = append(out, d)
		}
	}
	return out
}

func (s *sender) run(ctx context.Context) error {
	e := s.e
	// Tell every target to start consuming. Duplicates are harmless.
	for _, d := range s.active() {
		if err := s.notify(ctx, d); err != nil {
			return err
		}
	}
	s.lastNotify = time.Now()
	for _, d := range s.job.Request.DstNodes {
		if n, err := e.st.Acked(ctx, e.Self(), s.job.ID, d); err == nil {
			s.resumeFrom = max(s.resumeFrom, n)
		}
	}

	total := s.job.TotalChunks
	if total > 0 { // produced by an earlier run
		s.totals.finish(s.status.st.Files, s.status.st.Bytes, true)
	} else {
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go s.count(cctx)
	}
	ride := &rideOut{log: s.log}
	for produced := total > 0; !produced; {
		n, err := s.produce(ctx)
		if err != nil && !isNoSpace(err) && ride.retry(ctx, err) {
			s.resumeFrom = s.status.st.Chunks // regenerate, keep what is staged
			continue
		}
		if isNoSpace(err) {
			// The share is smaller than the configured quotas. Wait for
			// targets to consume, then regenerate from the start without
			// re-staging what is already published.
			if err := s.waitAfterNoSpace(ctx, err); err != nil {
				return err
			}
			s.resumeFrom = s.status.st.Chunks
			continue
		}
		if err != nil {
			return err
		}
		for {
			err := e.st.SetComplete(ctx, s.job.ID, n)
			if err == nil {
				break
			}
			if !ride.retry(ctx, err) {
				return err
			}
		}
		ride.ok()
		s.totals.finish(s.status.st.Files, s.status.st.Bytes, true)
		total, produced = n, true
	}
	s.status.st.Chunks = total
	s.status.st.Phase = "waiting for targets"
	s.status.put(ctx, true)
	s.log.Info("all chunks staged; waiting for targets", "chunks", total)
	wait := newWaitLog(s.log, "waiting for targets", waitLogAfter, waitLogEvery)
	for {
		done, err := s.gc(ctx, total)
		if err == nil && !done {
			if err = s.check(ctx, true); err == nil {
				err = s.detachLaggards(ctx, total)
			}
		}
		if err != nil && ride.retry(ctx, err) {
			continue
		}
		if err != nil || done {
			wait.done()
			return err
		}
		ride.ok()
		wait.tick("acked", s.ackSummary(total))
		if err := e.sleep(ctx); err != nil {
			return err
		}
	}
}

// produce walks the source paths into chunks and returns the chunk count.
func (s *sender) produce(ctx context.Context) (int, error) {
	e, req := s.e, s.job.Request
	s.status.st.Phase = "sending"
	if s.resumeFrom > 0 {
		s.status.st.Phase = fmt.Sprintf("resuming after chunk %d", s.resumeFrom)
	}
	s.status.put(ctx, true)
	sink := &shareSink{s: s, ctx: ctx}
	w := bundle.NewWriter(sink, int64(e.cfg.Transfer.ChunkSize), req.Compress)
	sink.w = w
	defer func() {
		w.Abort()
		if sink.cw != nil {
			sink.cw.Abort() // drop the half-written temp file
		}
	}()
	add := func(p, rel string, ent api.Entry, err error) error {
		if rel == receive.StagingDir || strings.HasPrefix(rel, receive.StagingDir+"/") {
			return localfs.SkipDir
		}
		if err != nil {
			s.status.problem("%s: %v", rel, err)
			return nil
		}
		if err := s.check(ctx, false); err != nil {
			return err
		}
		err = w.Add(bundle.Item{Name: s.nameFor(p, rel), Entry: ent, Open: func() (bundle.File, error) {
			return e.fs.Open(req.SrcRoot, rel)
		}})
		var ie *bundle.ItemError
		if errors.Is(err, bundle.ErrUnsupported) || errors.As(err, &ie) {
			s.status.problem("%v", err)
			err = nil
		}
		s.status.st.Files, s.status.st.Bytes = w.Files, w.Bytes
		s.status.put(ctx, false)
		return err
	}
	visit := func(p string) error {
		if !req.NoRecurse {
			return e.fs.Walk(req.SrcRoot, p, func(rel string, ent api.Entry, err error) error {
				return add(p, rel, ent, err)
			})
		}
		ent, err := e.fs.Stat(req.SrcRoot, p)
		if errors.Is(err, fs.ErrNotExist) {
			s.status.problem("%s: vanished since it was planned", p)
			return nil
		}
		if err := add(p, p, ent, err); err != nil && err != localfs.SkipDir {
			return err
		}
		return nil
	}
	if req.PathsBlob != "" {
		rc, err := e.st.OpenBlob(ctx, req.PathsBlob)
		if err != nil {
			return 0, err
		}
		err = readPathList(rc, visit)
		rc.Close()
		if err != nil {
			return 0, err
		}
	} else {
		for _, p := range req.Paths {
			if err := visit(p); err != nil {
				return 0, err
			}
		}
	}
	n, err := w.Close()
	s.status.st.Files, s.status.st.Bytes, s.status.st.Chunks = w.Files, w.Bytes, n
	return n, err
}

// nameFor is an entry's name in the bundle: relative to BaseDir when set
// (sync), else under the copied path's base name.
func (s *sender) nameFor(src, rel string) string {
	base := s.job.Request.BaseDir
	switch {
	case base == "":
		return bundleName(src, rel)
	case rel == base:
		return "."
	case base == ".":
		return rel
	}
	return strings.TrimPrefix(rel, base+"/")
}

// bundleName maps a walked path to its name in the bundle: a copied path
// lands under its base name in the destination directory, like cp does;
// "." copies the root's contents.
func bundleName(src, rel string) string {
	if src == "." {
		return rel
	}
	return path.Join(path.Base(src), strings.TrimPrefix(strings.TrimPrefix(rel, src), "/"))
}

// check polls cancellation and target failures, at most every 2s unless
// force. A failed target is detached while others remain; if it was the
// last one, the job fails.
func (s *sender) check(ctx context.Context, force bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !force && time.Since(s.lastCheck) < 2*time.Second {
		return nil
	}
	s.lastCheck = time.Now()
	j, err := s.e.st.GetJob(ctx, s.job.ID)
	if err != nil {
		return err
	}
	if j.Cancelled {
		return errCancelled
	}
	for _, d := range j.Detached {
		s.detached[d.Node] = true
	}
	renotify := time.Since(s.lastNotify) > renotifyEvery
	for _, d := range s.active() {
		st := roleState(j, api.RoleTarget, d)
		s.tstate[d] = st
		switch st {
		case "":
			// Not started yet: the notify may have been lost (or the node
			// was busy, or restarted from an old inbox). Send it again.
			if renotify {
				s.log.Info("target has not started; notifying it again", "target", d)
				if err := s.notify(ctx, d); err != nil {
					s.log.Warn("notify failed", "target", d, "err", err)
				}
			}
		case api.JobFailed:
			if len(s.active()) == 1 {
				return fmt.Errorf("target %s failed", d)
			}
			if err := s.detach(ctx, d, "failed: "+targetError(j, d)); err != nil {
				return err
			}
		case api.JobCancelled:
			return errCancelled
		}
	}
	if renotify {
		s.lastNotify = time.Now()
	}
	return nil
}

// renotifyEvery is how often a target that has not started is notified
// again.
var renotifyEvery = time.Minute

// notify asks target d to start consuming this job.
func (s *sender) notify(ctx context.Context, d string) error {
	msg, err := s.e.newMessage(d, transport.TypeReceive, transport.JobRef{ID: s.job.ID})
	if err != nil {
		return err
	}
	msg.Notify = true
	if err := s.e.tr.Send(ctx, msg); err != nil {
		return fmt.Errorf("notify %s: %w", d, err)
	}
	return nil
}

func targetError(j *api.Job, node string) string {
	for _, p := range j.Participants {
		if p.Role == api.RoleTarget && p.Node == node && p.Error != "" {
			return p.Error
		}
	}
	return "unknown error"
}

// gc deletes chunks every active target has consumed and reports whether
// they have all consumed all total chunks (total < 0: not known yet).
func (s *sender) gc(ctx context.Context, total int) (bool, error) {
	minAck := -1
	now := time.Now()
	for _, d := range s.active() {
		n, err := s.e.st.Acked(ctx, s.e.Self(), s.job.ID, d)
		if err != nil {
			return false, err
		}
		if n > s.acks[d] {
			s.acks[d] = n
			s.progress[d] = now
			s.log.Info("target acked", "target", d, "chunks", n)
		}
		if minAck < 0 || n < minAck {
			minAck = n
		}
	}
	for ; s.purged < minAck; s.purged++ {
		if err := s.e.st.DeleteChunk(ctx, s.job.ID, s.purged); err != nil {
			return false, err
		}
	}
	return total >= 0 && minAck >= total, nil
}

// ackSummary lists each active target's acked chunks, e.g. "a=3/4 b=1/4".
func (s *sender) ackSummary(total int) string {
	var parts []string
	for _, d := range s.active() {
		if total >= 0 {
			parts = append(parts, fmt.Sprintf("%s=%d/%d", d, s.acks[d], total))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%d", d, s.acks[d]))
		}
	}
	return strings.Join(parts, " ")
}

// detachLaggards is called while the source is blocked (no staging space,
// or waiting for the last acks). A target that is holding things up and
// has made no progress for detach_after, or is offline, is dropped so the
// others can finish. The last active target is never dropped for that,
// but a target that was forgotten (gone from the registry) is: if it was
// the last one, the job fails instead of waiting forever.
func (s *sender) detachLaggards(ctx context.Context, total int) error {
	if time.Since(s.lastNodes) > 2*time.Second {
		nodes, err := s.e.tr.Nodes(ctx)
		if err != nil {
			return err
		}
		s.online = map[string]bool{}
		for _, n := range nodes {
			s.online[n.ID] = n.Online
		}
		s.lastNodes = time.Now()
		for _, d := range s.active() {
			if _, ok := s.online[d]; ok {
				delete(s.gone, d)
			} else if s.gone[d].IsZero() {
				s.gone[d] = s.lastNodes
			}
		}
	}
	for _, d := range s.active() {
		// Missing for a while, not just once: a listing can miss an entry
		// that is being replaced.
		if s.gone[d].IsZero() || time.Since(s.gone[d]) < s.e.cfg.OfflineAfter {
			continue
		}
		if len(s.active()) == 1 {
			return fmt.Errorf("target %s is no longer registered (forgotten)", d)
		}
		if err := s.detach(ctx, d, "no longer registered"); err != nil {
			return err
		}
	}
	active := s.active()
	if len(active) < 2 {
		return nil
	}
	minAck := -1
	for _, d := range active {
		if minAck < 0 || s.acks[d] < minAck {
			minAck = s.acks[d]
		}
	}
	cfg := s.e.cfg
	for _, d := range active {
		if len(s.active()) < 2 {
			break
		}
		holding := s.acks[d] == minAck
		if total >= 0 {
			holding = s.acks[d] < total
		}
		if !holding {
			continue
		}
		idle := time.Since(s.progress[d])
		switch {
		case !s.online[d] && idle > cfg.OfflineAfter:
			if err := s.detach(ctx, d, "offline"); err != nil {
				return err
			}
		case idle > cfg.Transfer.DetachAfter:
			reason := fmt.Sprintf("no progress for %s", idle.Round(time.Second))
			if s.tstate[d] == api.JobQueued {
				reason = fmt.Sprintf("queued on the target for %s (all its job slots busy)", idle.Round(time.Second))
			}
			if err := s.detach(ctx, d, reason); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *sender) detach(ctx context.Context, node, reason string) error {
	d := api.Detached{Node: node, Reason: reason, Chunks: s.acks[node], At: time.Now().UTC()}
	if err := s.e.st.Detach(ctx, s.job.ID, d); err != nil {
		return err
	}
	s.detached[node] = true
	s.status.note("detached %s after %d chunks: %s", node, d.Chunks, reason)
	s.status.put(ctx, true)
	s.log.Warn("target detached", "target", node, "after_chunks", d.Chunks, "reason", reason)
	return nil
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// noSpaceWait is how long the source pauses after the share filled up.
var noSpaceWait = 10 * time.Second

func (s *sender) waitAfterNoSpace(ctx context.Context, cause error) error {
	s.status.st.Phase = "share full, waiting for targets to free space"
	s.status.put(ctx, true)
	s.e.log.Warn("share full; retrying", "job", s.job.ID, "err", cause)
	deadline := time.Now().Add(noSpaceWait)
	for time.Now().Before(deadline) {
		if _, err := s.gc(ctx, -1); err != nil {
			return err
		}
		if err := s.check(ctx, true); err != nil {
			return err
		}
		if err := s.detachLaggards(ctx, -1); err != nil {
			return err
		}
		if err := s.e.sleep(ctx); err != nil {
			return err
		}
	}
	return nil
}

// waitForSpace blocks until staging one more chunk stays within quota.
func (s *sender) waitForSpace(ctx context.Context) error {
	t := s.e.cfg.Transfer
	waiting := false
	wait := newWaitLog(s.log, "waiting for staging space", waitLogAfter, waitLogEvery)
	for {
		if _, err := s.gc(ctx, -1); err != nil {
			return err
		}
		jobBytes, total, err := s.e.st.StagedBytes(ctx, s.job.ID)
		if err != nil {
			return err
		}
		if jobBytes+int64(t.ChunkSize) <= int64(t.JobQuota) && total+int64(t.ChunkSize) <= int64(t.StagingQuota) {
			if waiting {
				s.status.st.Phase = "sending"
				s.status.put(ctx, true)
			}
			wait.done()
			return nil
		}
		if !waiting {
			waiting = true
			s.status.st.Phase = "waiting for staging space (targets consuming)"
			s.status.put(ctx, true)
		}
		wait.tick("staged_job", size(jobBytes), "staged_share", size(total), "acked", s.ackSummary(-1))
		if err := s.check(ctx, true); err != nil {
			return err
		}
		if err := s.detachLaggards(ctx, -1); err != nil {
			return err
		}
		if err := s.e.sleep(ctx); err != nil {
			return err
		}
	}
}

// shareSink stages chunks on the share for a sender.
type shareSink struct {
	s   *sender
	ctx context.Context
	w   *bundle.Writer // for progress counters
	cw  transport.ChunkWriter
}

func (k *shareSink) Create(seq int) (io.WriteCloser, error) {
	if seq < k.s.resumeFrom {
		k.cw = nil
		return nopWriteCloser{io.Discard}, nil
	}
	if err := k.s.waitForSpace(k.ctx); err != nil {
		return nil, err
	}
	cw, err := k.s.e.st.CreateChunk(k.ctx, k.s.job.ID, seq)
	if err != nil {
		return nil, err
	}
	k.cw = cw
	return nopWriteCloser{cw}, nil
}

func (k *shareSink) Commit(meta bundle.ChunkMeta) error {
	s := k.s
	if k.cw == nil {
		// Regenerated chunk from before a restart: if the old one is still
		// staged it must be identical, or the source changed meanwhile.
		if old, err := s.e.st.ChunkMeta(k.ctx, s.e.Self(), s.job.ID, meta.Seq); err == nil {
			var m bundle.ChunkMeta
			if json.Unmarshal(old, &m) == nil && m.SHA256 != meta.SHA256 {
				return fmt.Errorf("source changed since the interrupted run (chunk %d differs); start the copy again", meta.Seq)
			}
		}
		return nil
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := k.cw.Commit(b); err != nil {
		return err
	}
	k.cw = nil
	s.status.st.Chunks = meta.Seq + 1
	s.status.st.Files, s.status.st.Bytes = k.w.Files, k.w.Bytes
	s.status.put(k.ctx, true)
	s.stored += meta.Size
	ratio := "-"
	if meta.Size > 0 && meta.Raw > 0 {
		ratio = fmt.Sprintf("%.2f", float64(meta.Raw)/float64(meta.Size))
	}
	s.log.Info("chunk staged", "chunk", meta.Seq, "entries", meta.Entries, "raw", size(meta.Raw), "stored", size(meta.Size),
		"ratio", ratio, "files_so_far", k.w.Files, "bytes_so_far", size(k.w.Bytes))
	return nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
