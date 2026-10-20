package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"fkbr/internal/api"
	"fkbr/internal/product"
)

// mkdir makes directories, with their parents, on any node.
func mkdir(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("mkdir", flag.ContinueOnError)
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if fl.NArg() == 0 {
		return errors.New("usage: mkdir [node:]/path...")
	}
	for _, a := range fl.Args() {
		node, root, p, err := place(ctx, c, a)
		if err != nil {
			return err
		}
		if err := c.Mkdir(ctx, api.MkdirRequest{Node: node, Root: root, Path: p}); err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
	}
	return nil
}

// rm deletes entries; folders (with everything in them) only with -r.
// Each path must exist: a typo is an error, not a quiet nothing.
func rm(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("rm", flag.ContinueOnError)
	recursive := fl.Bool("r", false, "also delete folders, with everything in them")
	if err := parseFlags(fl, args); err != nil {
		return err
	}
	if fl.NArg() == 0 {
		return errors.New("usage: rm [-r] [node:]/path...")
	}
	type key struct{ node, root string }
	var order []key
	groups := map[key][]string{}
	for _, a := range fl.Args() {
		node, root, p, err := place(ctx, c, a)
		if err != nil {
			return err
		}
		e, err := lookup(ctx, c, node, root, p)
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		if e.Type == api.TypeDir && !*recursive {
			return fmt.Errorf("%s is a folder; use rm -r to delete it with everything in it", a)
		}
		if strings.HasSuffix(a, "/") && e.Type != api.TypeDir {
			return fmt.Errorf("%s is not a folder (a trailing / means one)", a) // like coreutils rm
		}
		k := key{node, root}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], p)
	}
	var failed error
	for _, k := range order {
		res, err := c.Remove(ctx, api.RemoveRequest{Node: k.node, Root: k.root, Paths: groups[k]})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %d\n", res.Removed)
		for _, e := range res.Errors {
			fmt.Fprintln(out, "error:", e)
		}
		if len(res.Errors) > 0 {
			failed = fmt.Errorf("%d could not be removed", len(res.Errors))
		}
	}
	return failed
}

// mv copies, then deletes the sources, but only when every target got
// everything: nothing failed, skipped (an existing file kept) or detached.
func mv(ctx context.Context, c api.Client, args []string, out io.Writer) error {
	fl := flag.NewFlagSet("mv", flag.ContinueOnError)
	fl.Usage = func() { fmt.Fprintln(fl.Output(), mvUsage) }
	req, err := transferArgs(ctx, c, fl, args)
	if err != nil {
		return err
	}
	if len(req.Exclude) > 0 {
		return errors.New("mv cannot exclude: what is left out would be deleted with the sources (use cp, then rm)")
	}
	for _, p := range req.Paths {
		if p == "." {
			return errors.New("mv cannot move a whole shared folder (it cannot be deleted afterwards); use cp")
		}
	}
	req.Move = true // the nodes refuse a move onto its own sources
	id, err := c.Submit(ctx, req)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "job %s submitted; the sources are deleted once every copy succeeded\n", id)
	ferr := follow(ctx, c, id, out)
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "detached: the sources are kept")
		return nil
	}
	if ferr != nil {
		return fmt.Errorf("%w; the sources are kept", ferr)
	}
	j, err := c.Job(ctx, id)
	if err != nil {
		return err
	}
	var problems int64
	for _, p := range j.Participants {
		problems += p.Errors + p.Skipped
	}
	if j.State != api.JobDone || problems > 0 || len(j.Detached) > 0 {
		return fmt.Errorf("the copy ended %s with %d errors or skipped entries and %d detached targets; the sources are kept",
			j.State, problems, len(j.Detached))
	}
	res, err := c.Remove(ctx, api.RemoveRequest{Node: req.SrcNode, Root: req.SrcRoot, Paths: req.Paths})
	if err != nil {
		return fmt.Errorf("copied, but deleting the sources failed: %w", err)
	}
	for _, e := range res.Errors {
		fmt.Fprintln(out, "error deleting a source:", e)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("copied, but %d sources could not be deleted", len(res.Errors))
	}
	fmt.Fprintf(out, "moved; %d sources deleted\n", res.Removed)
	return nil
}

var mvUsage = product.Expand(`usage: mv [-conflict skip|overwrite|overwrite-if-older] [-z] SRC... DST

Copies like cp, then deletes the sources, but only if every target got
everything: nothing failed, nothing was skipped (with the default
-conflict skip, an existing file counts as skipped), no target was
detached. Otherwise the sources stay. Ctrl-C detaches: the copy goes on,
the sources stay.
  {name} /mnt/xfer mv /outbox/report.pdf hostb:/inbox`)

// place resolves [node:]/path to a node, root and path inside it.
func place(ctx context.Context, c api.Client, arg string) (node, root, p string, err error) {
	node, root, p = ParseTarget(arg)
	if root, p, err = resolveRoot(ctx, c, node, root, p); err != nil {
		return
	}
	if root == "" {
		err = fmt.Errorf("%s: missing root", arg)
	}
	if p == "" {
		p = "."
	}
	return
}

// lookup finds entry p by listing its folder (there is no stat call).
func lookup(ctx context.Context, c api.Client, node, root, p string) (api.Entry, error) {
	dir, name := path.Split(p)
	if dir = path.Clean(dir); dir == "" {
		dir = "."
	}
	for off := 0; ; {
		res, err := c.List(ctx, api.ListRequest{Node: node, Root: root, Path: dir, Offset: off})
		if err != nil {
			return api.Entry{}, err
		}
		for _, e := range res.Entries {
			if e.Name == name {
				return e, nil
			}
		}
		off += len(res.Entries)
		if len(res.Entries) == 0 || off >= res.Total {
			return api.Entry{}, errors.New("no such file or folder")
		}
	}
}
