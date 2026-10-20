# fkbr FAQ

**What is it?**
Copy and sync files between machines that share an NFS mount. One file, nothing to install.

**Which file?**
`fkbr-linux-amd64` for x86-64, `fkbr-linux-arm64` for ARM. Works on almost any Linux, old or new.

**How do I start?**
On every machine:
```
cd /dir/to/share
./fkbr /mnt/nfs/somedir
```
This opens the TUI. The machine joins under its hostname and shares the folder you are in.

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

**A machine was offline during a copy?**
It gets nothing; the others get everything. Catch it up later: F9, pick the job, press `u` (or `./fkbr /mnt/nfs/somedir catchup JOB`). A sync works too.

**Something broke halfway?**
Nothing half-copied ever appears: files are put in place only when all of them have arrived and been checked. Restart fkbr and the copy goes on where it stopped.

**How fast is it?**
About as fast as tar through the same share, and faster when copying to several machines at once. The NFS server's network sets the pace; a bigger `job_quota` does not help. Numbers: `docs/benchmarks.md`.

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
Another user (or `sudo`) already runs fkbr here under that name. Use that one, or start yours with `-node othername`.

**Two copies into the same folder at once?**
Safe. Every file arrives whole. If both copies have the same file, you get one of them; which one is not defined.

**Share read-only?**
Start with `-ro`.

**Different users on each machine?**
That works. fkbr opens its folders on the share to everyone, so keep the share where only your machines can reach it.

**Does it clean up after itself?**
Yes, but only while it runs. Think of fkbr as a mail carrier who tidies the mailbox while on duty. Letters are thrown away once delivered. Small notes about finished jobs stay for an hour, then the carrier throws those away too. If no carrier is on duty, nothing gets tidied until one shows up. There is no cron job or service doing it for you.

**Someone deleted things in the share?**
Running machines put back what they need within seconds. Copies that were running may need to be started again.

**Tidy up right now?**
`./fkbr /mnt/nfs/somedir prune -keep 0`
Removes everything finished. Copies still running are left alone.

**Uninstall?**
Delete the file, `/mnt/nfs/somedir`, and fkbr's files in `/tmp` (`/tmp/fkbr*`).

**More?**
`docs/operations.md`
