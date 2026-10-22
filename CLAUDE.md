# CLAUDE.md

fkbr: a swarm file commander and sync tool for hosts that share only an
NFS mount. Nodes never talk over the network; they exchange requests and
data through a directory on the share. Go, single static binary.

**This file is committed and public.** Keep it free of IP addresses, host
names, user names, e-mail addresses and anything about where fkbr is
used. The owner's private notes (test lab, deploys, history) are in
`CLAUDE.local.md`, git-ignored, only on their machine. If it is not
loaded you are probably in a cloud session: see "Cloud sessions".

User-facing docs: `README.md`, `docs/FAQ.md`, `docs/operations.md`,
`docs/protocol.md` (the design), `docs/benchmarks.md`, `docs/testplan.md`
(what could break, how it is tested, results). License: Apache 2.0,
copyright "The Author".

## Hard requirements (from the owner, do not regress)

- **Nothing installed, ever.** No package, install script, systemd unit,
  system user, or files in `/etc`, `/var` or `$HOME`. Ship one binary;
  config is optional (flags, `$FKBR_CONFIG`, or `fkbr.yaml` beside the
  binary). "Very incognito".
- **Swarm, no master/slave.** `cd /dir && fkbr /mnt/nfs/somedir` registers
  the host (named after its short hostname) and shares the current
  directory. Targets are a node, `n1,n2`, `@all`, or in the TUI `@dest`.
  Roles are optional tags only.
- **Runs on any Linux**, x86-64 and ARM64: `CGO_ENABLED=0`, no glibc
  dependency, kernel 3.10 compatible (CentOS 7 is the oldest target).
- **No background service or cron.** A running TUI or `daemon` does all
  the work, including cleanup. Runtime files live in
  `/tmp/fkbr-<uid>-<node>` (not `/run/user`, which logout deletes); the
  dir must be ours and not writable by others. A TUI that runs the node
  logs there (`node.log`, kept after exit, rotated at 1 MiB; F1 shows the
  path); the daemon logs to stderr.
- **Every build produces both** `dist/fkbr-linux-amd64` and
  `dist/fkbr-linux-arm64`; `bin/fkbr` is a symlink to the host one.
- **Commit only when asked.** No `Co-Authored-By:` or any other
  attribution in commit messages.
- **The share is open by design:** world-writable, requests not
  authenticated (one trust domain). Records from the share are validated
  (`ValidID`, size caps), but don't propose groups or authentication.
- **Docs for users stay short.** The FAQ uses very few, simple words.
  Durations in docs as h/min/s, not raw seconds; benchmarks.md shows only
  current numbers. The F1 help is part of the docs: keep it current,
  lines under 72 columns, one key per row (render it before changing it).
  The help flows with the window (`flowText`): write each paragraph as one
  line in `helpText()`; key rows stay rows and wrap under their
  description.

## Cloud sessions

- There is no NFS share or test lab here. Run `go test ./...`,
  `make vet`, `make` and `make test-race` (fine on x86-64), and say in
  the summary what still needs a check on real NFS hosts (attribute
  caching, several hosts, kill/stall/NAS-cut behaviour) so the owner can
  run it.
- Work and push on the session's branch; the owner merges and deploys.
  Never merge into or push to `master`: the owner replays branch commits
  onto `master` under their own name, so their hashes differ from yours.

## Build, test, version

go.mod says `go 1.26.0`: the oldest toolchain the owner builds with.
bubbletea/lipgloss/bubbles v2 (charm.land) would be a port, not a bump.

```
make            # both arches + dist/SHA256SUMS, bin/fkbr -> host arch
make vet        # go vet + gofmt check
go test ./...   # ~90 s on a small arm64 box; engine tests run real nodes on temp dirs
FKBR_TEST_TRANSPORT=ssh go test ./internal/engine   # the same, nodes linked as over ssh
make test-race  # needs a 48-bit VMA: fails on some arm64 kernels; on a 2-CPU box run
                # single passes (-count=3 OOMs bundle.test, slows TestContract past 15 s)
scripts/bench.sh -n 3   # 1 source + N targets on one machine
```

Version: `product.Release` ("1.0"; raise it for a release) plus the commit
from `make` (`-X main.commit`, `-dirty` for uncommitted changes; the
Makefile asks git with `-c safe.directory='*'`). Build `make` *after*
committing so a deployed version has no `-dirty`. A plain `go build`
falls back to Go's vcs build info (`buildCommit()` in main.go), then to
`internal/product/commit.txt` (`$Format:%h$`, `export-subst` in
`.gitattributes`, filled in by git archive downloads). A copy without
`.git` is just `v1.0`; `make COMMIT=abc1234` overrides. `fkbr version`
prints `fkbr v1.0 <commit>`, the TUI ticker the same; the `nodes`
VERSION column shows just the commit.

Tests use `t.TempDir()` as the share. Engine helpers (`nodeCfg`, `build`,
`node`, `runWorker`, `waitJob`, `waitUntil`, `sameTree`, `makeTree`) are
in `internal/engine/*_test.go`; fault wrappers (`faultStore`,
`stuckInbox`, `lossyInbox`) in `faults_test.go` and `stall_test.go`.
Model tests: `model_test.go` (TestSyncModel, `FKBR_MODEL_ROUNDS`),
`move_model_test.go`, `danger_test.go`; fuzzers `FuzzReadNative`,
`FuzzExclude`.

## Layout

```
cmd/fkbr/main.go            arg parsing (SHARE positional, default cmd tui; globalFlags: global
                            flags after the command too), node startup (waits for Ready)
internal/api                frontend-facing types + Service interface, error codes
internal/api/local          in-process client (engine + localfs + nfsmailbox)
internal/store              the share: registry, inboxes, jobs, chunks, blobs in a directory;
                            layout() self-heal; Options.Private for a share only this user reaches
internal/api/remote         HTTP+JSON over the unix socket, SSE events, ServeSocket self-heal
internal/engine             jobs, send/recv, fan-out/detach, sync, files ops, events,
                            worker lock, instance.go (one worker per node ID)
internal/transport          Transport (messaging) + Store (jobs/data/blobs) interfaces
internal/transport/nfsmailbox  the store on NFS: the refresh trick
internal/sshlink            nodes over ssh (docs/ssh.md): home.go (records, relay), remote.go,
                            session.go (runs ssh, backoff, Probe); sshlink/frame the link protocol;
                            cmd/fkbr/link.go is `fkbr link`, the remote end (hidden command)
internal/bundle             chunks in fkbr's own format (native.go, optional zstd); big files
                            split into segments over consecutive chunks
internal/receive            os.Root-confined staging (.fkbr-tmp), commit, metadata order
internal/inventory          streaming snapshots + k-way merge sync planner
internal/localfs            os.Root access to roots, chown/DAC capability probes
internal/mountinfo          /proc/self/mountinfo parsing, NFS caching detection
internal/exclude            rsync-like excludes
internal/safetext           cleaning text from elsewhere before it is drawn
internal/check              `fkbr check`
internal/config             YAML/flags, defaults (node=hostname, root=cwd "files")
internal/frontend/{cli,tui} CLI commands (parseFlags: flags anywhere); bubbletea TUI
internal/product            Name, EnvPrefix, Release, Version(commit), Expand("{name}")
```

Every runtime use of the name comes from `internal/product`.

## Key design facts

- Per-node inboxes with a single consumer; atomic tmp+rename publication;
  liveness from heartbeat mtimes on the NFS server clock. Every protocol
  file is replaced by rename, never edited in place.
- **NFS attribute caching:** shares are often NFSv3 with `actimeo=1800`;
  clients then see stale directories and negative dentries for up to
  30 min. `nfs_refresh: auto` detects this from the mount options;
  nfsmailbox creates and removes a tiny `.r-*` file in a directory before
  reading it (throttled to 100 ms per directory); if that fails with
  ENOENT the parent is refreshed too.
- **Never look up a name before it exists; wait by listing** (`listed.go`:
  `readListed`, `listed`). A failed lookup caches "missing", and the Linux
  client can keep it forever. Replies, chunk metas, acks, COMPLETE/CANCEL,
  info.json all wait by listing.
- **Soft mounts:** transfers retry EIO/ETIMEDOUT/ESTALE for up to
  `transientBudget` (3 min) of consecutive errors (`engine/transient.go`).
  On hard mounts a host cut off from the NAS blocks and resumes.
- Metadata order on receive is mode → mtime → chown → setuid re-chmod, so
  it works with only CAP_CHOWN. Directories are chowned only with
  CAP_DAC_OVERRIDE.
- Sync is one-way (incremental or mirror). Plans are dry runs stored as
  blobs; applying runs deletes first, then one transfer per target group.
  `max_deletes` (default 1000) needs force.
- **Chunk format (protocol 2):** fkbr's own, not tar (`bundle/native.go`,
  protocol.md "Chunk format"): magic FKB2, per entry kind +
  prefix-shared name + varints, data unpadded, 0 byte at the end. The
  reader is strict (bounds on every field, CleanRel names, end byte
  required) and fuzzed. Mixed protocol versions refuse each other with a
  clear message.
- Sender: chunk sizes ramp 4, 8, 16 … MiB up to `chunk_size`
  (`bundle.Writer.limit`, depends only on seq: resume-safe); the walk runs
  ahead of the bundle writer and small files (≤ 1 MiB) are read by 8
  workers within a 64 MiB budget (`engine/prefetch.go`); the bundle keeps
  walk order. Receiver: reads each chunk ahead (`readAhead`), sets file
  metadata through the open fd (`setFileMeta`), moves a new destination
  dir in with one rename (`moveNewDir`), creates staged files through a
  cached staging dir (`stagingDir`; closed in MarkBad, Commit and Abort).
- Fan-out: chunks staged once (job_quota 1 GiB window), deleted when every
  *active* target acked. A target offline for `offline_after` (30 s) or
  without progress for `detach_after` (10 min) is detached while others
  remain; detached targets install nothing and `catchup` re-sends. The
  *last* target is detached only when offline for `detach_after`; a
  source with no active target is done.
- Cleanup: handled requests are deleted at once. Finished jobs, plans and
  failed requests are pruned after `keep_done` (1h) by a running node, at
  start and every half `keep_done`. Floors under any keep: a job stays
  1 min after its last status, plans/scans 10 min and never while an
  unfinished job uses them; job folders without job.json are pruned.
  Freshness is judged by entries, not dir mtimes (the refresh trick bumps
  those). `forget NODE` removes an offline node; pruning forgets nodes
  offline longer than `forget_after` (7 days). A TUI that runs the node
  leaves the registry on a clean quit (`LeaveOnStop`) unless an unfinished
  job needs it or the entry was a daemon's (`NodeInfo.Transient`).

### Robustness rules (each found by a test; don't regress)

Regression tests for these are in `internal/engine/robust_test.go` and
package tests; docs/testplan.md lists them.

- Fetch never drops a claimed request (retries, parks after 20 failed
  reads). The heartbeat runs on its own goroutine. Prune ages are mtimes
  against `serverNow()`, never the local clock; IDs/times come from the
  share clock (`Mailbox.Now`, `id.At`).
- A source re-sends `receive` every minute to targets without a status. A
  peer missing from the registry for `offline_after` fails the job. A
  target whose source is `done` installs at once.
- **One worker per node ID** (`engine/instance.go`): info.json carries an
  instance token, PID and boot ID; the heartbeat file holds the token. A
  worker refuses an ID online on another host or run by a live process
  here. A crashed worker is replaced at once. A foreign token later: the
  later starter stops without leaving. The lock file says
  running/stopping; a new worker waits for a stopping one (2 min).
- **Self-healing share:** `Mailbox.layout()` recreates tmp/, nodes/<self>,
  inbox/<self>/* and reply/<self> on ENOENT (never job dirs); own dirs
  only once registered (a CLI without a node creates none). Heartbeat
  returns `ErrNotRegistered` when info.json is gone or unreadable and the
  engine re-registers. Unreadable `nodes/` entries are skipped;
  `forget` removes one once its heartbeat is stale (`StaleEntry`).
- Names not valid UTF-8 travel as NUL+base64 (`api.EncodeName`, custom
  JSON methods on every API type with paths). Chunk names up to 64 KiB
  (`bundle.MaxNameLen`). Bulk share writes `Sync()` every ~2 s of data
  (`store/writeback.go`) so heartbeats don't queue behind a chunk.
  Share reads are capped at 64 MiB.
- Target scans probe the mtime range (`localfs.MtimeRange`, inside the
  root, never writing on `sync -n`) and name rules (`localfs.Names`: vfat,
  exfat, casefold) into the snapshot header; the planner clamps mtimes,
  leaves out unstorable names and case/form clashes (`Refused`); excludes
  fold on case-insensitive targets. The receiver refuses names FAT/exFAT
  alter, skips chmod on NoModes filesystems.
- Receive: install continues past entries it can't place (counted as
  errors); owner-read-only dirs are made writable for the moment, like
  rsync; zero blocks past EOF become holes; a failed install removes its
  staging. Jobs waiting for a `max_jobs` slot publish `queued`.
- A copy/move of a named source path that does not exist fails
  (`api.ErrNotFound` at Submit, `missingPaths` in send.go).
- A node whose shared dir is deleted/moved/replaced stops and says so
  (`localfs.CheckRoots` every 5 s).

### Excludes, moves, plan listings

- `internal/exclude`: name anywhere, path anchored at the top of what is
  copied/synced, `/x` anchored name, `dir/` dirs only; path.Match, no
  `**`. cp: patterns apply inside the named paths; sync: both sides'
  scans skip them, so excluded entries are never deleted. Scans write
  `<node>.holders` (folders above excluded entries); the planner never
  deletes or type-replaces those, and RemoveRequest carries Exclude/Base
  as a second net (`removeExcept`). `NodeInfo.Features` guards mixed
  builds.
- Moves (TUI F6, CLI `mv` = cp + Remove after a clean job) refuse
  excludes, and a move into/onto its own sources is refused at Submit
  (`moveOverlap`) and by the source with symlinks resolved
  (`checkMoveHere`).
- `SyncPlanEntries` reads a plan's copy-*/delete-* lists. `sync -n` prints
  20 per list, `-v` all. Sync `-update`/`-u` (`PlanOptions.KeepNewer`).

## TUI

Filled Norton Commander look; colours in `theme.go`, each a
`CompleteColor` with 256/16-colour fallbacks. `TestThemePaintsEveryCell`
fails on any cell without a background, so every string drawn must carry
the panel or dialog background (pad with a styled fill, not lipgloss's
own padding). Without colours, `styles.highlight` writes reverse video.

- Names: `name/` dirs, `name@` symlinks, `../` up row (`pane.formatRow`).
- Liveness: every 30 s the header name slides out, the version runs
  through as a ticker and slides back (`anim.go`); a yellow spinner
  replaces a corner of the jobs panel (`withCorner`). The owner rejected a
  pulsing or blinking cursor bar; don't bring it back.
- `@dest`: Insert/`+`/`-`/`*` in a node list mark nodes in `model.dest`,
  shared by both panes. While any are marked, F5/F4 default to `@dest`;
  the dialog expands it to names and sets `detach_offline`. `dest` and
  `all` are reserved role names.
- F4 sync: active pane = source, other pane/@dest = destination. No
  selection: the whole directory; marked folders: one plan each; marked
  files: one copy with overwrite. One confirm dialog for all; `l` lists
  all deletions.
- F1 help is `model.helpText()` in `dialogs.go`: title "fkbr v1.0 <commit>
  · help", sections in capitals, one key per row, 15-column key field.
- Job details (Enter in F9) update live; totals come from a background
  count on the source (`engine/count.go`).
- `q` quits like F10. F3 highlights XML/JSON. `-colors auto|true|256|16|none`.
- Mouse (`mouse.go`): click, double click = Enter, right click marks
  (drag marks every row passed), wheel, key bar cells, dialog hints and
  settings lines clickable. `-mouse=false` turns it off.
- Scrollbars (`scrollbar.go`) only when content does not fit.
- Text from elsewhere goes through `internal/safetext` before it is drawn;
  `View()` ends in `safetext.Screen` as a net. The CLI cleans stdout only
  on a terminal (piped/`-json` stays exact).

## Screenshots

`scripts/screenshots/take.sh` builds a five-node demo under
`/tmp/fkbr-demo`, drives the TUI with pyte (`shoot.py`, 120x34) and writes
`docs/screenshots/*.png` (needs pyte and pillow; uses `uv` when `PY` is
unset). Retake only when the look changed. Demo data stays neutral. In a
pty, answer `ESC]11;?` and `ESC[6n` or nothing draws for ~5 s.

## Gotchas when editing

- Python string replacements silently miss after gofmt realigns struct
  fields, and a placeholder can match real code. Use the Edit tool.
- Don't chain a file write after a command that can fail with `&&`.
- Unix socket paths are limited to about 100 bytes; use `/tmp` or short
  relative paths for test sockets.
- The engine tests' `nodeCfg` bypasses config defaults. Set any new config
  field there or tests get zero values.
- Anchor `pkill`/`pgrep` patterns with `^` (an unanchored one can match
  the shell running it).

## Known, minor, not fixed

- `/proc` files arrive empty (stat size 0, like tar/rsync).
- A TUI attached to a TUI that quits keeps retrying the dead socket.
- A copy to a node that just left says "destination: missing root".
- bubbletea v1's init queries the terminal (OSC 11 + DSR): 5 s where
  nothing answers.
- rsync gaps: metadata-only changes re-send whole files (owner chose not
  to build it), hard links, ACLs/xattrs, one-file-system, bandwidth limit,
  --backup.

## Open ideas (not requested yet)

- `-borders single` for terminals whose fonts mangle double lines.
- Bidirectional sync with a baseline.
- The TUI copy/sync dialogs still show the internal root id `files`.
- An `.fkbr-tmp` staging dir is visible in destination roots during a
  transfer.
- Sparse files cross the share as real zeros; detecting holes on the
  source would help.
- A job whose only problems are type clashes ends `done` with errors.
- A single-target copy waits forever if the share stays full.
