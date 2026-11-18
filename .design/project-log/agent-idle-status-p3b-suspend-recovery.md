# Idle auto-suspend durability: commit retry and sweep recovery (ptone/scion#8204)

Phase 3b of ptone/scion#7826. Makes an idle auto-suspend durable across a failed record write, a
hub restart and a stop timeout, so no agent is left stopped without `suspend_reason` after an idle
suspend.

- **Commit point.** `ClaimIdleSuspend` still commits the intent (`run_intent=stopped` plus the claim
  lease). The record (`phase=suspended`, `suspend_reason=idle`) is written by exactly one of two
  guarded writers: the worker (`IfIdleSuspendCommit`, lease unexpired) or sweep recovery
  (`IfIdleSuspendRecover`, lease expired).
- **Worker (`pkg/hub/server.go`, `pkg/hub/idle_suspend_recovery.go`).**
  - The record write is retried with backoff (100ms to 2s, 30s budget) on a detached context. A
    guard rejection after an ambiguous attempt re-reads the row and counts `suspended(idle)` with the
    same claim stamp as recorded.
  - The claim is no longer cleared when the record write fails, the guard rejects, or the stop
    outcome is unknown. It is still cleared after the record and on a definite stop refusal (broker
    status, request not sent, run mismatch), where the intent is reverted as before.
  - A stop timeout, cancellation or transport error does not revert the intent.
- **Recovery.** `recoverIdleSuspendClaims` runs first in each idle sweep tick under the same gates
  (`lifecycle.idle_suspend` and `auto_suspend_idle`). It selects rows through
  `ListExpiredIdleSuspendClaims` (claim expired, `run_intent=stopped`, phase `stopping`, `stopped` or
  `error`) and writes `suspended(idle)` under `IfIdleSuspendRecover`, which re-checks the stamp, run,
  intent, phase, start claim, reincarnation and deletion on the locked row and clears the claim. A
  row still `running` is left alone. The post-record steps are shared with the worker
  (`completeAutoSuspendRecord`).
- **Delivery.** `IdleSuspendClaimActive` now means an idle suspend is in progress: lease unexpired,
  or the stamp still set with `run_intent=stopped` on a row not yet `suspended`. Every user or hub
  lifecycle write clears the stamp, so this never holds a user-stopped row. Delivery and auto-wake
  paths therefore never dispatch into a container whose stop may have landed; they wait, then
  auto-wake once recovery records the suspension, or refuse with the "suspending" error.
  `IdleSuspendClaimLeaseActive` keeps the strict lease meaning for the intent-stop backstop,
  membership loss and the stalled auto-suspend.
- **Docs.** The "not yet safe for production" notes were removed from the experiment registry and
  `reference/experiments.md`.
- **Tests.** `pkg/store/entadapter/agent_idle_suspend_recover_test.go` (store guards) and
  `pkg/hub/idle_suspend_recovery_test.go` (fault injection on the record write, crash between stop
  and commit, stop timeout versus refusal, user stop in the window, gates off, and a DM during an
  unsettled suspend).
- **Residual.** A row that stays `running` after its claim expired (the stop never landed) is not
  converted; the claim stays inert and the existing run-intent reconciliation applies.
