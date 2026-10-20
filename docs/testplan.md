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
| Mixed builds of the same protocol | Work together; older builds lack newer checks | **todo** |

## How to run the lab tests

- **Torture** (`~/fkbr-lab/torture`): rounds of random copies and syncs
  between five nodes, with kill -9, SIGSTOP and restarts, verified by
  sha256 manifests. See its README.
- **Weird** (`~/fkbr-lab/weird`): `weird.py [test...]` runs odd cases with
  several nodes on one machine; `nfscut.py target|source` cuts a lab node
  off the NAS mid-transfer; `fuzz_tui.py SECONDS SEED` (with pyte) throws
  random keys and resizes at the TUI.
- **Funky** (`~/fkbr-lab/funky`): `funky.py [test...]` (with pyte; sudo
  for the FAT image and hostnames): how fkbr is started and where, hostile
  names and peers, odd target filesystems and limits.
- **Compare** (`~/fkbr-lab/compare`): `compare.py setup | setup_big |
  run N | runbig | clean` times fkbr, tar, rsync and borg through the
  share and verifies every copy; results go to `docs/benchmarks.md`.
- **Explore** (`~/fkbr-lab/explore`): `explore.py quota | quota2 |
  concurrent | same | dup | queued | clean`, each a focused experiment;
  results append to `results.log`.
