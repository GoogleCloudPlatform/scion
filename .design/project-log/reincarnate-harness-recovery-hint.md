# Reincarnate --harness: a fail-forward recovery hint that works

**Date:** 2026-11-14
**Branch:** scion/reincarnate-harness-d1
**Issue:** ptone/scion#7757 (e2e defect labelled D2 in the e2e report)

## Problem

A home-resetting `scion reincarnate --harness` that fails after the reset
fails forward: the row stays on the new harness config with phase `error`
and `reincarnation_state` `failed`. The failure message carries a recovery
hint (`homeResetRecoveryHint`, `pkg/hub/reincarnate_worker.go`). After a
start-stage failure the hint named `scion start <agent>`, which the Hub
refuses with 409 "already exists": for an error-phase agent,
`handleExistingAgent` starts in place only with Resume plus ForceResume
(`resumeInPlaceDecision`). The hint also said "fix the cause", which is
not actionable when the cause is a bad `-i` image: the image is recorded
into CreateInputs and every later start or reincarnation replays it.

## Choice of command

- `scion start <agent>`: 409, the duplicate-name branch.
- `scion resume --force <agent>`: passes the gate, but starts the
  container without reprovisioning (a home the failed reprovision left
  half-built stays so), passes the harness resume flag on a home that holds
  no session, does not stop a container an ambiguous start may have left,
  and keeps the bad image.
- `scion reincarnate <agent>`: accepted from `reincarnation_state` failed;
  stops the agent, reprovisions the new config and starts it, at every
  failure stage. It replays the recorded image and model; `-i <image>` and
  `--model <model>` replace them (recorded as explicit edits).
- `scion reincarnate <agent> --harness <old>`: unchanged; the switch drops
  the new harness's image and model.

Every variant (reprovision stage, definitive start, ambiguous start) now
names the plain reincarnate with the `-i`/`--model` note, then the switch
back. A failed reprovision adds "and may be incomplete" to the home note.

## Tests

`TestReincarnateHarness_RecoveryHintCommandAccepted` (hub shard 2) fails a
switch with `-i codex-img:bad` at each stage, checks the hint, checks that
a create without resume (the old `scion start`) classifies as the
duplicate-name branch, then runs the hinted command (plain, and with
`-i codex-img:good`) and asserts 202 and a completed reincarnation on the
new config. `TestReincarnateHarness_FailureHandling` asserts no hint names
`scion start`.

The docs-site agent-lifecycle reincarnate section does not quote the hint.
