# Test plan for a cloud session (about 4 h)

A run of benchmarks and fault tests on one machine, over a share and
over ssh, with one big file and with 200,000 small files. It ends in a
report the owner checks on real hosts. Read `CLAUDE.md` first ("Cloud
sessions" and the hard requirements).

## Rules

- Work on your session's branch; commit and push there. Never touch
  `master`.
- **Report, don't fix.** Don't change product code (`cmd/`, `internal/`)
  or user docs (`docs/`, `README.md`). Scripts and results go under
  `testruns/<YYYY-MM-DD>-cloud/` on your branch.
- **This repo is public.** Nothing you commit may hold a host name, IP
  address, user name or e-mail address. Write "the machine", "node a".
- Push at least every hour (partial results must survive a cut-off),
  and stop starting new work at 3 h 45 min to write the report.
- Durations in the report as h/min/s (`1 min 6 s`, `7.6 s`), not raw
  seconds. Every timed run ends with a checksum comparison of source and
  every target; a run that doesn't match is a finding, not a number.
- Use `t.TempDir()`-style scratch under `/tmp/fkbr-cloud/` and remove it
  at the end. Anchor every `pkill`/`pgrep` pattern with `^`.

## 0. Setup (20 min)

1. `make`, `make vet`, `go test ./...`. Note failures, keep going.
2. Record the machine in `testruns/.../MACHINE.md`: CPU model, cores,
   flags that matter (`grep -o -w 'sha_ni\|aes\|avx2\|avx512f\|pclmulqdq'
   /proc/cpuinfo | sort -u`, or `sha2 aes pmull crc32` on arm64), RAM,
   free disk under `/tmp`, filesystem of `/tmp`, kernel, Go version,
   `rsync --version` (with its checksum list), `ssh -V`.
3. **Disk:** the sets below need about 15 GB with 3 targets. With less,
   use 2 targets and say so.
4. **Page cache:** try `sync; echo 3 | sudo tee /proc/sys/vm/drop_caches`.
   If it works, drop caches before every timed run; if not, every run is
   warm-cache: say so in the report.
5. **NFS:** try for at most 15 min to get a real NFS mount on this
   machine (kernel nfsd or a userspace server, exported and mounted on
   localhost). If it works, run section 2 on it and on a plain
   directory. If not, the share is a plain directory (as in the engine
   tests) and the report says the NFS numbers are not NFS.
6. **ssh:** try for at most 15 min to run sshd as yourself on a high port
   (`ssh-keygen` a host key and a user key under `/tmp/fkbr-cloud/ssh`,
   `/usr/sbin/sshd -f <config> -p 2222`, an `authorized_keys`, and a
   `~/.ssh/config` with hosts `fk-a`, `fk-b`, `fk-c` all pointing at
   127.0.0.1 port 2222, `StrictHostKeyChecking accept-new`). Linked
   nodes then get names: `fkbr ssh:a=fk-a:/tmp/fkbr-cloud/a,b=fk-b:...`
   with `-ssh-bin` the built binary. If sshd is impossible, put a
   stand-in `ssh` first in `PATH` that drops ssh's options and runs the
   remote command locally (`sh -c`), and say every ssh number is
   without encryption.

## 1. Data sets

Random content (compression must not help), made once, kept until the
end:

| Set | Content |
|---|---|
| `big` | one 1 GiB file |
| `f200k` | 200,000 files of 1–3 KiB, in 2,000 folders (about 400 MiB) |
| `mixed` | 50 files of 20–60 MiB plus 20,000 files of 4–16 KiB |

## 2. Over a share (about 1 h)

Nodes on one machine as in `scripts/bench.sh`: one config per node
(`node_id`, `nfs_root`, `state_dir`, `socket` under `/tmp`, a root),
each node a `daemon`. Every case 3 times; report the median and the
spread.

- `cp` of `big`, `f200k`, `mixed` to 1 target and to 3 targets, with and
  without `-z`.
- The same with `tar` (`tar -cf` onto the share, `tar -xf` on every
  target at once) and `rsync -a` (onto the share, then from it on every
  target at once) as baselines. For `f200k` skip rsync if one run passes
  10 min, and say so.
- `sync` of `f200k` to 2 targets: first sync; re-plan when nothing
  changed; then change 200 files, delete 100, add 100 and sync with
  `-mirror`; then `sync -checksum` when nothing changed. Plan time and
  apply time separately.
- Peak memory (`VmHWM` in `/proc/<pid>/status`, sampled every 0.5 s) of
  source and targets for `big` and `f200k`.

## 3. Over ssh (about 1 h)

A home node (`fkbr ssh:... daemon` in its own directory) links the
other nodes; `fkbr ... cp` from the home directory submits. Every case 3
times; median and spread.

- `big`, `f200k`, `mixed`: home → 1 target, home → 3 targets, and
  relayed (a linked node to another linked node through the home).
- Baseline: `rsync -a` over ssh to the same hosts (one per target at
  once), and `rsync -a -e 'ssh -J fk-a'` for relayed.
- Peak memory of the home node and of each `fkbr link` process during
  `big` and `f200k`. Over ssh the source holds chunks in memory: check
  that it stays inside `staging_quota` (a quarter of RAM, at most
  512 MiB) plus a margin, and say how much.
- One run of `big` and of `f200k` with the home node started with
  `-chunk-size 64M -job-quota 256M` (it passes them on to its links), to
  see whether the defaults (32M / 128M) are right on this machine.

## 4. Faults under load (about 45 min)

On `big` and on `f200k`, over ssh and over the share. After each: job
state, every target's checksum, nothing left in staging (`.fkbr-tmp`),
no chunk files on any disk over ssh. Time to recover.

- kill -9 the source's node mid-transfer, start it again.
- kill -9 a target's node mid-transfer, start it again.
- SIGSTOP a target for 20 s, then SIGCONT (over ssh: the `fkbr link`
  process).
- Over ssh: kill -9 the `ssh` process of one link mid-transfer (the home
  node starts it again).
- Over ssh: kill -9 the home node mid-transfer, start it again.
- A source file changed during the copy (append to a file in `mixed`
  while it is sent): the job must say so, the target must not get a
  half-changed file.

## 5. The open race failure (about 45 min, may run beside section 2)

Under `-race`, ssh-mode engine tests failed twice without a known
cause: `TestSSHSourceRestart` once in 38 runs ("link timed out"; no
300 ms timeout exists in that test, so what timed out is unknown), and
once in 8 runs a test whose output was lost.

1. Loop `FKBR_TEST_TRANSPORT=ssh GOTRACEBACK=all go test -race -count=1
   -timeout 5m -run 'TestSSH' ./internal/engine` as long as the budget
   allows (a few seconds a round), keeping the **full output of every
   failing round**.
2. Then loop the whole ssh-mode engine suite under `-race` the same way.
3. For a failure: which test, the exact error line, which call or timer
   ran out, and the goroutines on both ends at that moment. Say whether
   it can happen outside a test (with the real timers in
   `internal/config`) or only with the test's short ones.

On a machine with 2 CPUs run single passes (`-count=1`, one package at a
time).

## 6. Report (15 min)

In `testruns/<date>-cloud/`:

- `RESULTS.md`: the machine in one paragraph, then one table per
  section (median, spread, fkbr vs the baseline), and what the numbers
  say in a few lines each. Say plainly what is not like real hosts
  (no NFS, warm cache, all nodes on one disk, ssh stand-in).
- `FINDINGS.md`: one entry per finding: what happened against what was
  expected, how to reproduce it (exact commands), the log lines, and how
  sure you are (seen once / every time). Findings are bugs, wrong
  results, leftovers, hangs, memory over the bounds, or a number far off
  `docs/benchmarks.md` that the environment doesn't explain.
- `logs/`: raw outputs (trim anything over a few MB); `scripts/`: what
  you ran, so the owner can rerun it on the lab.
- At the end of `RESULTS.md`, "Check on real hosts": what this machine
  could not show (attribute caching, several hosts, NAS cuts, real
  network latency).

Commit, push your branch, and end with a short summary: runs per
section, findings, and what the owner should validate first.
