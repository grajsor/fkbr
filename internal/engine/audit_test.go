package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/localfs"
)

// Security audit 2026-10-22, finding 2: a state dir someone else made
// first, or a lock file planted as a symlink, must not be used.
func TestWorkerLockRefusesPlantedStateDir(t *testing.T) {
	top := t.TempDir()
	victim := filepath.Join(top, "victim")
	os.WriteFile(victim, []byte("mine\n"), 0o600)

	open := filepath.Join(top, "open")
	os.Mkdir(open, 0o700)
	os.Chmod(open, 0o777)
	if _, _, err := lockWorker(open, slog.New(slog.DiscardHandler)); !errors.Is(err, localfs.ErrNotPrivate) {
		t.Errorf("dir others can write to: err = %v, want ErrNotPrivate", err)
	}

	planted := filepath.Join(top, "planted")
	os.Mkdir(planted, 0o700)
	os.Symlink(victim, filepath.Join(planted, "worker.lock"))
	if l, _, err := lockWorker(planted, slog.New(slog.DiscardHandler)); err == nil {
		l.state("running")
		t.Error("took a lock that is a symlink")
	}
	if b, _ := os.ReadFile(victim); string(b) != "mine\n" {
		t.Errorf("the symlink's target was written: %q", b)
	}
}

// Finding 4: a sync scan's probe (it writes a file) stays in the root.
func TestScanProbeStaysInRoot(t *testing.T) {
	share := t.TempDir()
	cfg := nodeCfg(t, share, "a")
	e := build(t, cfg)
	root := cfg.Roots[0].Path
	outside := t.TempDir()
	os.Mkdir(filepath.Join(root, "sub"), 0o755)
	os.Symlink(outside, filepath.Join(root, "away"))

	if _, err := e.scanLocal(context.Background(), scanRequest{PlanID: "p1", Root: "data", Path: "../x", Target: true}, nil); err == nil {
		t.Error("scan of ../x accepted")
	}
	for rel, want := range map[string]string{
		".":            root,
		"sub":          filepath.Join(root, "sub"),
		"away":         root, // a symlink out of the root
		"away/deeper":  root,
		"missing/here": root,
		"../x":         root,
	} {
		got := insideRoot(root, rel)
		if real, err := filepath.EvalSymlinks(want); err == nil {
			want = real
		}
		if real, err := filepath.EvalSymlinks(got); err == nil {
			got = real
		}
		if got != want {
			t.Errorf("insideRoot(%q) = %s, want %s", rel, got, want)
		}
	}
}
