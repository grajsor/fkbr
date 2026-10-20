package engine

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"fkbr/internal/api"
)

// A move deletes its sources once the copy is done. If, on the same node
// and root, the destination is a source, lies inside one, or would put a
// source onto itself, that deletes the copy too: such a move must not
// start. Submit checks the paths as given; the source node checks them
// again with symlinks resolved, which only it can do.

// moveOverlap checks slash paths inside one root.
func moveOverlap(req api.TransferRequest, paths []string, dstDir string) error {
	for _, p := range paths {
		land := path.Join(dstDir, path.Base(p))
		if req.BaseDir != "" {
			land = path.Join(dstDir, inventoryRel(req.BaseDir, p))
		}
		if p == "." || dstDir == p || strings.HasPrefix(dstDir, p+"/") || land == p {
			return fmt.Errorf("%w: moving %s to %s would delete what it copies", api.ErrBadPath, p, dstDir)
		}
	}
	return nil
}

// sameRootTarget reports whether the job's source is also one of its
// targets, into the same root: the only case where a move can overlap.
func sameRootTarget(req api.TransferRequest, targets []string) bool {
	for _, t := range targets {
		if t == req.SrcNode && req.DstRoot == req.SrcRoot {
			return true
		}
	}
	return false
}

// checkMoveHere is moveOverlap on this node's real paths.
func (e *Engine) checkMoveHere(req api.TransferRequest) error {
	var rootPath string
	for _, r := range e.fs.Roots() {
		if r.ID == req.SrcRoot {
			rootPath = r.Path
		}
	}
	realRoot, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return err
	}
	// real resolves rel inside the root as far as it exists; the rest is
	// appended as given (the destination may not exist yet).
	real := func(rel string) string {
		rest := ""
		for p := rel; ; p = path.Dir(p) {
			if r, err := filepath.EvalSymlinks(filepath.Join(rootPath, filepath.FromSlash(p))); err == nil {
				if out, err := filepath.Rel(realRoot, r); err == nil {
					return path.Join(filepath.ToSlash(out), rest)
				}
			}
			if p == "." || p == "/" {
				return rel
			}
			rest = path.Join(path.Base(p), rest)
		}
	}
	var paths []string
	for _, p := range req.Paths {
		// A source's own last name is not followed: moving a symlink
		// moves the link.
		paths = append(paths, path.Join(real(path.Dir(p)), path.Base(p)))
	}
	return moveOverlap(req, paths, real(req.DstDir))
}
