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
	"slices"

	"fkbr/internal/api"
	"fkbr/internal/bundle"
	"fkbr/internal/product"
	"fkbr/internal/receive"
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
	rcv, err := receive.New(root, id, req.DstDir, receive.Options{
		Conflict:   req.Conflict,
		Chown:      e.info.ChownCapable,
		ChownDirs:  e.info.ChownCapable && e.info.DACOverride,
		AllowSetID: e.cfg.Transfer.AllowSetID,
	})
	if err != nil {
		return status.finish(ctx, api.JobFailed, err)
	}

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
	res, err := rcv.Commit()
	if err != nil {
		return status.finish(ctx, api.JobFailed, fmt.Errorf("install: %w", err))
	}
	st := &status.st
	st.Files = res.Files + res.Symlinks + res.Dirs
	st.Bytes = res.Bytes
	st.Skipped = res.Skipped
	st.Errors += res.Errors
	for _, p := range res.Problems {
		if len(st.Problems) < 200 {
			st.Problems = append(st.Problems, p)
		}
	}
	if res.OwnerNotPreserved > 0 {
		st.Problems = append(st.Problems, fmt.Sprintf("ownership not preserved on %d entries (needs root or CAP_CHOWN)", res.OwnerNotPreserved))
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
	status.st.Phase = "receiving"
	status.put(ctx, true)
	for {
		raw, err := e.st.ChunkMeta(ctx, src, id, next)
		if errors.Is(err, fs.ErrNotExist) {
			j, err := e.st.GetJob(ctx, id)
			if err != nil {
				return err
			}
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
				// Done without publishing chunk next: nothing more is coming.
				if j.TotalChunks == 0 || next >= j.TotalChunks {
					return nil
				}
			}
			if err := e.sleep(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var meta bundle.ChunkMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("chunk %d meta: %w", next, err)
		}
		if err := e.applyChunk(ctx, src, id, &meta, rcv, status); err != nil {
			return fmt.Errorf("chunk %d: %w", next, err)
		}
		next++
		if err := e.st.SetAcked(ctx, src, id, e.Self(), next); err != nil {
			return err
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
	h := sha256.New()
	cr := &countingReader{r: io.TeeReader(rc, h)}
	applyErr := bundle.Read(cr, meta.Compressed, func(hd *bundle.Header, data io.Reader) error {
		status.st.Bytes += hd.Length
		if hd.Entry.Type != api.TypeFile || hd.Last() {
			status.st.Files++ // provisional; Commit sets the real counts
		}
		return rcv.Apply(hd, data)
	})
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
