# Hybrid Deployment Tier — export-layout cutover and fstab hardening

Branch `scion/hybrid-tier-p2-fix`.

## Overview

Hardens the export-layout guidance's cutover procedure and fstab entry in
`docs/deploy/hybrid-tier.md`.

## Cutover procedure: never delete from a path a mount can shadow

The cutover procedure copies the existing tree into a temporary mountpoint
first (`rsync -aHAX`, preserving hard links and ACLs), stops the hub, agents,
and `nfs-server`, takes a final quick sync to catch anything written since
the bulk copy, unmounts the temporary mountpoint, and only then moves the
now-unmounted old tree aside to `/srv/scion-shared.old` -- a plain directory
nothing is ever mounted over. The final cleanup step names that path
explicitly, so it can never remove anything other than the retired copy. A
rollback covers the window from the start of the step that recreates the
export mountpoint and adds the fstab line -- including a failed attempt
at that step -- up to that final cleanup: stop the hub and agents, stop
`nfs-server`, unmount, remove the fstab line, reload systemd (so the
just-removed mount unit is no longer live), move `.old` back into place,
and restart `nfs-server`, the hub, and agents. A separate, shorter form
covers stopping before that step is even started: just unmount the
temporary mountpoint (and move `.old` back if it was already moved).
It also notes that anything written after the last rsync stays only on
the image (recoverable by loop-mounting the image read-only and copying
it out), and that the image itself needs to be unmounted if still
mounted, then removed (or reused without re-formatting) before retrying
the cutover, since the create step's own guard refuses an image that
already exists.

## fstab entry: `nofail`

The fstab entry combines `x-systemd.before=nfs-server.service` and
`x-systemd.required-by=nfs-server.service`, so `nfs-server.service` won't
start if the mount fails. Because `required-by=` is set, `systemd-fstab-
generator` does not also attach this mount to `local-fs.target`
(systemd.mount(5)), so a failed mount blocks only `nfs-server`, never boot.
`nofail` is kept anyway, defensively: it keeps that same "boot never blocks
on this mount" property true even if `required-by=` is ever removed from
this line.

## Other fixes to the same recipe

- The fsck pass is `0`: systemd-fstab-generator only schedules fsck for
  device paths, so a nonzero pass on a loop-mounted regular file is a no-op
  that just logs a boot-time warning.
- One line of sizing guidance: size the image for the scratchpad's expected
  footprint, leaving headroom on `/`, and remember a cutover also needs free
  space for the image on top of the tree it's copying.
- The separate-disk variant of the recipe: skip `fallocate`, guard the
  format with `blkid -p <dev>` (format only if it reports no filesystem),
  use `UUID=<uuid>` or `/dev/disk/by-id/...` in fstab rather than a raw
  device name that can change across reboots, and use fsck pass `2` rather
  than `0`, since fsck does apply to a real device.
- The exports(5) `mp` option's enforcement is nfs-utils userspace
  (`exportfs`/`rpc.mountd`), not the kernel.
- The cutover's rsync commands use `-aHAX`, preserving hard links.

## Verification

`docs/deploy/hybrid-tier.md`'s recipe and cutover blocks are prose/command
blocks, not executable from this repo; checked by eye against
systemd.mount(5), exports(5), and systemd-fstab-generator behaviour.
