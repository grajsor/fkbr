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

	refreshMu   sync.Mutex
	refreshedAt map[string]time.Time
	refreshSeq  atomic.Uint64
	openedUp    sync.Once
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
	m := &Mailbox{root: root, self: self, opts: opts, refreshedAt: map[string]time.Time{}}
	for _, d := range []string{
		"tmp", "nodes", "inbox", "reply",
		m.inbox(self, "new"), m.inbox(self, "cur"), m.inbox(self, "failed"),
		filepath.Join("reply", self),
		filepath.Join("nodes", self),
	} {
		if err := mkdir(filepath.Join(root, d)); err != nil {
			return nil, fmt.Errorf("nfsmailbox: %w", err)
		}
	}
	if err := m.checkProtocol(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Mailbox) checkProtocol() error {
	p := filepath.Join(m.root, "protocol-version")
	want := strconv.Itoa(api.ProtocolVersion)
	data, err := os.ReadFile(p)
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
		return fmt.Errorf("nfsmailbox: share uses protocol %s, this binary speaks %s", got, want)
	}
	return nil
}

func (m *Mailbox) Self() string { return m.self }

// inbox returns the share-relative path of a node's inbox state directory.
func (m *Mailbox) inbox(node, state string) string {
	return filepath.Join("inbox", node, state)
}

func (m *Mailbox) path(parts ...string) string {
	return filepath.Join(append([]string{m.root}, parts...)...)
}

func (m *Mailbox) write(dest string, data []byte) error {
	return atomicfile.Write(m.path("tmp"), dest, data, shareFileMode)
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
	return m.write(m.path("nodes", m.self, "info.json"), data)
}

func (m *Mailbox) Heartbeat(_ context.Context) error {
	// Creating and writing a new file lets the NFS server stamp the mtime.
	return m.write(m.path("nodes", m.self, "heartbeat"), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"))
}

// serverNow returns the current time according to the share's clock.
func (m *Mailbox) serverNow() (time.Time, error) {
	f, err := os.CreateTemp(m.path("tmp"), ".probe-"+m.self+"-*")
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
		data, err := os.ReadFile(m.path("nodes", de.Name(), "info.json"))
		if errors.Is(err, fs.ErrNotExist) {
			continue // directory created but never registered
		}
		if err != nil {
			return nil, err
		}
		var n api.Node
		if err := json.Unmarshal(data, &n.NodeInfo); err != nil {
			return nil, fmt.Errorf("nfsmailbox: node %s: info.json: %w", de.Name(), err)
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
	m.fresh(m.path("nodes", msg.To))
	if _, err := os.Stat(m.path("nodes", msg.To, "info.json")); err != nil {
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
	defer os.RemoveAll(dir)
	t := time.NewTicker(m.opts.Poll)
	defer t.Stop()
	for seq := 0; ; {
		m.fresh(dir)
		data, err := os.ReadFile(filepath.Join(dir, seqName(seq)))
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

func (m *Mailbox) Fetch(_ context.Context) ([]*transport.Message, error) {
	var names []string
	// After a restart, requests claimed but never finished are redelivered.
	if !m.recovered {
		cur, err := listJSON(m.path(m.inbox(m.self, "cur")))
		if err != nil {
			return nil, err
		}
		names = append(names, cur...)
		m.recovered = true
	}
	m.fresh(m.path(m.inbox(m.self, "new")))
	fresh, err := listJSON(m.path(m.inbox(m.self, "new")))
	if err != nil {
		return nil, err
	}
	for _, n := range fresh {
		err := os.Rename(m.path(m.inbox(m.self, "new"), n), m.path(m.inbox(m.self, "cur"), n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	var out []*transport.Message
	for _, n := range names {
		p := m.path(m.inbox(m.self, "cur"), n)
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var msg transport.Message
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID+".json" != n {
			// Unreadable or renamed: park it where an operator can look.
			os.Rename(p, m.path(m.inbox(m.self, "failed"), n))
			continue
		}
		out = append(out, &msg)
	}
	return out, nil
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

func (m *Mailbox) Prune(_ context.Context, maxAge time.Duration) error {
	m.openedUp.Do(m.openUp) // once per process: new entries get the right modes
	cutoff := time.Now().Add(-maxAge)
	var errs []error
	if des, err := os.ReadDir(m.path("tmp")); err == nil {
		stale := time.Now().Add(-StaleTmp)
		for _, de := range des {
			if fi, err := de.Info(); err == nil && fi.ModTime().Before(stale) {
				errs = append(errs, os.Remove(m.path("tmp", de.Name())))
			}
		}
	}
	os.RemoveAll(m.path(m.inbox(m.self, "done"))) // used by older versions
	for _, d := range []string{
		m.path(m.inbox(m.self, "failed")),
		m.path("reply", m.self), // abandoned reply dirs (requester crashed)
	} {
		des, err := os.ReadDir(d)
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
