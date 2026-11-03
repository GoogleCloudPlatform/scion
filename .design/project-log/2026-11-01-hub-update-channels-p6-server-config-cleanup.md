# Project Log - 2026-11-01 - Hub update channels, Phase 6: server-config cleanup, docs sweep, rebuild branch validation

Tracking: ptone/scion#6009; branch validation: ptone/scion#6987. Design: hub-update-channels design §3 D8 ("Server-config
page duplicate (option 9)") and §5 Phase 6 (approved).
Branch `scion/hu-p6` from upstream main `664b05c35`. Item 3 is on `scion/hu-p6-on-p4`, which is `scion/hu-p6` with P4
(`scion/hu-p4-mf1`) merged in.

## Task
Leave one update path in the UI. Sweep the docs loose ends. Validate the admin `branch` param in both rebuild executors.

## Changes
- **Server-config page** (`web/src/components/pages/admin-server-config.ts`).
  - Removed:
    - the Check for Updates / Update Now flow, which guessed the deployment tier from `check-updates` and posted
      `update-binary` or `rebuild-server` itself;
    - its state, confirm dialog, dispatch code and styles.
  - Added a read-only "Scion version: X (channel)" line and a "Manage updates →" link to `/admin/maintenance`.
    - The version and the *effective* channel come from `GET /api/v1/admin/maintenance/update-status`.
    - Before that call answers, on an error, or on a body without `current.version`/`channel`, the line shows the
      server-config `scion_version` with no channel, so the page never guesses a channel.
    - An empty effective channel (a git build) is shown without "()".
  - Kept the Git Commit and Build Time fields. They are read-only build info, not update controls.
  - The `UpdateStatus` type is reused from the Maintenance card with `import type`.
  - No Shoelace or icon changes: the removed icons are still used elsewhere.
- **Docs.**
  - P3-N2 line in the boot-failure guard section of `docs/deploy/single-node-vm.md`, summarised in `hub-setup-gce.md`.
    When a release that predates the guard performs an update, it does not rewrite `update-state.json`. In that case:
    - the guard refuses on the target/from mismatch and leaves the Hub stopped, never swapping wrongly;
    - if no file exists yet, the missing-file row applies instead;
    - the next update by a guard-aware release, or the next `deploy.sh` switch, rewrites the file.
  - `hub-server.md`: updates start from the Maintenance page only; the rebuild branch rules.
- **Branch validation, #6987** (`pkg/hub/maintenance/branch_param.go`, `maintenance_executors.go`).
  - `RebuildServerExecutor` (via `runRebuildSteps`) and `RebuildWebExecutor` validate the effective branch before
    preflight or any step runs. The effective branch is the `branch` param, else the configured branch.
  - A name is refused when it:
    - starts with `-`;
    - fails `git check-ref-format --branch`;
    - is returned changed by check-ref-format (`@{-N}` expansion).
  - With git ≥ 2.44 (read from `git version`), `--end-of-options` goes before the branch in `git checkout` and
    `git reset --hard`. git 2.24 added the flag, but checkout and reset accept it only from 2.44 (before that they
    read it as a revision or path), so on Ubuntu 22.04/24.04 and Debian 12 it is not passed. The leading-`-` refusal
    and check-ref-format are the security boundary either way. (The first version used 2.24; the #6987 review
    found it, C1.)
  - The errors quote the name and never echo git's output; a git that cannot run is reported as "could not
    validate branch", not as an invalid branch (review O1, O2).
  - Both checks run through `gitOutput`. The host-git door counts in `pkg/util/git_host_door_test.go` are therefore
    unchanged:

    | Function | Count | Made up of |
    |---|---|---|
    | `runRebuildSteps` | 5 | 4 literals + 1 dynamic start |
    | `RebuildWebExecutor.Run` | 6 | 5 literals + 1 dynamic start |
    | `gitOutput` | 1 | |

    `branch_param.go` has no `"git"` literal and no process start. Only a comment in the door test changed.

## Findings and decisions
- **F§7.3 (channel default) and the `release_channel` schema description were already fixed at base `664b05c35`.**
  - Evidence:
    - `deploy.sh:106-107,1830`;
    - `docs/deploy/single-node-vm.md:703-705`;
    - `hub-setup-gce.md:191`;
    - `agent-runbook-single-node-vm.md:230`;
    - `settings-v1.schema.json:2695`.
  - Checked with a "defaults to/is stable" regex: 0 files. Negative control with the same regex for nightly: 2 files.
  - No change.
- **`/healthz` `version` is still the hardcoded `"0.1.0"`** (`pkg/hub/handlers_health.go:152`). It is not changed:
  - the design marks it optional;
  - `scionVersion` already carries the real version, and restart polling uses it;
  - perf/bench relies on the literal;
  - external probes may parse the field.

  Suggested follow-up: drop the field, or document it as the API schema version.
- **The #6134 release-notes URL safety cases** moved from the server-config tests to
  `web/src/utils/release-notes-link.test.ts`, so coverage is kept after the banner removal.

## Tests
- Web: "Read-only version line and Manage updates link" replaces the 11 "Tier-based maintenance dispatch" tests. It covers:
  - the version and channel;
  - the link;
  - the fallback on 403;
  - a 200 with no channel, and a 200 with an empty channel;
  - no controls and no update requests.

  New `release-notes-link.test.ts` (6 cases).
- Go:
  - `branch_param_test.go` runs the real git: valid, option-shaped and invalid names, `@{-1}`, version parsing, args.
  - `rebuild_branch_test.go` (linux, stubs) covers both executors:
    - option-shaped names run nothing;
    - a configured `-c` is refused;
    - an invalid name stops after check-ref-format;
    - `--end-of-options` is omitted on git 2.43 and passed on git 2.44 (both executors), with a boundary table for
      endOfOptionsSupported;
    - a git that cannot run gives "could not validate branch", and no error carries a raw control character.

## Gates
- Web lane at `2b7732c5b`: prettier, vitest 306/306, typecheck, lint (0 errors), build: all pass.
- Web lane at `94ea9db19`: prettier, vitest 290/290 on the changed file, typecheck, lint: all pass.
- Go gates for #6987: see the dev log (`hub-update-channels/log/hu-p6-dev*.md`).
