package engine

import (
	"context"
	"errors"
	"log/slog"
	"syscall"
	"time"
)

// Soft NFS mounts return EIO or ETIMEDOUT (ESTALE after a server restart)
// while the server is unreachable, instead of waiting for it. Transfers
// ride that out: the failing step waits and is tried again, for up to
// transientBudget of consecutive errors. Short requests (listings and the
// like) still fail at once; the user can simply retry those.
var (
	transientWait   = 5 * time.Second
	transientBudget = 3 * time.Minute
)

func isTransient(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.ESTALE)
}

// rideOut tracks one job's run of consecutive transient share errors.
type rideOut struct {
	log   *slog.Logger
	since time.Time
}

// retry reports whether err is worth another try, after waiting; false
// means give up and return err.
func (r *rideOut) retry(ctx context.Context, err error) bool {
	if !isTransient(err) {
		return false
	}
	now := time.Now()
	if r.since.IsZero() {
		r.since = now
		r.log.Warn("share I/O error; waiting and retrying", "err", err, "for_up_to", transientBudget)
	}
	if now.Sub(r.since) > transientBudget {
		r.log.Error("share I/O errors for too long; giving up", "err", err, "after", now.Sub(r.since).Round(time.Second))
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(transientWait):
	}
	return true
}

// ok ends a run of errors.
func (r *rideOut) ok() {
	if !r.since.IsZero() {
		r.log.Info("share I/O recovered", "after", time.Since(r.since).Round(time.Second))
		r.since = time.Time{}
	}
}
