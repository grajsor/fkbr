#!/usr/bin/env bash
# demo-setup.sh BIN: four fkbr daemons on a throwaway share under /tmp/fkbr-demo,
# each sharing a realistic tree, plus a finished copy and sync for the jobs panel.
set -euo pipefail
BIN=$1
D=/tmp/fkbr-demo
rm -rf "$D"
mkdir -p "$D/share"

f() { # path size date
	mkdir -p "$(dirname "$1")"
	head -c "$2" /dev/urandom > "$1"
	touch -d "$3" "$1"
}
sparse() { mkdir -p "$(dirname "$1")"; truncate -s "$2" "$1"; touch -d "$3" "$1"; }

W=$D/web01
f $W/site/index.html 18432 "2026-10-18 21:40"
f $W/site/about.html 9216 "2026-10-02 10:15"
f $W/site/css/main.css 24576 "2026-10-18 21:40"
f $W/site/js/app.js 141312 "2026-10-18 21:40"
f $W/site/img/logo.svg 6144 "2026-06-11 08:02"
f $W/site/img/hero.webp 388000 "2026-09-29 14:31"
f $W/logs/access.log 52428800 "2026-10-19 09:58"
f $W/logs/error.log 412000 "2026-10-19 09:41"
f $W/logs/access.log.1.zst 9437184 "2026-10-18 00:00"
f $W/releases/2026-10-17/site.tar.zst 3145728 "2026-10-17 16:20"
f $W/releases/2026-10-18/site.tar.zst 3170304 "2026-10-18 21:39"
f $W/nginx.conf 4096 "2026-08-21 12:00"
f $W/robots.txt 120 "2025-12-01 09:00"
f $W/sitemap.xml 30720 "2026-10-18 21:40"
f $W/deploy.sh 2048 "2026-09-04 11:12"
chmod +x $W/deploy.sh
ln -s releases/2026-10-18 $W/current

B=$D/db01
sparse $B/dumps/orders-2026-10-17.sql.zst 1288490188 "2026-10-17 02:05"
sparse $B/dumps/orders-2026-10-18.sql.zst 1302000000 "2026-10-18 02:05"
sparse $B/dumps/orders-2026-10-19.sql.zst 1311000000 "2026-10-19 02:05"
f $B/conf/postgresql.conf 29696 "2026-07-30 15:44"
f $B/conf/pg_hba.conf 5120 "2026-07-30 15:44"
f $B/scripts/vacuum.sh 1536 "2026-05-12 09:30"
f $B/README.txt 900 "2026-03-03 13:13"

K=$D/backup01
mkdir -p $K/incoming $K/archive/2026-09 $K/archive/2026-10
sparse $K/archive/2026-09/web01-full.tar.zst 7516192768 "2026-09-30 23:59"
sparse $K/archive/2026-10/web01-full.tar.zst 7650000000 "2026-10-15 23:59"
f $K/RETENTION.md 2400 "2026-01-20 10:00"

U=$D/build01
f $U/artifacts/fkbr-linux-amd64 10346656 "2026-10-19 09:55"
f $U/artifacts/fkbr-linux-arm64 9568416 "2026-10-19 09:55"
f $U/artifacts/SHA256SUMS 166 "2026-10-19 09:55"
mkdir -p $U/workspace/src $U/cache
cat > $U/workspace/config.xml <<'XML'
<?xml version="1.0" encoding="UTF-8"?>
<pipeline name="site">
  <!-- nightly build of the web site, published to web01 -->
  <description>Builds and packages the site</description>
  <source type="git">
    <url>ssh://git@scm.example.com/web/site.git</url>
    <branch>main</branch>
  </source>
  <schedule cron="0 2 * * *" timezone="UTC"/>
  <steps>
    <step name="test">make test</step>
    <step name="build">make release VERSION=${BUILD_NUMBER}</step>
    <step name="package">tar -C dist -c . | zstd -19 > site.tar.zst</step>
  </steps>
  <publish>
    <target node="web01" path="releases/${DATE}"/>
    <keep>14</keep>
  </publish>
  <concurrent>false</concurrent>
</pipeline>
XML
touch -d "2026-10-16 15:20" $U/workspace/config.xml

O=$D/ops01
mkdir -p $O
f $O/runbook.md 14000 "2026-10-12 08:45"

for n in web01 db01 backup01 build01; do
	(cd $D/$n && nohup setsid "$BIN" -node $n "$D/share" daemon > $D/$n.log 2>&1 < /dev/null &)
done
for _ in $(seq 50); do
	[ "$(cd $D/ops01 && "$BIN" -node ops01 "$D/share" nodes 2>/dev/null | grep -c online)" -ge 4 ] && break
	sleep 0.2
done
# History for the jobs panel: a copy and a sync, started from web01.
cd $D/web01
"$BIN" -node web01 "$D/share" cp -z /site backup01:/incoming/web01 > /dev/null
"$BIN" -node web01 "$D/share" sync -z /releases build01:/workspace/releases > /dev/null || true
plan=$("$BIN" -node web01 "$D/share" sync -n /releases build01:/workspace/releases | awk '/apply with/{print $NF}')
[ -n "$plan" ] && "$BIN" -node web01 "$D/share" sync -plan "$plan" > /dev/null || true
# The site has changed since that copy, and backup01's copy holds a page
# the site dropped: a sync has something to show.
f $W/site/contact.html 7168 "2026-10-19 09:20"
f $W/site/css/main.css 25088 "2026-10-19 09:20"
f $K/incoming/web01/site/promo-2026-09.html 11264 "2026-09-01 08:00"
"$BIN" -node web01 "$D/share" nodes
"$BIN" -node web01 "$D/share" jobs 2>/dev/null | head -5 || true
