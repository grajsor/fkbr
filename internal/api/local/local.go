// Package local builds an in-process api.Client: the engine wired to the
// real local filesystem and the NFS mailbox.
package local

import (
	"log/slog"

	"fkbr/internal/config"
	"fkbr/internal/engine"
	"fkbr/internal/localfs"
	"fkbr/internal/transport/nfsmailbox"
)

// Client is an api.Client backed by an engine in this process. Run (from
// the embedded engine) turns it into the node's worker as well.
type Client struct {
	*engine.Engine
}

func Open(cfg *config.Config, version string, log *slog.Logger) (*Client, error) {
	fsys, err := localfs.Open(cfg.Roots)
	if err != nil {
		return nil, err
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
	return &Client{engine.New(cfg, fsys, mb, mb, engine.Options{Version: version, Logger: log})}, nil
}
