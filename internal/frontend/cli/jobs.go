package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/product"
)

var cpUsage = product.Expand(`usage: cp [-conflict skip|overwrite|overwrite-if-older] [-z] [-detach] SRC... DST

SRC is [node:]/path inside the node's shared directory (or
[node:]root/path when a node shares several roots); all sources must be
on one node. DST is node:/dir, n1,n2:/dir or @all:/dir (every other node;
@role works too). Sources are copied into dir, created if needed, and
sent once for all destinations. Nodes that are offline, fall behind or
fail are detached; '{name} catchup JOB' re-sends to them later.
Examples:
  {name} /mnt/xfer cp /reports hostb:/incoming      # here -> hostb
  {name} /mnt/xfer cp hostb:/logs/app.log /          # hostb -> here
  {name} /mnt/xfer cp /release @all:/release         # to every node`)

func cp(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("cp", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprintln(fl.Output(), cpUsage) }
	conflict := fl.String("conflict", string(api.DefaultConflictPolicy), "what to do when a destination entry exists")
	compress := fl.Bool("z", false, "zstd-compress chunks on the share")
	detach := fl.Bool("detach", false, "print the job ID and return immediately")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() < 2 {
		fl.Usage()
		return errors.New("need at least one source and a destination")
	}
	req := api.TransferRequest{Conflict: api.ConflictPolicy(*conflict), Compress: *compress}
	srcs := fl.Args()[:fl.NArg()-1]
	for i, s := range srcs {
		node, root, p := ParseTarget(s)
		root, p, err := resolveRoot(ctx, c, node, root, p)
		if err != nil {
			return err
		}
		if root == "" {
			return fmt.Errorf("source %q: missing root", s)
		}
		if p == "" {
			p = "."
		}
		if i == 0 {
			req.SrcNode, req.SrcRoot = node, root
		} else if node != req.SrcNode || root != req.SrcRoot {
			return fmt.Errorf("all sources must be on the same node and root (%q vs %s:%s)", s, req.SrcNode, req.SrcRoot)
		}
		req.Paths = append(req.Paths, p)
	}
	dnodes, droot, ddir := ParseTarget(fl.Arg(fl.NArg() - 1))
	droot, ddir, err := resolveRoot(ctx, c, dnodes, droot, ddir)
	if err != nil {
		return err
	}
	if droot == "" {
		return errors.New("destination: missing root")
	}
	if dnodes == "" {
		dnodes = c.Self()
	}
	req.DstNodes = strings.Split(dnodes, ",")
	req.DstRoot, req.DstDir = droot, ddir

	id, err := c.Submit(ctx, req)
	if err != nil {
		return err
	}
	if *detach {
		fmt.Fprintln(out, id)
		return nil
	}
	fmt.Fprintf(os.Stderr, "job %s submitted (Ctrl-C detaches; the job keeps running)\n", id)
	return follow(ctx, c, id, out)
}

// follow shows progress until the job ends, then a summary. The live
// progress line is only drawn when stderr is a terminal.
func follow(ctx context.Context, c api.Client, id string, out io.Writer) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	tty := isTerminal(os.Stderr)
	for {
		j, err := c.Job(context.WithoutCancel(ctx), id)
		if err != nil {
			return err
		}
		if j.State.Terminal() {
			if tty {
				fmt.Fprint(os.Stderr, "\r\033[K")
			}
			return summary(j, out)
		}
		if tty {
			fmt.Fprintf(os.Stderr, "\r\033[K%.*s", termWidth(), progressLine(j))
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "\ndetached; follow with '%[1]s job %[2]s', stop with '%[1]s cancel %[2]s'\n", product.Name, id)
			return nil
		case <-tick.C:
		}
	}
}

func progressLine(j *api.Job) string {
	parts := []string{string(j.State)}
	for _, p := range j.Participants {
		s := fmt.Sprintf("%s(%s): %d files %s, %d chunks", p.Node, p.Role, p.Files, humanBytes(p.Bytes), p.Chunks)
		if p.Phase != "" {
			s += " [" + p.Phase + "]"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

func summary(j *api.Job, out io.Writer) error {
	fmt.Fprintf(out, "job %s %s\n", j.ID, j.State)
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  NODE\tROLE\tSTATE\tFILES\tBYTES\tCHUNKS\tSKIPPED\tERRORS")
	var errs int64
	for _, p := range j.Participants {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\t%s\t%d\t%d\t%d\n", p.Node, p.Role, p.State, p.Files, humanBytes(p.Bytes), p.Chunks, p.Skipped, p.Errors)
		errs += p.Errors
	}
	tw.Flush()
	for _, p := range j.Participants {
		if p.Error != "" {
			fmt.Fprintf(out, "  %s: %s\n", p.Node, p.Error)
		}
		for _, pr := range p.Problems {
			fmt.Fprintf(out, "  %s: %s\n", p.Node, pr)
		}
	}
	if len(j.Detached) > 0 {
		fmt.Fprintln(out, "  detached targets (no data installed):")
		for _, d := range j.Detached {
			fmt.Fprintf(out, "    %s: %s (after %d chunks)\n", d.Node, d.Reason, d.Chunks)
		}
		fmt.Fprintf(out, "  re-send to them with: %s catchup %s\n", product.Name, j.ID)
	}
	if j.CatchUpOf != "" {
		fmt.Fprintf(out, "  catch-up of job %s\n", j.CatchUpOf)
	}
	switch {
	case j.State != api.JobDone:
		return fmt.Errorf("job %s", j.State)
	case errs > 0:
		return fmt.Errorf("job done with %d errors", errs)
	}
	return nil
}

func jobs(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("jobs", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "print JSON")
	if err := fl.Parse(args); err != nil {
		return err
	}
	js, err := c.Jobs(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, js)
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tSTATE\tCREATED\tFROM\tTO\tPATHS")
	for _, j := range js {
		r := j.Request
		state := string(j.State)
		if len(j.Detached) > 0 {
			state += fmt.Sprintf(" (%d detached)", len(j.Detached))
		}
		paths := strings.Join(r.Paths, " ")
		if r.SyncPlan != "" {
			paths = fmt.Sprintf("(sync of %s: %d entries)", r.BaseDir, r.PathsCount)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s:%s\t%s:%s/%s\t%s\n", j.ID, state, j.Created.Local().Format("2006-01-02 15:04:05"),
			r.SrcNode, r.SrcRoot, strings.Join(r.DstNodes, ","), r.DstRoot, r.DstDir, paths)
	}
	return tw.Flush()
}

func job(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("job", flag.ContinueOnError)
	asJSON := fl.Bool("json", false, "print JSON")
	wait := fl.Bool("f", false, "follow until the job ends")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return errors.New("usage: job [-json] [-f] ID")
	}
	if *wait {
		return follow(ctx, c, fl.Arg(0), out)
	}
	j, err := c.Job(ctx, fl.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(out, j)
	}
	if !j.State.Terminal() {
		fmt.Fprintln(out, progressLine(j))
		return nil
	}
	return summary(j, out)
}

func cancel(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: cancel ID")
	}
	if err := c.Cancel(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(out, "cancellation requested for job %s\n", args[0])
	return nil
}

func catchup(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("catchup", flag.ContinueOnError)
	detach := fl.Bool("detach", false, "print the new job ID and return immediately")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 1 {
		return errors.New("usage: catchup [-detach] JOB")
	}
	id, err := c.CatchUp(ctx, fl.Arg(0))
	if err != nil {
		return err
	}
	if *detach {
		fmt.Fprintln(out, id)
		return nil
	}
	fmt.Fprintf(os.Stderr, "catch-up job %s submitted\n", id)
	return follow(ctx, c, id, out)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// termWidth returns $COLUMNS-1, or 159 if unset, so the progress line
// does not wrap (a wrapped line cannot be redrawn with \r).
func termWidth() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 1 {
		return n - 1
	}
	return 159
}

// events prints the event stream as JSON lines until interrupted.
func events(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: events")
	}
	ch, err := c.Subscribe(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	return nil
}
