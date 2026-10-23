package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/config"
)

// A home node whose socket other users reach does not let them make it
// ssh somewhere with this user's keys, nor change its config file.
func TestNoLinksOverAnOpenSocket(t *testing.T) {
	base := t.TempDir()
	conf := filepath.Join(base, "fkbr.yaml")
	cfg := &config.Config{
		NodeID: "home", Home: true, SocketMode: 0o660, ConfigPath: conf,
		StateDir: filepath.Join(base, "state"),
		Roots:    []config.Root{{ID: "files", Path: base}},
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	c, err := Open(cfg, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.AddLink(context.Background(), "web01", true); !errors.Is(err, api.ErrNotAllowed) {
		t.Errorf("add: %v", err)
	}
	if err := c.RemoveLink(context.Background(), "web01"); !errors.Is(err, api.ErrNotAllowed) {
		t.Errorf("remove: %v", err)
	}
	if _, err := os.Stat(conf); !os.IsNotExist(err) {
		t.Errorf("config file written: %v", err)
	}
}
