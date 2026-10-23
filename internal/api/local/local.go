// Package local builds an in-process api.Client: the engine wired to the
// real local filesystem and the node's share (on NFS, or the home node's
// private directory of an ssh swarm), or to its link to an ssh swarm's
// home node.
package local

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"

	"fkbr/internal/api"
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
	cfgPath  string // where linked hosts are kept (config.WritePath)
	// sockMode lets others in: they get no say in which hosts this node
	// sshes to with this user's keys.
	sockMode config.FileMode
}

// linksShut refuses linking at runtime on a node others reach.
func (c *Client) linksShut() error {
	if c.sockMode&0o077 != 0 {
		return fmt.Errorf("%w: socket_mode %o lets other users reach this node, so it links only the hosts in its config file's ssh list",
			api.ErrNotAllowed, c.sockMode)
	}
	return nil
}

func Open(cfg *config.Config, version string, log *slog.Logger) (*Client, error) {
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		return nil, err
	}
	if cfg.Home || len(cfg.SSH) > 0 {
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
		home:   home, hosts: hosts, sessions: sshlink.SessionOptions{Bin: cfg.SSHBin, Args: cfg.LinkArgs()}, cfgPath: cfg.ConfigPath,
		sockMode: cfg.SocketMode,
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

var errNotHome = fmt.Errorf("%w: this node runs on a share; ssh hosts are linked by a node started without one, or with ssh:", api.ErrNotAllowed)

var _ api.Linker = (*Client)(nil)

// Links is the state of an ssh swarm's sessions.
func (c *Client) Links(context.Context) ([]api.Link, error) {
	if c.home == nil {
		return nil, errNotHome
	}
	saved, _ := config.SavedHosts(c.cfgPath)
	var out []api.Link
	for _, l := range c.home.Links() {
		out = append(out, api.Link{Host: l.Host, Node: l.Node, State: l.State, Error: l.Error, Saved: slices.Contains(saved, l.Host)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

// AddLink links one host (as on the command line: web01:/srv/www) while
// the node runs, and with save keeps it in the config file's ssh list.
func (c *Client) AddLink(ctx context.Context, spec string, save bool) error {
	if c.home == nil {
		return errNotHome
	}
	if err := c.linksShut(); err != nil {
		return err
	}
	hs, err := sshlink.ParseHosts(spec)
	if err != nil {
		return fmt.Errorf("%w: %w", api.ErrBadPath, err)
	}
	if len(hs) != 1 {
		return fmt.Errorf("%w: one host at a time", api.ErrBadPath)
	}
	err = c.home.Add(ctx, hs[0])
	if err != nil && !(save && errors.Is(err, sshlink.ErrLinked)) {
		return err // linked already but not saved (ssh: on the command line): save it
	}
	if save {
		if err := config.SaveHost(c.cfgPath, hs[0].String()); err != nil {
			return fmt.Errorf("linked, but not saved: %w", err)
		}
	}
	return nil
}

// RemoveLink stops linking host (its node leaves the swarm) and removes it
// from the config file's ssh list.
func (c *Client) RemoveLink(_ context.Context, host string) error {
	if c.home == nil {
		return errNotHome
	}
	if err := c.linksShut(); err != nil {
		return err
	}
	saved, _ := config.SavedHosts(c.cfgPath)
	err := c.home.Remove(host)
	if slices.Contains(saved, host) {
		if ferr := config.ForgetHost(c.cfgPath, host); ferr != nil {
			return errors.Join(err, ferr)
		}
		return nil // a saved host that was not linked: forgotten
	}
	return err
}

// LinkStates is the state of an ssh swarm's sessions (tests).
func (c *Client) LinkStates() []sshlink.LinkState {
	if c.home == nil {
		return nil
	}
	return c.home.Links()
}
