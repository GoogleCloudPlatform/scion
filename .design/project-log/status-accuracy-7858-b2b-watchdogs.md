# harness_error watchdogs: OTel API error and first turn (ptone/scion#7937 B2b, part of ptone/scion#7858)

**Date:** 2026-11-14
**Branch:** scion/status-accuracy-7858-b2b (merged into scion/status-accuracy-7858 by the EM)

## Problem

B1 sets `harness_error` from a turn-end hook that carries an error. Some harnesses never send one: Claude
Code, Gemini CLI and Codex can show a model API error in the TUI and end the turn without a failing completion
hook. Others never start the first turn (login prompt, auth screen). Both still showed `working`, then STALLED.

## What B2b ships

- **Hook marks** (`handlers.TurnMarkHandler`, registered in `sciontool hook`). Every turn event writes its time
  to `/tmp/scion-turn-events`: a turn-start event (prompt-submit, agent-start, model-start, tool-start), or an
  error-free model-end or agent-end. An errored
  model-end or tool-end writes `/tmp/scion-last-harness-error.json` atomically, with B1's reason format,
  redaction and 300-rune cap. The pending error does not set harness_error. Both paths are injectable.
- **`APIOutcome` / `Pipeline.SetAPIOutcomeObserver`**: the native usage deriver passes each recorded
  (non-duplicate) model API outcome to the observer: OK or not, HTTP status, error text (the provider message
  when the text is a JSON error body) and model. Pinned against the claude, gemini-cli and codex fixtures.
- **API-error watchdog** (`init_harness_watchdog.go`, 10 s tick). It reports `model API <status>: <message>`
  when all of these hold:
  - the primary model's latest outcome in this run is a 400/401/403/404 failure;
  - nothing happened after it (no success, no turn event, no agent-info.json write) for the grace period;
  - the activity is thinking or working;
  - there is no live work.
- **Primary model** (`init_api_outcome_tracker.go`). The tracker keeps outcomes per model and picks the primary
  model in this order:
  - SCION_MODEL, when an observed label matches it (alias, provider-prefix and @version tolerant);
  - else the model with the most successes in this run;
  - else every model, so a primary failure before any success is still reported.
- **Grace period.** 30 s once a turn end was seen after the failure (a status write and activity `working`).
  Otherwise 120 s.
- **First-turn watchdog.** It runs when `SCION_INITIAL_TASK=1` and the harness has a hook dialect: built-in
  claude, gemini-cli or codex, or a staged `~/.scion/harness/dialect.yaml` (hermes has neither). If no
  turn-start event arrives within 3 min of readiness, it reports
  `harness did not start the initial task: <pending error | pane tail>`. The pane tail is the last 5 non-empty
  lines of `tmux capture-pane -p -J -t scion:agent -S -40`. It runs as the agent user, through
  `procreap.OutputManaged` because init is PID 1 with the reaper running.
- **Hook path wins.** Both watchdogs write with `StatusHandler.SetHarnessErrorIf`, a locked conditional
  update that never replaces an existing harness_error. They report to the Hub, with B1's
  `HarnessErrorStatusUpdate`, only when that write happened.
- The watchdogs run in the readiness watcher's goroutine after readiness, so init's shutdown path stops them
  with the watcher.

## Findings and deviations

- **Task signal (design §4.2(3)).** init never sees the task: it reaches the harness only in the tmux command,
  and `~/.scion/task.md` survives a later start that has no task. EM-approved option A: `pkg/agent` Start sets
  `SCION_INITIAL_TASK=1` when the effective task is non-empty and removes any inherited value otherwise. The
  hub never persists `SCION_*` keys into AppliedConfig.Env (`shouldPersistResolvedEnvKey`). Start always
  rewrites the variable, so a resume or reincarnation cannot carry a stale value.
- **Live work.** ptone/scion#7855 (`pkg/sciontool/liveness`) is not merged. `LiveWork` is an injected
  predicate that defaults to "no live work", with `TODO(ptone/scion#7855)` for whichever PR lands second.
- **"No hook activity change"** is the later of the turn-event marker and the agent-info.json mtime. A hook
  write after the failure restarts the 30 s quiet period; it never cancels the report. gemini-cli 0.68.0's
  AfterAgent (no error field) and Claude's Stop rewrite `working` right after a failed turn, and the report
  must still come 30 s later. A real retry shows up as a newer API outcome, success or failure, not as a
  status write. Tested with the captured codex sequence (stuck `thinking`, no completion hook) and the
  captured gemini-cli sequence.
- **Telemetry off means the OTel path is off.** Outcomes come only from the native deriver, which runs when
  `SCION_USAGE_SOURCE=native`. claude, codex and gemini-cli provisioners set that only when telemetry is
  enabled.
- **"Started" for the first-turn watchdog.** B2a's real-CLI captures show every harness submitting the
  prompt (UserPromptSubmit, BeforeAgent, PreInvocation, userPromptSubmitted) before the model call fails. So
  "no turn-start event" alone only catches failures before the prompt (login, auth screen). The watchdog
  also fires when the turn so far ended on a model error, meaning a pending **model-end** error newer than the
  last turn-start event. That covers muse-code 1.4.20 (PostLLMCall error, no Stop). A pending tool-end error
  does not count this way, because a failed tool is normal work.
- **Coverage this leaves.** A codex 0.169.0 failed turn sends no hook after UserPromptSubmit and carries no
  error. Only the OTel watchdog sees it, and only with telemetry on; without telemetry the agent goes STALLED
  as today. copilot has no error carrier, so after the prompt it also goes STALLED as today. The first-turn
  rule applies to the first turn only, so mid-session muse-code errors stay STALLED.
- **Pending reason source.** It is "model API" for a model-end error, so muse-code's reason reads
  `harness did not start the initial task: model API: API error 400: …`.
- **Review R1 (EM-approved).** The first version used "the latest outcome, any model" with a flat 30 s grace.
  Two kinds of healthy turn produced a sticky harness_error:
  - a side or sub-agent model's 404 while the main model streamed for more than 30 s;
  - a retried 400 whose retry streamed for more than 30 s.

  Now outcomes are correlated with the primary model, and the 120 s grace applies until a turn end is seen.
  **Deviation:** the thinking-without-turn-end path, codex 0.169's silent failure, is detected at about 120 s,
  not within the 60 s of criterion 10. It is still under the 5-minute stall threshold. The primary-model rule
  deviates slightly from the EM's "latest success, else SCION_MODEL". SCION_MODEL comes first when it matches
  an observed label, then the model with the most successes. This keeps a side call that succeeds after the
  main model failed from hiding that failure.
- **Review R2 (EM-approved).** OpenCode maps no agent-start, model-start or initial prompt-submit, and pi's
  prompt-submit is human input only. So an error-free model-end or agent-end now counts as a turn event.
  Residual: an OpenCode or pi first LLM step that runs past 3 min before any tool call or step-finish is still
  reported. This is pinned in `TestFirstTurnWatchdog_OpenCode`.
- **Final review R1: the first-turn watchdog owns its report.** pi has no model events, and its error-free
  agent-end does not clear harness_error, because agent-end uses clearNotificationWaiting. So a healthy text-only
  first turn longer than 3 min would have stayed in harness_error. After reporting, the watchdog keeps watching
  on a 10 s tick until init shuts down. At the first turn event after the report, it restores `working` and
  reports that to the Hub. The restore is guarded by `StatusHandler.RestoreFromHarnessErrorIf`: only
  harness_error that still carries the watchdog's own reason is replaced, so a hook-path harness_error is never
  cleared. agent-end still does not clear harness_error in general, because Claude's Stop and gemini-cli's
  AfterAgent fire without an error right after a failed turn. The long-first-step residual on OpenCode and pi
  now clears itself.
- **Copilot retried error (final hooks review).** copilot reports failed model calls (errorOccurred) but not
  successful ones. A 503 retried to success early in the first turn leaves a pending model-end error newer than
  the prompt. With no tool call before the 3-minute check, the watchdog reports, and the retraction clears it at
  the next tool-start or agentStop. The fatal 400 (no agentStop) still reports. Both are pinned with the
  captured fixtures.
- **Tail redaction window sized in runes.** `sanitizeTailEnd` used a 768-byte window. With 3-byte box-drawing
  runes, that window held fewer runes than the 300-rune budget, and a cut between a key ("Bearer", "API_KEY:")
  and its value kept the value unredacted. The window is now 428 runes (budget plus context), shrunk to stay under
  SanitizeText's byte cap. It is split at a word after 64 runes of context into context and kept parts, joined by
  a newline and sanitized together, and only the part after the newline is kept. If a redaction swallows the
  newline (Bearer's replacement takes the following whitespace), the split moves one word further.
  `TestFormatHarnessTailReason_StraddlingCredentialIsRedacted` failed before the change.
- **Pane-tail reason** uses B3's `FormatHarnessTailReason`, which keeps the newest end when cutting. The
  pending-error detail keeps `FormatHarnessErrorReason`.
- **Pane tail.** A full-screen TUI's last lines are often its own status bar (the Claude prompt and footer).
  The tail is evidence, not a diagnosis, and the pending error is used first when there is one.
