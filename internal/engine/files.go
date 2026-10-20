package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
	"fkbr/internal/receive"
	"fkbr/internal/transport"
)

// removeTimeoutFactor stretches request_timeout for removals, which may
// have to delete large trees on the remote side.
const removeTimeoutFactor = 10

func (e *Engine) Mkdir(ctx context.Context, req api.MkdirRequest) error {
	if req.Node == "" || req.Node == e.Self() {
		return e.mkdirLocal(req)
	}
	return e.remoteCall(ctx, req.Node, transport.TypeMkdir, req, nil, 1)
}

func (e *Engine) Remove(ctx context.Context, req api.RemoveRequest) (*api.RemoveResult, error) {
	if req.Node == "" || req.Node == e.Self() {
		return e.removeLocal(req)
	}
	var res api.RemoveResult
	if err := e.remoteCall(ctx, req.Node, transport.TypeRemove, req, &res, removeTimeoutFactor); err != nil {
		return nil, err
	}
	return &res, nil
}

func (e *Engine) Read(ctx context.Context, req api.ReadRequest) (*api.ReadResult, error) {
	if req.Node == "" || req.Node == e.Self() {
		return e.readLocal(req)
	}
	var res api.ReadResult
	if err := e.remoteCall(ctx, req.Node, transport.TypeRead, req, &res, 1); err != nil {
		return nil, err
	}
	return &res, nil
}

// remoteCall sends a single-reply request and decodes the reply into out.
func (e *Engine) remoteCall(ctx context.Context, node, typ string, body, out any, timeoutFactor int) error {
	if err := e.requireOnline(ctx, node); err != nil {
		return err
	}
	timeout := e.cfg.RequestTimeout * time.Duration(timeoutFactor)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msg, err := e.newMessage(node, typ, body)
	if err != nil {
		return err
	}
	if err := e.tr.Send(ctx, msg); err != nil {
		return err
	}
	err = e.tr.AwaitReplies(ctx, msg.ID, func(r *transport.Reply) error {
		if out == nil || len(r.Body) == 0 {
			return nil
		}
		return json.Unmarshal(r.Body, out)
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s on %s: no answer within %s (is its worker running?)", typ, node, timeout)
	}
	return err
}

// writablePath resolves a root-relative path for modification, refusing
// the root itself and the staging area.
func (e *Engine) writablePath(rootID, rel string) (string, error) {
	c, err := localfs.CleanRel(rel)
	if err != nil {
		return "", err
	}
	if c == "." {
		return "", fmt.Errorf("%w: refusing to modify the root itself", api.ErrBadPath)
	}
	if c == receive.StagingDir || strings.HasPrefix(c, receive.StagingDir+"/") {
		return "", fmt.Errorf("%w: %s is %s's staging area", api.ErrBadPath, receive.StagingDir, product.Name)
	}
	if h := e.shareIn(rootID); h != "" && (c == h || strings.HasPrefix(c, h+"/") || strings.HasPrefix(h, c+"/")) {
		return "", fmt.Errorf("%w: %s is or holds the share (%s)", api.ErrBadPath, c, h)
	}
	return c, nil
}

// shareIn is the share's path inside root rootID, or "" (config.Root.Hide).
func (e *Engine) shareIn(rootID string) string {
	for _, r := range e.cfg.Roots {
		if r.ID == rootID {
			return r.Hide
		}
	}
	return ""
}

func (e *Engine) mkdirLocal(req api.MkdirRequest) error {
	p, err := e.writablePath(req.Root, req.Path)
	if err != nil {
		return err
	}
	root, err := e.fs.WritableRoot(req.Root)
	if err != nil {
		return err
	}
	return root.MkdirAll(p, 0o755)
}

func (e *Engine) removeLocal(req api.RemoveRequest) (*api.RemoveResult, error) {
	root, err := e.fs.WritableRoot(req.Root)
	if err != nil {
		return nil, err
	}
	res := &api.RemoveResult{}
	for _, raw := range req.Paths {
		p, err := e.writablePath(req.Root, raw)
		if err == nil {
			err = removeAll(root, p)
		}
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", raw, err))
			continue
		}
		res.Removed++
	}
	return res, nil
}

// removeAll removes p and everything below it. Directories that are
// read-only for their owner (copied that way, say) would stop it, so on a
// permission error they are made writable and it is tried again; p's
// parent gets its mode back afterwards. Others' directories stay as they
// are, and so does the error.
func removeAll(root *os.Root, p string) error {
	err := root.RemoveAll(p)
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	fs.WalkDir(root.FS(), p, func(q string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			if fi, err := d.Info(); err == nil && fi.Mode().Perm()&0o700 != 0o700 {
				root.Chmod(q, fi.Mode().Perm()|0o700)
			}
		}
		return nil
	})
	if fi, err := root.Lstat(path.Dir(p)); err == nil && fi.Mode().Perm()&0o200 == 0 && root.Chmod(path.Dir(p), fi.Mode().Perm()|0o200) == nil {
		defer root.Chmod(path.Dir(p), fi.Mode().Perm())
	}
	return root.RemoveAll(p)
}

func (e *Engine) readLocal(req api.ReadRequest) (*api.ReadResult, error) {
	if req.Limit <= 0 || req.Limit > api.MaxReadLimit {
		req.Limit = api.MaxReadLimit
	}
	f, err := e.fs.Open(req.Root, req.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", api.ErrBadPath, path.Clean(req.Path))
	}
	buf := make([]byte, req.Limit)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return &api.ReadResult{Data: buf[:n], Size: fi.Size(), EOF: req.Offset+int64(n) >= fi.Size()}, nil
}

// serveFileOp handles mkdir/remove/read requests from peers.
func (e *Engine) serveFileOp(ctx context.Context, m *transport.Message) error {
	var out any
	switch m.Type {
	case transport.TypeMkdir:
		var req api.MkdirRequest
		if err := json.Unmarshal(m.Body, &req); err != nil {
			return err
		}
		if err := e.acceptWrite(ctx, m.From); err != nil {
			return err
		}
		if err := e.mkdirLocal(req); err != nil {
			return err
		}
		e.log.Info("mkdir for a peer", "from", m.From, "path", req.Root+"/"+req.Path)
	case transport.TypeRemove:
		var req api.RemoveRequest
		if err := json.Unmarshal(m.Body, &req); err != nil {
			return err
		}
		if err := e.acceptWrite(ctx, m.From); err != nil {
			return err
		}
		res, err := e.removeLocal(req)
		if err != nil {
			return err
		}
		e.log.Info("removed for a peer", "from", m.From, "root", req.Root, "paths", summary(req.Paths, 3),
			"removed", res.Removed, "errors", len(res.Errors))
		out = res
	case transport.TypeRead:
		var req api.ReadRequest
		if err := json.Unmarshal(m.Body, &req); err != nil {
			return err
		}
		res, err := e.readLocal(req)
		if err != nil {
			return err
		}
		out = res
	}
	var body []byte
	if out != nil {
		var err error
		if body, err = json.Marshal(out); err != nil {
			return err
		}
	}
	return e.tr.Reply(ctx, m, &transport.Reply{Final: true, Body: body})
}
