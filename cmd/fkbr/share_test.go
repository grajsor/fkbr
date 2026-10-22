package main

import (
	"slices"
	"testing"

	"fkbr/internal/config"
)

// The share given on the command line wins over the config file's,
// whichever kind each is; $FKBR_SHARE only fills a gap.
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
		{"file ssh over env", "", []string{"web01"}, "", "", "/mnt/env", "", []string{"web01"}},
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
