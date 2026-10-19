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

**How do I copy?**
In the TUI, select files and press F5. In a shell:
```
./fkbr /mnt/nfs/somedir cp /file othermachine:/
```

**To every machine?**
Use `@all` as the destination, or press Ctrl+A in the F5 dialog.

**Sync a folder?**
```
./fkbr /mnt/nfs/somedir sync /dir othermachine:/dir
```
Add `-n` to preview first. Add `-mirror` to also delete extra files.

**Compress?**
Add `-z` to `cp` or `sync` (Ctrl+Z in the dialogs). It uses zstd. It helps with text, logs and programs, and costs nothing for files that are already compressed.

**Who's online?**
`./fkbr /mnt/nfs/somedir nodes`

**Something failed in the TUI?**
Look in `/tmp/fkbr-<your uid>-<machine>/node.log`. F1 shows the exact path.

**Is my setup OK?**
`./fkbr /mnt/nfs/somedir check`

**Keep it running after logout?**
`nohup ./fkbr /mnt/nfs/somedir daemon &`

**Stop it?**
F10 in the TUI, or kill it. Unfinished jobs resume next time.

**Remove a machine that's gone?**
`./fkbr /mnt/nfs/somedir forget NAME`. You rarely need to: quitting the TUI removes the machine, and machines gone for 7 days are removed by themselves. A stopped daemon stays, so it can catch up on `@all` copies later.

**Two machines with the same hostname?**
Start one with `-node othername`.

**Share read-only?**
Start with `-ro`.

**Different users on each machine?**
That works. fkbr opens its folders on the share to everyone, so keep the share where only your machines can reach it. If a node says "no answer within 30s" after an update from an older version, run `./fkbr /mnt/nfs/somedir check` on each machine.

**Does it clean up after itself?**
Yes, but only while it runs. Think of fkbr as a mail carrier who tidies the mailbox while on duty. Letters are thrown away once delivered. Small notes about finished jobs stay for an hour, then the carrier throws those away too. If no carrier is on duty, nothing gets tidied until one shows up. There is no cron job or service doing it for you.

**Tidy up right now?**
`./fkbr /mnt/nfs/somedir prune -keep 0`
Removes everything finished. Jobs still running are left alone.

**Uninstall?**
Delete the file and `/mnt/nfs/somedir`.

**More?**
`docs/operations.md`
