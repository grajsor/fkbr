package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"fkbr/internal/api/local"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
	"fkbr/internal/sshlink"
	"fkbr/internal/sshlink/frame"
)

type linkFlags struct {
	node, state string
	roots       rootFlags
	readOnly    bool
	roles       string
	verbose     bool
	// From the home node: what may be staged here.
	chunkSize, jobQuota, stagingQuota string
}

// runLink is `fkbr link`: this host's node in an ssh swarm (docs/ssh.md),
// started by the home node's ssh in the directory it shares, and linked to
// the home node over stdin and stdout until the home node stops it or the
// link ends. It logs to stderr, which ssh hands to the home node; nothing
// else may write to stdout.
func runLink(f linkFlags) error {
	cfg := &config.Config{Link: true, NodeID: f.node, StateDir: f.state}
	var err error
	if cfg.NodeID == "" {
		if cfg.NodeID, err = config.DefaultNodeID(); err != nil {
			return err
		}
	}
	if cfg.StateDir == "" {
		// Not the state dir of a node of this name on a share.
		cfg.StateDir = config.DefaultStateDir(cfg.NodeID) + "-link"
	}
	cfg.NFSRoot = filepath.Join(cfg.StateDir, "chunks")
	if len(f.roots) > 0 {
		cfg.Roots = f.roots
	} else {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		r, err := config.DirRoot(wd, cfg.NFSRoot, !f.readOnly)
		if err != nil {
			return err
		}
		cfg.Roots = []config.Root{r}
	}
	if f.roles != "" {
		cfg.Roles = strings.Split(f.roles, ",")
	}
	if err := cfg.SetTransfer(f.chunkSize, f.jobQuota, f.stagingQuota); err != nil {
		return err
	}
	if err := cfg.Finish(); err != nil {
		return err
	}
	level := slog.LevelInfo
	if f.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := localfs.PrivateDir(cfg.StateDir); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	r, err := sshlink.Dial(ctx, stdio{}, cfg.NodeID, buildCommit(), cfg.NFSRoot, frame.Options{}, log)
	if err != nil {
		return err
	}
	defer r.Close()
	c, err := local.OpenLink(cfg, r, buildCommit(), log)
	if err != nil {
		return err
	}
	// It lives while the home node does, like a TUI's node: it leaves
	// when stopped.
	n, err := startNodeWith(ctx, cfg, log, true, c)
	if err != nil {
		return err
	}
	log.Info("linked node running", "node", cfg.NodeID, "roots", len(cfg.Roots))
	select {
	case <-ctx.Done():
	case <-r.Stopping():
	case <-r.Done():
		log.Warn("link to the home node ended", "err", r.Err())
	case <-n.exited:
		n.c.Close()
		return n.err
	}
	return n.stop()
}

// stdio is this process's stdin and stdout as one connection.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error {
	os.Stdin.Close()
	return os.Stdout.Close()
}
