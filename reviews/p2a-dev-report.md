# Phase 2a dev report: migration gate + catch-up that works

Branch: `scion/agent-reincarnate-2` (from `upstream-main` @ `ec90cb335`)
Head SHA: `3dd53e5` (report commit; gate/F4 code at `c7b570e83cd81ed80b57905b5458bd9f5a7e6a34`)
Fork PR: https://github.com/ptone/scion/pull/2086 (references ptone/scion#2082)
Developer: ar-dev-2

## 1. F4 root cause

**Finding: F4 was already fixed at the source level, in the same commit (`ec90cb335`,
"Phase 1b") this branch was cut from. It was not fixed here.** What remained undone
was the deny-side test coverage the brief asked for, which this branch adds.

- The reported symptom — `scion conversation catch-up`/`list`/`messages` failing
  inside an agent container with `notifications require Hub mode. Enable with
  'scion hub enable <endpoint>'` — is raised by `requireHubClient` at
  `cmd/notifications.go:203-205`:
  ```go
  if !settings.IsHubEnabled() && !config.IsHubContext() {
      return nil, nil, fmt.Errorf("notifications require Hub mode. Enable with 'scion hub enable <endpoint>'")
  }
  ```
  `cmd/conversation.go`'s `list`/`messages`/`catch-up`/`get`/etc. all call this same
  shared helper (`cmd/conversation.go:267,332,381,457,507,573,601,638,665,691`).
  `config.IsHubContext()` (`pkg/config/project_marker.go:167-171`) is the in-container
  fallback: it returns true when `SCION_HUB_ENDPOINT`/`SCION_HUB_URL`/`SCION_PROJECT_ID`
  are set, which they always are for a broker-dispatched agent
  (`pkg/hub/httpdispatcher.go:2251-2264,2551-2560`). This fallback — and the whole
  `cmd/notifications.go`/`cmd/conversation.go` file pair — is **new in `ec90cb335`**
  (confirmed via `git log -S"config.IsHubContext()" -- cmd/notifications.go`, one hit:
  `ec90cb3`). `cmd/cli_mode.go`'s `agentAllowed` map already lists `conversation`,
  `conversation.list`, `conversation.messages`, `conversation.catch-up`, and all the
  `notifications.*` entries (`cmd/cli_mode.go:52-57,119-129`) — so the allow-list was
  never the blocker either.
- `cmd/conversation_test.go` already carries the regression tests for this exact bug,
  tracked as **ptone/scion#1909**: `TestRunConversationList_AgentHubContext_HubNotEnabledInSettings`
  and `TestRunConversationCatchUp_AgentHubContext_HubNotEnabledInSettings` (lines
  665-736), both landed in `ec90cb335`.
- Server-side, I looked for a matching authz gap (the brief's second candidate: "hub
  authz for agent principals reading conversations"). None exists: `handleGetConversation`
  and `handleConvListMessages` are `RouteAuthenticated` (accepts any identity, not
  user-only), and their handler-level checks (`authorizeDMRead`/`isCanonicalDMParticipant`
  for direct conversations, `authorizeGroupConversationAccess` for group) treat agent and
  user principals identically. A deny test for exactly the catch-up scenario — an agent
  reading a conversation it is not a participant of — already exists:
  `TestDMAccess_ThirdPrincipalDenied` (`pkg/hub/dm_access_test.go:80-164`) exercises both
  `handleGetConversation` and `handleConvListMessages` (the route `catch-up`'s
  `ListMessages` client call hits) with a non-participant agent identity and asserts 403.

**Live verification.** I reproduced F4 in my own agent container: the *installed*
`scion` binary (`scion dev (commit c1506776)`, baked into the harness image, predating
`ec90cb335`) fails with exactly the reported error on `scion conversation list`. A CLI
built from this branch's checkout (`go build ./...` at `upstream-main`, before any of my
changes) against the same live hub session and the same env **does not** fail —
`conversation list` returns normally, and (after I set up a real DM by messaging
agent-migrate-lead) `conversation list`/`messages`/`catch-up` all worked end to end. This
confirms the fix is real and already on `upstream-main`; the container I'm running in
just hasn't been rebuilt with it yet.

**What "fixing F4" therefore means in 2a scope:** nothing to change in production code.
I added one CLI-level test the existing suite was missing —
`TestRunConversationCatchUp_AgentHubContext_ForbiddenForNonParticipant`
(`cmd/conversation_test.go`) — which proves a 403 from the hub surfaces through
`runConversationCatchUp` as a clear error (not the old "requires Hub mode" text, not a
silently-swallowed empty result). The hub-side deny test already existed
(`TestDMAccess_ThirdPrincipalDenied`); this closes the loop at the CLI layer per the
lead's request.

**In-image CLI version needed:** any `scion` build at or after `ec90cb335`
("reincarnate: accept shared-workspace and hub-managed agents (Phase 1b)", the commit
that added `cmd/notifications.go`'s `IsHubContext()` fallback). The harness image
currently ships an older build (`c1506776`, pre-`ec90cb335`) and needs to be refreshed
before AC-8b's in-container catch-up step is live end to end — this is the same class of
image-lag issue as ptone/scion#1910 (missing `reincarnate` in-image), and per A11.4's
precedent for that issue, it's a harness-image release matter, out of scope for this PR.
agent-migrate-lead is tracking this dependency for UAT planning.

## 2. Gate design and state-ordering answer

**Design.** Reused the existing `reincarnationInFlight(agent)` predicate
(`pkg/hub/reincarnate_worker.go:49-56`) — already the single shared source of truth for
"is a migration in progress" (A11 item 2 established this pattern for suppressing crash
status; I extended its use rather than inventing a second list of states). It is `true`
for `pending`/`stopping`/`provisioning`/`starting` and `false` for `""` (none) and
`failed`. I deliberately used this instead of the older design-doc wording
(`agents.reincarnation_state != ""`), because that literal reading would keep the gate on
forever after a failed migration (`ReincarnationState` stays `"failed"`, never reset to
`""`) — inconsistent with the brief's own requirement that "the gate turns off ... after
failure (state cleared)." Using `reincarnationInFlight` makes "cleared" mean "no longer
non-terminal," which covers both completion and failure, and keeps one predicate for
every reincarnation-aware code path in the package.

**Where the gate sits in each of the three paths**, and what it does instead of the
prior behavior for a genuinely non-running agent:

| Path | File | Ordinary non-running behavior | Migration-gate behavior |
|---|---|---|---|
| `handleAgentMessage` (human sender) | `pkg/hub/handlers_agent_messaging.go` | 409 before persistence (phase switch, `:1551-1565`) | Wake attempt and phase-conflict check both skip (`&& !reincarnating` at `:1541,1551`); message persists (`DispatchState=deferred`); 202 short-circuit right before the managed/broker dispatch branches (`:2001-2016`) |
| `ExecuteAgentDM` (agent→agent DM) | `pkg/hub/agent_dm_operation.go` | 409-equivalent `AgentDMError` from `validateAgentDeliverable`/`wakeAgentForDM` (Phase 1b, `:333-343`) | Wake/validate skipped (`if !deferred` at `:333`); message persists with `DispatchState=deferred` (`:357-363`); dispatch (step 11) short-circuits, returning `AgentDMDeferred` (`:466-475`) |
| `MessageBrokerProxy.deliverToAgent` (pub/sub) | `pkg/hub/messagebroker.go` | #1820 gate drops the message with **no persistence** (`:747-759`, unchanged, this is the non-goal ptone/scion#1820 behavior) | Gate computed *before* the #1820 check (`:747-757`) so a migrating agent — necessarily non-`running` for most of the migration — gets persist-and-defer instead of #1820's drop; dispatch short-circuits after persistence (`:885-894`) |

All three set the persisted row's `DispatchState` to a new value,
`store.MessageDispatchDeferred` (`pkg/store/models.go`), distinct from `pending`
(dispatch not yet attempted), `dispatched` (attempted and accepted), and `failed`
(attempted and rejected) — "deferred" means dispatch was deliberately never attempted.
The response envelope is `202` with a `deferred` field in every path (`{"message_id":
..., "status": "deferred", "deferred": "agent is reincarnating", ...}`), via
`WriteAgentDMResult`'s new `AgentDMDeferred` case, `MessageDeliveryResponse.Deferred`,
and the inline JSON write in `handleAgentMessage`'s human-sender branch.

**Non-goal preserved.** The ordinary 409 (human sender, unrelated stopped agent) and the
#1820 drop (broker path, unrelated stopped agent) are unchanged for an agent that is
*not* reincarnating — verified by a boundary test
(`TestHandleAgentMessage_HumanSender_ReincarnationFailed_OrdinaryPhaseCheckApplies`) that
sets `ReincarnationState=failed` **and** `Phase=stopped` on the same agent and confirms
the 409 still fires: a terminal reincarnation state does not accidentally widen the gate
into masking a real stop.

**State-ordering answer (the brief's specific question): is there a gap between
clearing the state and the new container being ready to receive?**

Yes, and it is bounded, pre-existing, and explicitly out of scope (not something 2a
introduces or needs to close):

- Tracing `runReincarnationWorker` (`pkg/hub/reincarnate_worker.go:494-578`): the
  `starting` step's write (`reincarnationState: store.ReincarnationStateStarting`) lands
  *before* `dispatcher.DispatchAgentStart` is called (`:501-514`). `reincarnation_state`
  is **not** cleared until the completion CAS (`:522-548`) succeeds and the final
  `updateReincarnationStep` write (`:562-574`) lands — both happen *after*
  `DispatchAgentStart` has already returned successfully. So the gate is provably still
  ON for the entire span from "start dispatched" through "state cleared": no message can
  slip through in a state where the container might not exist yet.
- The gap is on the *other* side: once `reincarnation_state` clears to `""`, the gate
  turns off and the three delivery paths resume normal dispatch — but `DispatchAgentStart`
  (`pkg/hub/httpdispatcher.go:2150`) only confirms the broker *accepted* the start
  request, not that the harness inside the new container has finished booting and is
  actually listening. Phase is deliberately left at `starting` (not forced to `running`)
  at completion (`reincarnate_worker.go:550-557`) precisely because of this — the
  container's own status report is what moves it to `running`. So there is a short window,
  after the gate lifts, where a normally-dispatched message could still race the new
  container's readiness.
- This is **not a new problem 2a introduces**: it is the same race that exists for any
  ordinary agent start/wake today (a message dispatched to a freshly-started agent before
  its harness is listening) — the general silent-drop bug the design doc explicitly
  carves out as **ptone/scion#1820**, a non-goal for this phase (§2, confirmed again in
  Amendment A25's non-goals list: "a general delivery queue (#1820)"). The migration gate
  closes the *additional* window migration opens (dispatching into a container that is
  provably stopped/being torn down/being reprovisioned) without claiming to solve the
  pre-existing ordinary-start race, which is out of scope here.

## 3. File-by-file summary

- `pkg/store/models.go` — added `MessageDispatchDeferred = "deferred"` alongside the
  existing `Message.DispatchState` constants.
- `pkg/hub/messagebroker.go` — `deliverToAgent`: compute `reincarnationInFlight` before
  the #1820 gate; skip #1820 and set `DispatchState=deferred` when true; short-circuit
  before `dispatchWithBrokerRetry`.
- `pkg/hub/agent_dm_operation.go` — `ExecuteAgentDM`: new `AgentDMDeferred` outcome
  constant; compute `deferred` before Phase 1b (wake/validate); skip wake/validate when
  deferred; set `DispatchState=deferred` at message construction; short-circuit before
  dispatch (step 11), returning `AgentDMDeferred`; `WriteAgentDMResult` gained a
  `case AgentDMDeferred` writing `202 {"deferred": "agent is reincarnating", ...}`.
- `pkg/hub/dm_audit.go` — added `DispatchDeferred` to the `DispatchOutcome` audit enum
  (distinct from `DispatchSkipped`/`DispatchFailed`), used by `ExecuteAgentDM`'s
  `LogDMDispatchOutcome` call for the deferred path.
- `pkg/hub/handlers_agent_messaging.go` — `handleAgentMessage`: compute `reincarnating`
  next to the existing `senderIsAgent` check; gate the wake-attempt and phase-conflict
  blocks with `&& !reincarnating`; set the human-sender `storeMsg.DispatchState`
  conditionally; add the 202-deferred short-circuit right before the managed/broker
  dispatch branches; extend the agent-sender response switch with an `AgentDMDeferred`
  case; added `MessageDeliveryResponse.Deferred` field.
- `pkg/hub/reincarnate_worker.go` — `buildReincarnationPreamble` gained
  `migrationStart, migrationEnd time.Time` parameters; step 2's wording now names the
  window and reinstates "messages ... can be read with catch-up" (keeping the #1910
  image-lag fallback sentence). Call site re-reads the reincarnation record for
  `RequestedAt` (start) and reuses `provisioningNow` (end, a close proxy for when the
  gate will lift a few steps later); falls back to `provisioningNow` for both if the
  re-read fails (non-fatal to the migration).
- `pkg/hubclient/agents.go` — `MessageResponse` and `OutboundMessageResult` both gained
  a `Deferred string` field for the CLI to key on.
- `cmd/message.go` — `sendMessageViaHub` and both `sendMessageViaConversation` branches
  (agent-ref-in-agent-context, and the conv:/@email/#thread outbound path) print the
  deferred notice when `result.Status == "deferred"` instead of the generic
  delivered/dispatched line.
- `cmd/conversation_test.go` — added the F4 deny-test (§1).
- `pkg/hub/handlers_agent_reincarnate_test.go` — updated the preamble golden test
  (`TestBuildReincarnationPreamble_DoesNotPromiseRedelivery` →
  `TestBuildReincarnationPreamble_CatchUpWindow`) for the new signature/wording.
- `pkg/hub/reincarnation_gate_test.go` (new) — the gate test matrix (§4).
- `cmd/message_test.go`, `cmd/message_convref_test.go` — CLI deferred-print tests (§4).

## 4. Tests

All new tests use the real SQLite store (`testServer`/`newBrokerTestStore`/
`deliverySetup`), not mocks of the gate, per the brief.

`pkg/hub/reincarnation_gate_test.go`:
- `TestDeliverToAgent_Reincarnating_PersistsDeferred_NoDispatch` — table over
  `pending`/`stopping`/`provisioning`/`starting`: message persists
  (`DispatchState=deferred`), no dispatch call.
- `TestDeliverToAgent_ReincarnationFailed_DispatchesNormally` /
  `TestDeliverToAgent_ReincarnationNone_DispatchesNormally` — gate off after
  failure/completion.
- `TestExecuteAgentDM_ReincarnatingTarget_Defers` — deferred outcome, no dispatch call,
  row present with `Msg`/`DispatchState` correct (persisted-row-in-history assertion).
- `TestExecuteAgentDM_ReincarnatingTarget_WakeRequested_StillDefers` — proves `Wake:true`
  against a migrating, non-suspended target is skipped rather than hitting
  `wakeAgentForDM`'s default-case 400.
- `TestExecuteAgentDM_ReincarnationFailed_NotGated` — gate off after failure, normal
  dispatch.
- `TestHandleAgentMessage_HumanSender_ReincarnatingAgent_Returns202Deferred` — full HTTP
  round trip: 202, `MessageDeliveryResponse{Status:"deferred", Deferred:"agent is
  reincarnating"}`, no dispatch call, persisted row visible via `s.GetMessage`.
- `TestHandleAgentMessage_HumanSender_ReincarnatingAgent_WakeRequested_StillDefers` —
  same with `wake:true`.
- `TestHandleAgentMessage_HumanSender_ReincarnationFailed_OrdinaryPhaseCheckApplies` —
  boundary/regression test: `failed` + genuinely `stopped` still gets the ordinary 409.
- `TestHandleAgentMessage_AgentSender_ReincarnatingTarget_Returns202Deferred` — the
  `handleAgentMessage` → `ExecuteAgentDM` fork, over HTTP, with the same assertions.
- `TestBuildReincarnationPreamble_CatchUpWindow` (in
  `handlers_agent_reincarnate_test.go`) — pins the exact window wording, the reinstated
  catch-up claim, the retained #1910 fallback sentence, and that "redeliver" never
  appears.

`cmd/conversation_test.go`:
- `TestRunConversationCatchUp_AgentHubContext_ForbiddenForNonParticipant` — 403 from a
  mock hub surfaces as a clear CLI error, not "requires Hub mode" and not silence.

`cmd/message_test.go` / `cmd/message_convref_test.go`:
- `TestSendMessageViaHub_ReincarnatingAgent_PrintsDeferredNotice`,
  `TestSendMessageViaConversation_AgentRef_AgentContext_Deferred`,
  `TestSendMessageViaConversation_OutboundPath_Deferred` — all three CLI send paths
  print the deferred notice and treat it as success, not an error.

## 5. Gate results

- `GOCACHE=/tmp/gocache-ar-dev-2`; never touched `/scion-volumes/gocache`, never ran
  `go clean -cache/-modcache`.
- All Go commands run with
  `env -u SCION_AUTO_EXPOSE_PORTS -u SCION_PROJECT -u SCION_GROVE`. I additionally had to
  unset `SCION_HUB_ENDPOINT`/`SCION_HUB_URL`/`SCION_PROJECT_ID`/`SCION_CLI_MODE` for test
  runs: this container is itself a live hub-connected agent, so those real env vars leak
  into `go test` and make one unrelated pre-existing test
  (`TestHubAllOrOneActions`, `cmd/hub_all_or_one_test.go`) try to hit the real production
  hub and fail with 401 — confirmed unrelated to this change (fails identically on a
  totally clean tree with those vars set; passes with them unset). Noting this as an
  environment fact, not a gate I altered.
- `go build -buildvcs=false ./...` — clean.
- `make fmt` — no diff beyond what I wrote (confirmed via `git status`/`git diff --stat`
  before and after).
- `go test ./cmd/...` — **ok** (both `cmd` and `cmd/sciontool/commands`), ~32s.
- Full `pkg/hub` suite, matching the Makefile's `test-hub-sqlite` target exactly
  (`go test -count=1 -timeout 30m -skip '^(TestDEF164_AtAgentSlug_DeliversToAgent|TestDEF164_AtAgentSlug_DMConversationCreated|TestDEF152_AgentToAgentDM_DeliversViaOutbound|TestCreateTemplateV2_ScopeIDInjectionBlocked)$' ./pkg/hub/...`)
  — **ok**, all sub-packages, 800s for the main `pkg/hub` package, zero failures.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1` on
  `./pkg/hub/... ./pkg/store/... ./pkg/hubclient/... ./cmd/...` — **0 issues** (one
  `staticcheck QF1002` finding on first run, fixed by converting an `if`/`else if` chain
  to a tagged `switch` in `cmd/message.go`; re-ran clean).
- Live verification: reproduced F4 with the container's stock (pre-`ec90cb335`) CLI, and
  confirmed a branch-built CLI does not reproduce it against the same live hub session
  (§1).

## 6. Head SHA and fork PR

- Gate/F4 code head SHA: `c7b570e83cd81ed80b57905b5458bd9f5a7e6a34`; this report's own
  commit is `3dd53e5`.
- Fork PR: https://github.com/ptone/scion/pull/2086, opened against `ptone/scion` `main`,
  referencing `ptone/scion#2082`.
