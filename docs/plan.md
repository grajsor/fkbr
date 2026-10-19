# fkbr — multi-node file commander over a shared NFS mailbox

> **Implementation notes (deviations from this original plan).**
> - No installation of any kind: no RPM, install script, systemd unit or
>   system user. One binary runs from wherever it is copied, configured by
>   flags (or an optional `fkbr.yaml` beside it). `fkbr tui` is itself the
>   node's worker when no `fkbr daemon` runs.
> - No SQLite inventory: sync uses streaming snapshots on the share and a
>   k-way merge (flat memory; 1M entries planned in 18 s at 16 MB).
> - Sync is one-way (incremental or mirror); bidirectional sync with a
>   baseline was not built.
> - Remote API is HTTP+JSON over the Unix socket, not JSON-RPC.
> - A swarm instead of master/slave: `fkbr /mnt/nfs/dir` registers the host
>   (named after its hostname) sharing the current directory; targets are a
>   node, a list or `@all`. Roles remain optional tags.
> - Added along the way: mkdir/remove/read on any node, `fkbr check`.
> - Runs on any Linux with kernel 3.10 or newer, not only RHEL 7–9; the
>   lab added Rocky 10 and Debian 12 (the Pi).
> - Chunks and sync snapshots use zstd, not gzip: on the lab NAS it was
>   20–30% faster than gzip, and faster than lz4 (see `docs/benchmarks.md`).
> - A TUI that runs the node itself leaves the share when it quits, unless
>   a daemon had registered the machine; nodes offline for `forget_after`
>   (7 days) are forgotten automatically.

## Context

Several RHEL hosts (a mix of RHEL 7 with glibc 2.17 / kernel 3.10, and RHEL 9 with glibc 2.34) each have their own local filesystem. The **only** thing they share is an NFS mount, with no network path between them. We want a Norton/Total Commander-style tool: start `fkbr` on each host and point it at the NFS directory. You can then browse any node, send files, ask a node to tar/zstd trees and send them, and run incremental or mirror syncs over millions of files and tens of thousands of directories.

The design must allow for:
- **N nodes, not 2.** One "master" can push the same data to many "slaves" (fan-out). Point-to-point between any two nodes must also work.
- **Modularity.** A headless core engine with a stable API. The TUI, a later web UI, the CLI, and anything else are pluggable frontends on top of it.

This plan builds on the earlier ChatGPT discussion and fixes a few points in it (see the end).

Decisions made:
- **Name:** `fkbr`, one Go module and one binary.
- **First frontend:** a dual-pane TUI, plus the CLI. A web UI comes later and uses the same API.
- **Privileges:** runs as a service account. Ownership is preserved best-effort; capability is detected at startup and reported.
- **NFS size:** unknown, so staging is always chunked, with a quota and backpressure.
- **Sync modes:** manual copy, incremental and mirror, from any node to any other node.
- **Metadata to preserve:** mode, mtime, symlinks and ownership (best-effort).

## Build and portability

- Pure Go: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o fkbr ./cmd/fkbr`. This dev machine is an arm64 Pi with Go 1.27.2, so the build is always a cross-compile.
- No cgo dependencies. The local DB uses **`modernc.org/sqlite`** (pure Go).
- Avoid syscalls that have no fallback on kernel 3.10 (`openat2`, `statx`-only code, `copy_file_range` without a fallback). `os.Root` falls back on old kernels, but still test it on RHEL 7.
- `make dist` produces `fkbr-linux-amd64` plus a sha256. Check it with `file` and `ldd` (expect "not a dynamic executable").

## Layered architecture (modularity)

```
 Frontends (pluggable)   ┌────────┐ ┌────────┐ ┌────────┐ ┌──────────┐
                         │  TUI   │ │  CLI   │ │ Web UI │ │ scripts/ │
                         │(bubble │ │        │ │ (later)│ │ cron     │
                         │  tea)  │ │        │ │        │ │          │
                         └───┬────┘ └───┬────┘ └───┬────┘ └────┬─────┘
                             └──── api.Client (Go interface) ──┘
                                 │ in-process   │ or local socket
 API layer               ┌───────┴──────────────┴────────┐
                         │ api: Service interface + event │  JSON-RPC/HTTP over
                         │ stream, DTOs (no UI concepts)  │  unix socket (0600)
                         └───────────────┬────────────────┘
 Engine (headless)       ┌───────────────┴────────────────────────────────┐
                         │ jobs · scheduler · fanout · syncplan · browse  │
                         └──┬──────────┬──────────┬──────────┬────────────┘
 Subsystems           transport   bundle    receive    inventory   localfs
 (behind interfaces)  (NFS        (tar/zstd (safe      (SQLite     (stat/list/
                       mailbox)    chunks)   extract)   scanner)    metadata)
```

Rules that keep it modular:
- **`internal/api` is the only thing frontends import.** It defines `Service` (methods such as `Nodes()`, `List(node, root, path, page)`, `Submit(JobSpec)`, `Cancel(id)`, `Jobs(filter)`, `PlanSync(...)`, `ApplyPlan(...)`) and `Subscribe() <-chan Event` (progress, job state, peer online/offline). The DTOs are plain structs with JSON tags.
- **Two implementations of `api.Client`:** `api/local`, which wraps the engine in-process, and `api/remote`, which talks JSON-RPC/HTTP to `fkbr daemon` over a unix socket. The TUI connects to a running daemon if it finds one; otherwise it embeds the engine. A later web UI is just an HTTP handler over the same `Service` plus static assets, served on localhost or the socket.
- **The engine never imports frontend code.** The engine depends on interfaces (`transport.Transport`, `inventory.Store`, `localfs.FS`), so tests can swap in fakes. Swapping NFS for something else later (SSH, S3) means writing a new `Transport`.
- **Frontend registry.** `cmd/fkbr` wires subcommands (`tui`, `daemon`, `web`, `ls`, `get`, ...). Each frontend is a package with one `Run(ctx, api.Client, args)` entry point, so adding one does not touch the engine.

## Nodes, roles and fan-out

- **Node registry on NFS:** `nodes/<id>/info.json` holds the node ID, hostname, OS, fkbr version, protocol version, exported roots, role tags and ownership capability. `nodes/<id>/heartbeat` is touched periodically.
- **Roles are policy, not code.** The protocol is symmetric: every node runs the same worker. "Master" and "slave" are config:
  - `accept_writes_from: [master1]` on slaves. A slave rejects write jobs from anyone else.
  - `groups: {web: [s1, s2, s3]}`, so jobs can target a group.
  - A node can be read-only (it only sends).
- **Fan-out data path (stage once, consume many).** A master pushing to K slaves writes each chunk to NFS **once**:
  ```
  data/<job>/manifest.json        targets: [s1,s2,s3]
  data/<job>/chunk-00001          sha256 in manifest
  data/<job>/acks/<node>/00001    written by each receiver after verify+extract
  data/<job>/COMPLETE
  ```
  A chunk is deleted when every *active* target has acked it. Quota is counted per staged chunk, not per target.
- **Slow or offline targets:** each target gets a `max_lag_chunks`. A target that falls behind or goes offline is **detached** and marked `pending-catchup`, so the others keep moving. Its catch-up runs later as a separate point-to-point job; inventory-based sync makes that cheap.
- **Per-target state:** a job's result is a vector of per-target outcomes (done / failed / detached). The TUI and API show them per target.
- Remote **browse** works against any node: the frontend picks the node, and the pane shows `node:root/path`.

## NFS mailbox protocol

```
/nfs/fkbr/
├── protocol-version
├── nodes/<id>/{info.json,heartbeat}
├── inbox/<id>/{new,cur,done,failed}/<ulid>.json   # requests addressed to <id>
├── reply/<id>/<req-id>/...                         # replies back to the requester
├── data/<job-id>/...                               # staged chunks (see fan-out)
└── tmp/                                            # every write starts here
```

- **Per-node inbox with a single consumer.** Each request is addressed to one executor node, and only that node reads its inbox. There are no claim races and no reliance on NFS locks; this scales to N nodes unchanged. A fan-out job is one request to the source plus a "receive job X" notice in each target's inbox.
- **Atomic publication:** write in `tmp/`, then `fsync`, `close`, `rename` to the final name. Readers trust only final names, which close-to-open consistency makes safe.
- **Message types:** versioned JSON with `protocol: 1`:
  - `list` and `stat` reply with paged NDJSON.
  - `send`, `fetch`, `bundle`, `receive`.
  - `scan` and `export-inventory`.
  - `cancel`.
  - Progress goes in a separate small file that is replaced in place (tmp+rename).
- **Lifecycle:** `new` → `cur` → `done` or `failed`, plus a result record. Requests are idempotent by ID, and resume works from the manifest and the ack state.
- **Liveness without trusting host clocks:** compare the peer heartbeat's mtime with the mtime of a freshly touched probe file of our own. Both mtimes come from the NFS server's clock.
- **Polling:** about 200 ms when active, about 5 s when idle, configurable. Document mount options (`actimeo`/`lookupcache`). inotify does not work across NFS clients.
- **Backpressure:** the sender writes chunks only while staged bytes are under `staging_quota` (both global and per job).
- **Garbage collection:** the source GCs finished jobs. Any node can GC jobs whose owner has been dead longer than `gc_after`.

## Safe receive and metadata

- All paths are relative to a configured **root ID**. Reject absolute paths and `..`. Extract through `os.Root` scoped to the destination root so symlinks cannot escape.
- Extract into `<dest>/.fkbr-tmp/<job>/`, then rename into place. The conflict policy is checked at commit time: `skip`, `overwrite`, `overwrite-if-older` or `overwrite-if-unchanged-since-plan`.
- Apply metadata in this order: content → symlinks → `lchown` (best-effort; failures are counted and reported) → mode (setuid/setgid stripped unless explicitly allowed) → mtime last, with directories after their children.
- Detect chown capability at startup and publish it in `info.json`. Optional UID/GID mapping per node.

## Inventory and sync (millions of files)

- A local SQLite DB at `~/.local/state/fkbr/inventory.db`, **never on NFS**. Columns: `entries(root_id, rel_path, type, size, mtime_ns, mode, uid, gid, link_target, hash, scan_gen)`. Only a *complete* scan can infer deletions.
- The scanner uses a bounded worker pool, reading one directory at a time with `os.ReadDir`. It writes in batched transactions of about 10k rows and is resumable.
- **Sync flow:** each node exports a path-sorted, compressed NDJSON snapshot to NFS. A streaming merge-diff produces the plan (copy / update / delete / conflict). The plan always gets a dry-run preview, and applying it turns into bundle jobs.
- **Fan-out sync:** the master diffs against each slave's snapshot. Files needed by several slaves are bundled once and targeted to that subset.
- **Mirror deletes** need explicit confirmation and are capped by `max_deletes`. Bidirectional sync uses a baseline per root pair.
- The browser never walks recursively. It lists one directory at a time, paged.

## TUI (first frontend)

- `bubbletea` + `lipgloss`, built only against `api.Client`.
- Two panes; either pane can show any node (a node picker on Alt+F1/F2). Keys:
  - Space/Insert selects. F5 copies to the other pane's node, or to a group (multi-target).
  - F6 moves, F7 makes a directory, F8 deletes (with confirmation), F3 views.
  - A queue panel shows per-target progress.
- The status bar shows online nodes, ownership capability and staging usage.

## Project layout

```
fkbr/
├── cmd/fkbr/main.go            # subcommand wiring only
├── internal/
│   ├── api/                    # Service interface, DTOs, events
│   │   ├── local/              # in-process client (wraps engine)
│   │   └── remote/             # JSON-RPC over unix socket client + server
│   ├── engine/                 # jobs, scheduler, fanout, browse; implements api.Service
│   ├── transport/              # Transport interface
│   │   └── nfsmailbox/         # NFS implementation (inboxes, data, heartbeat, GC)
│   ├── bundle/                 # chunked tar(+zstd), manifest, checksums
│   ├── receive/                # os.Root extract, conflicts, metadata
│   ├── inventory/              # scanner, SQLite store, snapshots
│   ├── syncplan/               # merge-diff, mirror/incremental/baseline, fan-out grouping
│   ├── localfs/                # FS interface + real impl (list/stat/chown capability)
│   ├── config/
│   └── frontend/
│       ├── cli/
│       ├── tui/
│       └── web/                # later
├── docs/{protocol.md,api.md}  Makefile  go.mod
```

## Milestones

1. **Core and protocol:** config, `localfs`, the `transport` interface plus `nfsmailbox` (inboxes, atomic writes, node registry, heartbeat), the `api` interface, `engine` with browse, `api/local`. CLI: `fkbr nodes`, `fkbr ls node:root/path`. Static cross-built binary.
2. **Point-to-point transfer:** `bundle`, `receive`, chunking, quota, cancel/resume. CLI `get` and `put`.
3. **Fan-out:** multi-target manifests, per-target acks, lag detach and catch-up, groups, `accept_writes_from`.
4. **Daemon and remote API:** `fkbr daemon`, JSON-RPC over a unix socket, the event stream, a systemd unit.
5. **TUI** on `api.Client`.
6. **Inventory and sync:** the scanner, snapshots, incremental and mirror plans, fan-out sync, dry-run, delete caps.
7. **Hardening:** failure injection, benchmarks with 1 master and 3+ slaves, docs. The web UI is a follow-up on the same API.

## Changes from the ChatGPT design

- Per-node single-consumer inboxes instead of a shared claim queue. This scales to N nodes with no locks.
- Fan-out stages each chunk once and collects per-target acks. Lagging targets are detached.
- Liveness uses NFS server mtimes, not host clocks.
- Explicit staging quota with backpressure.
- Pure-Go SQLite, which `CGO_ENABLED=0` requires.
- A layered core with an API boundary, so frontends are plugins.

## Verification

- **Unit tests:**
  - atomic write and rename
  - manifest and checksum round-trip
  - tar escape rejection
  - conflict policies
  - the merge-diff table
  - fan-out ack and GC logic: a chunk is deleted only after every active target acks
  - lag detach
- **Engine tests with fakes:** `transport` and `localfs` fakes, so engine logic runs without disk or NFS.
- **Integration (local):** one test process runs 1 master and 3 slaves, each with its own root, sharing a temp dir as fake NFS. Fan out a tree and assert content and metadata on every slave. Kill a slave mid-job: the others finish, and the killed slave is detached and later caught up. Fill the quota and assert backpressure.
- **API contract:** run the same test suite against `api/local` and against `api/remote` over a socket.
- **Real environment:** the binary on RHEL 7 and RHEL 9 against the real NFS mount. `ldd` should report not dynamic. Benchmark few large files, 50k small files and 2M files in deep trees, then tune chunk size, concurrency and polling.
- Run `go vet`, `staticcheck` and `go test -race ./...`.
