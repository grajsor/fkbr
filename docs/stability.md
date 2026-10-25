# Stability

fkbr is tested by breaking things on purpose while it works: processes
are killed and frozen, hosts are cut off from the NFS server and from
each other, disks fill, clocks jump, and folders are deleted under
running jobs. Then every copy is checked file by file against its source.

**No fault run has ever corrupted a file, or lost one in a copy, sync or
move.** What the runs did find (jobs that failed or hung when they should
have carried on, and answers that were wrong) is fixed, and each fix has
a test that fails without it.

## At a glance

| | |
|---|---:|
| Hours of fault runs | **over 35 h** |
| Copied trees checked against their source | **over 10,000** |
| Faults injected (kills, stops, cuts, full disks, …) | **over 1,000** |
| Files lost or corrupted | **0** |
| Bugs found by these runs, each fixed with a test | **47** |
| Tests in `go test ./...` (also run with nodes linked over ssh, and under `-race`) | **over 330** |
| Random inputs fed to the chunk reader, the exclude matcher and the TUI | **over 9 million** |

The full list of cases, and every bug with its test, is in
`testplan.md`.

## The lab

Five hosts on one NAS, every pair also linked over ssh:

```
                ┌───────────────────────────────────┐
                │ NAS   NFSv3, hard, actimeo=1800   │
                └─────────────────┬─────────────────┘
                                  │ 1 GbE
     ┌─────────────┬──────────────┼──────────────┬──────────────┐
┌────┴─────┐ ┌─────┴────┐ ┌───────┴──┐ ┌─────────┴┐ ┌───────────┴┐
│ CentOS 7 │ │ Rocky 8  │ │ Rocky 9  │ │ Rocky 10 │ │ Raspberry  │
│ kernel   │ │          │ │          │ │ kernel   │ │ Pi 5       │
│ 3.10     │ │          │ │          │ │ 6.12     │ │ arm64      │
└──────────┘ └──────────┘ └──────────┘ └──────────┘ └────────────┘
└────────── x86-64 VMs, 2 vCPU, 1.7 GiB RAM, XFS ─┘
```

The torture test runs **two swarms on these hosts at once**:

- an **NFS swarm**: five nodes on a directory of the share, each with a
  second root on a 200 MiB tmpfs that fills up;
- an **ssh swarm**: one home node linking the other four; every 3
  rounds the home node moves to another host, the Pi included.

Limits are kept small on purpose, so that the edge cases come often:
chunks of 1-4 MiB, quotas of a few chunks, one or two jobs at a time per
node, targets detached after 2 min without progress.

## What it throws at fkbr

Up to three faults at once, a new one every 5-20 s, each undone within
2 min:

| Fault | What it does |
|---|---|
| **kill -9** of a node, the ssh home node, a remote `fkbr link`, or the ssh client | A crash mid-transfer, on either end |
| **SIGSTOP** of a node, the home node or a link | A frozen process that looks dead to its peers, then wakes up |
| **SIGTERM** and restart | A clean stop in the middle of a job |
| **NAS cut** | A blackhole route to the NFS server: the hard mount hangs |
| **ssh cut** | A blackhole between the home node and a host: the link goes silent without closing |
| **netem** | Delay of up to 150 ms, jitter, and packet loss of up to 3 % |
| **Clock jump** | A host's clock set 15 s to 2 min wrong, then put right |
| **Memory squeeze** | Only about 250 MiB left free |
| **CPU stall** | Every core busy |
| **Full disk** | The target root filled to the last block |

And the jobs are made hard on purpose:

| Job | What happens to it |
|---|---|
| **cp, sync** | Copies (some compressed, some into the root that fills up) and mirror syncs, to one to four targets |
| **churn** | The source changes while it is being read |
| **vandal** | The target folder is deleted, swapped for a symlink out of the root, made unreadable (chmod 000), or blocked by a file |
| **cancel** | Cancelled 1-10 s in |
| **mv** | A move, which must never lose its source |
| **conflict** | Two sources copy into the same folder at once |

## What must hold

After every round, with the faults off:

- A job that ended **done** left a target **exactly** like its source:
  content, type, mode, mtime, symlink target.
- After any trouble (failed, detached, cancelled, vandalised), **one**
  mirror sync must make the target exact.
- A file written by two sources at once is **whole**, from one of them.
- A move never loses data: either the source is still there, or every
  target has all of it.
- **Nothing is written outside a root**, even through a symlink swapped
  in mid-copy.
- The CLI's exit code tells the truth.
- A job never fails because of a fault fkbr should ride out (a short
  cut, a restart, a stop).
- No job hangs; no staging, temp chunk or chunk file is left behind; no
  panic in any log; every node is back online within 2 min.
- Memory and open files stay flat over time.

## The runs

| Date | Run | Build | Time | Jobs | Trees checked | Faults | Found |
|---|---|---|---|---:|---:|---:|---:|
| 2026-10-20 | funky: how fkbr is started and where, hostile names and peers, FAT targets, limits | | | | | | **9** |
| 2026-10-20/24 | torture, five runs | 0575d4b … 6410f10 | 1 h | 235 | 706 | 52 | 0 |
| 2026-10-21 | robust test: bad network, chaos, odd file systems, vandalised share | f9ac962 | 4 h 36 min | | about 2,100 | over 140 | **17** |
| 2026-10-21 | robust test, second run | da7f98f | 1 h 26 min | | about 640 | | **4** |
| 2026-10-21/22 | **soak** | 7fad3d1 | **16 h** | **1,099** | **3,496** | 133 | 0 |
| 2026-10-23 | ssh: links cut, home node frozen and killed, CentOS 7 as home | 1ef0a75 | | | | | **4** |
| 2026-10-23 | ssh with `/tmp` in memory, a target stopped mid-copy | | | | | | **3** |
| 2026-10-24 | one machine, over a share and over ssh, `-race` | 6a7fa44 | 4 h | | | | **6** |
| 2026-10-24/25 | torture v2, run 1 | 00bd754 | 1 h 44 min | 217 | 638 | 172 | **2** |
| 2026-10-25 | torture v2, run 2 | + 2 fixes | 2 h 32 min | 377 | 1,030 | 314 | **1** |
| 2026-10-25 | torture v2, soak | + 2 fixes | 1 h 14 min | 202 | 538 | 25 | 0 |
| 2026-10-25 | torture v2, run 3 | + 3 fixes | 1 h 37 min | 226 | 577 | 176 | **1** |
| 2026-10-25 | **torture v2, run 4** | **e79d440** | 1 h 26 min | 216 | 579 | 178 | 0 |

"Trees checked" counts target folders compared file by file with their
source after a job.

The last run, on e79d440 with every fix, found nothing: 216
jobs under 178 faults, every target exact or made exact by one sync,
memory at most 75 MiB per node, open files at most 26.

The 16 h soak showed no leak: the longest-running process (7 h 18 min)
stayed at 42-91 MB of memory and 8-15 open files. In the night of
2026-10-25 the clocks went back an hour (summer time ended) in the
middle of run 2; nothing noticed.

## What the runs found

Each of these is fixed and has a regression test (`testplan.md`
sections 8, 13 and 14):

- **Jobs that gave up too soon.** A link cut of 30-45 s over ssh, or two
  short NAS drops 3 min apart, failed a copy that should have carried on.
- **Jobs that waited forever:** for a target that was gone, or a source
  and a target each waiting for the other after the ssh home node froze.
- **Wrong answers.** A `sync` that exited 0 with nothing copied; a
  cancelled job shown as failed; a job shown running though every target
  was done.
- **Left-overs:** half-received staging after a restart, temp chunks
  after a kill.
- **Odd file systems:** names FAT and exFAT change or refuse, mtimes XFS
  cannot store, folders on case-insensitive targets.
- **One deletion bug:** on case-insensitive file systems a mirror could
  delete a folder an exclude was meant to protect. Fixed the same day.

## Targeted tests

Beside the random runs, single cases aimed at one weak spot each
(2026-10-25, over a share and over ssh):

| Case | Result |
|---|---|
| Bytes flipped in staged chunks and their records on the share | Every flip caught; nothing damaged installed |
| A source folder swapped for a symlink to `/etc` mid-copy | Nothing from `/etc` copied (6 of 6) |
| Files made on the target after a mirror's dry run | Kept when the plan is applied |
| The target remounted read-only, or out of inodes, mid-job | Fails cleanly; the node keeps running |
| Paths of 4,356 bytes (past the 4,096-byte limit) | Copied exact |
| 100,000 files in one folder | 9 s, at most 58 MiB of memory |
| 40 CLI calls at once; 20 jobs into one job slot | All answered; all 20 done and exact |
| An ssh link throttled to 2 Mbit/s and saturated | Done, exact |
| kill -9 at an exact moment (target installing; home node while a target installs) | Resumed, exact |
| The ssh home node restarted 18 times under one copy | Done, exact, one link per host |
| The same sync again right after it (5 data sets, 4 kinds of target) | Nothing to do, 19 of 19 |

## In every build

`go test ./...` runs real nodes on temporary directories, about 2 min:

- **over 330 tests**, many of them found by the runs above;
- the same engine tests again with every node linked as over ssh
  (`FKBR_TEST_TRANSPORT=ssh`), and both under the race detector;
- **model tests:** random trees, options and moves, checked against a
  simple model of what sync and mv should do;
- **fuzzers** for the chunk reader, excludes and the ssh link protocol
  (over 9 million inputs so far), plus random keys, resizes and mouse
  events thrown at the TUI.

## Not covered yet

- The NAS itself restarting mid-transfer.
- ssh over a real wide-area network or a jump host (only lab links, with
  netem).
- Several users on one share.
