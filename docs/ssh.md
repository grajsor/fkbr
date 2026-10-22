# SSH (design)

Status: design, not built yet. See "Phases" at the end.

## Goal

Hosts that share no directory, but that you can reach with ssh, work as
nodes of one swarm. As with NFS, each host needs only the fkbr binary: no
service, no open port besides sshd, nothing installed. Logins, keys and
host checks are ssh's: fkbr adds no authentication of its own.

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

Host names go to `ssh` as typed, so `~/.ssh/config`, the agent,
`ProxyJump` and `known_hosts` all apply. `-ssh-bin PATH` names fkbr on the
other hosts (default: `fkbr` on their `PATH`). CLI commands attach to this
host's node, as today.

The host that runs the `ssh:` command is the **hub** below.

## How it works

The protocol stays as it is (`protocol.md`). Only the share moves.

- **The share is a directory on the hub**, private to it:
  `/tmp/fkbr-<uid>-<node>/hub` (or `-hub DIR`). The hub's own node uses it
  as a local share.
- **One ssh session per host.** The hub runs the user's `ssh HOST`, which
  starts `fkbr` there in a hidden stdio mode, in the given directory. Over
  that session's stdin and stdout the hub serves the hub directory with
  SFTP (`github.com/pkg/sftp`, server side, confined to that directory).
- **The remote node is an ordinary node** whose share is the hub
  directory reached over SFTP on its stdin and stdout. It has the same
  registry entry, inbox, heartbeat, chunks, acks and job records as on
  NFS. Atomic publication uses SFTP's `posix-rename` and `fsync`.
- **The mailbox gets a small file system interface** (`vfs`): the local
  one (as today, with the NFS refresh trick when the share is on NFS) and
  SFTP. The mailbox code is the same for both.
- **One clock.** Heartbeats and pruning use mtimes on the hub's file
  system, as they use the NFS server's today.
- **Data passes through the hub.** A copy from web01 to db01 is staged on
  the hub, like it is on the NAS with NFS. The hub's disk holds the
  `job_quota` window (1 GiB per job).

### Lifetime and failures

- A remote node lives as long as its ssh session. When the hub quits,
  ssh closes; the remote node stops, removes its staging and leaves, like
  a TUI on SIGHUP today.
- A dropped link (network, ssh killed) looks like a cut from the NAS.
  The remote node cannot dial back, so it stops at once; on the hub its
  jobs detach it after `offline_after`, as today. The hub starts the
  session again, with backoff, and `catchup` re-sends.
- A remote fkbr on another protocol version refuses with the usual
  message. Mixed builds of one protocol work together, as with NFS.

### Trust

Remote nodes read and write only the hub directory: the SFTP server is
confined to it. Within the swarm the rules are those of NFS: one trust
domain, records from the share validated (`ValidID`, size caps, blob
names). The SFTP backend must keep those checks; they live in the mailbox,
so it does.

### Differences from NFS

- The hub is needed: without it there is no swarm. This fits "a running
  TUI does all the work".
- Throughput is bound by the hub's links; a copy between two remote nodes
  crosses them twice.
- Every poll is a few SFTP round trips; the poll interval may need to be
  longer on slow links.
- No NFS attribute caching: the refresh trick is off for SFTP.

## Packages

| Package | |
|---|---|
| `internal/transport/vfs` | the file system interface and its local implementation |
| `internal/transport/nfsmailbox` | unchanged protocol, on `vfs` (the name stays; NFS tricks only on the local file system) |
| `internal/transport/sftpfs` | `vfs` over an SFTP client |
| `internal/sshlink` | ssh sessions: start, serve SFTP to them, restart with backoff, state for the TUI |
| `cmd/fkbr` | `ssh:` shares, the hidden stdio mode |
| `internal/check` | ssh checks: host reachable, fkbr found there, its version, SFTP works |

## Testing

In `go test`, with no ssh:

- A `vfs` conformance test for both implementations; SFTP over an
  in-process pipe (pkg/sftp server and client).
- The mailbox tests and the engine tests with every node's share over an
  SFTP pipe: the whole suite, on the new backend.
- `sshlink` with a fake `ssh` command that runs fkbr locally, so a node
  can be killed, its link cut and the hub stopped mid-transfer.

On real hosts (lab): real ssh and latency; the link dropped mid-transfer
in both directions; the hub killed and restarted; a big transfer through
the hub; x86-64 and ARM64 nodes in one swarm.

## Phases

1. `vfs`, and the mailbox on it. No change in behaviour; the whole suite
   stays green.
2. `sftpfs`, and the suite over SFTP pipes.
3. `sshlink`, `ssh:` shares, the stdio mode.
4. `check`, link state in the TUI and `nodes`, user docs (FAQ,
   operations.md), test plan rows.

## Open questions

- Copies between two remote nodes could go straight from one to the
  other over ssh, skipping the hub. Only if the hub's links turn out to
  be the limit.
- The hub could copy its own binary to a host that has none (into the
  state dir there, same architecture only). Off unless asked for.
- An NFS swarm and an ssh swarm in one TUI. Not planned: one share per
  node.
