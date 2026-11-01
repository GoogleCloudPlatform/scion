# Project Log - 2026-11-01 - Hub update channels, Phase 4: Git/release toggle and rebuild through the Installer

Tracking: ptone/scion#6009. Design: hub-update-channels design §3 D1, D5, D8 and §5 Phase 4 (approved).
Branch `scion/hu-p4` from upstream main `664b05c35`.

## Task
Let a hub that has a maintenance repo switch between "Build from Git" and "Release binary" from the
Maintenance page, and back. Do it with no new sudoers lines on starter hubs.

## Changes
- **rebuild-server goes through the update `Installer`** (`pkg/hub/maintenance/maintenance_executors.go`).
  It is now a `RestartingExecutor` and runs these steps in order:
  1. `Installer.Preflight`, before any git fetch;
  2. git fetch and checkout/reset;
  3. `make web`;
  4. `go build -ldflags "-X pkg/version.Commit=<sha> -X pkg/version.BuildTime=<rfc3339>"` into a temp dir;
  5. `VerifyGitBuild`: the binary must report the commit and no release version;
  5a. the pre-update database snapshot (design D4; review r2): the same `DBSnapshotter` as update-binary, the
      path recorded in the pending update, and a failed snapshot installs nothing;
  6. `Stage`/`Activate` under the name `git-<sha12>`;
  7. the starter-hub unit drift check, for sudo-install only;
  8. the before-restart hook, then `Restart`.

  If the hook or the restart fails, the Deactivator rolls the install back.
  - On the symlink layout, the build goes into `releases/git-<sha12>/` and is swapped and retained like a
    release. Rebuilding the commit that is already active is refused before the build starts.
  - With sudo-install, the build goes through `<RepoPath>/scion.rebuild` and the existing
    `/etc/sudoers.d/scion-rebuild-server` rules. This is the same host behaviour as before.
- **Naming.** `maintenance.GitReleaseName` gives `git-<sha12>`. `RunningReleaseName` is the version, or
  `git-<sha12>` when there is none.
  - Boot reconciliation (`server.go`) and the update-state booting/verified markers (`cmd/server_update_state.go`,
    `update_downgrade.go`) now use the running release name.
  - As a result, a git rebuild is verified on boot.
  - Boot-failure guard coverage (review r1, hu-em's policy):
    - release→git is covered like release→release: the rebuild looks up the publish date of a preview/stable
      `from` for `previous_safe`;
    - git→git and git→release fail closed: `previous_safe` is `false` for a `git-*` `from`, because a commit's
      schema position against the floor cannot be proven, so a crash loop leaves the hub stopped for the
      manual restore;
    - this is documented as a residual in `docs/deploy/single-node-vm.md`.
- **Selection.** `PendingRestart`/`PendingUpdate` carry `mode` and `commit`.
  - A verified rebuild writes `system.release_selection` `{mode:"git", version:"git-<sha12>"}`, with no
    channel or pin. Nothing is written before the rebuild succeeds (D5).
  - `lastUpdate.mode` is `git` for a rebuild.
  - `failStaleRestartingRuns` also covers rebuild-server.
- **check-updates** accepts `?mode=git|release` and returns 400 on any other value. Without it,
  `deployment_tier` decides, as before. The git check compares the running commit (the checkout's HEAD only
  when the checkout lacks it, reported as `compared`) with `origin/<repo_branch>`.
- On the symlink layout, a rebuild removes a stale `system.unit_drift` record, because the starter-hub unit
  check no longer runs there.
- **UI** (`admin-maintenance-update-card.ts`, `admin-maintenance.ts`):
  - an `sl-radio-group` "Build from Git" | "Release binary", shown only when `mode.repoConfigured`;
  - a Git panel with the configured branch (read-only), "Check for upstream commits" and Rebuild. Rebuild
    opens the page's run dialog;
  - rebuild-server leaves Routine Operations when the card offers it;
  - the page follows a rebuild run through the restart by uptime, because a git build's `/healthz` does not
    report `git-<sha12>`.
- Docs: `reference/server-config.md` (rebuild, selection mode, check-updates mode) and
  `hosted/single-node/hub-server.md`.

## Findings
- sudo-install, with its exact-command `sudo -n -l` preflight, had already landed in Phase 1 (the design's
  phasing amendment). Phase 4 only adds a test for the case where sudo is absent.
- The unit drift check compares against `scripts/starter-hub/scion-hub.service.tmpl`. On deploy.sh
  (symlink) hubs it would always report drift, so it now runs for sudo-install only.
- deploy.sh already treats `current -> releases/git-<sha12>` as an adopted git build, so no script change
  was needed.
- A git rebuild has no `releases/git-<sha>/plugins/`. Phase 5 handles that case.
