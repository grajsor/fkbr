# Benchmarks

## fkbr, tar, rsync and borg through the share (2026-10-20, protocol 2)

**Setup**
- **Share:** a NAS over NFSv3 on 1 GbE (`actimeo=1800`, hard).
  Raw speed per client: write about 115 MB/s, read 70–107 MB/s; all
  hosts together get about 100 MB/s.
- **Nodes:** four x86-64 VMs, 2 vCPU and 1.7 GiB RAM each (CentOS 7.9,
  Rocky 8.10, 9.8 and 10.2), on local XFS.
- **Runs:** source the Rocky 9 VM. "1 target" is Rocky 10; "3 targets"
  are Rocky 10, Rocky 8 and CentOS 7. Page caches dropped on every host before each run.
  Times are from the start until every target has everything; every copy
  was then checked with sha256 against the source. The 1 GiB and 20,000
  file cases ran twice (the mean is shown), the larger ones once.

**Data** (all random, so compression does not help anyone)

| Set | Content |
|---|---|
| 1 GiB file | one file |
| 20,000 files | 4–16 KiB each, 225 MB, in 200 folders |
| 200,000 files | 1–3 KiB each, 391 MiB, in 2,000 folders |
| 1,000,000 entries | 950,000 files of 0.5–1.5 KiB (about 950 MB) in 50,100 folders |

**How each tool was used**
- **fkbr:** `fkbr cp /bench/SET targets:/dst`, no `-z`.
- **tar:** `tar -cf` onto the share on the source, then `tar -xf` from the
  share on every target at once.
- **rsync:** `rsync -rlpt` from the source onto the share (`-a` fails: the
  NAS refuses `chgrp`), then `rsync -a` from the share on every target at
  once.
- **borg 1.4.5:** `borg create` into a repository on the share (no
  encryption, default lz4), then `borg extract` on the target. Its static
  binary needs glibc 2.31, so only Rocky 9 and 10 can run it: one
  target only.

**Results**

| | fkbr | tar | rsync | borg |
|---|---|---|---|---|
| 1 GiB file → 1 target | 21 s | 21 s | 20 s | 28 s |
| 1 GiB file → 3 targets | **33 s** | 38 s | 38 s | – |
| 20,000 files → 1 target | 6.9 s | **6.0 s** | 2 min 33 s | 18 s |
| 20,000 files → 3 targets | **10 s** | 12 s | 2 min 58 s | – |
| 200,000 files → 1 target | 43 s | **32 s** | 27 min 41 s | 2 min 7 s |
| 200,000 files → 3 targets | 1 min 3 s | **57 s** | not run | – |
| 1,000,000 entries → 1 target | 3 min 38 s | **3 min 16 s** | not run | 9 min 46 s |
| 1,000,000 entries → 3 targets | 5 min 38 s | **5 min 20 s** | not run | – |

rsync was not run where it would have taken hours: at its pace, a million
files would take about 2 h 20 min to one target.

**What this shows**
- **Big files: the NAS sets the pace.** To one target every tool takes
  about 20 s per GiB. To three, fkbr is ahead: its targets read each chunk
  while the source is still writing the next, whereas tar and rsync must
  finish writing to the share before anyone can read.
- **Small files: fkbr and tar are close.** fkbr is ahead for 20,000 files
  to three targets and within 10–35% of tar for 200,000 and a million.
  What is left is the target's work per file: fkbr writes each file into
  a staging area first and moves it into place when everything has
  arrived, so nothing half-copied ever shows; tar extracts in place.
- **rsync through NFS is not an option for many files:** it creates every
  file on the share one by one, 20–40 times slower than packing them.
- **borg is a backup tool:** it is three to four times slower than tar for
  small files, and its strengths (deduplication, history) do not help a
  one-off copy.
- **What the numbers leave out.** With tar or rsync you run two steps by
  hand on every host, clean up the share afterwards, and nothing is
  verified, resumed or retried. During this test a target did not see the
  staging directory another host had just created, for as long as its NFS
  client cached it as missing; fkbr lists directories before reading them
  for exactly that reason (`docs/protocol.md`, rule 2).

**How fkbr gets there** (`docs/protocol.md`, Transfers and Chunk format)
- Its own compact chunk format: about 10 bytes per entry plus the part of
  its name that differs from the one before. For 200,000 files of 2 KiB,
  395 MiB go over the share for 391 MiB of data.
- The first chunk is 4 MiB and the sizes double up to `chunk_size`, so
  targets start within a second.
- The source walks ahead and reads small files with a few workers while it
  packs; targets read each chunk ahead while they write files.
- Targets set a file's metadata through the open file, create files in the
  directory they keep open, and move a directory that is new at the
  destination into place with one rename.

## fkbr and rsync over ssh (2026-10-23)

The same four VMs and data, no share: the Rocky 9 VM runs fkbr's home
node and links the targets over ssh (`fkbr ssh:HOST:/dir,...`). rsync
runs `rsync -a` over plain ssh, one per target at once. "Relayed" copies
between two linked hosts through a third (Rocky 8): fkbr's home node
passes the data on in memory; rsync uses `ssh -J`. Page caches dropped
before each run, every copy checked with sha256, one run each. fkbr
holds the chunks it sends in memory, never on disk.

| | fkbr | rsync |
|---|---|---|
| 1 GiB file → 1 target | 9.1 s | **6.6 s** |
| 1 GiB file → 3 targets | 21 s | **19 s** |
| 1 GiB file, relayed | 10.1 s | **9.0 s** |
| 20,000 files → 1 target | 4.1 s | 4.0 s |
| 20,000 files → 3 targets | **7.1 s** | 9.2 s |
| 20,000 files, relayed | **3.6 s** | 4.4 s |
| 200,000 files → 1 target | **21 s** | 26 s |
| 200,000 files → 3 targets | **45 s** | 1 min 6 s |
| 200,000 files, relayed | **20 s** | 27 s |

- **One big file:** rsync streams it; fkbr packs it into chunks, each
  with a SHA-256 checked on both ends, and stays a couple of seconds per
  GiB behind.
- **Many files, or several targets:** fkbr is ahead. It packs files into
  chunks and makes each chunk once for every target, where rsync sends
  each file on its own, once per target.
- Over NFS rsync is no match (above): it creates every file on the share
  one by one. Over ssh it is at its best, straight onto the target's
  disk.

## Sync (2026-10-19, four targets)

The 20,000-file mail store, `sync -z` to four targets:

| Step | Time |
|---|---|
| plan, first sync (20,201 entries per target) | 1.2 s |
| apply, first sync | 10.1 s |
| plan after 200 edits, 100 deletions, 100 new files | 0.8 s |
| apply (452 entries, 0.8 MiB per target) | 1.1 s |
| plan with `-mirror` (adds the 100 deletions) | 0.7 s |
| apply with `-mirror` | 1.9 s |
| re-plan when already in sync | 0.8 s |

## Sync of a million files (2026-10-19, one Pi)

Two nodes on the same Pi, 1,000,000 files in 10,101 directories:

| Step | Result |
|---|---|
| first sync plan | 1,010,101 entries in 18 s, 16 MB peak memory |
| first full copy | 7 min 42 s; `diff -rq` clean |
| re-plan | 0 to do, 23.6 s |
| 100 changed + 50 deleted files + 1 removed dir, mirror | 251 copies / 52 deletes planned in 23.9 s, applied in 2.2 s |

Memory does not grow with the number of files: scans stream snapshots to
the share, and the planner merges them in one pass.

## Choosing the codec (2026-10-19)

`-z` compresses chunks with zstd (fastest level, 1 MiB window, up to 2
threads). gzip and lz4 were measured too and dropped.

**On the lab:** `cp` from the Rocky 9 VM to the CentOS 7 and Rocky 8 VMs
over the NFSv3 share (2 runs each).

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
