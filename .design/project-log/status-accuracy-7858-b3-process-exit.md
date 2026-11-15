# Process-exit error reporting (ptone/scion#7938, part of ptone/scion#7858)

**Date:** 2026-11-14
**Branch:** scion/status-accuracy-7858-b3 (merged into scion/status-accuracy-7858)

## Problem

When the harness process died, the reason was often lost:

- A harness that exited within `sciontool init`'s 100 ms startup window took a branch that reported nothing.
  tmux exits 0 once the session ends, so init could even exit 0 and the agent showed as `stopped`.
- A required pre-start failure was written only to the local `agent-info.json`. Runtimes whose broker
  cannot read the container home never saw the reason.
- A crash said only `Agent crashed with exit code N`.
- `scion start --wait-ready` reported a `harness_error` agent as ready.

## What B3 ships

- `tmuxAgentWindowCmd` (all runtimes): `<harness>; scion_hrc=$?; (umask 077; tmux capture-pane -p -J -S -40
  -t "$TMUX_PANE" > /tmp/scion-harness-tail) 2>/dev/null; echo $scion_hrc > /tmp/scion-harness-exit-code`.
  The tail is written before the exit code. A failing or missing tmux never changes the recorded code.
- `classifyExit` gains a `harnessTail` argument. It appends the last 5 non-empty lines (joined with ` | `)
  using `handlers.FormatHarnessTailReason`. That function applies B1's escape stripping and
  `logging.SanitizeText` redaction, and caps the whole reason at 300 runes. Unlike a harness_error reason,
  it cuts the tail from its start (`…` followed by the newest output), so the last line, usually the error,
  survives (review C1).
- Immediate-exit branch (`classifyImmediateExit`): a non-zero harness code is a crash. So is a missing
  exit-code file when the child is the tmux wrapper (`childRecordsHarnessExitCode`: the exit-code path appears
  in the child's arguments or in `SCION_START_CMD`), and so is a non-zero child code with no harness code.
  Each is reported locally and to the Hub with the exit code (`reportInitFailureWithExitCode`). init returns
  the harness code.
- `preStartErr` goes through `reportInitFailure` (`pre-start hook failed: <err>`).
- init removes stale exit-code and tail files before it launches the harness.
- `--wait-ready` exits 1 with the reason on `harness_error`, and on a terminal phase.
- `scion list` PHASE shows `error (<exit reason label>)` when an exit reason is recorded.

## Findings and deviations

- **Missing file is only a failure for the wrapper.** Plain child commands (for example the Cloud Run
  `CommandArgs` path) never write the exit-code file. Treating a missing file as a failure for every child
  would turn a clean, quick exit into a crash.
- **Exit code to the Hub.** `reportInitFailure` sent no exit code. Criterion 16 asks for one, so the
  immediate path uses `reportInitFailureWithExitCode`, which `reportInitFailure` now wraps.
- **Message wording.** The pre-start message keeps the existing `pre-start hook failed: …`, not the design's
  `provision failed: …`. The hook can also be a project hook, which is not provisioning. A harness that never
  started reads `Agent failed to start (exit code N): the harness did not start: no harness exit code was
  recorded`.
- **`error (…)` only with a reason.** Init failures record no exit reason, and `error (unknown)` would add
  noise. `halted` keeps `(unknown)`.
- **Stale files.** /tmp survives a container restart. Without the removal, a restart could report the
  previous run's code or tail.
- **Test isolation.** `TestReadHarnessExitCode*` used to write the real `/tmp/scion-harness-exit-code`. The
  package TestMain now points both paths at its sandbox.

## Validation

- Unit and RunInit tests in `cmd/sciontool/commands/init_exit_report_test.go` run against an httptest Hub.
  They cover a harness failure in the 100 ms window (exit 127, tail), a missing exit-code file, stale files,
  a crash after startup with a tail, and a pre-start failure.
- `pkg/runtime/harness_wrapper_test.go` runs the generated wrapper through both quoting layers with a fake
  tmux. It checks the capture order, the 0600 mode, the target pane and that the status survives.
- The wrapper line was also run once by hand under real tmux 3.5a on a private `-L` socket: rc 7, and the
  tail held the harness's output.
