// Package config loads and validates the per-node YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"fkbr/internal/mountinfo"
	"fkbr/internal/product"
)

type Root struct {
	ID         string `yaml:"id"`
	Path       string `yaml:"path"`
	AllowWrite bool   `yaml:"allow_write"`
	// Local: shown only to this node's own TUI and CLI; never published,
	// never reachable by other nodes (see LocalRoot).
	Local bool `yaml:"local"`
	// Hide is the share's path inside this root, if it is there (cd ~ &&
	// fkbr ~/box): left out of listings and transfers, never written. Set
	// by Finish.
	Hide string `yaml:"-"`
}

type Poll struct {
	// Active is the inbox/reply poll interval while there is recent traffic.
	Active time.Duration `yaml:"active"`
	// Idle is the poll interval after IdleAfter without traffic.
	Idle      time.Duration `yaml:"idle"`
	IdleAfter time.Duration `yaml:"idle_after"`
}

type Config struct {
	NodeID  string `yaml:"node_id"`
	NFSRoot string `yaml:"nfs_root"`
	// SSH lists the hosts of an ssh swarm ("host" or "host:/path", as ssh
	// takes the host); this node is then its home node, and the share is
	// a private directory in StateDir (docs/ssh.md).
	SSH []string `yaml:"ssh"`
	// SSHBin is fkbr on the ssh hosts (default "fkbr", on their PATH).
	SSHBin string `yaml:"ssh_bin"`
	// LocalRoot is a directory this node's own TUI and CLI may browse and
	// copy from (and, without a share anyone else writes, into) but no
	// other node sees: root "local". Default "/"; "none" turns it off.
	LocalRoot string `yaml:"local_root"`
	// Link: this node runs under `fkbr link`, linked to a home node over
	// ssh; its share is where it keeps the chunks it sends.
	Link bool `yaml:"-"`
	// Home: this node is the home node of an ssh swarm (SSH lists hosts,
	// or there is no share at all and hosts may be added later); its share
	// is a private directory in StateDir.
	Home bool `yaml:"-"`
	// StateDir is local (never NFS) runtime storage: worker lock, socket.
	// Default $TMPDIR (normally /tmp)/fkbr-<uid>-<node>.
	StateDir string `yaml:"state_dir"`
	// Socket is the daemon's API socket (default <state_dir>/fkbr.sock);
	// SocketMode its permissions (default 0600: only the daemon's user).
	Socket     string   `yaml:"socket"`
	SocketMode FileMode `yaml:"socket_mode"`
	Roles      []string `yaml:"roles"`
	// AcceptWritesFrom restricts which nodes (IDs or @roles) may initiate
	// writes into this node. Empty: any node. See api.NodeInfo.
	AcceptWritesFrom  []string      `yaml:"accept_writes_from"`
	Roots             []Root        `yaml:"roots"`
	Poll              Poll          `yaml:"poll"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	OfflineAfter      time.Duration `yaml:"offline_after"`
	RequestTimeout    time.Duration `yaml:"request_timeout"`
	// NFSRefresh: "auto" (default) compensates for NFS mounts that cache
	// attributes for long, "always" or "never" force it (see
	// nfsmailbox.Options.Refresh).
	NFSRefresh string `yaml:"nfs_refresh"`
	// KeepDone is how long finished job records, sync plans and snapshots,
	// and failed requests stay on the share (processed requests are
	// removed at once). Default 1h.
	KeepDone time.Duration `yaml:"keep_done"`
	// ForgetAfter removes nodes offline for longer than this from the
	// registry, as `forget` would. Default 7 days; negative: never.
	ForgetAfter time.Duration `yaml:"forget_after"`
	Transfer    Transfer      `yaml:"transfer"`
	Sync        Sync          `yaml:"sync"`
}

type Sync struct {
	// MaxDeletes caps deletions per target in a sync plan unless the apply
	// is forced (-1: no limit).
	MaxDeletes int `yaml:"max_deletes"`
}

type Transfer struct {
	// ChunkSize is the target size of one staged chunk on the share.
	ChunkSize ByteSize `yaml:"chunk_size"`
	// StagingQuota caps bytes staged on the share by this node, all jobs.
	StagingQuota ByteSize `yaml:"staging_quota"`
	// JobQuota caps bytes staged for a single job.
	JobQuota ByteSize `yaml:"job_quota"`
	// MaxJobs limits concurrent jobs per role (sending, receiving).
	MaxJobs int `yaml:"max_jobs"`
	// AllowSetID keeps setuid/setgid bits on received files; off by default
	// so a peer cannot plant privileged executables.
	AllowSetID bool `yaml:"allow_setid"`
	// DetachAfter is how long a fan-out source waits for a target that is
	// holding everyone back (stalled or offline) before dropping it. The
	// last remaining target is never dropped.
	DetachAfter time.Duration `yaml:"detach_after"`
}

// FileMode is a permission mode written in octal, e.g. 0660 or "0660".
type FileMode uint32

func (m *FileMode) UnmarshalYAML(n *yaml.Node) error {
	v, err := strconv.ParseUint(strings.TrimPrefix(n.Value, "0o"), 8, 32)
	if err != nil || v > 0o777 {
		return fmt.Errorf("invalid mode %q (want octal like 0660)", n.Value)
	}
	*m = FileMode(v)
	return nil
}

// ByteSize is a byte count that YAML may give as 1048576, "512MiB", "2GiB",
// "10G" (powers of 1024 throughout).
type ByteSize int64

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func ParseByteSize(s string) (ByteSize, error) {
	t := strings.TrimSuffix(strings.TrimSpace(s), "B")
	t = strings.TrimSuffix(t, "i")
	mult := int64(1)
	if t != "" {
		if i := strings.IndexByte("KMGT", t[len(t)-1]); i >= 0 {
			mult = int64(1) << (10 * (i + 1))
			t = t[:len(t)-1]
		}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return ByteSize(v * mult), nil
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// ValidID reports whether s is usable as a node or root ID. IDs become
// directory names on the share, so they are deliberately restrictive.
func ValidID(s string) bool { return idRE.MatchString(s) }

// DefaultPath returns the config file to use when none is given, or ""
// for none: $FKBR_CONFIG, else fkbr.yaml next to the executable. fkbr is
// meant to run from wherever it was copied, without installing anything,
// so it never looks in system or home directories.
func DefaultPath() string {
	if p := os.Getenv(product.EnvPrefix + "CONFIG"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), product.Name+".yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Load reads, defaults and validates a config file.
func Load(path string) (*Config, error) {
	c, err := Read(path)
	if err != nil {
		return nil, err
	}
	return c, c.Finish()
}

// Read decodes a config file without defaults or validation, so that
// command-line flags can be layered on top before Finish.
func Read(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return decode(data)
}

// Parse decodes YAML, rejecting unknown keys, then applies defaults and
// validates.
func Parse(data []byte) (*Config, error) {
	c, err := decode(data)
	if err != nil {
		return nil, err
	}
	return c, c.Finish()
}

func decode(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && err != io.EOF {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

// Finish applies defaults and validates.
func (c *Config) Finish() error {
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	return c.placeShare()
}

// placeShare checks the roots against the share, with symlinks resolved:
// a root inside the share is refused (it would hand out fkbr's own files),
// and a share inside a root is hidden from that root.
func (c *Config) placeShare() error {
	share := RealPath(c.NFSRoot)
	for i := range c.Roots {
		r := &c.Roots[i]
		p := RealPath(r.Path)
		if p == share || strings.HasPrefix(p, share+"/") {
			return fmt.Errorf("root %q (%s) is inside the share %s; run %s from the directory you want to share, or pass -root",
				r.ID, r.Path, c.NFSRoot, product.Name)
		}
		r.Hide = ""
		if inside, ok := strings.CutPrefix(share, strings.TrimSuffix(p, "/")+"/"); ok {
			r.Hide = inside
		}
	}
	return nil
}

// RealPath is p with symlinks resolved, or just cleaned if that fails.
func RealPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// ParseRoot parses a root given on the command line: "id=path" (writable)
// or "id=path:ro".
func ParseRoot(s string) (Root, error) {
	id, p, ok := strings.Cut(s, "=")
	if !ok || id == "" || p == "" {
		return Root{}, fmt.Errorf("root %q: want id=/path or id=/path:ro", s)
	}
	r := Root{ID: id, Path: p, AllowWrite: true}
	if trimmed, ro := strings.CutSuffix(p, ":ro"); ro {
		r.Path, r.AllowWrite = trimmed, false
	}
	return r, nil
}

// DefaultStateDir is per-node runtime storage for the worker lock and the
// socket; nothing in it needs to survive a reboot. It is deliberately not
// $XDG_RUNTIME_DIR: logind deletes that when the user's last session ends,
// while a node may keep running under nohup or inside tmux. The node keeps
// its files fresh so tmpfiles cleaning of /tmp never ages them out.
func DefaultStateDir(node string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d-%s", product.Name, os.Getuid(), node))
}

func (c *Config) applyDefaults() {
	if c.StateDir == "" && c.NodeID != "" {
		c.StateDir = DefaultStateDir(c.NodeID)
	}
	if c.Socket == "" && c.StateDir != "" {
		c.Socket = filepath.Join(c.StateDir, product.Name+".sock")
	}
	switch {
	case c.Link:
		if c.NFSRoot == "" && c.StateDir != "" {
			c.NFSRoot = filepath.Join(c.StateDir, "chunks")
		}
		// Asking for requests waits on the home node (sshlink), so ask
		// again at once.
		setDur(&c.Poll.Active, 10*time.Millisecond)
		setDur(&c.Poll.Idle, 10*time.Millisecond)
	case c.Home || len(c.SSH) > 0:
		c.Home = true
		if c.NFSRoot == "" && c.StateDir != "" {
			c.NFSRoot = filepath.Join(c.StateDir, "ssh")
		}
		// The share is on local disk: looking is cheap.
		setDur(&c.Poll.Active, 20*time.Millisecond)
		setDur(&c.Poll.Idle, 100*time.Millisecond)
	}
	if c.SocketMode == 0 {
		c.SocketMode = 0o600
	}
	if c.NFSRefresh == "" {
		c.NFSRefresh = "auto"
	}
	setDur(&c.Poll.Active, 200*time.Millisecond)
	setDur(&c.Poll.Idle, time.Second)
	setDur(&c.Poll.IdleAfter, 30*time.Second)
	setDur(&c.HeartbeatInterval, 5*time.Second)
	setDur(&c.OfflineAfter, 30*time.Second)
	setDur(&c.RequestTimeout, 30*time.Second)
	setDur(&c.KeepDone, time.Hour)
	setDur(&c.ForgetAfter, 7*24*time.Hour)
	t := &c.Transfer
	if t.ChunkSize == 0 {
		t.ChunkSize = 128 << 20
	}
	if t.StagingQuota == 0 {
		t.StagingQuota = 4 << 30
	}
	if t.JobQuota == 0 {
		t.JobQuota = 1 << 30
	}
	if t.MaxJobs == 0 {
		t.MaxJobs = 2
	}
	setDur(&t.DetachAfter, 10*time.Minute)
	if c.Sync.MaxDeletes == 0 {
		c.Sync.MaxDeletes = 1000
	}
}

func setDur(d *time.Duration, def time.Duration) {
	if *d == 0 {
		*d = def
	}
}

func (c *Config) Validate() error {
	var errs []error
	if !ValidID(c.NodeID) {
		errs = append(errs, fmt.Errorf("node_id %q: must match %s", c.NodeID, idRE))
	}
	if !filepath.IsAbs(c.NFSRoot) {
		errs = append(errs, fmt.Errorf("nfs_root %q: must be an absolute path", c.NFSRoot))
	}
	if !filepath.IsAbs(c.StateDir) {
		errs = append(errs, fmt.Errorf("state_dir %q: must be an absolute path", c.StateDir))
	}
	switch c.NFSRefresh {
	case "auto", "always", "never":
	default:
		errs = append(errs, fmt.Errorf("nfs_refresh %q: want auto, always or never", c.NFSRefresh))
	}
	if !filepath.IsAbs(c.Socket) || len(c.Socket) > 100 {
		errs = append(errs, fmt.Errorf("socket %q: must be an absolute path of at most 100 bytes", c.Socket))
	}
	for _, r := range c.Roles {
		if !ValidID(r) {
			errs = append(errs, fmt.Errorf("roles: %q must match %s", r, idRE))
		}
		if r == "all" || r == "dest" { // api.GroupAll, api.GroupDest
			errs = append(errs, fmt.Errorf("roles: %q is reserved (@%s is built in)", r, r))
		}
	}
	for _, a := range c.AcceptWritesFrom {
		if !ValidID(strings.TrimPrefix(a, "@")) {
			errs = append(errs, fmt.Errorf("accept_writes_from: %q is not a node ID or @role", a))
		}
	}
	seen := map[string]bool{}
	for i, r := range c.Roots {
		if !ValidID(r.ID) {
			errs = append(errs, fmt.Errorf("roots[%d].id %q: must match %s", i, r.ID, idRE))
		}
		if seen[r.ID] {
			errs = append(errs, fmt.Errorf("roots[%d].id %q: duplicate", i, r.ID))
		}
		seen[r.ID] = true
		if !filepath.IsAbs(r.Path) {
			errs = append(errs, fmt.Errorf("roots[%d].path %q: must be an absolute path", i, r.Path))
		}
	}
	if c.OfflineAfter <= c.HeartbeatInterval {
		errs = append(errs, fmt.Errorf("offline_after (%s) must exceed heartbeat_interval (%s)", c.OfflineAfter, c.HeartbeatInterval))
	}
	if c.ForgetAfter > 0 && c.ForgetAfter <= c.OfflineAfter {
		errs = append(errs, fmt.Errorf("forget_after (%s) must exceed offline_after (%s)", c.ForgetAfter, c.OfflineAfter))
	}
	t := c.Transfer
	if t.ChunkSize < 1<<20 {
		errs = append(errs, fmt.Errorf("transfer.chunk_size must be at least 1MiB"))
	}
	if t.JobQuota < t.ChunkSize || t.StagingQuota < t.JobQuota {
		errs = append(errs, fmt.Errorf("transfer: need chunk_size <= job_quota <= staging_quota"))
	}
	if t.MaxJobs < 1 {
		errs = append(errs, fmt.Errorf("transfer.max_jobs must be at least 1"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return nil
}

// DefaultRootID is the ID of the root fkbr shares by default: the
// directory it was started in.
const DefaultRootID = "files"

// DefaultNodeID derives a node ID from the short hostname, mapped onto the
// allowed characters.
func DefaultNodeID() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-_")
	if len(id) > 63 {
		id = id[:63]
	}
	if !ValidID(id) {
		return "", fmt.Errorf("cannot derive a node ID from hostname %q; pass -node", h)
	}
	return id, nil
}

// DirRoot shares dir (normally the current directory) as the default root.
// It refuses "/" and anything inside the share itself.
func DirRoot(dir, share string, writable bool) (Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Root{}, err
	}
	if abs == "/" {
		return Root{}, fmt.Errorf("refusing to share / by default; cd to the directory to share, or pass -root")
	}
	if s, err := filepath.Abs(share); err == nil && (abs == s || strings.HasPrefix(abs, s+"/")) {
		return Root{}, fmt.Errorf("%s is inside the share %s; run %s from the directory you want to share", abs, s, product.Name)
	}
	return Root{ID: DefaultRootID, Path: abs, AllowWrite: writable}, nil
}

// PrivateShare reports whether nobody but this swarm's nodes can write the
// share: an ssh swarm's records and links, where no directory is open to
// others.
func (c *Config) PrivateShare() bool { return c.Link || c.Home }

// RefreshNFS decides whether to compensate for NFS attribute caching on
// the share (nfs_refresh: auto looks at its mount options).
func (c *Config) RefreshNFS() bool {
	switch c.NFSRefresh {
	case "always":
		return true
	case "never":
		return false
	}
	m := mountinfo.Of(c.NFSRoot)
	return m != nil && m.CachesAttributes()
}
