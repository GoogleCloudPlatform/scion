# Phase 2a round-1 dev report

Branch: `scion/agent-reincarnate-2`
Previous head (reviewed): `abcb94f4d2f4ff3238d83b7e2e03e6d45baf1111`
New head SHA: fae6d627e37bdb705cb24ad808c4507f5da8c048 (code+test commit); this report is committed on top.
Base: upstream-main `ec90cb3359890ef5dcaff4404d05e832bc5aba39` — re-fetched twice during this round; still current, **no rebase needed**.
Review addressed: `reviews/p2a-r1.md` (ar-rev-2a-1, REQUEST CHANGES, 4 Required)
Dispositions implemented: design.md Amendment **A25.1**, as written.

## 1. Per-item table

| Item | Disposition | File:line | Test(s) |
|---|---|---|---|
| **R1** — human-sender deferred path skipped `--notify` and @mention fan-out | Accepted as written: gate only the dispatch, not the post-delivery adapter work. `req.Notify` and `processMentions` now run unconditionally; `registerGroupPrimary` stays skipped (participant = dispatched semantics) with a comment; `MentionResults` is included in the deferred response. | `pkg/hub/handlers_agent_messaging.go:1541` (`reincarnating` computed once), `:2100-2113` (Notify, unconditional), `:2115-2122` (`registerGroupPrimary`, still gated `!reincarnating`), `:2124-2132` (`processMentions`, unconditional), `:1968` (`MentionResults` in the deferred response struct) | `TestHandleAgentMessage_R1_DeferredStillRunsNotifyAndMentions` |
| **R2** — deferred 202 returned when persistence failed | Accepted as written: a persist failure while reincarnating now returns 500 (`failed to persist message; agent is reincarnating, retry`), never 202. | `pkg/hub/handlers_agent_messaging.go:2006-2010` | `TestHandleAgentMessage_R2_PersistFailureOnDeferredPathIsNot202` |
| **R3** — gate missing on group[] fan-out, chat v2, `processMentions`, legacy broker-inbound; scheduler | Accepted, all paths, as written. Each of the four message paths gets the same persist-deferred/skip-dispatch/report-deferred treatment; the scheduler fails loudly instead of dispatching or silently succeeding. | group[]: `pkg/hub/handlers_agent_messaging.go:2304-2311` (gate computed), `:2404-2410` (dispatch skip) · chat v2: `pkg/hub/handlers_chat_v2.go:1392-1399` (gate), `:1444-1447` (persist), `:1560-1563` (dispatch skip) · mentions: `pkg/hub/handlers_agent_messaging.go:2997-3004` (gate), `:3041-3047` (dispatch skip) · legacy broker-inbound: `pkg/hub/handlers_broker_inbound.go:423-441` (dispatch skip; this endpoint dispatches before persisting, so the gate wraps the dispatch call itself), `:471-475` (persisted DispatchState), `:566-572` (response) · scheduler: `pkg/hub/server.go:3536-3544` | `TestHandleGroupMessage_R3_MigratingRecipientDeferred`, `TestSendAgentRouted_R3_MigratingPrimaryDuringProvisioningDeferred`, `TestProcessMentions_R3_MigratingMentionedAgentDeferred`, `TestHandleBrokerInbound_R3_MigratingRecipientDeferred`, `TestMessageEventHandler_R3_MigratingTargetFailsLoudly` |
| **R4** — preamble's catch-up window end was wrong and unrunnable as written | Accepted as written: dropped the end timestamp; step 2 is now "Run `scion conversation list` ..., then for each run `scion conversation catch-up <ref> --since <duration>`, choosing a duration long enough to reach back to {start}. Messages sent to you since {start} were saved ..., not dropped." `#1910` fallback sentence kept. `buildReincarnationPreamble` dropped its `migrationEnd` parameter. | `pkg/hub/reincarnate_worker.go:745-800` (function + docstring), `:461-462` (call site) | `TestBuildReincarnationPreamble_CatchUpWindow` (rewritten) |
| **O1** — residual window after state clears | Declined as written — no code change. Corrected in this report (§3 below): the gated paths' post-clear behaviour is a loud 409, not the #1820 silent drop. | (report text only) | n/a |
| **O2** — broadcast persistence; deferred notice on pub/sub | Accepted in part, as written. Broadcasts keep the pre-existing #1820 rejection (no conversation exists for a deferred broadcast to be caught up on). The deferred notice to agent senders is implemented: `publishDeliveryDeferred` mirrors `publishDeliveryFailed` with a distinct `DELIVERY_DEFERRED` status/`delivery-deferred` system category. | `pkg/hub/messagebroker.go:759-773` (broadcast carve-out: `deferred := reincarnationInFlight(agent) && !msg.Broadcasted`), `:894-908` (call site), `:1067-1099` (`publishDeliveryDeferred`); `pkg/messages/types.go` (`SystemCategoryDeliveryDeferred`) | `TestDeliverToAgent_O2_BroadcastToMigratingAgentKeepsRejection`, `TestDeliverToAgent_O2_AgentSenderGetsDeferredNotice` |
| **Nit** — `migrationStart` should be the claim time, not `rec.RequestedAt` | Accepted. `runReincarnationWorker` now takes `migrationStart time.Time` from the handler's `claimedAt` (the exact `ReincarnationUpdatedAt` stamp at claim, `handlers_agent_reincarnate.go:361-368`), instead of re-reading the record a few ms later. | `pkg/hub/handlers_agent_reincarnate.go:421-427` (call site passes `claimedAt`), `pkg/hub/reincarnate_worker.go:375, 461-462` (signature + use; the `GetAgentReincarnation` re-read is gone) | Covered by existing reincarnate-worker tests (signature change); no new test needed — this is a value-source change, not new behavior. |
| **Nit** — F4 fix commit corrected | Accepted. §1 of `p2a-dev-report.md`'s claim that the fix landed in `ec90cb335` is corrected below (§4) to `7da41f865` (ptone/scion#1909, #1914). `ec90cb335` is where this branch is based, not where the fallback was introduced — my earlier `git log -S` ran against a shallow single-commit fetch and could not see the true origin commit; unshallowing found it. | (report text only) | n/a |
| **Nit** — `OutboundMessageResult.Deferred` comment hard to follow | Accepted. Comment simplified to "Deferred is set only when Status == \"deferred\"." | `pkg/hubclient/agents.go:582-584` | n/a (comment-only) |

## 2. Mutation proofs

Per-fix, I reverted the fix in place (via a scripted string replacement, not `git stash`, to keep the diff clean), ran the one regression test the fix exists for, confirmed it failed with the exact symptom the review described, then restored the file and reconfirmed the test passes. All eight mutations are below; each shows the fix is load-bearing for its test.

| Fix | Mutation | Test result without fix |
|---|---|---|
| R1 | `req.Notify` and `processMentions` re-gated with `&& !reincarnating` | FAIL: `"[]" should have 1 item(s), but has 0` — the mention result vanished |
| R2 | `if reincarnating && persistedMsgID == ""` → `if false && ...` | FAIL: `Should not be: 202` — response was `{"status":"deferred","message_id":""}` |
| R3 / group[] | `recipDeferred := reincarnationInFlight(agent)` → `:= false && ...` | FAIL: `-deferred / +dispatched` |
| R3 / chat v2 | `primaryReincarnating := reincarnationInFlight(primaryAgent)` → `:= false && ...` | FAIL: dispatcher recorded a dispatch to the migrating primary during `provisioning` |
| R3 / mentions | `mentionDeferred := reincarnationInFlight(mentionAgent)` → `:= false && ...` | FAIL: `-deferred / +dispatched` |
| R3 / broker-inbound | `agentReincarnating := reincarnationInFlight(agent)` → `:= false && ...` | FAIL: `-deferred / +dispatched` |
| R3 / scheduler | `if reincarnationInFlight(agent)` → `if false && ...` | FAIL: error no longer contains "reincarnating" (fell through to an unrelated authz error instead) |
| O2 / broadcast | `deferred := reincarnationInFlight(agent) && !msg.Broadcasted` → drop `&& !msg.Broadcasted` | FAIL: a row was persisted (`DispatchState: deferred`) for a broadcast, which the fix says must not happen |
| O2 / notice | `p.publishDeliveryDeferred(ctx, agentSlug, msg)` commented out | FAIL: `Expected value not to be nil` — sender received no notice |

All restores were verified: `go build -buildvcs=false ./...` and the affected test both pass after each revert.

## 3. O1 wording correction (declined item, report-only)

The original `p2a-dev-report.md` §2 described the residual post-clear window as related to "the same pre-existing ordinary-start race as #1820" and implied a silent drop. The reviewer is right that this is not accurate for the three gated paths (`handleAgentMessage`, `ExecuteAgentDM`, `deliverToAgent`): `validateAgentDeliverable` (`wake_dm.go:249-254`) and the human phase switch (`handlers_agent_messaging.go`) return a **loud 409** — "not yet running ... wait for it to reach running state" — for the interval between `reincarnation_state` clearing and the container's first status report moving phase to `running`. The broker path calls `publishDeliveryFailed`, also loud. So in the gated paths there is a **behaviour cliff, not a silent drop**: one message one second before the clear is deferred and saved; one message one second after is rejected and not saved, but the sender is told either way. The only path where messages during that narrow window are *dispatched* rather than rejected is chat v2's `sendAgentRouted`, and that is exactly R3's fix — once `reincarnationInFlight` is false there (state cleared), `isAgentUnreachable` still governs and behaves as it always has for a `starting` phase (no phase check to reject it, same pre-existing chat v2 behavior, unrelated to migration). Per A25.1, extending the gate to the first status report (leaving `reincarnation_state=starting` until phase actually becomes `running`) is deferred to Phase 3 as it touches the sweep/resume logic; no code change made here.

## 4. F4 commit correction (nit, report-only)

`p2a-dev-report.md` §1 stated the `IsHubContext()` fallback in `requireHubClient` (`cmd/notifications.go:203`) landed in `ec90cb335`. That commit is where this branch is based — the whole conversation/notification subsystem (including that fallback) is new relative to any earlier point I could see, but the actual origin commit, once the repo was un-shallowed (`git fetch --unshallow` — my original single-commit fetch could not see any history before `ec90cb335`), is:

```
7da41f86 fix(cli): allow conversation/notifications commands in agent hub context (ptone/scion#1909) (#1914)
```

`git merge-base --is-ancestor 7da41f865 upstream-main` confirms it is an ancestor of `ec90cb335`. The conclusion is unchanged: the fix is on `upstream-main`, and the harness image build `c1506776` (the stock CLI in my own container, used for the original live-reproduction of F4) predates it. The minimum in-image CLI version needed is "at or after `7da41f865`", not "at or after `ec90cb335`" as originally (imprecisely) stated.

## 5. File-by-file summary (this round only)

- `pkg/hub/handlers_agent_messaging.go` — R1 (notify/mentions run unconditionally; MentionResults in the deferred response), R2 (persist-failure 5xx), R3 for group[] fan-out and `processMentions` (per-recipient gate, `deferred` status).
- `pkg/hub/handlers_chat_v2.go` — R3 for `sendAgentRouted` (`primaryReincarnating` computed ahead of `isAgentUnreachable`, deferred instead of failed, dispatch skipped).
- `pkg/hub/handlers_broker_inbound.go` — R3 for the legacy external-channel inbound endpoint (dispatch call wrapped in the gate check; this endpoint dispatches before persisting, so the persisted row's `DispatchState` and the JSON response's `deferred` field are set from the same flag computed before dispatch).
- `pkg/hub/server.go` — R3 for the scheduler's `messageEventHandler` (explicit error instead of a dispatch attempt).
- `pkg/hub/messagebroker.go` — O2: broadcast carve-out on the existing gate predicate; new `publishDeliveryDeferred` (mirrors `publishDeliveryFailed`).
- `pkg/messages/types.go` — new `SystemCategoryDeliveryDeferred` constant for the O2 notice.
- `pkg/hub/reincarnate_worker.go` — R4 (preamble wording, dropped `migrationEnd` param) and the claim-time Nit (`migrationStart` parameter added to `runReincarnationWorker`, no more `GetAgentReincarnation` re-read).
- `pkg/hub/handlers_agent_reincarnate.go` — Nit: passes `claimedAt` to `runReincarnationWorker`.
- `pkg/hub/handlers_agent_reincarnate_test.go` — golden test rewritten for R4's new wording and the dropped parameter.
- `pkg/hubclient/agents.go` — Nit: simplified `OutboundMessageResult.Deferred` comment.
- `pkg/hub/reincarnation_gate_r1_test.go` (new) — all nine round-1 tests (§1 table).
- `pkg/hub/msg_containment_callsite_test.go` — governance-registry entry for the new `publishDeliveryDeferred` → `DispatchAgentMessage` call site (caught by the full-suite gate, see §6).

## 6. Gates

- `GOCACHE=/tmp/gocache-ar-dev-2` throughout; never touched `/scion-volumes/gocache`, never ran `go clean -cache/-modcache`.
- All commands run with `env -u SCION_AUTO_EXPOSE_PORTS -u SCION_PROJECT -u SCION_GROVE -u SCION_HUB_ENDPOINT -u SCION_HUB_URL -u SCION_HUB_PROJECT_ID -u SCION_CLI_MODE` per the brief.
- `go build -buildvcs=false ./...` — clean.
- `go vet ./pkg/hub/...` — clean.
- `make fmt` — no diff beyond the intended changes (`git status`/`git diff --stat` identical before/after).
- `go test ./cmd/...` — **ok** (`cmd` 31.6s, `cmd/sciontool/commands` 12.4s), including `TestHubAllOrOneActions` (confirmed passing with the brief's env-unset list; it was a leak from this container's own live hub env vars, not a code issue — see the round-0 report for the same note).
- Full `pkg/hub` suite via the exact `test-hub-sqlite` Makefile invocation (`-skip` list, `-timeout 30m`) — **ok**, zero failures. First run caught one real regression (below), fixed, then reconfirmed clean at 921.0s for the main `pkg/hub` package (plus `auth`/`authzop`/`githubapp`/`imagecheck`, all ok).
  - **Regression caught and fixed:** the first run failed `TestExternalEffectCallSiteClassification` (`msg_containment_callsite_test.go`) — a governance test enumerating every `DispatchAgentMessage`/`dispatchWithBrokerRetry` call site and requiring each to be classified `guarded` or `exempt`. My new `publishDeliveryDeferred` (O2) call was unclassified. Added an entry mirroring the existing `publishDeliveryFailed` one (`exempt`, "derivative: delivery-deferred notice to original sender") in `pkg/hub/msg_containment_callsite_test.go`. No production behavior changed by this fix — it is a test-registry addition only. Rerun confirmed clean.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./pkg/store/... ./pkg/hubclient/... ./pkg/messages/... ./cmd/...` — **0 issues**.
- Targeted regression run covering every touched area (`Reincarnat|R1_|R2_|R3_|O2_|TestHandleAgentMessage_|TestOutboundMessage_|TestAgentMessage_|TestBroadcast_|TestGroupMessage_|TestSendAgentRouted|TestDeliverToAgent|TestDelivery_|TestBufferedFlush`) — ok, 65.7s, zero failures.
- Upstream main re-checked twice this round (once via `--unshallow`, once via a fresh `main:` fetch): still `ec90cb3359890ef5dcaff4404d05e832bc5aba39`. **Branch is not behind; no rebase performed.**

## 7. Head SHA

`fae6d627e37bdb705cb24ad808c4507f5da8c048` (round-1 code+test commit). Fork PR https://github.com/ptone/scion/pull/2086 updated with this branch.
