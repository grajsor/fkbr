# fkbr NFS mailbox protocol (version 1)

Nodes share nothing but one directory, normally an NFS mount. They never
open network connections to each other. This document describes what lives
on the share and the rules every node follows. The implementation is
`internal/transport/nfsmailbox`.

## Layout

```
<nfs_root>/
├── protocol-version                 "1\n"; a node refuses to start on a mismatch
├── tmp/                             every file is written here first
├── nodes/<id>/info.json             registry entry (api.NodeInfo)
├── nodes/<id>/heartbeat             rewritten every heartbeat_interval
├── inbox/<id>/new/<req>.json        requests waiting for node <id>
├── inbox/<id>/cur/<req>.json        claimed, being processed
├── inbox/<id>/failed/<req>.json     failed or unreadable
├── reply/<id>/<req>/<seq>.json      reply parts for requests sent by <id>
├── jobs/<job>/job.json              api.Job as submitted (initiator, once)
├── jobs/<job>/status/<node>.<role>.json   participant status (that node only)
├── jobs/<job>/COMPLETE              total chunk count (source, once)
├── jobs/<job>/CANCEL                exists = cancellation requested (anyone)
├── jobs/<job>/detached/<node>.json  target dropped from the job (source, or initiator at submit)
├── data/<src>/<job>/<seq>.chunk     staged chunk (source)
├── data/<src>/<job>/<seq>.json      chunk meta; written after the chunk = published
└── data/<src>/<job>/acks/<node>     number of chunks <node> has consumed
```

Node and root IDs must match `^[a-z0-9][a-z0-9_.-]{0,62}$`.

## Rules

1. **Atomic publication.** A file is written to `tmp/`, fsynced, closed and
   then renamed to its final name. Readers ignore everything else. Since
   `tmp/` is on the same export, the rename is atomic. Close-to-open
   consistency guarantees a reader that sees the name also sees the content.
2. **One consumer per inbox.** Only node `<id>` reads `inbox/<id>/`, so
   there are no claim races and NFS locks are never used. On the node itself,
   a `flock` on local disk (`state_dir/worker.lock`) keeps a second worker
   from starting.
3. **Claiming.** The worker renames `new/X` to `cur/X` before processing,
   then deletes it, or moves it to `failed/` (pruned after `keep_done`).
   After a crash, anything left in
   `cur/` is processed again, so request handlers must be idempotent.
4. **Replies.** Before publishing a request, the requester creates
   `reply/<self>/<req>/`. The executor writes parts `00000000.json`,
   `00000001.json`, ... The last part has `"final": true`, and a part with
   `"error"` ends the stream. Once done, or on timeout, the requester deletes
   the directory. An executor that finds the directory missing stops, because
   nobody is listening any more.
5. **Liveness.** `heartbeat` is recreated through the usual tmp+rename path,
   so its mtime comes from the NFS server. To judge a peer, a node writes its
   own probe file to `tmp/` and compares the two mtimes, which both come from
   the server clock. Host clock skew therefore doesn't matter. A node counts
   as offline when its heartbeat is older than `offline_after`.
6. **Leaving.** A node is removed from the registry (its `nodes/`,
   `inbox/`, `reply/` and `data/` directories) in three ways: `forget` by
   any node once it is offline; by any running node when it has been
   offline longer than `forget_after` (default 7 days); or by itself when a
   TUI that runs it quits. The last applies only to entries with
   `"transient": true`, which a TUI writes when it finds no entry or
   another transient one; a TUI keeps a daemon's entry as it is. A node
   that an unfinished job still involves (source, or target not detached)
   does not leave.

## Message

```json
{"protocol":1,"id":"<24 hex>","type":"list","from":"master","to":"slave1",
 "created":"2026-10-18T19:00:00Z","body":{...}}
```

IDs are 16 hex digits of Unix nanoseconds followed by 8 random hex digits,
so file names sort by creation time.

| type      | body                                     | reply parts                                     |
|-----------|------------------------------------------|-------------------------------------------------|
| `list`    | `api.ListRequest` (root, path, offset, limit) | `api.ListResult`, ≤2000 entries per part, concatenated in seq order |
| `send`    | `{"id": job}`, `notify: true`            | none; the source produces the job's chunks      |
| `receive` | `{"id": job}`, `notify: true`            | none; a target consumes the job's chunks        |
| `mkdir`   | `api.MkdirRequest`                       | one empty final part                            |
| `remove`  | `api.RemoveRequest`                      | `api.RemoveResult`                              |
| `read`    | `api.ReadRequest` (offset, limit)        | `api.ReadResult`                                |
| `scan`    | `{"plan_id", "root", "path", "checksum"}` | progress parts with scan stats; the snapshot itself goes to `blobs/scans/` (see Sync) |

`notify` messages are fire-and-forget: no reply directory is created.
`mkdir` and `remove` are refused unless the requester passes the target's
write policy.

## Transfers

1. **Submit.** Any node (the initiator) validates the request against the
   registry: nodes online, roots exist, the destination root is writable.
   It writes `jobs/<job>/job.json` and sends `send` to the source node.
2. **Produce.** The source sends `receive` to each target, then walks the
   source paths (lexical, depth-first) into a bundle: a sequence of chunks,
   each a complete tar archive (zstd optional) of about `chunk_size`. A
   file bigger than the space left in a chunk is split into segments
   carried in consecutive chunks; the PAX records `FKBR.off` and
   `FKBR.total` give each segment's offset and the file's full size. Each
   chunk's meta (`size`, `sha256`, `first` entry, `bad` files) is published
   after the chunk itself. The source writes `COMPLETE` after the last
   chunk.
3. **Backpressure.** Before staging a chunk, the source waits until its
   staged bytes for this job stay under `job_quota` and its bytes for all
   jobs (`data/<self>/`) under `staging_quota`. It deletes chunk N once
   every target's ack count is above N.
4. **Consume.** A target reads chunks in order. It verifies size and
   sha256, extracts into `<root>/.fkbr-tmp/<job>/tree` through `os.Root`,
   then raises its ack count. Files listed in `bad` (changed or unreadable
   while the source read them) are dropped and reported.
5. **Install.** After the last chunk, the target renames entries into
   `dst_dir` under the conflict policy (`skip`, `overwrite`,
   `overwrite-if-older`). It applies directory metadata deepest first (only
   to directories it created, unless the policy is `overwrite`), then
   removes the staging area.
6. **Finish.** Every participant publishes its own status. The job's state
   is derived from them: `done` when all are done, `failed` if any failed,
   `cancelled` once all have stopped after `CANCEL`. The source removes
   `data/<src>/<job>/` when it finishes.

**Metadata.** The mode is applied first (setuid and setgid are stripped
unless `allow_setid`), then mtime, then ownership. Ownership goes last
because once a file belongs to someone else, a non-root daemon can no
longer change it. `chown` clears setuid and setgid, so those bits are
re-applied afterwards, which needs CAP_FOWNER. Ownership is best-effort:
it needs root or CAP_CHOWN, and for directories also CAP_DAC_OVERRIDE (see
`operations.md`). Failures are counted and reported.
Symlink mtimes are not preserved. Hard links arrive as separate files.
Devices, fifos and sockets are skipped and reported.

**Crash recovery.** The `send`/`receive` request stays in `inbox/<id>/cur/`
until its job ends, so a restarted worker picks it up again.
- *Target:* continues from its ack count. Staging persists, and re-applying
  a chunk is idempotent.
- *Source:* regenerates the stream from the start (the walk is
  deterministic) without re-staging chunks that were already published. If
  a regenerated chunk differs from a still-staged one, the source changed,
  and the job fails rather than mixing old and new data.

## Fan-out, groups and write policy

**Groups.** A job may have several targets. `@all` expands to every
registered node except the source. Each chunk is staged once,
and the source deletes it when every *active* target has acked it. A
destination of `@role` expands, at submit time, to every registered node
carrying that role except the source; roles come from each node's own
`roles:` config, published in `info.json`. Group members that are offline
at submit are recorded as detached with the reason `offline at submit`.

**Detaching.** While the source is blocked (no staging space, or waiting
for final acks), it drops a target that is holding things up:
- the target is offline and has made no progress for `offline_after`, or
- it has made no progress for `transfer.detach_after`, or
- it reported `failed`.

The last active target is never dropped. With a single target, a failure
fails the job and an offline target makes the job wait (cancel it to
stop). A detached target that notices (immediately, or when it comes back)
discards its staging area and reports `detached`. Nothing partial is ever
installed. A job that finished for some targets but detached others ends
`partial`.

**Catch-up.** `CatchUp(job)` (`fkbr catchup JOB`) submits a new job with
the same request whose targets are the detached nodes that are online
now. It records `catch_up_of`. It re-sends everything; a `sync` to the
same targets copies only what differs.

**Write policy.** `accept_writes_from` (node IDs or `@role`) lists who may
initiate writes into a node; empty means anyone, and a node may always
pull into itself. The initiator checks it at submit, and the receiving
node enforces it again before writing. This is a policy guard between
cooperating nodes, not security: anyone who can write to the share can
forge requests.

**Cancel.** Anyone may write `CANCEL`. Participants notice within a few
seconds; the source removes its staged data and targets remove their
staging areas.

## Sync

A sync makes `dst_path` on each target match `src_path` on the source. It
is one-way, in `incremental` mode (copy new and changed entries) or
`mirror` mode (also delete extra entries).

1. **Scan.** The planning engine sends `scan` to the source and to every
   target, and scans in parallel. Each node walks its directory and writes
   a snapshot to `blobs/scans/<plan>/<node>.snap`: zstd-compressed JSON lines in
   walk order (depth-first, names sorted per directory), after a header.
   Progress comes back as non-final reply parts every 2 seconds. A scan
   fails if a node sends no progress for 4 × `request_timeout`. An entry
   that cannot be read is recorded as type `e`; a directory whose listing
   failed is followed by an `e` line for itself.
2. **Plan.** One streaming k-way merge of the source snapshot with all
   target snapshots. Paths compare component-wise (`a/x` < `a-b`), which
   matches walk order, so memory stays flat: 1M entries took 16 MB. Per
   target:
   - *new* → copy;
   - *changed* (size, mtime in whole seconds, mode without setuid/setgid,
     symlink target, ownership only where that target can apply it,
     sha256 with `checksum`) → copy;
   - *type changed* → delete, then copy;
   - *extra* (mirror only) → delete, collapsed to the top-most directory.
   A directory whose contents change on a target gets its own entry
   re-sent, so its mtime is restored and the next plan converges to
   nothing. Nothing is ever deleted below a source path that could not be
   read.
3. **Groups.** Each entry goes into the path list for the exact set of
   targets that need it (`blobs/plans/<plan>/copy-<mask>`). Entries needed
   by several targets are thus bundled once and fanned out. Deletions go
   to `delete-<node>`. Paths are JSON-quoted, one per line. The plan
   record is `plans/<plan>/plan.json`.
4. **Apply.** Removals beyond `max_deletes` on any target block the apply
   unless it is forced. Deletions run first, in batches of 1000 through
   ordinary `remove` requests. Then each group becomes a transfer job with
   `paths_blob`, `base_dir` = `src_path` and `no_recurse` (each listed
   entry is sent alone), with conflict policy `overwrite`. A plan is
   applied at most once.

Offline members of a `@role` destination are skipped and listed; the
next sync catches them up. Plans and snapshots are pruned after
`keep_done` (default 1h) by running nodes, or on demand with `prune`.

## Mount recommendations

The Linux NFS client caches directory attributes and lookups by default
(`acdirmin` is 30 s), which can delay how soon a node sees new requests. For
responsive browsing, mount the share on every node with short attribute
caching:

```
nfs  rw,hard,actimeo=1,lookupcache=positive  0 0
```

`lookupcache=positive` stops the client from caching "file not found",
which the reply polling depends on. Use the same export on all nodes.
