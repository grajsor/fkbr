package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
)

func (e *Engine) runReceive(ctx context.Context, id string) error {
	job, err := e.st.GetJob(ctx, id)
	if err != nil {
		return err
	}
	status := e.newStatus(id, api.RoleTarget)
	if prev := ownStatus(job, e.Self(), api.RoleTarget); prev != nil {
		if prev.State.Terminal() {
			return nil
		}
		status.st = *prev
		status.st.State = api.JobRunning // was queued, or interrupted
	}
	req := job.Request
	if !slices.Contains(req.DstNodes, e.Self()) {
		return status.finish(ctx, api.JobFailed, fmt.Errorf("%s is not a target of this job", e.Self()))
	}
	// The receiver enforces its own write policy; the initiator's check at
	// submit time is only a courtesy.
	if err := e.acceptWrite(ctx, job.Initiator); err != nil {
		return status.finish(ctx, api.JobFailed, err)
	}
	root, err := e.fs.WritableRoot(req.DstRoot)
	if err != nil {
		return status.finish(ctx, api.JobFailed, err)
	}
	_, noModes := localfs.Limits(e.dstPath(req.DstRoot, req.DstDir))
	rcv, err := receive.New(root, id, req.DstDir, receive.Options{
		NoModes:    noModes,
		Conflict:   req.Conflict,
		Chown:      e.info.ChownCapable,
		ChownDirs:  e.info.ChownCapable && e.info.DACOverride,
		AllowSetID: e.cfg.Transfer.AllowSetID,
		Hide:       e.shareIn(req.DstRoot),
	})
	if err != nil {
		return status.finish(ctx, api.JobFailed, err)
	}

	log := e.log.With("job", id)
	log.Info("receiving", "from", req.SrcNode, "into", req.DstRoot+"/"+req.DstDir, "resume_at_chunk", status.st.Chunks)
	if job.IsDetached(e.Self()) {
		err = errDetached // dropped while this node was down
	} else {
		err = e.consume(ctx, job, rcv, status)
	}
	if err != nil && ctx.Err() == nil && !errors.Is(err, errCancelled) {
		// A chunk may vanish because the source dropped us mid-read.
		if j, jerr := e.st.GetJob(ctx, id); jerr == nil && j.IsDetached(e.Self()) {
			err = errDetached
		}
	}
	switch {
	case ctx.Err() != nil:
		return ctx.Err() // keep staging; resume on restart
	case errors.Is(err, errDetached):
		// An orderly outcome, not a failure of this request.
		log.Warn("detached by the source; nothing installed", "after_chunks", status.st.Chunks)
		rcv.Abort()
		status.finish(ctx, api.JobDetached, fmt.Errorf("%w; run '%s catchup %s'", err, product.Name, id))
		return nil
	case errors.Is(err, errCancelled):
		rcv.Abort()
		return status.finish(ctx, api.JobCancelled, nil)
	case err != nil:
		rcv.Abort()
		return status.finish(ctx, api.JobFailed, err)
	}

	status.st.Phase = "installing"
	status.put(ctx, true)
	start := time.Now()
	res, err := rcv.Commit()
	if err != nil {
		rcv.Abort() // a failed job never resumes: don't leave its staging behind
		return status.finish(ctx, api.JobFailed, fmt.Errorf("install: %w", err))
	}
	log.Info("installed", "files", res.Files, "dirs", res.Dirs, "symlinks", res.Symlinks, "bytes", size(res.Bytes),
		"skipped", res.Skipped, "errors", res.Errors, "took", time.Since(start).Round(time.Millisecond))
	st := &status.st
	st.Files = res.Files + res.Symlinks + res.Dirs
	st.Bytes = res.Bytes
	st.Skipped = res.Skipped
	st.Errors += res.Errors
	for _, p := range res.Problems {
		status.note("%s", p)
	}
	if res.OwnerNotPreserved > 0 {
		status.note("ownership not preserved on %d entries (needs root or CAP_CHOWN)", res.OwnerNotPreserved)
	}
	return status.finish(ctx, api.JobDone, nil)
}

// consume applies chunks in order until the source has published them all.
func (e *Engine) consume(ctx context.Context, job *api.Job, rcv *receive.Receiver, status *statusWriter) error {
	src, id := job.Request.SrcNode, job.ID
	next, err := e.st.Acked(ctx, src, id, e.Self())
	if err != nil {
		return err
	}
	// The acks vanish with the source's job data once it is done, so a
	// restart after the last ack would otherwise start again at 0.
	next = max(next, status.st.Chunks)
	status.st.Phase = "receiving"
	status.put(ctx, true)
	log := e.log.With("job", id)
	wait := newWaitLog(log, "waiting for chunk", waitLogAfter, waitLogEvery)
	ride := &rideOut{log: log}
	srcGone := &goneWatch{e: e, node: src}
	for {
		// Staged chunks may keep coming; a stopping worker must not apply
		// them all before it lets go (the job resumes on restart).
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := e.st.ChunkMeta(ctx, src, id, next)
		if err != nil && !errors.Is(err, fs.ErrNotExist) && ride.retry(ctx, err) {
			continue
		}
		if errors.Is(err, fs.ErrNotExist) {
			j, err := e.st.GetJob(ctx, id)
			if err != nil {
				if ride.retry(ctx, err) {
					continue
				}
				return err
			}
			total := "unknown yet"
			if j.TotalChunks > 0 {
				total = fmt.Sprint(j.TotalChunks)
			}
			wait.tick("chunk", next, "of", total, "source", src, "source_state", roleState(j, api.RoleSource, ""))
			switch {
			case j.Cancelled:
				return errCancelled
			case j.IsDetached(e.Self()):
				return errDetached
			case j.TotalChunks > 0 && next >= j.TotalChunks:
				return nil
			}
			switch roleState(j, api.RoleSource, "") {
			case api.JobFailed:
				return fmt.Errorf("source %s failed", src)
			case api.JobCancelled:
				return errCancelled
			case api.JobDone:
				// The source finishes only once every target still in the
				// job has acked every chunk, so this one has them all,
				// even if it was interrupted before it could install.
				return nil
			}
			if srcGone.gone(ctx) {
				return fmt.Errorf("source %s is no longer registered (forgotten)", src)
			}
			if err := e.sleep(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		wait.done("chunk", next)
		wait = newWaitLog(log, "waiting for chunk", waitLogAfter, waitLogEvery)
		var meta bundle.ChunkMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("chunk %d meta: %w", next, err)
		}
		start := time.Now()
		files, bytes := status.st.Files, status.st.Bytes
		if err := e.applyChunk(ctx, src, id, &meta, rcv, status); err != nil {
			if ride.retry(ctx, err) {
				status.st.Files, status.st.Bytes = files, bytes // applied again from the start
				continue
			}
			return fmt.Errorf("chunk %d: %w", next, err)
		}
		ride.ok()
		log.Info("chunk applied", "chunk", next, "entries", meta.Entries, "stored", size(meta.Size), "took", time.Since(start).Round(time.Millisecond))
		next++
		for {
			err := e.st.SetAcked(ctx, src, id, e.Self(), next)
			if err == nil {
				break
			}
			if !ride.retry(ctx, err) {
				return err
			}
		}
		status.st.Chunks = next
		status.put(ctx, false)
	}
}

// errChecksum marks a chunk whose stored bytes do not match its meta.
var errChecksum = errors.New("checksum mismatch")

// applyChunk stages one chunk. A chunk that fails its checksum is read
// once more (a transient read problem must not kill the job); staging the
// same chunk twice is harmless. A persistent mismatch fails the target,
// and nothing from it is ever installed.
func (e *Engine) applyChunk(ctx context.Context, src, id string, meta *bundle.ChunkMeta, rcv *receive.Receiver, status *statusWriter) error {
	if err := rcv.MarkBad(meta.Bad); err != nil {
		return err
	}
	for _, b := range meta.Bad {
		status.problem("%s: changed or unreadable on the source, not copied", b)
	}
	files, bytes := status.st.Files, status.st.Bytes
	err := e.readChunk(ctx, src, id, meta, rcv, status)
	if errors.Is(err, errChecksum) {
		e.log.Warn("chunk failed its checksum; reading it again", "job", id, "chunk", meta.Seq, "err", err)
		status.st.Files, status.st.Bytes = files, bytes
		err = e.readChunk(ctx, src, id, meta, rcv, status)
	}
	return err
}

func (e *Engine) readChunk(ctx context.Context, src, id string, meta *bundle.ChunkMeta, rcv *receive.Receiver, status *statusWriter) error {
	rc, err := e.st.OpenChunk(ctx, src, id, meta.Seq)
	if err != nil {
		return err
	}
	defer rc.Close()
	ra := readAhead(rc)
	defer ra.Close()
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(ra, h)}
	applyErr := bundle.Read(cr, meta.Compressed, func(hd *bundle.Header, data io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err // staging the chunk again on restart is harmless
		}
		status.st.Bytes += hd.Length
		if hd.Entry.Type != api.TypeFile || hd.Last() {
			status.st.Files++ // provisional; Commit sets the real counts
		}
		return rcv.Apply(hd, data)
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Hash the whole stored chunk (including trailers, or the rest after a
	// parse error, so corruption is reported as such).
	if _, err := io.Copy(io.Discard, cr); err != nil {
		return err
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != meta.SHA256 || cr.n != meta.Size {
		return fmt.Errorf("%w (got %d bytes sha256 %.12s…, want %d bytes %.12s…)", errChecksum, cr.n, sum, meta.Size, meta.SHA256)
	}
	return applyErr
}

// goneWatch tells when a node has been missing from the registry for
// longer than offline_after (forgotten), checking at most every 2s. One
// missing listing is not enough: an entry being replaced can be missed.
type goneWatch struct {
	e           *Engine
	node        string
	since, last time.Time
}

func (g *goneWatch) gone(ctx context.Context) bool {
	if time.Since(g.last) < 2*time.Second {
		return false
	}
	g.last = time.Now()
	nodes, err := g.e.tr.Nodes(ctx)
	if err != nil {
		return false
	}
	for _, n := range nodes {
		if n.ID == g.node {
			g.since = time.Time{}
			return false
		}
	}
	if g.since.IsZero() {
		g.since = g.last
	}
	return time.Since(g.since) > g.e.cfg.OfflineAfter
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// acceptWrite checks this node's accept_writes_from policy for a job
// initiated by initiator.
func (e *Engine) acceptWrite(ctx context.Context, initiator string) error {
	info := api.NodeInfo{ID: initiator}
	if nodes, err := e.tr.Nodes(ctx); err == nil {
		for _, n := range nodes {
			if n.ID == initiator {
				info = n.NodeInfo
			}
		}
	}
	if !e.info.AcceptsWritesFrom(&info) {
		return fmt.Errorf("%w: %s does not accept writes from %s", api.ErrNotAllowed, e.Self(), initiator)
	}
	return nil
}

// staleStagingAge is how old the staging of a job the share no longer
// knows must be before it is swept (a margin, not a rule: such a job was
// pruned at least a minute after it finished).
var staleStagingAge = 10 * time.Minute

// sweepStaging removes staging (<root>/.fkbr-tmp/<job>) that no receive
// will ever use again: the job is gone from the share, this node was
// detached from it, or this node's part of it has ended. A receive only
// cleans its own staging when it runs, so a target that was down while
// it was detached, or forgotten, kept it for good (robust test 2, F3).
func (e *Engine) sweepStaging(ctx context.Context) {
	for _, r := range e.fs.Roots() {
		if !r.Writable {
			continue
		}
		root, err := e.fs.WritableRoot(r.ID)
		if err != nil {
			continue
		}
		f, err := root.Open(receive.StagingDir)
		if err != nil {
			continue
		}
		des, _ := f.ReadDir(-1)
		f.Close()
		for _, de := range des {
			id := de.Name()
			if !de.IsDir() || len(id) != 24 || ctx.Err() != nil {
				continue
			}
			e.jobsMu.Lock()
			busy := e.running[transport.TypeReceive+"/"+id]
			e.jobsMu.Unlock()
			if busy {
				continue
			}
			why := ""
			j, err := e.st.GetJob(ctx, id)
			switch {
			case errors.Is(err, api.ErrUnknownJob):
				if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > staleStagingAge {
					why = "job no longer on the share"
				}
			case err != nil:
			case j.IsDetached(e.Self()):
				why = "detached from the job"
			case roleState(j, api.RoleTarget, e.Self()).Terminal():
				why = "its part of the job has ended"
			}
			if why == "" {
				continue
			}
			p := path.Join(receive.StagingDir, id)
			if err := root.RemoveAll(p); err != nil {
				e.log.Warn("could not remove old staging", "root", r.ID, "job", id, "err", err)
				continue
			}
			root.Remove(receive.StagingDir) // only when it is empty
			e.log.Info("removed old staging", "root", r.ID, "job", id, "why", why)
		}
	}
}
