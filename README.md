# fkbr

fkbr copies and syncs files between Linux hosts that share only an NFS
mount. The hosts never connect to each other: requests and data pass
through a directory on the share. It ships as a single static binary with
a dual-pane TUI and a scriptable CLI.

## Features

- **Nothing to install.** One static binary for x86-64 or ARM64. It runs
  on any Linux with kernel 3.10 or newer and does not depend on glibc. No
  package, service, system user or files in `/etc`.
- **Over ssh too.** Hosts without a common share, reached with ssh, are
  linked from one of them: `fkbr ssh:web01:/srv/www,db01`, or with F7 in
  the TUI (kept in the config file). Data goes straight from source to
  target (`docs/ssh.md`).
- **On its own.** `fkbr` with no share opens the TUI in the directory you
  are in and in `/`, which only you see.
- **No master.** Each host that runs `fkbr /mnt/nfs/xfer` joins as a node
  named after its hostname. Copy to one node, a list of nodes, or all of
  them (`@all`).
- **Fan-out.** Data is staged on the share once and read by every target.
  Slow or offline targets are detached and can catch up later.
- **Dual-pane TUI** in the style of Norton Commander. Either pane browses
  any node; copy, move, sync, view, create and delete work on any node,
  with live job progress.
- **Full mouse support.** Click to move, double-click to open, right-click
  to mark (or drag to mark several), and scroll with the wheel or drag
  the scrollbars. The function key bar and every dialog option are
  clickable. Hold Shift to select text, or start with `-mouse=false`.
- **Sync** in one direction, incremental or mirror, over millions of files
  with flat memory use. Every sync is planned as a dry run you confirm.
  Excludes, keep-newer and a delete limit protect the target.
- **Fast.** About as fast as tar through the same share, and faster when
  copying to several hosts (`docs/benchmarks.md`).
- **Crash-safe.** Atomic writes on the share, checksummed chunks and
  resumable jobs. Files are installed only after the whole transfer has
  arrived. Tested by killing, freezing and restarting nodes mid-transfer,
  cutting hosts off the NFS server, and deleting parts of the share under
  running nodes: over 10,000 copies checked, no file ever lost or
  corrupted (`docs/stability.md`).

## Screenshots

Five nodes on one share. Two files are marked on web01 (left); backup01
(right) holds an earlier copy of web01's site.

![Two panes: web01 and backup01, with the jobs panel below](docs/screenshots/fkbr-main.png)

**Copy (F5).** The destination defaults to the other pane, or to the
nodes marked with Insert in a node list (`@dest`). Type `n1,n2` for a
list, or press Ctrl+A for all nodes. Excludes are optional.

![The copy dialog](docs/screenshots/fkbr-copy.png)

**Sync (F4).** Makes the other side match this pane: the whole folder, or
only the marked folders. Every sync starts as a dry run that shows what
each target would copy, replace and delete; nothing changes until you
confirm. Here web01's site is mirrored to all four other nodes, and
backup01 would lose a page the site no longer has.

![The sync plan for a mirror to four nodes](docs/screenshots/fkbr-sync.png)

**Jobs (F9).** Either pane can show any node. The jobs panel lists running
and finished jobs.

![db01 and backup01, with the jobs panel focused](docs/screenshots/fkbr-jobs.png)

**Job details (Enter).** Progress, rate and ETA, and what each node is
doing, updated live.

![Details of a running copy of three database dumps to four nodes](docs/screenshots/fkbr-progress.png)

**View (F3).** Shows a file from any node, with XML and JSON highlighted.

![The viewer showing a build configuration on build01](docs/screenshots/fkbr-view.png)

**F1** lists every key.

## Quick start

Build with Go 1.26 or newer. `make` cross-compiles both architectures:

```
make    # dist/fkbr-linux-amd64 and dist/fkbr-linux-arm64
```

Copy the binary to each host. On each host, change to the directory to
share and start fkbr with a directory on the NFS mount:

```
cd /srv/data
fkbr /mnt/nfs/xfer
```

The host joins as a node, shares the current directory and everything
below it, and opens the TUI. Quitting the TUI leaves the swarm;
`fkbr /mnt/nfs/xfer daemon` keeps the host in it without a terminal.

Before first use, run `fkbr /mnt/nfs/xfer check` on each host. It checks
the mount options, atomic rename on the share, clock skew and node-name
uniqueness.

## Command line

Commands use the node running on the same host (TUI or daemon):

```
fkbr /mnt/nfs/xfer nodes                                    # nodes and their state
fkbr /mnt/nfs/xfer cp /reports @all:/incoming               # copy to every node
fkbr /mnt/nfs/xfer cp hostb:/logs/app.log /                 # fetch from a node
fkbr /mnt/nfs/xfer sync -mirror -n /site hostb,hostc:/site  # preview a mirror
fkbr /mnt/nfs/xfer jobs                                     # running and recent jobs
```

`docs/operations.md` covers every command and option (`-node`, `-ro`,
`-root id=/path`, `-roles`, ...). `fkbr version` prints the release and
the commit it was built from.

## Documentation

- `docs/FAQ.md`: short answers to common questions; start here
- `docs/operations.md`: running nodes, commands, TUI keys, sync,
  privileges, and what fkbr leaves on a host
- `docs/architecture.md`: how fkbr is built: layers, NFS and ssh, the
  packages
- `docs/protocol.md`: the on-share protocol (mailboxes, chunks, fan-out,
  sync)
- `docs/benchmarks.md`: measured performance
- `docs/stability.md`: how fkbr is broken on purpose, and what held
- `docs/testplan.md`: what could break, how it is tested, and the results
- `examples/config.yaml`: every option, for those who prefer a config file
  to flags
- `docs/ssh.md`: how nodes work over ssh
- `docs/local.md`: design for local file systems (not built yet)

## Development

```
make test                    # unit, integration and failure-injection tests
make test-race               # needs a kernel with a 48-bit VMA (not a Raspberry Pi 5)
make vet
scripts/bench.sh             # 1 source + N targets on one machine
scripts/screenshots/take.sh  # retake docs/screenshots (needs pyte and pillow)
```

The code is layered:

- **Frontends:** `internal/frontend/{cli,tui}`, using only `internal/api`.
- **API:** `internal/api`, with an in-process client (`api/local`) and a
  Unix-socket client (`api/remote`).
- **Engine:** `internal/engine`: jobs, fan-out and sync.
- **Subsystems:** `store` (the share), `transport/nfsmailbox` (the share
  on NFS), `sshlink` (nodes over ssh), `bundle`, `receive`, `inventory`
  and `localfs`.

## License

Copyright 2026 The Author. Licensed under the [Apache License 2.0](LICENSE).
