# Hold-until-session re-drive for Conduit `deliver-message` (Phase A2)

**Date:** 2026-11-15
**Based on:** `main` after Phase A (`GoogleCloudPlatform/scion#4875`)
**Tracked in:** ptone/scion#7881 (parent goal ptone/scion#7818)

## What changed

- **Additive schema and store state machine (`pkg/ent/schema/`, `pkg/store/`).**
  - Added dispatch state `awaiting_session`, nullable `messages` columns
    `hold_until`, `hold_launch_id`, `not_reached`, `not_reached_at` and
    immutable `scheduled_event_id` (all `json:"-"`, omitted from message API/SSE
    serialization), partial indexes on `dispatch_state = 'awaiting_session'`,
    and the per-agent `agent_delivery_holds` marker table (`agent_id`,
    `hold_until`, `launch_id`, `held_count`, `claim_owner`, `claim_until`,
    `created`). No `DowngradeFloor` bump is needed because the migration is
    strictly additive.
  - `UnbindMessageDeliveryWithProof` stamps `not_reached = 'no_session'` and
    `not_reached_at = storeNow` only when the hub `/deliver` handler saw
    `router.ErrNoSession`; `BindMessageDelivery` clears both columns when
    binding to `delivering`, and consuming a proof into `awaiting_session` also
    clears both columns. `awaiting_session` is excluded from
    `bindableDispatchStates` (`{dispatched, pending}`),
    `FailUndeliveredMessage`, reconcile, and the 24 h stuck-pending sweep.
  - All transitions entering or leaving `awaiting_session` serialize on the
    target agent row in sorted agent-ID order (`FOR UPDATE` on PostgreSQL;
    single-writer transaction on SQLite) and synchronize `agent_delivery_holds`
    in the same transaction, enforcing `maxHeldMessagesPerAgent = 100`, setting
    `hold_until` once via `COALESCE(hold_until, storeNow + 10m)` so re-holds
    never extend the deadline, and coordinating cross-replica re-drive passes
    via `AcquireAgentRedriveLease` / `RenewAgentRedriveLease` /
    `ReleaseAgentRedriveLease` so multi-batch drains never interleave across hub
    nodes and new arrivals continue queuing behind the leased drain.
- **Proved `no_session` classification and broker report (`pkg/agentcontrol/`,
  `pkg/agent/`, `pkg/runtimebroker/`, `pkg/hubclient/`, `pkg/hub/`).**
  - Hub `/deliver` sets `reason: "no_session"` on `router.ErrNoSession` and
    `reason: "not_advertised"` on `ErrAgentControlNotAdvertised`, and strips any
    agent-supplied `reason` from agent responses.
  - On runtimes without exec (Cloud Run without an IAP exec connector),
    `AgentManager` returns `ErrAwaitingSession` iff the final bounded Conduit
    attempt answered HTTP 503 `outcome="not_reached", reason="no_session"` (and
    for split oversize flushes, only on the sub-flush that actually failed,
    never on unsent trailing sub-flushes).
  - The runtime broker sets `code: "awaiting_session"` on `MessageFailure`
    reports (reporting synchronously on an interrupt that carries a hub
    `message_id`, and answering 200 iff the hub response confirms `held > 0`).
    The hub validates `code` as an enum (`""` or `"awaiting_session"`),
    transitions eligible `dispatched` rows with a fresh `no_session` proof
    (`<= 2m`) to `awaiting_session`, and suppresses the sender failure notice
    while held.
- **Pre-dispatch ordering across all writers (`pkg/hub/message_hold.go`).**
  - Added `holdOrDispatch`, which atomically records `hold_launch_id` from
    `agents.run_id` and diverts new messages to `awaiting_session` when an
    `agent_delivery_holds` marker exists (under the 100-row cap), keeping
    creation order across buffered messages and interrupts (re-driven as
    buffered).
  - Wired all 13 persisted-message dispatch call sites through `holdOrDispatch`
    when `hub.conduit_deliver` is enabled, including broker-inbound handlers and
    scheduler `messageEventHandler` (which persists a `store.Message` row with
    immutable `scheduled_event_id` and `message_id` before dispatch when the
    flag is on, while preserving exact Phase A non-persisting / post-dispatch
    behaviour when `hub.conduit_deliver` is off; `ptone` Q5=a, 2026-11-15),
    enforced by `TestHoldOrDispatchCallSites`. Client and broker message APIs
    reject `channel = "scheduler"` so scheduled provenance cannot be spoofed.
- **Re-drive builder, leased 64-row batched worker, session admission hook, and
  expiry (`pkg/hub/stored_message_dispatch.go`, `pkg/hub/message_hold_redrive.go`,
  `pkg/conduit/relay/relay.go`).**
  - Added pure `storedMessageDispatch` per the `ptone/scion#7949` contract so
    re-driven rows carry `withDispatchMessageID(ctx, row.ID)` and a rebuilt
    `StructuredMessage` envelope (failing unrecoverable rows with
    `"message cannot be re-delivered"`), plus `enrichHeldChoiceResponse` to
    reconstruct `[choice-response]` agent lines and choice metadata from
    `choice_decisions` for single, override, and set answers.
  - Added `relay.Config.OnAgentSessionAdmitted`, wired alongside
    `hubRelaySessionRecheck` and a 30 s cluster-wide backstop sweep. Coalesced
    per-agent workers acquire a 2 min DB-backed per-agent re-drive lease
    (renewed mid-batch and before/during/after settle waits so slow passes never
    lose exclusivity across replicas), claim
    ordered batches of at most 64 rows via per-row conditional updates,
    re-validate target standing, `run_id == hold_launch_id`, sender existence,
    delegation grant, and `authorizeAgentMessage` or `scheduled_event_id`
    authority (re-holding fail-closed on lookup errors or pass context
    deadlines, and falling back to terminal `failed` + sender notice if a
    re-hold is refused), dispatch back-to-back so the broker debounces each
    batch into one paste, and poll `GetMessagesByIDs` with bounded backoff for
    the batch to leave `dispatched` before claiming the next batch.
  - Added event-driven expiry (`"agent stopped"`) across stop, suspend,
    auto-suspend, heartbeat terminal/missing-container transitions, delete,
    reincarnate, and membership-loss agent holds, alongside ungated TTL expiry
    (`"agent session did not connect"`), stopped-agent sweep cleanup, and
    non-`deliver-message` session expiry
    (`"agent cannot receive messages over conduit"`).

## Known limitations

- Re-driven messages rebuild `StructuredMessage` from stored `messages` columns
  (`storedMessageDispatch`), plus `choice_decisions` enrichment for
  `choice-response` rows: `plain` and non-Choices envelope metadata (such as
  delegation provenance keys) are not persisted on `messages` rows today, so
  re-driven non-choice messages carry the standard sender envelope without those
  metadata keys (documented in `.design/conduit/v2.22-deliver-message.md`, T1).
- Over `maxHeldMessagesPerAgent` (100), pre-dispatch sends proceed directly to
  normal broker dispatch without queuing behind held rows, and broker failure
  reports fall through to ordinary `failed` handling.
- If a hub process dies abruptly (OOM or node loss) after `ClaimHeldMessages`
  moved rows `awaiting_session → dispatched` and before handing them to the
  runtime broker, those claimed rows stay `dispatched` (`delivery_id IS NULL`)
  without re-drive or sender notice, matching existing persist-then-dispatch
  writers; graceful shutdown cancels `abortCtx` and re-holds them (RV3-O4).
