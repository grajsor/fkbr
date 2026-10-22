// Package engine is the headless core of fkbr. It implements api.Service
// for frontends and, when Run is called, acts as this node's worker: it
// heartbeats and executes requests that peers put in its inbox.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/config"
	"fkbr/internal/id"
	"fkbr/internal/localfs"
	"fkbr/internal/product"
	"fkbr/internal/transport"
)

const (
	DefaultListLimit = 1000
	MaxListLimit     = 50000
	// replyPageSize is how many entries go into one reply part on the share.
	replyPageSize = 2000
)

type Options struct {
	Version string
	Logger  *slog.Logger
}

type Engine struct {
	cfg  *config.Config
	fs   localfs.FS
	tr   transport.Transport
	st   transport.Store
	info api.NodeInfo
	log  *slog.Logger

	// applyMu makes ApplySync's check and mark of plan.Applied one step
	// for every client of this node (TUI, CLI, socket).
	applyMu sync.Mutex

	// Job execution state, used only by Run.
	jobsWG  sync.WaitGroup
	jobsMu  sync.Mutex
	running map[string]bool // role+"/"+job ID
	sendSem chan struct{}
	recvSem chan struct{}
	// leaveOnStop makes a clean stop of Run remove this node from the
	// registry (see LeaveOnStop).
	leaveOnStop atomic.Bool
	// ready is closed once Run has passed its start checks (see Ready).
	ready     chan struct{}
	readyOnce sync.Once

	hub hub
}

var _ api.Service = (*Engine)(nil)

func New(cfg *config.Config, fsys localfs.FS, tr transport.Transport, st transport.Store, opts Options) *Engine {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	host, _ := os.Hostname()
	return &Engine{
		cfg:     cfg,
		fs:      fsys,
		tr:      tr,
		st:      st,
		log:     log,
		running: map[string]bool{},
		ready:   make(chan struct{}),
		sendSem: make(chan struct{}, cfg.Transfer.MaxJobs),
		recvSem: make(chan struct{}, cfg.Transfer.MaxJobs),
		info: api.NodeInfo{
			ID:               cfg.NodeID,
			Hostname:         host,
			OS:               osRelease(),
			Version:          opts.Version,
			Protocol:         api.ProtocolVersion,
			Roles:            cfg.Roles,
			Roots:            fsys.Roots(),
			ChownCapable:     localfs.ProbeChown(cfg.StateDir),
			DACOverride:      localfs.ProbeDACOverride(cfg.StateDir),
			AcceptWritesFrom: cfg.AcceptWritesFrom,
			PID:              os.Getpid(),
			BootID:           bootID(),
			Features:         []string{api.FeatureExclude, api.FeatureMove},
		},
	}
}

func (e *Engine) Self() string { return e.cfg.NodeID }

// Share is the share directory this node runs on.
func (e *Engine) Share() string { return e.cfg.NFSRoot }

// LocalRoots are the directories this node shares, with their paths.
func (e *Engine) LocalRoots() []api.Root { return e.fs.Roots() }

// LeaveOnStop makes this node leave the share when Run stops cleanly, so a
// node that only runs while someone uses it (the TUI) is not left behind
// as an offline member of @all. It stays if a job still needs it, and
// when a daemon had registered it: then it is a member that happens to be
// run by a TUI for now (e.g. after the daemon crashed).
func (e *Engine) LeaveOnStop() { e.leaveOnStop.Store(true) }

// Ready is closed once Run is this node's worker: it holds the lock, is
// registered and heartbeating. A Run that is refused never closes it.
func (e *Engine) Ready() <-chan struct{} { return e.ready }

func (e *Engine) Close() error { return e.fs.Close() }

func (e *Engine) Nodes(ctx context.Context) ([]api.Node, error) {
	return e.tr.Nodes(ctx)
}

func (e *Engine) Forget(ctx context.Context, node string) error {
	if node == e.Self() {
		return fmt.Errorf("%w: cannot forget this node itself", api.ErrNotAllowed)
	}
	nodes, err := e.tr.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID != node {
			continue
		}
		if n.Online {
			return fmt.Errorf("%w: %s is online; stop it first", api.ErrNotAllowed, node)
		}
		return e.tr.Unregister(ctx, node)
	}
	// An entry nobody can read (damaged info.json) is not listed: forget
	// it too, once its heartbeat is stale.
	if s, ok := e.tr.(interface {
		StaleEntry(string) (bool, time.Duration)
	}); ok {
		if exists, age := s.StaleEntry(node); exists {
			if age >= 0 && age < e.cfg.OfflineAfter {
				return fmt.Errorf("%w: %s heartbeats (its registry entry cannot be read); stop it first", api.ErrNotAllowed, node)
			}
			return e.tr.Unregister(ctx, node)
		}
	}
	return fmt.Errorf("%w: %q", api.ErrUnknownNode, node)
}

func (e *Engine) List(ctx context.Context, req api.ListRequest) (*api.ListResult, error) {
	if req.Node == "" {
		req.Node = e.Self()
	}
	if req.Limit <= 0 {
		req.Limit = DefaultListLimit
	}
	if req.Limit > MaxListLimit {
		req.Limit = MaxListLimit
	}
	if req.Offset < 0 {
		return nil, fmt.Errorf("list: negative offset")
	}
	if req.Node == e.Self() {
		return e.listLocal(req)
	}
	return e.listRemote(ctx, req)
}

func (e *Engine) listLocal(req api.ListRequest) (*api.ListResult, error) {
	clean, err := localfs.CleanRel(req.Path)
	if err != nil {
		return nil, err
	}
	all, err := e.fs.List(req.Root, clean)
	if err != nil {
		return nil, err
	}
	res := &api.ListResult{Node: e.Self(), Root: req.Root, Path: clean, Offset: req.Offset, Total: len(all)}
	if req.Offset < len(all) {
		res.Entries = all[req.Offset:min(len(all), req.Offset+req.Limit)]
	}
	return res, nil
}

func (e *Engine) listRemote(ctx context.Context, req api.ListRequest) (*api.ListResult, error) {
	if err := e.requireOnline(ctx, req.Node); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.RequestTimeout)
	defer cancel()
	msg, err := e.newMessage(req.Node, transport.TypeList, req)
	if err != nil {
		return nil, err
	}
	if err := e.tr.Send(ctx, msg); err != nil {
		return nil, err
	}
	var res *api.ListResult
	err = e.tr.AwaitReplies(ctx, msg.ID, func(r *transport.Reply) error {
		var page api.ListResult
		if err := json.Unmarshal(r.Body, &page); err != nil {
			return err
		}
		if res == nil {
			res = &page
			return nil
		}
		res.Entries = append(res.Entries, page.Entries...)
		return nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("list on %s: no answer within %s (is its worker running?)", req.Node, e.cfg.RequestTimeout)
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (e *Engine) requireOnline(ctx context.Context, node string) error {
	nodes, err := e.tr.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.ID != node {
			continue
		}
		if !n.Online {
			return fmt.Errorf("%w: %s (last seen %s ago)", api.ErrNodeOffline, node, n.LastSeen.Round(time.Second))
		}
		return nil
	}
	return fmt.Errorf("%w: %q", api.ErrUnknownNode, node)
}

// now is the share's clock where the transport knows it (a host's own
// clock may be off: IDs and job times must sort with everyone else's).
func (e *Engine) now() time.Time {
	if c, ok := e.tr.(interface{ Now() time.Time }); ok {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Engine) newMessage(to, typ string, body any) (*transport.Message, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	now := e.now()
	return &transport.Message{
		Protocol: api.ProtocolVersion,
		ID:       id.At(now),
		Type:     typ,
		From:     e.Self(),
		To:       to,
		Created:  now,
		Body:     b,
	}, nil
}

// Run is the node worker. It registers the node, heartbeats, and serves the
// inbox until ctx is cancelled. Only one worker may run per node; a lock in
// the local state dir enforces that.
func (e *Engine) Run(ctx context.Context) error {
	lock, waitedFor, err := lockWorker(e.cfg.StateDir, e.log)
	if err != nil {
		return err
	}
	defer lock.release()
	if waitedFor > 0 {
		// It released the lock; let it exit before the registry check
		// below takes its entry for a live second worker.
		for i := 0; i < 50 && processAlive(waitedFor); i++ {
			time.Sleep(100 * time.Millisecond)
		}
	}
	ctx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	// Runs after the jobs below have paused, while the lock is still held,
	// so a new worker for this node cannot register in between.
	stopped := false
	defer func() {
		if stopped && e.leaveOnStop.Load() && e.info.Transient {
			e.leave()
		}
	}()
	lockCheck := time.NewTicker(lockCheckInterval)
	defer lockCheck.Stop()
	rootCheck := time.NewTicker(rootCheckInterval)
	defer rootCheck.Stop()

	// Refuse to take over a node ID another live host or process is using:
	// with IDs defaulting to hostnames, two machines could otherwise
	// collide, and two users on one host would (see instance.go).
	e.info.Started = time.Now().UTC()
	e.info.Instance = newToken(e.info.Started)
	nodes, err := e.tr.Nodes(ctx)
	for _, n := range nodes {
		if n.ID != e.Self() || !n.Online {
			continue
		}
		if n.Hostname != e.info.Hostname {
			return fmt.Errorf("%w: %q is in use by host %s; pass -node with another name", ErrNodeIDTaken, n.ID, n.Hostname)
		}
		if e.runningHere(n) {
			return fmt.Errorf("%w: %q is already running on this host (pid %d, user %s, since %s); stop it, or pass -node with another name",
				ErrNodeIDTaken, n.ID, n.PID, processUser(n.PID), n.Started.Local().Format(time.DateTime))
		}
	}
	liveInstances.Store(e.info.Instance, true)
	defer liveInstances.Delete(e.info.Instance)
	// A TUI only visits unless a daemon registered this node before. If the
	// registry cannot be read, assume it did.
	e.info.Transient = false
	if e.leaveOnStop.Load() && err == nil {
		e.info.Transient = true
		for _, n := range nodes {
			if n.ID == e.Self() && !n.Transient {
				e.info.Transient = false
			}
		}
	}
	if err := e.tr.Register(ctx, e.info); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	// A token found now was left by the entry the check above allowed (a
	// crashed worker); only tokens written after this one count. An entry
	// damaged or removed since Register is written again, as the
	// heartbeat loop does.
	_, err = e.tr.Heartbeat(ctx, e.info.Instance)
	for try := 0; errors.Is(err, transport.ErrNotRegistered) && try < 3; try++ {
		e.log.Warn("this node's registry entry is missing or unreadable; registering again")
		if err = e.tr.Register(ctx, e.info); err == nil {
			_, err = e.tr.Heartbeat(ctx, e.info.Instance)
		}
	}
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	e.log.Info("worker started", "node", e.Self(), "roots", len(e.info.Roots), "chown", e.info.ChownCapable)
	e.readyOnce.Do(func() { close(e.ready) })

	// The heartbeat has its own goroutine: a slow listing, read or prune in
	// the loop below must not make this node look offline. It stops after
	// the jobs have paused (defers run in reverse) and before leaving.
	hbCtx, hbStop := context.WithCancel(context.WithoutCancel(ctx))
	hbDone := make(chan struct{})
	conflict := make(chan error, 1)
	go e.heartbeat(hbCtx, hbDone, conflict)
	defer func() { hbStop(); <-hbDone }()
	// Prune at start (a node may have been off for days) and then every
	// half keep_done, so nothing outlives keep_done by much.
	e.prune(ctx)
	prune := time.NewTicker(max(time.Minute, min(time.Hour, e.cfg.KeepDone/2)))
	defer prune.Stop()
	poll := time.NewTimer(0)
	defer poll.Stop()
	lastActivity := time.Now()
	// Jobs stop with ctx and resume from the share when the worker restarts.
	// When the worker stops on its own (lock lost, node ID taken), runCancel
	// stops them first.
	defer func() { runCancel(); e.jobsWG.Wait() }()

	for {
		select {
		case <-ctx.Done():
			e.log.Info("worker stopping", "node", e.Self())
			lock.state("stopping") // a new worker waits for us (lockWorker)
			stopped = true
			return nil
		case err := <-conflict:
			// Not stopped: the registry entry is the other worker's now.
			e.log.Error("node ID in use by another worker; stopping this worker", "err", err)
			return err
		case <-lockCheck.C:
			if err := lock.maintain(); err != nil {
				e.log.Error("worker lock lost; stopping this worker", "err", err)
				return err
			}
		case <-rootCheck.C:
			if err := e.fs.CheckRoots(); err != nil {
				// Stopped on purpose: a TUI's node leaves the share.
				e.log.Error("shared directory gone; stopping this node", "err", err)
				stopped = true
				return fmt.Errorf("%w; this node stopped (start %s again from the directory to share)", err, product.Name)
			}
		case <-prune.C:
			e.prune(ctx)
		case <-poll.C:
			msgs, err := e.tr.Fetch(ctx)
			if err != nil {
				e.log.Warn("fetch failed", "err", err)
			}
			for _, m := range msgs {
				e.handle(ctx, m)
			}
			if len(msgs) > 0 {
				lastActivity = time.Now()
			}
			interval := e.cfg.Poll.Active
			if time.Since(lastActivity) > e.cfg.Poll.IdleAfter {
				interval = e.cfg.Poll.Idle
			}
			poll.Reset(interval)
		}
	}
}

// heartbeat refreshes this node's heartbeat until ctx ends. If another
// worker uses the node ID and this one must yield, it reports that on
// conflict and stops.
func (e *Engine) heartbeat(ctx context.Context, done chan<- struct{}, conflict chan<- error) {
	defer close(done)
	t := time.NewTicker(e.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			other, err := e.tr.Heartbeat(ctx, e.info.Instance)
			if errors.Is(err, transport.ErrNotRegistered) {
				e.log.Warn("this node's registry entry is missing or unreadable; registering again")
				err = e.tr.Register(ctx, e.info)
			}
			if err != nil {
				e.log.Warn("heartbeat failed", "err", err)
			}
			if other == "" {
				continue
			}
			if err := e.heartbeatConflict(other); err != nil {
				conflict <- err
				return
			}
			if err := e.tr.Register(ctx, e.info); err != nil {
				e.log.Warn("re-register failed", "err", err)
			}
		}
	}
}

// prune removes finished records older than keep_done from the share.
func (e *Engine) prune(ctx context.Context) {
	if err := e.Prune(ctx, e.cfg.KeepDone); err != nil {
		e.log.Warn("prune failed", "err", err)
	}
	e.sweepStaging(ctx)
}

func (e *Engine) Prune(ctx context.Context, keep time.Duration) error {
	return errors.Join(
		e.tr.Prune(ctx, keep),
		e.st.PruneJobs(ctx, keep),
		e.st.PruneBlobs(ctx, keep),
		e.forgetStale(ctx),
	)
}

// forgetStale removes nodes offline for longer than forget_after.
func (e *Engine) forgetStale(ctx context.Context) error {
	if e.cfg.ForgetAfter <= 0 {
		return nil
	}
	nodes, err := e.tr.Nodes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range nodes {
		if n.ID == e.Self() || n.Online || n.LastSeen < e.cfg.ForgetAfter {
			continue
		}
		e.log.Info("forgetting node offline too long", "node", n.ID, "last_seen", n.LastSeen.Round(time.Minute))
		errs = append(errs, e.tr.Unregister(ctx, n.ID))
	}
	return errors.Join(errs...)
}

// leave removes this node from the share unless an unfinished job still
// needs it; such a node stays registered and resumes when started again.
func (e *Engine) leave() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	jobs, err := e.Jobs(ctx)
	if err != nil {
		e.log.Warn("not leaving the share: cannot list jobs", "err", err)
		return
	}
	if id := neededBy(jobs, e.Self()); id != "" {
		e.log.Info("staying registered: an unfinished job needs this node", "job", id)
		return
	}
	if err := e.tr.Leave(ctx); err != nil {
		e.log.Warn("leaving the share failed", "err", err)
		return
	}
	e.log.Info("left the share", "node", e.Self())
}

// neededBy returns an unfinished job in which node is the source or a
// target that has not been detached, or "".
func neededBy(jobs []api.Job, node string) string {
	for _, j := range jobs {
		if j.State.Terminal() {
			continue
		}
		if j.Request.SrcNode == node || (slices.Contains(j.Request.DstNodes, node) && !j.IsDetached(node)) {
			return j.ID
		}
	}
	return ""
}

func (e *Engine) handle(ctx context.Context, m *transport.Message) {
	log := e.log.With("req", m.ID, "type", m.Type, "from", m.From)
	if m.Type == transport.TypeSend || m.Type == transport.TypeReceive {
		e.startJob(ctx, m)
		return
	}
	if m.Type == transport.TypeRemove || m.Type == transport.TypeScan {
		// May delete a large tree: don't hold up the inbox meanwhile.
		e.jobsWG.Add(1)
		go func() {
			defer e.jobsWG.Done()
			e.finishRequest(ctx, log, m, e.dispatch(ctx, m))
		}()
		return
	}
	e.finishRequest(ctx, log, m, e.dispatch(ctx, m))
}

// finishRequest sends an error reply if needed and moves the request out
// of the inbox.
func (e *Engine) finishRequest(ctx context.Context, log *slog.Logger, m *transport.Message, err error) {
	if err != nil {
		log.Info("request failed", "err", err)
		if !m.Notify {
			if rerr := e.tr.Reply(ctx, m, &transport.Reply{Final: true, Error: err.Error(), Code: api.ErrorCode(err)}); rerr != nil {
				log.Debug("error reply not delivered", "err", rerr)
			}
		}
	} else {
		log.Debug("request done")
	}
	if ferr := e.tr.Finish(ctx, m, err != nil); ferr != nil {
		log.Warn("finish failed", "err", ferr)
	}
}

func (e *Engine) dispatch(ctx context.Context, m *transport.Message) error {
	if m.Protocol != api.ProtocolVersion {
		return fmt.Errorf("unsupported protocol %d", m.Protocol)
	}
	if m.To != e.Self() {
		return fmt.Errorf("request addressed to %q delivered to %q", m.To, e.Self())
	}
	switch m.Type {
	case transport.TypeList:
		return e.serveList(ctx, m)
	case transport.TypeMkdir, transport.TypeRemove, transport.TypeRead:
		return e.serveFileOp(ctx, m)
	case transport.TypeScan:
		return e.serveScan(ctx, m)
	default:
		return fmt.Errorf("unknown request type %q", m.Type)
	}
}

func (e *Engine) serveList(ctx context.Context, m *transport.Message) error {
	var req api.ListRequest
	if err := json.Unmarshal(m.Body, &req); err != nil {
		return fmt.Errorf("bad list request: %w", err)
	}
	req.Node = e.Self()
	res, err := e.List(ctx, req)
	if err != nil {
		return err
	}
	// Split into parts so no single file on the share gets huge.
	entries := res.Entries
	for seq := 0; ; seq++ {
		n := min(len(entries), replyPageSize)
		page := *res
		page.Entries = entries[:n]
		entries = entries[n:]
		body, err := json.Marshal(&page)
		if err != nil {
			return err
		}
		if err := e.tr.Reply(ctx, m, &transport.Reply{Seq: seq, Final: len(entries) == 0, Body: body}); err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
	}
}

// ErrNodeIDTaken: another host is online under this node's ID.
var ErrNodeIDTaken = errors.New("node ID taken")

// ErrWorkerRunning: another process already runs this node's worker.
var ErrWorkerRunning = errors.New("another " + product.Name + " worker is already running for this node")

// ErrLockLost: the worker lock file disappeared and another process took
// its place before this worker could take it back.
var ErrLockLost = errors.New("worker lock lost to another process")

// Runtime-file upkeep. /tmp is cleaned by systemd-tmpfiles (10 days on
// many distributions), so the worker keeps its lock (and dir) fresh, and re-takes the
// lock path if it vanishes anyway.
var (
	lockCheckInterval = 30 * time.Second
	lockTouchInterval = 10 * time.Minute
)

// rootCheckInterval is how often the worker checks that its shared
// directories are still where they were (localfs.CheckRoots).
var rootCheckInterval = 5 * time.Second

// workerLock is an exclusive flock in the local state dir. Local-disk flock
// is reliable, unlike locks on the NFS share.
type workerLock struct {
	path      string
	files     []*os.File // every fd we hold a lock on (old ones after a re-take)
	ino       uint64
	lastTouch time.Time
}

func lockWorker(stateDir string, log *slog.Logger) (l *workerLock, waitedFor int, err error) {
	if err := localfs.PrivateDir(stateDir); err != nil {
		return nil, 0, err
	}
	l = &workerLock{path: filepath.Join(stateDir, "worker.lock"), lastTouch: time.Now()}
	// A worker that is stopping (SIGTERM; it pauses its jobs, which can
	// take a while) holds the lock a little longer: wait for it, so a
	// restart right after a stop does not fail.
	var deadline, nextLog time.Time
	for {
		err = l.take()
		if !errors.Is(err, ErrLockLost) {
			break
		}
		pid, stopping := lockHolder(l.path)
		if !stopping {
			return nil, 0, fmt.Errorf("%w (lock %s)", ErrWorkerRunning, l.path)
		}
		now := time.Now()
		if deadline.IsZero() {
			deadline = now.Add(stopWait)
		}
		if now.After(deadline) {
			return nil, 0, fmt.Errorf("%w: it has been stopping for %s (pid %d; lock %s)", ErrWorkerRunning, stopWait, pid, l.path)
		}
		if now.After(nextLog) {
			log.Info("waiting for the previous worker to stop", "pid", pid)
			nextLog = now.Add(10 * time.Second)
		}
		waitedFor = pid
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		return nil, 0, err
	}
	return l, waitedFor, nil
}

// stopWait is how long a new worker waits for a stopping one.
var stopWait = 2 * time.Minute

// lockHolder reads the lock file: the holder's PID and whether it said it
// is stopping (see workerLock.state).
func lockHolder(path string) (pid int, stopping bool) {
	b, _ := os.ReadFile(path)
	f := strings.Fields(string(b))
	if len(f) == 2 {
		pid, _ = strconv.Atoi(f[1])
		return pid, f[0] == "stopping"
	}
	return 0, false
}

// state writes "running <pid>" or "stopping <pid>" into the lock file.
func (l *workerLock) state(s string) {
	if len(l.files) == 0 {
		return
	}
	f := l.files[len(l.files)-1]
	if f.Truncate(0) == nil {
		f.WriteAt([]byte(fmt.Sprintf("%s %d\n", s, os.Getpid())), 0)
	}
}

// take opens the lock path and flocks it, failing with ErrLockLost if
// another process holds it.
func (l *workerLock) take() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return ErrLockLost
	}
	var st, now syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		f.Close()
		return err
	}
	// The worker we waited for may have removed the path before it let
	// go: then this lock is on a deleted file. Take the path again.
	if err := syscall.Stat(l.path, &now); err != nil || now.Ino != st.Ino {
		f.Close()
		return l.take()
	}
	l.files, l.ino = append(l.files, f), st.Ino
	l.state("running")
	return nil
}

// maintain refreshes the lock's timestamps now and then, and takes the
// path back if the file was deleted or replaced.
func (l *workerLock) maintain() error {
	var st syscall.Stat_t
	if err := syscall.Stat(l.path, &st); err != nil || st.Ino != l.ino {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
			return err
		}
		if err := l.take(); err != nil {
			return err
		}
	}
	if time.Since(l.lastTouch) >= lockTouchInterval {
		now := time.Now()
		os.Chtimes(l.path, now, now)
		os.Chtimes(filepath.Dir(l.path), now, now)
		l.lastTouch = now
	}
	return nil
}

func (l *workerLock) release() {
	for _, f := range l.files {
		f.Close()
	}
}

// osRelease returns a short OS description, preferring /etc/redhat-release.
func osRelease() string {
	if b, err := os.ReadFile("/etc/redhat-release"); err == nil {
		return strings.TrimSpace(string(b))
	}
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "linux"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "linux"
}
