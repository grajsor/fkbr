# Benchmarks

## Five nodes over real NFS (2026-10-19)

**Setup**
- **Share:** Synology NAS over NFSv3 on 1 GbE. Raw speed per client with
  `dd`: write about 115 MB/s, read 70–107 MB/s.
- **Nodes:** four x86-64 VMs (2 vCPU, 1.7 GiB RAM; CentOS 7.9, Rocky 8.10,
  9.8 and 10.2, mounted with `actimeo=1800`) and a Raspberry Pi 5
  (4 cores, 4 GiB, Debian 12, 1 GbE, `acdirmin=60,nconnect=4`).
- **Runs:** source rhel9. "1 target" is rhel8; "4 targets" is `@all`
  (rhel7, rhel8, rhel10 and the Pi). Each case ran three times; the tables
  show the median. Times are from `fkbr cp` starting until every target
  has installed everything.

**Workloads**

| Workload | Size | zstd ratio |
|---|---|---|
| VM images: 2 random files | 1024 MiB | 1.00 |
| Web access logs: 8 generated files | 1025 MiB | 4.73 |
| Software tree: `/usr/share` + `/usr/lib64` | 488 MiB, 18,140 entries | 2.43 |
| Mail store: 20,000 text files of about 4 KiB | 82 MiB | 5.46 |

**Copy times**

| Workload | 1 target | 1 target, `-z` | 4 targets | 4 targets, `-z` |
|---|---|---|---|---|
| VM images | 20.1 s (51 MiB/s) | 20.1 s | 42.1 s | 42.1 s |
| Web logs | 19.1 s (54 MiB/s) | **10.6 s** (97 MiB/s) | 41.6 s | **18.6 s** |
| Software tree | 18.1 s | **13.6 s** | 28.6 s | **18.6 s** |
| Mail store | 11.1 s | **8.6 s** | 14.1 s | **10.6 s** |
| Web logs, Pi as source | 18.6 s | **9.0 s** | | |

**CPU time** (seconds of daemon CPU per copy, 1 target)

| Workload | Source | Source, `-z` | Target | Target, `-z` |
|---|---|---|---|---|
| VM images (x86 source) | 3.6 | 4.5 | 3.5 | 3.7 |
| Web logs (x86 source) | 3.6 | 6.7 | 3.4 | 2.4 |
| Web logs (Pi source) | 1.8 | 8.2 | 4.1 | 2.5 |

**Sync** of the mail store to all four targets (`sync -z`):

| Step | Time |
|---|---|
| plan, first sync (20,201 entries per target) | 0.4–1.2 s |
| apply, first sync | 11–19 s |
| plan after 200 edits, 100 deletions, 100 new files | 0.8–1.8 s |
| apply (452 entries, 0.8 MiB per target) | 1.1–1.2 s |
| plan with `-mirror` (adds the 100 deletions) | 0.9–1.5 s |
| apply with `-mirror` | 1.9–2.0 s |
| re-plan when already in sync | 0.7–0.9 s |

**Peak memory** (VmHWM over the whole run): 39 MiB for the source, 16–26
MiB for the targets.

**What this shows**
- **Bulk data is limited by the NAS link.** Every byte is written to the
  share once and read back by each target over the same 1 GbE link, so one
  target gets about half of it (51 MiB/s).
- **Fan-out is cheap.** Four targets take about twice as long as one, for
  four times the deliveries: the source stages each chunk once and the
  targets read in parallel.
- **Use `-z` unless the data is already compressed.** It halves the time
  for logs to one target, and more than halves it to four. On random data
  it changed nothing, and it costs about 1 CPU second per GiB more at the
  source. Targets spend less CPU with `-z`, because they read fewer bytes.
- **Small files cost per file, not per byte.** About 1,800–2,300 files/s,
  and four targets are barely slower than one, since each installs in
  parallel.
- **The Pi is as fast a source as the x86 VMs.** Compression costs it about
  8 CPU seconds per GiB of logs, on one of its four cores.

This benchmark also found a bug: a target that looked up a job's staging
directory before the source created it cached "does not exist" for as long
as its mount caches attributes. On the Pi (`acdirmin=60`), that stalled
two in five fan-out runs for up to 60 s. Since the fix, ten runs in a row
showed no stall. A rarer one showed up in the sync runs: a node sometimes
missed a reply that was already on the share, for minutes, and the same
caused occasional "no answer within 30s" in production. Nodes now wait
for files by listing the directory instead of looking up the name (see
`docs/protocol.md`, rule 2).

## One machine (from milestone 6)

`scripts/bench.sh -n 3 -large 512 -small 50000 -tree 200000`: one source and
three targets on one Raspberry Pi 5, with the share and every node's
directory on the same SSD. A fan-out to three targets writes each byte
four times to that disk, so read these as a lower bound.

| Workload | Time | Throughput | Notes |
|---|---|---|---|
| 3 × 512 MiB files → 3 nodes (cp, `@all`) | 26.6 s | 58 MiB/s | source 1537 MiB; 4 copies written to one disk |
| 50,000 × 4 KiB files → 3 nodes (cp) | 30.0 s | 6 MiB/s | per-file costs dominate (stat, tar header, rename) |
| sync plan, 200,000 files, deep tree | 6.9 s | | scans of all 4 nodes in parallel |
| sync apply (all new) | 147 s | | one transfer job, fanned out to 3 nodes |
| sync re-plan (converged) | 14.3 s | | 0 entries left to copy |

Peak daemon memory (VmHWM): 16 MiB for the source and 20–21 MiB for each
target.

### Larger trees

Two nodes on the same Pi, 1,000,000 files in 10,101 directories:

| Step | Result |
|---|---|
| first sync plan | 1,010,101 entries in 18 s, 16 MB peak memory |
| first full copy | 462 s; `diff -rq` clean |
| re-plan | 0 to do, 23.6 s |
| 100 changed + 50 deleted files + 1 removed dir, mirror | 251 copies / 52 deletes planned in 23.9 s, applied in 2.2 s |

Memory does not grow with the number of files: scans stream snapshots to
the share, and the planner merges them in one pass.

## Choosing the codec

`-z` compresses chunks with zstd (fastest level, 1 MiB window, up to 2
threads). gzip and lz4 were measured too and dropped.

**On the lab:** `cp` from rhel9-test to rhel7-test and rhel8-test over the
Synology NFSv3 share (2 runs each).

| Data | none | gzip | lz4 | zstd |
|---|---|---|---|---|
| tree: `/usr/share` + `/usr/lib64`, 510 MiB, 18,140 entries | 20–22 s | 18.1–19.1 s | 17.0–17.1 s | 14.0–15.6 s |
| one 256 MiB file, compressible (2.3×) | 7.6–8.6 s | 7.1–8.2 s | 5.5–5.6 s | 5.1–5.6 s |
| one 256 MiB file, random | 7.5–8.7 s | 7.6–8.1 s | 8.6–10.1 s | 7.6 s |

**The codecs alone:** one 256 MiB mixed tar, in memory.

| Codec | Ratio | Pi 5: compress / decompress | i7-8700K (2 vCPU): compress / decompress |
|---|---|---|---|
| gzip BestSpeed | 2.04 | 109 / 116 MiB/s | 195 / 190 MiB/s |
| lz4 | 1.81 | 236 / 1159 MiB/s | 358 / 2201 MiB/s |
| zstd fastest, 1 thread | 2.27 | 155 / 511 MiB/s | 249 / 1009 MiB/s |
| zstd fastest, 2 threads, 1 MiB window (used) | 2.25 | 194 / – MiB/s | 380 / – MiB/s |

Every codec keeps up with what one target gets from the share, so the
ratio decides: zstd puts the fewest bytes on the share, and decompresses
about five times faster than gzip.

## What dominates where

- **Large files:** the NAS link. Chunks are written to the share once
  whatever the number of targets, then read by each target.
- **Many small files:** per-file costs on both ends: `stat` and `open` at
  the source, and staging, `rename` and metadata calls at the target.
  Chunks hold many files each, so the NFS cost per small file stays low.
- **Sync scans:** `stat` throughput of each node's local disk. Scans run
  in parallel on every node and do not touch NFS except to write the
  snapshot.
