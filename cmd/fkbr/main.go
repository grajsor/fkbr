// Command fkbr is a multi-node file commander whose nodes talk only through
// a shared NFS directory. It installs nothing: copy the binary to each host
// and run it. This file only wires subcommands to frontends.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"fkbr/internal/api"
	"fkbr/internal/api/local"
	"fkbr/internal/api/remote"
	"fkbr/internal/check"
	"fkbr/internal/config"
	"fkbr/internal/engine"
	"fkbr/internal/frontend/cli"
	"fkbr/internal/frontend/tui"
	"fkbr/internal/product"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, product.Name+":", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, product.Expand(`usage: {name} [flags] SHARE [command [args]]

Run it on every host with the same SHARE, a directory on the shared NFS
mount. Each host registers there as a node (named after its hostname) and
shares the directory you start it in, with everything below it. Nothing is
installed. Without a command, the TUI opens.

  cd /srv/data && {name} /mnt/nfs/xfer

Copy to one node, a list (n1,n2), or every other node (@all).

commands:
`))
	fmt.Fprintf(os.Stderr, "  %-8s %s\n", "tui", "dual-pane commander; also runs this node unless a daemon does")
	fmt.Fprintf(os.Stderr, "  %-8s %s\n", "daemon", "run this node headless (foreground; use nohup/tmux to keep it)")
	fmt.Fprintf(os.Stderr, "  %-8s %s\n", "check", "validate this node's setup: roots, NFS mount, share, registry")
	names := make([]string, 0, len(cli.Commands))
	for n := range cli.Commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-8s %s\n", n, cli.Commands[n])
	}
	fmt.Fprintf(os.Stderr, "  %-8s %s\n\nSHARE may also come from -share, $%sSHARE or a config file.\n\nflags:\n", "version", "print the version", product.EnvPrefix)
	flag.PrintDefaults()
}

type rootFlags []config.Root

func (r *rootFlags) String() string { return fmt.Sprint(*r) }
func (r *rootFlags) Set(s string) error {
	root, err := config.ParseRoot(s)
	if err == nil {
		*r = append(*r, root)
	}
	return err
}

func run() error {
	cfgPath := flag.String("config", "", product.Expand("config file (default ${ENV}CONFIG, else {name}.yaml next to the binary, else none)"))
	node := flag.String("node", "", "this node's ID (default: short hostname)")
	share := flag.String("share", "", "the shared directory (alternative to the SHARE argument)")
	readOnly := flag.Bool("ro", false, "share the current directory read-only")
	stateDir := flag.String("state", "", product.Expand("local runtime dir for lock and socket (default /tmp/{name}-<uid>-<node>)"))
	var roots rootFlags
	flag.Var(&roots, "root", "share these directories instead of the current one: id=/path or id=/path:ro (repeatable)")
	roles := flag.String("roles", "", "comma-separated role tags, e.g. master or slave")
	keep := flag.Duration("keep", 0, "how long finished jobs, sync plans and failed requests stay on the share (default 1h)")
	nfsRefresh := flag.String("nfs-refresh", "", "compensate for NFS attribute caching: auto (default), always, never")
	forceLocal := flag.Bool("local", false, "do not attach to a running node; run the engine in this process")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Usage = usage
	flag.Parse()
	args := flag.Args()
	if len(args) == 0 && *share == "" && os.Getenv(product.EnvPrefix+"SHARE") == "" && *cfgPath == "" && config.DefaultPath() == "" {
		usage()
		return fmt.Errorf("which shared directory? e.g. %s /mnt/nfs/xfer", product.Name)
	}
	shareArg := ""
	if len(args) > 0 && !isCommand(args[0]) {
		shareArg = args[0]
		// Flags may also follow SHARE: fkbr /mnt/xfer -node a cp ...
		if err := flag.CommandLine.Parse(args[1:]); err != nil {
			return err
		}
		args = flag.Args()
	}
	cmd := "tui"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "version" {
		fmt.Println(product.Name, version)
		return nil
	}
	if !isCommand(cmd) {
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}

	cfg := &config.Config{}
	if *cfgPath == "" {
		*cfgPath = config.DefaultPath()
	}
	if *cfgPath != "" {
		var err error
		if cfg, err = config.Read(*cfgPath); err != nil {
			return err
		}
	}
	// Arguments and flags override the file; then fall back to defaults:
	// node named after the host, sharing the current directory.
	switch {
	case shareArg != "":
		cfg.NFSRoot = shareArg
	case *share != "":
		cfg.NFSRoot = *share
	case cfg.NFSRoot == "":
		cfg.NFSRoot = os.Getenv(product.EnvPrefix + "SHARE")
	}
	if cfg.NFSRoot == "" {
		return fmt.Errorf("which shared directory? e.g. %s /mnt/nfs/xfer", product.Name)
	}
	abs, err := filepath.Abs(cfg.NFSRoot)
	if err != nil {
		return err
	}
	cfg.NFSRoot = abs
	if *node != "" {
		cfg.NodeID = *node
	}
	if cfg.NodeID == "" {
		if cfg.NodeID, err = config.DefaultNodeID(); err != nil {
			return err
		}
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir
	}
	if len(roots) > 0 {
		cfg.Roots = roots
	}
	if len(cfg.Roots) == 0 {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		r, err := config.DirRoot(wd, cfg.NFSRoot, !*readOnly)
		if err != nil {
			return err
		}
		cfg.Roots = []config.Root{r}
	}
	if *roles != "" {
		cfg.Roles = strings.Split(*roles, ",")
	}
	if *nfsRefresh != "" {
		cfg.NFSRefresh = *nfsRefresh
	}
	if *keep != 0 {
		cfg.KeepDone = *keep
	}
	if err := cfg.Finish(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "daemon":
		return daemon(ctx, cfg, log)
	case "check":
		return runCheck(ctx, cfg)
	case "tui":
		return runTUI(ctx, cfg, *forceLocal)
	}
	c, err := connect(ctx, cfg, *forceLocal, log)
	if err != nil {
		return err
	}
	defer c.Close()
	return cli.Run(ctx, c, cmd, args, os.Stdout)
}

func isCommand(s string) bool {
	if _, ok := cli.Commands[s]; ok {
		return true
	}
	switch s {
	case "tui", "daemon", "check", "version":
		return true
	}
	return false
}

// connect prefers the node's running daemon (or TUI) and falls back to an
// in-process engine, which can browse and submit but executes nothing:
// jobs run where a node's worker runs.
func connect(ctx context.Context, cfg *config.Config, forceLocal bool, log *slog.Logger) (api.Client, error) {
	if !forceLocal {
		if c, err := remote.Dial(ctx, cfg.Socket); err == nil {
			log.Debug("using running node", "socket", cfg.Socket)
			return c, nil
		}
	}
	return local.Open(cfg, version, log)
}

// node is this host's worker plus its API socket, running in the background.
type node struct {
	c      *local.Client
	cancel context.CancelFunc
	done   chan error
}

// startNode runs the worker and serves the socket until stop is called.
func startNode(ctx context.Context, cfg *config.Config, log *slog.Logger) (*node, error) {
	c, err := local.Open(cfg, version, log)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		c.Close()
		return nil, err
	}
	l, err := remote.Listen(cfg.Socket, os.FileMode(cfg.SocketMode))
	if err != nil {
		c.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	n := &node{c: c, cancel: cancel, done: make(chan error, 1)}
	apiErr := make(chan error, 1)
	go func() { apiErr <- remote.ServeSocket(ctx, l, cfg.Socket, os.FileMode(cfg.SocketMode), c, log) }()
	go func() {
		err := c.Run(ctx) // returns at once if another worker holds the lock
		cancel()
		if aerr := <-apiErr; err == nil {
			err = aerr
		}
		os.Remove(cfg.Socket)
		// Leave nothing behind locally once our lock is released; never
		// touch a lock another process holds.
		if !errors.Is(err, engine.ErrWorkerRunning) {
			os.Remove(filepath.Join(cfg.StateDir, "worker.lock"))
			os.Remove(cfg.StateDir) // only if empty
		}
		n.done <- err
	}()
	return n, nil
}

// stop ends the worker (jobs pause and resume on the next start) and waits.
func (n *node) stop() error {
	n.cancel()
	err := <-n.done
	n.c.Close()
	return err
}

func daemon(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	n, err := startNode(ctx, cfg, log)
	if err != nil {
		return err
	}
	log.Info("node running", "node", cfg.NodeID, "socket", cfg.Socket)
	select {
	case <-ctx.Done():
		return n.stop()
	case err := <-n.done:
		n.c.Close()
		return err
	}
}

// runTUI attaches to a running node, or else runs this node itself for as
// long as the TUI is open.
func runTUI(ctx context.Context, cfg *config.Config, forceLocal bool) error {
	if !forceLocal {
		if c, err := remote.Dial(ctx, cfg.Socket); err == nil {
			defer c.Close()
			return tui.Run(ctx, c, tui.Options{})
		}
	}
	quiet := slog.New(slog.DiscardHandler) // log lines would draw over the UI
	n, err := startNode(ctx, cfg, quiet)
	if err != nil {
		return err
	}
	// A node that only lives while the TUI is open leaves the share on quit.
	n.c.LeaveOnStop()
	terr := tui.Run(ctx, n.c, tui.Options{RunsNode: true})
	if err := n.stop(); err != nil && terr == nil {
		terr = err
	}
	return terr
}

// runCheck prints the environment checks; any FAIL makes it exit non-zero.
func runCheck(ctx context.Context, cfg *config.Config) error {
	mi, err := os.Open("/proc/self/mountinfo")
	if err == nil {
		defer mi.Close()
	}
	fails := 0
	for _, r := range check.Run(ctx, cfg, mi) {
		fmt.Printf("%-4s  %-11s %s\n", r.Level, r.Area, r.Msg)
		if r.Level == check.Fail {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("%d check(s) failed", fails)
	}
	return nil
}
