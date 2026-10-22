# Local file systems (design)

Status: design, not built yet. See "Phases" at the end.

Two separate things:

1. **A share on any file system**, not only NFS: a local folder, an SMB
   mount, a VM's shared folder.
2. **Local mode**: fkbr on one host, without a share, as a two-pane
   commander and sync tool between that host's folders and drives.

## 1. A share on any file system

This already works: the share is a directory, and the mailbox only needs
atomic rename-over, `fsync` and directory listings. Tests run on temp
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

```
fkbr -local
```

The host runs a node of its own, with no share and no other nodes. The
TUI's panes browse this host's roots: the current directory (`files`),
and with `-drives` each mounted drive (`/`, `/home`, `/media/...`,
`/mnt/...`: real file systems from `/proc/self/mountinfo`; no `proc`,
`sysfs`, `tmpfs` or bind mounts twice), named after its mount point
(`media-usb`). `-root id=/path` adds roots, as today. Copy, move, sync,
view, create and delete work between any two of them, with the same
plans, excludes, `max_deletes`, resumable jobs and checks.

- **The share is private:** `/tmp/fkbr-<uid>-<node>/local`, not
  readable by other users. Nothing is published there for anyone else.
- **Same-node copies skip the chunks.** Today a copy from a node to
  itself is staged as chunks on the share, then read back: two writes of
  every byte. In local mode the engine copies files straight into the
  receiver's staging area (`.fkbr-tmp` in the destination root), so
  install stays atomic and crash-safe: nothing partial is ever installed.
  The same path helps a node that copies to itself on NFS.
- Read-only roots (`-ro`, `id=/path:ro`) stay read-only. FAT, exFAT and
  case-folding targets keep their name rules (`protocol.md`, "Sync").
- `fkbr` with no share and no config keeps printing the usage. Local
  mode is asked for with `-local`, so a forgotten share argument never
  starts a node that shares nothing.

## Packages

| Package | |
|---|---|
| `internal/local` | local mode: the private share, drive discovery from mountinfo, root names |
| `internal/engine` | the same-node fast path (`samenode.go`), for local mode and NFS alike |
| `internal/check` | file system checks for any share; local-mode checks |
| `cmd/fkbr` | `-local`, `-drives` |

The share in local mode is the mailbox on the local file system (`vfs`,
see `ssh.md`), so the protocol code is the one used everywhere.

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
2. `-local` with a private share and roots; drive discovery (`-drives`).
3. The same-node fast path.
4. TUI touches: drive names in the pane header, a drive list, user docs.

## Open questions

- Should local mode pick up drives plugged in while it runs? Proposed:
  yes, by re-reading mountinfo when the node list is shown.
- A pane on another node's drives in a swarm (roots added by `-drives`
  on that node). Works by design, as roots; nothing extra.
