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
