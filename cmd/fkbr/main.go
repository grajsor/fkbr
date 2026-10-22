// Command fkbr is a multi-node file commander whose nodes talk only through
// a shared NFS directory. It installs nothing: copy the binary to each host
// and run it. This file only wires subcommands to frontends.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/api/local"
	"fkbr/internal/api/remote"
	"fkbr/internal/check"
	"fkbr/internal/config"
	"fkbr/internal/engine"
	"fkbr/internal/frontend/cli"
	"fkbr/internal/frontend/tui"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
	"fkbr/internal/safetext"
)

// commit is set at build time with -ldflags "-X main.commit=..." (make).
var commit = "dev"

// version is the release and commit, e.g. "v1.0 5cc1bcd".
var version = product.Version(buildCommit())

// buildCommit is the commit make stamped in, else the one Go records for
// a plain `go build` inside a git checkout, else the one git wrote into a
// zip or tar.gz download, else "dev".
func buildCommit() string {
	if commit != "dev" {
		return commit
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return archivedOr(commit)
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) < 7 {
		return archivedOr(commit)
	}
	if dirty {
		return rev[:7] + "-dirty"
	}
	return rev[:7]
}

func archivedOr(def string) string {
	if c := product.ArchivedCommit(); c != "" {
		return c
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, product.Name+":", safetext.Clean(err.Error()))
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
	colors := flag.String("colors", "auto", "TUI colours: auto, true, 256, 16 or none")
	mouse := flag.Bool("mouse", true, "TUI: clicks, right-click marks, wheel (-mouse=false leaves the mouse to the terminal)")
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
	global, args := globalFlags(flag.CommandLine, cmd, args)
	if err := flag.CommandLine.Parse(global); err != nil {
		return err
	}
	if _, ok := cli.Commands[cmd]; !ok && isCommand(cmd) && len(args) > 0 {
		return fmt.Errorf("%s takes no arguments: %q", cmd, strings.Join(args, " "))
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

	// A closed terminal (ssh dropped) stops the node cleanly too, so a TUI
	// leaves the share; under nohup, SIGHUP stays ignored.
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	ctx, stop := signal.NotifyContext(context.Background(), sigs...)
	defer stop()

	switch cmd {
	case "daemon":
		return daemon(ctx, cfg, log)
	case "check":
		return runCheck(ctx, cfg)
	case "tui":
		return runTUI(ctx, cfg, *forceLocal, tui.Options{Colors: *colors, Version: version, NoMouse: !*mouse}, level)
	}
	c, err := connect(ctx, cfg, *forceLocal, log)
	if err != nil {
		return err
	}
	defer c.Close()
	return cli.Run(ctx, c, cmd, args, stdout())
}

// stdout cleans what goes to a terminal of control characters (names and
// peer fields could carry escapes); piped output stays exact, as with ls.
func stdout() io.Writer {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return safetext.Writer(os.Stdout)
	}
	return os.Stdout
}

// commandFlags are command flags that share a name with a global flag; after
// that command, the command's own wins.
var commandFlags = map[string][]string{"prune": {"keep"}, "sync": {"v"}}

// globalFlags takes the global flags out of a command's arguments, so they
// work after the command too ("fkbr SHARE daemon -node a"). Everything from
// "--" on stays with the command.
func globalFlags(fs *flag.FlagSet, cmd string, args []string) (global, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return global, append(rest, args[i:]...)
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if len(a) < 2 || a[0] != '-' || f == nil || slices.Contains(commandFlags[cmd], name) {
			rest = append(rest, a)
			continue
		}
		global = append(global, a)
		b, isBool := f.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(isBool && b.IsBoolFlag()) && i+1 < len(args) {
			i++
			global = append(global, args[i])
		}
	}
	return global, rest
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
		c, notice, err := dialNode(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if c != nil {
			log.Debug("using running node", "socket", cfg.Socket)
			if notice != "" {
				fmt.Fprintln(os.Stderr, product.Name+": note:", safetext.Clean(notice))
			}
			return c, nil
		}
	}
	return local.Open(cfg, buildCommit(), log) // the registry shows just the commit
}

// dialNode attaches to this node's running daemon or TUI; nil if none
// answers. A node of this name that runs for another share is an error. One
// that shares other directories than this command would (started
// elsewhere, or with other -root/-ro) is used, with a notice saying so.
func dialNode(ctx context.Context, cfg *config.Config) (*remote.Client, string, error) {
	// A socket in a directory someone else controls may not be our node.
	if err := localfs.CheckPrivateDir(filepath.Dir(cfg.Socket)); errors.Is(err, localfs.ErrNotPrivate) {
		return nil, "", err
	}
	c, err := remote.Dial(ctx, cfg.Socket)
	if err != nil {
		return nil, "", nil
	}
	if s := c.Share(); s != "" && config.RealPath(s) != config.RealPath(cfg.NFSRoot) {
		c.Close()
		return nil, "", fmt.Errorf("%q already runs on this host for the share %s; pass -node with another name to use %s too",
			cfg.NodeID, s, cfg.NFSRoot)
	}
	if theirs := c.LocalRoots(); theirs != nil && !sameRoots(theirs, cfg.Roots) {
		var ds []string
		for _, r := range theirs {
			d := r.Path
			if !r.Writable {
				d += " (read-only)"
			}
			ds = append(ds, d)
		}
		return c, fmt.Sprintf("using the running node %s, which shares %s", c.Self(), strings.Join(ds, ", ")), nil
	}
	return c, "", nil
}

func sameRoots(a []api.Root, b []config.Root) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Writable != b[i].AllowWrite || config.RealPath(a[i].Path) != config.RealPath(b[i].Path) {
			return false
		}
	}
	return true
}

// node is this host's worker plus its API socket, running in the background.
type node struct {
	c      *local.Client
	cancel context.CancelFunc
	exited chan struct{} // closed when the worker has stopped; then err is set
	err    error
}

// startNode runs the worker and serves the socket until stop is called. It
// returns once the worker runs, or with the reason it could not start (the
// node ID is in use, say), so a TUI never runs on without its worker.
// leave makes the node leave the share when it stops (a TUI's node); the
// worker reads it as it starts.
func startNode(ctx context.Context, cfg *config.Config, log *slog.Logger, leave bool) (*node, error) {
	c, err := local.Open(cfg, buildCommit(), log)
	if err != nil {
		return nil, err
	}
	if leave {
		c.LeaveOnStop()
	}
	if err := localfs.PrivateDir(cfg.StateDir); err != nil {
		c.Close()
		return nil, err
	}
	l, err := remote.Listen(cfg.Socket, os.FileMode(cfg.SocketMode))
	if err != nil {
		c.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	n := &node{c: c, cancel: cancel, exited: make(chan struct{})}
	apiErr := make(chan error, 1)
	go func() { apiErr <- remote.ServeSocket(ctx, l, cfg.Socket, os.FileMode(cfg.SocketMode), c, log) }()
	go func() {
		err := c.Run(ctx) // returns at once if another worker holds the lock
		cancel()
		if aerr := <-apiErr; err == nil {
			err = aerr
		}
		// A worker started while this one stopped may listen there already.
		if sc, err := net.DialTimeout("unix", cfg.Socket, 200*time.Millisecond); err == nil {
			sc.Close()
		} else {
			os.Remove(cfg.Socket)
		}
		// Leave nothing behind locally once our lock is released; never
		// touch a lock another process holds.
		if !errors.Is(err, engine.ErrWorkerRunning) {
			os.Remove(filepath.Join(cfg.StateDir, "worker.lock"))
			os.Remove(cfg.StateDir) // only if empty
		}
		n.err = err
		close(n.exited)
	}()
	select {
	case <-c.Ready():
		return n, nil
	case <-n.exited:
		c.Close()
		if n.err == nil {
			n.err = ctx.Err()
		}
		return nil, n.err
	}
}

// stop ends the worker (jobs pause and resume on the next start) and waits.
func (n *node) stop() error {
	n.cancel()
	<-n.exited
	n.c.Close()
	return n.err
}

func daemon(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	n, err := startNode(ctx, cfg, log, false)
	if err != nil {
		return err
	}
	log.Info("node running", "node", cfg.NodeID, "socket", cfg.Socket)
	select {
	case <-ctx.Done():
		return n.stop()
	case <-n.exited:
		n.c.Close()
		return n.err
	}
}

// runTUI attaches to a running node, or else runs this node itself for as
// long as the TUI is open.
// opts carries the display settings; runTUI fills in the rest.
func runTUI(ctx context.Context, cfg *config.Config, forceLocal bool, opts tui.Options, level slog.Level) error {
	if !forceLocal {
		c, notice, err := dialNode(ctx, cfg)
		if err != nil {
			return err
		}
		if c != nil {
			defer c.Close()
			opts.Notice = notice
			return tui.Run(ctx, c, opts)
		}
	}
	// Log lines would draw over the UI, so the node logs to a file beside
	// its lock and socket; it stays after exit for looking at problems.
	log, logPath := slog.New(slog.DiscardHandler), ""
	if err := localfs.PrivateDir(cfg.StateDir); err == nil {
		p := filepath.Join(cfg.StateDir, "node.log")
		if lf, err := openLogFile(p, 1<<20); err == nil {
			defer lf.Close()
			log, logPath = slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: level})), p
		}
	}
	// A node that only lives while the TUI is open leaves the share on quit.
	n, err := startNode(ctx, cfg, log, true)
	if err != nil {
		return err
	}
	log.Info("TUI runs this node", "node", cfg.NodeID, "version", version)
	// If the worker stops on its own (lock lost, or another worker started
	// at the same moment kept the ID), the TUI ends too and stop returns why.
	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()
	go func() {
		select {
		case <-n.exited:
			tcancel()
		case <-tctx.Done():
		}
	}()
	opts.RunsNode, opts.LogPath = true, logPath
	terr := tui.Run(tctx, n.c, opts)
	// The worker's reason comes first: when it stopped on its own (its
	// directory deleted, lock lost), that is why the TUI ended.
	if err := n.stop(); err != nil {
		return err
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
