// Package nfsmailbox implements transport.Transport over a directory shared
// by all nodes (normally an NFS mount). Layout under the share root:
//
//	protocol-version
//	tmp/                              every write starts here, then rename
//	nodes/<id>/info.json              registry entry
//	nodes/<id>/heartbeat              rewritten periodically; mtime = liveness
//	inbox/<id>/{new,cur,failed}/     requests addressed to <id>
//	reply/<id>/<req-id>/<seq>.json    replies to requests sent by <id>
//
// Each inbox has a single consumer (its node), so no locking is needed on
// the share. Liveness compares heartbeat mtimes with the mtime of a freshly
// written probe file; both are set by the NFS server, so host clock skew
// does not matter. See docs/protocol.md.
package nfsmailbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"fkbr/internal/api"
	"fkbr/internal/atomicfile"
	"fkbr/internal/config"
	"fkbr/internal/product"
	"fkbr/internal/transport"
)

// ErrRequesterGone means the reply directory no longer exists: the requester
// timed out or was cancelled, so there is nobody to answer.
var ErrRequesterGone = errors.New("requester gone")

type Options struct {
	// Poll is how often AwaitReplies checks for new reply parts.
	Poll time.Duration
	// OfflineAfter is the heartbeat age after which a node counts as offline.
	OfflineAfter time.Duration
	// Refresh compensates for NFS mounts that cache attributes and lookups
	// for long (e.g. actimeo=1800): before reading a directory, or a file
	// in it, that other nodes change, a tiny file is created and removed
	// in that directory. The client then revalidates the directory and its
	// entries, so renamed-in files and new names become visible at once.
	Refresh bool
}

type Mailbox struct {
	root      string
	self      string
	opts      Options
	recovered bool
	// claimed holds requests moved to cur/ but not yet handed out, with
	// how often reading them failed. Only Fetch (one goroutine) uses it.
	claimed map[string]int

	refreshMu   sync.Mutex
	refreshedAt map[string]time.Time
	refreshSeq  atomic.Uint64
	openedUp    sync.Once
	// offset is the share's clock minus this host's (ns), from mtimes the
	// NFS server set (see Now).
	offset atomic.Int64
	// registered: this mailbox is a node's (Register was called), so it has
	// its own folders on the share. A client that only looks (a CLI
	// command without a running node) creates none of them.
	registered atomic.Bool
}

// Now is the current time by the share's clock: job IDs and times made
// with it sort right whatever this host's clock says. It is measured when
// the share is opened and on every heartbeat.
func (m *Mailbox) Now() time.Time { return time.Now().Add(time.Duration(m.offset.Load())) }

// measureOffset records the server clock, as an mtime the server just set
// (st), against the local time around the write.
func (m *Mailbox) measureOffset(before time.Time, server time.Time) {
	local := before.Add(time.Since(before) / 2)
	m.offset.Store(int64(server.Sub(local)))
}

// refreshEvery throttles directory refreshes per directory.
const refreshEvery = 100 * time.Millisecond

// fresh makes other clients' changes in dir visible (see Options.Refresh).
func (m *Mailbox) fresh(dir string) {
	if !m.opts.Refresh {
		return
	}
	now := time.Now()
	m.refreshMu.Lock()
	if now.Sub(m.refreshedAt[dir]) < refreshEvery {
		m.refreshMu.Unlock()
		return
	}
	m.refreshedAt[dir] = now
	m.refreshMu.Unlock()
	p := filepath.Join(dir, fmt.Sprintf(".r-%s-%d", m.self, m.refreshSeq.Add(1)))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		f.Close()
		os.Remove(p)
		return
	}
	// dir may exist on the server while this client still caches it as
	// missing (looked up before another node created it). Refresh the
	// parent so that cached "does not exist" is dropped.
	if parent := filepath.Dir(dir); errors.Is(err, fs.ErrNotExist) && parent != dir && strings.HasPrefix(parent, m.root) {
		m.fresh(parent)
	}
}

// freshFile refreshes the directory containing path.
func (m *Mailbox) freshFile(path string) { m.fresh(filepath.Dir(path)) }

var _ transport.Transport = (*Mailbox)(nil)

// Open prepares the share layout for node self and checks the protocol
// version. The share root itself must already exist (it is the mount).
func Open(root, self string, opts Options) (*Mailbox, error) {
	if !config.ValidID(self) {
		return nil, fmt.Errorf("nfsmailbox: invalid node id %q", self)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("nfsmailbox: share root: %w", err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("nfsmailbox: share root %s is not a directory", root)
	}
	// Nodes running as other users create top-level entries here too.
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == uint32(os.Geteuid()) && fi.Mode().Perm() != shareDirMode {
		os.Chmod(root, shareDirMode)
	}
	if opts.Poll <= 0 {
		opts.Poll = 200 * time.Millisecond
	}
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = 30 * time.Second
	}
	m := &Mailbox{root: root, self: self, opts: opts, refreshedAt: map[string]time.Time{}, claimed: map[string]int{}}
	if err := m.layout(); err != nil {
		return nil, fmt.Errorf("nfsmailbox: %w", err)
	}
	if err := m.checkProtocol(); err != nil {
		return nil, err
	}
	before := time.Now()
	if now, err := m.serverNow(); err == nil {
		m.measureOffset(before, now)
	}
	return m, nil
}

func (m *Mailbox) checkProtocol() error {
	p := filepath.Join(m.root, "protocol-version")
	want := strconv.Itoa(api.ProtocolVersion)
	data, err := readFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		if err := m.write(p, []byte(want+"\n")); err != nil {
			return fmt.Errorf("nfsmailbox: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("nfsmailbox: %w", err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		return fmt.Errorf("share %s uses protocol %s, this %s speaks %s: run the same version on every node "+
			"(to switch, stop all nodes, empty the share directory, and start them with the new version)", m.root, got, product.Name, want)
	}
	return nil
}

func (m *Mailbox) Self() string { return m.self }

// layout creates the share's fixed directories and this node's own. Open
// calls it, and so does any write, probe or inbox read that finds one of
// them gone (someone cleaned up the share under running nodes), so the
// share heals instead of failing until every node restarts. It never
// recreates a job's directories: a pruned job stays gone.
func (m *Mailbox) layout() error {
	dirs := []string{"tmp", "nodes", "inbox", "reply"}
	if m.registered.Load() {
		dirs = append(dirs, m.inbox(m.self, "new"), m.inbox(m.self, "cur"), m.inbox(m.self, "failed"),
			filepath.Join("reply", m.self), filepath.Join("nodes", m.self))
	}
	for _, d := range dirs {
		if err := mkdir(m.path(d)); err != nil {
			return err
		}
	}
	return nil
}

// inbox returns the share-relative path of a node's inbox state directory.
func (m *Mailbox) inbox(node, state string) string {
	return filepath.Join("inbox", node, state)
}

func (m *Mailbox) path(parts ...string) string {
	return filepath.Join(append([]string{m.root}, parts...)...)
}

func (m *Mailbox) write(dest string, data []byte) error {
	err := atomicfile.Write(m.path("tmp"), dest, data, shareFileMode)
	if errors.Is(err, fs.ErrNotExist) && m.layout() == nil {
		err = atomicfile.Write(m.path("tmp"), dest, data, shareFileMode)
	}
	return err
}

func (m *Mailbox) writeJSON(dest string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return m.write(dest, data)
}

func (m *Mailbox) Register(_ context.Context, info api.NodeInfo) error {
	if info.ID != m.self {
		return fmt.Errorf("nfsmailbox: registering %q from node %q", info.ID, m.self)
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	m.registered.Store(true)
	if err := m.layout(); err != nil {
		return err
	}
	return m.write(m.path("nodes", m.self, "info.json"), data)
}

func (m *Mailbox) Heartbeat(_ context.Context, token string) (string, error) {
	p := m.path("nodes", m.self, "heartbeat")
	var other string
	m.freshFile(p)
	if b, err := readFile(p); err == nil {
		// Older versions wrote a timestamp; only tokens count.
		if t := strings.TrimSpace(string(b)); t != token && strings.HasPrefix(t, "w1 ") {
			other = t
		}
	}
	// Creating and writing a new file lets the NFS server stamp the mtime.
	before := time.Now()
	if err := m.write(p, []byte(token+"\n")); err != nil {
		return other, err
	}
	if fi, err := os.Stat(p); err == nil {
		m.measureOffset(before, fi.ModTime())
	}
	// The entry may have been removed or damaged under us (readers skip an
	// entry they cannot parse, so the node would be invisible): the caller
	// registers again.
	ok, err := m.listed(m.path("nodes", m.self), "info.json")
	if err == nil && !ok {
		return other, transport.ErrNotRegistered
	}
	if err == nil {
		var info api.NodeInfo
		if data, err := readFile(m.path("nodes", m.self, "info.json")); err == nil && (json.Unmarshal(data, &info) != nil || info.ID != m.self) {
			return other, transport.ErrNotRegistered
		}
	}
	return other, nil
}

// StaleEntry reports whether node has a registry folder that Nodes leaves
// out (its info.json is missing or cannot be read) and how long ago it
// last heartbeat (-1: never).
func (m *Mailbox) StaleEntry(node string) (bool, time.Duration) {
	if !config.ValidID(node) {
		return false, 0
	}
	m.fresh(m.path("nodes"))
	if _, err := os.Stat(m.path("nodes", node)); err != nil {
		return false, 0
	}
	now, err := m.serverNow()
	if err != nil {
		return false, 0
	}
	if fi, err := os.Stat(m.path("nodes", node, "heartbeat")); err == nil {
		return true, now.Sub(fi.ModTime())
	}
	return true, -1
}

// serverNow returns the current time according to the share's clock.
func (m *Mailbox) serverNow() (time.Time, error) {
	f, err := os.CreateTemp(m.path("tmp"), ".probe-"+m.self+"-*")
	if errors.Is(err, fs.ErrNotExist) && m.layout() == nil {
		f, err = os.CreateTemp(m.path("tmp"), ".probe-"+m.self+"-*")
	}
	if err != nil {
		return time.Time{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return time.Time{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func (m *Mailbox) Nodes(_ context.Context) ([]api.Node, error) {
	now, err := m.serverNow()
	if err != nil {
		return nil, fmt.Errorf("nfsmailbox: probe: %w", err)
	}
	m.fresh(m.path("nodes"))
	des, err := os.ReadDir(m.path("nodes"))
	if err != nil {
		return nil, err
	}
	var out []api.Node
	for _, de := range des {
		if !de.IsDir() || !config.ValidID(de.Name()) {
			continue
		}
		m.fresh(m.path("nodes", de.Name()))
		// An entry that cannot be read is left out, so one damaged or
		// half-created entry does not make every node fail.
		data, err := readFile(m.path("nodes", de.Name(), "info.json"))
		if err != nil {
			continue // never registered, or unreadable
		}
		var n api.Node
		if json.Unmarshal(data, &n.NodeInfo) != nil || n.ID != de.Name() {
			continue
		}
		n.Self = n.ID == m.self
		n.LastSeen = -1
		if fi, err := os.Stat(m.path("nodes", de.Name(), "heartbeat")); err == nil {
			n.LastSeen = now.Sub(fi.ModTime())
			// Small negative ages are attribute-cache noise; treat as fresh.
			n.Online = n.LastSeen < m.opts.OfflineAfter
			if n.LastSeen < 0 {
				n.LastSeen = 0
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func (m *Mailbox) Unregister(_ context.Context, node string) error {
	if !config.ValidID(node) {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, node)
	}
	if node == m.self {
		return fmt.Errorf("nfsmailbox: a node cannot unregister itself")
	}
	return m.remove(node)
}

func (m *Mailbox) Leave(context.Context) error { return m.remove(m.self) }

func (m *Mailbox) remove(node string) error {
	var errs []error
	// The registry entry goes first, so the node vanishes from listings
	// even if removing its data takes a while.
	for _, d := range []string{"nodes", "inbox", "reply", "data"} {
		errs = append(errs, os.RemoveAll(m.path(d, node)))
	}
	return errors.Join(errs...)
}

func (m *Mailbox) Send(_ context.Context, msg *transport.Message) error {
	if msg.From != m.self {
		return fmt.Errorf("nfsmailbox: message from %q sent by %q", msg.From, m.self)
	}
	if !config.ValidID(msg.To) {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, msg.To)
	}
	if ok, _ := m.listed(m.path("nodes", msg.To), "info.json"); !ok {
		return fmt.Errorf("%w: %q", api.ErrUnknownNode, msg.To)
	}
	// The reply directory must exist before the request is visible, so the
	// executor can tell "requester gone" from "not created yet".
	if !msg.Notify {
		if err := mkdir(m.path("reply", m.self, msg.ID)); err != nil {
			return err
		}
	}
	if err := mkdir(m.path(m.inbox(msg.To, "new"))); err != nil {
		return err
	}
	if err := m.writeJSON(m.path(m.inbox(msg.To, "new"), msg.ID+".json"), msg); err != nil {
		os.RemoveAll(m.path("reply", m.self, msg.ID))
		return err
	}
	return nil
}

func (m *Mailbox) AwaitReplies(ctx context.Context, reqID string, fn func(*transport.Reply) error) error {
	dir := m.path("reply", m.self, reqID)
	defer func() {
		os.RemoveAll(dir)
		if !m.registered.Load() {
			os.Remove(filepath.Dir(dir)) // a visitor leaves nothing (only if empty)
		}
	}()
	t := time.NewTicker(m.opts.Poll)
	defer t.Stop()
	for seq := 0; ; {
		data, err := m.readListed(dir, seqName(seq)) // see listed.go
		switch {
		case err == nil:
			var r transport.Reply
			if err := json.Unmarshal(data, &r); err != nil {
				return fmt.Errorf("nfsmailbox: reply %s/%d: %w", reqID, seq, err)
			}
			if r.Error != "" {
				return &api.CodedError{Msg: r.From + ": " + r.Error, Code: r.Code}
			}
			if err := fn(&r); err != nil {
				return err
			}
			if r.Final {
				return nil
			}
			seq++
			continue // the next part may already be there
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func seqName(seq int) string { return fmt.Sprintf("%08d.json", seq) }

// maxReadTries is how often Fetch tries to read a claimed request before
// parking it in failed/.
const maxReadTries = 20

// Fetch claims new requests (new/ -> cur/) and returns them. A request that
// cannot be claimed or read now is tried again on the next call; one bad
// file never holds up the others, and a claimed request is never dropped.
// The error reports problems, but the returned messages are still valid.
func (m *Mailbox) Fetch(_ context.Context) ([]*transport.Message, error) {
	var errs []error
	// After a restart, requests claimed but never finished are redelivered.
	if !m.recovered {
		cur, err := listJSON(m.path(m.inbox(m.self, "cur")))
		if err != nil {
			errs = append(errs, err)
		} else {
			for _, n := range cur {
				m.claimed[n] += 0
			}
			m.recovered = true
		}
	}
	m.fresh(m.path(m.inbox(m.self, "new")))
	fresh, err := listJSON(m.path(m.inbox(m.self, "new")))
	if err != nil {
		errs = append(errs, err)
	}
	for _, n := range fresh {
		err := os.Rename(m.path(m.inbox(m.self, "new"), n), m.path(m.inbox(m.self, "cur"), n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err) // still in new/; tried again next time
			continue
		}
		m.claimed[n] += 0
	}
	names := make([]string, 0, len(m.claimed))
	for n := range m.claimed {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*transport.Message
	for _, n := range names {
		p := m.path(m.inbox(m.self, "cur"), n)
		data, err := readFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			delete(m.claimed, n)
			continue
		}
		if err != nil {
			if m.claimed[n]++; m.claimed[n] >= maxReadTries {
				os.Rename(p, m.path(m.inbox(m.self, "failed"), n))
				delete(m.claimed, n)
			}
			errs = append(errs, err)
			continue
		}
		delete(m.claimed, n)
		var msg transport.Message
		// From and ID name the reply folder: a forged "../" in them would
		// reach outside the share's folder (security audit 2026-10-22).
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID+".json" != n ||
			!config.ValidID(msg.ID) || !config.ValidID(msg.From) || !config.ValidID(msg.To) {
			// Unreadable or renamed: park it where an operator can look.
			os.Rename(p, m.path(m.inbox(m.self, "failed"), n))
			continue
		}
		out = append(out, &msg)
	}
	if len(errs) > 0 {
		m.layout() // the inbox may have been removed under us
	}
	return out, errors.Join(errs...)
}

// pruneOrphanData removes this node's staged data for jobs whose record is
// gone. Normally the source removes it when the job ends, but a removal
// that races with another node's refresh file can leave the directory.
func (m *Mailbox) pruneOrphanData(cutoff time.Time) error {
	jobs, err := m.listNames(m.path("jobs")) // by listing: see listed.go
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	des, err := os.ReadDir(m.path("data", m.self))
	if err != nil {
		return nil
	}
	var errs []error
	for _, de := range des {
		if slices.Contains(jobs, de.Name()) {
			continue
		}
		if fi, err := de.Info(); err == nil && fi.ModTime().Before(cutoff) {
			errs = append(errs, os.RemoveAll(m.path("data", m.self, de.Name())))
		}
	}
	return errors.Join(errs...)
}

func listJSON(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		if n := de.Name(); strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Mailbox) Reply(_ context.Context, req *transport.Message, r *transport.Reply) error {
	if !config.ValidID(req.From) || !config.ValidID(req.ID) {
		return fmt.Errorf("nfsmailbox: no reply to %q request %q: invalid name", req.From, req.ID)
	}
	dir := m.path("reply", req.From, req.ID)
	m.freshFile(dir)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ErrRequesterGone
	}
	r.Protocol = api.ProtocolVersion
	r.ReqID = req.ID
	r.From = m.self
	return m.writeJSON(filepath.Join(dir, seqName(r.Seq)), r)
}

// Finish removes a processed request; a failed one is kept in failed/
// (until pruned) so an operator can see what went wrong.
func (m *Mailbox) Finish(_ context.Context, msg *transport.Message, failed bool) error {
	cur := m.path(m.inbox(m.self, "cur"), msg.ID+".json")
	if !failed {
		return os.Remove(cur)
	}
	return os.Rename(cur, m.path(m.inbox(m.self, "failed"), msg.ID+".json"))
}

// StaleTmp is how long a temp file on the share may go unmodified before
// any node removes it as left over from a crashed writer. Writers in
// progress touch their file continuously.
const StaleTmp = time.Hour

// Pruning compares mtimes, which the NFS server sets, with the server's
// clock (serverNow), never the local one: a host whose clock runs ahead
// would otherwise delete other nodes' files that are still in use.

func (m *Mailbox) Prune(_ context.Context, maxAge time.Duration) error {
	m.openedUp.Do(m.openUp) // once per process: new entries get the right modes
	now, err := m.serverNow()
	if err != nil {
		return fmt.Errorf("nfsmailbox: probe: %w", err)
	}
	cutoff := now.Add(-maxAge)
	var errs []error
	if des, err := os.ReadDir(m.path("tmp")); err == nil {
		stale := now.Add(-StaleTmp)
		for _, de := range des {
			if fi, err := de.Info(); err == nil && fi.ModTime().Before(stale) {
				errs = append(errs, os.Remove(m.path("tmp", de.Name())))
			}
		}
	}
	os.RemoveAll(m.path(m.inbox(m.self, "done"))) // used by older versions
	errs = append(errs, m.pruneOrphanData(cutoff))
	for _, d := range []string{
		m.path(m.inbox(m.self, "failed")),
		m.path("reply", m.self), // abandoned reply dirs (requester crashed)
	} {
		des, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue // not a registered node here (or not yet)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, de := range des {
			fi, err := de.Info()
			if err != nil || !fi.ModTime().Before(cutoff) {
				continue
			}
			errs = append(errs, os.RemoveAll(filepath.Join(d, de.Name())))
		}
	}
	return errors.Join(errs...)
}
