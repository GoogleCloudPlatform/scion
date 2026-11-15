# Idle status phase 2b: suspend_reason and auto-wake on new work

**Date:** 2026-11-15
**Branch:** scion/agent-idle-status-p2b
**Issue:** ptone/scion#7945 (phase 2b of ptone/scion#7826)

## What changed

- **`suspend_reason` column** (nullable string on `agents`; `state.SuspendReason`:
  `user`, `idle`, `stalled`). A manual suspend records `user` and
  `auto_suspend_stalled` records `stalled`. `idle` is defined now, and the idle
  sweep (phase 3) will write it. The reason is written only by `UpdateAgentStatus`
  (`AgentStatusUpdate.SuspendReason`) together with the suspended phase. An empty
  value writes nothing, so a report that only repeats the suspended phase keeps the
  stored reason. `entc.ClearSuspendReasonHook`, registered next to
  `ClearLaunchResumePhaseHook` on every client, clears the reason on any write of a
  phase other than `suspended` or `starting`. A resume clears it, and the rollback
  of a failed resume (starting back to suspended) keeps it. The reason appears as
  `suspendReason` in the agent API and as `suspended (idle)` in `scion list`.
- **Experiment `lifecycle.idle_suspend`** (server layer, off by default). It gates
  auto-wake now and will gate the phase 3 sweep.
- **Auto-wake** (`pkg/hub/auto_wake.go`, `shouldAutoWake`): the experiment is on,
  the agent is suspended, and its reason is `idle` or `stalled`. The sender needs
  only the authorization to deliver: `wakeResumeDenial` is skipped. Holds still
  apply, and a held agent is never woken.
  - Synchronous wake, then deliver, through `wakeAgentForDM`, exactly as an
    explicit wake works: user DM (`handleAgentMessage`), web chat primary
    (`sendAgentRouted`, which also covers chat scheduled sends), agent DM
    (`ExecuteAgentDM`), and scheduled message fires (`messageEventHandler`).
  - Background wake for the paths with tight budgets or a shared loop: agent mention
    fan-out (`ExecuteAgentDM` with `SkipPhaseGate`, new outcome
    `AgentDMWakeQueued`), web chat secondaries, and notification delivery (the
    dispatcher's `autoWakeFn`). A per-agent run in `queueAutoWake` resumes the agent
    once and then runs every queued item. The goroutines are tracked by
    `autoWakeWork` (a `brokerWorkTracker`) and stopped at hub shutdown before the
    broker connections and the store close.
  - Failure: the DM paths return the wake error as today. A mention or secondary
    copy is marked failed with "auto-wake failed: ...". Notifications stay parked.
    The notification sweep calls `ListParkedNotificationSubscribers` and wakes
    idle- or stalled-suspended, unheld subscribers that have parked notifications
    again. After a wake, parked notifications are delivered by
    `drainUndispatchedNotifications` for the subscriber's broker.

## Decisions (engineering manager, 2026-11-14)

- **Sync vs async:** the design (§5) does not say which delivery paths wait for the
  resume. The DM-like paths, where the sender waits, are synchronous. Mentions,
  chat secondaries and notifications are asynchronous: the mention fan-out has a 10 s
  overall and 5 s per-mention budget, and the notification dispatcher is a single
  event loop. This keeps §5's failure rule (mentions buffered, notifications
  parked).
- **Hold suspension records no reason** (`membership_loss.go`): this deviates from
  "set on every suspend". The enum has no matching value, a hold blocks delivery
  anyway, and NULL means the agent never auto-wakes.

## Notes and follow-ups

- Rows suspended before this change have no reason and never auto-wake.
  `DowngradeFloor` is unchanged (additive nullable column).
- A failed resume that ends in `error` (not rolled back to suspended) clears the
  reason, so that agent no longer auto-wakes.
- Out of scope: routed inbound from chat plugins
  (`handlers_broker_inbound_routed.go`) and delegated sends still refuse a suspended
  target. They are candidate follow-ups.
- An unconfirmed failed start holds the agent's start claim until the claim's hold
  ends, so a sweep retry soon after such a failure is deferred. The next sweep
  after the hold ends wakes the agent.
