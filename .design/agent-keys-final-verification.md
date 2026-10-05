# Agent keys: final cutover verification (Keys 4.3)

Task: ptone/scion#2202. Master design: ptone/scion#2184. Contract: `.design/agent-keys-contract.md`.
Inventory: `.design/agent-keys-raw-inventory.md`. User migration guide:
`docs-site/src/content/docs/reference/raw-message-removal.md`.

This document records the evidence that the core agent keys work (tasks 0.1 to 4.3,
ptone/scion#2191 to #2202) is complete, and keeps the deferred work separate.

## Tested revision

| Item | Value |
| --- | --- |
| Branch | `scion/agent-keys-4-3` |
| Base | Upstream `main` at `0bc54fda` (Keys 4.2 merged upstream as GoogleCloudPlatform/scion#2434; includes the ptone/scion#2721 fix, GoogleCloudPlatform/scion#2349 at `f8af776c`) |
| Tested revision | `827ebeb6` (the four 4.3 commits rebased onto upstream `main`). The commit that records this revision changes only this file. Part A was first verified at `266dbcc6` on top of the fork 4.2 head `957c3a2`. |
| Environment | Hermetic only: unit tests, `httptest` servers, mock runtimes and a private disposable tmux server. No live Hub, deployed agent, `scion start` or `scion create` was used. |

After 4.2 merged upstream, this branch was rebased onto upstream `main` and the matrix, gates and
stale-reference sweep were re-run at the tested revision above. The sweep found no new message-raw
references in the upstream changes.

## 1. Post-removal matrix

Each row maps to existing tests. A row marked **new** was a gap found by this task and is covered
by a test added on this branch. Every listed package passed at the tested revision.

### 1.1 The dedicated keys path works

| Mode / layer | Tests |
| --- | --- |
| Hub, both route shapes, user and agent callers | `pkg/hub`: `TestExecuteAgentKeys_Success`, `TestExecuteAgentKeys_HumanCrossProjectWithAttach_Dispatched`, `TestAgentActionKeysRoute_BothShapesAgree` |
| Hub to broker over the real HTTP dispatcher | `pkg/hub`: `TestExecuteAgentKeys_RealHTTPDispatcherIntegration`, `TestHTTPAgentDispatcher_DispatchAgentKeys_*` |
| Runtime broker route, HTTP and control channel | `pkg/runtimebroker`: `TestSendKeys_HTTP_Success`, `TestSendKeys_ViaControlChannelDispatch`, `TestControlChannel_Keys_*` |
| Terminal primitive, exact argv, real tmux | `pkg/agent`: `TestSendKeys_ArgvExactness`, `TestRealTmuxSendKeys`, `TestRealTmuxLoadBufferDeliversLargePayload` |
| Local mode, linked and unlinked projects | `cmd`: `TestSendKeysLocalWithManager_HubLinkedProject_ExactDelivery`, `TestSendKeysLocalWithManager_UnlinkedProject_ExactDelivery`; `pkg/agent`: `TestSendKeysLocal_*` |
| Hub client, both routes, no replay | `pkg/hubclient`: `TestAgentService_SendKeys_TopLevelRoute`, `TestAgentService_SendKeys_ProjectScopedRoute`, `TestSendKeys_NoReplay_*` |
| Attach-parity authorization, agent relationship | `pkg/hub`: `TestExecuteAgentKeys_AgentCallerRequiresAttachRelationship`, `TestAgentActionKeysRoute_*` |

### 1.2 Old-wire raw is rejected on every ingress

| Ingress | Tests |
| --- | --- |
| Hub `POST /api/v1/agents/{id}/message` and project-scoped `/message` | `pkg/hub`: `TestMessageRoutes_RetiredRawRejectedWithoutSideEffects`, `TestMessageRoutes_RetiredRawRejectedForAgentCaller`, `TestMessageRoutes_RetiredRawResponseNamesNoTarget`, `TestMessageRoutes_PreAuthBodyCap`, `TestMessageRoutes_BrokenReadWithRawFailsClosed` |
| Project broadcast | `pkg/hub`: `TestProjectBroadcast_RetiredRawRejectedWithoutSideEffects` |
| Broker inbound and routed inbound (plugins) | `pkg/hub`: `TestBrokerInbound_RetiredRawRejectedBeforeSenderSynthesis`, `TestRetiredRawIngress_BodyCap` |
| Scheduled events and recurring schedules | `pkg/hub`: `TestCreateScheduledEvent_Raw*`, `TestSchedule_CreateRaw*`, `TestSchedule_UpdateRaw*` |
| Runtime broker `/message` | `pkg/runtimebroker`: `TestSendMessage_RetiredRawRejectedWithoutSideEffects` |
| Plugin gRPC wire (field 11 reserved) | `pkg/plugin/grpcbroker`: `TestStructuredMessageRawFieldReserved` |
| Shared probe (spellings, values, case, malformed) | `pkg/messages`: `TestHasRetiredRawField` |
| Positive controls (Plain, normal, interrupt unchanged) | `pkg/hub`: `TestMessageRoutes_PlainNormalInterruptUnaffected`, `TestSchedule_CreateNonRawPayloadStillWorks`, `TestCreateScheduledEvent_NonRawPayloadStillWorks`; `pkg/runtimebroker`: `TestSendMessage_PlainNormalInterruptUnaffected`, `TestSendMessage_HubShapedNearLimitDelivered`; `pkg/messages`: `TestFormatForDelivery_IgnoresRetiredRawMember` |

### 1.3 The CLI replacement is reachable; `message --raw` makes no wire call

| Check | Tests |
| --- | --- |
| `scion keys` is registered and survives agent-mode restrictions | `cmd`: `TestKeysCmd_IsRegistered`, the agent-mode keys survival test in `cmd/cli_mode_test.go` |
| `scion keys` uses the `/keys` route and never falls back to `/message` | `cmd`: `TestSendKeysViaHub_PostsToKeysRoute`, `TestSendKeysViaHub_OldHub_FailsClearly_NeverFallsBackToMessage` |
| `message --raw` fails with zero HTTP calls for every target form and mode | `cmd`: `TestMessageCmd_RawFlag_ZeroWireCalls`, with positive control `TestMessageCmd_WithoutRawFlag_ReachesCountingServer` |
| Removal guidance names `scion keys` | `cmd`: `TestRemovedFlag_Raw` |

### 1.4 No content leaks into history, audit or logs

| Surface | Tests |
| --- | --- |
| Keys create no message, conversation, event or observer | `pkg/hub`: `TestExecuteAgentKeys_NoMessagingSideEffectsOnAnyOutcome`, `TestExecuteAgentKeys_NoObserverFanOut`, `TestExecuteAgentKeys_NeverReachesDispatchAgentMessage` |
| Keys audit is content-free | `pkg/hub`: `TestExecuteAgentKeys_AuditIsContentFree`, `pkg/hub/keys_no_content_leak_test.go` |
| Broker keys route: logs, spans, errors, message log | `pkg/runtimebroker`: `TestSendKeys_HTTP_NoLeakOfDistinctiveSecret`, `TestSendKeys_HTTP_NeverWritesMessageLog`; `pkg/agent`: `TestSendKeys_DeliveryFailure_SecretInBackendErrorAndOutput_NotSurfaced` |
| Raw rejection on `/message` routes: content-free audit | `pkg/hub`: `TestMessageRoutes_RetiredRawAuditIsContentFree` |
| Raw rejection on broker inbound: content-free logs | `pkg/hub`: `TestBrokerInbound_RetiredRawLogsAreContentFree` |
| Raw rejection on scheduled payloads: content-free audit and logs | **new** `pkg/hub`: `TestScheduledPayload_RawTombstoneIsContentFree` |
| Raw rejection on broker `/message`: no content in default or request log | **new** `pkg/runtimebroker`: `TestSendMessage_RetiredRawRejectionLogsAreContentFree` (plus the existing message-log spies) |
| Rejected raw is never persisted | the `...WithoutSideEffects` tests above; scheduled tests assert no stored row |

### 1.5 Commands and results at the tested revision

| Command | Result |
| --- | --- |
| `go test ./pkg/messages/ ./pkg/agentkeys/ ./pkg/plugin/grpcbroker/ ./pkg/hubclient/ ./pkg/messaging/` | pass |
| `go test ./pkg/runtimebroker/` (whole package) | pass |
| `go test -run 'SendKeys\|Keys' ./pkg/agent/` and the real-tmux tests | pass (tmux present; real-tmux tests ran, not skipped) |
| `go test -run 'Keys\|Message\|RemovedFlag\|DeprecatedFlag\|Deprecation\|Mode' ./cmd/` | pass |
| `go test -run 'Keys\|TestMessageRoutes_\|TestProjectBroadcast_Retired\|TestBrokerInbound_Retired\|TestRetiredRawIngress\|TestSchedule_\|TestCreateScheduledEvent_\|TestScheduledPayload_\|TestValidateScheduledEventPayloadJSON\|DMObserver\|AgentDM' ./pkg/hub/` (focused subset, `GOMEMLIMIT=4GiB`) | pass |
| `gofmt -l pkg cmd`, `go vet ./pkg/hub/ ./pkg/runtimebroker/` | clean |
| `golangci-lint run --new-from-rev=upstream-main ./pkg/hub/... ./pkg/runtimebroker/...` | 0 issues |
| `go build -buildvcs=false ./...`, `make check-annotation-prefix` | clean |
| docs-site `astro build` with D2 generation skipped (no `d2` binary in the sandbox) | the new page builds and its links validate; the 15 reported broken links all point at D2-rendered pages (`/hosted/ha/runtime-broker/` and two others) and appear only because D2 was skipped |

## 2. Version alignment

| Component | State | Skew behaviour |
| --- | --- | --- |
| Plugin proto | `StructuredMessage` field 11 and name `raw` are `reserved`; generated `broker.pb.go` has no `Raw` field (`TestStructuredMessageRawFieldReserved`) | Messages flow Hub to plugin only over gRPC. An older Hub that sets field 11 decodes in a current plugin as an ordinary message: the plugin delivers chat text, never keystrokes. Plugin inbound is HTTP and rejects `message.raw`. |
| Hub | No raw DTO field, no bridge, all ingresses reject `raw` | A current Hub never sends raw to a broker: `MessageRequest` and `StructuredMessage` have no Raw field. |
| Runtime broker | `/message` rejects `structured_message.raw` and top-level `raw`; `/keys` is the only injection route | An older Hub that still sends raw to a current broker gets 422. A current Hub with an older broker that lacks `/keys` gets `422 keys_unsupported`, never a message downgrade (`TestHTTPRuntimeBrokerClient_ExecuteKeys_OldBrokerIsUnsupported`, `TestControlChannelBrokerClient_ExecuteKeys_OldBrokerUnsupported`). |
| CLI | `--raw` rejected in argument validation; `scion keys` posts only to `/keys` | An older CLI that sends `raw` to a current Hub gets 422. A current CLI against an older Hub without `/keys` reports `hub_unsupported` and does not fall back. An older CLI binary in **local** mode still has its own local raw path; that is client-side code outside this tree, and an upgrade removes it. |
| Extras (chat apps, broker-log, a2a bridge) | Every `extras/*/go.mod` uses `replace github.com/GoogleCloudPlatform/scion => ../../`; no copied message types with a Raw field | Built from the same tree, so no skew. |
| Image builds | `image-build/*` build `scion` and `sciontool` from the same checkout (`VERSION` and `GIT_COMMIT` are labels only). Go pins match `go.mod` (1.26.1). | No drift. |

No skew path silently reintroduces raw delivery: each mismatch ends in an explicit 422 or a
`hub_unsupported` / `keys_unsupported` rejection.

## 3. Stale-reference sweep

Searched `docs-site/`, `docs/`, `.design/`, `pkg/config/embeds/`, `cmd/` help text, `web/`,
`extras/`, `image-build/` and the operator process skills for raw message delivery,
`message --raw`, `MessageRaw`, the raw bridge, or guidance pointing at them.

Fixed on this branch:

- `.design/agent-keys-raw-inventory.md` listed a migration notice in `release-notes.md` that did
  not exist. Added the migration guide `reference/raw-message-removal.md`, linked it from
  `release-notes.md`, `reference/api.md`, `reference/cli.md` and
  `reference/messaging-authorization.md`, and updated the inventory.
- `.design/managed-agents.md` CLI mapping table still had a `scion message --raw` row; it now
  names `scion keys` and `422 keys_unsupported`.
- `.design/messaging-conversation-model.md` pointed at an "eventual `message --raw` deprecation
  warning"; added a post-removal note that the flag was removed outright.

Kept as intentional:

- `.design/agent-keys-contract.md` bridge-era sections: the historical record, overridden by §0.
- `.design/messaging-conversation-model-findings.md` (`--raw` is a no-op in Hub mode): a dated
  findings report.
- `.design/project-log/*`, `changelog/*.md` and `docs-site/.../release-notes/*.md`: dated
  history.
- `cmd/message.go` hidden `--raw` flag and `errRawFlagRemoved`; `cmd/keys.go` help text: the
  rejection adapter and migration guidance.
- Unrelated "raw" uses (secret `Encoding: "raw"`, `ref.Raw`, hook `Raw` maps, web log-viewer
  `msg.raw`, `String.raw`): not message raw.
- `pkg/config/embeds/`, `web/` and `extras/`: no message-raw references.

Outside this repository: the fork's operator process skill (`scion-process`, Known Operational
Patterns) still tells operators to dismiss harness prompts with `scion message <agent> --raw ...`.
That command now fails, so the guidance should move to `scion keys <agent> "0"` /
`scion keys <agent> "Enter"`. This was reported to the issue owner rather than edited here.

## 4. Per-task evidence audit

| Task | Acceptance criteria (summary) | Evidence | Status |
| --- | --- | --- | --- |
| 0.1 ptone/scion#2191 Freeze contract | One request/result/auth contract with no literal-vs-sequence ambiguity; human cross-project distinct from agent refusal; Plain keeps message semantics; retirement governed by reachability and inventory | `.design/agent-keys-contract.md`; fork PR ptone/scion#2215, upstream #2118 (`154edf3d`) | Delivered. Fork issue still open; close at cleanup. |
| 0.2 ptone/scion#2192 Reject unsafe raw forms | Plugin `message.raw` cannot claim a user sender; zero side effects for every rejection; no cross-project escape; raw+plain 400, unsupported 422; no raw content in logs | Upstream #2125 (`ec56fa39`). The 4.2 tombstone tests in §1.2 and §1.4 supersede its tests. | Delivered. Fork issue still open; close at cleanup. |
| 1.1 ptone/scion#2193 Broker keys route and primitive | Exact argv, no added Enter; project isolation; no interleaving; expiry honoured; no StructuredMessage; secrets absent | Upstream #2137 (`f48c2c69`); `pkg/agent/sendkeys*_test.go`, `pkg/runtimebroker/handlers_keys_test.go` | Closed, complete. |
| 1.2 ptone/scion#2194 Keys across transports | Deadline preserved on both transports; same route; at most one dispatch; no durable queue; old broker detected without a Raw fallback | Upstream #2139 (`14a2ccfd`); `pkg/hub/*_keys_test.go`, `pkg/runtimebroker/controlchannel_keys_test.go` | Closed, complete. |
| 2.1 ptone/scion#2195 Authorization gate | User/UAT/agent matrices; closed mode does not block, open mode does not grant; no self/parent bypass; both route shapes agree | Upstream #2140 (`a604ad4c`); `pkg/hub/authorize_agentkeys*_test.go` | Closed, complete. |
| 2.2 ptone/scion#2196 ExecuteAgentKeys | Operation ID and canonical agent ID; explicit failures, no wake or queue; budget isolation; content-free audit; zero messaging writes | Upstream #2224 (`ad9dda98`); `pkg/hub/execute_agent_keys*_test.go`. Also delivered the agent relationship check (ptone/scion#2460). | Closed, complete. |
| 2.3 ptone/scion#2197 Bridge legacy raw | Bridge parity with `/keys` | Upstream #2231 (`e761178b`) | Closed. The bridge was later removed by 4.2, as planned. |
| 3.1 ptone/scion#2198 Hubclient and CLI | Local same-name isolation; user/agent modes; no replay under retries; one JSON result | Upstream #2233 (`0259486c`); `cmd/keys_test.go`, `pkg/hubclient/keys_test.go` | Closed, complete. The temporary `--raw` alias criterion was superseded by 4.2. |
| 3.2 ptone/scion#2199 Docs and caller inventory | Advertised syntax works; caller/image inventory; raw not advertised; accurate operational notes | Upstream #2234 (`117ebf42`); docs in `docs-site/`; inventory rewritten post-removal in 4.2 | Closed, complete. |
| 4.1 ptone/scion#2200 Integrated UAT | Focused tests and CI at a recorded revision; no replay; no side effects or leaks; caller dispositions; removal gate recorded | UAT accepted at `d95a1dab`; tests upstream #2286 (`ebd5d8f5`), #2325 (`2365ba09`); docs #2243 (`d95a1dab`) | Closed, accepted. Defects it found are listed in §5. |
| 4.2 ptone/scion#2201 Remove raw | Zero-side-effect retirement tests for every ingress; no injection via message API or `--raw`; both spellings and all values rejected; no production Raw field; keys still works; Plain unchanged | Fork PR ptone/scion#2905, head `957c3a2`, review approved; merged upstream as GoogleCloudPlatform/scion#2434 (`0bc54fda`); §1 of this document | Complete (merged upstream). Fork issue still open; close at cleanup. |
| 4.3 ptone/scion#2202 Final verification | Keys path complete and retired route fails safely; checks tied to a revision; callers migrated or receive an intentional error; no outstanding accepted findings; deferred work separated | This document; the two new tests; migration guide | Verified at the tested revision on upstream `main`; fork PR referencing ptone/scion#2202. |

## 5. Open items found during core verification

These were not blockers recorded by any core review; they are listed so the issue owner sees them
when closing core:

- ptone/scion#2721 (closed): in an unlinked local project, the created-agent on-disk scan in
  `agent.List` ignored the `scion.name` filter, so `scion keys <name>` could report an ambiguity or
  pick a created-only agent that was not requested. Fixed upstream by GoogleCloudPlatform/scion#2349
  (`f8af776c`), which is in the tested base; the local keys tests in §1.1 pass on it.
- ptone/scion#2724 (open): a bare `SCION_HUB` env var breaks settings load. It was found during
  4.1 but is a config issue, not a keys issue.

## 6. Deferred (not part of core completion)

| Item | State | Why deferred |
| --- | --- | --- |
| Phase 5: ptone/scion#2190, task 5.1 ptone/scion#2203 (use the common relationship authorization pipeline) | Open, not dispatched | Depends on the auth refactor (ptone/scion#2119, #2120). It is a separate lane by design and must not be marked complete with core. |
| ptone/scion#2460 agent-caller relationship check in the keys gate | Closed; delivered in upstream #2224 | Listed for completeness; Phase 5's broader pipeline work stays open. |
| ptone/scion#2585 apiclient retry loop does not rewind request bodies on 5xx | Open | General client defect. Keys bypasses retries (`TestSendKeys_NoReplay_WithRetryConfigured`), so the keys no-replay guarantee does not depend on it. |
| ptone/scion#2628 classify never-dispatched HTTP-route failures as `keys_unavailable` | Open | Outcome-classification precision: today the result is a safe but pessimistic `keys_outcome_unknown`. |
| ptone/scion#2647 deterministic concurrency test for rate budgets | Open | Test-precision follow-up from 4.1. |
| ptone/scion#2726 filter parity and created-only messaging in the local on-disk scan | Open | Hardening follow-up from the ptone/scion#2722 review. |
| ptone/scion#2877 propagate early cancellation to queued broker keys requests | Open | Efficiency improvement. Expiry is already re-checked before injection, so this is not a safety gap. |
