# fkbr FAQ

**What is it?**
Copy and sync files between machines that share an NFS mount. One file, nothing to install.

**Which file?**
`fkbr-linux-amd64` for x86-64, `fkbr-linux-arm64` for ARM. Works on almost any Linux, old or new.

**Where do I put it?**
On each machine's own disk, not on the share: anyone who can write to the share could swap it. If you keep a config file beside it (`fkbr.conf`), that goes there too.

**How do I change its settings?**
Press F11 in it: colours, mouse, what copies start with, the machine's name and share, limits. They are kept in `~/.fkbr.conf`; a `fkbr.conf` beside fkbr, if you make one, is used instead.

**How do I start?**
On every machine:
```
cd /dir/to/share
./fkbr /mnt/nfs/somedir
```
This opens the TUI. The machine joins under its hostname and shares the folder you are in.

**No NFS, only ssh?**
Start it on one machine with the others listed: `./fkbr ssh:web01,db01:/data`. Or just `./fkbr`, then F2 and F7 to add a machine; it is kept in the config file for next time (`fkbr.conf` beside fkbr if you have one, else `~/.fkbr.conf`). They need the fkbr file too, and ssh must log in without a password (a key). See `operations.md`, "Over ssh".

**No share at all?**
`./fkbr` alone opens the TUI here: this folder on the left, `/` on the right. Nobody else sees `/`.

**How do I copy?**
In the TUI, mark files with Insert and press F5. F6 moves instead. In a shell:
```
./fkbr /mnt/nfs/somedir cp /file othermachine:/
./fkbr /mnt/nfs/somedir cp othermachine:/logs/app.log /
```
`othermachine:/logs` means the `logs` folder in what that machine shares.

**To every machine?**
Use `@all` as the destination, or press Ctrl+A in the F5 dialog.

**To some of them?**
In the TUI's node list, mark them with Insert. F5 then copies to them (`@dest`).

**Sync a folder?**
```
./fkbr /mnt/nfs/somedir sync /dir othermachine:/dir
```
Add `-n` to preview first. Add `-mirror` to also delete extra files.
In the TUI: F4. It syncs the folder you are in, or only the folders you marked, and always shows what it will do first.

**Someone changed a file on the target; will sync overwrite it?**
Yes, a sync makes the target match the source. Add `-u` (Ctrl+N in the TUI) to keep files that are newer on the target.

**Move, delete or make folders from a script?**
`fkbr /mnt/nfs/xfer mv /a hostb:/b`, `rm -r hostb:/old`, `mkdir hostb:/new`. `mv` deletes the sources only if the copy fully succeeded.

**Leave some files out?**
`-exclude '*.tmp' -exclude .git` on `cp` or `sync`, or the "exclude" field in the TUI. In a sync, excluded files are never copied and never deleted. Details: `docs/operations.md`.

**Leave out the folders `jobs` and `plugins` and all `.json` files?**
In the TUI's "exclude" field, type `jobs/ plugins/ *.json`: spaces between, no quotes. The `/` at the end means folders only. This leaves them out at any depth; for only the ones at the top of what you sync, type `/jobs/ /plugins/ *.json`. In the CLI, quote each one: `-exclude 'jobs/' -exclude 'plugins/' -exclude '*.json'`.

**A sync plan takes long?**
The status line shows which machine it scans and in which folder. A folder that should be left out but shows up there needs an exclude.

**See what a sync will delete?**
`fkbr /mnt/nfs/xfer sync -n -mirror /src hostb:/dst` lists it (add `-v` for everything). In the TUI, F4 shows the first ones; press `l` for all.

**A machine was offline during a copy?**
It gets nothing; the others get everything. Catch it up later: F9, pick the job, press `u` (or `./fkbr /mnt/nfs/somedir catchup JOB`). A sync works too. The same works for a machine whose copy failed, say when its ssh link was down too long.

**Something broke halfway?**
Nothing half-copied ever appears: files are put in place only when all of them have arrived and been checked. Restart fkbr and the copy goes on where it stopped.

**How fast is it?**
About as fast as tar through the same share, and faster when copying to several machines at once. The NFS server's network sets the pace; a bigger `job_quota` does not help. Numbers: `docs/benchmarks.md`.

**Why so much faster than rsync for many small files?**
rsync through the share makes every file on the NFS server one at a time. Each file costs several trips to the server (create, write, set the time, rename), and each trip waits for an answer. 20,000 files means about 100,000 waits. fkbr packs many small files into a few big chunks, like sending one parcel instead of 20,000 letters. The server sees a few big writes, and each machine unpacks onto its own disk. All targets read the same chunks, too. In the lab, 20,000 small files took fkbr 7 s and rsync 2 min 33 s.

**Compress?**
Add `-z` to `cp` or `sync` (Ctrl+Z in the dialogs). It helps with text, logs and programs, and costs almost nothing for files that are already compressed.

**Who's online?**
`./fkbr /mnt/nfs/somedir nodes`

**Something failed?**
In the TUI: F9, pick the job, Enter. Every machine's problems are listed there. More detail is in the log: F1 shows where it is, or for a daemon, the file you sent its output to.

**Is my setup OK?**
`./fkbr /mnt/nfs/somedir check`

**Keep it running after logout?**
```
cd /dir/to/share
nohup ./fkbr /mnt/nfs/somedir daemon > /tmp/fkbr.log 2>&1 &
```
The log goes to `/tmp`, not into the folder you share.

**Stop it?**
F10 or `q` in the TUI; `kill` for a daemon. Unfinished copies go on next time.

**Which version?**
`./fkbr version`, or watch the top-left corner of the TUI: every 30 seconds the version scrolls by.

**Upgrading?**
Run the same version on every machine. If the versions do not fit together, fkbr says so and tells you what to do.

**Remove a machine that's gone?**
`./fkbr /mnt/nfs/somedir forget NAME`. You rarely need to: quitting the TUI removes the machine, and machines gone for 7 days are removed by themselves. A stopped daemon stays, so it can catch up on `@all` copies later.

**Two machines with the same hostname?**
Start one with `-node othername`.

**"already running on this host"?**
Another user (or `sudo`) already runs fkbr here under that name. Use that one, or start yours with `-node othername`. Started right after stopping the old one? Then it waits until the old one has stopped.

**"belongs to uid N, not to this user"?**
Someone else made fkbr's folder in `/tmp` before you did, so fkbr does not trust it. Ask them, or root, to remove it, or start with `-state /some/dir/of/yours`.

**Two copies into the same folder at once?**
Safe. Every file arrives whole. If both copies have the same file, you get one of them; which one is not defined.

**Share read-only?**
Start with `-ro`.

**The share is inside the folder I share?**
Fine (`cd ~ && fkbr ~/box`). fkbr leaves the share out.

**A `?` in a file name?**
The name holds a character your terminal could act on, like an escape or a line break. fkbr shows it as `?`. Copies keep the real name.

**Mouse?**
Click to move, double-click to open, right-click to mark (hold and drag to mark more), wheel or scrollbar to scroll. In a dialog, click Enter, Esc or an option. To select text, hold Shift (or start with `-mouse=false`).

**Copy to a USB stick (FAT)?**
Works, also exFAT. Names the stick cannot hold (`a:b`, `name.`, symlinks, or `README` next to `readme`) are skipped and reported; the rest arrives. Sync knows this, so it does not try them again, and an exclude like `cache/` also keeps `CACHE` there.

**Different users on each machine?**
That works. fkbr opens its folders on the share to everyone, so keep the share where only your machines can reach it.

**Does it clean up after itself?**
Yes, but only while it runs. Think of fkbr as a mail carrier who tidies the mailbox while on duty. Letters are thrown away once delivered. Small notes about finished jobs stay for an hour, then the carrier throws those away too. If no carrier is on duty, nothing gets tidied until one shows up. There is no cron job or service doing it for you.

**Someone deleted things in the share?**
Running machines put back what they need within seconds. Copies that were running may need to be started again.

**Tidy up right now?**
`./fkbr /mnt/nfs/somedir prune -keep 0`
Removes everything finished (what ended in the last minute stays a little longer). Copies still running are left alone.

**Uninstall?**
Delete the file, `/mnt/nfs/somedir`, and fkbr's files in `/tmp` (`/tmp/fkbr*`).

**More?**
`docs/operations.md`
