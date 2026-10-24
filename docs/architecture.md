# Architecture

How fkbr is built: the layers, how the parts talk over an NFS share and
over ssh, and what each package does. The details live elsewhere:
`protocol.md` (what is on the share), `ssh.md` (nodes over ssh),
`operations.md` (running it).

## The idea

Every host runs the same binary as a **node**. A node shares one or more
directories (its **roots**) and does the work for them: it lists them,
reads from them and writes into them. Nodes are equal; there is no master.
A copy, move or sync is a **job** with one source node and one or more
target nodes.

fkbr opens no port of its own. Nodes exchange requests and records
through a **store**:

- **over NFS** the store is a directory on the share that every node
  mounts, and the data goes through it too, as staged chunks;
- **over ssh** the store is a private directory on the **home node**,
  which starts the other nodes over ssh and answers their calls over
  those sessions; data goes from source to target over the same
  sessions, never to disk.

The engine does not know which one it has: both implement the same two
interfaces (`transport.Transport` and `transport.Store`).

## Layers

```
 Frontends    ┌─────────────┐  ┌─────────────┐
              │ TUI         │  │ CLI         │      frontend/{tui,cli}
              │ (bubbletea) │  │ cp, sync, … │
              └──────┬──────┘  └──────┬──────┘
                     └───────┬────────┘
                             │ api.Client
 API          ┌──────────────┴───────────────┐
              │ api.Service: nodes, list,    │      api
              │ jobs, sync plans, events     │
              └──────┬────────────────┬──────┘
                in-process       unix socket: HTTP + JSON,
                api/local        events as SSE (api/remote)
                     └───────┬────────┘
 Engine       ┌──────────────┴────────────────────────────┐
              │ jobs · send · receive · fan-out · detach  │  engine
              │ sync plans · file ops · events · cleanup  │
              └──┬─────────────┬────────────────┬─────────┘
                 │             │                │
 Subsystems  Transport      Store            on the node itself:
             (registry,     (jobs, statuses, bundle    chunk format
              requests,      chunks, acks,   receive   installing
              replies)       blobs)          inventory scans, plans
                 │             │             localfs   the roots
                 └──────┬──────┘             exclude, mountinfo
                        │
          ┌─────────────┴──────────────┐
 Stores   store on NFS                 sshlink: the home node's store
          (store + nfsmailbox)         in its private dir; remote
                                       nodes call it over their link
```

A frontend uses only `internal/api`. It runs the engine in its own
process (a TUI that is the node) or attaches to a node already running on
this host through its socket (`/tmp/fkbr-<uid>-<node>/`). A `daemon` is
the same engine with no frontend.

## Over an NFS share

```
     host a                     host b                     host c
 ┌────────────┐             ┌────────────┐             ┌────────────┐
 │ node a     │             │ node b     │             │ node c     │
 │ engine     │             │ engine     │             │ engine     │
 │ roots: /x  │             │ roots: /y  │             │ roots: /z  │
 └─────┬──────┘             └─────┬──────┘             └─────┬──────┘
       │ NFS                      │ NFS                      │ NFS
 ══════╧══════════════════════════╧══════════════════════════╧══════
   share/  nodes/    registry: info.json + heartbeat per node
           inbox/    requests, one inbox per node (new/ cur/ failed/)
           reply/    answers to requests
           jobs/     job.json, statuses, COMPLETE, CANCEL, detached
           data/     staged chunks + their metas + acks
           blobs/    sync scans and plans
           tmp/      every file is written here, then renamed
```

- **Liveness.** Each node rewrites its heartbeat file every 5 s. A node is
  online while its heartbeat is fresh, judged by mtimes on the NFS
  server's clock, never the local one.
- **Requests.** A request (list a folder, scan, remove, send) is a file
  renamed into the target node's `inbox/<id>/new/`. Only that node
  consumes it; the answer comes back under `reply/<sender>/`.
- **Writes are atomic.** Every file is written in `tmp/` and renamed into
  place, never edited in place. A reader sees all of a file or nothing.
- **NFS caching.** Clients may cache directory listings for up to 30 min
  (`actimeo`). fkbr makes and removes a tiny `.r-*` file in a directory
  before it reads it, which forces a fresh listing, and it waits for
  files by listing a directory, never by looking a name up (a failed
  lookup can be cached as "missing" for good).

### A copy over NFS

```
   initiator           source                    targets
   (the TUI/CLI's      ──────                    ───────
    node)
   job.json ─────────▶ walks the paths
   "send" ───────────▶ packs files into chunks
                       (FKB2, xxh3-128, zstd -z)
                       data/<src>/<job>/N.chunk,
                       then N.json: published ─▶ reads chunk N, checks
                       "receive" to each target  it, stages the files,
                                                 acks/<node> = N
                       deletes chunk N once
                       every active target
                       acked it
                       COMPLETE: chunk count ──▶ installs: renames the
                                                 staged files into place,
                                                 sets metadata
                       status done ◀─────────── status done
```

- **Chunks.** Chunks grow 4, 8, 16 … MiB up to `chunk_size`. Small files
  are read ahead by 8 workers; big files span several chunks.
- **Fan-out.** One chunk serves every target. `job_quota` bounds what one
  job stages and `staging_quota` all of them.
- **Slow and dead targets.** A target offline for 30 s, or without
  progress for 10 min, is detached so the others go on; `catchup` sends
  the job to it again later.
- **Installing.** Targets stage into `.fkbr-tmp` in the destination root
  and install only at the end, so a partial job installs nothing.
- **Restarts.** A restarted source or target resumes from the acks.

## Over ssh

```
                 home node (runs `fkbr ssh:b:/y,c:/z`)
              ┌──────────────────────────────────────┐
              │ engine, roots: its current directory │
              │ records: a store in its private dir  │
              │ (registry, inboxes, jobs, acks)      │
              │ answers each remote node's calls     │
              └───────┬──────────────────────┬───────┘
                      │ ssh -T b             │ ssh -T c
                      │ 'fkbr link'          │ 'fkbr link'
                      │ one session each:    │
                      │ framed streams,      │
                      │ flow control, pings  │
              ┌───────┴──────┐        ┌──────┴───────┐
              │ node b       │        │ node c       │
              │ engine       │        │ engine       │
              │ roots: /y    │        │ roots: /z    │
              └──────────────┘        └──────────────┘
```

- **Sessions.** The home node starts `fkbr link` on each host with the
  user's own `ssh` (keys, agent, `~/.ssh/config`, jump hosts all work).
  All traffic with that node is one ssh session, split into streams with
  their own flow control (`sshlink/frame`).
- **Records stay on the home node.** Its store sits in a private
  directory, and each remote node's calls are answered as that node, so
  owner checks hold. The home node is a trust boundary: every name that
  comes off the wire is checked before it becomes a path.
- **Chunks never touch a disk.** A source keeps its chunks in memory until
  every active target has read them. A target reads straight from its
  source; between two remote nodes the home node relays the stream in
  memory. Chunks are 32 MiB, `job_quota` is 128 MiB and `staging_quota` is
  a quarter of the RAM (at most 512 MiB), and a chunk waits for free
  memory.
- **One clock.** IDs, job times and liveness use the home node's clock.
- **Failures.**
  - A cut link reads as an I/O error, which transfers ride out for 3 min
    while the home node reconnects with backoff.
  - A restarted source rebuilds its chunks from the lowest one a target
    has not acked, and checks them against the last chunk each target
    applied.
  - When the home node quits, it stops its remote nodes first.

```
 a copy from b to c:   b's memory ──link──▶ home (relay, memory) ──link──▶ c
 a copy from home to c:          home's memory ──link──▶ c
```

## Sync

A sync makes one or more targets match a source, one way, like
`rsync -a SRC/ DST/`.

1. **Scan.** The node planning the sync asks each side to scan, as a
   `scan` request. Each scan streams a snapshot to the store as a blob,
   along with the target's limits: its mtime range and name rules
   (FAT/exFAT, case folding).
2. **Plan.** The planner merges the sorted snapshots in one pass, k-way,
   so memory does not grow with the number of files. Its output is a plan
   blob: per target, the lists of what to copy and what to delete. A dry
   run (`sync -n`) stops here and prints the plan's ID.
3. **Apply.** Deletes run first, and only those the plan listed. Then
   comes one copy job per group of targets that need the same files, so
   each file is sent once for all of them. `max_deletes` guards against a
   mistake.

## A node's life

- **Start.** A node takes a lock in its state dir, registers in the
  registry and refuses an ID that is running elsewhere (an instance token
  in the heartbeat). It then starts the heartbeat on its own goroutine
  and the worker that fetches requests.
- **While running.** It answers requests, runs its share of jobs, prunes
  finished jobs, plans and failed requests after `keep_done`, and stops
  if its root is deleted or replaced.
- **Stop.** It finishes writing its statuses and leaves the registry,
  unless an unfinished job still needs it.
- **Self-healing.** If the share loses a node's folders, the node creates
  them again; if its registry entry is gone, it registers again.

## Packages

| Package | What it does |
|---|---|
| `cmd/fkbr` | arguments, starting a node (share, `ssh:`, or on its own), `fkbr link` (the remote end over ssh) |
| `internal/frontend/tui` | the Norton Commander-style TUI: panes, dialogs, mouse, help, links to ssh hosts (F7/F8) |
| `internal/frontend/cli` | the commands: `cp`, `mv`, `sync`, `ls`, `rm`, `jobs`, `nodes`, … |
| `internal/api` | the API every frontend uses: `Service`, types, error codes; names that are not UTF-8 travel as NUL+base64 |
| `internal/api/local` | the API in-process: engine + store |
| `internal/api/remote` | the API over the node's unix socket (HTTP + JSON, events as SSE) |
| `internal/engine` | jobs, sending, receiving, fan-out and detaching, sync plans and applying, file operations, events, the worker lock, cleanup |
| `internal/transport` | the `Transport` (registry and requests) and `Store` (jobs, chunks, acks, blobs) interfaces |
| `internal/store` | both interfaces over a directory: the share, or the home node's private dir |
| `internal/transport/nfsmailbox` | the store on NFS, with the refresh trick against attribute caching |
| `internal/sshlink` | nodes over ssh: the home end (records, relay), the remote end, sessions, chunks in memory |
| `internal/sshlink/frame` | the link protocol: frames, streams, flow control, keepalive |
| `internal/bundle` | fkbr's chunk format (FKB2): strict reader, optional zstd, big files split over chunks |
| `internal/receive` | installing on a target: staging in `.fkbr-tmp`, confined to the root (`os.Root`), metadata in the right order, holes for zero blocks |
| `internal/inventory` | streaming scans, snapshots and the k-way merge planner |
| `internal/localfs` | the roots: confined access, capability probes (chown, DAC), name rules, mtime range, private-dir checks |
| `internal/exclude` | rsync-like exclude patterns |
| `internal/mountinfo` | reads `/proc/self/mountinfo` to spot NFS attribute caching |
| `internal/config` | flags, the config file and defaults; the TUI's saved ssh hosts |
| `internal/check` | `fkbr check`: the share, the mount options, ssh hosts |
| `internal/safetext` | cleans text from elsewhere before a terminal draws it |
| `internal/atomicfile`, `internal/id` | write-then-rename files; sortable unique IDs from the store's clock |
| `internal/product` | the name, version and environment prefix, in one place |

## What keeps it safe

- **Nothing partial is installed.** Targets stage and rename into place at
  the end. A cut-off job leaves the destination as it was, apart from the
  staging, which is swept.
- **Every chunk is checked.** Each chunk's xxh3-128 checksum is in its meta
  and checked on both ends, and the chunk reader refuses anything out of
  bounds.
- **Writes stay inside the roots.** A target writes only inside its roots
  (`os.Root`), so a symlink cannot lead it out.
- **Names from the store are validated.** Records from the share or the
  wire are validated (IDs, sizes, names) before use.
- **One worker per node ID,** even across hosts.
- **The tests behind these rules** are listed in `testplan.md`.
