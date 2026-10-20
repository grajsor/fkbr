package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A second worker for the same node ID on the same host (another user, or
// another -state dir) must be refused: it would share the inbox and write
// whatever it claims into its own directory.
func TestSecondWorkerOnSameHostRefused(t *testing.T) {
	share := t.TempDir()
	a, _ := node(t, share, "a")
	runWorker(t, a)
	twin := build(t, nodeCfg(t, share, "a")) // own state dir and root, same host
	err := twin.Run(context.Background())
	if !errors.Is(err, ErrNodeIDTaken) {
		t.Fatalf("second worker: %v", err)
	}
	if n, _ := registered(t, a, "a"); n.Instance != a.info.Instance {
		t.Fatalf("registry entry taken over: %+v", n)
	}
}

// A worker that crashed leaves a fresh heartbeat behind; a new one on the
// same host must not wait for it to go stale.
func TestRestartAfterCrashAllowed(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	ghost := build(t, cfg)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	info := ghost.info
	info.Instance, info.PID, info.Started = newToken(time.Now()), dead.Process.Pid, time.Now()
	if err := ghost.tr.Register(context.Background(), info); err != nil {
		t.Fatal(err)
	}
	ghost.tr.Heartbeat(context.Background(), info.Instance)
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	runWorker(t, build(t, cfg)) // fails the test if Run returns an error
}

// Two workers that slipped past the start check (started at the same
// moment): the one that started later stops; the other keeps the entry.
func TestHeartbeatConflict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		otherAge  time.Duration // the other worker started this long before a
		wantYield bool
	}{
		{"other started first", time.Hour, true},
		{"other started later", -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			share := t.TempDir()
			a, _ := node(t, share, "a")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			waitUntil(t, func() bool { n, ok := registered(t, a, "a"); return ok && n.Online }, "a online")
			other := newToken(time.Now().Add(-tc.otherAge))
			hb := filepath.Join(share, "nodes", "a", "heartbeat")
			if err := os.WriteFile(hb, []byte(other+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !tc.wantYield || !errors.Is(err, ErrNodeIDTaken) {
					t.Fatalf("Run returned %v", err)
				}
				if _, ok := registered(t, a, "a"); !ok {
					t.Fatal("yielding worker removed the registry entry")
				}
			case <-time.After(time.Second):
				if tc.wantYield {
					t.Fatal("worker that started later kept running")
				}
				n, _ := registered(t, a, "a")
				if n.Instance != a.info.Instance {
					t.Fatalf("entry not re-registered: %+v", n)
				}
			}
		})
	}
}
