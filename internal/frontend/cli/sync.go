package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"fkbr/internal/api"
	"fkbr/internal/product"
)

const syncUsage = `usage: sync [-mirror] [-checksum] [-z] [-n] [-max-deletes N] [-force] SRC DST
       sync -plan ID [-force]

Makes DST match SRC, like "rsync -a SRC/ DST/". SRC is [node:]/dir, DST
is node:/dir, n1,n2:/dir or @all:/dir (add the root name if a node shares
several roots). Both sides are scanned and
compared (type, size, mtime, mode, symlink target, ownership where the
target can apply it); only differences are sent, and entries needed by
several targets are sent once for all of them.

  -mirror       also delete destination entries missing from the source
  -n            dry run: show the plan and its ID, change nothing
  -plan ID      apply a plan made earlier with -n
  -max-deletes  per-target deletion limit (0: node default, -1: none)
  -force        apply even if a target exceeds the deletion limit`

func syncCmd(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("sync", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprintln(fl.Output(), syncUsage) }
	mirror := fl.Bool("mirror", false, "delete destination entries missing from the source")
	checksum := fl.Bool("checksum", false, "also compare file content hashes (reads every file)")
	compress := fl.Bool("z", false, "zstd-compress chunks on the share")
	dry := fl.Bool("n", false, "dry run")
	planID := fl.String("plan", "", "apply this plan")
	maxDel := fl.Int("max-deletes", 0, "deletion limit per target")
	force := fl.Bool("force", false, "allow exceeding the deletion limit")
	if err := fl.Parse(args); err != nil {
		return err
	}

	var plan *api.SyncPlan
	var err error
	if *planID != "" {
		if fl.NArg() != 0 {
			return errors.New("-plan takes no SRC/DST")
		}
		if plan, err = c.SyncPlan(ctx, *planID); err != nil {
			return err
		}
	} else {
		if fl.NArg() != 2 {
			fl.Usage()
			return errors.New("need SRC and DST")
		}
		req := api.SyncRequest{Mode: api.SyncIncremental, Checksum: *checksum, Compress: *compress, MaxDeletes: *maxDel}
		if *mirror {
			req.Mode = api.SyncMirror
		}
		var dnodes string
		req.SrcNode, req.SrcRoot, req.SrcPath = ParseTarget(fl.Arg(0))
		dnodes, req.DstRoot, req.DstPath = ParseTarget(fl.Arg(1))
		if req.SrcRoot, req.SrcPath, err = resolveRoot(ctx, c, req.SrcNode, req.SrcRoot, req.SrcPath); err != nil {
			return err
		}
		if req.DstRoot, req.DstPath, err = resolveRoot(ctx, c, dnodes, req.DstRoot, req.DstPath); err != nil {
			return err
		}
		if req.SrcRoot == "" || req.DstRoot == "" {
			return errors.New("SRC and DST need a root")
		}
		if dnodes == "" {
			dnodes = c.Self()
		}
		req.DstNodes = strings.Split(dnodes, ",")
		if plan, err = planWithProgress(ctx, c, req); err != nil {
			return err
		}
	}
	printPlan(plan, out)
	if *dry || plan.Applied {
		if plan.Applied {
			fmt.Fprintln(out, "plan already applied")
		} else {
			fmt.Fprintf(out, "dry run; apply with: %s sync -plan %s\n", product.Name, plan.ID)
		}
		return nil
	}
	if len(plan.Groups) == 0 && totalDeletes(plan) == 0 {
		fmt.Fprintln(out, "nothing to do")
		return nil
	}
	if plan, err = c.ApplySync(ctx, plan.ID, *force); err != nil {
		return err
	}
	for _, e := range plan.DeleteErrors {
		fmt.Fprintln(out, "delete error:", e)
	}
	var failed error
	for _, id := range plan.Jobs {
		fmt.Fprintf(os.Stderr, "job %s\n", id)
		if err := follow(ctx, c, id, out); err != nil {
			failed = err
		}
		if ctx.Err() != nil {
			return nil // detached
		}
	}
	if failed == nil && len(plan.DeleteErrors) > 0 {
		failed = fmt.Errorf("%d deletions failed", len(plan.DeleteErrors))
	}
	return failed
}

// planWithProgress plans while printing scan progress from the event
// stream (when stderr is a terminal and events are available).
func planWithProgress(ctx context.Context, c api.Client, req api.SyncRequest) (*api.SyncPlan, error) {
	if isTerminal(os.Stderr) {
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if ch, err := c.Subscribe(sctx); err == nil {
			go func() {
				seen := map[string]api.ScanProgress{}
				for ev := range ch {
					if ev.Type != api.EventScan || ev.Scan == nil {
						continue
					}
					seen[ev.Scan.Node] = *ev.Scan
					var parts []string
					for n, s := range seen {
						st := fmt.Sprintf("%s %d entries", n, s.Entries)
						if s.Done {
							st += " ✓"
						}
						parts = append(parts, st)
					}
					fmt.Fprintf(os.Stderr, "\r\033[Kscanning: %.*s", termWidth()-10, strings.Join(parts, ", "))
				}
			}()
			defer fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}
	return c.PlanSync(ctx, req)
}

func totalDeletes(p *api.SyncPlan) int64 {
	var n int64
	for _, t := range p.Targets {
		n += t.Delete + t.Replace
	}
	return n
}

func printPlan(p *api.SyncPlan, out io.Writer) {
	r := p.Request
	fmt.Fprintf(out, "sync plan %s: %s:%s/%s → %s:%s/%s (%s)\n", p.ID, r.SrcNode, r.SrcRoot, r.SrcPath,
		strings.Join(r.DstNodes, ","), r.DstRoot, r.DstPath, r.Mode)
	fmt.Fprintf(out, "source: %d entries, %s\n", p.SrcEntries, humanBytes(p.SrcBytes))
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  TARGET\tCOPY\tBYTES\tREPLACE\tDELETE\tUNCHANGED")
	for _, t := range p.Targets {
		del := fmt.Sprint(t.Delete)
		if t.OverLimit {
			del += fmt.Sprintf(" (over limit %d)", p.MaxDeletes)
		}
		fmt.Fprintf(tw, "  %s\t%d\t%s\t%d\t%s\t%d\n", t.Node, t.Copy, humanBytes(t.CopyBytes), t.Replace, del, t.Unchanged)
	}
	tw.Flush()
	for _, g := range p.Groups {
		fmt.Fprintf(out, "  transfer to %s: %d entries, %s\n", strings.Join(g.Targets, ","), g.Entries, humanBytes(g.Bytes))
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(out, "  skipped %s (offline)\n", s)
	}
	for _, pr := range p.Problems {
		fmt.Fprintln(out, "  problem:", pr)
	}
}
