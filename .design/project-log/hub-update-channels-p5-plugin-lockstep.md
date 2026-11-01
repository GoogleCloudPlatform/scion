# Project Log: hub update channels, Phase 5: plugin lockstep (D6)

Issue: ptone/scion#6009. Branch `scion/hu-p5`, based on upstream main `664b05c35`. Agent: hu-p5-dev.

## Task

When the hub updates itself (`update-binary`), the broker plugins in `~/.scion/plugins/broker` must move to the same release as the hub, verified by the same `SHA256SUMS`. They must stay in lockstep across rollbacks too. Design §3 D6 and §5 Phase 5.

## What changed

- `pkg/hub/maintenance/plugin_lockstep.go` (new)
  - `preparePlugins` runs in the download phase, after the hub binary is verified. For each regular, executable `scion-plugin-<name>` it:
    - applies the `CheckAssetURL` pin;
    - downloads with `fetchCapped` under the same `maxBytes` and `DownloadTimeout`;
    - checks `ExpectedChecksum` against the same `SHA256SUMS`;
    - extracts with `extractRootEntry`, using the caps of `ExtractScionBinary`.
  - A missing asset: warning, plugin kept. An unwritable or unreadable directory: warning, lockstep skipped. Symlinks and unexpected names: warning, left alone. Any URL, checksum or extraction error fails the whole update before the snapshot.
  - `pluginSwap` puts the new binaries in a hidden `<plugindir>/.scion-update-*/` on the same filesystem and hard-links the old ones there as backups. It then renames each new binary into place, which is atomic per file. A failure part-way puts back the plugins already replaced. Stale staging directories are removed by the next run.
  - The `PluginRetainer` optional interface is used for retention. Downgrade confirmations list the plugins the target does not publish (`pluginRisks`, also from `CheckTarget` for same-channel downgrades).
- `binary_update.go`
  - Adds `BinaryUpdateOptions.PluginDir` (`""` turns lockstep off).
  - Order of the run: hub verify → plugin prepare → snapshot → Stage → plugin retention → Activate → plugin swap → before-restart hook → Restart.
  - A failed plugin swap, hook or Restart runs `Deactivate`, and restores the plugins only if `Deactivate` succeeded. sudo-install cannot be undone, so its plugins stay new and still match the hub that will run.
  - The static plugin risk text now describes lockstep.
- `plugin_retain_unix.go` (new): `symlinkInstaller.ActiveRelease` and `RetainPlugins`. They copy the plugins installed with each release to `releases/<tag>/plugins/` via `plugins.partial` and a rename. Old current gets the pre-update binaries; the target gets the new or kept ones.
- `plugin_reconcile_unix.go` (new): `ReconcilePluginsAfterRollback`.
  - It runs at boot, from `cmd/server_foreground.go` right after `markUpdateStateBooting`, before the store and the plugin manager.
  - Conditions: `rollback.marker` {from, to} exists and `current` points at `releases/<to>`. Then for each `X` in `releases/<to>/plugins/`, it replaces `pluginDir/X` only if its SHA-256 equals `releases/<from>/plugins/X`, i.e. it is the failed release's build.
  - It is idempotent and never touches a user-rebuilt or missing plugin. Release dirs, their `plugins/` dirs and the compared files must not be symlinks.
- `rollback_marker.go` (new): `ReadRollbackMarker` moved here from `pkg/hub/update_downgrade.go`. pkg/hub keeps aliases with the old unexported names, so its tests are unchanged.
- `maintenance_executors.go`: `extractScionBinary` is now `extractRootEntry(..., "scion", ...)`. Messages are unchanged for `scion`.
- `pkg/hub/update_status.go` / `admin_maintenance.go`: `updatePluginDir()` passes `~/.scion/plugins/broker`.
- `docs/deploy/single-node-vm.md`: new section "Chat plugins and updates".

## Decisions

- **Copies, not symlinks, in the plugin directory.** `pkg/plugin` discovery does follow symlinks:
  - `resolvePluginPath` uses `os.Stat(candidate)` (pkg/plugin/discovery.go:116);
  - `scanPluginDir` uses `DirEntry.Info()`, which is Lstat, so a symlink's 0777 bits pass the exec check even when it dangles (discovery.go:154, 161).

  But a link through `current/plugins/` would dangle whenever something switches `current` without filling `plugins/`: deploy.sh `scion-layout.sh install`, P4 rebuilds into `releases/git-<sha12>/`, or a manual repoint. The plugin would then fail to start silently. hu-em agreed.
- **Guard rollback is fixed in Go, not in the root script.** The root script stays minimal (design D1 amendment 1). The restored release always has the reconcile code: `releases/<to>/plugins/` exists only if `<to>` was the hub that ran a P5 lockstep update.
- Plugins are not executed for verification (they are go-plugin servers with no version command). Lockstep relies on SHA256SUMS identity (design §8: plugin versions are not stamped).

## Rollback paths

- **Downgrade (P3):** an ordinary run, so lockstep installs the target's assets. Missing ones are warned about and kept, and listed in the 409 risk list.
- **Failed hook or Restart (`Deactivate`):** plugins are restored from the hard-link backups.
- **Boot-failure guard swap:** the boot reconcile described above.
- **deploy.sh re-run:** installs the selected plugins from the release it installs (unchanged).

## Overlap with Phase 4 (hu-p4-dev)

`installer_symlink.go` is unchanged. The new methods are in `plugin_retain_unix.go`. `PluginRetainer` is optional: a strategy without it skips retention.

## Tests

See the dev log `/scion-volumes/scratchpad/projects/hub-update-channels/log/hu-p5-dev.md` for the list and the gate evidence.
