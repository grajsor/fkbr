# Benchmarks

Measured with `scripts/bench.sh -n 3 -large 512 -small 50000 -tree 200000`:
one source node and three targets, all on one machine.

- **Host:** Raspberry Pi 5 (aarch64, 4 cores, 4 GiB RAM), ext4 on an SSD.
- **Shared disk:** the share and all four nodes' directories are on that
  one SSD.
- **Write load:** a fan-out to three targets makes the disk write each byte
  four times (staging plus three installs), on top of reading it.

Read the numbers as a lower bound and repeat the script on the real hosts
and NFS server.

| Workload | Time | Throughput | Notes |
|---|---|---|---|
| 3 × 512 MiB files → 3 nodes (cp, `@all`) | 26.6 s | 58 MiB/s | source 1537 MiB; 4 copies written to one disk |
| 50,000 × 4 KiB files → 3 nodes (cp) | 30.0 s | 6 MiB/s | per-file costs dominate (stat, tar header, rename) |
| sync plan, 200,000 files, deep tree | 6.9 s | | scans of all 4 nodes in parallel |
| sync apply (all new) | 147 s | | one transfer job, fanned out to 3 nodes |
| sync re-plan (converged) | 14.3 s | | 0 entries left to copy |

Peak daemon memory (VmHWM): 16 MiB for the source and 20–21 MiB for each
target.

## Larger trees (from milestone 6)

Two nodes on the same Pi, 1,000,000 files in 10,101 directories:

| Step | Result |
|---|---|
| first sync plan | 1,010,101 entries in 18 s, 16 MB peak memory |
| first full copy | 462 s; `diff -rq` clean |
| re-plan | 0 to do, 23.6 s |
| 100 changed + 50 deleted files + 1 removed dir, mirror | 251 copies / 52 deletes planned in 23.9 s, applied in 2.2 s |

Memory does not grow with the number of files: scans stream snapshots to
the share, and the planner merges them in one pass.

## Compression (`-z`)

`-z` compresses chunks with zstd (fastest level, 1 MiB window, up to 2
threads). gzip and lz4 were measured too and dropped.

**On the lab:** `cp` from rhel9-test to rhel7-test and rhel8-test over the
Synology NFSv3 share (2 runs each).

| Data | none | gzip | lz4 | zstd |
|---|---|---|---|---|
| tree: `/usr/share` + `/usr/lib64`, 510 MiB, 18,140 entries | 20–22 s | 18.1–19.1 s | 17.0–17.1 s | 14.0–15.6 s |
| one 256 MiB file, compressible (2.3×) | 7.6–8.6 s | 7.1–8.2 s | 5.5–5.6 s | 5.1–5.6 s |
| one 256 MiB file, random | 7.5–8.7 s | 7.6–8.1 s | 8.6–10.1 s | 7.6 s |

Peak daemon memory (VmHWM) after these runs: gzip 21 MiB at the source and
12–17 MiB at the targets; zstd 29 MiB and 13–19 MiB.

**The codecs alone:** one 256 MiB mixed tar, in memory.

| Codec | Ratio | Pi 5: compress / decompress | i7-8700K (2 vCPU): compress / decompress |
|---|---|---|---|
| gzip BestSpeed | 2.04 | 109 / 116 MiB/s | 195 / 190 MiB/s |
| lz4 | 1.81 | 236 / 1159 MiB/s | 358 / 2201 MiB/s |
| zstd fastest, 1 thread | 2.27 | 155 / 511 MiB/s | 249 / 1009 MiB/s |
| zstd fastest, 2 threads, 1 MiB window (used) | 2.25 | 194 / – MiB/s | 380 / – MiB/s |

Every codec is faster than the ~35 MiB/s share, so the ratio decides. zstd
puts the fewest bytes on the share, and decompresses about five times
faster than gzip.

## What dominates where

- **Large files:** disk and network bandwidth. Chunks are written to the
  share once whatever the number of targets, then read by each target.
- **Many small files:** per-file costs on both ends: `stat` and `open` at
  the source, and staging, `rename` and metadata calls at the target. On
  real NFS, add the round trips for writing chunks to the share. Chunks
  hold many files each, so the NFS cost per small file stays low.
- **Sync scans:** `stat` throughput of each node's local disk. Scans run
  in parallel on every node and do not touch NFS except to write the
  snapshot.
