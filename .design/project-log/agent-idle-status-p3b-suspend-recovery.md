# Idle auto-suspend durability: commit retry and sweep recovery (ptone/scion#8204)

**Branch:** scion/agent-idle-status-p3b
**Issue:** ptone/scion#8204 (phase 3b of ptone/scion#7826)

Phase 3b. Makes an idle auto-suspend durable across a failed record write, a
hub restart and a stop timeout, so no agent is left stopped without `suspend_reason` after an idle
suspend.

- **Commit point.** `ClaimIdleSuspend` still commits the intent (`run_intent=stopped` plus the claim
  lease). The record (`phase=suspended`, `suspend_reason=idle`) is written by exactly one of two
  guarded writers: the worker (`IfIdleSuspendCommit`, lease unexpired) or sweep recovery
  (`IfIdleSuspendRecover`, lease expired).
- **Worker (`pkg/hub/server.go`, `pkg/hub/idle_suspend_recovery.go`).**
  - The record write is retried with backoff (100ms to 2s, 30s budget) on a detached context, and
    returns at once on `ErrNotFound`. A guard rejection after an ambiguous attempt re-reads the row
    and counts `suspended` for the same run and claim stamp as recorded, whatever the reason.
  - The claim is no longer cleared when the record write fails, the guard rejects, or the stop
    outcome is unknown. It is still cleared after the record and on a definite stop refusal.
  - The stop classification is inverted: every `DispatchAgentStop` error is an unknown outcome (claim
    kept, intent not reverted) except the allowlist in `stopDefinitelyRefused` (request not sent,
    broker not connected, dial failure, run mismatch, broker 400/401/403). An unknown outcome whose
    container still runs converges through the intent-stop backstop and then recovery.
- **Recovery.** `recoverIdleSuspendClaims` runs first in each idle sweep tick and needs only the
  `lifecycle.idle_suspend` experiment, not `auto_suspend_idle`. A delivery wait that finds an
  expired claim on a row already stopping, stopped or error also tries the recovery write for that
  one row. It selects rows through `ListExpiredIdleSuspendClaims` (claim expired,
  `run_intent=stopped`, phase `stopping`, `stopped` or `error`) and writes `suspended(idle)` under
  `IfIdleSuspendRecover`, which re-checks the stamp, run, intent, phase, start claim, reincarnation
  and deletion on the locked row and clears the claim. A row still `running` is left to the
  intent-stop backstop. The post-record steps are shared with the worker
  (`completeAutoSuspendRecord`).
- **Abandon.** A row whose stop never lands (running, intent stopped, claim expired) fails deliveries
  fast; the sweep abandons the suspend once the claim has been expired for three leases
  (`AbandonIdleSuspend`: same stamp and run, the listed `run_intent_at`, no start claim or deletion;
  reverts the intent to running and records a message, logs WARN and publishes the status), so the
  agent is deliverable again as after a failed stop on main. The stamp is kept as a backoff marker:
  the candidate query and `ClaimIdleSuspend` skip the row until `idleSuspendBackoff()` (two ceilings
  plus a lease after the stamp, about 26 minutes, or 15 after the abandon) has passed, so a
  persistent failure retries about every 30 minutes.
  On a definite stop refusal the worker reverts the intent before clearing the claim.
- **Own intent only.** The idle machinery acts only on the intent its claim wrote
  (`IdleSuspendIntentIsOwn`: `run_intent_at <= idle_suspend_claim_until`; the claim writes both in one
  write and system writers yield while the lease is active). A newer system stop intent (membership
  hold, stalled auto-suspend) is neither recovered, abandoned nor in progress. In-progress and
  stop-all cover only the phases the machinery can settle (running, stopping, stopped, error).
- **Delivery and stop-all.** `IdleSuspendClaimActive` means an idle suspend is in progress: lease
  unexpired, or, while the experiment is on, the stamp still set with `run_intent=stopped` on a row
  not yet `suspended`. With the experiment off it is lease-only (no claim checks beyond the lease).
  A user stop-all selects in-progress rows by the same definition, so it wins over recovery.
  `IdleSuspendClaimLeaseActive` keeps the strict lease meaning for the intent-stop backstop,
  membership loss and the stalled auto-suspend.
- **Docs.** The "not yet safe for production" notes were removed from the experiment registry and
  `reference/experiments.md`.
- **Tests.** `pkg/store/entadapter/agent_idle_suspend_recover_test.go` (store guards) and
  `pkg/hub/idle_suspend_recovery_test.go` (fault injection on the record write, crash between stop
  and commit, stop timeout versus refusal, user stop in the window, gates off, and a DM during an
  unsettled suspend).
- **Residual.** Recovery writes an empty retained activity: the pre-stop activity is not recorded
  anywhere once the container's stop report clears it.
