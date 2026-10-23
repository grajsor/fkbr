package sshlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
	"fkbr/internal/transport"
)

// Remote is a node's end of its link to the home node. Records go to the
// home node; the chunks this node sends stay in a private directory on
// its own disk (chunkDir) until every target has them.
type Remote struct {
	sess     *frame.Session
	self     string
	chunks   *store.Dir
	chunkDir string
	log      *slog.Logger
	offset   atomic.Int64  // home clock minus ours, ns
	got      atomic.Uint64 // Seq of the last fetch answer read whole (see outbox)
	stop     chan struct{}
	stopOnce atomicOnce
}

type atomicOnce struct{ done atomic.Bool }

func (o *atomicOnce) Do(f func()) {
	if o.done.CompareAndSwap(false, true) {
		f()
	}
}

var (
	_ transport.Transport = (*Remote)(nil)
	_ transport.Store     = (*Remote)(nil)
)

// Dial starts the remote end on conn (stdin and stdout under ssh): the
// handshake, then hello as node self. chunkDir must be private and on
// local disk.
func Dial(ctx context.Context, conn io.ReadWriteCloser, self, version, chunkDir string, opts frame.Options, log *slog.Logger) (*Remote, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(chunkDir, 0o700); err != nil {
		return nil, err
	}
	chunks, err := store.Open(chunkDir, self, store.Options{Private: true, NoSync: true})
	if err != nil {
		return nil, err
	}
	sess, err := frame.Server(conn, opts)
	if err != nil {
		return nil, err
	}
	r := &Remote{sess: sess, self: self, chunks: chunks, chunkDir: chunkDir, log: log, stop: make(chan struct{})}
	before := time.Now()
	var now time.Time
	if err := do(ctx, sess, "hello", &hello{Node: self, Protocol: api.ProtocolVersion, Version: version, Link: linkVersion}, &now); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ssh link: %w", err)
	}
	r.measure(before, now)
	go serve(context.Background(), sess, r.handlers(), log)
	r.sweep(ctx)
	return r, nil
}

// Done is closed when the link has ended.
func (r *Remote) Done() <-chan struct{} { return r.sess.Done() }

// Err says why the link ended.
func (r *Remote) Err() error { return r.sess.Err() }

// Stopping is closed when the home node asks this node to stop.
func (r *Remote) Stopping() <-chan struct{} { return r.stop }

// Close ends the link.
func (r *Remote) Close() error { return r.sess.Close() }

func (r *Remote) measure(before, home time.Time) {
	local := before.Add(time.Since(before) / 2)
	r.offset.Store(int64(home.Sub(local)))
}

// Now is the home node's clock, as the share's clock is on NFS.
func (r *Remote) Now() time.Time { return time.Now().Add(time.Duration(r.offset.Load())) }

// sweep removes what a node killed mid-job left in its chunk store: the
// chunks of jobs the home node no longer has or in which this node's
// source part has ended (the source keeps them only until every active
// target acked), and half-written chunks in tmp/. It runs when the link
// comes up and with every prune.
func (r *Remote) sweep(ctx context.Context) {
	if des, err := os.ReadDir(filepath.Join(r.chunkDir, "data", r.self)); err == nil {
		for _, de := range des {
			j, err := r.GetJob(ctx, de.Name())
			if errors.Is(err, api.ErrUnknownJob) || (err == nil && r.sourceEnded(j)) {
				r.chunks.RemoveJobData(ctx, de.Name())
			}
		}
	}
	if des, err := os.ReadDir(filepath.Join(r.chunkDir, "tmp")); err == nil {
		for _, de := range des {
			if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > store.StaleTmp {
				os.Remove(filepath.Join(r.chunkDir, "tmp", de.Name()))
			}
		}
	}
}

func (r *Remote) sourceEnded(j *api.Job) bool {
	for _, p := range j.Participants {
		if p.Node == r.self && p.Role == api.RoleSource {
			return p.State.Terminal()
		}
	}
	return false
}

func (r *Remote) handlers() map[string]handler {
	return map[string]handler{
		"chunk.meta": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[chunkRef](raw)
			if err != nil {
				return nil, err
			}
			if err := r.ownChunk(a); err != nil {
				return nil, err
			}
			return waitMeta(ctx, r.chunks, a)
		},
		"chunk.open": func(ctx context.Context, raw json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
			a, err := args[chunkRef](raw)
			if err != nil {
				return nil, err
			}
			if err := r.ownChunk(a); err != nil {
				return nil, err
			}
			rc, err := r.chunks.OpenChunk(ctx, a.Src, a.ID, a.Seq)
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			if err := w.Header(nil); err != nil {
				return nil, err
			}
			_, err = io.Copy(w, rc)
			return nil, err
		},
		"stop": func(context.Context, json.RawMessage, io.Reader, *answerWriter) (any, error) {
			r.stopOnce.Do(func() { close(r.stop) })
			return nil, nil
		},
	}
}

func (r *Remote) ownChunk(a *chunkRef) error {
	if err := a.check(); err != nil {
		return err
	}
	if a.Src != r.self {
		return fmt.Errorf("%w: chunk of %s asked of %s", api.ErrBadPath, a.Src, r.self)
	}
	return nil
}

// recordTimeout bounds a record call. Record calls finish even when ctx
// is cancelled, as writes to a share do: a stopping worker writes its
// last status and leaves with the context it was stopped with.
var recordTimeout = 30 * time.Second

func record(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
}

// rec makes a record call (see record).
func (r *Remote) rec(ctx context.Context, method string, args, out any) error {
	ctx, cancel := record(ctx)
	defer cancel()
	return do(ctx, r.sess, method, args, out)
}

// Transport: everything goes to the home node.

func (r *Remote) Self() string { return r.self }

func (r *Remote) Register(ctx context.Context, info api.NodeInfo) error {
	return r.rec(ctx, "register", &info, nil)
}

func (r *Remote) Heartbeat(ctx context.Context, token string) (string, error) {
	var other string
	before := time.Now()
	err := r.rec(ctx, "heartbeat", map[string]string{"token": token}, &other)
	if err == nil {
		var now time.Time
		if r.rec(ctx, "now", nil, &now) == nil {
			r.measure(before, now)
		}
	}
	return other, err
}

func (r *Remote) Nodes(ctx context.Context) ([]api.Node, error) {
	var ns []api.Node
	err := r.rec(ctx, "nodes", nil, &ns)
	return ns, err
}

// StaleEntry is nfsmailbox's, asked of the home node (see engine.Forget).
func (r *Remote) StaleEntry(node string) (bool, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var s staleEntry
	if do(ctx, r.sess, "stale", map[string]string{"node": node}, &s) != nil {
		return false, 0
	}
	return s.Exists, s.Age
}

func (r *Remote) Unregister(ctx context.Context, node string) error {
	return r.rec(ctx, "unregister", map[string]string{"node": node}, nil)
}

// Leave removes this node from the home node's records and its staged
// chunks from local disk.
func (r *Remote) Leave(ctx context.Context) error {
	err := r.rec(ctx, "leave", nil, nil)
	os.RemoveAll(filepath.Join(r.chunkDir, "data", r.self))
	return err
}

func (r *Remote) Send(ctx context.Context, msg *transport.Message) error {
	return r.rec(ctx, "send", msg, nil)
}

func (r *Remote) AwaitReplies(ctx context.Context, reqID string, fn func(*transport.Reply) error) error {
	c, err := open(ctx, r.sess, "await", map[string]string{"req_id": reqID}, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.answer(ctx, nil); err != nil {
		return err
	}
	for {
		line, err := readLine(c.r)
		if err != nil {
			return c.ctxErr(ctx, err)
		}
		var l awaitLine
		if err := json.Unmarshal(line, &l); err != nil {
			return fmt.Errorf("ssh link: bad reply: %w", err)
		}
		if l.End {
			io.Copy(io.Discard, c)
			return decodeErr(&answer{E: l.E, C: l.C})
		}
		if l.Reply != nil {
			if err := fn(l.Reply); err != nil {
				return err // Close resets the call; the home node stops waiting
			}
		}
	}
}

func (r *Remote) Fetch(ctx context.Context) ([]*transport.Message, error) {
	var f fetched
	if err := do(ctx, r.sess, "fetch", &fetchArgs{Ack: r.got.Load()}, &f); err != nil {
		return nil, err // not acked: the home node sends these again
	}
	r.got.Store(f.Seq)
	return f.Msgs, decodeErr(&answer{E: f.E, C: f.C})
}

func (r *Remote) Reply(ctx context.Context, req *transport.Message, rep *transport.Reply) error {
	return r.rec(ctx, "reply", &replyArgs{*req, *rep}, nil)
}

func (r *Remote) Finish(ctx context.Context, msg *transport.Message, failed bool) error {
	return r.rec(ctx, "finish", &finishArgs{*msg, failed}, nil)
}

func (r *Remote) Prune(ctx context.Context, maxAge time.Duration) error {
	r.sweep(ctx)
	return r.rec(ctx, "prune", map[string]time.Duration{"max_age": maxAge}, nil)
}

// Store: records go to the home node; chunks stay here.

func (r *Remote) CreateJob(ctx context.Context, job *api.Job) error {
	return r.rec(ctx, "job.create", job, nil)
}

func (r *Remote) GetJob(ctx context.Context, id string) (*api.Job, error) {
	var j api.Job
	if err := r.rec(ctx, "job.get", map[string]string{"id": id}, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

func (r *Remote) ListJobs(ctx context.Context) ([]api.Job, error) {
	var js []api.Job
	err := r.rec(ctx, "job.list", nil, &js)
	return js, err
}

func (r *Remote) PutStatus(ctx context.Context, id string, st *api.JobStatus) error {
	return r.rec(ctx, "job.status", &statusArgs{id, *st}, nil)
}

func (r *Remote) RequestCancel(ctx context.Context, id string) error {
	return r.rec(ctx, "job.cancel", map[string]string{"id": id}, nil)
}

func (r *Remote) SetComplete(ctx context.Context, id string, chunks int) error {
	return r.rec(ctx, "job.complete", &completeArgs{id, chunks}, nil)
}

func (r *Remote) Detach(ctx context.Context, id string, d api.Detached) error {
	return r.rec(ctx, "job.detach", &detachArgs{id, d}, nil)
}

func (r *Remote) CreateChunk(ctx context.Context, id string, seq int) (transport.ChunkWriter, error) {
	return r.chunks.CreateChunk(ctx, id, seq)
}

func (r *Remote) ChunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error) {
	if src == r.self {
		return r.chunks.ChunkMeta(ctx, src, id, seq)
	}
	var meta []byte
	if err := do(ctx, r.sess, "chunk.meta", &chunkRef{src, id, seq}, &meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func (r *Remote) OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	if src == r.self {
		return r.chunks.OpenChunk(ctx, src, id, seq)
	}
	c, err := open(ctx, r.sess, "chunk.open", &chunkRef{src, id, seq}, nil)
	if err != nil {
		return nil, err
	}
	if err := c.answer(ctx, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (r *Remote) DeleteChunk(ctx context.Context, id string, seq int) error {
	return r.chunks.DeleteChunk(ctx, id, seq)
}

func (r *Remote) SetAcked(ctx context.Context, src, id, node string, n int) error {
	return r.rec(ctx, "ack.set", map[string]any{"src": src, "id": id, "node": node, "n": n}, nil)
}

func (r *Remote) Acked(ctx context.Context, src, id, node string) (int, error) {
	var n int
	err := r.rec(ctx, "ack.get", map[string]string{"src": src, "id": id, "node": node}, &n)
	return n, err
}

func (r *Remote) StagedBytes(ctx context.Context, id string) (int64, int64, error) {
	return r.chunks.StagedBytes(ctx, id)
}

// FreeBytes is the space left here, where the chunks are staged.
func (r *Remote) FreeBytes() (int64, error) { return r.chunks.FreeBytes() }

// RemoveJobData removes the chunks here and the acks on the home node.
func (r *Remote) RemoveJobData(ctx context.Context, id string) error {
	err := r.chunks.RemoveJobData(ctx, id)
	return errors.Join(err, r.rec(ctx, "data.remove", map[string]string{"id": id}, nil))
}

func (r *Remote) PruneJobs(ctx context.Context, maxAge time.Duration) error {
	return r.rec(ctx, "job.prune", map[string]time.Duration{"max_age": maxAge}, nil)
}

func (r *Remote) CreateBlob(ctx context.Context, name string) (transport.ChunkWriter, error) {
	pr, pw := io.Pipe()
	w := &blobWriter{pw: pw, done: make(chan error, 1)}
	go func() {
		c, err := open(ctx, r.sess, "blob.create", &blobArgs{name}, pr)
		if err == nil {
			err = c.answer(ctx, nil)
			c.Close()
		}
		pr.CloseWithError(err) // a writer still writing learns why
		w.done <- err
	}()
	return w, nil
}

func (r *Remote) OpenBlob(ctx context.Context, name string) (io.ReadCloser, error) {
	c, err := open(ctx, r.sess, "blob.open", &blobArgs{name}, nil)
	if err != nil {
		return nil, err
	}
	if err := c.answer(ctx, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (r *Remote) PruneBlobs(ctx context.Context, maxAge time.Duration) error {
	return r.rec(ctx, "blob.prune", map[string]time.Duration{"max_age": maxAge}, nil)
}

// blobWriter streams a blob to the home node, which publishes it when
// Commit has sent the end.
type blobWriter struct {
	pw   *io.PipeWriter
	done chan error
	err  error
	over bool
}

var errAborted = errors.New("blob write aborted")

func (w *blobWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *blobWriter) Commit([]byte) error {
	if w.over {
		return w.err
	}
	w.over = true
	w.pw.Close()
	w.err = <-w.done
	return w.err
}

func (w *blobWriter) Abort() {
	if !w.over {
		w.over = true
		w.pw.CloseWithError(errAborted)
		<-w.done
	}
}
