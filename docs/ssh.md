# SSH (design)

Status: design, not built yet. See "Phases" at the end.

## Goal

Hosts that share no directory, but that you can reach with ssh, work as
nodes of one swarm. Data goes straight from source to target over ssh,
never resting anywhere on the way. fkbr runs the user's own `ssh`, so
whatever ssh reaches (`ProxyJump`, bastions, `~/.ssh/config`, the agent,
`known_hosts`), fkbr reaches. Each host needs only the fkbr binary: no
service, no open port besides sshd, nothing installed, and no
authentication of fkbr's own.

## Use

```
cd /srv/data
fkbr ssh:web01:/srv/www,db01:/var/backups
```

This host joins as a node and shares the current directory, as it does
today. It also starts fkbr on web01 and db01 over ssh, sharing `/srv/www`
and `/var/backups` there, and opens the TUI with all three nodes. Every
node can be a source or a target: copy, move, sync, view and delete work
as they do with NFS. Without `:/path`, a host shares its login directory.
`-ssh-bin PATH` names fkbr on the other hosts (default: `fkbr` on their
`PATH`). CLI commands attach to this host's node, as today.

The host that runs the `ssh:` command is the **home** node below.

## What stays, what changes

The engine stays as it is: jobs, fan-out, detach, catch-up, sync plans,
excludes, moves, resume. It talks to the share only through the
`transport.Transport` and `transport.Store` interfaces; over ssh they are
implemented by `internal/sshlink` instead of `nfsmailbox`.

| | NFS | ssh |
|---|---|---|
| Chunks | files on the share | **the same chunks** (FKB2, zstd, SHA-256, the strict reader), streamed from source to target |
| Chunk metadata | `.meta` files | the same metadata, sent ahead of each chunk |
| Requests, replies, acks, status, cancel | files, polled every 200 ms to 1 s | messages, sent at once |
| Liveness | heartbeat file mtimes | the ssh session, plus a heartbeat message |
| Job records, registry, sync plans and snapshots | files on the share | files in a private directory on the home node |
| Clock | the NFS server's | the home node's |

ssh compression stays off (`-o Compression=no`): chunks are already zstd.

## How it works

### Sessions

The home node runs `ssh HOST` once per host, starting fkbr there in a
hidden stdio mode in the given directory. All traffic with that node
runs over this one session's stdin and stdout, as frames of a small
multiplexed protocol: many requests and chunk streams at once, each with
its own flow control, so one big transfer never holds up a directory
listing.

### Control state

The home node keeps the swarm's records in a private directory
(`/tmp/fkbr-<uid>-<node>/ssh`), with the existing mailbox code on local
disk: job records, statuses, acks, detaches, cancel, registry, sync plans
and snapshots. Remote nodes read and write them with messages; a write
returns once the home node has committed it (atomic rename, as today).
The home node pushes changes (a new request, a reply, a status) to the
nodes that wait on them, so nothing polls.

### Data: straight from source to target

- `CreateChunk` on the source sends the chunk to every active target of
  the job, each over its own path. A chunk to a node is written straight
  into that node's staging area (`.fkbr-tmp` in its destination root),
  where `OpenChunk` reads it. Nothing is stored in between.
- **Paths.** Between the home node and a remote node, the path is their
  ssh session. Between two remote nodes, the home node forwards the
  frames from one session to the other in memory, as a jump host does,
  never writing them to disk. A later option lets a remote node run its
  own `ssh` to the other one, where that path exists.
- **The window.** `StagedBytes` counts the bytes sent to a target and
  not yet acked; `job_quota` bounds it, as it bounds staging on the
  share. A slow target fills its window: the source waits, and after
  `detach_after` without progress it detaches that target, as today. The
  others go on.
- **Fan-out** sends a chunk once per target (NFS stages it once for all).
  This is the price of point-to-point, and it is what lets every target
  get its own path.
- **Acks** go back to the source and to the home node's records. The
  source drops a chunk from its window when every active target acked it
  (`DeleteChunk`).

### Failures and resume

- A target that restarts, or whose session comes back, asks for its
  first chunk not acked yet. The source builds chunks the same way every
  time (chunk sizes depend only on the sequence number), so it
  regenerates from there; a source whose files changed meanwhile is
  detected by `First`, as today. Nothing partial is ever installed.
- A dropped session looks like a node going offline: its jobs detach it
  after `offline_after`, as today. The home node starts the session
  again, with backoff; `catchup` re-sends what was detached.
- A remote node lives as long as its session. When the home node quits,
  ssh closes; remote nodes stop, remove their staging and leave, like a
  TUI on SIGHUP today.
- A remote fkbr on another protocol version refuses with the usual
  message. Mixed builds of one protocol work together, as with NFS.

### Trust

ssh decides who reaches which host. Within the swarm the rules are those
of NFS: one trust domain; what a node receives from others is validated
as it is from the share (`ValidID`, size caps, blob names, the strict
chunk reader with its checksum). The frame protocol is fuzzed like the
chunk reader.

## Packages

| Package | |
|---|---|
| `internal/sshlink/frame` | the multiplexed frame protocol over one stream: request/reply, push, chunk streams with flow control |
| `internal/sshlink` | `Transport` and `Store` over a session: the home side (records on local disk, routing between sessions) and the remote side |
| `internal/sshlink/session` | starting `ssh`, restarts with backoff, state for the TUI and `nodes` |
| `cmd/fkbr` | `ssh:` shares, the hidden stdio mode |
| `internal/check` | ssh checks: host reachable, fkbr found there, its version |

`nfsmailbox` does not change.

## Testing

In `go test`, with no ssh:

- The frame protocol: unit tests, and a fuzzer on the reader.
- The engine suite with nodes linked by in-process pipes instead of a
  share: copies, fan-out, sync, detach, cancel, resume. Pipes that stall,
  drop or cut mid-chunk stand in for bad links.
- Sessions with a fake `ssh` command that runs fkbr locally, so a node
  can be killed, its link cut and the home node stopped mid-transfer.

On real hosts (lab): real ssh and latency (local network, and a link
with 20 to 50 ms round trips); a `ProxyJump` path; sessions dropped
mid-transfer in both directions; the home node killed and restarted; a
big transfer between two remote nodes; x86-64 and ARM64 in one swarm;
throughput against `scp`, `rsync -e ssh` and NFS for `benchmarks.md`.

## Phases

1. `frame`: the protocol, its tests and fuzzer.
2. `sshlink` over in-process pipes, and the engine suite on it.
3. `session`, `ssh:` shares, the stdio mode.
4. `check`, link state in the TUI and `nodes`, user docs (FAQ,
   operations.md), test plan rows.

## Open questions

- Direct sessions between remote nodes (instead of forwarding through
  the home node), where one can ssh to the other.
- The home node could copy its own binary to a host that has none (into
  the state dir there, same architecture only). Off unless asked for.
- An NFS swarm and an ssh swarm in one TUI. Not planned: one share per
  node.
