package engine

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"fkbr/internal/api"
)

// eventInterval is how often the hub polls the share while anyone is
// subscribed. The share has no change notifications, so polling it once
// here is cheaper than every frontend polling on its own.
const eventInterval = time.Second

// afterSnapshot runs in Subscribe between the snapshot and joining the hub;
// tests use it to make another subscriber leave there.
var afterSnapshot = func() {}

// hub fans out change events to subscribers. It polls only while there is
// at least one subscriber.
type hub struct {
	mu     sync.Mutex
	subs   map[chan api.Event]bool
	cancel context.CancelFunc
}

func (e *Engine) Subscribe(ctx context.Context) (<-chan api.Event, error) {
	h := &e.hub
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan api.Event]bool{}
	}
	h.mu.Unlock()

	// The initial snapshot goes to this subscriber only, before it joins.
	nodes, jobs, err := e.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	afterSnapshot()
	// Room for all of the snapshot and then for the changes after it: what
	// does not fit would be counted as seen by pollEvents and never sent.
	ch := make(chan api.Event, len(nodes)+len(jobs)+256)
	now := time.Now().UTC()
	for i := range nodes {
		trySend(ch, api.Event{Type: api.EventNode, Time: now, Node: &nodes[i]})
	}
	for i := range jobs {
		trySend(ch, api.Event{Type: api.EventJob, Time: now, Job: &jobs[i], JobID: jobs[i].ID})
	}

	h.mu.Lock()
	h.subs[ch] = true
	// Judged here, not before the snapshot: a subscriber that was the last
	// one may have left meanwhile and stopped the poller.
	if h.cancel == nil {
		pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		h.cancel = cancel
		go e.pollEvents(pctx, nodes, jobs)
	}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.mu.Lock()
		delete(h.subs, ch)
		if len(h.subs) == 0 && h.cancel != nil {
			h.cancel()
			h.cancel = nil
		}
		h.mu.Unlock()
		close(ch)
	}()
	return ch, nil
}

// trySend drops the event if the subscriber is not keeping up.
func trySend(ch chan api.Event, ev api.Event) {
	select {
	case ch <- ev:
	default:
	}
}

func (e *Engine) broadcast(ev api.Event) {
	e.hub.mu.Lock()
	defer e.hub.mu.Unlock()
	for ch := range e.hub.subs {
		trySend(ch, ev)
	}
}

func (e *Engine) snapshot(ctx context.Context) ([]api.Node, []api.Job, error) {
	nodes, err := e.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	jobs, err := e.Jobs(ctx)
	return nodes, jobs, err
}

// nodeKey changes when anything but LastSeen changes.
func nodeKey(n api.Node) string {
	n.LastSeen = 0
	b, _ := json.Marshal(n)
	return string(b)
}

func jobKey(j api.Job) string {
	b, _ := json.Marshal(j)
	return string(b)
}

func (e *Engine) pollEvents(ctx context.Context, nodes []api.Node, jobs []api.Job) {
	seenNodes := map[string]string{}
	for _, n := range nodes {
		seenNodes[n.ID] = nodeKey(n)
	}
	seenJobs := map[string]string{}
	for _, j := range jobs {
		seenJobs[j.ID] = jobKey(j)
	}
	t := time.NewTicker(eventInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		nodes, jobs, err := e.snapshot(ctx)
		if err != nil {
			e.log.Debug("event poll failed", "err", err)
			continue
		}
		now := time.Now().UTC()
		for i, n := range nodes {
			if k := nodeKey(n); seenNodes[n.ID] != k {
				seenNodes[n.ID] = k
				e.broadcast(api.Event{Type: api.EventNode, Time: now, Node: &nodes[i]})
			}
		}
		here := map[string]bool{}
		for _, n := range nodes {
			here[n.ID] = true
		}
		for id := range seenNodes {
			if !here[id] {
				delete(seenNodes, id)
				e.broadcast(api.Event{Type: api.EventNodeGone, Time: now, NodeID: id})
			}
		}
		present := map[string]bool{}
		for i, j := range jobs {
			present[j.ID] = true
			if k := jobKey(j); seenJobs[j.ID] != k {
				seenJobs[j.ID] = k
				e.broadcast(api.Event{Type: api.EventJob, Time: now, Job: &jobs[i], JobID: j.ID})
			}
		}
		for id := range seenJobs {
			if !present[id] {
				delete(seenJobs, id)
				e.broadcast(api.Event{Type: api.EventJobGone, Time: now, JobID: id})
			}
		}
	}
}
