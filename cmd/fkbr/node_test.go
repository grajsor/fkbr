package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/config"
	"fkbr/internal/engine"
)

func testCfg(t *testing.T, share string) *config.Config {
	t.Helper()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.Mkdir(data, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		NodeID:     "n1",
		NFSRoot:    share,
		StateDir:   filepath.Join(base, "st"),
		NFSRefresh: "never",
		Roots:      []config.Root{{ID: "files", Path: data, AllowWrite: true}},
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A second node with the same ID on this host (another user, another
// working directory) must fail before the TUI draws: it used to run on
// without a worker, its copies served by the other node's directory.
func TestStartNodeRefusesTakenID(t *testing.T) {
	share := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := startNode(context.Background(), testCfg(t, share), log, false)
	if err != nil {
		t.Fatal(err)
	}
	defer first.stop()
	second, err := startNode(context.Background(), testCfg(t, share), log, false)
	if !errors.Is(err, engine.ErrNodeIDTaken) {
		if second != nil {
			second.stop()
		}
		t.Fatalf("second node: %v", err)
	}
}

// A TUI's node leaves the share when it stops. LeaveOnStop must be set
// before the worker starts: it decides at start whether it is a visitor.
func TestTUINodeLeavesOnStop(t *testing.T) {
	share := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n, err := startNode(context.Background(), testCfg(t, share), log, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "nodes", "n1")); err != nil {
		t.Fatalf("not registered: %v", err)
	}
	if err := n.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(share, "nodes", "n1")); !os.IsNotExist(err) {
		t.Fatalf("TUI node still registered after stop: %v", err)
	}
}
