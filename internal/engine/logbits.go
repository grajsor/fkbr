package engine

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Waits longer than this are logged, and then every waitLogEvery, so a
// hung transfer shows who waits for what without flooding normal ones.
var (
	waitLogAfter = 10 * time.Second
	waitLogEvery = time.Minute
)

// waitLog logs one wait once it gets long.
type waitLog struct {
	log         *slog.Logger
	msg         string
	after, each time.Duration
	start, next time.Time
	logged      bool
}

func newWaitLog(log *slog.Logger, msg string, after, each time.Duration) *waitLog {
	return &waitLog{log: log, msg: msg, after: after, each: each, start: time.Now()}
}

// tick is called on every poll while waiting; args describe the state.
func (w *waitLog) tick(args ...any) {
	now := time.Now()
	if now.Sub(w.start) < w.after || now.Before(w.next) {
		return
	}
	w.log.Info(w.msg, append(args, "for", now.Sub(w.start).Round(time.Second))...)
	w.logged, w.next = true, now.Add(w.each)
}

// done ends the wait, logging its length if it was long enough to log.
func (w *waitLog) done(args ...any) {
	if w.logged {
		w.log.Info(w.msg+": over", append(args, "after", time.Since(w.start).Round(time.Second))...)
	}
	w.logged = false
}

// size formats a byte count for logs.
func size(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	v, s := float64(n), ""
	for _, s = range []string{"KiB", "MiB", "GiB", "TiB"} {
		v /= unit
		if v < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f%s", v, s)
}

// rate formats bytes over a duration.
func rate(n int64, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return size(int64(float64(n)/d.Seconds())) + "/s"
}

// summary shortens a list for one log line.
func summary(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ",")
	}
	return strings.Join(items[:max], ",") + fmt.Sprintf(",… (%d)", len(items))
}
