# Operating fkbr

fkbr installs nothing. There is no package, install script, service or
system user. You copy one static binary to each host and run it:
`fkbr-linux-amd64` on x86-64, `fkbr-linux-arm64` on ARM. The same file
runs on any Linux with kernel 3.10 or newer, since it needs no glibc.
Tested on CentOS 7, Rocky 8, 9 and 10, and Debian 12.

## What fkbr leaves on a host

| Where | What | Lifetime |
|---|---|---|
| wherever you copied it | the binary (any name) and optionally `fkbr.yaml` beside it | yours |
| `/tmp/fkbr-<uid>-<node>/` | worker lock and API socket; `node.log` when the TUI runs the node | lock and socket removed on clean exit; the log stays (at most 2 × 1 MiB) |
| `<root>/.fkbr-tmp/` in a destination root | staging for a transfer in progress | removed when the transfer ends |
| the share directory you pass | node registry; finished job records, sync plans and failed requests | records pruned after `keep_done` (1h), see below |

Nothing is written to `/etc`, `/var` or your home directory, and fkbr never
reads configuration from them.

**Permissions on the share.** Hosts usually run fkbr as different users,
and every node creates and deletes entries in the others' directories. So
fkbr makes its directories on the share `rwxrwxrwx` and its files
`rw-r--r--`, whatever your umask. Anyone who can reach the share directory
can therefore read and change what fkbr keeps there, so put it where only
your hosts and users can get to it (the NFS export, or a parent directory
with tighter permissions). Shares made by versions before this used
`rwxrwx---`; each node opens up what it created when it starts, and
`fkbr SHARE check` lists directories that are still closed and who owns
them.

The runtime dir is deliberately not `/run/user/<uid>`: logind deletes that
when your last session logs out, while a node may keep running in `tmux`
or under `nohup`. In `/tmp`, the node touches its lock and socket every 10
minutes, so `systemd-tmpfiles` (which ages out `/tmp` after 10 days on
many distributions) leaves them alone. If they are removed anyway, it recreates them
within 30 s. If another process has meanwhile taken the lock, the worker
stops rather than run twice.

### Cleanup of the share

There is no cron job or service. Cleaning up is done by fkbr itself, and
only while it runs:

- Requests are deleted as soon as they have been handled. Staged data is
  deleted as soon as every target has it.
- A running node (TUI or daemon) removes finished job records, sync plans
  and snapshots, and failed requests once they are older than `keep_done`
  (default 1h; `-keep` or `keep_done:` changes it). It does this when it
  starts and then every half `keep_done`.
- If no node runs, nothing is removed until one starts again. Use
  `fkbr SHARE prune` to clean up right away; `prune -keep 0` removes
  everything finished. Running jobs are never touched.
- Quitting a TUI that ran the node itself removes the machine from the
  share, unless an unfinished job still needs it or a daemon had
  registered the machine before (say, the daemon crashed and you started
  the TUI meanwhile). A daemon stays registered when stopped, so `@all`
  jobs record that it missed them and `catchup` can re-send later.
- Machines offline for more than `forget_after` (default 7 days;
  `forget_after: -1` turns it off) are removed by a running node. `forget`
  removes one right away.
- If someone deletes parts of the share while nodes run (`tmp/`, a node's
  entry or inbox, `jobs/`, `data/`), the running nodes recreate what they
  need within seconds and register again. Entries in `nodes/` that cannot
  be read are ignored; delete such a directory by hand.

## Starting a node

You need a directory on the shared NFS mount that every host can write
to, for example `mkdir /mnt/nfs/xfer`. Then, on each host, start fkbr in
the directory you want to share:

```
cd /srv/data
fkbr /mnt/nfs/xfer
```

- **Name:** the node is named after the short hostname. Use `-node NAME`
  when two hosts share a hostname, or to run several nodes on one host.
  fkbr refuses to start if another host is online under the same name, or
  if another fkbr process on this host runs that node already (for
  example the same name started by another user, or with another
  `-state`).
- **Root:** the node shares the current directory and everything below it,
  writable. `-ro` makes it read-only. `-root id=/path[:ro]` (repeatable)
  shares explicit directories instead. fkbr refuses to share `/` by
  default, or a directory inside the share (symlinks are followed for
  this check). If the share is inside the shared directory (`cd ~ &&
  fkbr ~/box`), the node leaves it out: peers do not see it, and nothing
  is copied into or deleted from it.
- **Peers:** paths look like `hostb:/sub/dir`. With several roots on a
  node, name the root: `hostb:logs/app.log`.
- **Groups:** `@all` targets every other node. In the TUI, `@dest` is the
  nodes you marked in the node list (Insert); it lasts until you quit.
  `-roles a,b` adds tags for
  your own groups (`@a`) and for `accept_writes_from`. Neither is needed
  otherwise; every node is equal.
- **Flags:** they can go anywhere: before or after the share, or after
  the command (`fkbr /mnt/nfs/xfer daemon -ro`). `--` ends them. The share
  can also come from `-share` or `$FKBR_SHARE`.
- **Runtime dir:** `-state DIR` overrides where the lock and socket go.

**The TUI is the node.** While a TUI runs, it does all of this node's work:
- heartbeats
- answers browse requests from peers
- executes its side of transfers
- serves the local socket, so CLI commands from another shell work

If you quit while this node takes part in running jobs, the TUI asks
first. Those jobs pause and resume from the share when fkbr runs here
again; nothing is lost. Quitting also takes the machine off the share,
unless such a job needs it, so `@all` stops counting it (see Cleanup). Use
a daemon for a machine that should stay a member while nobody is logged
in.

Every 30 s the name in the top left slides out and back in, and a small
yellow spinner turns in the bottom-right corner of the jobs panel (of the
right pane while there are no jobs), so you can tell at a glance that the
TUI is alive. `q` quits, like F10.

The mouse works too: a click moves the cursor, a double click opens, a
right click marks, the wheel scrolls, and a click on the key bar presses
that key. Hold Shift to select text, or start with `-mouse=false`.

The TUI writes this node's log to `/tmp/fkbr-<uid>-<node>/node.log` (F1
shows the path; `-v` adds debug lines). It stays after you quit, so a
failed copy can be looked into later.

**Headless:** `fkbr /mnt/nfs/xfer daemon` does the same without a UI, in
the foreground, and logs to stderr. To keep it running after you log out, use `nohup` or a
`tmux` or `screen` session; no service is needed. A TUI started while a
daemon runs attaches to that daemon instead of starting a second worker.
If that daemon shares other directories than you would from here (another
working directory, `-ro`, `-root`), you get a note saying what it shares.
If it runs for another share, fkbr stops and asks for another `-node`
name. Closing the terminal (an ssh session that drops) stops a TUI's node
cleanly, like quitting.

**Config file (optional).** The same settings can live in a YAML file:
`-config FILE`, `$FKBR_CONFIG`, or `fkbr.yaml` next to the binary. See
`examples/config.yaml` for every option, including tuning. Arguments and
flags override the file.

Check a host before relying on it:

```
cd /srv/data && fkbr /mnt/nfs/xfer check
```

This validates:
- the shared directory
- that the runtime dir is on local disk
- the share's filesystem type and NFS mount options
- atomic rename on the share
- the protocol version
- clock skew against the NFS server
- node-name collisions with other hosts
- privileges
- whether a running node is reachable

### NFS mount options

The Linux NFS client caches directory attributes for 30 s by default
(`acdirmin`), which delays how quickly nodes notice each other's requests.
Mount the share on every host with short caching:

```
nfs-server:/export/transfer  /mnt/transfer  nfs  rw,hard,actimeo=1,lookupcache=positive  0 0
```

You often can't choose the options, and fkbr copes without them. With
long caching it refreshes directories before reading them and waits for
files by listing; `check` then reports the caching as information only.

`soft` mounts are the one thing to raise with whoever owns the mount:
while the NFS server is unreachable, calls fail with I/O errors instead of
waiting, and other programs on the mount can lose writes. fkbr transfers
ride this out: on `EIO`, `ETIMEDOUT` or `ESTALE` the source or target
waits and retries, for up to 3 minutes of consecutive errors, and the log
shows when it started and recovered. `check` warns about `soft`.

## Commands

All of these attach to the running node (TUI or daemon) through its socket.
With no node running they work in-process, which can browse and submit but
not execute jobs: jobs run where a node's worker runs. `-local` forces
in-process mode.

| Command | |
|---|---|
| `nodes` | registered nodes, online state, roots, chown level, write policy |
| `forget NODE...` | remove offline nodes from the share (they re-register if started again) |
| `ls [node:]/path` | list a directory on any node |
| `cp [node:]/path... node\|n1,n2\|@all:/dir` | copy; `-conflict`, `-z`, `-detach` |
| `sync [-mirror] [-n] SRC DST` | make DST match SRC (see below) |
| `jobs`, `job [-f] ID`, `cancel ID`, `catchup ID` | follow and control jobs |
| `events` | live change stream as JSON lines |
| `check` | validate the host (see above) |
| `version` | the release and the commit it was built from, e.g. `fkbr v1.0 f0c0e0b`; `-dirty` if it had uncommitted changes. A zip or tar.gz download of the repo carries its commit too; only a copy without `.git` shows just `v1.0` (`make COMMIT=abc1234` sets it by hand) |

A command's own flags may come before or after its paths
(`sync /a hostb:/a -mirror` works); after `--` everything is a path.

The API is HTTP+JSON on the socket (see `internal/api/remote/server.go`),
for example `curl --unix-socket /tmp/fkbr-$(id -u)-$(hostname -s)/fkbr.sock http://fkbr/v1/jobs`.

## Privileges

fkbr runs as whatever user starts it. What it can preserve on received
data depends on that user:

| Running as | Effect on received data |
|---|---|
| ordinary user | content, modes and mtimes; everything owned by that user; setuid/setgid kept only with `allow_setid` |
| root | also UID/GID of files and directories, and setuid/setgid with `allow_setid` |

Without root, the binary itself can carry Linux file capabilities
(`setcap cap_chown,cap_fowner,cap_dac_override+ep ./fkbr`). That marks the
file, so it is visible to anyone who inspects it. With only CAP_CHOWN,
files keep their owners but directories stay with the running user: once a
directory belongs to its original owner, a non-root process could no longer
write into it.

`nodes` shows each node's level in the CHOWN column (`no`, `files`, `full`).
Job summaries report entries whose ownership or setuid bits could not be
applied.

## TUI

Each pane starts at the node list. Enter opens a node's shared directory,
Backspace goes back up and F2 jumps to the node list, so either pane can
show any node. Nodes with several roots show a roots level in between. F1 lists all
keys.

| Key | Action |
|---|---|
| Insert / Space, `+` `-` `*` | select; select all, none, invert. In the node list they mark nodes: the marked nodes are `@dest` |
| F3 | view a file (first 64 KiB; XML and JSON highlighted, binary files as hex) |
| F4 | sync: make the other side match this pane (plan shown first). Nothing selected: the whole directory. Selected folders: each into a folder of the same name; selected files are copied. Mirror (Ctrl+O) deletes only inside what is synced |
| F5 / F6 | copy / move the selection. The destination defaults to `@dest` while nodes are marked, else to the other pane; type `node` or `n1,n2`, or press Ctrl+A for every node (`@all`). Marked nodes that are offline are detached, to catch up later, as with `@all` |
| F7 / F8 | make directory / delete (asks first) on whichever node the pane shows |
| F9 | jobs panel with progress and ETA: Enter for details (each node's state, time-stamped notes), `c` cancel, `u` catch up |
| F10, `q` | quit; if this TUI is the node and it has running jobs, it asks first (its part pauses and resumes the next time) |
| F1, `?` | help: every key, how sync works, and this node's log file |

The colours fill the screen in dark green, like Norton Commander's blue.
They need 256 colours or more; xterm-like terminals that report only 16
(`TERM=xterm`) are treated as 256-colour, since nearly all of them are.
With only 16 colours the panels turn black with green text. `-colors
true|256|16|none` overrides the detection, and `NO_COLOR=1` turns colours
off (the cursor then shows in reverse video).

Every 30 seconds the name in the top-left corner slides out, the version
(`v1.0 f0c0e0b`) scrolls through, and the name slides back in; a small
spinner turns in the bottom-right corner. Both show the TUI is alive.

Directories end in `/` and symlinks in `@`, like `ls -F`; `../` goes
up. A name too long for its column scrolls slowly on the cursor row, so you
can read all of it.

Move deletes the source only after the copy finished with no errors, no
skipped entries and no detached targets. Otherwise the source is kept and
the status line says why. Panes showing a job's destination refresh when
the job finishes.

## Sync

`fkbr sync [-mirror] [-checksum] [-n] SRC DST` scans both sides, prints the
plan and applies it. Only differences are transferred.

- `-n` stops after the plan and prints its ID; `fkbr sync -plan ID` applies
  that plan later.
- `-mirror` also deletes extra entries on the targets. If any target would
  lose more than `sync.max_deletes` entries (default 1000), the apply is
  refused unless you pass `-force`. `-max-deletes N` changes the limit for
  one run.
- DST may name several nodes, `@all` or `@role`. Each entry is sent once for all
  the targets that need it.
- In the TUI, F4 always shows the plan for confirmation first.

Measured performance is in `benchmarks.md`.

## Day-to-day

- Ctrl-C on `cp` or `sync` only detaches the terminal; the job keeps
  running in the nodes.
- `cancel ID` stops a job everywhere and cleans up staging.
- `catchup ID` re-sends a partial fan-out job to targets that were
  detached.
- Restarting a node is safe: its jobs resume from the state on the share.
- To remove fkbr, stop it and delete the binary and its directory on the
  share.
