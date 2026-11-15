# Hold-until-session re-drive for Conduit `deliver-message` (Phase A2)

**Date:** 2026-11-15
**Stacked on:** `scion/exec-schedules-a` (`ptone/scion#7916`) at `4a6144da789001f2cb179af2311613c37f01d42c`
**Tracked in:** ptone/scion#7881 (parent goal ptone/scion#7818)

## What changed

- **Additive schema and store state machine (`pkg/ent/schema/`, `pkg/store/`).**
  - Added dispatch state `awaiting_session`, nullable `messages` columns
    `hold_until`, `hold_launch_id`, `not_reached` and `not_reached_at` (all
    `json:"-"`, omitted from message API/SSE serialization), partial indexes on
    `dispatch_state = 'awaiting_session'`, and the per-agent
    `agent_delivery_holds` marker table (`agent_id`, `hold_until`, `launch_id`,
    `held_count`, `created`). No `DowngradeFloor` bump is needed because the
    migration is strictly additive.
  - `UnbindMessageDeliveryWithProof` stamps `not_reached = 'no_session'` and
    `not_reached_at = storeNow` only when the hub `/deliver` handler saw
    `router.ErrNoSession`; `BindMessageDelivery` clears both columns when
    binding to `delivering`, and consuming a proof into `awaiting_session` also
    clears both columns. `awaiting_session` is excluded from
    `bindableDispatchStates` (`{dispatched, pending}`),
    `FailUndeliveredMessage`, reconcile, and the 24 h stuck-pending sweep.
  - All transitions entering or leaving `awaiting_session` serialize on the
    target agent row (`FOR UPDATE` on PostgreSQL; single-writer transaction on
    SQLite) and synchronize `agent_delivery_holds` in the same transaction,
    enforcing `maxHeldMessagesPerAgent = 100` and setting `hold_until` once via
    `COALESCE(hold_until, storeNow + 10m)` so re-holds never extend the
    deadline.
- **Proved `no_session` classification and broker report (`pkg/agentcontrol/`,
  `pkg/agent/`, `pkg/runtimebroker/`, `pkg/hub/`).**
  - Hub `/deliver` sets `reason: "no_session"` on `router.ErrNoSession` and
    `reason: "not_advertised"` on `ErrAgentControlNotAdvertised`, and strips any
    agent-supplied `reason` from agent responses.
  - On runtimes without exec (Cloud Run without an IAP exec connector),
    `AgentManager` returns `ErrAwaitingSession` iff the final bounded Conduit
    attempt answered HTTP 503 `outcome="not_reached", reason="no_session"` (and
    for split oversize flushes, only on the sub-flush that actually failed,
    never on unsent trailing sub-flushes).
  - The runtime broker sets `code: "awaiting_session"` on `MessageFailure`
    reports (reporting synchronously before answering 200 on an interrupt that
    carries a hub `message_id`). The hub validates `code` as an enum (`""` or
    `"awaiting_session"`), transitions eligible `dispatched` rows with a fresh
    `no_session` proof (`<= 2m`) to `awaiting_session`, and suppresses the
    sender failure notice while held.
- **Pre-dispatch ordering across all writers (`pkg/hub/message_hold.go`).**
  - Added `holdOrDispatch`, which atomically records `hold_launch_id` from
    `agents.run_id` and diverts new messages to `awaiting_session` when an
    `agent_delivery_holds` marker exists (under the 100-row cap), keeping
    creation order across buffered messages and interrupts (re-driven as
    buffered).
  - Wired all 13 persisted-message dispatch call sites through `holdOrDispatch`,
    including broker-inbound handlers and scheduler `messageEventHandler`
    (which now persists a `store.Message` row with `message_id` before dispatch;
    `ptone` Q5=a, 2026-11-15), enforced by `TestHoldOrDispatchCallSites`.
- **Re-drive builder, 64-row batched worker, session admission hook, and expiry
  (`pkg/hub/stored_message_dispatch.go`, `pkg/hub/message_hold_redrive.go`,
  `pkg/conduit/relay/relay.go`).**
  - Added pure `storedMessageDispatch` per the `ptone/scion#7949` contract so
    re-driven rows carry `withDispatchMessageID(ctx, row.ID)` and a rebuilt
    `StructuredMessage` envelope (failing unrecoverable rows with
    `"message cannot be re-delivered"`).
  - Added `relay.Config.OnAgentSessionAdmitted`, wired alongside
    `hubRelaySessionRecheck` and a 30 s cluster-wide backstop sweep. Coalesced
    per-agent workers claim ordered batches of at most 64 rows via per-row
    conditional updates, re-validate target standing, `run_id == hold_launch_id`,
    sender existence, delegation grant, and `authorizeAgentMessage`/scheduled
    authority (re-holding fail-closed on lookup errors), dispatch back-to-back
    so the broker debounces each batch into one paste, and wait for the batch to
    leave `dispatched` before claiming the next batch.
  - Added event-driven expiry (`"agent stopped"`) across stop, suspend,
    auto-suspend, heartbeat terminal/missing-container transitions, delete,
    reincarnate, and membership-loss agent holds, alongside TTL expiry
    (`"agent session did not connect"`) and non-`deliver-message` session
    expiry (`"agent cannot receive messages over conduit"`).

## Known limitations

- Re-driven messages rebuild `StructuredMessage` from stored `messages` columns
  (`storedMessageDispatch`): `plain` and allowlisted envelope metadata
  (provenance keys, Choices IDs) are not persisted on `messages` rows today, so
  re-driven messages always carry the standard sender envelope and omit those
  metadata keys (documented in `.design/conduit/v2.22-deliver-message.md`, T1).
- Over `maxHeldMessagesPerAgent` (100), pre-dispatch sends proceed directly to
  normal broker dispatch without queuing behind held rows, and broker failure
  reports fall through to ordinary `failed` handling.
