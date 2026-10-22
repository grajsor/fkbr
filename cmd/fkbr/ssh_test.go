package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/api/local"
	"fkbr/internal/config"
	"fkbr/internal/sshlink"
)

// With FKBR_TEST_MAIN=1 the test binary is fkbr itself: the stand-in ssh
// below runs it as `fkbr link` on the "remote" hosts.
func TestMain(m *testing.M) {
	if os.Getenv("FKBR_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeSSH writes a stand-in for ssh: it skips ssh's options, prints a
// login banner, and runs the command here, naming the node after the host
// and keeping its state under $FKBR_TEST_STATE.
func fakeSSH(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do
	case "$1" in
	--) shift; break ;;
	-o) shift 2 ;;
	*) shift ;;
	esac
done
dest=$1
cmd=$(printf '%s' "$2" | sed "s|'link'\$|'-node' '$dest' '-state' '$FKBR_TEST_STATE/$dest' 'link'|")
echo "Welcome to $dest"
exec sh -c "$cmd"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitJobDone(t *testing.T, c api.Client, id string) {
	t.Helper()
	var j *api.Job
	waitFor(t, "job "+id, 30*time.Second, func() bool {
		var err error
		j, err = c.Job(context.Background(), id)
		return err == nil && j.State.Terminal()
	})
	if j.State != api.JobDone {
		t.Fatalf("job %s: %s %+v", id, j.State, j.Participants)
	}
}

func sameFile(t *testing.T, a, b string) {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil || !bytes.Equal(x, y) {
		t.Errorf("%s and %s differ (%v, %v)", a, b, err1, err2)
	}
}

// A home node and two hosts linked over (stand-in) ssh: copies both ways
// and between the two remote nodes, a sync, and a clean stop in which the
// remote nodes leave.
func TestSSHSwarm(t *testing.T) {
	base := t.TempDir()
	dirs := map[string]string{}
	for _, n := range []string{"home", "hosta", "hostb", "state"} {
		dirs[n] = filepath.Join(base, n)
		if err := os.Mkdir(dirs[n], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FKBR_TEST_MAIN", "1")
	t.Setenv("FKBR_TEST_STATE", dirs["state"])
	cfg := &config.Config{
		NodeID:   "home",
		StateDir: filepath.Join(dirs["state"], "home"),
		SSH:      []string{"hosta:" + dirs["hosta"] + ",hostb:" + dirs["hostb"]},
		Roots:    []config.Root{{ID: "files", Path: dirs["home"], AllowWrite: true}},
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(newSyncWriter(&logs), nil))
	c, err := local.Open(cfg, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessions(sshlink.SessionOptions{SSH: []string{fakeSSH(t)}, Bin: os.Args[0], StopWait: 10 * time.Second})
	ctx := context.Background()
	n, err := startNodeWith(ctx, cfg, log, true, c)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			n.stop()
		}
	}()
	online := func(id string) bool {
		ns, _ := c.Nodes(ctx)
		for _, nd := range ns {
			if nd.ID == id && nd.Online {
				return true
			}
		}
		return false
	}
	waitFor(t, "both hosts online", 30*time.Second, func() bool { return online("hosta") && online("hostb") })
	ns, _ := c.Nodes(ctx)
	for _, nd := range ns {
		if nd.ID == "hosta" && !strings.Contains(nd.Link, "connected") {
			t.Errorf("hosta link: %q", nd.Link)
		}
	}

	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB
	os.MkdirAll(filepath.Join(dirs["hosta"], "tree/sub"), 0o755)
	os.WriteFile(filepath.Join(dirs["hosta"], "tree/big"), big, 0o644)
	for i := range 20 {
		os.WriteFile(filepath.Join(dirs["hosta"], "tree/sub", fmt.Sprint("f", i)), []byte(fmt.Sprint("file ", i)), 0o644)
	}
	os.WriteFile(filepath.Join(dirs["home"], "report"), []byte("from home"), 0o644)

	// Remote to remote: relayed by the home node.
	id, err := c.Submit(ctx, api.TransferRequest{SrcNode: "hosta", SrcRoot: "files", Paths: []string{"tree"}, DstNodes: []string{"hostb"}, DstRoot: "files"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, c, id)
	sameFile(t, filepath.Join(dirs["hosta"], "tree/big"), filepath.Join(dirs["hostb"], "tree/big"))
	sameFile(t, filepath.Join(dirs["hosta"], "tree/sub/f7"), filepath.Join(dirs["hostb"], "tree/sub/f7"))

	// Home to both, and remote to home.
	id, err = c.Submit(ctx, api.TransferRequest{SrcRoot: "files", Paths: []string{"report"}, DstNodes: []string{"@all"}, DstRoot: "files"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, c, id)
	sameFile(t, filepath.Join(dirs["home"], "report"), filepath.Join(dirs["hosta"], "report"))
	sameFile(t, filepath.Join(dirs["home"], "report"), filepath.Join(dirs["hostb"], "report"))
	id, err = c.Submit(ctx, api.TransferRequest{SrcNode: "hostb", SrcRoot: "files", Paths: []string{"tree"}, DstNodes: []string{"home"}, DstRoot: "files", DstDir: "back"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobDone(t, c, id)
	sameFile(t, filepath.Join(dirs["hosta"], "tree/big"), filepath.Join(dirs["home"], "back/tree/big"))

	// A mirror sync from hosta to hostb, deleting an extra file.
	os.WriteFile(filepath.Join(dirs["hostb"], "tree/extra"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dirs["hosta"], "tree/new"), []byte("new"), 0o644)
	plan, err := c.PlanSync(ctx, api.SyncRequest{SrcNode: "hosta", SrcRoot: "files", SrcPath: "tree", DstNodes: []string{"hostb"}, DstRoot: "files", DstPath: "tree", Mode: api.SyncMirror})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := c.ApplySync(ctx, plan.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range applied.Jobs {
		waitJobDone(t, c, j)
	}
	sameFile(t, filepath.Join(dirs["hosta"], "tree/new"), filepath.Join(dirs["hostb"], "tree/new"))
	if _, err := os.Stat(filepath.Join(dirs["hostb"], "tree/extra")); !os.IsNotExist(err) {
		t.Errorf("mirror left tree/extra: %v", err)
	}

	// A clean stop: the remote nodes leave the swarm while linked.
	stopped = true
	if err := n.stop(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"hosta", "hostb"} {
		if _, err := os.Stat(filepath.Join(cfg.NFSRoot, "nodes", id)); !os.IsNotExist(err) {
			t.Errorf("%s still registered after the home node stopped: %v", id, err)
		}
	}
	if !strings.Contains(logs.String(), "Welcome to hosta") {
		t.Errorf("login banner not logged:\n%s", logs.String())
	}
}

// A remote target killed mid-transfer: the home node starts its session
// again and the job resumes there, installing everything once.
func TestSSHTargetKilledMidTransfer(t *testing.T) {
	base := t.TempDir()
	dirs := map[string]string{}
	for _, n := range []string{"home", "hosta", "state"} {
		dirs[n] = filepath.Join(base, n)
		if err := os.Mkdir(dirs[n], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FKBR_TEST_MAIN", "1")
	t.Setenv("FKBR_TEST_STATE", dirs["state"])
	cfg := &config.Config{
		NodeID:   "home",
		StateDir: filepath.Join(dirs["state"], "home"),
		SSH:      []string{"hosta:" + dirs["hosta"]},
		Roots:    []config.Root{{ID: "files", Path: dirs["home"], AllowWrite: true}},
		Transfer: config.Transfer{ChunkSize: 1 << 20, JobQuota: 4 << 20},
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	c, err := local.Open(cfg, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessions(sshlink.SessionOptions{SSH: []string{fakeSSH(t)}, Bin: os.Args[0], StopWait: 10 * time.Second})
	ctx := context.Background()
	n, err := startNodeWith(ctx, cfg, slog.New(slog.DiscardHandler), true, c)
	if err != nil {
		t.Fatal(err)
	}
	defer n.stop()
	pid := func() int {
		ns, _ := c.Nodes(ctx)
		for _, nd := range ns {
			if nd.ID == "hosta" && nd.Online {
				return nd.PID
			}
		}
		return 0
	}
	waitFor(t, "hosta online", 30*time.Second, func() bool { return pid() != 0 })
	first := pid()
	data := make([]byte, 40<<20)
	for i := range data {
		data[i] = byte(i * 7 / 3)
	}
	os.WriteFile(filepath.Join(dirs["home"], "big"), data, 0o644)
	id, err := c.Submit(ctx, api.TransferRequest{SrcRoot: "files", Paths: []string{"big"}, DstNodes: []string{"hosta"}, DstRoot: "files"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a few chunks on hosta", 30*time.Second, func() bool {
		j, err := c.Job(ctx, id)
		if err != nil {
			return false
		}
		for _, p := range j.Participants {
			if p.Node == "hosta" && p.Chunks >= 3 {
				return true
			}
		}
		return false
	})
	if p, _ := os.FindProcess(first); p != nil {
		p.Kill()
	}
	waitFor(t, "hosta back", 30*time.Second, func() bool { p := pid(); return p != 0 && p != first })
	waitJobDone(t, c, id)
	sameFile(t, filepath.Join(dirs["home"], "big"), filepath.Join(dirs["hosta"], "big"))
}

// A host whose fkbr cannot start: the link state says why, and the home
// node keeps trying.
func TestSSHHostWithoutFkbr(t *testing.T) {
	base := t.TempDir()
	t.Setenv("FKBR_TEST_STATE", base)
	cfg := &config.Config{
		NodeID:   "home",
		StateDir: filepath.Join(base, "home"),
		SSH:      []string{"nowhere"},
		Roots:    []config.Root{{ID: "files", Path: base, AllowWrite: true}},
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	c, err := local.Open(cfg, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessions(sshlink.SessionOptions{SSH: []string{fakeSSH(t)}, Bin: "fkbr-not-installed", StopWait: time.Second})
	n, err := startNodeWith(context.Background(), cfg, slog.New(slog.DiscardHandler), true, c)
	if err != nil {
		t.Fatal(err)
	}
	defer n.stop()
	waitFor(t, "the link error", 20*time.Second, func() bool {
		for _, l := range c.LinkStates() {
			if strings.Contains(l.Error, "not found") && strings.HasPrefix(l.State, "retrying") {
				return true
			}
		}
		return false
	})
}

type syncWriter struct {
	mu chan struct{}
	w  *bytes.Buffer
}

func newSyncWriter(w *bytes.Buffer) *syncWriter { return &syncWriter{mu: make(chan struct{}, 1), w: w} }

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	return s.w.Write(p)
}

// No share: the home node starts alone; a host linked while it runs is
// saved in the config file and linked again at the next start; unlinked,
// its node leaves and the file forgets it.
func TestSSHHostsLinkedAtRuntime(t *testing.T) {
	base := t.TempDir()
	for _, n := range []string{"home", "hosta", "state", "userhome"} {
		os.Mkdir(filepath.Join(base, n), 0o755)
	}
	t.Setenv("FKBR_TEST_MAIN", "1")
	t.Setenv("FKBR_TEST_STATE", filepath.Join(base, "state"))
	t.Setenv("HOME", filepath.Join(base, "userhome"))
	conf := filepath.Join(base, "userhome", ".fkbr.conf")
	ssh := fakeSSH(t)
	start := func(saved []string) (*node, *local.Client) {
		cfg := &config.Config{
			NodeID:     "home",
			Home:       true,
			SSH:        saved,
			ConfigPath: conf,
			StateDir:   filepath.Join(base, "state", "home"),
			Roots:      []config.Root{{ID: "files", Path: filepath.Join(base, "home"), AllowWrite: true}},
		}
		if err := cfg.Finish(); err != nil {
			t.Fatal(err)
		}
		c, err := local.Open(cfg, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		c.SetSessions(sshlink.SessionOptions{SSH: []string{ssh}, Bin: os.Args[0], StopWait: 10 * time.Second})
		n, err := startNodeWith(context.Background(), cfg, slog.New(slog.DiscardHandler), true, c)
		if err != nil {
			t.Fatal(err)
		}
		return n, c
	}
	ctx := context.Background()
	online := func(c *local.Client, id string) bool {
		ns, _ := c.Nodes(ctx)
		for _, nd := range ns {
			if nd.ID == id && nd.Online {
				return true
			}
		}
		return false
	}
	n, c := start(nil)
	if ls, err := c.Links(ctx); err != nil || len(ls) != 0 {
		t.Fatalf("links at start: %v %v", ls, err)
	}
	host := "hosta:" + filepath.Join(base, "hosta")
	if err := c.AddLink(ctx, host, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hosta online", 30*time.Second, func() bool { return online(c, "hosta") })
	saved, _ := config.SavedHosts(conf)
	if len(saved) != 1 || saved[0] != host {
		t.Fatalf("saved: %v", saved)
	}
	if ls, _ := c.Links(ctx); len(ls) != 1 || !ls[0].Saved || ls[0].Node != "hosta" {
		t.Errorf("links: %+v", ls)
	}
	if err := n.stop(); err != nil {
		t.Fatal(err)
	}

	// The next start links the saved host on its own.
	n, c = start(saved)
	waitFor(t, "hosta online again", 30*time.Second, func() bool { return online(c, "hosta") })
	if err := c.RemoveLink(ctx, host); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hosta gone", 20*time.Second, func() bool {
		ns, _ := c.Nodes(ctx)
		for _, nd := range ns {
			if nd.ID == "hosta" {
				return false
			}
		}
		return true
	})
	if saved, _ := config.SavedHosts(conf); len(saved) != 0 {
		t.Errorf("still saved: %v", saved)
	}
	n.stop()
}
