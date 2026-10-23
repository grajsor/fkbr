// Package cli is the scriptable command-line frontend. Like every frontend
// it talks to fkbr only through api.Client.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"text/tabwriter"
	"time"

	"fkbr/internal/api"
)

// Commands lists the subcommands this frontend handles.
var Commands = map[string]string{
	"nodes":   "list registered nodes and whether they are online",
	"forget":  "remove offline nodes from the share: forget NODE...",
	"prune":   "clean finished records off the share now: prune [-keep 0]",
	"ls":      "list a directory: ls [node:]/path (or [node:]root/path with several roots)",
	"cp":      "copy: cp [node:]/path... node|n1,n2|@all:/dir (also: put, get)",
	"put":     "alias for cp",
	"mv":      "move: mv SRC... DST (copy, then delete the sources if every copy succeeded)",
	"mkdir":   "make folders, with their parents: mkdir [node:]/path...",
	"rm":      "delete: rm [-r] [node:]/path... (-r for folders)",
	"get":     "alias for cp",
	"jobs":    "list transfer jobs",
	"job":     "show one job: job [-f] ID",
	"cancel":  "cancel a job: cancel ID",
	"catchup": "re-send a job to its detached targets: catchup ID",
	"events":  "stream node and job changes as JSON lines",
	"sync":    "make DST match SRC (incremental or -mirror), -n for a dry run",
}

// Run executes one CLI subcommand.
func Run(ctx context.Context, c api.Client, cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "nodes":
		return nodes(ctx, c, args, out)
	case "forget":
		return forget(ctx, c, args, out)
	case "prune":
		return prune(ctx, c, args, out)
	case "ls":
		return ls(ctx, c, args, out)
	case "cp", "put", "get":
		return cp(ctx, c, args, out)
	case "mv":
		return mv(ctx, c, args, out)
	case "mkdir":
		return mkdir(ctx, c, args, out)
	case "rm":
		return rm(ctx, c, args, out)
	case "jobs":
		return jobs(ctx, c, args, out)
	case "job":
		return job(ctx, c, args, out)
	case "cancel":
		return cancel(ctx, c, args, out)
	case "catchup":
		return catchup(ctx, c, args, out)
	case "events":
		return events(ctx, c, args, out)
	case "sync":
		return syncCmd(ctx, c, args, out)
	}
	return fmt.Errorf("unknown command %q", cmd)
}

func nodes(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("nodes", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "print JSON")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	ns, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, ns)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	links := false // an ssh swarm: say how each node is reached
	for _, n := range ns {
		links = links || n.Link != ""
	}
	head := "NODE\tSTATE\tLAST SEEN\tHOST\tOS\tROLES\tROOTS\tCHOWN\tWRITERS\tVERSION"
	if links {
		head += "\tLINK"
	}
	fmt.Fprintln(tw, head)
	for _, n := range ns {
		name := n.ID
		if n.Self {
			name += " *"
		}
		state := "offline"
		if n.Online {
			state = "online"
		}
		seen := "never"
		if n.LastSeen >= 0 {
			seen = n.LastSeen.Round(time.Second).String()
		}
		roots := make([]string, len(n.Roots))
		for i, r := range n.Roots {
			roots[i] = r.ID
			if !r.Writable {
				roots[i] += "(ro)"
			}
		}
		writers := "any"
		if len(n.AcceptWritesFrom) > 0 {
			writers = strings.Join(n.AcceptWritesFrom, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%v\t%s\t%s", name, state, seen, n.Hostname, n.OS,
			dash(strings.Join(n.Roles, ",")), dash(strings.Join(roots, ",")), chownLabel(n.NodeInfo), writers, dash(n.Version))
		if links {
			link := n.Link
			if n.Self {
				link = "home"
			}
			fmt.Fprintf(tw, "\t%s", dash(link))
		}
		fmt.Fprintln(tw)
	}
	return tw.Flush()
}

func ls(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("ls", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "print JSON")
	offset := fl.Int("offset", 0, "skip this many entries")
	limit := fl.Int("limit", 0, "max entries (default 1000)")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return fmt.Errorf("usage: ls [-json] [-offset N] [-limit N] [node:]root[/path]")
	}
	node, root, p := ParseTarget(fl.Arg(0))
	root, p, err := resolveRoot(ctx, c, node, root, p)
	if err != nil {
		return err
	}
	if root == "" {
		return lsRoots(ctx, c, node, *asJSON, out)
	}
	res, err := c.List(ctx, api.ListRequest{Node: node, Root: root, Path: p, Offset: *offset, Limit: *limit})
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, res)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 1, ' ', tabwriter.AlignRight)
	for _, e := range res.Entries {
		name := e.Name
		if e.Type == api.TypeDir {
			name += "/"
		}
		if e.LinkTarget != "" {
			name += " -> " + e.LinkTarget
		}
		fmt.Fprintf(tw, "%s\t%d:%d\t%d\t %s\t %s\n", ModeString(e), e.UID, e.GID, e.Size,
			time.Unix(0, e.MtimeNs).Format("2006-01-02 15:04"), name)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if shown := res.Offset + len(res.Entries); shown < res.Total {
		fmt.Fprintf(out, "... %d of %d entries shown; use -offset %d for more\n", len(res.Entries), res.Total, shown)
	}
	return nil
}

func lsRoots(ctx context.Context, c api.Client, node string, asJSON bool, out io.Writer) error {
	if node == "" {
		node = c.Self()
	}
	ns, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range ns {
		if n.ID != node {
			continue
		}
		if asJSON {
			return writeJSON(out, n.Roots)
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ROOT\tACCESS\tPATH")
		for _, r := range n.Roots {
			access := "ro"
			if r.Writable {
				access = "rw"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.ID, access, r.Path)
		}
		return tw.Flush()
	}
	return fmt.Errorf("%w: %q (has its worker ever run?)", api.ErrUnknownNode, node)
}

// ParseTarget splits "node:root/some/path". The node part is optional
// ("root/path" means the local node); "node:" alone selects the node's roots.
func ParseTarget(s string) (node, root, rel string) {
	if i := strings.IndexByte(s, ':'); i >= 0 && !strings.Contains(s[:i], "/") {
		node, s = s[:i], s[i+1:]
	}
	s = strings.TrimPrefix(s, "/")
	root, rel, _ = strings.Cut(s, "/")
	return node, root, rel
}

// ModeString renders an entry like ls -l does, e.g. "drwxr-xr-x".
func ModeString(e api.Entry) string {
	b := []byte("----------")
	switch e.Type {
	case api.TypeDir:
		b[0] = 'd'
	case api.TypeSymlink:
		b[0] = 'l'
	case api.TypeOther:
		b[0] = '?'
	}
	const rwx = "rwxrwxrwx"
	for i := range 9 {
		if e.Mode&(1<<(8-i)) != 0 {
			b[i+1] = rwx[i]
		}
	}
	// Special bits replace the execute slot: s/S for setuid/setgid, t/T sticky.
	special := func(bit uint32, pos int, set, unset byte) {
		if e.Mode&bit == 0 {
			return
		}
		if b[pos] == 'x' {
			b[pos] = set
		} else {
			b[pos] = unset
		}
	}
	special(0o4000, 3, 's', 'S')
	special(0o2000, 6, 's', 'S')
	special(0o1000, 9, 't', 'T')
	return string(b)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// chownLabel summarises how much ownership a node can preserve.
func chownLabel(n api.NodeInfo) string {
	switch {
	case n.ChownCapable && n.DACOverride:
		return "full"
	case n.ChownCapable:
		return "files"
	}
	return "no"
}

// sharedRoots leaves out the roots only their node sees.
func sharedRoots(rs []api.Root) []api.Root {
	var out []api.Root
	for _, r := range rs {
		if !r.Local {
			out = append(out, r)
		}
	}
	return out
}

// resolveRoot applies the single-root shorthand. Nodes started the default
// way share one directory, so "node:/sub/dir" means a path in that root
// rather than a root named "sub". When the first segment is not a root of
// every addressed node, and those nodes all have exactly one root with the
// same ID, the segment is treated as part of the path. A node's root
// "local" (only it sees it) does not count; a path that could mean it or a
// folder of that name in the shared root is refused. nodes is "" (this
// node), "n1,n2", "@all" or "@role".
func resolveRoot(ctx context.Context, c api.Client, nodes, root, rel string) (string, string, error) {
	all, err := c.Nodes(ctx)
	if err != nil {
		return "", "", err
	}
	var targets []api.Node
	for _, spec := range strings.Split(nodes, ",") {
		for _, n := range all {
			switch {
			case spec == "" && n.ID == c.Self(),
				spec == n.ID,
				spec == "@"+api.GroupAll && n.ID != c.Self(),
				strings.HasPrefix(spec, "@") && n.HasRole(spec[1:]):
				targets = append(targets, n)
			}
		}
	}
	if len(targets) == 0 {
		return root, rel, nil // let the engine report the unknown node
	}
	hasRoot, ownRoot := root != "", root != ""
	single := ""
	for i, n := range targets {
		found := false
		for _, r := range n.Roots {
			found = found || r.ID == root
			ownRoot = ownRoot && (r.ID != root || r.Local)
		}
		hasRoot = hasRoot && found
		shared := sharedRoots(n.Roots)
		if len(shared) != 1 || (i > 0 && shared[0].ID != single) {
			single = ""
			break
		}
		single = shared[0].ID
	}
	if hasRoot && ownRoot && single != "" {
		// cd /usr && fkbr: is /local/bin root local's /bin or /usr/local/bin?
		if _, err := c.List(ctx, api.ListRequest{Node: nodes, Root: single, Path: root, Limit: 1}); err == nil {
			p := path.Join(root, rel)
			return "", "", fmt.Errorf("%w: %s could be in root %s or in the folder %s of root %s; write %s/%s for the folder",
				api.ErrBadPath, p, root, root, single, single, p)
		}
	}
	if hasRoot || single == "" {
		return root, rel, nil
	}
	if root == "" {
		return single, rel, nil
	}
	if rel == "" {
		return single, root, nil
	}
	return single, root + "/" + rel, nil
}

func forget(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: forget NODE... (offline nodes only)")
	}
	var failed error
	for _, n := range args {
		if err := c.Forget(ctx, n); err != nil {
			fmt.Fprintf(out, "%s: %v\n", n, err)
			failed = fmt.Errorf("some nodes were not forgotten")
			continue
		}
		fmt.Fprintf(out, "forgot %s\n", n)
	}
	return failed
}

func prune(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("prune", flag.ContinueOnError)
	keep := fl.Duration("keep", 0, "keep records younger than this")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if err := c.Prune(ctx, *keep); err != nil {
		return err
	}
	fmt.Fprintf(out, "pruned finished records older than %s (running jobs are kept)\n", *keep)
	return nil
}

// parseFlags parses a command's flags wherever they are among its
// arguments, so "sync /a b:/c -mirror" works like "sync -mirror /a b:/c".
// After "--" everything is an argument.
func parseFlags(fl *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fl.Parse(args); err != nil {
			return err
		}
		rest := fl.Args()
		if len(rest) == 0 {
			break
		}
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
	return fl.Parse(append([]string{"--"}, pos...))
}

// excludeFlags adds -exclude PAT (repeatable) and -exclude-from FILE to
// fl; the function it returns gives all patterns after parsing.
func excludeFlags(fl *flag.FlagSet) func() ([]string, error) {
	var pats []string
	fl.Func("exclude", "leave out entries matching `PAT` (repeatable)", func(s string) error {
		pats = append(pats, s)
		return nil
	})
	from := fl.String("exclude-from", "", "read exclude patterns from `FILE`, one per line (# starts a comment)")
	return func() ([]string, error) {
		if *from == "" {
			return pats, nil
		}
		b, err := os.ReadFile(*from)
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				pats = append(pats, l)
			}
		}
		return pats, nil
	}
}
