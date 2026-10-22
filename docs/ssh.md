# SSH (design)

Status: built and tested without real ssh (`testplan.md`, section 10);
real hosts, latency and cut links still to be tried on the lab. User
docs: `operations.md`, "Over ssh".

## Goal

Hosts that you can reach with ssh work as nodes of one swarm, with no
directory in common. Data goes straight from source to target over ssh,
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

This host joins as a node and shares the current directory. It also
starts fkbr on web01 and db01 over ssh, sharing `/srv/www` and
`/var/backups` there, and opens the TUI with all three nodes. Every node
can be a source or a target: copy, move, sync, view and delete work
between any two. Without `:/path`, a host shares its login directory.
`-ssh-bin PATH` names fkbr on the other hosts (default: `fkbr` on their
`PATH`). CLI commands attach to this host's node.

The host that runs the `ssh:` command is the **home** node below; the
others are **remote** nodes.

## Sessions

The home node runs `ssh -T -o BatchMode=yes -o Compression=no HOST` once
per host, which starts `fkbr link` there in the given directory. All
traffic with that node runs over this one session's stdin and stdout.
`BatchMode` keeps ssh from asking for a password over the TUI: keys or
the agent must do. ssh compression is off: chunks are already zstd.

### The link protocol (`internal/sshlink/frame`)

Each side first writes `fkbr-link 1\n`; what the home node reads before
it (a login banner from a shell's rc files) is logged and skipped, and
"command not found" and the like come back as the error. Then frames:
type (1 byte), stream ID (4), length (4), payload (at most 64 KiB).

- **Streams.** Many at once, opened by either side, each with its own
  flow control: a side may send 4 MiB on a stream before the reader gives
  window back. A big transfer never holds up a directory listing.
- **Keepalive.** An idle session pings every 15 s; one that hears nothing
  for 45 s ends.
- **Calls.** Each call is one stream: a JSON line naming the method and
  its arguments, an optional raw body, then a JSON line with the error
  (message and code, so the engine can still tell "not found" from "not
  allowed") or the result, and an optional raw body.
- The reader is strict (frame sizes, window, stream IDs) and fuzzed.

## The nodes' ends (`internal/sshlink`)

The engine talks only to the `transport.Transport` and `transport.Store`
interfaces; sshlink implements them for both kinds of node.

### Records: on the home node

The home node keeps the swarm's records in a private directory
(`/tmp/fkbr-<uid>-<node>/ssh`, mode 0700) with the share store
(`internal/store`): registry, inboxes, job records, statuses, acks,
detaches, cancels, sync plans and snapshots. For each remote node it
opens the store as that node and answers that node's calls with it, so
every record keeps its owner checks (only a node writes its own status
and acks).

- A record write returns once the home node has committed it (atomic
  rename). Record calls finish even if the caller is stopping, as a
  stopping worker writes its last status and leaves.
- **Nothing polls over the network.** A remote node's "anything for me?"
  waits on the home node for up to a second and answers as soon as a
  request is there; waiting for replies is one call that streams each
  reply part as it arrives.
- **One clock.** IDs, job times and liveness use the home node's clock.

### Data: straight from source to target

- A source keeps the chunks it makes (FKB2, zstd, SHA-256, as everywhere)
  in a private directory on its own disk until every active target has
  acked them; `job_quota` bounds them per job.
- A target reads each chunk from its source over the links and checks it
  with the strict chunk reader, as it would any chunk. Between the home
  node and a remote node the path is their session; between two remote
  nodes the home node relays the stream in memory, never writing it.
- A slow target holds its source's window; after `detach_after` without
  progress it is detached and the others go on. `catchup` re-sends.
- A chunk goes once per target, each over its own path.

### Failures and resume

- A dropped link reads as an I/O error, which transfers ride out for up
  to 3 minutes while the home node starts the session again (with
  backoff). A node whose link stays down goes offline after
  `offline_after` and is detached, as any offline node.
- A restarted node asks its source for the first chunk it has not acked;
  the source builds chunks the same way every time and sends from there.
  Nothing partial is ever installed.
- When the home node quits, it asks every remote node to stop: each
  leaves the swarm while its link is still up, then ssh closes.
- A request handed to a remote node stays on the home node until that
  node's next "anything for me?" says the answer arrived; if it did not,
  the request goes out again (alone, and after 20 tries it is parked in
  failed/). One answer carries at most 16 MiB of requests.
- A remote fkbr on another protocol version is refused with the usual
  message.

### Trust

ssh decides who reaches which host. A remote node reaches only the home
node's calls; those keep the store's checks (IDs, names, size caps, owner
checks), and chunk calls check their names too. The swarm is one trust
domain: any node can read and write what the others share.

## Packages

| Package | |
|---|---|
| `internal/sshlink/frame` | the link protocol: streams, flow control, keepalive |
| `internal/sshlink` | the home and remote ends: `Transport` and `Store` over a link |
| `internal/store` | the share store, here on the home node's private directory |
| `cmd/fkbr` | `ssh:` shares, `fkbr link` |
| `internal/check` | ssh checks: host reachable, fkbr found there, its version |

## Testing

In `go test`, with no ssh:

- `frame`: big and many streams, a stalled stream next to a busy one,
  banners, resets, a silent peer, bad frames, and a fuzzer.
- `FKBR_TEST_TRANSPORT=ssh go test ./internal/engine` runs the whole
  engine suite with every node but the first linked to it over an
  in-process pipe: copies, fan-out, sync, detach, cancel, restarts.
  Tests that inject faults into a share directory's files are skipped
  there.
- Sessions with a fake `ssh` command that runs fkbr locally, so a node
  can be killed, its link cut and the home node stopped mid-transfer.

On real hosts (lab): real ssh and latency (local network, and 20 to 50 ms
round trips); a `ProxyJump` path; links dropped mid-transfer in both
directions; the home node killed and restarted; a big transfer between
two remote nodes; x86-64 and ARM64 in one swarm; throughput against
`scp` and `rsync -e ssh` for `benchmarks.md`.

## Open questions

- Direct sessions between remote nodes (instead of relaying through the
  home node), where one can ssh to the other.
- The home node could copy its own binary to a host that has none (into
  the state dir there, same architecture only). Off unless asked for.
- A TUI started on a remote host, attached to the node that runs there.
