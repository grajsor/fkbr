#!/usr/bin/env bash
# Benchmark fkbr with one master and N slaves on this machine, all using
# one directory as the "NFS" share. Prints a Markdown table.
#
#   scripts/bench.sh [-n slaves] [-large MiB] [-small count] [-tree count] [-dir workdir]
#
# Every node shares the same disk here, so a fan-out to N slaves writes
# (N+1) copies to one device: read the numbers as a lower bound, and run
# the same workloads on the real hosts and NFS server.
set -euo pipefail

SLAVES=3 LARGE_MIB=512 SMALL=50000 TREE=200000 WORK=""
while [ $# -gt 0 ]; do
	case "$1" in
	-n) SLAVES=$2; shift 2 ;;
	-large) LARGE_MIB=$2; shift 2 ;;
	-small) SMALL=$2; shift 2 ;;
	-tree) TREE=$2; shift 2 ;;
	-dir) WORK=$2; shift 2 ;;
	*) sed -n '2,10p' "$0"; exit 2 ;;
	esac
done
BIN=$(cd "$(dirname "$0")/.." && pwd)/bin/fkbr
[ -x "$BIN" ] || { echo "build first: make build" >&2; exit 1; }
WORK=${WORK:-$(mktemp -d /tmp/fkbr-bench.XXXXXX)}
mkdir -p "$WORK/share"
PIDS=()
cleanup() { kill "${PIDS[@]}" 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT

nodes=(master)
for i in $(seq 1 "$SLAVES"); do nodes+=("s$i"); done
for n in "${nodes[@]}"; do
	mkdir -p "$WORK/$n/data"
	cat > "$WORK/$n.yaml" <<CFG
node_id: $n
nfs_root: $WORK/share
state_dir: $WORK/$n/state
socket: /tmp/fkbr-bench-$$-$n.sock
roots: [{id: data, path: $WORK/$n/data, allow_write: true}]
CFG
	"$BIN" -config "$WORK/$n.yaml" daemon 2>"$WORK/$n.log" &
	PIDS+=($!)
done
M="$BIN -config $WORK/master.yaml"
for _ in $(seq 50); do
	[ "$($M nodes -json 2>/dev/null | grep -c '"online": true')" -eq ${#nodes[@]} ] && break
	sleep 0.2
done

now() { date +%s.%N; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.1f", b-a}'; }
rate() { awk -v mb="$1" -v s="$2" 'BEGIN{ if (s>0) printf "%.0f MiB/s", mb/s; else print "-" }'; }
peak() { awk '/VmHWM/{printf "%d", $2/1024}' "/proc/$1/status"; }
ROWS=()
row() { ROWS+=("| $1 | $2 | $3 | $4 |"); }

src=$WORK/master/data
echo "generating workloads in $WORK ..." >&2
mkdir -p "$src/large" "$src/small" "$src/tree"
for i in 1 2 3; do head -c "${LARGE_MIB}M" /dev/urandom > "$src/large/f$i.bin"; done
python3 -I - "$src/small" "$SMALL" "$src/tree" "$TREE" <<'PY'
import os, sys
small, ns, tree, nt = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
for i in range(ns):
    d = os.path.join(small, f"d{i // 1000:03d}")
    os.makedirs(d, exist_ok=True)
    with open(os.path.join(d, f"f{i:06d}"), "wb") as f:
        f.write(os.urandom(4096))
for i in range(nt):  # deep: 5 levels, 10 wide, files at the leaves
    p = os.path.join(tree, *[f"l{(i // 10**k) % 10}" for k in range(5, 0, -1)])
    os.makedirs(p, exist_ok=True)
    with open(os.path.join(p, f"f{i:07d}"), "wb") as f:
        f.write(b"x" * (i % 512))
PY

run_cp() { # name path MiB
	local t0 t1
	t0=$(now)
	$M cp "data/$2" @all:data/cp >/dev/null
	t1=$(now)
	row "$1 → ${SLAVES} nodes (cp, @all)" "$(secs "$t0" "$t1") s" "$(rate "$3" "$(secs "$t0" "$t1")")" "source $(du -sm "$src/$2" | cut -f1) MiB"
}
run_cp "3 × ${LARGE_MIB} MiB files" large $((3 * LARGE_MIB))
run_cp "$SMALL × 4 KiB files" small $((SMALL * 4 / 1024))

t0=$(now); $M sync -n data/tree @all:data/tree >"$WORK/plan.txt"; t1=$(now)
row "sync plan, $TREE files deep tree" "$(secs "$t0" "$t1") s" "-" "scan of master + ${SLAVES} slaves"
plan=$(awk '/^sync plan/{sub(":","",$3); print $3}' "$WORK/plan.txt")
t0=$(now); $M sync -plan "$plan" >/dev/null; t1=$(now)
row "sync apply (all new)" "$(secs "$t0" "$t1") s" "-" "one transfer, fanned out"
t0=$(now); $M sync -n data/tree @all:data/tree >"$WORK/replan.txt"; t1=$(now)
left=$(awk '$1 ~ /^s[0-9]/ {c+=$2} END{print c+0}' "$WORK/replan.txt")
row "sync re-plan (converged)" "$(secs "$t0" "$t1") s" "-" "$left entries left to copy"

mem=""
for i in "${!nodes[@]}"; do mem+="${nodes[$i]} $(peak "${PIDS[$i]}") MiB, "; done

echo
echo "| Workload | Time | Throughput | Notes |"
echo "|---|---|---|---|"
printf '%s\n' "${ROWS[@]}"
echo
echo "Peak daemon memory (VmHWM): ${mem%, }"
echo "Host: $(uname -m), $(nproc) CPUs, $(awk '/MemTotal/{printf "%.1f GiB", $2/1048576}' /proc/meminfo), share fs $(df -T "$WORK" | awk 'NR==2{print $2}')"
