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

## 2. Process failures

| Case | Expected | Status |
|---|---|---|
| kill -9, SIGSTOP (longer than `offline_after`), SIGTERM of source or target mid-transfer | Resume, or detach and catch up; never a partial install | lab (torture, 62 events) |
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
| One host loses the network to the NAS for minutes | Others detach it and finish; it catches up | **todo** (needs root for iptables) |
| Protocol version mismatch on the share | Clear refusal | auto |

## 4. Load and limits

| Case | Expected | Status |
|---|---|---|
| Independent jobs at once (1→2,3 while 5→4) | All verify; total NAS throughput is shared | lab: ~100 MB/s total either way |
| More jobs than `max_jobs` on one node | Extra jobs show `queued`; a fan-out drops a queued target after `detach_after` with a clear reason | auto, lab |
| `job_quota` 256 MiB / 1 GiB / 4 GiB | 1 and 4 GiB equal, 256 MiB slower | lab |
| 1 million small files, one sync | Memory bounded, plan streams | **todo** |
| Files bigger than `job_quota` (e.g. 20 GiB) | Segments flow through the window | **todo** on the lab (auto for 3 MiB through 256 KiB) |
| Hundreds of finished jobs on the share | Listing stays fast, pruning keeps up | **todo** |
| Target disk full during receive | Target fails cleanly, staging removed | **todo** |
| Destination folder not writable for some entries | Those entries are errors; the rest installs; staging removed | auto |

## 5. Mixed fleet

| Case | Expected | Status |
|---|---|---|
| CentOS 7 (kernel 3.10) to Rocky 10, x86-64 and ARM64 | Same results everywhere | lab |
| Mixed fkbr versions on one share | Same protocol works; older nodes lack newer checks | **todo** |

## How to run the lab tests

- **Torture** (`~/fkbr-lab/torture`): rounds of random copies and syncs
  between five nodes, with kill -9, SIGSTOP and restarts, verified by
  sha256 manifests. See its README.
- **Explore** (`~/fkbr-lab/explore`): `explore.py quota | quota2 |
  concurrent | same | dup | queued | clean`, each a focused experiment;
  results append to `results.log`.
