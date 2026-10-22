package engine

// Local-only roots (config.Root.Local) are this node's own. Its TUI and
// CLI browse them, and copies this node starts may read from them and,
// where no one outside the swarm can write the share, into them. No other
// node sees or reaches them:
//
//   - they are not published (NodeInfo.Roots); Nodes adds them to this
//     node's own entry for its TUI and CLI only;
//   - a request from another node that names one is refused as an
//     unknown root;
//   - a job that reads or writes one runs only if this node submitted it.
//     Submit keeps the request in the private state dir (own/<job>.json);
//     the source and the target refuse a job whose record differs from
//     it. Job records on a share can be forged or changed, the state dir
//     cannot;
//   - writes into one need a private share (an ssh swarm, local mode): on
//     NFS anyone who can write the share could forge the chunks that
//     would be installed there;
//   - syncs do not use them yet: their path lists and snapshots are blobs
//     on the share.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/localfs"
)

// errLocalRoot: what a local-only root refuses.
var errLocalRoot = errors.New("local root")

func (e *Engine) isLocalRoot(id string) bool {
	for _, r := range e.fs.Roots() {
		if r.ID == id {
			return r.Local
		}
	}
	return false
}

func publishedRoots(rs []api.Root) []api.Root {
	var out []api.Root
	for _, r := range rs {
		if !r.Local {
			out = append(out, r)
		}
	}
	return out
}

// withLocalRoots adds this node's local-only roots to its own entry.
func (e *Engine) withLocalRoots(ns []api.Node) []api.Node {
	var local []api.Root
	for _, r := range e.fs.Roots() {
		if r.Local {
			local = append(local, r)
		}
	}
	if len(local) == 0 {
		return ns
	}
	for i := range ns {
		if ns[i].ID == e.Self() {
			ns[i].Roots = append(slices.Clone(ns[i].Roots), local...)
		}
	}
	return ns
}

// refuseLocalRoot answers a request from another node that names a
// local-only root as if the root did not exist.
func (e *Engine) refuseLocalRoot(body json.RawMessage) error {
	var r struct {
		Root string `json:"root"`
	}
	if json.Unmarshal(body, &r) == nil && e.isLocalRoot(r.Root) {
		return fmt.Errorf("%w: %q", api.ErrUnknownRoot, r.Root)
	}
	return nil
}

// touchesLocal reports whether this node reads (as source) or writes (as
// target) a local-only root in req.
func (e *Engine) touchesLocal(req *api.TransferRequest) (src, dst bool) {
	src = req.SrcNode == e.Self() && e.isLocalRoot(req.SrcRoot)
	dst = slices.Contains(req.DstNodes, e.Self()) && e.isLocalRoot(req.DstRoot)
	return src, dst
}

// checkLocalSubmit refuses what local-only roots do not do, and a move on
// this node from one root into another that holds it.
func (e *Engine) checkLocalSubmit(req *api.TransferRequest) error {
	if req.Move && req.SrcNode == e.Self() && slices.Contains(req.DstNodes, e.Self()) && req.SrcRoot != req.DstRoot {
		if err := e.moveAcrossRoots(req); err != nil {
			return err
		}
	}
	src, dst := e.touchesLocal(req)
	if !src && !dst {
		return nil
	}
	if req.PathsBlob != "" {
		return fmt.Errorf("%w: syncs do not use root %q (it is %s's own); copy instead", api.ErrNotAllowed, e.rootOf(req, src), e.Self())
	}
	if dst && !e.cfg.PrivateShare() {
		return fmt.Errorf("%w: root %q takes no data through a share others can write; copy into a shared root", api.ErrNotAllowed, req.DstRoot)
	}
	return nil
}

func (e *Engine) rootOf(req *api.TransferRequest, src bool) string {
	if src {
		return req.SrcRoot
	}
	return req.DstRoot
}

// moveAcrossRoots refuses a move on this node into or onto one of its own
// sources through another root (the local root "/" holds the shared
// directory; two roots may overlap): it would delete what it copied.
func (e *Engine) moveAcrossRoots(req *api.TransferRequest) error {
	paths := map[string]string{}
	for _, r := range e.cfg.Roots {
		paths[r.ID] = r.Path
	}
	dst := config.RealPath(filepath.Join(paths[req.DstRoot], filepath.FromSlash(req.DstDir)))
	for _, p := range req.Paths {
		src := config.RealPath(filepath.Join(paths[req.SrcRoot], filepath.FromSlash(p)))
		if dst == src || strings.HasPrefix(dst, src+"/") || filepath.Dir(src) == dst {
			return fmt.Errorf("%w: moving %s:%s into %s:%s would move it into or onto itself", api.ErrBadPath, req.SrcRoot, p, req.DstRoot, req.DstDir)
		}
	}
	return nil
}

func (e *Engine) ownDir() string { return filepath.Join(e.cfg.StateDir, "own") }

// keepOwn records a job this node submitted that touches a local-only
// root.
func (e *Engine) keepOwn(job *api.Job) error {
	if src, dst := e.touchesLocal(&job.Request); !src && !dst {
		return nil
	}
	b, err := json.Marshal(&job.Request)
	if err != nil {
		return err
	}
	if err := localfs.PrivateDir(e.ownDir()); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.ownDir(), job.ID+".json"), b, 0o600)
}

// checkOwn refuses to run job here, as role, if it touches a local-only
// root and is not the job this node submitted.
func (e *Engine) checkOwn(job *api.Job, role api.Role) error {
	src, dst := e.touchesLocal(&job.Request)
	if (role == api.RoleSource && !src) || (role == api.RoleTarget && !dst) {
		return nil
	}
	refuse := func(why string) error {
		e.log.Warn("refusing a job on a local root", "job", job.ID, "role", role, "why", why)
		return fmt.Errorf("%w: job %s uses root %q but %s", errLocalRoot, job.ID, e.rootOf(&job.Request, role == api.RoleSource), why)
	}
	if job.Initiator != e.Self() {
		return refuse("was not started from this node")
	}
	if role == api.RoleTarget && !e.cfg.PrivateShare() {
		return refuse("its data would come through a share others can write")
	}
	if job.Request.PathsBlob != "" {
		return refuse("lists its paths in a blob")
	}
	kept, err := os.ReadFile(filepath.Join(e.ownDir(), job.ID+".json"))
	if err != nil {
		return refuse("this node has no record of starting it")
	}
	b, err := json.Marshal(&job.Request)
	if err != nil || !bytes.Equal(b, kept) {
		return refuse("its record differs from what this node submitted")
	}
	return nil
}

// ownKeep is how long the records of submitted jobs are kept; a job not
// done by then cannot resume on a local root.
const ownKeep = 7 * 24 * time.Hour

func (e *Engine) pruneOwn() {
	des, err := os.ReadDir(e.ownDir())
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return
	}
	for _, de := range des {
		if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > ownKeep {
			os.Remove(filepath.Join(e.ownDir(), de.Name()))
		}
	}
}
