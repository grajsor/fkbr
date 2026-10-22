# Local file systems (design)

Status: local mode is built in part (section 2); the rest is design. See "Phases" at the end.

Two separate things:

1. **A share on any file system**, not only NFS: a local folder, an SMB
   mount, a VM's shared folder.
2. **Local mode**: fkbr on one host, without a share, as a two-pane
   commander and sync tool between that host's folders and drives.

## 1. A share on any file system

This already works: the share is a directory, and the share store
(`internal/store`) only needs atomic rename-over, `fsync` and directory
listings. Tests run on temp
dirs. What changes is that it becomes a supported setup, not a test
setup:

- `check` reports the file system the share is on and checks what the
  protocol needs (atomic rename-over, mtimes visible to all nodes, names).
  For a share on local disk it says "local share: nodes on this host
  only" instead of warning that it is not NFS.
- The NFS refresh trick stays `auto`: on only when the mount options
  call for it.
- Supported, once tested on the lab: local disk (several nodes on one
  host, with `-node`), SMB/CIFS, virtiofs and 9p (VM shared folders). A
  file system that cannot rename atomically over an existing file is
  refused by `check`, as today.
- Not supported: a USB disk carried from host to host. Jobs need their
  nodes online at the same time.

## 2. Local mode

Built: `fkbr` with no share opens the TUI on this host alone, as an ssh
swarm's home node with no hosts yet (`ssh.md`; user docs in
`operations.md`, "Without a share"). The panes open in the directory it
was started in and in root `local` (`/` by default), which only this
host's TUI and CLI see: it is not published, other nodes cannot reach it,
and a job touching it runs only if this node submitted it, checked
against the request kept in the private state dir
(`engine/localroot.go`). Its share is private
(`/tmp/fkbr-<uid>-<node>-ssh/ssh`, 0700).

Not built yet:

- **Drives** (`-drives`): each mounted drive as a root (`/home`,
  `/media/...`, `/mnt/...`: real file systems from
  `/proc/self/mountinfo`; no `proc`, `sysfs`, `tmpfs` or bind mounts
  twice), named after its mount point. Today `/` covers them; `-root
  id=/path` adds roots.
- **Same-node copies skip the chunks.** A copy from a node to itself is
  staged as chunks, then read back: two writes of every byte. The engine
  could copy files straight into the receiver's staging area
  (`.fkbr-tmp` in the destination root), so install stays atomic and
  crash-safe. The same path would help a node that copies to itself on
  NFS.
- **Syncs on the local root**: their path lists and snapshots would have
  to be kept where no one else can change them.

## Packages

| Package | |
|---|---|
| `internal/engine` | built: local-only roots (`localroot.go`); to do: the same-node fast path (`samenode.go`), for local mode and NFS alike |
| `internal/check` | to do: file system checks for any share |
| `cmd/fkbr` | built: no share, `-local-root`; to do: `-drives` |

The share in local mode is the share store (`internal/store`) on a
private directory, as on an ssh swarm's home node.

## Testing

In `go test`: drive discovery from sample mountinfo files (bind mounts,
removable drives, odd mount point names); local-mode engine tests copying
and syncing between roots; the fast path with kill and resume like the
existing transfer tests, and with the same checks (whole files only,
nothing left in staging, metadata as documented).

On real hosts: a USB stick (FAT, exFAT) as a root; SMB and virtiofs as
shares; a big copy between two disks against `cp -a` and rsync for
`benchmarks.md`.

## Phases

1. `check` for shares on any file system, docs.
2. Done: no share, a private share, root `local`. To do: `-drives`.
3. The same-node fast path.
4. TUI touches: drive names in the pane header, a drive list.

## Open questions

- Should local mode pick up drives plugged in while it runs? Proposed:
  yes, by re-reading mountinfo when the node list is shown.
- A pane on another node's drives in a swarm (roots added by `-drives`
  on that node). Works by design, as roots; nothing extra.
