# fkbr

fkbr is a file commander and sync tool for hosts that share nothing but an
NFS mount. Nodes never connect to each other. They exchange requests and
data through a directory on the shared mount.

- One static binary (x86-64 or ARM64) for any Linux with kernel 3.10 or
  newer; it needs no glibc. Nothing is installed: no package, service,
  system user or config in `/etc`.
- A dual-pane TUI, Norton Commander style, where either pane can browse any
  node. Copy, move, mkdir, delete and view work on any node.
- A swarm, not a hierarchy: start `fkbr /mnt/nfs/dir` on each host and it
  joins. Copies go to one node, a list, or every node (`@all`). Data is
  staged once and every target reads it. Slow or offline targets are
  detached and can catch up later.
- Sync of millions of files: streaming scans with flat memory, incremental
  or mirror mode, dry-run plans, and a delete limit.
- Crash-safe: atomic publication on the share, chunk checksums, and
  resumable jobs. Transfers are staged and installed only when complete.

## What it looks like

Five nodes on one share. On the left, two files on web01 are selected; on
the right is backup01, where an earlier copy of web01's site arrived:

![Two panes: web01 and backup01, with the jobs panel below](docs/screenshots/fkbr-main.png)

F5 copies the selection. The destination defaults to the other pane, and
you can type `n1,n2` or press Ctrl+A for every node:

![The copy dialog](docs/screenshots/fkbr-copy.png)

Any pane can show any node. F9 moves to the jobs panel:

![db01 and backup01, with the jobs panel focused](docs/screenshots/fkbr-jobs.png)

Enter on a job shows its details while it runs: progress, rate and ETA,
and what each node is doing:

![Details of a running copy of three database dumps to four nodes](docs/screenshots/fkbr-progress.png)

F3 shows a file from any node, with XML and JSON highlighted:

![The viewer showing a Jenkins job config.xml from build01](docs/screenshots/fkbr-view.png)

## Quick start

New here? Read `docs/FAQ.md`. It takes a minute.

Build it on any machine with Go ≥ 1.25 (it cross-compiles):

```
make               # dist/fkbr-linux-amd64 and dist/fkbr-linux-arm64, static
```

Copy the binary to each host. On each host, `cd` to the directory you want
to share and point fkbr at a directory on the NFS mount:

```
cd /srv/data
fkbr /mnt/nfs/xfer
```

That's all. The host registers itself in `/mnt/nfs/xfer` as a node named
after its hostname, shares the current directory with everything below it,
and opens the TUI. Every host that does the same joins the swarm. There
are no masters or slaves: copy to one node, a list (`hostb,hostc`), or
every other node (`@all`; Ctrl+A in the TUI's copy dialog). Quitting the
TUI leaves the swarm again; `fkbr /mnt/nfs/xfer daemon` keeps a host in it.

From another shell on the same host, commands go through the running
node:

```
fkbr /mnt/nfs/xfer nodes
fkbr /mnt/nfs/xfer cp /reports @all:/incoming
fkbr /mnt/nfs/xfer cp hostb:/logs/app.log /
fkbr /mnt/nfs/xfer sync -mirror -n /site hostb,hostc:/site
```

Run `fkbr /mnt/nfs/xfer check` on each host first. It checks the NFS mount
options, atomic rename on the share, clock skew and node-name uniqueness.
Options such as `-node`, `-ro`, `-root id=/path` and `-roles` are covered
in `docs/operations.md`.

## Documentation

- `docs/FAQ.md`: the shortest way to get going
- `docs/operations.md`: running nodes, commands, TUI keys, sync, privileges,
  and what fkbr leaves on a host
- `docs/protocol.md`: the on-share protocol (mailboxes, chunks, fan-out,
  sync)
- `docs/benchmarks.md`: measured performance
- `examples/config.yaml`: every option, if you prefer a config file to flags

## Development

```
make test         # unit, integration and failure-injection tests
make test-race    # needs a 48-bit VMA kernel (not a Raspberry Pi 5)
make vet
scripts/bench.sh  # 1 master + N slaves on one machine
```

The layers:
- **Frontends:** `internal/frontend/{cli,tui}`, which only use
  `internal/api`.
- **API:** `internal/api`, with an in-process client (`api/local`) and a
  Unix-socket client (`api/remote`).
- **Engine:** `internal/engine`, which handles jobs, fan-out and sync.
- **Subsystems:** `transport/nfsmailbox`, `bundle`, `receive`, `inventory`
  and `localfs`.
