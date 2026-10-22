# What the cloud session did (2026-10-22)

A long Claude Code cloud session on this repo: what it found, what it
changed, why, how it was tested, the mistakes it made and how they were
fixed, and what still needs real hosts. The work itself is in two pull
requests. This file is the only thing pushed to `master` directly, on the
owner's request.

- **PR 1**, branch `claude/peaceful-allen-6ly79z`: sync safety, the share
  store split out of `nfsmailbox`, and nodes over ssh.
- **PR 2**, branch `claude/peaceful-allen-6ly79z-tui`, stacked on PR 1:
  the TUI on its own (no share), ssh hosts linked from the TUI, and root
  `local` that only this host sees.

Two commits of PR 1 are already on `master`, replayed by the owner:
`c6f3486` (tests pass as root) and `dffe962` (a sync plan is applied once),
with the owner's follow-up `c2d3b81`. PR 1 was rebased onto `master`
afterwards, so it holds only the rest.

## 1. Starting point

- The repo had no open issues or PRs and no CI. `make vet` was clean.
- `go test ./...` failed in two places, both only because the cloud
  container runs as root:
  - `TestScanRealTree` expected 6 bytes and 1 error, but root can read the
    test's locked directory, so it counts 7 bytes and 0 errors. The test
    already adjusted its expected file list for root, not the counts.
  - `TestFailedTargetDetached` makes a target read-only with `chmod 0555`
    to make it fail; root ignores that. Four other tests already skip as
    root; this one did not.
- Fixed both (now on `master` as `c6f3486`).

## 2. Test plan gaps, then three sync findings

Asked to suggest (not implement) gaps in `docs/testplan.md`, the session
read the plan against the code. After the owner's `CLAUDE.md` arrived,
two suggestions were dropped (forged plans: the share is open by design;
xattrs/ACLs: a known gap the owner chose). Three sync cases were then
probed with throwaway tests, before anything was changed:

1. **One plan applied four times at once through one node ran four
   times** (four jobs). `ApplySync` checked `plan.Applied`, ran the
   deletions, submitted the jobs and only then saved the plan. Fixed: the
   check and the mark are one step per node (a mutex), and the plan is
   saved as applied before anything runs, so a crash mid-apply cannot
   leave it open to a second apply either. Two *different* nodes applying
   the same plan in the same instant can still both pass: the share has
   no exclusive create; closing that needs a protocol change tested on
   real NFS. Now on `master` (`dffe962`, `c2d3b81`). Test:
   `TestSyncPlanAppliesOnceAtATime`, which fails without the fix.
2. **A mirror deleted files created after its dry run.** A file made on a
   target after the plan, inside a folder the plan removes, was deleted
   without ever being shown, and it existed only on the target. Fixed in
   PR 1 (`Sync: delete only what the dry run showed`): remove requests of
   a sync now name the plan, and the target deletes only what its own
   scan for that plan saw *as it still is* (type; size and mtime of
   files; symlink targets). Whatever is new or changed stays, with the
   folders holding it, and the apply lists it as a plan problem; the next
   dry run shows it. Older builds ignore the new field and delete as
   before (graceful in a mixed fleet). Test:
   `TestSyncKeepsChangesSinceTheDryRun`, which fails without the fix.
3. **A type change whose source vanished before it was sent** behaved
   acceptably (the job reports "vanished since it was planned", the next
   sync settles). Kept as a regression test,
   `TestSyncSourceGoneAfterDeletes`.

`docs/protocol.md`, `docs/operations.md` and `docs/testplan.md` describe
each.

## 3. SSH: how the design got where it is

The owner wanted fkbr to work without an NFS share: over ssh, and on
local drives, as separate modules. The design changed four times, each
time for a reason worth keeping:

1. **First idea: the share on an ssh host, over SFTP.** The mailbox code
   would run unchanged over SFTP. The owner chose "direct node-to-node"
   instead, with the system's ssh and an SFTP library.
2. **The session then measured the mailbox's cost.** Three daemons on one
   local share, traced with `strace`: each node makes about 15 share
   operations a second idle, and 25 to 65 a second during a copy of
   300 MB plus 2,000 small files (the source about 1,050 in 16 s). Over
   SFTP each is one to three round trips: fine on a LAN (like NFS), but a
   minute or more of waiting per copy at 20 to 50 ms. So polling files
   over ssh was dropped.
3. **The owner: stream the same chunks.** The chunk format (FKB2, zstd,
   SHA-256, the strict and fuzzed reader) does not depend on the mailbox:
   the engine builds and checks chunks, and the store only holds bytes.
   So over ssh the chunks stay byte-for-byte the same.
4. **The owner: straight from source to target, nothing in between.**
   That also survives jump hosts: fkbr runs the user's own `ssh`, so
   whatever ssh reaches (`ProxyJump`, `~/.ssh/config`, the agent),
   fkbr reaches.

The owner also objected to the ssh code depending on `nfsmailbox`, and
chose a shared, neutral store over a fully separate one. Result below.

## 4. What was built for ssh (PR 1)

### `internal/store`: the share store, split out of `nfsmailbox`

- The code for the registry, inboxes, jobs, statuses, acks, chunks and
  blobs in a directory moved to `internal/store` as `store.Dir` (git
  records the files as renames, so history follows them).
- `nfsmailbox` keeps what NFS needs, the attribute-cache refresh trick,
  as a hook (`store.Options.Fresh`), and opens the store with it as
  before. NFS behaviour is unchanged.
- `Options.Private`: directories 0700 and files 0600 whatever the umask,
  for a share only one process reaches. Test: `TestPrivateShareModes`.

### `internal/sshlink/frame`: the link protocol

- Each side writes `fkbr-link 1\n` first. What comes before it (a login
  banner from shell rc files) is skipped and logged; "command not found"
  and the like come back as the error.
- Frames: type, stream ID, length, payload (at most 64 KiB). Many streams
  at once, opened by either side, each with its own flow control (4 MiB
  window), so a big transfer never holds up a directory listing.
- Keepalive: a ping every 15 s; a session that hears nothing for 45 s
  ends.
- Strict reader (frame sizes, windows, stream IDs) and a fuzzer,
  `FuzzSession` (about 620,000 inputs in 30 s, no panic or hang). Tests:
  big and many streams, a stalled stream beside a busy one, banners,
  resets, a silent peer, bad frames.

### `internal/sshlink`: the nodes' ends

The engine only knows the `transport.Transport` and `transport.Store`
interfaces; `sshlink` implements them for both ends.

- **Records stay on the home node** (the host that runs `ssh:`), in a
  private `store.Dir`. For each remote node it opens the store *as that
  node* and answers that node's calls with it, so every record keeps its
  owner checks (only a node writes its own status and acks).
- **Nothing polls over the network.** A remote node's "anything for me?"
  waits on the home node and answers as soon as a request is there;
  waiting for replies is one call that streams each reply part.
- **Chunks** are kept on the source's own disk until every active target
  has acked them (bounded by `job_quota`), and read straight from it by
  each target. Between two remote nodes the home node relays the stream
  in memory, never to disk.
- **Record calls complete even when the caller is stopping**, as writes
  to a share do. Found by running the engine suite over links (below): a
  restarted node failed to register because the link honoured a
  cancelled context that NFS would have ignored.
- **A dropped link reads as EIO**, so transfers ride it out for up to
  3 min, as on a soft NFS mount, while the home node restarts the session.

### Sessions, command line, TUI

- `fkbr ssh:web01:/srv/www,db01` runs `ssh -T -o BatchMode=yes -o
  Compression=no` to each host and starts the hidden `fkbr link` there, in
  the given directory. `BatchMode`: no password prompt over the TUI.
  Compression off: chunks are zstd already.
- Sessions restart with backoff (1 s, doubling to 1 min). Quitting the
  home node asks every remote node to stop, so each leaves the swarm
  while its link is still up; after a timeout, ssh is ended.
- `check` probes each host and shows its fkbr version; `nodes` and the
  TUI node list show each link's state.

### Tests for ssh

- **The whole engine suite over links:** `FKBR_TEST_TRANSPORT=ssh go test
  ./internal/engine`. The first node built on a share becomes the home
  node; every other node is linked to it over an in-process pipe. It
  passes, also with `-race`. Tests that inject faults into a share
  directory's files are skipped there (`nfsOnly`). Of the five that
  failed at first, one was the real bug above; four were mixed setups
  (one node on NFS, the others linked) that cannot exist.
- **End to end** (`cmd/fkbr/ssh_test.go`): a stand-in `ssh` script runs
  the test binary itself as the remote fkbr. Copies home to remote,
  remote to home and remote to remote, a mirror sync, a clean stop after
  which the remote nodes have left, a target killed with `kill -9`
  mid-transfer that reconnects and finishes the job, and a host without
  fkbr that shows why.

## 5. What was built for the TUI (PR 2)

### Root `local`, seen only by this host

The owner asked for `/` in the TUI. Simply sharing `/` would hand the
whole file system to every peer (and, on NFS, to anyone who can write the
share), so it became a local-only root (`engine/localroot.go`):

- Not published; requests from other nodes naming it are refused as an
  unknown root.
- A job that reads or writes it runs only if this node submitted it: the
  request is kept in the private state dir (`own/<job>.json`), and the
  source and target refuse a job whose record differs. Job records on a
  share can be forged or changed; the state dir cannot.
- Writes into it only without a share (ssh, or on its own): on NFS anyone
  who can write the share could forge the chunks that would be installed
  there.
- Syncs on it are refused for now: their path lists and snapshots are
  blobs on the share.
- `-local-root DIR` or `local_root:`; `none` turns it off. `check` reads
  it but never writes into it (it had written a test file into `/`; found
  by trying it, fixed).

Tests: `TestLocalRootUnseenByPeers`, `TestLocalRootOnlyOwnJobs` (a job
made up by another node, and the node's own job changed on the share,
both refused), `TestLocalRootWritesNeedPrivateShare`.

**An older bug found on the way:** a move on one node from one root into
another root that holds it (two roots overlapping on disk) was accepted
and would delete what it had copied. Now refused for any two roots.
`TestMoveAcrossOverlappingRoots` fails without the fix.

### The TUI without a share; ssh hosts from the TUI

- `fkbr` with no share opens the TUI on this host alone (an ssh swarm's
  home node with no hosts yet; state dir `/tmp/fkbr-<uid>-<node>-ssh`),
  instead of printing the usage. Left pane: the directory it was started
  in; right pane: another online node, else root `local`.
- In the node list, F7 links a host while the node runs and saves it;
  F8 unlinks the one under the cursor. A host whose link is not up shows
  there with why (for example "Permission denied (publickey)").
- `api.Linker` (list, add, remove links) is also on the socket.

### Where linked hosts are saved

This went back and forth with the owner; the final rule:

- The config file's `ssh:` list. The config file is `-config`, else
  `$FKBR_CONFIG`, else `fkbr.yaml` beside the binary if it exists, else
  `~/.fkbr.conf`. The same order is used for reading.
- `~/.fkbr.conf` is created only when a host is linked and none of the
  others exists, with mode 0600: the one file fkbr may put in `$HOME`,
  recorded in `CLAUDE.md` as the owner's call.
- Only the `ssh:` list changes; other keys, comments and the file's mode
  stay; never written through a symlink.
- A share on the command line wins over the file's ssh hosts; a file
  naming both `nfs_root` and `ssh` is refused.

Tests: `TestConfigPathOrder`, `TestSavedHosts`,
`TestSSHHostsLinkedAtRuntime` (linked, saved, linked again at the next
start, unlinked and forgotten), and TUI tests for the start panes, the
host rows, F7 and F8.

## 6. Verification

On the final state of both branches, in this cloud container (x86-64, as
root):

- `make vet`
- `go test ./...`
- `go test -race ./...`
- `FKBR_TEST_TRANSPORT=ssh go test -race ./internal/engine`
- `make` (both static binaries)

All pass. The F1 help's new section was rendered at 72 columns.

## 7. Mistakes, and how they were fixed

- **Attribution in a commit message.** The first commit carried
  `Co-Authored-By` lines, which `CLAUDE.md` forbids. Reworded and
  force-pushed to the session branch before anything else was built on
  it.
- **A 15 MB binary in a commit.** A stray `go build ./cmd/fkbr` left
  `./fkbr` in the repo root, and the next commit picked it up
  (`.gitignore` covered `bin/` and `dist/` only). Removed from that
  commit, `/fkbr` added to `.gitignore`, PR 1's branch force-pushed; every
  later push checked the branch for large blobs.
- **A commit made after `make vet` failed.** A badly chained command went
  on to commit after gofmt reported a misaligned struct; caught before
  any push, fixed and amended.
- **A wrong commit split.** When PR 2's history was rewritten to drop the
  `~/fkbr.conf` version, new code first landed in the docs commit; caught
  by checking each commit's files before pushing, and split again.
- **PR 1 conflicted after the owner replayed two of its commits** onto
  `master`; rebased the rest onto `master` (the replays matched the
  originals exactly) and force-pushed with lease.

All force-pushes went to the session's own branches, with lease. Until
this file, nothing was pushed to `master`.

## 8. Still needs real hosts

- Two nodes applying one sync plan at the same moment, on NFS with
  attribute caching on.
- The pre-delete re-check on NFS, and an older build as a mirror target.
- ssh: real ssh, a `ProxyJump` path, 20 to 50 ms round trips, links cut
  by the network (not a kill), the home node killed, x86-64 and ARM64 in
  one swarm, a host whose `/tmp` is in memory, and throughput against
  `scp` and `rsync -e ssh` for `benchmarks.md`.
- The TUI on a real terminal: start without a share, F7 a real host,
  quit and start again.

## 9. Not done (in the designs)

- Local mode's drives as roots (`-drives`), and same-node copies without
  chunks (`docs/local.md`).
- Several ssh connections per host, if one connection's encryption turns
  out to be the limit.
- Direct sessions between two remote nodes instead of relaying through
  the home node.
