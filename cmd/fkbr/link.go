package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api/local"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
	"fkbr/internal/sshlink"
	"fkbr/internal/sshlink/frame"

	"golang.org/x/sys/unix"
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
	conn, err := linkStdio()
	if err != nil {
		return err
	}
	r, err := sshlink.Dial(ctx, conn, cfg.NodeID, buildCommit(), cfg.NFSRoot, frame.Options{}, log)
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
		return stopWithin(n.stop, linkStopWait)
	case <-n.exited:
		n.c.Close()
		return n.err
	}
	return n.stop()
}

// pollable moves fd (the link's stdin or stdout) to a descriptor of its
// own in non-blocking mode, so Go's poller runs it and a Close ends a Read
// or Write in progress. sshd's pipes are blocking: there a write on a pipe
// the dead home's sshd no longer reads stayed in the kernel for good, and
// so did the link (power cut of the home, 2026-11-04). fd itself is
// pointed at /dev/null: nothing else writing there can reach the home
// node, or a file that later gets that number.
func pollable(fd int, name string) (*os.File, error) {
	own, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		err = unix.Dup3(int(null.Fd()), fd, 0)
		null.Close()
	}
	if err == nil {
		err = unix.SetNonblock(own, true)
	}
	if err != nil {
		unix.Close(own)
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return os.NewFile(uintptr(own), name), nil
}

// stdio is this process's stdin and stdout as one connection.
type stdio struct{ in, out *os.File }

func linkStdio() (stdio, error) {
	in, err := pollable(0, "stdin")
	if err != nil {
		return stdio{}, err
	}
	out, err := pollable(1, "stdout")
	if err != nil {
		in.Close()
		return stdio{}, err
	}
	return stdio{in, out}, nil
}

func (s stdio) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s stdio) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s stdio) Close() error {
	s.in.Close()
	return s.out.Close()
}

// linkStopWait bounds a link's stop once its home node is gone.
const linkStopWait = time.Minute

// stopWithin waits up to d for stop. A link whose home is gone has nothing
// left to finish there (its records are the home's), and one stuck in
// stopping would keep its lock, so the home could not link this host
// again until someone killed it. Giving up ends the process, as a kill
// -9 would: transfers resume from the home's records.
func stopWithin(stop func() error, d time.Duration) error {
	res := make(chan error, 1)
	go func() { res <- stop() }()
	select {
	case err := <-res:
		return err
	case <-time.After(d):
		return fmt.Errorf("still stopping %s after the link ended: giving up", d)
	}
}
