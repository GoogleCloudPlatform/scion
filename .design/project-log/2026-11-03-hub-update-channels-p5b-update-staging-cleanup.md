# Project Log - 2026-11-03 - Hub update channels, P5b: update staging left behind after a successful update

Tracking: ptone/scion#7177 (found by the P5 post-merge VM test, F2). Branch `scion/hu-p5b` from upstream main
`444807c22`.

## Problem
After a successful `update-binary`, `/tmp/scion-update-*` (the downloads, about 2.76 GB on the VM over several
updates) and `~/.scion/plugins/broker/.scion-update-*` (empty `new/` and `old/`) were left behind. Both were
removed only after `Installer.Restart` returned, but the restart (self-SIGTERM, or `systemctl restart`) usually
ends the process first. The next update then removed the plugin staging with a misleading "interrupted or
failed" warning, and the downloads piled up.

## Changes
- **`pkg/hub/maintenance/binary_update.go`.** After a successful Activate and plugin apply, and before the
  before-restart hook and Restart:
  - the download temp directory is removed. Stage has copied the hub binary, and the plugin swap its new builds,
    so nothing reads it any more. This is the same pattern P4 used for the rebuild temp directory. The deferred
    removal still covers the failure paths;
  - the plugin staging is marked applied.
- **`pkg/hub/maintenance/plugin_lockstep.go`.**
  - `markApplied` removes the now-empty `new/` and writes an `applied` marker. **`old/` is kept**: an in-process
    rollback (hook or Restart failure → `undoInstall` → `swap.restore`) needs it until the restart. After the
    restart nothing uses it, because a boot-failure rollback restores plugins from `releases/<tag>/plugins`.
  - `restore` removes the marker first. A directory kept by a failed restore holds the only copies of old builds,
    so it is never treated as finished. If that removal fails (not ENOENT) and a rename fails too, it is
    retried (review r1 O1). If the retry fails too (review r2), the directory, which holds the only old builds, is
    renamed off the staging prefix to `.scion-plugins-kept-<UTC ts>`, which no cleanup matches, and the error says
    where it is. If even that rename fails, the error tells the operator to move `old/` aside before the next start.
  - `removeStalePluginStaging` (next update) removes a marked directory with a plain note. Unmarked ones still get
    the "interrupted or failed" warning.
  - New `RemoveAppliedPluginStaging(pluginDir)` removes only marked directories. The marker must be a regular file.
- **`cmd/server_update_state.go` / `server_foreground.go`.** The hub calls `RemoveAppliedPluginStaging` at
  startup, next to `reconcilePluginsAfterRollback` (before plugins load). It is best-effort and logged.
- **`docs/deploy/single-node-vm.md`:** the success-path behaviour.

## Decisions
- `old/` is not removed before the restart: the brief allows keeping it until the restart and removing it at the
  next start, and removing it earlier would break the hook/Restart failure rollback. The marker is what makes
  the next start (and the next update) treat it as finished rather than interrupted.
- The failure paths are unchanged:
  - Activate failure → cleanup;
  - apply failure → undo + restore;
  - hook/Restart failure → Deactivate + restore;
  - failed restore → kept, unmarked.
- On upstream main, the plugin supervisor (`pkg/plugin/supervisor.go`, started in `cmd/server_foreground.go`) can
  respawn a hub-managed plugin from its plugin directory between the lockstep swap and the hub restart, so that
  plugin may run its new build against the running hub for that window. P5's lockstep design accepts this window;
  P5b's staging cleanup is not involved.

## Tests
`plugin_staging_fs_test.go` (untagged, review r1 O2): the next update's stale cleanup and `RemoveAppliedPluginStaging`,
which use no unix-only call. `pkg/hub/maintenance` is not in `.github/windows-test-packages.txt`, so they compile
for Windows but do not run on the Windows job until the package is listed (a CI change outside this task).

`plugin_staging_test.go` (unix, because the swap itself uses unix-only `copyExecutable`):
- the marker-removal failure cases:
  - still marked after a retry → moved to `.scion-plugins-kept-*`, ignored by both cleanups, old build intact;
  - the move fails too (read-only dir) → kept in place, and the error says to move `old/` aside;
  - retried successfully → unmarked and left by the startup cleanup;
  - restore succeeds → no error despite the marker;
- at the restart hook, no download temp is left; exactly one staging directory, marked, without `new/`, with the
  old build in `old/`;
- a hook failure after the mark still restores the old build and removes the staging;

`plugin_lockstep_test.go`: the restore-failure test now marks before the failure, then checks that the kept
staging is no longer marked and that the startup cleanup leaves it.

## Gates
To be run on named windows: C (pkg/hub/maintenance), U and R (the scanners read pkg/hub/maintenance), A+B
(cmd), and the hub shards per the guard-shard rule. See the dev log.
