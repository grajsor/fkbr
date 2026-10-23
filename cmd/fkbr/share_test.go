package main

import (
	"slices"
	"testing"

	"fkbr/internal/api"
	"fkbr/internal/config"
)

// The share given on the command line wins over the config file's,
// whichever kind each is; $FKBR_SHARE over the file's ssh list only (F7
// may have written that to ~/.fkbr.conf unasked).
func TestPickShare(t *testing.T) {
	for _, c := range []struct {
		name           string
		fileRoot       string
		fileSSH        []string
		arg, flag, env string
		wantRoot       string
		wantSSH        []string
	}{
		{"nfs arg over file ssh", "", []string{"web01"}, "/mnt/nfs/xfer", "", "", "/mnt/nfs/xfer", nil},
		{"nfs flag over file ssh", "", []string{"web01"}, "", "/mnt/nfs/xfer", "", "/mnt/nfs/xfer", nil},
		{"ssh arg over file nfs", "/mnt/nfs/xfer", nil, "ssh:db01", "", "", "", []string{"db01"}},
		{"env over file ssh", "", []string{"web01"}, "", "", "/mnt/env", "/mnt/env", nil},
		{"file ssh without env", "", []string{"web01"}, "", "", "", "", []string{"web01"}},
		{"file nfs over env", "/mnt/file", nil, "", "", "/mnt/env", "/mnt/file", nil},
		{"env when the file has none", "", nil, "", "", "/mnt/env", "/mnt/env", nil},
		{"env ssh", "", nil, "", "", "ssh:web01", "", []string{"web01"}},
	} {
		cfg := &config.Config{NFSRoot: c.fileRoot, SSH: c.fileSSH}
		if err := pickShare(cfg, c.arg, c.flag, c.env); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if cfg.NFSRoot != c.wantRoot || !slices.Equal(cfg.SSH, c.wantSSH) {
			t.Errorf("%s: share %q, ssh %q; want %q, %q", c.name, cfg.NFSRoot, cfg.SSH, c.wantRoot, c.wantSSH)
		}
	}
}

// Root "local" is the user's whole disk as that user: only for a TUI or
// CLI of this user, never a daemon or a node others reach by its socket.
func TestLocalRootOnlyForThisUser(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		mode config.FileMode
		want bool
	}{
		{"tui", 0, true},
		{"ls", 0o600, true},
		{"daemon", 0, false},
		{"tui", 0o660, false},
		{"tui", 0o606, false},
	} {
		cfg := &config.Config{SocketMode: c.mode}
		addLocalRoot(cfg, c.cmd)
		got := slices.ContainsFunc(cfg.Roots, func(r config.Root) bool { return r.Local })
		if got != c.want {
			t.Errorf("%s, socket %o: local root %v, want %v", c.cmd, c.mode, got, c.want)
		}
	}
	cfg := &config.Config{NFSRoot: t.TempDir(), NodeID: "a", SocketMode: 0o660,
		Roots: []config.Root{{ID: "files", Path: t.TempDir()}, {ID: "mine", Path: t.TempDir(), Local: true}}}
	if err := cfg.Finish(); err == nil {
		t.Error("a local root with a group socket was accepted")
	}
}

// A CLI attached to a daemon has root local and the daemon has not: no
// "using the running node X, which shares ..." note for that alone.
func TestSameRootsLeavesOutLocal(t *testing.T) {
	dir := t.TempDir()
	daemon := []api.Root{{ID: "files", Path: dir, Writable: true}}
	cli := []config.Root{{ID: "files", Path: dir, AllowWrite: true}, {ID: config.LocalRootID, Path: "/", AllowWrite: true, Local: true}}
	if !sameRoots(daemon, cli) {
		t.Error("root local made them differ")
	}
	if sameRoots(daemon, []config.Root{{ID: "files", Path: t.TempDir(), AllowWrite: true}}) {
		t.Error("another directory is the same")
	}
}
