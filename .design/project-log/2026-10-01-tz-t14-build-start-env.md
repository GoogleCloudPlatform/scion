# tz task #14 (P2a-0): extract `buildStartEnv`

**Date:** 2026-10-01
**Branch:** `scion/tz-t14`
**Fork issue:** ptone/scion#2507 (part of ptone/scion#2457, design Option A)

## What changed

`DispatchAgentStart` and `DispatchAgentRestart` (`pkg/hub/httpdispatcher.go`)
each independently assembled the resolved agent environment: applied-config
env, model/thinking-level overrides, Hub-storage env (user/project/hub/broker
scopes, plus progeny), type-aware secrets, agent identity and
hub-connectivity vars, workspace sharing mode and git-ness, GCP identity
vars, a fresh Hub auth token, a transport token, and any GitHub App lifecycle
token. The two copies had already drifted slightly in structure (identical
output, different internal variable names and call order for `resolveSecrets`
err handling) and would only keep drifting as later tz-refactor tasks
(P2a-B, P2a-1) add a `TZ` resolver into this path.

Extracted the shared logic into one new unexported method,
`(d *HTTPAgentDispatcher) buildStartEnv(ctx, agent, caller, startedVerb) startEnvResult`,
called from both `DispatchAgentStart` and `DispatchAgentRestart`. `caller`
("DispatchAgentStart"/"DispatchAgentRestart") and `startedVerb`
("start"/"restart") exist only to keep each dispatch path's existing
log-message and warning wording byte-identical — the one place the two paths'
wording differed was the secrets-resolution-failure message ("agent will
start/restart without injected secrets").

`startEnvResult` bundles everything a caller needs afterwards: `env`,
`classifications`, `secrets` (only `DispatchAgentStart` passes this to
`StartAgent`; `RestartAgent` takes no secrets param), `storageEnvCount` (for
`DispatchAgentStart`'s existing debug summary log), `projectInfo` (for
`DispatchAgentStart`'s `projectPath`/`projectSlug`/`sharedDirs`/
`sharedWorkspace`), and `workspace` (for `DispatchAgentStart`'s
`StartExtras.Workspace`; restart never recreates the workspace, matching
today's behavior).

This is pure scaffolding for P2a-B (task #15) and P2a-1 (task #16), which
will add the hub-authority `TZ` rule and `resolveAgentTZ` inside
`buildStartEnv` without having to touch two call sites.

## Why one behavior-neutral reordering was necessary

`DispatchAgentStart` used to call `resolveDispatchProjectInfo` *before*
building `resolvedEnv` (it needs `projectInfo.projectPath`/`projectSlug` for
the later `StartAgent` call); `DispatchAgentRestart` called it lazily, right
before the workspace-mode block, since restart never needs the path/slug.
Sharing one function body meant picking one position; `buildStartEnv` now
calls it at the point `DispatchAgentRestart` already used (right before the
workspace-mode use), and `DispatchAgentStart` reads `projectPath`/
`projectSlug` off the returned bundle instead.

This does not change `resolvedEnv`'s final contents, the precedence between
env sources, or any warning: `resolveDispatchProjectInfo` is a pure,
side-effect-free store read whose result isn't consulted by anything written
into `resolvedEnv` before the workspace-mode block in either original
function. It only changes the relative order of two independent store reads
internal to `DispatchAgentStart`. The new golden test below cannot prove this
on its own — both callers now run the same `buildStartEnv`, so it cannot
detect a regression caused specifically by moving the call, and its fixture
doesn't set a workspace mode label, so it never exercises the reordered
block. The evidence for the reorder itself is the inspection argument above,
plus the existing, unedited coverage of that exact block:
`TestHTTPAgentDispatcher_DispatchAgentStart_InjectsWorkspaceMode`
(`httpdispatcher_test.go:5065`),
`TestHTTPAgentDispatcher_DispatchAgentRestart_InjectsWorkspaceMode` (`:5186`),
and `TestHTTPAgentDispatcher_DispatchAgentStart_CarriesWorkspaceDispatchMetadata`
(`:2002`) — all pass unchanged. The golden test's role is narrower: it is a
drift guard against a future caller-side divergence (for example a `TZ`
write landing in only one of the two call sites in tasks #15/#16), not proof
that today's reorder is neutral.

## Test evidence

- New test: `TestHTTPAgentDispatcher_BuildStartEnv_StartAndRestartProduceIdenticalEnv`
  (`pkg/hub/httpdispatcher_test.go`) — a fixture agent with config env,
  project- and user-scope storage env (`InjectionModeAlways`), an
  `InjectionModeAsNeeded` storage var (must not be injected by either path),
  an environment-type secret (must be injected) and a non-environment
  (`file`-type) secret (must not leak into the env map). Calls
  `DispatchAgentStart` then `DispatchAgentRestart` on the same agent against
  the same mock client and asserts `assert.Equal(t, startEnv, restartEnv)` —
  the golden comparison the acceptance criteria asked for.
- All pre-existing `DispatchAgentStart`/`DispatchAgentRestart`/
  `TZInjection_*` tests pass unchanged (no test edits needed): ran
  `go test -buildvcs=false -p 2 ./pkg/hub/... -run
  'TestHTTPAgentDispatcher_DispatchAgent(Start|Restart)|TestHTTPAgentDispatcher_TZInjection'`.
- `gofmt -l` clean on both changed files; `go vet -buildvcs=false -p 2
  ./pkg/hub/...` clean.
- This is a pure refactor of env assembly with no timestamp handling added
  or touched, so the dev-common both-TZ requirement ("wherever a timestamp
  crosses a store or wire boundary in your change") does not apply to new
  logic here. As a belt-and-suspenders check anyway, the targeted dispatch
  tests above (including the new golden test) were re-run under
  `TZ=Asia/Tokyo`: pass, identical to UTC. Under `TZ=Asia/Kathmandu` every
  `createTestStore`-backed test in `pkg/hub` — not just this change's tests —
  fails in store migration: `failed to migrate test store: empty agent role
  backfill: sql: Scan error on column index 6, name "create_time": unsupported
  Scan, storing driver.Value type string into type *time.Time`. This
  reproduces identically on upstream main at e572e72, so it is a pre-existing
  store/`Time.Scan` gap (store-boundary work, task #2 territory), not a
  signal about this change. Confirmed by tz-t14-rev-1 in round-1 review.
- `GOFLAGS="-buildvcs=false -p=2" make ci`: `fmt-check`/`lint`/`check-custom`
  and `pkg/hub` all pass. `test-fast` fails in `./cmd`, `pkg/harness` and
  `pkg/sciontool/supervisor` due to this sandbox session's own leaked
  `SCION_*`/`CLAUDE_CODE_*` container env vars (a real hub endpoint plus a
  native-telemetry-policy conflict), not from this change — confirmed by
  rerunning each with the offending vars stripped (`env -u ...`), all green,
  and the same 4 tests fail the same way on upstream main with the raw
  container env. `go build -buildvcs=false -p 2 ./...` is green separately
  (`make ci` never reaches the `build` step once `test-fast` fails first).
  So `make ci` is not green end-to-end in this container as shipped; it is
  green once the sandbox's own leaked env vars are excluded, and the one
  package this change touches (`pkg/hub`) was green from the first run with
  no scrubbing needed.

## Follow-ups / adjacent observations (not fixed, per scope)

- `workspaceSpecFor`'s doc comment ("`buildCreateRequest` and
  `DispatchAgentStart` both call this single builder") was already stale
  before this change — `DispatchAgentRestart` called it too. Worth a
  one-line comment fix whenever someone is next in that file.
- `resolveEnvFromStorage` always returns a `nil` error today (errors are
  swallowed per-scope inside the loop), so the `if err != nil` branch in both
  original call sites — and now in `buildStartEnv` — is dead code. Not
  touched here since fixing it is unrelated to this extraction's scope (pure
  refactor, no behavior change), but worth a follow-up if anyone revisits
  this function's error handling.
