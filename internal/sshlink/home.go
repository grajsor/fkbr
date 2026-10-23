// Package sshlink lets nodes without a common share work as one swarm
// over ssh (docs/ssh.md). The home node keeps the swarm's records (the
// registry, inboxes, jobs, statuses, acks, sync plans) in a private
// directory with the share code of nfsmailbox; every other node is linked
// to it by one ssh session, on which it makes its record calls. Chunks
// stay on their source's own disk and go straight to each target over
// the links, through the home node's memory between two remote nodes.
package sshlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/sshlink/frame"
	"fkbr/internal/store"
	"fkbr/internal/transport"
)

// waitStep is how often a call that waits looks again on local disk.
// fetch waits up to fetchWait for a request, so a remote node hears of one
// at once without asking all the time. A chunk's meta is waited for only
// briefly: the receiver paces its own tries and checks the job between
// them (it may have ended, so no chunk is coming).
var (
	waitStep  = 10 * time.Millisecond
	fetchWait = time.Second
	metaWait  = 200 * time.Millisecond
)

// Home is the home node's end of every link.
type Home struct {
	dir  string // the private share
	self string
	opts store.Options
	log  *slog.Logger

	mu    sync.Mutex
	peers map[string]*peer
	// hosts is the link state by host, for Nodes and the TUI.
	hosts map[string]*hostState
	// stopping: Stop asked the linked nodes to stop, so their links end
	// as they should.
	stopping atomic.Bool
	// While Run runs: its context and options, and how to stop each
	// host's session (Add, Remove).
	runCtx  context.Context
	runOpts *SessionOptions
	keepers map[string]context.CancelFunc
	keeping sync.WaitGroup
	started chan struct{} // closed once Run has set the above
}

type peer struct {
	node string
	host string
	sess *frame.Session
	mb   *store.Dir // opened as this node
	out  outbox
	// stopped: Remove asked this node to stop.
	stopped atomic.Bool
}

// fetchBudget bounds the requests in one fetch answer, well below the
// line the other side reads (maxHeader); the rest wait for the next one.
// maxSendTries: a request that fails to arrive this often in a row, sent
// alone, is parked in failed/ (as store's maxReadTries).
const (
	fetchBudget  = 16 << 20
	maxSendTries = 20
)

// outbox is what the home node has claimed from a linked node's inbox
// and not yet seen arrive. store.Fetch hands a claimed request out once;
// over a link its answer can be lost while the link stays up (unreadable,
// or cut short), so the request stays here until the node's next fetch
// acks that answer, and goes out again if it does not. When the link
// ends, the next one opens the store anew, which hands them all out again.
type outbox struct {
	mu    sync.Mutex
	seq   uint64               // of the last answer
	sent  []*transport.Message // in it, until acked
	tries int                  // answers in a row whose requests were not acked
	held  []*transport.Message // claimed, not sent yet
}

// fetch answers a linked node's fetch that acks answer ack: what was not
// acked first (alone after a failure), then what was held back, else new
// requests, waiting up to fetchWait for one.
func (o *outbox) fetch(ctx context.Context, mb *store.Dir, ack uint64, log *slog.Logger) (*fetched, error) {
	o.mu.Lock() // also keeps mb.Fetch to one caller, as it must be
	defer o.mu.Unlock()
	if ack == o.seq {
		o.sent, o.tries = nil, 0
	} else if len(o.sent) > 0 {
		o.tries++
	}
	queue := append(append([]*transport.Message(nil), o.sent...), o.held...)
	if len(queue) > 0 && o.tries >= maxSendTries {
		log.Warn("ssh link: a request never arrived; parked in failed/", "node", mb.Self(), "id", queue[0].ID, "type", queue[0].Type)
		mb.Finish(ctx, queue[0], true)
		queue, o.tries = queue[1:], 0
	}
	var ferr error
	if len(queue) == 0 {
		deadline := time.Now().Add(fetchWait)
		for {
			queue, ferr = mb.Fetch(ctx)
			if len(queue) > 0 || ferr != nil || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(waitStep):
			}
		}
	}
	var send []*transport.Message
	size := 0
	for len(queue) > 0 {
		m := queue[0]
		b, err := json.Marshal(m)
		if err != nil || len(b) > maxHeader-64<<10 {
			log.Warn("ssh link: a request too big for the link; parked in failed/", "node", mb.Self(), "id", m.ID, "type", m.Type, "bytes", len(b))
			mb.Finish(ctx, m, true)
			queue = queue[1:]
			continue
		}
		if len(send) > 0 && (size+len(b) > fetchBudget || o.tries > 0) {
			break
		}
		send, size, queue = append(send, m), size+len(b), queue[1:]
	}
	o.seq++
	o.sent, o.held = send, queue
	return &fetched{Msgs: send, Seq: o.seq, E: errText(ferr), C: errorCode(ferr)}, nil
}

// NewHome uses dir (private, on local disk) as the swarm's share, with
// self as the home node's ID. The share is kept private (opts.Private).
func NewHome(dir, self string, opts store.Options, log *slog.Logger) *Home {
	opts.Private = true
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Home{dir: dir, self: self, opts: opts, log: log, peers: map[string]*peer{}, hosts: map[string]*hostState{}, started: make(chan struct{})}
}

// Open returns the home node's own transport and store: the mailbox on
// the private share, with chunk reads from linked nodes going to them.
func (h *Home) Open() (*HomeNode, error) {
	mb, err := store.Open(h.dir, h.self, h.opts)
	if err != nil {
		return nil, err
	}
	return &HomeNode{Dir: mb, h: h}, nil
}

// HomeNode is the home node's transport.Transport and transport.Store.
type HomeNode struct {
	*store.Dir
	h *Home
}

var (
	_ transport.Transport = (*HomeNode)(nil)
	_ transport.Store     = (*HomeNode)(nil)
)

func (n *HomeNode) ChunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error) {
	if src == n.h.self {
		return n.Dir.ChunkMeta(ctx, src, id, seq)
	}
	return n.h.chunkMeta(ctx, src, id, seq)
}

func (n *HomeNode) OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	if src == n.h.self {
		return n.Dir.OpenChunk(ctx, src, id, seq)
	}
	return n.h.openChunk(ctx, src, id, seq)
}

// Nodes adds each linked node's link state.
func (n *HomeNode) Nodes(ctx context.Context) ([]api.Node, error) {
	ns, err := n.Dir.Nodes(ctx)
	n.h.annotate(ns)
	return ns, err
}

func (h *Home) peer(node string) *peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.peers[node]
}

type chunkRef struct {
	Src string `json:"src"`
	ID  string `json:"id"`
	Seq int    `json:"seq"`
}

func (c *chunkRef) check() error {
	if !config.ValidID(c.Src) || !config.ValidID(c.ID) || c.Seq < 0 {
		return fmt.Errorf("%w: chunk %s/%s/%d", api.ErrBadPath, c.Src, c.ID, c.Seq)
	}
	return nil
}

// chunkMeta asks src, a linked node, for a chunk's meta.
func (h *Home) chunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error) {
	p := h.peer(src)
	if p == nil {
		return nil, linkDown(fmt.Errorf("%s is not linked", src))
	}
	var meta []byte
	if err := do(ctx, p.sess, "chunk.meta", &chunkRef{src, id, seq}, &meta); err != nil {
		return nil, err
	}
	return meta, nil
}

func (h *Home) openChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error) {
	p := h.peer(src)
	if p == nil {
		return nil, linkDown(fmt.Errorf("%s is not linked", src))
	}
	c, err := open(ctx, p.sess, "chunk.open", &chunkRef{src, id, seq}, nil)
	if err != nil {
		return nil, err
	}
	if err := c.answer(ctx, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

type hello struct {
	Node     string `json:"node"`
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Link     int    `json:"link"`
}

// linkVersion is what the calls over a link mean, on top of Protocol:
// 2 acks fetch answers (outbox). Builds before it sent none (0).
const linkVersion = 2

// ServeConn runs the home end of one link (an ssh session's pipes) until
// it ends. host names the link in logs and in Nodes.
func (h *Home) ServeConn(ctx context.Context, conn io.ReadWriteCloser, host string, opts frame.Options) error {
	sess, err := frame.Client(conn, opts)
	if err != nil {
		return err
	}
	defer sess.Close()
	if len(sess.Banner) > 0 {
		h.log.Info("ssh link: the remote shell printed before fkbr started", "host", host, "text", firstLine(sess.Banner))
	}
	caller := ctx // ctx below also ends with the session
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-sess.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	// The first call says who is at the other end.
	st, err := sess.Accept(ctx)
	if err != nil {
		return err
	}
	p, err := h.greet(ctx, st, sess, host)
	if err != nil {
		h.setState(host, "", "refused", err.Error())
		return err
	}
	h.log.Info("ssh link up", "host", host, "node", p.node)
	h.setState(host, p.node, "connected", "")
	defer func() {
		h.mu.Lock()
		if h.peers[p.node] == p {
			delete(h.peers, p.node)
		}
		h.mu.Unlock()
		h.setState(host, p.node, "down", errText(sess.Err()))
	}()
	serve(ctx, sess, h.handlers(p), h.log)
	<-sess.Done()
	switch {
	case h.stopping.Load() || p.stopped.Load():
		h.log.Info("ssh link closed: the node stopped", "host", host, "node", p.node)
	case caller.Err() == nil:
		h.log.Warn("ssh link down", "host", host, "node", p.node, "err", sess.Err())
	}
	return sess.Err()
}

func (h *Home) greet(ctx context.Context, st *frame.Stream, sess *frame.Session, host string) (*peer, error) {
	var p *peer
	serveOne(ctx, st, map[string]handler{"hello": func(_ context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
		a, err := args[hello](raw)
		if err != nil {
			return nil, err
		}
		if a.Protocol != api.ProtocolVersion || a.Link != linkVersion {
			return nil, fmt.Errorf("%s on %s speaks protocol %d (link %d), this one %d (link %d): run the same version on every node",
				a.Node, host, a.Protocol, a.Link, api.ProtocolVersion, linkVersion)
		}
		if a.Node == h.self {
			return nil, fmt.Errorf("%w: %s on %s has the home node's name; pass -node with another name there", api.ErrNotAllowed, a.Node, host)
		}
		mb, err := store.Open(h.dir, a.Node, h.opts)
		if err != nil {
			return nil, err
		}
		// One step: two hosts saying the same name at once must not
		// both get in.
		h.mu.Lock()
		if other := h.peers[a.Node]; other != nil {
			h.mu.Unlock()
			return nil, fmt.Errorf("%w: a node named %s is linked already (from %s)", api.ErrNotAllowed, a.Node, other.host)
		}
		p = &peer{node: a.Node, host: host, sess: sess, mb: mb}
		h.peers[a.Node] = p
		h.mu.Unlock()
		return mb.Now(), nil
	}}, h.log)
	if p == nil {
		return nil, fmt.Errorf("ssh link to %s: the other side did not say hello", host)
	}
	return p, nil
}

// Stop asks every linked node to stop, so each leaves while its link is
// still up, and waits for them up to wait.
func (h *Home) Stop(wait time.Duration) {
	h.stopping.Store(true)
	h.mu.Lock()
	var ps []*peer
	for _, p := range h.peers {
		ps = append(ps, p)
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(ctx, p.sess, "stop", nil, nil)
			select {
			case <-p.sess.Done():
			case <-ctx.Done():
			}
		}()
	}
	wg.Wait()
}

func (h *Home) handlers(p *peer) map[string]handler {
	mb := p.mb
	type node struct {
		Node string `json:"node"`
	}
	type jobID struct {
		ID string `json:"id"`
	}
	type age struct {
		MaxAge time.Duration `json:"max_age"`
	}
	type acked struct {
		Src  string `json:"src"`
		ID   string `json:"id"`
		Node string `json:"node"`
		N    int    `json:"n,omitempty"`
	}
	return map[string]handler{
		"now": func(context.Context, json.RawMessage, io.Reader, *answerWriter) (any, error) {
			return mb.Now(), nil
		},
		"register": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[api.NodeInfo](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Register(ctx, *a)
		},
		"heartbeat": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[struct {
				Token string `json:"token"`
			}](raw)
			if err != nil {
				return nil, err
			}
			return mb.Heartbeat(ctx, a.Token)
		},
		"nodes": func(ctx context.Context, _ json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			ns, err := mb.Nodes(ctx)
			h.annotate(ns)
			return ns, err
		},
		"stale": func(_ context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[node](raw)
			if err != nil {
				return nil, err
			}
			exists, age := mb.StaleEntry(a.Node)
			return staleEntry{exists, age}, nil
		},
		"unregister": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[node](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Unregister(ctx, a.Node)
		},
		"leave": func(ctx context.Context, _ json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			return nil, mb.Leave(ctx)
		},
		"send": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[transport.Message](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Send(ctx, a)
		},
		"await": func(ctx context.Context, raw json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
			a, err := args[struct {
				ReqID string `json:"req_id"`
			}](raw)
			if err != nil {
				return nil, err
			}
			if err := w.Header(nil); err != nil {
				return nil, err
			}
			err = mb.AwaitReplies(ctx, a.ReqID, func(r *transport.Reply) error { return writeJSONLine(w, awaitLine{Reply: r}) })
			end := awaitLine{End: true}
			if err != nil {
				end.E, end.C = err.Error(), errorCode(err)
			}
			writeJSONLine(w, end)
			return nil, errAnswered
		},
		"fetch": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[fetchArgs](raw)
			if err != nil {
				return nil, err
			}
			return p.out.fetch(ctx, mb, a.Ack, h.log)
		},
		"reply": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[replyArgs](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Reply(ctx, &a.Req, &a.Reply)
		},
		"finish": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[finishArgs](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Finish(ctx, &a.Msg, a.Failed)
		},
		"prune": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[age](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Prune(ctx, a.MaxAge)
		},
		"job.create": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[api.Job](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.CreateJob(ctx, a)
		},
		"job.get": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[jobID](raw)
			if err != nil {
				return nil, err
			}
			return mb.GetJob(ctx, a.ID)
		},
		"job.list": func(ctx context.Context, _ json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			return mb.ListJobs(ctx)
		},
		"job.status": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[statusArgs](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.PutStatus(ctx, a.ID, &a.St)
		},
		"job.cancel": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[jobID](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.RequestCancel(ctx, a.ID)
		},
		"job.complete": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[completeArgs](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.SetComplete(ctx, a.ID, a.Chunks)
		},
		"job.detach": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[detachArgs](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.Detach(ctx, a.ID, a.D)
		},
		"job.prune": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[age](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.PruneJobs(ctx, a.MaxAge)
		},
		"ack.set": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[acked](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.SetAcked(ctx, a.Src, a.ID, a.Node, a.N)
		},
		"ack.get": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[acked](raw)
			if err != nil {
				return nil, err
			}
			return mb.Acked(ctx, a.Src, a.ID, a.Node)
		},
		"data.remove": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[jobID](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.RemoveJobData(ctx, a.ID)
		},
		"blob.create": func(ctx context.Context, raw json.RawMessage, body io.Reader, _ *answerWriter) (any, error) {
			a, err := args[blobArgs](raw)
			if err != nil {
				return nil, err
			}
			w, err := mb.CreateBlob(ctx, a.Name)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(w, body); err != nil {
				w.Abort()
				return nil, err
			}
			return nil, w.Commit(nil)
		},
		"blob.open": func(ctx context.Context, raw json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
			a, err := args[blobArgs](raw)
			if err != nil {
				return nil, err
			}
			rc, err := mb.OpenBlob(ctx, a.Name)
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
		"blob.prune": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[age](raw)
			if err != nil {
				return nil, err
			}
			return nil, mb.PruneBlobs(ctx, a.MaxAge)
		},
		// Chunks of another node: the home node's own, or relayed from
		// the node that has them.
		"chunk.meta": func(ctx context.Context, raw json.RawMessage, _ io.Reader, _ *answerWriter) (any, error) {
			a, err := args[chunkRef](raw)
			if err != nil {
				return nil, err
			}
			if err := a.check(); err != nil {
				return nil, err
			}
			if a.Src == h.self {
				return waitMeta(ctx, mb, a)
			}
			return h.chunkMeta(ctx, a.Src, a.ID, a.Seq)
		},
		"chunk.open": func(ctx context.Context, raw json.RawMessage, _ io.Reader, w *answerWriter) (any, error) {
			a, err := args[chunkRef](raw)
			if err != nil {
				return nil, err
			}
			if err := a.check(); err != nil {
				return nil, err
			}
			var rc io.ReadCloser
			if a.Src == h.self {
				rc, err = mb.OpenChunk(ctx, a.Src, a.ID, a.Seq)
			} else {
				rc, err = h.openChunk(ctx, a.Src, a.ID, a.Seq)
			}
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
	}
}

// waitMeta answers a chunk-meta call from local disk, waiting up to
// metaWait for the chunk to be published.
func waitMeta(ctx context.Context, st transport.Store, a *chunkRef) ([]byte, error) {
	deadline := time.Now().Add(metaWait)
	for {
		meta, err := st.ChunkMeta(ctx, a.Src, a.ID, a.Seq)
		if !errors.Is(err, fs.ErrNotExist) || time.Now().After(deadline) {
			return meta, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitStep):
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type staleEntry struct {
	Exists bool          `json:"exists"`
	Age    time.Duration `json:"age"`
}

// fetchArgs acks the last fetch answer the node got (its Seq; see outbox).
type fetchArgs struct {
	Ack uint64 `json:"ack"`
}

type fetched struct {
	Msgs []*transport.Message `json:"msgs,omitempty"`
	Seq  uint64               `json:"seq"`
	E    string               `json:"e,omitempty"`
	C    string               `json:"c,omitempty"`
}

type awaitLine struct {
	Reply *transport.Reply `json:"reply,omitempty"`
	End   bool             `json:"end,omitempty"`
	E     string           `json:"e,omitempty"`
	C     string           `json:"c,omitempty"`
}

type replyArgs struct {
	Req   transport.Message `json:"req"`
	Reply transport.Reply   `json:"reply"`
}

type finishArgs struct {
	Msg    transport.Message `json:"msg"`
	Failed bool              `json:"failed"`
}

type statusArgs struct {
	ID string        `json:"id"`
	St api.JobStatus `json:"st"`
}

type completeArgs struct {
	ID     string `json:"id"`
	Chunks int    `json:"chunks"`
}

type detachArgs struct {
	ID string       `json:"id"`
	D  api.Detached `json:"d"`
}

type blobArgs struct {
	Name string `json:"name"`
}

// hostState is a host's link as the TUI and `nodes` show it.
type hostState struct {
	node  string
	state string
	err   string
}

func (h *Home) setState(host, node, state, err string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, kept := h.keepers[host]; h.keepers != nil && !kept {
		return // removed (Remove); its session is ending
	}
	hs := h.hosts[host]
	if hs == nil {
		hs = &hostState{}
		h.hosts[host] = hs
	}
	if node != "" {
		hs.node = node
	}
	hs.state, hs.err = state, err
}

// annotate sets Link on the nodes linked to the home node.
func (h *Home) annotate(ns []api.Node) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range ns {
		for host, hs := range h.hosts {
			if hs.node != ns[i].ID {
				continue
			}
			ns[i].Link = "ssh " + host + ": " + hs.state
			if hs.err != "" {
				ns[i].Link += ": " + hs.err
			}
		}
	}
}

// LinkState is one host's link.
type LinkState struct {
	Host, Node, State, Error string
}

// Links lists every host's link state, also of hosts whose node never
// came up (so their errors show somewhere).
func (h *Home) Links() []LinkState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []LinkState
	for host, hs := range h.hosts {
		out = append(out, LinkState{host, hs.node, hs.state, hs.err})
	}
	return out
}

func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
