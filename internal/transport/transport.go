// Package transport defines how nodes exchange requests and replies. The
// engine only sees this interface; nfsmailbox is the implementation over a
// shared NFS directory, and another (SSH, S3, ...) could replace it.
package transport

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"fkbr/internal/api"
)

// Message types.
const (
	TypeList = "list"
	// TypeSend asks the source node to produce a job's chunks.
	TypeSend = "send"
	// TypeReceive tells a target node to consume a job's chunks.
	TypeReceive = "receive"
	TypeMkdir   = "mkdir"
	TypeRemove  = "remove"
	TypeRead    = "read"
	// TypeScan asks a node to snapshot a directory for a sync plan.
	TypeScan = "scan"
)

// JobRef names a job; Body of send/receive messages.
type JobRef struct {
	ID string `json:"id"`
}

// Message is a request addressed to exactly one executor node.
type Message struct {
	Protocol int       `json:"protocol"`
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Created  time.Time `json:"created"`
	// Notify marks a fire-and-forget message: no reply channel is created.
	Notify bool            `json:"notify,omitempty"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Reply is one part of a response; large responses are split into parts
// with increasing Seq, the last one marked Final. An Error ends the stream.
type Reply struct {
	Protocol int    `json:"protocol"`
	ReqID    string `json:"req_id"`
	From     string `json:"from"`
	Seq      int    `json:"seq"`
	Final    bool   `json:"final"`
	Error    string `json:"error,omitempty"`
	// Code is api.ErrorCode(err) for Error, so the type survives the hop.
	Code string          `json:"code,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

type Transport interface {
	// Self is the local node ID.
	Self() string
	// Register publishes this node's info to the registry.
	Register(ctx context.Context, info api.NodeInfo) error
	// Heartbeat marks this node alive.
	Heartbeat(ctx context.Context) error
	// Nodes returns all registered nodes with liveness.
	Nodes(ctx context.Context) ([]api.Node, error)
	// Unregister deletes another node's registry entry, inbox, reply
	// directory and staged data.
	Unregister(ctx context.Context, node string) error
	// Leave removes this node's own registry entry, inbox, reply directory
	// and staged data. Call it only once the worker has stopped.
	Leave(ctx context.Context) error

	// Send delivers msg to msg.To's inbox and prepares the reply channel,
	// so AwaitReplies may be called right after.
	Send(ctx context.Context, msg *Message) error
	// AwaitReplies calls fn for each reply part to reqID in Seq order until
	// the Final part, an error reply, or ctx ends, then cleans up.
	AwaitReplies(ctx context.Context, reqID string, fn func(*Reply) error) error

	// Fetch claims requests waiting in this node's inbox, oldest first.
	// Only one worker per node may call it (the engine holds a local lock).
	// It may return messages and an error together; handle the messages.
	Fetch(ctx context.Context) ([]*Message, error)
	// Reply sends one reply part back to req.From.
	Reply(ctx context.Context, req *Message, r *Reply) error
	// Finish records that a fetched request has been processed.
	Finish(ctx context.Context, msg *Message, failed bool) error
	// Prune removes processed requests older than maxAge.
	Prune(ctx context.Context, maxAge time.Duration) error
}

// Store holds jobs and their staged data on the shared medium. Job records
// are written by the initiator; each participant only writes its own status
// and (for the source) chunks, so there are no concurrent writers per file.
type Store interface {
	CreateJob(ctx context.Context, job *api.Job) error
	// GetJob assembles the job record, participant statuses, completion and
	// cancellation. Job.State is left for the caller to derive.
	GetJob(ctx context.Context, id string) (*api.Job, error)
	ListJobs(ctx context.Context) ([]api.Job, error)
	PutStatus(ctx context.Context, id string, st *api.JobStatus) error
	RequestCancel(ctx context.Context, id string) error
	// SetComplete records that the source has published all chunks.
	SetComplete(ctx context.Context, id string, chunks int) error
	// Detach drops a target from a job (written by the source, or by the
	// initiator for targets offline at submit). GetJob lists them.
	Detach(ctx context.Context, id string, d api.Detached) error

	// CreateChunk starts writing chunk seq of a job sourced by this node.
	CreateChunk(ctx context.Context, id string, seq int) (ChunkWriter, error)
	// ChunkMeta returns a published chunk's metadata, or an error wrapping
	// fs.ErrNotExist if it is not published (yet, or any more).
	ChunkMeta(ctx context.Context, src, id string, seq int) ([]byte, error)
	OpenChunk(ctx context.Context, src, id string, seq int) (io.ReadCloser, error)
	DeleteChunk(ctx context.Context, id string, seq int) error
	// SetAcked records that node has consumed chunks [0, n).
	SetAcked(ctx context.Context, src, id, node string, n int) error
	Acked(ctx context.Context, src, id, node string) (int, error)
	// StagedBytes reports bytes staged by this node for one job and in total.
	StagedBytes(ctx context.Context, id string) (job, total int64, err error)
	// RemoveJobData deletes all staged data of a job sourced by this node.
	RemoveJobData(ctx context.Context, id string) error
	// PruneJobs removes finished job records older than maxAge.
	PruneJobs(ctx context.Context, maxAge time.Duration) error

	// Blobs are named files on the share ("kind/id/file", e.g. sync
	// snapshots and path lists). CreateBlob publishes on Commit (meta is
	// ignored), replacing an existing blob of that name.
	CreateBlob(ctx context.Context, name string) (ChunkWriter, error)
	OpenBlob(ctx context.Context, name string) (io.ReadCloser, error)
	// PruneBlobs removes "kind/id" directories untouched for maxAge.
	PruneBlobs(ctx context.Context, maxAge time.Duration) error
}

// ChunkWriter receives a chunk's bytes; nothing is visible to readers
// until Commit publishes the content and then meta.
type ChunkWriter interface {
	io.Writer
	Commit(meta []byte) error
	Abort()
}
