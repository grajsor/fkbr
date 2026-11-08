# Test plan

What could break fkbr, how we test it, and where each test stands. Unit
and engine tests run with `go test ./...` (nodes on temp dirs, one share).
Lab tests need several hosts on one NFS share; their scripts are kept
outside the repo because they hold lab addresses.

Status: **auto** = in `go test`, **lab** = run on the lab and passed,
**todo** = not run yet. `stability.md` sums up the fault runs.

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
| Share full for good: nothing of the job left on it for its targets to free | The job fails after `detach_after` (10 min) and says so: "share full for 10m0s, with nothing of this job left there to free: make room and start the copy again" (it waited until cancelled) | auto (`TestShareFullForGoodFails`) |
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
| 1,000,000 entries copied to 1 and 3 targets | All verified; 2 min 11 s and 4 min 35 s (2026-11-04) | lab (compare) |
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
| A sparse file: 1 GiB with 8 MiB of data, starting and ending in holes | Only the data crosses the share (holes were sent as zeros); the copy is equal and sparse; an older receiver installs it the same | auto (`TestSparseFileHolesNotSent`, `TestSparseExtents`); lab 2026-10-25, rhel9 → CentOS 7 over the NAS: 16.3 s and 13 chunks before, 1.0 s and 2 chunks (8 MiB) after; to an old-build node the same |
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
| A copy of a source path that does not exist ended `done` with one error (seen in the regression pass) | Refused at submit on the source, or the source fails the job before sending | `TestMissingSourcePathFails` |

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
| A binary or `fkbr.conf` kept on the share could be changed by anyone who can write there | Docs: keep both on local disk (operations.md, FAQ) | – |

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
| A private share (home node's records) | 0700/0600 whatever the umask | auto (`TestPrivateShareModes`) |
| Copies from the home node and from a linked node, to the other two, slow targets | No chunk file on any disk (share, state dirs) at any moment | auto (`TestSSHNoChunkFiles`) |
| A source restarted mid-job (home node, linked node): its chunks in memory are gone | It builds them again from the lowest unacked chunk; trees equal | auto (`TestSSHSourceRestart`) |
| A target restarted mid-job (home node, linked node) | It asks for its first unacked chunk, which the source still holds; trees equal | auto (`TestSSHTargetRestart`) |
| A restarted source whose file changed (same size and mtime) | The chunk a target applied comes out different: the job fails, "source changed" | auto (`TestSSHSourceChangedOnRestart`) |
| A chunk gone between its meta and its bytes (the source restarted in between) | The target waits for it again, not a failure | auto (`TestSSHChunkGoneAfterMeta`) |
| A slow target holds the window of a copy 16 times `job_quota` | The source holds at most `job_quota` (and a chunk's last header) in memory | auto (`TestSSHMemoryWithinQuota`) |
| Chunks in memory on real hosts: 1 GiB file to one target against rsync; a link cut mid-chunk; a source killed with -9 mid-job; memory on a 1.7 GiB box with a stopped target | Faster than with chunks on disk (10.6 s, rsync 7.1 s); the job resumes and is done, checksums equal; no swapping, at most `staging_quota` taken | lab 2026-10-23: 9.1 s (rsync 6.6 s); source link killed -9, target link killed -9, source cut off 25 s, the home node as source killed -9 and restarted: each copy done, checksums equal; a target stopped 20 s: source at most 170 MiB RSS (job_quota 128 MiB), no swapping |
| Real ssh, a `ProxyJump` path, 20 to 50 ms round trips | Works; throughput against `scp` and `rsync -e ssh` | **todo** (lab) |
| A link cut mid-transfer: a blackhole route for 60 s, `kill -9` of the home node's ssh | Session started again (45 s, then 2 s); the copy is done, checksums equal | lab 2026-10-23 |
| The home node SIGSTOPped for 60 s, or killed with -9 and started again, mid-copy | The linked nodes stop; with the home node back the job resumes and is done | lab 2026-10-23 (found the lost-end bug in section 11) |
| A copy between two linked hosts | Nothing over 1 MB ever on the home node's disk | lab 2026-10-23 |
| CentOS 7 as the home node; two directories of one host as named nodes (`a=host:/x,b=host:/y`); `-ssh-bin '~/fkbr/fkbr'` | Copies between all of them, checksums equal | lab 2026-10-23 |
| The TUI on an ARM64 host with no hosts; F7 links an x86-64 host | Linked and saved in the config file; copies both ways; F10 stops the remote node | lab 2026-10-23 |
| A source whose /tmp is a tmpfs (1.7 GB RAM, no swap), the target stopped for 40 s mid-copy: tmpfs 300 MB; 1100 MB; 870 MB (half the RAM) with two copies at once | 300 MB and 870 MB: "share full; retrying", waits, done. 1100 MB: staging stops at 897 MB (`job_quota` 1 GiB). Checksums equal, no OOM; at most 900 MB of RAM taken. Again after the fixes below: 870 MB with two copies, no full disk (the senders wait for room instead); with `-job-quota 256M -staging-quota 512M` on the home node, at most 449 MB staged | lab 2026-10-23 |

## 11. Review of the ssh feature (2026-10-22)

A multi-agent code review of the ssh work, then a pass of our own (the
role in a status file name, a quoted `~/` in `ssh_bin`).

| Found | Fixed | Test |
|---|---|---|
| A linked host's names (request, job and source IDs, a status's role) became paths on the home node unchecked: it could remove, write or read files anywhere in the home user's account | Every name a caller sends is checked in the store before it becomes a path | `TestRemoteCallsStayInTheShare` (hostile names in every field of every call) |
| A link cut mid-transfer was noticed only when TCP gave up (about 2 h): the keepalive waited behind the stuck write | Pings and pongs go out on their own; the session ends 45 s after the peer went quiet | `TestDeadLinkWhileWriting` |
| Something ssh left running (a ControlPersist master) holding its output open hung the session, the home node's stop and `check` | The session follows ssh's exit; its output is read 2 s more | `TestLingeringSSHDoesNotHang` |
| `fkbr /mnt/nfs/xfer` with `ssh:` in fkbr.yaml took the NFS share for the swarm's private records and chmodded it to 0700 | A share given on the command line replaces the file's; a file with both is refused | `TestPickShare`, `TestNFSAndSSHBothRefused` |
| A plan applied an hour after its dry run had lost its scans to pruning; every delete failed | A plan's scans are kept as long as the plan | `TestPruneKeepsScansWithTheirPlan` |
| An upload from a linked host that failed halfway was published cut short | The call is reset, not ended | `TestAbortedBlobNotCommitted` |
| Two hosts with one node name could both link | Name checked and claimed in one step | `TestSameNameLinksOnce` |
| A linked host killed mid-job kept its chunks and half-written files | Swept at link-up and with every prune | `TestLinkedNodeSweepsItsChunks` |
| Each batch of 1000 deletes read the target's whole scan again | One cursor reads it forward across an apply's batches | `TestScanReadOnceAcrossBatches` |
| A target on the build before deleted what changed since the dry run, unseen | An apply that deletes there is refused: update it first | `TestRecheckNeedsSupport` |
| `ssh_bin: ~/fkbr/fkbr` from fkbr.yaml reached the remote shell quoted | `~/` is the remote home | `TestRemoteCommandQuoting` |
| Lab ssh test: each linked node logged "fetch failed: context canceled" when the home node stopped, which read like a lost request | A fetch cut off because the worker stops is not logged | `TestStopLogsNoFetchFailure` |
| Lab ssh test: stopping the home node logged "ssh link down" for the nodes it had stopped, and a link that really died was logged only now and then (the check read a context the session's end cancels) | A link the home node ended logs as closed; one that dies always logs as down | `TestStopLogsNoLinkDown` |
| Lab ssh test, the source's side cut for 75 s while the home node relayed its chunk: the target failed at once with "stream reset", though the link came back 30 s later | A call that fails after its answer started resets with the cause's code, so the target sees a link down and waits; the reset no longer depends on a logger being set (without one, a cut-short body read as complete) | `TestFailureAfterAnswerKeepsItsCode` |
| Lab ssh test (2026-10-23), the home node SIGSTOPped for 60 s mid-copy: both linked nodes stopped; the source had failed the job and deleted its chunks, but the "failed" status went over the dead link and was lost, so after the restart the job resumed with its last chunk gone and source and target waited for each other for good. A target that lost its status the same way would resume on empty staging and install only the later chunks | Staged chunks and staging are removed only once the job's end is recorded (tried again for up to 3 min while the worker runs); else the next run resumes from them | `TestSourceEndLostKeepsChunks`, `TestTargetEndLostKeepsStaging` |
| Lab ssh test: "ssh session ended" gave the remote node's last log line ("chunk applied") as the reason | The reason is ssh's last word or the link's error | `TestSessionEndNotALogLine` |
| Lab test: `cd /usr && fkbr ls /local` with no node running listed `/`: in-process, this node is not registered, so neither the shorthand nor the root `local` check applied | This node's own roots are used | `TestResolveRoot` |
| Lab test: F7 with a config file of comments only replaced them | Kept | `TestSaveHostKeepsComments` |
| Lab test, /tmp in memory: a linked host read no config, so its quotas (1 GiB per job, 4 GiB in all) could not be lowered | The home node passes its `chunk_size`, `job_quota` and `staging_quota` (those it changed) to `fkbr link`; also flags `-chunk-size`, `-job-quota`, `-staging-quota` | `TestLinkArgs`, `TestHomePassesQuotas`, `TestSSHHostsLinkedAtRuntime` |
| Lab test, /tmp in memory: a full staging dir was found by a chunk write; each time the sender read the source again from the start (8 times for two copies) | Before each chunk the sender waits until it fits the disk, while some of its chunks are staged or being written; chunks being written hold their space, so two jobs do not both count it (the first lab rerun still hit a full disk 8 times that way) | `TestSmallDiskWaitedFor` (two copies at once), `TestSpaceBook` |
| Lab test: on a linked host a full /tmp was logged as "share full" | It names the staging dir | `TestNoSpaceNamesStagingDir` |
| Benchmark over ssh against rsync (2026-10-23): 200,000 small files took 45 s (rsync 27 s). The receiver truncated every file to its size, and on XFS a truncate writes the file out at once | A file is truncated only when its size is wrong (a hole at the end, a longer file from an earlier run): 29 s, the same fix speeds up targets on a share | `TestTruncateOnlyWhenNeeded` |
| Same benchmark: a 1 GiB file took 15.6 s (rsync 8.0 s). The source synced each 128 MiB chunk into its staging dir, so its disk wrote everything it read | Stores on local disk (home and linked nodes) don't sync; over ssh chunks are 32 MiB and `job_quota` 128 MiB, so chunks are gone before the kernel writes them out: 9.5 s | `TestNoSyncLeavesItToTheCache`, `TestSSHTransferDefaults` |
| Benchmark over ssh (2026-10-23, after the two fixes above): a 1 GiB file to one target took 10.6 s (rsync 7.1 s): the source still wrote each chunk to its staging dir and read it back | Over ssh chunks stay in the source's memory until its targets read them; no chunk file on any disk. A restarted source builds the unacked chunks again and checks the last one each target applied; `staging_quota` over ssh is a quarter of the RAM (at most 512 MiB) and a chunk waits for `MemAvailable` | `TestSSHNoChunkFiles`, `TestSSHSourceRestart`, `TestSSHSourceChangedOnRestart`, `TestSSHChunkGoneAfterMeta`, `TestSSHMemoryWithinQuota`, `TestMemChunks`, `TestSSHTransferDefaults` |
| A job a broken link made fail could only be copied again by hand | `catchup` (F9 `u`) re-sends to failed targets too | `TestCatchUpFailedTarget` |
| The home node forgot a request once it handed it out: a fetch answer lost while the link stayed up (unreadable, or over the 64 MiB line) left it unsent until the next link | Requests stay on the home node until the next fetch acks the answer, else go out again, alone; one that never arrives is parked in failed/ after 20 tries. An answer carries at most 16 MiB; a linked build without acks is refused | `TestLostFetchAnswerSentAgain`, `TestRemoteFetchAcks`, `TestFetchAnswerBounded`, `TestRequestThatNeverArrivesParked`, `TestOlderLinkRefused` |
| Flaky test runs (2026-10-23, about 1 run in 25): a test's share could not be removed ("directory not empty"): a home node's handler still wrote a record for a link the test had closed | A session's `serve` returns only once its calls in progress have; the engine tests' teardown waits for the home node's session | `TestServeWaitsForCalls` |
| Flaky test (1 run in 60, since 2026-10-19): `TestShareHiccupRecovers` locked a node's registry folder, and a worker's first prune (which gives the share's entries their modes, `store.openUp`) opened it again, so the node never went offline | The test waits for each worker's first prune (`Engine.pruned`) | `TestShareHiccupRecovers`, 150 runs in each mode |
| Flaky test runs (2026-10-23, 3 of 181 loop runs, both modes): Submit failed with "chmod jobs/<id>: no such file or directory". A prune took the job folder `CreateJob` had just made, still empty, for an old orphan (an empty folder had no newest entry) and removed it; one that read it empty could also remove the job once written | A job folder with nothing in it yet is judged by its own mtime | `TestPruneKeepsJobBeingCreated` |

Each test fails with its fix taken out.

## 12. On its own: root `local`, hosts from the TUI

`fkbr` with no share, root `local` (`/`) that only this host sees, and
ssh hosts linked from the TUI (`operations.md`, "Without a share").

| Case | Expected | Status |
|---|---|---|
| Another node lists, reads, makes a folder in, deletes in, or copies out of a node's local root | Refused as an unknown root; it is not in the registry; the node itself sees it | auto (`TestLocalRootUnseenByPeers`) |
| A job record made up by another node, or a node's own job changed on the share, naming its local root | The node refuses it (no record of starting it / record differs) and sends nothing; its own copy works | auto (`TestLocalRootOnlyOwnJobs`) |
| A copy into the local root over an NFS share; with no share (ssh, on its own) | Refused on NFS (anyone could forge the chunks); done without a share | auto (`TestLocalRootWritesNeedPrivateShare`) |
| A sync from or into the local root | Refused for now | auto (`TestLocalRootUnseenByPeers`) |
| `check` with the local root `/` | Reads it, writes nothing there | manual (cloud session) |
| Hosts kept in the config file: added twice, removed, comments and other keys in the file, a symlink in its place | Each host once; the rest of the file and its mode kept (a new file 0600); never written through the symlink | auto (`TestSavedHosts`) |
| Which config file: `-config`, `$FKBR_CONFIG`, `fkbr.conf` beside the binary (or `fkbr.yaml`, its older name), `~/.fkbr.conf` | Read and written in that order; `~/.fkbr.conf` only when none of the others is there | auto (`TestConfigPathOrder`) |
| No share: a host linked while the node runs, saved, linked again at the next start, unlinked | Online, saved; back on restart; leaves and is forgotten | auto (`TestSSHHostsLinkedAtRuntime`) |
| TUI: panes open in this directory and another node or the local root; a host not up yet shows with why; F7 links, F8 unlinks; on a share F7/F8 in the node list do nothing new | As stated | auto (`TestStartPanesAndLinkRows`, `TestLinkAndUnlinkFromTheNodeList`, `TestNoLinkKeysOnAShare`) |
| The TUI on a real terminal: start without a share, F7 a real host, quit and start again | As above | **todo** (lab) |

Found by a code review of this part (2026-10-23):

| Found | Fixed | Test |
|---|---|---|
| Root `local` (`/`, writable) was on every node, daemons too: whoever reached the socket (`socket_mode` 0660, a daemon run as root) could read and delete anywhere as that user | Only a TUI or CLI gets it, and only with an owner-only socket; a configured local root with a looser socket is refused | `TestLocalRootOnlyForThisUser` |
| The extra root broke the single-root shorthand: `cp /reports @all:/incoming`, `cp hostb:/logs/app.log /` failed | Root `local` does not count for the shorthand | `TestResolveRoot` |
| A host with no node yet could be marked into `@dest`, failing the whole copy | Marking skips it | `TestLinkRowsNotMarked` |
| F7 on a host linked already (from `ssh:` on the command line) did not save it | It is saved | `TestSSHHostsLinkedAtRuntime` |
| A config file that did not parse stopped fkbr without naming it (`~/.fkbr.conf` is read unasked) | The error names the file | `TestBadFileNamed` |
| The TUI asked the node whether it links hosts on every F7, F8 and F1 (and could act on a nil answer) | Asked once | `TestLinkerAskedOnce` |
| A move between overlapping roots submitted by another node was not checked | The source refuses it | `TestMoveAcrossOverlappingRootsFromElsewhere` |
| A copy on root `local` cut off by a reboot cannot resume (its record is in `/tmp`) | Known; it fails, start it again | (documented) |

Found by a code review of the whole ssh branch (2026-10-23):

| Found | Fixed | Test |
|---|---|---|
| `cd /usr && fkbr`, then `fkbr rm /local/x`: root `local` (`/`) took the path that meant `/usr/local/x` | A path that could mean either is refused; `files/local/x` names the folder | `TestResolveRoot` |
| `fkbr ssh:web01` with a config file that sets `nfs_root`, then F7: the `ssh:` list written beside it made the file unreadable | Not saved there; the host is linked, with the reason | `TestSaveHostBesideShare` |
| Once F7 had written `~/.fkbr.conf`, `$FKBR_SHARE` was ignored: fkbr became an ssh home node | `$FKBR_SHARE` wins over the file's `ssh:` list | `TestPickShare` |
| With `socket_mode` open to others, anyone reaching the socket could make the node ssh anywhere with the user's ssh keys | Linking at runtime (F7, F8, the socket) is refused on such a node | `TestNoLinksOverAnOpenSocket` |
| A linked host's node was always named after its short hostname: two directories on one host, or two hosts of one short name, could not both link | `NAME=host[:path]` names the node | `TestParseHosts`, `TestSSHHostsLinkedAtRuntime` |
| A remote that stayed up but never started `fkbr link` left the host "connecting" for good | The start is given up after 45 s and tried again | `TestSilentStartGivenUp` |
| A linked node's 10 ms poll also paced its jobs' waits, each a round trip to the home node | Only asking for requests is that quick | `TestLinkPolls` |
| A CLI attached to a daemon always said "using the running node X, which shares ...", because of root `local` | Root `local` is not compared | `TestSameRootsLeavesOutLocal` |
| The TUI asked a node on a share for its links on every node list refresh | Asked once | `TestNotHomeAskedOnce` |
| Two `firstLine` helpers that differed on long lines | One | (none) |
| A pane listing a node's roots (`files`, `local`) put its cursor back on the top row at every node refresh, a few seconds apart | The cursor stays on its row | `TestRootsRefreshKeepsCursor` |
| Backspace at the top of this host's shared folder showed a list of two roots instead of going on up one tree | It goes on up into root `local` at the folder above, and from `/` to the node list; a shared folder outside `local` keeps the roots list | `TestUpFromSharedIntoLocal` |
| Editing a file (F3, Ctrl+E): nano's keys, cut and paste of whole lines, undo and redo, find, a click places the cursor; finding in the viewer and the help | Saves only over the file as it was read; undo always gets back to the file as opened; cut lines go back whole above the cursor's line (they split a line when pasted at the cursor) | `TestEditorSaves`, `TestEditorCutPaste`, `TestEditorPasteWholeLines`, `TestEditorUndo`, `TestEditorUndoToStart`, `TestEditorFind`, `TestEditorCtrlX`, `TestEditorClick`, `TestViewerFind`, `TestThemePaintsEveryCell` |
| Settings (F11): the config file kept by the TUI | Only the keys changed are written, comments, the ssh list and the mode kept; a default removes its key; a value or a file the node could not start with is refused and named; Esc with changes asks, answered with Ctrl keys only; colours, theme and mouse show while chosen and ^X puts the old look back, the TUI's own apply when saved, unless a flag set them; five themes, each painting every cell at every colour depth | `TestSaveSettings`, `TestSaveSettingsRefused`, `TestSettingsDialog`, `TestSettingsDialogAsks`, `TestSettingsClick`, `TestSettingsTheme`, `TestSettingsNumbers`, `TestThemesComplete`, `TestThemePaintsEveryCell` |

## 13. Cloud test run (2026-10-24)

A 4 h run on one machine (no NFS: a plain directory as the share; ssh to
a local sshd): copies of one 1 GiB file, 200,000 small files and a mixed
set, syncs, kills, stops and edge cases, each 3 times, over a share and
over ssh, and 329 rounds of the ssh-mode engine tests under `-race`. No
data bug: every target matched its source.

| Found | Now | Test |
|---|---|---|
| Ssh-mode engine tests under `-race` failed now and then ("nothing from the other side for 300ms"): the whole process got no CPU for half a second, and the keepalive, waking before the readers that held fresh frames, gave up live links | Test links wait 5 s; a keepalive whose own tick came late (over 2 pings) waits one more ping | `TestLateTickHoldsVerdict` |
| A daemon started with its socket in a folder others may write to (`/tmp`); every CLI command then refused it and named `-state` | The node refuses to start, naming the socket and how to move it | `TestStartNodeRefusesOpenSocketDir`, `TestCheckSocketDir` |
| `scripts/bench.sh` put its sockets in `/tmp` and stopped before any benchmark | Sockets in a private folder of its own | (the script runs) |
| A source killed over a share left the chunk it was writing in `tmp/` for an hour (up to 134 MB each) | Removed when it writes that chunk again, and when the job ends | `TestKilledSourceTempChunksRemoved` |
| setuid/setgid left off (no `allow_setid`) without a word in the job | Noted in the job | `TestRoundTripMetadata` |
| Symlinks got the time they were made on the target | They keep the source's mtime, like `rsync -a` | `TestRoundTripMetadata` |
| `cp /x @all` (no colon), or `cp /x hostb` with `hostb` a node, copied into a folder of that name here (found on the lab) | Refused for cp, mv and sync, naming `@all:/dir`; `/hostb` is still a folder here | `TestDestinationNamesNodes` |

## 14. Torture v2 and edge tests (2026-10-24/25)

The lab torture test was rebuilt to hunt rather than repeat. An NFS swarm
and an ssh swarm run jobs at once, with small random limits (1-4 MiB
chunks, quotas of a few chunks, `max_jobs` 1-2). Up to 3 faults overlap:

- node, home node, ssh client and link kills and stops;
- NAS and link cuts, netem;
- clock jumps of up to 2 min, memory and CPU squeezes, a full target disk.

Jobs change their source while it is read, have their target vandalised
(deleted, swapped for a symlink out of the root, chmod 000), get cancelled,
collide in one folder and move. After any trouble, one mirror sync with the
faults off must make every target exact.

Run 1 (12 rounds, 217 jobs, 172 faults), run 2 (21 rounds, 377 jobs, 314
faults) and the edge tests: every target
matched or converged, and nothing was written outside a root. Corrupted
staged chunks were refused, never installed. Files made on a target after
a plan survived applying it. A source folder swapped for a symlink to
`/etc` mid-copy brought nothing from `/etc`. Paths past PATH_MAX and
100,000 files in one folder (peak 58 MiB RSS) copied exact. 40 CLI calls
at once were all answered, and 20 queued jobs into `max_jobs` 1 all ended
exact. Run 4, on e79d440 with the four fixes below (12 rounds, 216 jobs,
178 faults): nothing found.

| Found | Now | Test |
|---|---|---|
| Over ssh, a link cut for 30-45 s failed the copy ("context deadline exceeded"): a record call timed out (30 s) before the keepalive called the link dead (45 s), and a bare deadline is not an error transfers ride out | A record call the home node does not answer in time is a link down (EIO), which transfers ride out; lab: cuts of 15-90 s, target on the home node or not, all done | `TestUnansweredRecordCallIsLinkDown` |
| A cancel over ssh could end a target failed ("rename … acks/<node>: no such file or directory"): the source removed the job's data while the target wrote its ack | An ack whose folder is gone means the job's data was removed; the target then asks the job and ends cancelled; lab: 20 cancels over ssh, none failed | `TestCancelDuringAck` |
| Torture run 2: a target over ssh gave up "after 3m0s" of I/O errors though the link had been back for minutes: the 3 min budget is for consecutive errors, but a successful "chunk not there yet" did not end the run, so two drops 3 min apart counted as one (the source had the same: only a finished walk ended it) | A store answer, or a chunk staged since the last error, ends a run of errors; an outage that lasts still gives up | `TestTargetBudgetIsForConsecutiveErrors`, `TestSourceBudgetIsForConsecutiveErrors`, `TestIOErrorsGiveUp` |
| A cancel that removed a chunk after the target had read its meta (the source removes chunks before metas) ended that target failed ("chunk 1: open …: no such file or directory"; seen once in `TestCancelDuringAck`) | The target asks the job and ends cancelled | `TestCancelBetweenMetaAndChunk` |
| Torture run 3: `sync` exited 0 with nothing copied when its transfer could not be submitted (a target offline at the apply); the reason was in the plan but never shown, and the TUI said "0 transfer jobs submitted" | `sync` prints the problem and fails ("1 of 2 transfers not submitted"); the TUI shows it as an error | `TestSyncTransferNotSubmitted` (cli, tui) |

## 15. Review of file saving and sparse files (2026-10-25)

A review of the day's new code: saving a file through `api.Write` (a peer
replaces a file's content) and sending sparse files as their data only.
All five fixed, each with a test, and checked on the lab against the
build before.

| Found | Now | Test |
|---|---|---|
| A save kept setuid/setgid (a copy strips them unless `allow_setid`): any node the writers policy allows could replace a small setuid-root program and keep the bit | Removed unless `allow_setid`, and the save says so; lab: a setuid-root file saved on a node running as root came out `rwxr-xr-x` | `TestWriteDropsSetid` |
| The checks ran on the path as written, the write followed symlinks: a link into `.fkbr-tmp` or the share was written through | The file is opened once and the checks run on where it really is; lab: "notes leads to .fkbr-tmp/job/x … staging area", the staged file untouched | `TestWriteThroughSymlinkChecked` |
| "Unchanged since it was read" was size and mtime from a stat: on a root on NFS (`actimeo=1800`) the stat could be 30 min old, and two edits of one length in one mtime tick passed | The node checks the content's xxh3 through the opened file (opening fetches fresh attributes); lab: a file changed on rhel8 and saved from rhel9: the old build overwrote it, the new one says "changed since it was read" | `TestWriteChecksContent` |
| A save's new file had one name per process (two saves of one file collided), a crash left it in the user's folder, the folder was not synced, and a save in place said nothing | Random name, noted in the node's state dir and removed at the next start if a crash left it; folder synced; a save in place says why and that a crash could leave it mixed | `TestWriteLeftoverSwept`, `TestWriteFileInPlace` |
| Which parts of a sparse file were sent followed the disk layout, so a resumed source whose file was laid out differently (writeback, preallocation, a defrag) failed: "source changed since the interrupted run". Preallocated zeros were sent; btrfs/ZFS compressed files walked their extents for nothing | Zero blocks (64 KiB) are left out by content, the layout only spares reading holes; lab (Pi, ext4): an interrupted copy of a sparse file, zeros written into a hole meanwhile (content and mtime the same): the old build failed, the new one resumed, the copy equal | `TestSparseOutputFollowsContent`, `TestSparseExtents`, `TestSparseFileHolesNotSent` |

## 16. Second review of the share's boundaries (2026-10-25)

Another review of the code found what the 2026-10-22 audit had not
looked at: the share's own folders. Four of its five findings are fixed,
each with a test that fails on the build before; the fifth (staged bytes
counted by listing every chunk, slow only with 1 MiB chunks) is left.

| Found | Now | Test |
|---|---|---|
| The store worked on plain paths below the share, and took any entry for its folder: a share writer who put a symlink to a home directory in place of `reply/<node>` had that node's prune delete it, recursively; `tmp/` the same for old files, and opening up chmodded through links. Lab: the build before deleted every file in the test folder | Every file operation stays inside the share (`openat2` `RESOLVE_BENEATH`, or `os.Root` where the kernel has no openat2); a link in place of a layout folder is replaced by a real folder at the next prune or heartbeat. Lab: the test folder untouched, `reply/<node>` a folder again. Speed over the NAS the same as before (small files 5.56 s both, 1 GiB 19.2 s against 20.1 s; NFS calls 2,579 against 2,565) | `TestLayoutSymlinkStaysInside` (both ways), `TestLayoutSymlinkHealed` |
| Every start opened up (0777/0644) everything the user owned in the share directory, whatever it was; `/` or the home directory was taken as a share (`cd /tmp && fkbr ~`) | Only fkbr's own entries are opened up, never through a link; `/`, the home directory, the binary's directory and a directory with other things in it that is no share yet are refused, naming the reason. Lab: both refused, nothing changed | `TestShareDirRefused`, `TestOpenUpOnlyLayout` |
| A chunk from the share had no budget: a small zstd frame could unpack to terabytes, a chunk hold millions of empty entries, a file claim 4 EiB; the meta's list of files to drop applied before the checksum | At most 2 GiB stored or unpacked, 16 million entries (the writer starts a new chunk before), files 256 TiB, `chunk_size` at most 1 GiB; a meta claiming more fails at once; the drop list counts after the checksum | `TestChunkBudgets`, `TestChunkMetaOverBudget` |
| The receive journal of files to leave out kept one raw name a line: a name with a newline came back as two names after a restart | JSON lines (an older journal still reads) | `TestBadJournalNames` |

## 17. Once-over (2026-10-30)

A read of the code with gopls's checks, when the themes were added.

| Found | Now | Test |
|---|---|---|
| The home node read an ssh session's stderr by lines of at most 1 MiB and stopped at a longer one; nothing read the pipe after that, so the remote node blocked on its next log line once it filled | Lines longer than 64 KiB are logged cut short and the rest read to the end | `TestStderrLongLine` |
| The settings dialog's "save the changes?" took `y` and `n`: a letter typed on after Esc answered it (the editor's question had been changed for this) | Ctrl+S saves, Ctrl+X throws the changes away, Esc goes back; letters do nothing | `TestSettingsDialogAsks` |
| A bad `-theme` or `-colors` was refused only after the node had started (and joined the share) | Refused before anything starts | `TestSetColors` |
| The settings dialog said its appearance rows applied at once, but colours, theme and mouse changed only on save | They show as they are picked, Ctrl+X on leaving puts the old look back; the copy rows say "applies when saved" | `TestSettingsTheme` |
| In 24-bit colour a terminal showed the blue theme's folders on green and the yellow theme's on red: bold plus a 24-bit foreground and background is one SGR of 11 parameters, and it keeps 10 (the 11th overwrote the 10th, so the background lost its blue). Every theme was off; in green, red and black it barely showed | No SGR sequence the TUI writes has more than 5 parameters; a colour stays whole in one | `TestShortSGR`, `TestThemePaintsEveryCell` |
| Colours `auto` in F11 kept the depth set last: after `none` the screen stayed without colour, after `true` it stayed 24-bit | The terminal's depth is read once at start, before any choice; `auto` goes back to it | `TestSetColors` |
| Over ssh, a handler that failed while the call was still being sent (or before its answer was read) reached the caller as a bare reset: a relayed link down failed the job instead of being ridden out (6 h run, under load) | `open` and `answer` read a coded reset back as `Read` does: a link down stays EIO | `TestFailureWhileSendingKeepsItsCode`, `TestFailureAfterAnswerKeepsItsCode` |
| FuzzExclude: the test took `*/ /` for the name `*` (it trimmed spaces and slashes at once); it is the path `*/ ` | The test reads a pattern as `New` does; the input kept as a regression case | `FuzzExclude` |
| A node starting while a CLI polled it (same state dir) exited with "bind: no such file or directory": the CLI removes a state dir it made once it is empty, and that landed between the node making the dir and listening in it (lab runs, twice under load) | The node makes the dir again and listens; once the socket is in it, the dir stays | `TestListenAfterStateDirRemoved` |
| Settings: a number out of range said only "a number from 1", and `0` for the sync deletion limit "a number from -1" | The range is named; where `-1` is taken, so is that | `TestSettingsNumbers` |

## 18. Reboots and power cuts (2026-10-31)

Every fault before this one left the kernel's page cache alive: a killed
process's writes still reach the disk. Reboots (`reboot.py`) are clean,
so they do too. A power cut does not: the lab boxes are reset with sysrq
`b` (no sync, no unmount) at a chosen moment of a job (`powercut.py`).
After the cut, a job that ended done must leave its target exact,
checked by sha256, mode and mtime. A job cut short must resume, or
detach and catch up, never end done with a file missing. A `mv` must
not lose a file.

| Case | Before (6475202) | Now |
|---|---|---|
| Target cut 0, 2 and 10 s after its part was done | The folder gone (twice), or 1,181 of 1,538 files wrong (196 empty or zeros); the target had said done | Exact |
| `mv`, the target cut as soon as it returned (the sources deleted then) | (no result: harness) | Nothing lost |
| A lone target cut mid-copy (never detached) | Done with 3 files missing and one corrupt; a sync did not repair it (same size and mtime) | Done, exact |
| A target cut while installing 12,035 files | Done with 6,074 missing and 3,972 wrong | Done, exact |
| Over ssh: a linked target cut after done, and mid-copy; the home node cut mid-copy | (no result: harness) | Exact; the links end with their home; the cut job goes on and ends exact |
| Target, target 60 s late, source and ssh hosts rebooted; NFS remounted (`reboot.py`) | Done, exact | Done, exact |

| Found | Now | Test |
|---|---|---|
| A target never synced what it received: it acked chunks only in the page cache (the source then deletes them) and said done before its install was on disk. A cut lost data the job called done, and a `mv` had deleted the sources by then | One `syncfs(2)` on the root's filesystem before each ack, while the next chunk is applied, and one in `Commit` before the staging goes. Cost on the lab, against builds that did not wait, the same day (`benchmarks.md`): through the NAS nothing measurable for big files, 5–20% for small ones; over ssh 10–25% | `TestAckAfterStagingSynced`, `TestStagingSyncFailsOnce`, `TestCommitSyncs` |
| A TUI could live on after SIGTERM (one fuzzed TUI for 18 h; caught again in this run, with its goroutines): main cancels the TUI's context on SIGTERM and bubbletea's own handler took the same signal, sent its quit message to an event loop the cancel had ended, and its shutdown waited on that for good | bubbletea's signal handler is off; main's handles SIGINT, SIGTERM and SIGHUP | `TestQuitsOnSIGTERM` (hung in round 16 of 100 before) |

## 19. Benchmarks and every suite again (2026-11-04)

Every benchmark (`benchmarks.md`), the Go suites (also over ssh, under
`-race` on the Rocky 10 VM, the model 400 rounds, both fuzzers), then a
mix of every lab suite for 3 h 38 min on 5225f98: power cuts, reboots,
edge tests, NAS cuts, explore, ssh faults, cut and cancel repros,
torture v2, and on the Pi weird, funky, both TUI fuzzers, the Go suites
and fuzzers in six cycles.

| Suite | Result |
|---|---|
| Power cuts (10 cases) | 9 exact; the ssh home cut mid-copy found the hang below |
| Reboots (7 cases) | 14 checks exact, 0 findings |
| Edge tests (13) | 42 trees exact, 3 converged, 0 findings |
| NAS cut of a target and of a source | As designed: the cut target detached, nothing installed; the rest exact |
| Explore (quota, concurrent, same files, duplicate worker, queued) | All clean |
| ssh faults (5 cases) | All ok, every fault hit |
| Link cut at 9 moments, cancel at 20 | Every job done; every cancel clean |
| Torture v2 (seed 4101, 1 h 29 min) | 184 jobs, 358 trees exact and 106 made exact by one sync, 148 faults, 0 fkbr findings (one harness false alarm, below) |
| Pi, 6 cycles | weird, funky, `go test`, ssh mode, model: all pass; TUI fuzzers 212,246 keys and 136,162 mouse events, always alive; Go fuzzers about 87 million inputs |
| `-race` (Rocky 10 VM) | All pass except `internal/bundle`, killed for memory (1.7 GB): `TestChunkSizesRamp` and `TestSparseExtents` (a 1 GiB all-zero file kept twice in memory, which `-race` counts); both pass without `-race` |

| Found | Now | Test |
|---|---|---|
| A linked node could hang for good when its ssh home lost power while the link wrote to it. sshd's pipes are blocking; the dead home's sshd stopped reading (its ssh window full, and with the home's kernel gone nothing ever came back to reset the connection), and the link's write stayed in the kernel. The link saw the home gone after 45 s and began to stop, but its other writers waited behind the stuck one, and the stop waited for them. It kept its lock, so the restarted home could not link that host again (22 min, until killed) | `fkbr link` moves stdin and stdout to descriptors of its own in non-blocking mode (fd 0 and 1 then point at /dev/null), so Go's poller runs them and closing the link ends a write in progress. And once the link has ended, a stop that takes more than 1 min ends the process | `TestLinkStdoutCloseEndsStuckWrite` (the write stuck before), `TestLinkStdioLeavesDevNull`, `TestStopWithinGivesUp`; powercut.py again on the fix: all 10 cases exact, the ssh home cut 4 times, no link left behind |
| (Harness) A catch-up into torture's 200 MiB root failed with no fault on and was counted as a finding; the sync that followed into the same root failed with "no space left": that round's copies had filled it. The target's error was not logged, so the catch-up's own reason is unproven | `torture.py` logs the target's error, and notes a catch-up into the small root that failed for space, as it already did for convergence syncs | |

## 20. A second model's review (2026-11-05)

Another model read the code and sent ten suggestions, then a second pass
on the answer. Each point was checked against the code. Six are fixed,
each with a test that fails on the build before; the others are left, with
the reasons below.

| Found | Now | Test |
|---|---|---|
| The share's directory descriptor was copied, the lock let go, and used a moment later; a reopen (remount, share made again) closed it in between: the call failed with EBADF, or, the number reused by another folder, looked its names up there (reproduced: it read another folder's file). A first fix that kept the last four old descriptors open was still open to it after five reopens | `openBeneath` takes a duplicate of the descriptor under the lock and closes it after the call; a reopen closes the old ones at once (`os.Root` counts its users) | `TestReopenDuringOperation` (1, 5 and 12 reopens inside one call) |
| A store opened for a moment was never closed: the home node's store for a linked node was left to the garbage collector when the link ended, after a refused duplicate hello, and after a refused protocol; so was `fkbr check`'s | `Dir.Close` (idempotent), called at each of those | `TestCloseReleasesDescriptors`, `TestDirCloseAndFailedOpen`, `TestLinkChurnLeavesNoDescriptors` (60 descriptors left by 15 rounds before; the GC is off in the test) |
| A panic in a handler of the ssh link ended the whole process, all links and jobs | A call whose handler panics fails alone, with the stack in the log; the link and the other calls go on. Only the handler is covered, not the frame loops. Jobs are not wrapped: a panic there still stops the node, which resumes on restart | `TestHandlerPanicFailsOnlyItsCall` |
| A negative `max_age` (a linked host's `prune`, `job.prune`, `blob.prune`, or the store's own) put the cutoff in the future: `PruneJobs` deleted a finished job past its one-minute floor, `Prune` the node's failed and reply records. Only the blob prune and the orphan-folder case had a floor | All three prunes refuse a negative age; zero stays a policy (`-keep 0`) | `TestPruneRefusesNegativeAge`, `TestRemotePruneNegativeAge` |
| A sync plan whose lists could not be published (`planSink.commit` failed) left the writers open: a temp file in `tmp/` until it was collected, over ssh a call to the home node waiting for a body | `commit` aborts every writer when it fails | `TestPlanSinkCommitFailureAbortsWriters` |
| Over ssh a blob writer whose context ended before anything was written kept its call to the home node waiting on the pipe: resetting the stream does not wake the copy | The context closes the pipe | `TestBlobWriterFreedWhenContextEnds` |

Left as they are:

- Admission control per link (a limit on calls in progress, a deadline for
  the first line) and a size or quota for blobs a linked host sends: the
  swarm is one trust domain (ssh.md "Trust"), so these bound accident, not
  attack. A limit needs a number that no legitimate plan or scan exceeds.
- `recover` around a whole job: a panic there is a crash and the job
  resumes on restart, by design.
- HTTP server timeouts and a connection cap on the API socket (local
  clients; the socket is 0600 in a private directory by default).
- Closing the API's old listener when the socket is made again, and the
  `Lstat`, `Dial`, `Remove`, `Listen` window in `Listen`: one idle
  listener per removal, a private directory. Doable with
  `SetUnlinkOnClose(false)`; not worth the complexity.
- `workerLock.take` recurses without a bound if the lock path is replaced
  between its two syscalls every time; a bounded loop would do.

## 21. Third pass of the second model's review (2026-11-05)

A third pass went after paths the first two had not: syncs from a node to
itself, links inside a root, destinations on other filesystems, the prune
layout. Eight of its ten findings were real and reproduced (the review's
own reproductions, then ours); each is fixed with a test that fails on the
build before. Why none of the earlier runs met them: every sync went from
one node to another, every destination was on the root's own filesystem,
and the odd filesystems we tried were whole roots, never a mount below
one.

| Found | Now | Test |
|---|---|---|
| A sync from a node to itself (a folder into another, or between two roots) planned nothing and applied nothing: the source's scan and the target's were written to one blob, `scans/<plan>/<node>.snap`, and the planner read that as both sides | The source scan of a sync that has the node among its targets is written to `<node>.src.snap` (the request's `name`, checked on the node: only its own `.src`); targets keep `<node>.snap`, which `scannedBelow` and older nodes read. An older node as such a source gives an error (blob not found) where it gave a silent no-op | `TestSyncOnOneNode` (same root, other root, mirror delete, second plan empty), `TestSyncOnOneNodeWithExcludes`, `TestSyncOnOneNodeAndAnother` |
| A destination below a mount point (a drive under the root, a tmpfs, a bind mount): staging is at the root's top, rename across filesystems is `EXDEV`, and the job ended `done` with an error for every file (real tmpfs: 2 files and 7 errors of 9) | On `EXDEV` the staged file or link is copied into a temporary file in the destination's folder (holes stay holes; mode, mtime, owner as staged; fsync), renamed into place, and the staged one removed; the destination's filesystem is synced (`Receiver.Sync`) too. A staged file already given to another owner needs DAC override to be read back; without it the copy reports the error. A copy that is cut leaves `.fkbr-xdev-<job>-<hash>` in that folder | `TestInstallAcrossFilesystems` (EXDEV simulated, new and existing folder, same result as a rename), `TestCopyAcrossKeepsHoles`, `TestCopyToAnotherFilesystem` (a real tmpfs; `FKBR_TEST_MOUNT=1`, sudo) |
| `Hide` (the share inside a root) was checked on the path as written: `link/x` with `link -> box` listed, read, wrote, made folders in and removed from the share. A remove through one deleted the share's `info.json` | `localfs.Real` follows the links inside the root (in every root since section 22; the read path walks only where there is a share); lookup, list, open, mkdir, remove and the destination of a copy use where the path leads. Links themselves stay entries | `TestHideFollowsSymlinks`, `TestShareThroughSymlink` |
| A relative link in place of `jobs`, `blobs` or `data` to another folder of the share made `PruneBlobs`, `PruneJobs` and the orphan sweep delete what is in that folder (the share only refuses links that leave it) | `jobs`, `data` and `blobs` are layout folders: made again if a link or file stands there, and `PruneJobs` and `PruneBlobs` do it first | `TestPruneThroughLinkToSiblingFolder` |
| A target that holds `readme` while the source has `README` (it sorts first): the plan sent `README`, the receiver found the name taken, and no sync ever settled. Both spellings at the source with `readme` at the target the same | A target that folds names gets a pass over its snapshot and one over the source's before the merge (`Target.Again`, `PlanOptions.SrcAgain`); source names that fold onto another spelling the target keeps (a mirror keeps it only if the source has it too) are left out and reported | `TestPlanFoldedTargetNamesAhead` |
| `NameRules.Key` lowercased: `straße`/`STRASSE` and `Σ`/`ς` are one name on a casefold ext4 (checked on one) but two keys | Casefold uses Unicode full case folding (`cases.Fold`). vfat stays lowercasing: on a vfat mount `σ` and `Σ` are one name and `Σ` and `ς` two (checked). exFAT could not be mounted on this kernel, so it is unchecked and unchanged | `TestNameRulesKey` |
| A snapshot or plan list off the share was not checked: `../outside` read back as such | A snapshot entry must be a clean relative path of a known type; a path list line must pass `CleanRel` | `TestReaderRejectsBadEntries`, `TestReadPathListRefusesBadPaths` |
| The temporary chmod of a read-only folder (receive, remove) restored `Perm()` only: setgid and sticky were lost | The whole mode is restored (`localfs.ModeBits`) | `TestReadOnlyDirKeepsSetgidAndSticky`, `TestRemoveKeepsParentSetgidAndSticky` |
| The first 256 events of a subscriber's snapshot were kept, the rest dropped and counted as seen, so never sent | The channel is as big as the snapshot plus 256 | `TestSubscribeSnapshotIsComplete` (255 of 300 before) |
| (Notes of the review) The map of refreshed directories in a long-running NFS node grew for ever; `protocol.md` said a plan is applied "at most once" where the code says two nodes in one instant can both pass | The map forgets directories untouched for a minute past 1024; the document says what the code does | `TestRefresherForgetsOldDirectories` |

Left as decisions, not defects: the textual overlap checks of a sync or a
move on one node with overlapping roots, or two nodes exposing the same
directory (finding 9), and the admission limits of section 20 (finding 10).
Harness: `torture.py` treats the staging a deliberately failed job leaves
(edge `ro`, `inodes`) as a note for 10 min (a prune sweeps it), not as a
finding.

## 22. Fourth pass of the second model's review (2026-11-06)

The review of the third pass's report (`RESULT.md`) found the `Hide` fix
only half done and three smaller defects. Each was reproduced first (the
staging one with throwaway tests, three ways), and each is fixed with a test
that fails on the build before.

| Found | Now | Test |
|---|---|---|
| `localfs.Real` returned the path as written in a root without a share, so the staging check (`writablePath`, the copy's destination) never saw where a link leads: with `link -> .fkbr-tmp`, a mkdir made `.fkbr-tmp/new`, a remove deleted a folder of the staging area, and a copy into `link/<existing folder>` was installed there | `Real` follows links in every root (one `Lstat` per component, on writes and the copy's destination only; reads walk only where there is a share) | `TestRealWithoutShare`, `TestStagingThroughLink` (copy, mkdir, remove, through a link and through `d/up -> ../.fkbr-tmp`) |
| `TestShareThroughSymlink` made its source file after `Submit`, so `Submit` failed on the missing source and the refusal on the target was never reached; the report cited it as the receive test | The file exists first; the job must not be done and nothing may land in the share. Checked against the build with the receive-side `Real` taken out: fails | `TestShareThroughSymlink` |
| `lookahead` closed the blob but not the snapshot reader, so its decoder was never returned to the pool (no goroutine: the decoders are synchronous; an allocation per plan and folding target) | `eachEntry` closes both; `Reader.Close` may be called twice | `TestEachEntryClosesReader` |
| A copy across filesystems dropped a failed `Chown`/`Lchown`, so a job onto a filesystem that refuses owners (FAT below the root) reported none | Counted in `OwnerNotPreserved` as the plain install counts it; a failed setuid re-chmod is counted too | `TestCopyAcrossCountsOwnerFailures` (file and link; skipped as root) |

Checked on the lab (build `2aa2d61`, four boxes): `go test ./...` and the
engine tests in ssh mode pass; a copy to three nodes and a mirror sync to two
(Latin-1 name, a symlink) end with equal checksums and an empty second plan;
on a real node with `stlink -> .fkbr-tmp`, `mkdir` and `rm -r` through it are
refused ("staging area"), a copy into `stlink/keep` fails, and the staging
folder is as it was.

Documented, not changed: the planner's lookahead keeps two hash maps per
folding target (about 40 bytes per entry, some 600 MB at the 16 M limit), so
the planner's memory is flat except for those targets (`protocol.md`).
Declined: the review's note that `.fkbr-xdev-<job>-<hash>` may collide with a
user file: the name is removed on purpose so a rerun of the same job can
replace its own leftover, and a collision needs a file made with that job's
hash.

## 23. Fifth pass of the second model's review (2026-11-06)

A reviewer's top ten and bench list (`SUGGESTIONS.md`) were checked against
the code one by one; nine were real and are fixed, each with a test that
fails on the build before.

| Found | Now | Test |
|---|---|---|
| The last subscriber leaving while a new one read its snapshot stopped the event poller, and the newcomer (who saw a subscriber on arrival) never started it: no events | `Subscribe` decides to start the poller under the lock, after joining | `TestSubscribeAfterLastLeavesDuringSnapshot` (hook `afterSnapshot`) |
| A staged directory where a resumed run now has a symlink failed the chunk with ENOTEMPTY, and every retry the same | The symlink branch removes it with `RemoveAll`, as the file branch does | `TestResumeReplacesStagedDirByLink` |
| The wait after "share full" returned the first soft-mount error and failed the job | Its three calls ride out transient errors like the waits around them | `TestShareFullWaitRidesOutEIO` |
| `fkbr check` dereferenced nil when the clock-skew probe's `Stat` failed | A warning, no panic; the write error is reported too | (nil `Stat` cannot be provoked on a plain file system) |
| The F3 viewer wrapped by characters, so wide ones overflowed the dialog | Wraps by cells, in one pass | `TestWrapTextCells` |
| The cursor row's marquee dropped characters for a width in cells, so a wide name went blank at the rest | Drops cells | `TestScrolledWideNames` |
| A chunk applied again after a crash listed the whole staging folder for every file already there (N listings of N names in a flat folder) | `hasName` keeps the listing per folder and reads again only for a name it does not hold; dropped when staging is removed or committed | `TestReplayListsStagingFoldersOnce` (405 listings for 400 files before) |
| The jobs panel counted the arrow in `" → "` in bytes (5), leaving two cells of the line unused | Counted in cells | `TestJobLineKeepsProgress` (trailing room) |
| Sizes such as `256mib` or `2gb` were refused; `listed()` cached an empty folder when it could not open it (a wrong "clashes" refusal) | Case-insensitive suffix; an open that fails is not cached | `TestParseByteSize`; (no test for `listed`) |

Not real: the claim that sync re-copies setuid/setgid files forever. The
planner's mode mask is `0o1777`, which leaves those bits out on purpose.
Not done, by choice: `GetJob`'s repeated listings (the refresh before each
read is the NFS rule, and it has not shown as a cost), paging re-sorts in
the pane, a bound on a window grant (only a 32-bit build can be hurt, and
none is built), and the other bench notes (home-node ceilings, the `StagedBytes`
lock, directory fsync, scrollbar thumb, settings `Stat`, spinner repaint;
reasons in the reply to the reviewer).

## 24. Sixth pass of the second model's review (2026-11-07)

The reviewer's ten suggestions were checked against the code. It had read
only the docs, so most restated what is already recorded as known or
decided. Five were worth doing (the xattr note after a question about it):

| Found | Now | Test |
|---|---|---|
| `TestSparseExtents` made a 1 GiB file of holes and kept it twice; free without `-race`, 4.2 GB under it (the whole `bundle` package 5.7 GB, measured on the 8 GB lab VM), so `-race` could not run on a small box | 128 MiB, the same assertions | `TestSparseExtents` |
| `docs/stability.md` said "over 330 tests" in one place and "over 390" in another | Both say over 390 | |
| The user docs did not say what is not carried over (ACLs, xattrs, hard links, devices, `/proc`) | A short list in `operations.md` (Privileges); the code has no xattr or ACL handling | |
| ACLs and extended attributes were dropped without a word | The source counts the files it reads that have any (`Flistxattr`, not `security.selinux`) and the job notes "N files have ACLs or extended attributes; they are not copied". Nothing is carried: no format change | `TestHasUncarried`, `TestCopyNotesUncarriedXattrs` (fails without the note; skips where `user.*` xattrs are not allowed); the F9 row of a finished job with notes, errors or skips is yellow (`TestJobHasNotes`) |
| `fkbr check` did not mention a state dir in memory: an ssh home node's job records and the own jobs of root `local` are lost at a reboot | A warning where the swarm keeps its records there (home node, `ssh:` hosts), information elsewhere | `TestStateDirOnTmpfs` |

`-race` on the Rocky 10 VM, now with 8 GB, at `cef42d3`: every package
passes, the engine also in ssh mode, and `TestChunkSizesRamp` and
`TestSparseExtents` too. No data race.

Not done, with reasons: a docs gate for `make release` (it already fails
at the `cp`, and the F1 help width is tested), caps on blobs and calls
(section 20 lists why they stay), an in-repo chaos driver (the lab
scripts hold addresses; the fault wrappers and loops are in the tests),
the open `todo` rows (they need the NAS's owner or a lab slot), a `-strict`
flag and carrying xattrs, ACLs or hard links (a protocol change, a separate decision),
and a claim for the two-nodes-apply-one-plan race (known and narrow; the
store does use `O_EXCL`, so a `mkdir` claim might work on NFS, but that
needs a lab check first).

## 25. Seventh pass of the second model's review (2026-11-07)

The reviewer ran ten probes against `a29bf38`. Each was reproduced on the
Rocky 10 VM and fixed with a test that fails on the old code (run against
the old sources on the VM) and passes on the new.

| Found | Now | Test |
|---|---|---|
| `PruneBlobs` aged a blob folder by its own mtime, which the NFS refresh trick bumps on every read: a read plan was never pruned | Age is the newest entry; a folder with none (a blob being written) counts by its own mtime, so a fresh empty folder is not taken for an old orphan | `TestPruneBlobsIgnoresReadRefresh`, `TestPruneBlobsKeepsFreshEmptyFolder` |
| `Fetch` dropped its claim when moving a bad request to `failed/` failed, and the request stayed in `cur/` unseen | The claim stays and the error is returned; a request already gone is not an error | `TestFetchKeepsClaimWhenParkFails` |
| `CachesAttributes` read only the two minimum timeouts and let `lookupcache=positive` hide a long cache | The four timeouts as the kernel lists them (unlisted ones at its defaults 3/60/30/60, `actimeo` too), the maximum counts; `noac` still wins. Checked on the real `/shared` mount (all four 1800): true | `TestCachesAttributesTimeouts` |
| A sync deleted a file or folder on a FAT-like target to install a symlink it cannot store | The source entry is refused before any delete, in a sync or a copy plan; a folder keeps what is in it | `TestPlanRefusesSymlinkOverEntryOnNoSymlinkTarget` |
| The ACL/xattr note counted a file once per retry of the send | The count is local to one attempt | `TestUncarriedNoteCountsOnceAcrossRetry` |
| A node forgotten from the registry stayed in every TUI's list | `node_gone` event (`EventNodeGone`, `NodeID`); the TUI drops the node and its `@dest` mark, never its own. An older frontend ignores it | `TestForgottenNodeBroadcastsRemoval`, `TestNodeGoneEvent` |
| `ParseByteSize` wrapped around (`9223372036854775807KiB` was -1024); F11 took `99999d` | An error; times are at most ten years (`config.MaxDuration`, in `Validate` and F11) | `TestParseByteSizeOverflow`, `TestParseDurationsBounded`, `TestDurValueRange` |
| Settings after a `---` in the config file were ignored, unchecked | More than one YAML document is an error | `TestParseSecondDocumentRefused` |
| `Subscribe` failed with an empty message | The status text, as in `call` | `TestSubscribeErrorHasAMessage` |
| `fkbr check` printed through `fmt.Printf`, past the cleaning of `stdout()` | `runCheck` writes to the writer it gets | `TestRunCheckWritesThroughItsWriter` |
| (found while checking) `ServeSocket` never closed the old listener when it listened on the path again: a descriptor per recreation | Closed, without unlinking the new socket | `TestSocketRecreationClosesOldListener` |
| A failing read before the chunk's magic was reported as "not an fkbr chunk" | The read error is kept; a short or foreign chunk still gets the old message | `TestMagicReadErrorKept` |

| (round 3) A node that left the list and came back (an unreadable registry entry that healed) lost its `@dest` mark | The TUI parks the mark of a node that is gone and restores it when the node returns; `@dest` never names a node that is not listed | `TestNodeGoneAndBackKeepsDestMark` |

One existing test changed: the `TestMounts` line for a mount "set to 1 s"
listed only `acregmin=1,acdirmin=1`. A kernel prints only options that
differ from its defaults, so that line means `acdirmax=60`; it now lists
all four, as a kernel does for 1 s.

On the Rocky 10 VM at this tree: `go test ./...`, the engine in ssh mode
and `-race` on store, api/remote, engine, inventory, mountinfo, config
and cmd/fkbr pass.

Left as they are: `do()` in sshlink ignores its drain error (no caller
can reach a coded reset there), the Makefile's `bin/fkbr` on other
architectures, and the editor's byte/rune find.

## 26. Eighth pass of the second model's review (2026-11-07)

A read-only review of `998df32` (nothing was run on the reviewer's side).
Each finding was checked in the source first; the tests below were run
against the old sources on the Rocky 10 VM and fail there (the coverage
tests pass on both, as they cover gaps).

| Found | Now | Test |
|---|---|---|
| `fkbr check` used plain `os.*` on `tmp/` and `protocol-version`: a symlink put there by a share writer made it write, read back and delete files outside the share (and print one), and report "writable" | Everything in `check.share()` goes through an `os.Root` on the share; a link out of it is a failure, not followed | `TestShareCheckStaysInTheShare`, `TestShareCheckWritable` |
| A `COMPLETE` marker with a parse error, a negative or a huge count was taken as the chunk total (`Atoi` error dropped) | Anything but a count from 1 to 2^31-1 is no marker (0: not complete, targets wait) | `TestCompleteMarkerMustBeACount` |
| The source (and the target) made one try to record `done` after the staged data was already gone; a lost status left the job running for good | `finishThen` also on success: the end first, retried for the transient budget, the data after it | `TestSourceDoneEndLostRetries` |
| `finishThen` kept the data "for the next run" when the end stayed lost, but the request was parked in `failed/` (or removed): no next run | It returns `errEndLost`; `startJob` leaves the request claimed in `cur/`, so a restart resumes | `TestEndLostKeepsTheRequest` |
| A snapshot header with `mtime_step` near MaxInt64 (x 1e9 wraps), a negative step or min above max was accepted | `NewReader` refuses a step outside 0..1 day and min above max | `TestReaderRefusesBadHeaderNumbers` |
| `safetext` let invisible format characters through (zero-width space, word joiner, BOM, soft hyphen, tags, U+2028/9): "trusted<ZWSP>host" read as another name | Category Cf, Zl and Zp become `?`, except the joiner and non-joiner (emoji sequences, Persian and Indic scripts). A BOM at the start of a viewed file now shows as `?` | `TestCleanInvisibleFormat` |
| Coverage only: `atomicfile.Write`, `id.At`/`New`, `NewReader`'s error branches, `remote.Listen` over a stale socket had no tests | Tests added, no code change. The review's `id.At` with a negative time cannot happen (the share clock is after 1970); its "Chmod failure leaves the socket" is wrong (`Close` unlinks it) | `TestWrite*`, `TestAtShapeAndOrder`, `TestNewIsUnique`, `TestNewReaderErrors`, `TestListenReplacesStaleSocket` |

A reread of the same pass found two more (both fixed, run on the Rocky 10 VM):

| Found | Now | Test |
|---|---|---|
| `TestShareCheckStaysInTheShare` looked for `99` in every message; messages carry the temp dir, whose random suffix can hold it (54 failures in 400 runs) | It looks for `protocol 99`, what the old code printed | the same test (400 runs, 0 failures) |
| The setup checks of a job (not a target, write policy, root, missing path, wrong source) ended it with one try to record `failed`; a lost status parked the request in `failed/` and the job read as running for good | They go through `statusWriter.fail` (`finishThen`, nothing to clean): the request stays claimed | `TestSetupFailureWithLostEndKeepsTheRequest` (fails on the old code) |

A review of the test suite itself (2026-11-07, run as an ordinary user)
found nothing failing; three test-only changes: `FuzzReadNative` also
fuzzes compressed chunks (zstd seeds), the fake slow chunk open stops
on a cancelled context, and `TestConfigPathOrder` skips when it cannot
write beside the test binary. Kept as they are: fixed sleeps that are
minimum waits, cleanup waits without a timeout (a `go test` timeout
prints every stack), no `t.Parallel` (tests set package variables).

Left as it is: the world-connectable moment between `net.Listen` and
`Chmod` of the daemon socket (the state dir is private, nobody else can
reach the socket's folder); `shareModes` still lists the share with
`os.ReadDir` (it reads names only and never follows a link).

## 27. A review of our own code (2026-11-08)

Static analysis (`go vet`, staticcheck) was clean but for one unused
struct field in `send.go`, removed. Black-box runs on the lab with two
live nodes (special files, a file truncated, files removed, and a folder
replaced by a file during a copy; names with a leading dash, spaces and
a newline; trailing slashes; a copy onto the same node) found the
planner, sync apply and copy paths correct: a copy whose files vanish
ends `done` with errors and the next mirror sync converges. One CLI
footgun: an empty argument (an unset shell variable) meant the whole
shared folder, so `cp "$X" node:/d` copied everything and
`sync -mirror "$X" node:/d` could delete on the target. `cp`, `mv` and
`sync` now refuse an empty argument, and `rm` says a whole shared folder
is not removed (`TestEmptyArgumentRefused`).

## 28. Another model's review (2026-11-08)

Ten findings on `c6b003a`; each was read in the source, five were fixed
with a test that fails without the fix, five were not.

Fixed:

- An `EIO`, `ETIMEDOUT` or `ESTALE` on the byte after a chunk's end
  marker was reported as "data after the end of the chunk", which the
  target does not retry: one blip failed the job. It is now the read
  error (`TestErrorAfterTheEndKept`, like the magic bytes, section 25).
- Ctrl+W in the editor sliced the line at an offset counted in the
  lower-cased line. Two U+023A (lower case is one byte longer each) made
  it run past the line: a panic. The offset is now in the line itself (`TestMatcherOffsetsAreInTheLine`,
  `TestEditorFindWithWideLowerCase`).
- The read-ahead of a small file was not bounded by the size the walk
  saw: a file replaced by a big one before it was opened was held whole,
  eight at a time. It reads that size plus one byte (the bundle marks the
  file changed as before; `TestPrefetchReadsNoMoreThanItsSize`).
- A remove that had to make folders writable gave them
  `Perm()|0o700` and so dropped setgid and sticky on the ones left
  behind (only folders lacking the owner's rwx are touched, not `1777`
  as the review said; `TestOpenDirsKeepsSpecialBits`).
- A copy naming 120,000 paths (a shell glob) was one request over the
  socket's 2 MiB cap, "request body too large". The cap is 32 MiB; the
  kernel keeps an argument list under about 6 MiB
  (`TestSubmitTakesALongPathList`).

Not changed:

- `local/x` with an unreadable folder `local` in the shared dir takes
  the shorthand (root `local`): the check lists the folder and treats any
  error as absent. It needs that folder to exist and be unreadable; a
  fix needs a not-found code on the wire, and for a group or a list of
  nodes the check's list call names no single node and fails anyway.
- The target scan's probe leaves the destination folder's mtime changed
  when the user can write it but does not own it (once per filesystem and
  run). A dry run is not entirely free then; the `sync -n` help says so
  now. `sync -u` is not affected (it never keeps a folder).
- A failed listing in `listed`/`hasName` goes on to the rename: a folder
  that can be entered but not read is normal (mode `-wx`), and the case
  clash needs a folding filesystem with a failing disk as well.
- A same-size, same-mtime change during a read is sent as good: it takes
  two writes within one timestamp tick around the walk; hashing every
  file twice costs a read for a case the editor's save (where it
  matters) already covers.
- `Write` without a sum and a mtime checks the size only: older nodes
  send no mtime (api.go says so), the TUI sends everything, the socket is
  the user's own.

## How to run the lab tests

- **Torture** (`torture/`): an NFS swarm and an ssh swarm under stacked
  faults with small limits, checked by sha256 manifests and convergence
  (section 14); `edge.py` runs the targeted tests. See its README.
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
- **Reboot** (`reboot/`): `reboot.py OUTDIR [case...]` reboots a
  target, a source, an ssh host or the ssh home mid-copy, and remounts
  the share; `powercut.py OUTDIR [case...]` resets them without a sync
  (section 18).
- **Explore** (`explore/`): `explore.py quota | quota2 |
  concurrent | same | dup | queued | clean`, each a focused experiment;
  results append to `results.log`.
