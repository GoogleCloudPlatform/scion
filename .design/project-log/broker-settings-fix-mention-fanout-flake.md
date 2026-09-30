# Test hygiene: leaked background goroutines in pkg/hub testServer (ptone/scion#2418 investigation)

Branch `scion/broker-settings-fix-mention-fanout-flake`, cut from `upstream-main`
(`GoogleCloudPlatform/scion` main) and rebased onto its later tip `169540efc` before opening the PR.
PR `ptone/scion` (see message to EM for the number), not draft. Part of `ptone/scion#2061`. Task
brief: investigate the order-dependent MentionFanout flake, `ptone/scion#2418`.

## Summary

This PR does **not** claim to fix `ptone/scion#2418`. It fixes a real, independently-confirmed test
hygiene bug found while investigating that flake, and references the issue only as a possible
contributor. The flake itself was never reproduced deterministically and has never been observed in
a real CI run.

## What was investigated

Task: find the "polluter" test responsible for
`TestMentionFanout_ParticipantRegistrationSurvivesExpiredAggregateContext`
(`pkg/hub/agent_mention_fanout_test.go`) occasionally getting delivery status `"error"` instead of
`"delivered"` in a full-suite run, per the `gs://scion-xproject-exchange/ci-main/findings.md`
(section 2) finding and the `ptone/scion#2337` precedent's method.

- Mined the actual CI job logs the finding was based on (`gh api .../actions/jobs/109999090628/logs`
  and `.../109999090664/logs`, run 36748018756): no "MentionFanout" match in either. This failure has
  never been observed in a real CI run — only in an investigator's own ad hoc local
  `go test -p 2 -count=1 -timeout 30m ./pkg/hub/` run.
- Ruled out `ptone/scion#2366` (the LogCapture flake's slog.Default-hijack fix): its polluter test,
  `TestAgentPortProxyThroughTunnel`, runs at position 6121 of 8040 in default `go test -list` order;
  the MentionFanout victim runs at position 510 — 5611 tests earlier — so it cannot be responsible in
  default order, and the two failures' mechanisms are unrelated regardless.
- Ruled out `ptone/scion#2417` (real-HOME-write pollution): no evidence connects filesystem writes to
  this victim's failure; the mechanism found (below) is in-process goroutine/scheduler contention, not
  filesystem state.
- 6 full-suite reproduction attempts on bare upstream main (`go test -p 2 -count=1 -timeout 30m
  ./pkg/hub/...`: 2 default-order, 2 `-shuffle`, one each at `GOMAXPROCS=2` and `GOMAXPROCS=4`) never
  reproduced the flake, on a 32-core/125GiB sandbox far heavier than a GitHub-hosted runner.
- `go test -race` on a ~104-test neighborhood (every test in `agent_mention_fanout*_test.go`,
  `agent_dm_operation_test.go`, `agent_dm_parity_test.go`, `notifications_test.go`) reproduced the
  victim failing 1 time in 4 attempts on unfixed code — with status `"unauthorized"`, not the
  originally reported `"error"` — and 0 times in 4 attempts with the fix below applied. This is
  evidence of a probable contributor, not proof of the root cause: the flake's status string differs
  from the original report, and the base failure rate (1/4) is itself not fully deterministic.

## What was found and fixed (the actual bug, confirmed independently of the flake)

`New()` (`pkg/hub/server.go`) unconditionally starts 5 background goroutines on every `*Server` it
constructs:
- `newChatLinkService()` (`pkg/hub/chat_link_service.go:54-61`), called once each for
  `telegramLinkService`, `discordLinkService` and `teamsLinkService` (`server.go:1379-1385`), each
  spawns a `cleanupLoop` goroutine (1-minute ticker).
- `NewBrokerAuthService()` -> `NewNonceCache()` (`pkg/hub/brokerauth.go:93-101, 144-156`) spawns a
  `cleanup` goroutine (ticker at half the nonce TTL).
- `NewPreviewService()` (`pkg/hub/access_constraint_preview.go:95-112`) spawns a `cleanupNonces`
  goroutine (1-minute ticker).

`testServer(t)` and `testServerWithBrokerAuth(t)` (`pkg/hub/handlers_test.go`), used at roughly 2200
call sites across 255 `_test.go` files in `pkg/hub`, only ever called
`srv.Shutdown(context.Background())` in `t.Cleanup`. That is a no-op here:
`Server.Shutdown()` (`server.go:4544-4552`) returns immediately whenever `s.httpServer` is nil, which
it always is for these tests — they exercise handlers directly via `httptest.NewRecorder()` and never
call `Start()`. Even on the path where `Shutdown()` does run its body, it still never closes the three
link services or `previewService` — only the separate `Server.CleanupResources()`
(`server.go:4613-4674`, documented for the combined-mode "no listener of its own" case, which is
exactly what a unit test is, just never wired to one) closes those.

Confirmed via a goroutine dump printed by a `-race` run that hit `go test`'s default 10-minute
timeout: exactly 267 goroutines parked in `chatLinkService.cleanupLoop`, exactly 89 in
`NonceCache.cleanup`, and exactly 89 in `PreviewService.cleanupNonces` — 267 = 89*3, matching exactly
89 leaked `testServer()`-shaped constructions each leaking precisely 5 goroutines. Separately, a
temporary uncommitted `runtime.NumGoroutine()` probe showed 4053 live goroutines after just the 509
tests that precede the MentionFanout victim in default order.

## Fix (test-only)

Added the missing `Close()` calls to both helpers' `t.Cleanup`, alongside the existing `Shutdown()`
call:
```go
if srv.telegramLinkService != nil { srv.telegramLinkService.Close() }
if srv.discordLinkService != nil { srv.discordLinkService.Close() }
if srv.teamsLinkService != nil { srv.teamsLinkService.Close() }
if srv.brokerAuthService != nil { srv.brokerAuthService.Close() }
if srv.previewService != nil { srv.previewService.Close() }
```
All five `Close()`/`Stop()` methods are `sync.Once`-guarded (verified by reading each), so calling
them here is safe even for the few tests (`chat_link_handlers_test.go`) that already close these same
services themselves via their own `t.Cleanup`.

## Not fixed here (filed as follow-ups)

1. 17 other `pkg/hub` test files construct a `*Server` via `New(cfg, s)` directly instead of via
   `testServer(t)` and may share the identical leak pattern:
   `authz_bypass_agents_test.go`, `ge_exchange_route_test.go`, `handlers_auth_test.go`,
   `handlers_oidc_test.go`, `list_cursor_seal_test.go`, `seed_roles_test.go`, `server_test.go`,
   `template_bootstrap_test.go`, `template_file_handlers_test.go`,
   `reserved_platform_identity_test.go`, `agentrole_integration_test.go`, `bootstrap_test.go`,
   `external_bearer_ratelimit_test.go`, `harness_config_file_handlers_test.go`,
   `system_handlers_test.go`, `rs1_extended_test.go`, `workspace_handlers_test.go`.
2. `Server.Shutdown()`'s `if srv == nil { return nil }` guard (`server.go:4550-4552`) looks like a
   latent production gap independent of tests: any real caller invoking `Shutdown()` before `Start()`
   silently skips `ctxCancel()`, `brokerAuthService.Close()`, `scheduler.Stop()`,
   `notificationDispatcher.Stop()`, `lifecycleHookEvaluator.Stop()`, `presenceManager.Stop()`,
   `events.Close()` and `commandBus.Close()`. Separately, `previewService.Close()` is called by
   neither `Shutdown()` nor `CleanupResources()` in production code at all.

Both filed as fork issues on `ptone/scion`, referencing this PR and `ptone/scion#2418`.

## Gates run

- `go vet ./pkg/hub/`: clean.
- `golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/...`: 0 issues.
- `gofmt -l pkg/hub/handlers_test.go`: clean.
- `go test -race -run 'TestMentionFanout|TestProcessMentions' -count=1 ./pkg/hub/`: see PR/message
  for result.
- `go test -p 2 -timeout 30m ./pkg/hub/...` (full package, fixed branch): see PR/message for result.
- Full `-race` neighborhood evidence (4 unfixed attempts, 4 fixed attempts) recorded in
  `/scion-volumes/scratchpad/projects/broker-settings/reviews/fix-mention-fanout-evidence.md`.

## Full evidence

All commands, counts, and the honest characterization of what this does and does not prove:
`/scion-volumes/scratchpad/projects/broker-settings/reviews/fix-mention-fanout-evidence.md` (scratch,
not part of this repo).
