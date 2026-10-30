#!/usr/bin/env bash
# take.sh: rebuild the demo swarm and retake the README screenshots into
# docs/screenshots (SCALE=2 for double size). Needs pyte and pillow,
# and DejaVu fonts. With uv installed nothing else is needed (uv fetches
# them into its cache); otherwise PY=/path/to/python with both installed.
# Run `make` first; it uses bin/fkbr. The demo lives in /tmp/fkbr-demo.
set -u
cd "$(dirname "$0")/../.."
H=$PWD/scripts/screenshots
BIN=$PWD/bin/fkbr
OUT=$PWD/docs/screenshots
if [ -z "${PY:-}" ] && command -v uv > /dev/null; then
	py() { uv run -q --with pyte --with pillow python -I "$@"; }
else
	py() { "${PY:-python3}" -I "$@"; }
fi
stop() { pkill -TERM -f '[f]kbr-demo/share daemon'; sleep 1; }
# The demo's own home: its ~/.fkbr.conf is what the settings shot shows,
# never the user's.
DEMOHOME=/tmp/fkbr-demo-home
stage() {
	stop
	rm -rf /tmp/fkbr-demo $DEMOHOME /tmp/fkbr-$(id -u)-{ops01,web01,db01,backup01,build01}
	mkdir -p $DEMOHOME
	printf '%s\n' '# fkbr config: F11 in the TUI changes settings here' 'roles: [ops]' \
		'tui:' '  exclude: .git *.tmp' 'transfer:' '  max_jobs: 3' > $DEMOHOME/.fkbr.conf
	HOME=$DEMOHOME bash "$H/demo-setup.sh" "$BIN" > /tmp/fkbr-demo-setup.log 2>&1
	# Folders get a date of the demo's, not the day the shots are taken.
	find /tmp/fkbr-demo -mindepth 2 -type d ! -path '/tmp/fkbr-demo/share/*' -exec touch -d "2026-10-18 21:40" {} +
	touch -d "2026-10-19 02:05" /tmp/fkbr-demo/db01/dumps
	touch -d "2026-10-19 10:02" /tmp/fkbr-demo/backup01/incoming
}
run() { # steps file; screens.txt shows the text of each shot
	(cd /tmp/fkbr-demo/ops01 && export COLORTERM=truecolor TERM=xterm-256color &&
		py "$H/shoot.py" "$OUT" "$1" env HOME=$DEMOHOME "$BIN" -node ops01 ${THEME:+-theme $THEME} /tmp/fkbr-demo/share) >> /tmp/fkbr-demo-screens.txt 2>&1
}
: > /tmp/fkbr-demo-screens.txt
stage; run "$H/readme.steps"
stage; run "$H/progress.steps"   # a fresh demo, so the copy is the only running job
stage; run "$H/sync.steps"       # a fresh TUI: the header ticker starts after 30 s
# The other themes: the main screen in each, in one grid.
grid=()
for t in blue red yellow black; do
	stage; THEME=$t run "$H/theme.steps"
	mv "$OUT/fkbr-theme.png" "/tmp/fkbr-demo-theme-$t.png"
	grid+=("$t=/tmp/fkbr-demo-theme-$t.png")
done
py "$H/themes.py" "$OUT/fkbr-themes.png" "${grid[@]}"
stop
echo "screenshots in $OUT; screen text in /tmp/fkbr-demo-screens.txt"
