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
| 1,000,000 entries copied to 1 and 3 targets | All verified; 2 min 16 s and 4 min 57 s | lab (compare) |
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
| Hosts kept in the config file: added twice, removed, the owner's comments and keys, a symlink in its place | Each host once; the rest of the file and its mode kept (a new file 0600); never written through the symlink | auto (`TestSavedHosts`) |
| Which config file: `-config`, `$FKBR_CONFIG`, `fkbr.yaml` beside the binary, `~/.fkbr.conf` | Read and written in that order; `~/.fkbr.conf` only when none of the others is there | auto (`TestConfigPathOrder`) |
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
| With `socket_mode` open to others, anyone reaching the socket could make the node ssh anywhere with the owner's keys | Linking at runtime (F7, F8, the socket) is refused on such a node | `TestNoLinksOverAnOpenSocket` |
| A linked host's node was always named after its short hostname: two directories on one host, or two hosts of one short name, could not both link | `NAME=host[:path]` names the node | `TestParseHosts`, `TestSSHHostsLinkedAtRuntime` |
| A remote that stayed up but never started `fkbr link` left the host "connecting" for good | The start is given up after 45 s and tried again | `TestSilentStartGivenUp` |
| A linked node's 10 ms poll also paced its jobs' waits, each a round trip to the home node | Only asking for requests is that quick | `TestLinkPolls` |
| A CLI attached to a daemon always said "using the running node X, which shares ...", because of root `local` | Root `local` is not compared | `TestSameRootsLeavesOutLocal` |
| The TUI asked a node on a share for its links on every node list refresh | Asked once | `TestNotHomeAskedOnce` |
| Two `firstLine` helpers that differed on long lines | One | (none) |
| A pane listing a node's roots (`files`, `local`) put its cursor back on the top row at every node refresh, a few seconds apart (found by the owner, 2026-10-25) | The cursor stays on its row | `TestRootsRefreshKeepsCursor` |

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
- **Explore** (`explore/`): `explore.py quota | quota2 |
  concurrent | same | dup | queued | clean`, each a focused experiment;
  results append to `results.log`.
