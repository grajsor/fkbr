# fkbr NFS mailbox protocol (version 3)

Nodes share nothing but one directory, normally an NFS mount. They never
open network connections to each other. This document describes what lives
on the share and the rules every node follows. The implementation is
`internal/store`; `internal/transport/nfsmailbox` opens it on NFS, with
the refresh trick below.

## Layout

```
<nfs_root>/
├── protocol-version                 "3\n"; a node refuses to start on a mismatch
├── tmp/                             every file is written here first
├── nodes/<id>/info.json             registry entry (api.NodeInfo)
├── nodes/<id>/heartbeat             rewritten every heartbeat_interval; holds the worker's instance token
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
├── data/<src>/<job>/acks/<node>     number of chunks <node> has consumed
└── blobs/{plans,scans}/<id>/        sync plans and scan snapshots (see Sync)
```

Node and root IDs must match `^[a-z0-9][a-z0-9_.-]{0,62}$`.

## Rules

1. **Open modes.** Directories are created `0777` (no sticky bit) and files
   `0644`, set explicitly so the umask cannot narrow them: nodes run as
   different users and create, rename and delete entries in each other's
   directories. Files are never written in place, so nobody needs write
   access to a file. A node chmods entries it owns that have other modes
   once per process (shares from earlier versions used `0770`/`0660`).
2. **Wait by listing.** A node waiting for a file another node will create
   (a reply, the next chunk, an ack, `COMPLETE`) lists the directory and
   opens the name only once it is listed. Looking the name up early caches
   "does not exist", and NFS clients may keep that even after the file
   appears.
3. **Atomic publication.** A file is written to `tmp/`, fsynced, closed and
   then renamed to its final name. Readers ignore everything else. Since
   `tmp/` is on the same export, the rename is atomic. Close-to-open
   consistency guarantees a reader that sees the name also sees the content.
4. **One consumer per inbox.** Only node `<id>` reads `inbox/<id>/`, so
   there are no claim races and NFS locks are never used. On the node itself,
   a `flock` on local disk (`state_dir/worker.lock`) keeps a second worker
   from starting. The lock file says `running <pid>` or, once the worker
   got SIGTERM and pauses its jobs, `stopping <pid>`: a worker started
   meanwhile waits for it (up to 2 minutes) instead of giving up. A
   stopping worker stops consuming after the chunk in hand.
5. **Claiming.** The worker renames `new/X` to `cur/X` before processing,
   then deletes it, or moves it to `failed/` (pruned after `keep_done`).
   After a crash, anything left in
   `cur/` is processed again, so request handlers must be idempotent. A
   request that cannot be read is retried on the next poll without holding
   up the others, and moved to `failed/` after 20 tries.
6. **Replies.** Before publishing a request, the requester creates
   `reply/<self>/<req>/`. The executor writes parts `00000000.json`,
   `00000001.json`, ... The last part has `"final": true`, and a part with
   `"error"` ends the stream. Once done, or on timeout, the requester deletes
   the directory. An executor that finds the directory missing stops, because
   nobody is listening any more.
7. **Liveness.** `heartbeat` is recreated through the usual tmp+rename path,
   so its mtime comes from the NFS server. To judge a peer, a node writes its
   own probe file to `tmp/` and compares the two mtimes, which both come from
   the server clock. Host clock skew therefore doesn't matter. A node counts
   as offline when its heartbeat is older than `offline_after`. The
   heartbeat runs on its own, so a slow request does not delay it. Bulk
   writes (chunks, snapshots, path lists) are synced after about 2 s worth
   of data at the measured rate (4 to 64 MiB), so on a slow link the
   heartbeat never queues behind a whole chunk in the NFS client. (Over
   ssh the store is on local disk and nothing is synced.) Pruning
   judges ages the same way, by mtimes against the server clock, and job
   IDs and times use the server clock too: each node keeps the offset of
   its own clock from the mtime of its heartbeat.

   The heartbeat file holds the worker's instance token (`w1 <start ns>
   <random>`), also published in `info.json` with the worker's `pid` and the
   host's `boot_id`. A worker refuses to start under an ID whose online
   entry belongs to another host, or to a process still running on this
   host (a second user, or another `-state` dir: both would consume one
   inbox and install into their own directories). A crashed worker's entry
   can be replaced at once. If a heartbeat finds another worker's token,
   the worker that started later stops, without leaving the registry.
   A heartbeat that finds the node's `info.json` gone or unreadable registers it again,
   and a write, probe or inbox read that finds `tmp/` or one of the node's
   own directories gone recreates them, so a share cleaned up under
   running nodes heals. Entries in `nodes/` that cannot be read are left
   out of the node list; `forget` removes one once its heartbeat is stale.
   A process that only looks (a CLI command where no node runs) creates
   none of its node's directories.
8. **Leaving.** A node is removed from the registry (its `nodes/`,
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
   registry: nodes online, roots exist, the destination root is writable;
   when it is the source, the named paths exist. It writes
   `jobs/<job>/job.json` and sends `send` to the source node.
2. **Produce.** A source that does not find a named path fails the job
   before it sends anything (a sync's transfer instead notes entries that
   vanished since the plan). The source sends `receive` to each target, then walks the
   source paths (lexical, depth-first) into a bundle: a sequence of chunks,
   each complete in itself (zstd optional). The first chunk holds about
   4 MiB and each next one twice as much, up to `chunk_size`, so targets
   start within moments; the sizes depend only on the chunk number, so a
   resumed source regenerates the same chunks. The walk runs ahead and
   small files (up to 1 MiB) are read by a few workers in the meantime
   (at most 64 MiB ahead); the bundle keeps the walk order. A file bigger
   than the space left in a chunk is split into segments carried in
   consecutive chunks. A sparse file (fewer blocks than its size) is sent
   as its data only: segments at their offsets, holes of 64 KiB or more
   left out (`SEEK_DATA`/`SEEK_HOLE`), and an empty segment at 0 or at
   its size where it starts or ends in a hole, so receivers need nothing
   new. Each
   chunk's meta (`size`, `xxh3`, `first` entry, `bad` files) is published
   after the chunk itself. The source writes `COMPLETE` after the last
   chunk.
3. **Backpressure.** Before staging a chunk, the source waits until its
   staged bytes for this job stay under `job_quota` and its bytes for all
   jobs (`data/<self>/`) under `staging_quota`. It deletes chunk N once
   every target's ack count is above N.
4. **Consume.** A target reads chunks in order. It verifies size and
   checksum, extracts into `<root>/.fkbr-tmp/<job>/tree` through `os.Root`,
   then raises its ack count. It reads each chunk ahead (up to 16 MiB) while
   it writes files, and sets a file's metadata through the open file. Files listed in `bad` (changed or unreadable
   while the source read them) are dropped and reported.
5. **Install.** After the last chunk, the target renames entries into
   `dst_dir`. A directory that does not exist there yet moves in whole,
   with one rename. Everything else follows the conflict policy (`skip`, `overwrite`,
   `overwrite-if-older`). An entry that cannot be renamed into place (say,
   an unwritable directory) is reported as an error; the rest is still
   installed. Each file is replaced whole, so two jobs installing into
   the same directory at once leave every file complete, each from one of
   them. It applies directory metadata deepest first (only
   to directories it created, unless the policy is `overwrite`), then
   removes the staging area.
6. **Finish.** A participant whose job slots (`max_jobs`) are all taken
   publishes the state `queued` until one frees up; a source that drops
   such a target says so in the reason. Every participant publishes its
   own status. The job's state
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

## Chunk format

A chunk (before optional zstd) is the magic `FKB2`, its entries, and a
0 byte. Integers are LEB128 varints; the mtime is zigzag-encoded.

```
kind              'D' directory, 'L' symlink, 'F' file (or a segment of one)
shared, n, bytes  name = previous name[:shared] + bytes (n of them)
mode uid gid mtime_ns
'L': n, target
'F': size offset length, then length bytes of data, unpadded
```

Names share their beginning with the previous entry's, which in walk
order is usually the whole directory path, so an entry costs about 10
bytes plus the part of its name that differs. (The tar format used before
protocol 2 cost about 1.5 KiB per entry.)

A receiver trusts nothing in a chunk: names are cleaned (no absolute
paths, nothing climbing above the bundle root, no NUL, at most 64 KiB:
paths may be longer than PATH_MAX; the sender skips and reports longer
ones), the shared prefix cannot be longer than the previous name, mode,
IDs, sizes and offsets are range-checked, a segment must lie inside its
file, link targets are at most 4096 bytes, and an unknown kind, missing
data, a missing end byte or data after it is an error. The checksum in
the meta (`xxh3`: xxh3-128, hex) covers the whole stored chunk. It
guards against damage, not against forgery: the share is open by design.
Before protocol 3 it was SHA-256, which costs about 2.8 s per GiB on each
end of a CPU without SHA instructions; xxh3 takes a few hundredths. Extraction then goes through
`os.Root`, so nothing can land outside the destination root either way.

## Fan-out, groups and write policy

**Groups.** A job may have several targets. `@all` expands to every
registered node except the source. Each chunk is staged once,
and the source deletes it when every *active* target has acked it. A
destination of `@role` expands, at submit time, to every registered node
carrying that role except the source; roles come from each node's own
`roles:` config, published in `info.json`. Group members that are offline
at submit are recorded as detached with the reason `offline at submit`.
A named target that is offline fails the request, unless it sets
`detach_offline`: then it is detached the same way. The TUI sets it when
it expands `@dest` (the nodes marked in its node list) into names; `all`
and `dest` are never roles.

**Detaching.** While the source is blocked (no staging space, or waiting
for final acks), it drops a target that is holding things up:
- the target is offline and has made no progress for `offline_after`, or
- it has made no progress for `transfer.detach_after`, or
- it reported `failed`.

A target whose own status says `done` is never dropped, whatever its acks
say: a source back from a stall on the share can read stale ack listings
for a while, and the target installed everything.

The last active target is not dropped for being slow or failing (a
failure then fails the job), but it is once it has been offline for
`transfer.detach_after`: the job ends `partial` and frees the source's
job slot; `catchup` sends it later. A source with no active target left
is done. A share that stays full for `transfer.detach_after` while none
of the job's chunks are left on it for its targets to free fails the
job: only someone else could make room. A target that was forgotten (gone from the registry for
`offline_after`) is dropped too; if it was the last one, the job fails. A
target whose source was forgotten fails the same way. A target that has
not started (no status yet) is sent its receive request again every
minute, in case the first one was lost or could not be written (an
unwritable inbox delays that target, nothing else). When the source is
done but a target never said so and is forgotten or offline for longer
than `detach_after`, the job reads as `partial` with that target
detached; if it comes back, it installs and the job is done. Likewise,
when every target has ended but the source never said so and is
forgotten or offline for longer than `detach_after`, the job reads as
`done` (or `failed` if a target failed). A detached target that notices (immediately, or when it comes back)
discards its staging area and reports `detached`. A node that was down
meanwhile removes leftover staging when it prunes (at start and every
half `keep_done`): of jobs it was detached from, jobs whose part it has
ended, and jobs no longer on the share (after 10 min). Nothing partial is ever
installed. A job that finished for some targets but detached others ends
`partial`.

**Catch-up.** `CatchUp(job)` (`fkbr catchup JOB`) submits a new job with
the same request whose targets are the detached and the failed targets
that are online now. It records `catch_up_of`. It re-sends everything; a
`sync` to the same targets copies only what differs.

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
   Progress comes back as non-final reply parts every 2 seconds (entries,
   bytes, errors, and `dir`: the folder the walk is in, root-relative;
   older builds send none). A scan
   fails if a node sends no progress for 4 × `request_timeout`. An entry
   that cannot be read is recorded as type `e`; a directory whose listing
   failed is followed by an `e` line for itself. Names that are not valid
   UTF-8 are stored as NUL + base64 of their bytes (JSON would turn them
   into U+FFFD); the same form carries such names in every request.
   A target's header says what its filesystem cannot store: mtimes beyond
   the range it clamps to (probed once per filesystem with a temporary
   file in the target folder, whose times are put back: XFS without
   bigtime stops at 2038), and its name rules (vfat,
   exFAT, ext4 casefold: forbidden characters, trailing dots, ASCII only,
   no symlinks, names equal but for case or Unicode form). Excludes match
   case-insensitively there.
2. **Plan.** One streaming k-way merge of the source snapshot with all
   target snapshots. Paths compare component-wise (`a/x` < `a-b`), which
   matches walk order, so memory stays flat: 1M entries took 16 MB. Per
   target:
   - *new* → copy;
   - *changed* (size, mtime in whole seconds, mode without setuid/setgid,
     symlink target, ownership only where that target can apply it,
     xxh3-128 with `checksum`) → copy;
   - *type changed* → delete, then copy;
   - *extra* (mirror only) → delete, collapsed to the top-most directory.
   A directory whose contents change on a target gets its own entry
   re-sent, so its mtime is restored and the next plan converges to
   nothing. Nothing is ever deleted below a source path that could not be
   read. Mtimes are compared as the target can store them, and entries
   the target cannot store (by its name rules) are left out, counted
   (`refused`) and listed once as a plan problem.
3. **Groups.** Each entry goes into the path list for the exact set of
   targets that need it (`blobs/plans/<plan>/copy-<mask>`). Entries needed
   by several targets are thus bundled once and fanned out. Deletions go
   to `delete-<node>`. Paths are JSON-quoted, one per line. The plan
   record is `plans/<plan>/plan.json`. A target in several groups gets one
   job per group, running at the same time; each such group also lists the
   folders its entries go into (up to the top), so whichever job finishes
   last restores their mtimes.
4. **Apply.** Removals beyond `max_deletes` on any target block the apply
   unless it is forced. Deletions run first, in batches of 1000 through
   ordinary `remove` requests that name the plan: the target deletes only
   what its own scan for the plan saw as it still is (type; size and
   mtime of files; symlink targets). Whatever is new or changed since the
   dry run stays, with the folders holding it, and is listed as a plan
   problem; the next dry run shows it. A target on an older build would
   ignore the plan name and delete it anyway, so an apply that deletes
   there is refused (feature `recheck`). Then each group becomes a transfer job with
   `paths_blob`, `base_dir` = `src_path` and `no_recurse` (each listed
   entry is sent alone), with conflict policy `overwrite`. A plan is
   applied at most once.

Offline members of a `@role` destination are skipped and listed; the
next sync catches them up. Plans and snapshots are pruned after
`keep_done` (default 1h) by running nodes, or on demand with `prune`;
never while an unfinished job uses them, and never younger than ten
minutes (`prune -keep 0` included), so a sync being planned or applied
keeps them. Finished jobs stay at least a minute after their last
status, so the CLI that follows one reads how it ended.

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
