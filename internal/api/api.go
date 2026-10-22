// Package api is the boundary between the fkbr engine and its frontends
// (CLI, TUI). Frontends import only this package; the types here are
// plain JSON-serialisable DTOs so they can cross a socket unchanged.
package api

import (
	"context"
	"errors"
	"slices"
	"time"
)

// ProtocolVersion is the version of the on-NFS protocol and the wire DTOs.
// 2: chunks in fkbr's own format instead of tar (2026-10-20).
const ProtocolVersion = 2

var (
	ErrUnknownNode = errors.New("unknown node")
	ErrNodeOffline = errors.New("node offline")
	ErrUnknownRoot = errors.New("unknown root")
	ErrBadPath     = errors.New("invalid path")
	ErrUnknownJob  = errors.New("unknown job")
	ErrReadOnly    = errors.New("root is read-only")
	ErrNotAllowed  = errors.New("not allowed")
	ErrNotFound    = errors.New("no such file or directory")
)

// Root is a local directory a node exposes under a stable ID. Peers only
// ever address files as (root ID, relative path).
type Root struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

// NodeInfo is what a node publishes about itself in the shared registry.
type NodeInfo struct {
	ID           string   `json:"id"`
	Hostname     string   `json:"hostname"`
	OS           string   `json:"os"`
	Version      string   `json:"version"`
	Protocol     int      `json:"protocol"`
	Roles        []string `json:"roles,omitempty"`
	Roots        []Root   `json:"roots"`
	ChownCapable bool     `json:"chown_capable"`
	// DACOverride: may write into directories regardless of permissions
	// (root or CAP_DAC_OVERRIDE), so directory ownership can be preserved.
	DACOverride bool `json:"dac_override"`
	// AcceptWritesFrom lists node IDs and @roles allowed to initiate writes
	// into this node; empty means anyone. A node may always write to itself.
	AcceptWritesFrom []string  `json:"accept_writes_from,omitempty"`
	Started          time.Time `json:"started"`
	// Transient marks a node registered by a TUI that runs it only while
	// open; it leaves the share when the TUI quits.
	Transient bool `json:"transient,omitempty"`
	// Instance identifies one worker run (see engine/instance.go); PID and
	// BootID tell a live worker on the same host from a crashed one.
	Instance string `json:"instance,omitempty"`
	PID      int    `json:"pid,omitempty"`
	BootID   string `json:"boot_id,omitempty"`
	// Features are what this build can do beyond the protocol version (a
	// request needing one an older peer lacks is refused, not half done).
	Features []string `json:"features,omitempty"`
}

// FeatureExclude: the node honours Exclude in transfers and sync scans.
const FeatureExclude = "exclude"

// FeatureMove: as a source, the node refuses a move onto its own sources.
const FeatureMove = "move"

// FeatureRecheck: a sync's deletions on the node keep what changed since
// its scan (RemoveRequest.Plan).
const FeatureRecheck = "recheck"

// Has reports whether the node's build has feature f.
func (n *NodeInfo) Has(f string) bool { return slices.Contains(n.Features, f) }

// HasRole reports whether the node carries a role tag.
func (n *NodeInfo) HasRole(role string) bool {
	for _, r := range n.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AcceptsWritesFrom reports whether initiator may write into n. Patterns
// in n.AcceptWritesFrom are node IDs or "@role".
func (n *NodeInfo) AcceptsWritesFrom(initiator *NodeInfo) bool {
	if len(n.AcceptWritesFrom) == 0 || initiator.ID == n.ID {
		return true
	}
	for _, p := range n.AcceptWritesFrom {
		if p == initiator.ID || (len(p) > 1 && p[0] == '@' && initiator.HasRole(p[1:])) {
			return true
		}
	}
	return false
}

// Node is a registry entry plus liveness as observed by the caller.
type Node struct {
	NodeInfo
	Self   bool `json:"self"`
	Online bool `json:"online"`
	// LastSeen is the age of the node's last heartbeat, measured against
	// the NFS server clock. Negative means no heartbeat was ever written.
	LastSeen time.Duration `json:"last_seen_ns"`
	// Link is how the home node of an ssh swarm reaches this node, e.g.
	// "ssh web01: connected"; empty on a share.
	Link string `json:"link,omitempty"`
}

type EntryType string

const (
	TypeFile    EntryType = "file"
	TypeDir     EntryType = "dir"
	TypeSymlink EntryType = "symlink"
	TypeOther   EntryType = "other"
)

// Entry describes one directory entry. Mode holds Unix permission and
// setuid/setgid/sticky bits (e.g. 0o4755), not Go's fs.FileMode encoding.
type Entry struct {
	Name       string    `json:"name"`
	Type       EntryType `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	MtimeNs    int64     `json:"mtime_ns"`
	UID        uint32    `json:"uid"`
	GID        uint32    `json:"gid"`
	LinkTarget string    `json:"link_target,omitempty"`
}

// ListRequest asks for one page of a single directory. An empty Node means
// the local node. Path is relative to the root; "" or "." is the root itself.
type ListRequest struct {
	Node   string `json:"node,omitempty"`
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// ListResult is one page of a directory listing, sorted by name.
type ListResult struct {
	Node    string  `json:"node"`
	Root    string  `json:"root"`
	Path    string  `json:"path"`
	Offset  int     `json:"offset"`
	Total   int     `json:"total"`
	Entries []Entry `json:"entries"`
}

// Service is the engine's API as seen by frontends.
type Service interface {
	// Self returns the local node ID.
	Self() string
	// Nodes lists all registered nodes with liveness.
	Nodes(ctx context.Context) ([]Node, error)
	// Forget removes an offline node from the share (registry, inbox,
	// replies, staged data). It re-registers if it ever starts again.
	Forget(ctx context.Context, node string) error
	// Prune removes finished job records, sync plans and snapshots, and
	// failed requests older than keep from the share now (a running node
	// also does this by itself). Running jobs are never touched.
	Prune(ctx context.Context, keep time.Duration) error
	// List returns one page of a directory on any node.
	List(ctx context.Context, req ListRequest) (*ListResult, error)
	FileService
	JobService
	SyncService
	// Subscribe streams changes until ctx ends, starting with the current
	// state of every node and job. Events are snapshots, so a slow reader
	// may miss intermediate ones but never ends up with a stale view for
	// longer than the next change.
	Subscribe(ctx context.Context) (<-chan Event, error)
	Close() error
}

type EventType string

const (
	EventNode EventType = "node" // a node registered, changed or went on/offline
	EventJob  EventType = "job"  // a job was created or changed
	// EventJobGone is sent when a job record disappears (pruned).
	EventJobGone EventType = "job_gone"
	// EventScan reports sync scan progress (only from the engine planning).
	EventScan EventType = "scan"
)

// Event carries a snapshot of the node or job that changed.
type Event struct {
	Type  EventType     `json:"type"`
	Time  time.Time     `json:"time"`
	Node  *Node         `json:"node,omitempty"`
	Job   *Job          `json:"job,omitempty"`
	JobID string        `json:"job_id,omitempty"`
	Scan  *ScanProgress `json:"scan,omitempty"`
}

// Client is what frontends hold: an in-process engine or a remote daemon.
type Client = Service

// ConflictPolicy decides what happens when a received entry already exists
// at the destination.
type ConflictPolicy string

const (
	ConflictSkip           ConflictPolicy = "skip"
	ConflictOverwrite      ConflictPolicy = "overwrite"
	ConflictOverwriteOlder ConflictPolicy = "overwrite-if-older"
	DefaultConflictPolicy                 = ConflictSkip
)

func (p ConflictPolicy) Valid() bool {
	switch p {
	case ConflictSkip, ConflictOverwrite, ConflictOverwriteOlder:
		return true
	}
	return false
}

// TransferRequest copies Paths (relative to SrcRoot on SrcNode) into the
// directory DstDir of DstRoot on each of DstNodes, like "cp -a p1 p2 dir/".
// A path of "." copies the contents of the source root itself. DstNodes
// may contain "@role", meaning every registered node with that role; on
// submit it is expanded and members that are offline are recorded as
// detached so they can catch up later.
type TransferRequest struct {
	SrcNode  string   `json:"src_node"`
	SrcRoot  string   `json:"src_root"`
	Paths    []string `json:"paths"`
	DstNodes []string `json:"dst_nodes"`
	// DetachOffline: a named target that is offline is recorded as detached
	// (catch it up later), as an offline @role member is, instead of
	// failing the request. The TUI sets it for @dest.
	DetachOffline bool           `json:"detach_offline,omitempty"`
	DstRoot       string         `json:"dst_root"`
	DstDir        string         `json:"dst_dir"`
	Conflict      ConflictPolicy `json:"conflict"`
	Compress      bool           `json:"compress"`

	// Used by sync (set by ApplySync, not by users):
	// PathsBlob names a list of paths on the share, one per line, used
	// instead of Paths; PathsCount is its length, for display.
	PathsBlob  string `json:"paths_blob,omitempty"`
	PathsCount int64  `json:"paths_count,omitempty"`
	// BaseDir makes entries land at their path relative to BaseDir under
	// DstDir, instead of under their base name.
	BaseDir string `json:"base_dir,omitempty"`
	// NoRecurse sends each listed path as a single entry (a directory
	// without its contents).
	NoRecurse bool `json:"no_recurse,omitempty"`
	// Exclude leaves entries inside the copied paths out (see package
	// exclude); the paths named themselves are always sent.
	Exclude []string `json:"exclude,omitempty"`
	// Move: the initiator deletes the sources once the job is done, so it
	// must not overlap them (checked at submit and by the source node).
	Move bool `json:"move,omitempty"`
	// SyncPlan is the plan this transfer belongs to.
	SyncPlan string `json:"sync_plan,omitempty"`
}

type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	// JobDetached is a target's state after the source dropped it for
	// lagging or being offline; it needs a catch-up job.
	JobDetached JobState = "detached"
	// JobPartial is a job that completed for some targets while others
	// were detached.
	JobPartial JobState = "partial"
)

// Terminal reports whether no further progress will happen.
func (s JobState) Terminal() bool {
	switch s {
	case JobDone, JobFailed, JobCancelled, JobDetached, JobPartial:
		return true
	}
	return false
}

// Detached records a target dropped from a job.
type Detached struct {
	Node   string    `json:"node"`
	Reason string    `json:"reason"`
	Chunks int       `json:"chunks"` // chunks it had consumed
	At     time.Time `json:"at"`
}

type Role string

const (
	RoleSource Role = "source"
	RoleTarget Role = "target"
)

// JobStatus is one participant's view of a job, published by that node.
type JobStatus struct {
	Node    string   `json:"node"`
	Role    Role     `json:"role"`
	State   JobState `json:"state"`
	Phase   string   `json:"phase,omitempty"` // human-readable detail
	Files   int64    `json:"files"`
	Bytes   int64    `json:"bytes"`
	Chunks  int      `json:"chunks"`
	Skipped int64    `json:"skipped"`
	Errors  int64    `json:"errors"`
	Error   string   `json:"error,omitempty"`
	// Problems holds up to a few hundred per-entry messages (skips, errors).
	Problems []string  `json:"problems,omitempty"`
	Updated  time.Time `json:"updated"`
	// TotalFiles and TotalBytes are what the source will send, counted
	// alongside sending; final once Counted.
	TotalFiles int64 `json:"total_files,omitempty"`
	TotalBytes int64 `json:"total_bytes,omitempty"`
	Counted    bool  `json:"counted,omitempty"`
}

// Job is a submitted transfer and the statuses its participants published.
type Job struct {
	ID        string          `json:"id"`
	Initiator string          `json:"initiator"`
	Created   time.Time       `json:"created"`
	Request   TransferRequest `json:"request"`
	// TotalChunks is set once the source has produced every chunk.
	TotalChunks int  `json:"total_chunks,omitempty"`
	Cancelled   bool `json:"cancelled"`
	// CatchUpOf is the job this one re-sends to targets that missed it.
	CatchUpOf    string      `json:"catch_up_of,omitempty"`
	Detached     []Detached  `json:"detached,omitempty"`
	State        JobState    `json:"state"`
	Participants []JobStatus `json:"participants"`
}

// IsDetached reports whether node was dropped from the job.
func (j *Job) IsDetached(node string) bool {
	for _, d := range j.Detached {
		if d.Node == node {
			return true
		}
	}
	return false
}

// JobService is the transfer part of the API.
type JobService interface {
	// Submit validates and enqueues a transfer and returns its job ID.
	Submit(ctx context.Context, req TransferRequest) (string, error)
	Job(ctx context.Context, id string) (*Job, error)
	// Jobs lists known jobs, newest first.
	Jobs(ctx context.Context) ([]Job, error)
	// Cancel asks every participant to stop; it returns before they do.
	Cancel(ctx context.Context, id string) error
	// CatchUp submits a new job re-sending a job's data to its targets
	// that missed it (detached or failed) and are online now, and returns
	// the new job's ID.
	CatchUp(ctx context.Context, id string) (string, error)
}

// FileService covers small file operations on any node. Writes obey the
// target root's allow_write and the node's accept_writes_from.
type FileService interface {
	Mkdir(ctx context.Context, req MkdirRequest) error
	// Remove deletes entries recursively. The root itself and fkbr's
	// staging area cannot be removed.
	Remove(ctx context.Context, req RemoveRequest) (*RemoveResult, error)
	// Read returns up to Limit bytes (max MaxReadLimit) of a regular file.
	Read(ctx context.Context, req ReadRequest) (*ReadResult, error)
}

const MaxReadLimit = 1 << 20

type MkdirRequest struct {
	Node string `json:"node,omitempty"`
	Root string `json:"root"`
	Path string `json:"path"`
}

type RemoveRequest struct {
	Node  string   `json:"node,omitempty"`
	Root  string   `json:"root"`
	Paths []string `json:"paths"`
	// Exclude keeps what matches (patterns relative to Base, see package
	// exclude), and the folders holding it: a mirror sync's deletions
	// never take excluded files along.
	Exclude []string `json:"exclude,omitempty"`
	Base    string   `json:"base,omitempty"`
	// Plan marks a sync's deletions: the node deletes only what its scan
	// for that plan saw as it still is. Whatever is new or changed since
	// stays, with the folders holding it, and is listed in Kept.
	Plan string `json:"plan,omitempty"`
}

type RemoveResult struct {
	Removed int      `json:"removed"` // paths fully removed
	Errors  []string `json:"errors,omitempty"`
	Kept    []string `json:"kept,omitempty"` // with Plan: changed since the scan
}

type ReadRequest struct {
	Node   string `json:"node,omitempty"`
	Root   string `json:"root"`
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Limit  int    `json:"limit"`
}

type ReadResult struct {
	Data []byte `json:"data"`
	Size int64  `json:"size"` // whole file
	EOF  bool   `json:"eof"`
}

// GroupAll is the built-in destination group: "@all" is every registered
// node except the source.
const GroupAll = "all"

// GroupDest is the TUI's "@dest": the nodes marked in its node list. The
// TUI expands it before submitting; like "all", it is never a role.
const GroupDest = "dest"

// SyncMode selects what a sync does with destination entries that are not
// in the source.
type SyncMode string

const (
	// SyncIncremental copies new and changed entries and leaves extra
	// destination entries alone.
	SyncIncremental SyncMode = "incremental"
	// SyncMirror also deletes destination entries missing from the source.
	SyncMirror SyncMode = "mirror"
)

// SyncRequest makes DstPath on each target match SrcPath on the source
// (like "rsync -a src/ dst/"). Entries are compared by type, size, mtime
// (whole seconds), symlink target, mode and, where the target can apply
// it, ownership; Checksum also compares file content hashes.
type SyncRequest struct {
	SrcNode  string   `json:"src_node"`
	SrcRoot  string   `json:"src_root"`
	SrcPath  string   `json:"src_path"`
	DstNodes []string `json:"dst_nodes"` // node IDs or @role
	// DetachOffline: see TransferRequest.
	DetachOffline bool     `json:"detach_offline,omitempty"`
	DstRoot       string   `json:"dst_root"`
	DstPath       string   `json:"dst_path"`
	Mode          SyncMode `json:"mode"`
	Checksum      bool     `json:"checksum"`
	Compress      bool     `json:"compress"`
	// MaxDeletes caps deletions per target unless the apply is forced;
	// 0 means the node's sync.max_deletes, -1 means no limit.
	MaxDeletes int `json:"max_deletes"`
	// Exclude leaves entries out on both sides: they are neither copied
	// nor deleted (see package exclude).
	Exclude []string `json:"exclude,omitempty"`
	// KeepNewer keeps target files newer than the source's (rsync -u).
	KeepNewer bool `json:"keep_newer,omitempty"`
}

// SyncTarget is what a plan would do on one target.
type SyncTarget struct {
	Node      string `json:"node"`
	Copy      int64  `json:"copy"` // entries to (re)send
	CopyBytes int64  `json:"copy_bytes"`
	Replace   int64  `json:"replace"` // type changed: deleted, then copied
	Delete    int64  `json:"delete"`  // removals (mirror), collapsed to top-level
	Unchanged int64  `json:"unchanged"`
	Kept      int64  `json:"kept,omitempty"` // newer there, kept (KeepNewer)
	// Refused: entries its filesystem cannot store (FAT names, case
	// clashes), left out; Problems says which.
	Refused int64 `json:"refused,omitempty"`
	// OverLimit is set when Delete exceeds the plan's max deletes.
	OverLimit bool `json:"over_limit,omitempty"`
}

// SyncGroup is one transfer: entries needed by exactly this set of
// targets are bundled once and fanned out to all of them.
type SyncGroup struct {
	Targets []string `json:"targets"`
	Entries int64    `json:"entries"`
	Bytes   int64    `json:"bytes"`
	Blob    string   `json:"blob"` // path list on the share
}

// SyncPlan is a dry run: the result of scanning both sides and comparing.
// ApplySync carries it out.
type SyncPlan struct {
	ID         string       `json:"id"`
	Created    time.Time    `json:"created"`
	Initiator  string       `json:"initiator"`
	Request    SyncRequest  `json:"request"`
	MaxDeletes int          `json:"max_deletes"` // effective limit, -1 none
	SrcEntries int64        `json:"src_entries"`
	SrcBytes   int64        `json:"src_bytes"`
	Targets    []SyncTarget `json:"targets"`
	Groups     []SyncGroup  `json:"groups"`
	// Skipped lists group members that were offline when planning.
	Skipped  []string `json:"skipped,omitempty"`
	Problems []string `json:"problems,omitempty"`
	Applied  bool     `json:"applied"`
	Jobs     []string `json:"jobs,omitempty"`
	// DeleteErrors from applying (removals that failed).
	DeleteErrors []string `json:"delete_errors,omitempty"`
}

// SyncService plans and applies one-way syncs.
type SyncService interface {
	// PlanSync scans source and targets and stores a plan; nothing changes.
	// It can take long on big trees; progress arrives as EventScan.
	PlanSync(ctx context.Context, req SyncRequest) (*SyncPlan, error)
	// ApplySync performs a plan: deletions first, then one transfer job
	// per group. force allows exceeding the delete limit.
	ApplySync(ctx context.Context, planID string, force bool) (*SyncPlan, error)
	SyncPlan(ctx context.Context, planID string) (*SyncPlan, error)
	// SyncPlanEntries lists what a plan would copy and delete, up to limit
	// paths per list (0: all).
	SyncPlanEntries(ctx context.Context, planID string, limit int) (*SyncEntries, error)
}

// SyncEntries is a plan entry by entry. Paths are inside what is synced
// (relative to SrcPath for copies, to DstPath for deletions).
type SyncEntries struct {
	Copies  []SyncEntryList `json:"copies"`  // one per transfer group
	Deletes []SyncEntryList `json:"deletes"` // one per target with deletions (type changes too)
}

// SyncEntryList is one list of paths; More counts those left out by the
// limit.
type SyncEntryList struct {
	Node    string   `json:"node,omitempty"`    // for deletions
	Targets []string `json:"targets,omitempty"` // for copies
	Paths   []string `json:"paths"`
	More    int64    `json:"more,omitempty"`
}

// ScanProgress reports a node scanning for a sync plan.
type ScanProgress struct {
	PlanID  string `json:"plan_id"`
	Node    string `json:"node"`
	Entries int64  `json:"entries"`
	Bytes   int64  `json:"bytes"`
	Done    bool   `json:"done"`
	// Dir is the folder being scanned (root-relative), while not done.
	Dir string `json:"dir,omitempty"`
}
