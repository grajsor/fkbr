#!/usr/bin/env bash
# take.sh: rebuild the demo swarm and retake the README screenshots into
# docs/screenshots. Needs python3 with pyte and pillow, and DejaVu fonts:
#   python3 -m venv /tmp/shots-venv && /tmp/shots-venv/bin/pip install pyte pillow
#   PY=/tmp/shots-venv/bin/python scripts/screenshots/take.sh
# Run `make` first; it uses bin/fkbr. The demo lives in /tmp/fkbr-demo.
set -u
cd "$(dirname "$0")/../.."
H=$PWD/scripts/screenshots
BIN=$PWD/bin/fkbr
OUT=$PWD/docs/screenshots
PY=${PY:-python3}
stop() { pkill -TERM -f '[f]kbr-demo/share daemon'; sleep 1; }
stage() {
	stop
	rm -rf /tmp/fkbr-demo /tmp/fkbr-$(id -u)-{ops01,web01,db01,backup01,build01}
	bash "$H/demo-setup.sh" "$BIN" > /tmp/fkbr-demo-setup.log 2>&1
	touch -d "2026-10-19 02:05" /tmp/fkbr-demo/db01/dumps
	touch -d "2026-10-19 10:02" /tmp/fkbr-demo/backup01/incoming
}
run() { # steps file; screens.txt shows the text of each shot
	(cd /tmp/fkbr-demo/ops01 && env COLORTERM=truecolor TERM=xterm-256color \
		"$PY" -I "$H/shoot.py" "$OUT" "$1" "$BIN" -node ops01 /tmp/fkbr-demo/share) > /tmp/fkbr-demo-screens.txt 2>&1
}
stage; run "$H/readme.steps"
stage; run "$H/progress.steps"   # a fresh demo, so the copy is the only running job
stop
echo "screenshots in $OUT; screen text in /tmp/fkbr-demo-screens.txt"
