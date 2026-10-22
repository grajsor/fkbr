// Package local builds an in-process api.Client: the engine wired to the
// real local filesystem and the node's share (on NFS, or the home node's
// private directory of an ssh swarm), or to its link to an ssh swarm's
// home node.
package local

import (
	"context"
	"log/slog"
	"sync"

	"fkbr/internal/config"
	"fkbr/internal/engine"
	"fkbr/internal/localfs"
	"fkbr/internal/sshlink"
	"fkbr/internal/store"
	"fkbr/internal/transport/nfsmailbox"
)

// Client is an api.Client backed by an engine in this process. Run (from
// the embedded engine) turns it into the node's worker as well.
type Client struct {
	*engine.Engine
	// The home node of an ssh swarm runs its sessions while it runs.
	home     *sshlink.Home
	hosts    []sshlink.Host
	sessions sshlink.SessionOptions
}

func Open(cfg *config.Config, version string, log *slog.Logger) (*Client, error) {
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		return nil, err
	}
	if len(cfg.SSH) > 0 {
		c, err := openHome(cfg, fsys, version, log)
		if err != nil {
			fsys.Close()
		}
		return c, err
	}
	refresh := cfg.RefreshNFS()
	if refresh && log != nil {
		log.Info("share caches NFS attributes; refreshing directories before reads", "share", cfg.NFSRoot)
	}
	mb, err := nfsmailbox.Open(cfg.NFSRoot, cfg.NodeID, nfsmailbox.Options{
		Poll:         cfg.Poll.Active,
		OfflineAfter: cfg.OfflineAfter,
		Refresh:      refresh,
	})
	if err != nil {
		fsys.Close()
		return nil, err
	}
	return &Client{Engine: engine.New(cfg, fsys, mb, mb, engine.Options{Version: version, Logger: log})}, nil
}

// openHome makes this node the home node of an ssh swarm (docs/ssh.md).
func openHome(cfg *config.Config, fsys localfs.FS, version string, log *slog.Logger) (*Client, error) {
	var hosts []sshlink.Host
	for _, s := range cfg.SSH {
		hs, err := sshlink.ParseHosts(s)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, hs...)
	}
	if err := localfs.PrivateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	if err := localfs.PrivateDir(cfg.NFSRoot); err != nil {
		return nil, err
	}
	home := sshlink.NewHome(cfg.NFSRoot, cfg.NodeID, store.Options{Poll: cfg.Poll.Active, OfflineAfter: cfg.OfflineAfter}, log)
	n, err := home.Open()
	if err != nil {
		return nil, err
	}
	return &Client{
		Engine: engine.New(cfg, fsys, n, n, engine.Options{Version: version, Logger: log}),
		home:   home, hosts: hosts, sessions: sshlink.SessionOptions{Bin: cfg.SSHBin},
	}, nil
}

// OpenLink builds the client of a node linked to an ssh swarm's home node
// by r (see `fkbr link`).
func OpenLink(cfg *config.Config, r *sshlink.Remote, version string, log *slog.Logger) (*Client, error) {
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		return nil, err
	}
	return &Client{Engine: engine.New(cfg, fsys, r, r, engine.Options{Version: version, Logger: log})}, nil
}

// SetSessions changes how an ssh swarm's sessions are started (tests).
func (c *Client) SetSessions(o sshlink.SessionOptions) { c.sessions = o }

// Run is the engine's worker; on an ssh swarm's home node it also keeps
// the sessions to the other hosts until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	if c.home == nil {
		return c.Engine.Run(ctx)
	}
	var wg sync.WaitGroup
	sctx, stop := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-c.Ready():
			c.home.Run(sctx, c.hosts, c.sessions)
		case <-sctx.Done():
		}
	}()
	err := c.Engine.Run(ctx)
	stop()
	wg.Wait()
	return err
}

// Links is the state of an ssh swarm's sessions (nil elsewhere).
func (c *Client) Links() []sshlink.LinkState {
	if c.home == nil {
		return nil
	}
	return c.home.Links()
}
