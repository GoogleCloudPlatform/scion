# Reincarnate harness switch: user documentation

Phase: ptone/scion#7762, parent ptone/scion#7757.

Updated the lifecycle guide, CLI reference, harness-settings reference, experiment
catalog and release notes for `scion reincarnate --harness`. This docs-only phase
is based on upstream main and must land after P1–P4 (ptone/scion#7758,
ptone/scion#7759, ptone/scion#7760, ptone/scion#7761).

Sources checked: agreed scratchpad design §3.7 and the bodies/diffs of fork PRs
ptone/scion#7770 (P1), ptone/scion#7789 (P2), ptone/scion#7788 (P3), and
ptone/scion#7787 (P4). The pages cover the home-reset warning, losses and retained
data, dry-run preview, no rollback, configuration drops, credential refusals,
runtime support, Kubernetes image feature requirement, workspace residue and
failure recovery. No Go code or CLI help strings changed.

## Implemented behaviour that refines the design

The engineering manager confirmed these are deliberate P1 review dispositions
(N1/N2) and should be documented without reopening the design:

- Naming the current harness-config still resets the home.
- Post-reset reprovision failure calls for plain `scion reincarnate <agent>` to
  rebuild the new home; `scion start <agent>` is sufficient only when composition
  succeeded. Selecting the old harness-config again cannot recover lost files.
- Inline `config_dir`, `command_args`, `task_flag` and `user` are dropped without
  replacement CLI flags. Define them in the target harness-config. Image, model
  and auth can be pinned with flags in the same request.

No further code/doc mismatch or required help-string change found.

## Verification

- `npm ci --prefix docs-site`: exit 0; installed 455 packages.
- `PATH=/tmp/d2-v0.9.0/bin:$PATH npm run build --prefix docs-site`: exit 0;
  `All internal links are valid.`, `126 page(s) built in 8.48s`, `Complete!`.
  Node v24.22.2; D2 v0.9.0. Existing `tmux` and `caddy` syntax highlighting
  warnings fall back to `txt`; no broken links or build errors.
- From `docs-site`, `PATH=/tmp/d2-v0.9.0/bin:$PATH ./check-d2.sh`: exit 0;
  `Checked 10 d2 block(s), 0 failure(s).` (no diagrams changed).
- `make fmt-check`: exit 0; `Checking Go formatting...` / `Go formatting OK.`
- `git diff --check`: exit 0, no output.
- No docs lint script is defined by `docs-site/package.json`; the build performs
  link validation required by `docs-site/AGENTS.md`.
- `make ci` passed formatting and was interrupted during lint dependency
  downloads, before whole-Hub compilation or tests, because its unsharded
  whole-Hub test/vet recipes are prohibited by repository guidance.
  Full Go CI was not run for these prose-only changes. No executed gate failed.

Review: prose-only, direct maintainer review as assigned; no agent review loop.
