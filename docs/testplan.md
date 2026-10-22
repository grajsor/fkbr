# Test plan

What could break fkbr, how we test it, and where each test stands. Unit
and engine tests run with `go test ./...` (nodes on temp dirs, one share).
Lab tests need several hosts on one NFS share; their scripts are kept
outside the repo because they hold lab addresses.

Status: **auto** = in `go test`, **lab** = run on the lab and passed,
**todo** = not run yet.

## 1. Data correctness

| Case | Expected | Status |
|---|---|---|
| Copy and sync trees with odd names, symlinks, empty dirs, big files split over chunks | Target equals source (type, size, mode, mtime, sha256) | auto, lab (torture) |
| Chunk corrupt on the share, once or for good | Re-read once; persistent mismatch fails the target, nothing installed | auto |
| Source file changes while it is read | File reported and left out; the rest arrives | auto |
| Two jobs install into the same folder at once (same or different sources, skip/overwrite, cross traffic) | Every file whole, from one of them; no staging left | lab (explore `same`) |
| Copy a folder into a subfolder of itself | One nested copy, no recursion | lab |
| Mirror a folder while another job reads it | Readers get whole files; vanished files reported | lab |
| Two mirrors into one folder at once | Whole files; the folder matches one source (a race, like two rsyncs) | lab |
| Metadata: modes, mtimes, setuid without privileges, ownership with CAP_CHOWN only | As documented in protocol.md | auto |
| Hostile chunk on the share: escaping or overlong names, bad lengths and offsets, truncation, unknown kinds | Rejected before anything is written | auto, fuzzed (`FuzzReadNative`, 6 million inputs) |
| Resume after a crash with ramped chunk sizes and read-ahead | The source regenerates identical chunks | auto, lab (torture) |
| One sync plan applied four times at once through a node (TUI and CLI), then from another node | Applied once, one job; the others refused (all four ran, each submitting a job) | auto (`TestSyncPlanAppliesOnceAtATime`) |
| A mirror applied after files appeared or changed on the target since its dry run | Only what the dry run showed is deleted; the rest is kept and reported, and the next dry run shows it (a file made after the dry run was deleted) | auto (`TestSyncKeepsChangesSinceTheDryRun`) |
| A move on one node from a root into another root that holds it (two roots overlapping on disk, e.g. the local root `/` and the shared directory) | Refused: it would delete what it copied (it was accepted) | auto (`TestMoveAcrossOverlappingRoots`) |
| A type change whose source entry is gone by the time it is sent | Target entry deleted, the job reports it vanished, the next sync has nothing to do | auto (`TestSyncSourceGoneAfterDeletes`) |

## 2. Process failures

| Case | Expected | Status |
|---|---|---|
| kill -9, SIGSTOP (longer than `offline_after`), SIGTERM of source or target mid-transfer | Resume, or detach and catch up; never a partial install | lab (torture: 92 events in four runs, the last on protocol 2) |
| Target restarted after its last ack, before installing | Installs (the source is done and has removed the acks) | auto, found by torture |
| Request lost or unreadable in an inbox | Retried, parked after 20 tries; the source re-sends `receive` | auto |
| Source or target forgotten mid-job | Job fails, no endless wait | auto |
| Second worker for one node ID: other host, other user, other `-state`, both at once | Refused at start; if both slipped through, the later one stops | auto, lab (explore `dup`, before the fix: copies landed in the wrong directory) |
| Restart right after kill -9 (heartbeat still fresh) | Starts at once | auto |
| Worker lock file deleted | Lock re-taken, or worker stops if another took it | auto |

## 3. Share problems

| Case | Expected | Status |
|---|---|---|
| Soft mount: EIO/ETIMEDOUT/ESTALE for less than 3 min | Transfer rides it out | auto, lab (seen 5 times) |
| Share full (ENOSPC/EDQUOT) | Source waits for targets to free space and goes on; state shows "share full" | auto |
| Share full for good, single target | Waits until cancelled (by design; visible) | auto |
| NFS attribute caching (`actimeo=1800`), negative dentries | Refresh trick plus wait-by-listing; no stale reads | lab |
| Host clocks skewed against the NFS server | Liveness and pruning use server mtimes | auto (pruning by code review) |
| NAS restart during a transfer | Soft mount: errors, then resume within 3 min; hard mount: pause | **todo** (needs the NAS) |
| One host loses the network to the NAS for 90 s (blackhole route; hard mount) | A target is detached after 30 s, the others finish, it cleans up when back; a cut source makes the targets wait and the job finishes afterwards | lab (weird `nfscut.py`) |
| Protocol version mismatch on the share | Clear refusal | auto |

## 4. Load and limits

| Case | Expected | Status |
|---|---|---|
| Independent jobs at once (1→2,3 while 5→4) | All verify; total NAS throughput is shared | lab: ~100 MB/s total either way |
| More jobs than `max_jobs` on one node | Extra jobs show `queued`; a fan-out drops a queued target after `detach_after` with a clear reason | auto, lab |
| `job_quota` 256 MiB / 1 GiB / 4 GiB | 1 and 4 GiB equal, 256 MiB slower | lab |
| 1 million small files, one sync | Memory bounded, plan streams | lab (one Pi, 2026-10-19; `benchmarks.md`) |
| 1,000,000 entries copied to 1 and 3 targets | All verified; 3 min 38 s and 5 min 38 s | lab (compare) |
| fkbr against tar, rsync and borg through the share | See `benchmarks.md` | lab (compare) |
| Files bigger than `job_quota` (e.g. 20 GiB) | Segments flow through the window | **todo** on the lab (auto for 3 MiB through 256 KiB) |
| 200 copies submitted at once; 100,000 files in one folder | All done (27 s); `jobs` 0.03 s; 100k files in 12 s, target RSS 55 MiB | lab (weird `flood`, `bigdir`) |
| Target disk full during receive (40 MiB tmpfs) | Target fails cleanly, staging removed, the other target completes | lab (weird `diskfull`) |
| Destination folder not writable for some entries | Those entries are errors; the rest installs; staging removed | auto |

## 5. Odd inputs and a damaged share

| Case | Expected | Status |
|---|---|---|
| Names with newlines, invalid UTF-8, 255 bytes; a 2849-byte path | Arrive intact | lab (weird `names`) |
| Sparse, hard-linked, fifo, socket, unreadable file and dir, symlink loops | Sparse stays sparse; links as separate files; specials and unreadables reported | auto, lab |
| Destination through a symlink pointing outside the root; `../` paths | Refused, nothing written outside | lab |
| mtimes in 2100 and before 1970, nanoseconds | Kept exactly | lab |
| Read-only directories on later copies, syncs and deletes | Made writable for the moment, mode restored (like rsync) | auto, lab |
| Source changing during the send; cancel; copy onto itself | Whole files only; nothing left behind; file intact | lab |
| Mirror of an empty folder over 1500 files | Refused by `max_deletes` unless forced | lab |
| Junk in the share (broken `info.json`, stray files and dirs) | Ignored; every command keeps working | auto, lab |
| `tmp/`, a node's registry entry or inbox, `jobs/`, `data/` deleted under running nodes | Recreated on the next use; the node registers again | auto, lab (weird `vandal`) |
| Type clashes (dir where the target has a file, and back) | Left as they are, reported (like `cp`, even with overwrite) | lab |
| Spaces and unicode in the share path; root through a symlink | Work | lab |
| Subcommand flags after the paths (`sync /a b:/c -mirror`) | Same as before the paths | auto |
| Excludes (`*.tmp`, `.git`, `cache/`, `build/out`) on copy and mirror sync | Not sent; in a sync neither copied nor deleted on either side; a node without the feature refuses with its name | auto, Pi |
| Sync `-update` with files edited on the target | Newer target files kept and counted; older ones replaced; type changes applied | auto, Pi |
| CLI `rm` (folders need `-r`, missing paths are errors), `mkdir`, `mv` (sources deleted only after a clean copy; refuses `-exclude`) | As stated; TUI F6 also refuses an exclude | auto, Pi |
| Random trees and options, sync checked against an independent model (`TestSyncModel`: mirror, keep newer, excludes, type changes, same size and time with other content) | Target as the model says; source untouched; a second sync has nothing left | auto (40 rounds; 400 run once) |
| Random moves with the real `mv`, also onto and into the sources and through a symlink (`TestMoveModel`) | No file content lost; a successful move has everything at the destination | auto (30 rounds); it found that a move into its own subfolder deleted everything (also TUI F6) |
| Mirror with excludes over a folder the source lacks | Excluded files and their folders stay; found deleting them | auto, funky `danger` |
| Odd `rm`/`mkdir`/`mv` arguments (`/`, `..`, staging, the share, a symlink out, `file/`) | Nothing removed or created; `rm file/` refused like coreutils | auto |
| Exclude patterns, fuzzed | No panic; the top is never excluded | auto (3.5 million inputs) |
| Dry run of a mirror | Lists what it deletes and copies (20 per list, `-v` all; TUI: first 5, `l` all) | auto, Pi |
| TUI: 30,000 random keys and resizes down to 10x4 | No panic, no exit | lab (weird `fuzz_tui.py`) |

## 6. How fkbr is started and where

| Case | Expected | Status |
|---|---|---|
| TUI quits, or its terminal closes (SIGHUP, an ssh drop) | The node leaves the share; nothing left behind | auto, lab (funky `sighup`; found: a TUI stayed registered after quitting, and a hangup left entry, socket and lock) |
| Same node name, second share on the same host | Refused with what to do (it attached to the other share's node) | auto, lab (funky) |
| CLI or TUI started elsewhere, or with `-ro`/`-root`, while the node runs | Attaches, with a note on what the node shares | auto, lab (funky) |
| Share inside the shared directory (`cd ~ && fkbr ~/box`) | Share left out: not listed, sent, written or deleted (it was copied along) | auto, lab (funky) |
| Shared directory inside the share, reached by a symlink or `-root` | Refused (a symlink got past the check) | auto, lab (funky) |
| Names, link targets, a peer's host/OS, file contents with terminal escapes | Shown as `?` in the TUI and on a terminal; exact when piped, in `-json` and in actions (they reached the terminal raw) | auto, lab (funky) |
| Target on FAT: names it cannot hold, README next to readme, 2 s mtimes, no modes | Those names refused and reported, the rest arrives; sync settles (one bad name failed the whole copy; the clash overwrote silently; sync re-sent everything) | auto, lab (funky `vfat_*`) |
| Shared directory deleted, moved or replaced under a running node | The node stops within seconds and says why; a TUI closes and leaves the share (it went on failing every copy with "mkdirat .fkbr-tmp: no such file or directory") | auto, lab (funky `root_deleted`) |
| File over the target's `ulimit -f` | That file reported, the rest arrives | lab (funky) |
| Stdout and stderr closed, 40 open files, odd hostnames | Work | lab (funky) |
| Share entries replaced by symlinks to elsewhere | Pruning deletes nothing outside the share | lab (funky) |
| Files whose stat size is 0 (`/proc`) | Arrive empty, like with tar and rsync | lab (funky) |

## 7. Mixed fleet

| Case | Expected | Status |
|---|---|---|
| CentOS 7 (kernel 3.10) to Rocky 10, x86-64 and ARM64 | Same results everywhere | lab |
| Nodes on another protocol version on the same share | Refused, with what to do | auto |
| Mixed builds of the same protocol | Work together; older builds lack newer checks | lab (f9ac962 with the robust-test fixes, 2026-10-21) |

## 8. Robust test (2026-10-21)

Five hours on the lab with everything at once: a bad network (netem
delay, loss, reorder, 10 Mbit, NAS cuts, route flapping), process chaos
and clock skew, odd data on FAT, exFAT and casefold ext4, 45-job storms,
and a vandalised share. About 2,100 trees were checked against their
sources: no file content was ever lost or corrupted. It found 17 problems;
each now has a test (`internal/engine/robust_test.go` unless named) and
was checked again on the lab with new-build nodes.

| Found | Fixed | Test |
|---|---|---|
| A path over 4096 bytes failed the whole copy | Bound 64 KiB, longer ones skipped and reported | `TestPathLongerThanPathMax` |
| Sync lost names that are not valid UTF-8 (two failed the plan, a mirror "deleted" one without doing it) | Lossless form in snapshots and requests (`api.EncodeName`) | `TestNamesNotUTF8`, `TestNamesSurviveJSON` |
| A target in two job groups kept install times on folders | Each group carries the folders it installs into | `TestSyncTwoGroupsFolderMtimes` |
| Mtimes the target cannot store (XFS without bigtime) clamped silently, synced again every run | Targets probe their range; the plan compares within it; the receiver says so | `TestPlanClampedMtimes`, `TestMtimeNotStoredNoted` |
| A busy node on a slow link looked offline | Bulk writes synced every ~2 s of data | `TestBulkWritesSyncOnTheWay`; lab: 0 of 36 samples offline at 10 Mbit (before 11 of 36, 61 s) |
| A restart right after SIGTERM exited, node down | New worker waits for a stopping one | `TestRestartWaitsForStoppingWorker`; lab |
| SIGTERM did not stop a receiving job | Stops between chunks and entries | `TestStoppingTargetLetsGoBetweenChunks` |
| Job times and IDs from a skewed host's clock | Share clock | `TestNowIsTheSharesClock`; lab (+2 h) |
| FAT/exFAT stored `dir.` as `dir` | Refused | `TestTrailingDotStrippedRefused` |
| exFAT: one non-UTF-8 name failed the copy | Refused like other names | `TestRefusedNameRemovalFails` |
| Syncs to FAT/exFAT/casefold never came out empty | Plan leaves out what the target cannot store | `TestPlanNamesTheTargetCannotStore` |
| `-exclude cache/` did not keep `CACHE` on a case-insensitive target | Excludes match case-insensitively there | `TestWithKey` |
| `prune -keep 0` broke running syncs and left orphan job folders | Floors, plans in use kept, orphans pruned | `TestPruneFloors`, `TestPruneKeepsPlansInUse` |
| Jobs waited forever for a gone target and held the source's slot | Offline last target detached; gone targets end the job | `TestOfflineLastTargetDetached`, `TestTargetGoneAfterSourceFinished` |
| A damaged `info.json` hid a running node for good | Heartbeat re-registers; `forget` removes such an entry | `TestDamagedRegistryEntry` |
| A CLI without a running node created folders on the share | Only a registered node creates its folders | `TestVisitorLeavesNoFolders` |
| One unwritable inbox failed the whole fan-out | Notify retried; others carry on | `TestNotifyFailureIsNotFatal` |

A second, 90-minute run on the fixed build (da7f98f) checked the 17 again
(none came back) and found 4 more, fixed the same way:

| Found | Fixed | Test |
|---|---|---|
| A sync's target scan changed the target folder's mtime (the mtime probe above; even on `sync -n`) | The probe puts the folder's times back | `TestMtimeRange` |
| A source back from a NAS stall detached a target that had already said done; the job ended partial | A done target counts as finished, whatever its acks | `TestDoneTargetNotDetached` |
| A target restarted while it was being detached kept the job's staging for good | Swept at every prune | `TestStaleStagingSwept` |
| A job whose source was forgotten stayed `running` though every target was done | Shown done (or failed) | `TestSourceGoneAfterTargetsFinished` |
| A registry entry damaged between a worker's Register and its first heartbeat stopped the worker (found by `-race`) | Registers again, as the heartbeat loop does | `TestEntryLostAtStart` |
| A copy of a source path that does not exist ended `done` with one error (seen in the regression pass; asked for) | Refused at submit on the source, or the source fails the job before sending | `TestMissingSourcePathFails` |

## 9. Security audit (2026-10-22)

Who may do what: anyone who can write to the share can read and change
what every node shares. That is by design (the share is where the trust
is; see protocol.md, "Write policy"). The audit looked for ways past
that: from the share onto a node's host, and from another local user.

| Found | Fixed | Test |
|---|---|---|
| Another local user could make the state dir in `/tmp` first (its name is easy to guess); the node then wrote its lock, log and socket in their folder, following symlinks (shown on the Pi: a planted lock link made the node overwrite a file of the user) | The state dir must be the user's, not writable by others, or the node, CLI and `check` refuse it; lock and log are opened without following symlinks | `TestPrivateDir`, `TestWorkerLockRefusesPlantedStateDir` |
| Node names and IDs in requests and job records were not checked, so a forged `../` reached outside fkbr's folder on the share (reply files, ack folders) | Checked when read; bad job records are pruned | `TestForgedNamesStayOnTheShare` |
| A sync scan probed (wrote a file in) the folder it was asked for before checking the path: `../x`, or a symlink out of the root, led it elsewhere | Path checked first; probes only in a folder inside the root | `TestScanProbeStaysInRoot` |
| A huge file on the share (a request, job record or status) was read whole and could make a node run out of memory | Files over 64 MiB are refused | `TestShareReadsAreCapped` |
| A binary or `fkbr.yaml` kept on the share could be changed by anyone who can write there | Docs: keep both on local disk (operations.md, FAQ) | – |

Each test fails with its fix taken out.

## 10. Over ssh

Hosts linked over ssh instead of a share (`ssh.md`). Every case of
sections 1 to 8 that `go test` covers also runs with the nodes linked:
`FKBR_TEST_TRANSPORT=ssh go test ./internal/engine` (tests that inject
faults into a share directory's files are skipped there).

| Case | Expected | Status |
|---|---|---|
| The engine suite with every node but the first linked over a pipe | As on a share | auto (with `-race`) |
| A record write from a stopping worker (cancelled context) | Done, as on a share (it was cancelled: a restarted node failed to register) | auto (`TestLeaveOnStop` over links) |
| Link protocol: big and many streams, a stalled stream beside a busy one, banners, resets, a silent peer, bad frames | Each stream gets its data; the stalled one does not hold up the others; bad input ends the session | auto (`internal/sshlink/frame`), fuzzed (`FuzzSession`) |
| Home node and two hosts through a stand-in ssh: copies both ways and between the two hosts, a mirror sync, a clean stop | Files arrive; the hosts leave the swarm when the home node stops; a login banner is skipped and logged | auto (`TestSSHSwarm`) |
| The target's fkbr killed with -9 mid-transfer | Session started again, the job resumes and is done | auto (`TestSSHTargetKilledMidTransfer`) |
| A host without fkbr | Link state says why; the home node keeps trying | auto (`TestSSHHostWithoutFkbr`) |
| A private share (home node's records, staged chunks) | 0700/0600 whatever the umask | auto (`TestPrivateShareModes`) |
| Real ssh, a `ProxyJump` path, 20 to 50 ms round trips | Works; throughput against `scp` and `rsync -e ssh` | **todo** (lab) |
| A link cut mid-transfer (network, not a kill), both directions; the home node killed | Rides it out or detaches; resumes after | **todo** (lab) |
| x86-64 and ARM64 hosts in one ssh swarm; a host with /tmp in memory | Same results; staging bounded by `job_quota` | **todo** (lab) |

## 11. On its own: root `local`, hosts from the TUI

`fkbr` with no share, root `local` (`/`) that only this host sees, and
ssh hosts linked from the TUI (`operations.md`, "Without a share").

| Case | Expected | Status |
|---|---|---|
| Another node lists, reads, makes a folder in, deletes in, or copies out of a node's local root | Refused as an unknown root; it is not in the registry; the node itself sees it | auto (`TestLocalRootUnseenByPeers`) |
| A job record made up by another node, or a node's own job changed on the share, naming its local root | The node refuses it (no record of starting it / record differs) and sends nothing; its own copy works | auto (`TestLocalRootOnlyOwnJobs`) |
| A copy into the local root over an NFS share; with no share (ssh, on its own) | Refused on NFS (anyone could forge the chunks); done without a share | auto (`TestLocalRootWritesNeedPrivateShare`) |
| A sync from or into the local root | Refused for now | auto (`TestLocalRootUnseenByPeers`) |
| `check` with the local root `/` | Reads it, writes nothing there | manual (cloud session) |
| Hosts kept in the config file: added twice, removed, the owner's comments and keys, a symlink in its place | Each host once; the rest of the file and its mode kept (a new file 0600); never written through the symlink | auto (`TestSavedHosts`) |
| Which config file: `-config`, `$FKBR_CONFIG`, `fkbr.yaml` beside the binary, `~/.fkbr.conf` | Read and written in that order; `~/.fkbr.conf` only when none of the others is there | auto (`TestConfigPathOrder`) |
| No share: a host linked while the node runs, saved, linked again at the next start, unlinked | Online, saved; back on restart; leaves and is forgotten | auto (`TestSSHHostsLinkedAtRuntime`) |
| TUI: panes open in this directory and another node or the local root; a host not up yet shows with why; F7 links, F8 unlinks; on a share F7/F8 in the node list do nothing new | As stated | auto (`TestStartPanesAndLinkRows`, `TestLinkAndUnlinkFromTheNodeList`, `TestNoLinkKeysOnAShare`) |
| The TUI on a real terminal: start without a share, F7 a real host, quit and start again | As above | **todo** (lab) |

## How to run the lab tests

- **Torture** (`torture/`): rounds of random copies and syncs
  between five nodes, with kill -9, SIGSTOP and restarts, verified by
  sha256 manifests. See its README.
- **Weird** (`weird/`): `weird.py [test...]` runs odd cases with
  several nodes on one machine; `nfscut.py target|source` cuts a lab node
  off the NAS mid-transfer; `fuzz_tui.py SECONDS SEED` (with pyte) throws
  random keys and resizes at the TUI.
- **Funky** (`funky/`): `funky.py [test...]` (with pyte; sudo
  for the FAT image and hostnames): how fkbr is started and where, hostile
  names and peers, odd target filesystems and limits.
- **Compare** (`compare/`): `compare.py setup | setup_big |
  run N | runbig | clean` times fkbr, tar, rsync and borg through the
  share and verifies every copy; results go to `docs/benchmarks.md`.
- **Explore** (`explore/`): `explore.py quota | quota2 |
  concurrent | same | dup | queued | clean`, each a focused experiment;
  results append to `results.log`.
