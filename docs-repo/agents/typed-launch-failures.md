# Typed runtime launch failures

The Runtime Broker intentionally masks plain `Manager.Start` errors as `api.LaunchCodeRuntimeError` (`runtime_error`) with generic fixed text (`runtimeOpError`), so broker filesystem paths, container IDs, image references, pod names, and namespaces never leak to clients. Any launch failure that users need to see and act on must therefore be wired end-to-end as a typed failure code with fixed text and matching CLI and web labels.

## Required changes

1. **Wire code (`pkg/api/launch_codes.go`):** add a `LaunchCode*` constant (snake_case wire string). These values are stored and sent to clients, so never rename or change an existing value. If the failure occurs in a new launch phase, add the matching `LaunchStep*` constant in the same file. Pin any new constant in `TestLaunchVocabularyValues` (`pkg/api/launch_codes_test.go`).
2. **Runtime error (`pkg/runtime/interface.go`):** in `pkg/runtime/`, wrap the underlying error with `newLaunchFailure(code, step, reason, err)`, producing `&runtime.LaunchFailure{Code, Step, Reason, Err}` (callers outside `pkg/runtime/` can construct `&runtime.LaunchFailure{...}` directly):
   - `Code`: the `api.LaunchCode*` constant.
   - `Step`: the `api.LaunchStep*` constant where the failure occurred (falls back to the last reported step or `api.LaunchStepLaunching` when empty).
   - `Reason`: optional ASCII alphanumeric runtime reason enum only (at most 64 characters, e.g. `"ImagePullBackOff"`), never free text, image refs, pod names, or host paths. `launchReasonEnum` drops any non-alphanumeric value.
   - `Err`: the full underlying error; logged on the broker only and never sent to clients.
3. **Broker classification and user-facing text (`pkg/runtimebroker/run_launch.go`):**
   - Add an entry for the new `api.LaunchCode*` in `launchFailureText`. `classifyStartError` unwraps `*runtime.LaunchFailure` via `errors.As` and looks up `launchFailureText[lf.Code]`; any code missing from `launchFailureText` silently falls back to `api.LaunchCodeRuntimeError` with generic text.
   - Keep the text lowercase with no trailing period, fixed, and free of raw error text or broker internals. When `Reason` is a valid enum, `classifyStartError` appends `" (" + reason + ")"` (after `": it was <preempted|evicted>"` for `pod_disrupted`), keeping the fixed text intact in the message so the CLI's `strings.Contains(a.Message, label)` check prints the label once.
   - Non-runtime sentinel or typed errors that do not depend on a runtime step (e.g. policy, config, or broker-state refusals) are matched in `classifyStartErrorCode` instead of `launchFailureText`.
4. **CLI and web label parity (`cmd/launch_labels.go`, `web/src/shared/agent-launch-labels.ts`):**
   - Add the code to `launchCodeLabels` in `cmd/launch_labels.go` (keyed by `api.LaunchCode*`) and to `LAUNCH_CODE_LABELS` in `web/src/shared/agent-launch-labels.ts` (keyed by the wire string).
   - Keep the wording identical across `launchFailureText`, `launchCodeLabels`, and `LAUNCH_CODE_LABELS` (lowercase, no final period; the existing `launch_timeout` broker text is pod-specific and is deliberately exempt).
   - If you added a `LaunchStep*`, add matching entries to `launchStepLabels` and `LAUNCH_STEP_LABELS`.

## Verification

- `pkg/runtimebroker/launch_progress_test.go` (`TestClassifyStartError_LaunchFailure`): add a case asserting the code, step, fixed text, and that raw error details do not leak.
- `cmd/launch_labels_test.go` (`TestLaunchFailedError_EveryCodeHasALabel`, `TestLaunchProgress_EveryStepHasALabel`): parses `pkg/api` constants and fails if any `LaunchCode*` or `LaunchStep*` lacks a CLI label.
- `web/src/shared/agent-launch-labels.test.ts`: parses `pkg/api/launch_codes.go` and `launchFailureText` in `pkg/runtimebroker/run_launch.go` and fails if any code is missing from `LAUNCH_CODE_LABELS` or if a typed runtime failure's web label drifts from `launchFailureText` (excluding `launch_timeout`).

```bash
go test -buildvcs=false ./pkg/runtimebroker -run TestClassifyStartError_LaunchFailure
go test -buildvcs=false ./cmd -run 'TestLaunchFailedError_EveryCodeHasALabel|TestLaunchProgress_EveryStepHasALabel'
npm --prefix web test -- src/shared/agent-launch-labels.test.ts
```
