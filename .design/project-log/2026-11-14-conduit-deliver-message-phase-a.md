# Conduit deliver-message: message delivery over the agent session (Phase A)

**Date:** 2026-11-14
**Branch:** scion/exec-schedules-a
**Issue:** ptone/scion#7353 (Phase A of ptone/scion#7818; follow-up ptone/scion#7881)
**Design of record:** `.design/conduit/v2.22-deliver-message.md` (FROZEN 2026-11-14)

## What was already on main

- `hub.conduit_deliver` registered (server layer, alpha, off).
- `pkg/agent/tmuxdeliver`: the four-op step builder, the sanitiser, the three-key allowlist and route bounds, and the readiness loop, with the sanitiser table, real-tmux, shipped-harness-bounds and import tests. Nothing outside the package used it.

## What this branch adds

- **Frozen design** (`.design/conduit/`): r3.1 re-anchored to upstream `1fa4d50`, D11 = (b) applied (no hold-until-session; ptone/scion#7881), freeze notes 1-3, and freeze gaps G1-G7. G2-G7 were accepted by exec-schedules-em. G1 (bind accepts `pending` or `dispatched`) was proposed and sent to ptone; it is one constant (`bindableDispatchStates`).
- **`pkg/agentcontrol`**: the wire contract for body-carrying control routes (request schema, strict decode, bounds, outcomes, timing budget). The hub, the broker and sciontool share it, and Phase B adds the exec routes to it.
- **sciontool route** (`pkg/sciontool/control/deliver.go`): fixed schema, the hard stop with skew guard, the local readiness wait (capped at the hard stop), one flush at a time (409 `busy` / `in_progress`), and the agent-side key store (1024 entries, LRU, 15 min; `delivered`, `partial` and `expired_before_paste` are stored, while failures before any visible step are not). The local Runner runs tmux as the conduit PTY user. The route is registered only with the launch flag, tmux on PATH and the harness in tmux session `scion`. The handler doc records the §3.7 keyed-idempotency amendment.
- **Hub**: `callAgentControlJSON` (body, response body, per-route timeout); the launch flag (`hub.conduit_deliver` in the dispatch experiments and `SCION_HUB_EXPERIMENTS`, only while conduit is served); `POST /api/v1/runtime-brokers/{id}/agents/{agentId}/deliver`, which checks the experiment, the broker identity and assignment, validates the schema, binds the rows, sets `broker_id` and `not_after`, calls only `deliver-message` (110 s), and unbinds, finishes or replays. Also the authz entry `broker.agent.deliver`, the stale-delivering sweep in the broker message sweep, and `/message-failures` failing `partial` rows (G5).
- **Store**: states `delivering`/`delivered`/`partial`, additive columns `delivery_id`/`delivering_at`/`delivery_outcome` and a partial index. Bind, unbind, finish and sweep are compare-and-set methods. No destructive migration.
- **Broker**: the exec path now runs `tmuxdeliver`, so the sanitiser applies on exec too (a visible change for text with control characters). Message ids travel through the debounce buffer. Agents launched with the flag get the label `scion.conduit_deliver`. On Cloud Run, a flush with hub ids and in-bounds settings goes to `/deliver`. Not-reached answers retry 3 times, then exec. `partial`, `in_progress` and ambiguous answers become `PartialDeliveryError`. `bound`, `expired` and `invalid` are final. The injection lock is held across the call. Conduit targets skip the broker readiness probe (G7).

## Limits and follow-ups

- A broker with more than one hub connection keeps exec (it cannot tell which hub owns the agent).
- The stale-delivering sweep runs with the 5-minute broker message sweep, not on every reconcile pass.
- Docker and Kubernetes stay on exec (design §7 Phase B, opt-in per broker, later).
- Criterion 19 (A4) is operational: rerun it with this traffic before enabling on shared or production hubs.
- The §6.3 docker integration test (kill the session mid-delivery, replay) was not run here; the hub tests drive a real conduit session with the real sciontool route instead.
- `pkg/agent/harness_ready.go` still keeps its own copy of the readiness loop. `tmuxdeliver.Wait` is the same logic. Moving the broker onto it is a cleanup, not needed for A1-A6.

## Verification

All gates were run in the agent container, with `SCION_*` stripped, on the branch rebased on upstream `ddbdfa9`. The later rebase onto `ad80ecf` only changes `LATEST.json`.

- `make fmt-check lint-ci check-custom test-fast-ci build` (the agent-safe split from `docs-repo/agents/sandbox-go-toolchain.md`): **passed**. Output includes "Go formatting OK.", "All custom checks passed." and "Binary: ./build/scion". Package results: `pkg/agent` ok, `pkg/agent/tmuxdeliver` ok, `pkg/agentcontrol` ok, `pkg/runtimebroker` ok, `pkg/sciontool/control` ok, `pkg/store/entadapter` ok.
- `make test-hub-shard SHARD=K` and `make test-hub-shard SHARD=K NOSQLITE=1` for K = 1..6: **all passed**. The first shard 1 run found one failure: `TestCatalogHTTPEntryPoints_LiveMethodCheck` probed the experiment-gated `/deliver` route, which answers 404 while the experiment is off. It is fixed in 832c7a3 with the standard exclusion entry, and shard 1 passed on the rerun in both modes.
- `make vet-hub-shard SHARD=K` for K = 1..6: **all passed**. Shard 1 was re-vetted after the fix.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main` on the touched non-hub packages, and the separate `--tests=false ./pkg/hub` run: **0 issues** each.
- `./hack/check-authorization-catalog.sh`, `check-route-authz-manifest.sh` and `check-method-not-allowed.sh`: **passed**. The catalog report `.design/authorization-operation-catalog.md` was regenerated.
- `go generate ./pkg/ent`: no drift after the rebase.
- `GOOS=windows GOARCH=amd64 go build ./cmd/scion` and `hack/winsec-guard.sh`: **passed**.
- Real tmux: `TestRealTmux_Deliver512KiB` passed, delivering 512 KiB byte-exact through the route and the local Runner.
- **Not run:**
  - criterion 19 (A4), which is operational;
  - the §6.3 docker integration test (no docker daemon here);
  - full `make ci`, because it compiles the unsharded `pkg/hub` test package, which the repo rules forbid.

## Review round 1 (head 360a9e4 → fixes)

Code review (4 Required) and security review (1 Medium), with exec-schedules-em's dispositions:

- **R1, same-id duplicate could unbind a live delivery and allow a second paste.** The binding call is now the delivery's only owner across hub replicas. A same-id call on `delivering` rows gets 409 `in_progress` and never reaches the agent; only the owner unbinds or finishes the rows. This amends frozen §2.6 ("forwarded"); the amendment is recorded in the design. Tests: `TestBrokerAgentDeliver_ConcurrentSameIDHasOneOwner` (the reviewer's repro made a regression) and `TestBrokerAgentDeliver_OwnerUnbindOnlyAfterDuplicateRefused` (owner statuses 408, 409 busy, 413, 422, 429, 501, 503).
- **R2, the broker's hub client cut `/deliver` off at its 30 s default timeout.** The request now uses `apiclient.WithRequestTimeout(115 s)`, keeping no retry, no redirect and the caller's deadline. Tests with a real HTTP client: `TestDeliverMessage_OutlastsClientTimeout` (fails without the fix) and `TestDeliverMessage_NoRetryNoRedirect`. No other hubclient call was added in this PR.
- **R3, an oversize flush got a permanent 400 instead of exec.** `agentcontrol.SizeError`/`FitsRoute` cover raw text, JSON-encoded size (760 KiB) and the 64-id limit. The hub and sciontool answer 413 `too_large`; a malformed request stays 400. The broker checks the limits before choosing conduit and falls back to exec, failing with the named limit when exec is unavailable. Tests: `TestConduitDelivery_OversizeUsesExec` (both limits, encoded size, exec available and unavailable), `TestBrokerAgentDeliver_SizeLimitIs413`, `TestDeliverSizeLimits`.
- **R4, a failure report could overwrite `delivered`, and failure-first left no replayable outcome.** Store transitions are now conditional. `FailUndeliveredMessage` returns whether it won. `MarkMessageFailed` never fails `delivered`, and failing a `delivering` row stores partial/unknown in the same write. `/message-failures` notifies only when it won the transition. Tests: `TestFailureVersusFinish` (both orderings), `TestBrokerMessageFailures_NeverFailsDelivered`, `TestExecuteAgentDMFinalization_KeepsDelivered`.
- **S-A-01, `interrupt_paste` skipped the hard stop after its first key.** The hard stop is now checked before every `paste-buffer`. An expiry after the interrupt keys is a stored partial; an expiry before anything visible stays 408. Test: `TestDeliverHardStopAfterInterrupt` (fake clock, slow interrupt and slow load).
- **Attachments (FYI).** Attachment presence now travels with the flush, and a flush or interrupt with attachments uses exec. Test: `TestConduitDelivery_AttachmentsUseExec`.
- Declined or deferred per the dispositions: the govulncheck baseline, and the operational items (criterion 19, live Cloud Run, Docker integration).

### Round 1 verification (head 59220c6, base upstream `ad80ecf`)

- `make fmt-check lint-ci check-custom test-fast-ci build`: passed.
- `make test-hub-shard SHARD=K`, `make test-hub-shard SHARD=K NOSQLITE=1` and `make vet-hub-shard SHARD=K` for K = 1..6: all passed.
- golangci-lint `--new-from-rev=upstream-main` on the touched packages, plus `--tests=false ./pkg/hub`: 0 issues each.
- Windows cross-build of `cmd/scion` and `hack/winsec-guard.sh`: passed.
