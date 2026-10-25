# fkbr FAQ

## Getting started

**What is it?**
Copy and sync files between machines that share an NFS mount, or that reach each other with ssh. One file, nothing to install.

**Where do I get it?**
From the [releases page](https://github.com/grajsor/fkbr/releases/latest): `fkbr-linux-amd64.tar.gz` for x86-64, `fkbr-linux-arm64.tar.gz` for ARM (a `.zip` too). Unpack it with `tar -xzf fkbr-linux-amd64.tar.gz`; the program is the file `fkbr`.

**Which systems?**
Linux only, x86-64 or ARM64, old or new: from CentOS 7 on. Not Windows or macOS.

**Where do I put it?**
On each machine's own disk, not on the share: anyone who can write to the share could swap it. If you keep a config file beside it (`fkbr.conf`), that goes there too.

**How do I start?**
On every machine:
```
cd /dir/to/share
./fkbr /mnt/nfs/somedir
```
This opens the TUI. The machine joins under its hostname and shares the folder you are in.

**No NFS, only ssh?**
Start it on one machine with the others listed: `./fkbr ssh:web01:/srv/www,db01:/data`. Or just `./fkbr`, then F2 and F7 to add a machine; it is kept in the config file for next time. The other machines need the fkbr file too, and ssh must log in without a password (a key). See [operations.md](operations.md), "Over ssh".

**No share at all?**
`./fkbr` alone opens the TUI here: this folder on the left, `/` on the right. Nobody else sees `/`.

**How do I change its settings?**
Press F11 in it: colours, mouse, what copies start with, the machine's name and share, limits. They are kept in `~/.fkbr.conf`; a `fkbr.conf` beside fkbr, if you make one, is used instead.

## Copying and syncing

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

**See what a sync will delete?**
`fkbr /mnt/nfs/xfer sync -n -mirror /src hostb:/dst` lists it (add `-v` for everything). In the TUI, F4 shows the first ones; press `l` for all.

**Someone changed a file on the target; will sync overwrite it?**
Yes, a sync makes the target match the source. Add `-u` (Ctrl+N in the TUI) to keep files that are newer on the target.

**Move, delete or make folders from a script?**
`fkbr /mnt/nfs/xfer mv /a hostb:/b`, `rm -r hostb:/old`, `mkdir hostb:/new`. `mv` deletes the sources only if the copy fully succeeded.

**Leave some files out?**
`-exclude '*.tmp' -exclude .git` on `cp` or `sync`, or the "exclude" field in the TUI. In a sync, excluded files are never copied and never deleted. Details: [operations.md](operations.md).

**Leave out the folders `jobs` and `plugins` and all `.json` files?**
In the TUI's "exclude" field, type `jobs/ plugins/ *.json`: spaces between, no quotes. The `/` at the end means folders only. This leaves them out at any depth; for only the ones at the top of what you sync, type `/jobs/ /plugins/ *.json`. In the CLI, quote each one: `-exclude 'jobs/' -exclude 'plugins/' -exclude '*.json'`.

**A sync plan takes long?**
The status line shows which machine it scans and in which folder. A folder that should be left out but shows up there needs an exclude.

**Compress?**
Add `-z` to `cp` or `sync` (Ctrl+Z in the dialogs). It helps with text, logs and programs, and costs almost nothing for files that are already compressed.

**Big files that are mostly empty, like disk images?**
Only the data travels; the empty parts stay empty on the target too, so the copy takes no more disk than the original.

**Two copies into the same folder at once?**
Safe. Every file arrives whole. If both copies have the same file, you get one of them; which one is not defined.

**Copy to a USB stick (FAT)?**
Works, also exFAT. Names the stick cannot hold (`a:b`, `name.`, symlinks, or `README` next to `readme`) are skipped and reported; the rest arrives. Sync knows this, so it does not try them again, and an exclude like `cache/` also keeps `CACHE` there.

## In the TUI

**Look at a file, or find text in it?**
F3 shows it, from any machine. Ctrl+W finds; Enter alone finds the next. Ctrl+W works in the help (F1) too.

**Can I edit a file on another machine?**
Yes, a text file up to 64 KiB. Open it with F3, press Ctrl+E, change it, Ctrl+S saves. The keys are nano's (F1 lists them). If someone changed the file since you opened it, fkbr says so and does not save.

**I closed the editor without saving?**
Ctrl+X or Esc asks first: Ctrl+S saves, Ctrl+X throws the changes away, Esc goes back to the text. Plain letters do nothing there, so typing on can't answer by mistake.

**Mouse?**
Click to move, double-click to open, right-click to mark (hold and drag to mark more), wheel or scrollbar to scroll. In a dialog, click Enter, Esc or an option. In the editor, a click puts the cursor there. To select text, hold Shift (or start with `-mouse=false`).

**A `?` in a file name?**
The name holds a character your terminal could act on, like an escape or a line break. fkbr shows it as `?`. Copies keep the real name.

**Which version?**
`./fkbr version`, or watch the top-left corner of the TUI: every 30 seconds the version scrolls by.

## Speed and safety

**How fast is it?**
In the lab it beat tar, rsync and borg through the same share in every test, most of all with many small files and with several targets at once. Over ssh it is as fast as rsync or faster. The NFS server's network sets the pace; a bigger `job_quota` does not help. Numbers: [benchmarks.md](benchmarks.md).

**Why so much faster than rsync for many small files?**
rsync through the share makes every file on the NFS server one at a time. Each file costs several trips to the server (create, write, set the time, rename), and each trip waits for an answer. 20,000 files means about 100,000 waits. fkbr packs many small files into a few big chunks, like sending one parcel instead of 20,000 letters. The server sees a few big writes, and each machine unpacks onto its own disk. All targets read the same chunks, too. In the lab, 20,000 small files took fkbr 5.6 s and rsync 2 min 16 s.

**Can I trust it with my files?**
It is tested by breaking things on purpose while it copies: killed and frozen processes, cut networks, full disks, clocks that jump, folders deleted mid-copy. Every copy is then checked file by file. No run has ever lost or damaged a file. See [stability.md](stability.md).

**Something broke halfway?**
Nothing half-copied ever appears: files are put in place only when all of them have arrived and been checked. Restart fkbr and the copy goes on where it stopped.

**Different users on each machine?**
That works. fkbr opens its folders on the share to everyone, so keep the share where only your machines can reach it.

## When something goes wrong

**Something failed?**
In the TUI: F9, pick the job, Enter. Every machine's problems are listed there. More detail is in the log: F1 shows where it is, or for a daemon, the file you sent its output to.

**A machine was offline during a copy?**
It gets nothing; the others get everything. Catch it up later: F9, pick the job, press `u` (or `./fkbr /mnt/nfs/somedir catchup JOB`). A sync works too. The same works for a machine whose copy failed, say when its ssh link was down too long.

**Is my setup OK?**
`./fkbr /mnt/nfs/somedir check`

**Who's online?**
`./fkbr /mnt/nfs/somedir nodes`

**Two machines with the same hostname?**
Start one with `-node othername`.

**"already running on this host"?**
Another user (or `sudo`) already runs fkbr here under that name. Use that one, or start yours with `-node othername`. Started right after stopping the old one? Then it waits until the old one has stopped.

**"belongs to uid N, not to this user"?**
Someone else made fkbr's folder in `/tmp` before you did, so fkbr does not trust it. Ask them, or root, to remove it, or start with `-state /some/dir/of/yours`.

**Someone deleted things in the share?**
Running machines put back what they need within seconds. Copies that were running may need to be started again.

## Running it

**Keep it running after logout?**
```
cd /dir/to/share
nohup ./fkbr /mnt/nfs/somedir daemon > /tmp/fkbr.log 2>&1 &
```
The log goes to `/tmp`, not into the folder you share.

**Stop it?**
F10 or `q` in the TUI; `kill` for a daemon. Unfinished copies go on next time.

**Upgrading?**
Run the same version on every machine. If the versions do not fit together, fkbr says so and tells you what to do.

**Share read-only?**
Start with `-ro`.

**The share is inside the folder I share?**
Fine (`cd ~ && fkbr ~/box`). fkbr leaves the share out.

**Remove a machine that's gone?**
`./fkbr /mnt/nfs/somedir forget NAME`. You rarely need to: quitting the TUI removes the machine, and machines gone for 7 days are removed by themselves. A stopped daemon stays, so it can catch up on `@all` copies later.

**Does it clean up after itself?**
Yes, but only while it runs. Think of fkbr as a mail carrier who tidies the mailbox while on duty. Letters are thrown away once delivered. Small notes about finished jobs stay for an hour, then the carrier throws those away too. If no carrier is on duty, nothing gets tidied until one shows up. There is no cron job or service doing it for you.

**Tidy up right now?**
`./fkbr /mnt/nfs/somedir prune -keep 0`
Removes everything finished (what ended in the last minute stays a little longer). Copies still running are left alone.

**Uninstall?**
Delete the file, `/mnt/nfs/somedir`, fkbr's files in `/tmp` (`/tmp/fkbr*`), and `~/.fkbr.conf` or `fkbr.conf` if you have one.

**More?**
[operations.md](operations.md)
