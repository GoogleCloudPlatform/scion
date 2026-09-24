# auth-passthrough — Phase 5, bridge half (`hubBearer`)

**Agent:** ap-p5b-dev · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-23

## Summary

Implemented the bridge half of Phase 5 (`impl-design.md` §4.6, §5): a new A2A
bridge auth scheme, `hubBearer`, that forwards any bearer credential the Hub
itself accepts (in particular a Google user ID token attached by an upstream
caller such as Gemini Enterprise) to the Hub's `GET /api/v1/auth/me` for
admission, then forwards the same token verbatim on every downstream Hub
call. No credential exchange, no widening of `hubUAT`. The observability half
(§4.7) is out of scope for this phase (comes after Phases 2-4, per the
brief). `geGoogle` and the v0.3 JSON-RPC/agent-card work from
`GoogleCloudPlatform/scion#1847` are untouched.

## What changed

All changes are inside `extras/scion-a2a-bridge/**`, per the shared-branch
ownership split (Hub developers own `pkg/**`).

- **`internal/bridge/server.go`**
  - `ValidateConfig`'s scheme allowlist (`:154`) and no-api-key exception list
    (`:164`) both admit `hubBearer`.
  - `NewServer`'s per-scheme validator construction: `case "hubUAT",
    "hubBearer":` — both share the same `UATValidator` (identical `/auth/me`
    introspection + cache; the schemes differ only in the middleware guard).
  - `WarnOnOpenAuth`: added the `hubBearer` startup log line.
  - `authMiddleware`: new `case "hubBearer":`, structurally `hubUAT`'s case
    minus the `scion_pat_` prefix check. Empty token → 401
    `unauthorized: missing bearer token` (mirrors `hubJWT`'s existing
    empty-token message, and is what GoogleCloudPlatform/scion#1847 used for its `hubUAT` widening —
    reused here for the new scheme instead). On Hub rejection, logs at Info
    and returns exactly `unauthorized: token rejected by Scion Hub` (cherry-
    picked wording from GoogleCloudPlatform/scion#1847's `hubUAT`-widening hunk, since design §4.6
    asks for the identical message on the new scheme).
- **`internal/bridge/adminoverlay.go`** (`BuildAuthValidators`) — **not** in
  the brief's file list, but required for the feature to work at all in
  production: `cmd/scion-a2a-bridge/main.go` always calls `SetSnapshot`, and
  `authMiddleware` reads `uatV` from the snapshot whenever one is set (which
  is unconditionally, in the real binary) — `NewServer`'s constructor-time
  `s.uatValidator` is dead in that path. Without this change, a real
  deployment configured with `auth.scheme: hubBearer` would 500 on every
  request (`uatV == nil`) despite `NewServer`/`authMiddleware` being correct,
  and every unit test that goes through `NewServer` directly (no snapshot)
  would still pass — so this gap would not have been caught by any test the
  brief's file list implied. Added `case "hubUAT", "hubBearer":` there too,
  mirroring the `hubUAT` construction. Did **not** add `hubBearer` to
  `validAuthSchemes` (used only to gate the Admin-UI hot-apply `Configure()`
  path) — `geGoogle` is excluded from that map today for the same reason
  (schemes not yet exposed to the admin overlay), so this preserves existing
  precedent rather than introducing a new one; `hubBearer` is set via the
  static YAML `auth.scheme` key, which `main.go`'s initial `BuildSnapshot`
  already picks up at boot.
- **`internal/bridge/uatvalidator.go`** — cherry-picked GoogleCloudPlatform/scion#1847's `TokenType`
  split: `scion_pat_*` → `"uat"`, anything else the Hub accepted → `"bearer"`
  (reachable only via `hubBearer`, since `hubUAT`'s prefix check keeps
  non-PAT tokens from ever reaching `Validate`).
- **`internal/bridge/bridge.go`** (`callerHubClient`) — cherry-picked GoogleCloudPlatform/scion#1847's
  `case "uat", "bearer":`, forwarding `RawToken` verbatim with transport auth
  composed, exactly like `"uat"` already did. Did **not** port GoogleCloudPlatform/scion#1847's
  `Channel = "a2a-bridge"` or the `securitySchemes`/agent-card block (v0.3
  work, explicitly out of scope per the design and the brief).
- **`internal/bridge/caller.go`** — documented `"bearer"` in the `TokenType`
  field comment.
- **`internal/bridge/config.go`** — `AuthConfig.Scheme`/`.APIKey`/
  `.UATCacheTTL` doc comments mention `hubBearer`.
- **`README.md`**, **`scion-a2a-bridge.yaml.sample`** — documented the new
  scheme (admin-UI auth-scheme table, sample config comments + a commented
  example block), per the brief's "bridge config docs/sample config" item.
  Hub-side docs (`docs-site/...`) are Phase 4's job, untouched.
- **`internal/bridge/auth_test.go`, `uatvalidator_test.go`,
  `v0_compat_test.go`** — new/extended tests, see the acceptance-row table
  below.
- **`integration/auth_transport_process_test.go`,
  `integration/alternator_harness_test.go`** — B3 process-level test, see
  below.

## B3 investigation: pointing the Hub under test at a test Google JWKS

The brief asked me to investigate this before building anything, and to stop
and ask `ap-em` if the process test couldn't inject a test Google JWKS
without a new production config knob or a `pkg/` change. It doesn't need
one — the existing seam already covers it:

- `pkg/hub/google_credential_validator.go`'s `NewGoogleCredentialValidator`
  takes an `*http.Client` parameter; when passed `nil` (which is what
  `pkg/hub/server.go`'s `New()` does), the constructed `&http.Client{}` has
  no `Transport` set, so `net/http` resolves it to `http.DefaultTransport`
  **at call time**, not at construction time.
- `extras/scion-a2a-bridge/integration/auth_transport_process_test.go`'s
  `serveHubProcess` (the Hub subprocess used by the existing `geGoogle`
  exchange tests) already overrides the **process-global**
  `http.DefaultTransport` with `rewritePinnedGoogleTransport` before calling
  `hub.New(cfg, store)`. Every Google-bound HTTPS request the Hub process
  makes afterwards — including `GoogleCredentialValidator`'s hardcoded JWKS
  fetch (`https://www.googleapis.com/oauth2/v3/certs`) — is transparently
  redirected to the fake Google test process.
- This is the exact seam the design sanctions (`*http.Client`/custom
  `RoundTripper`), already proven to work for the exchange path (which
  shares the same base validator instance in production, per Phase 1's
  server.go wiring). No new production seam, config knob, or `pkg/` change
  was needed; I did not ask `ap-em` because the investigation had a clean
  answer within existing constraints.

What *was* needed, entirely inside `extras/scion-a2a-bridge/integration/`:
1. `serveHubProcess` now also sets `cfg.Federation` (a `user`-type trusted
   issuer for `https://accounts.google.com`, `ExpectedAudience:
   testGoogleClientID`, and an explicit placeholder `JWKSURL` so
   `NewFederationAuthenticator` doesn't attempt live OIDC discovery of the
   real `accounts.google.com` at Hub startup — the placeholder is never
   fetched, since `googleTrust()` only reads `IssuerConfig`'s audience/type
   fields, and cryptographic verification goes through
   `GoogleCredentialValidator` against the pinned, hardcoded JWKS URL
   instead). This mirrors the pattern already used by `pkg/hub`'s own
   `auth_external_bearer_test.go` (`newGoogleTrustFederationAuth`).
2. `serveHubProcess`'s mock router now forwards `GET /api/v1/auth/me` to the
   real `productionHandler` (the same `hubServer.Handler()` already used for
   the exchange endpoint), so the request actually exercises
   `UnifiedAuthMiddleware` → `serveExternalBearer` — real production code,
   not a test double.
3. The mock `/message` endpoint (used only for the test harness's own
   message-receipt bookkeeping) now accepts either a Hub-minted JWT
   (existing `geGoogle`-exchange tests) or, falling back, a Google ID token
   verified via a second `hub.NewGoogleCredentialValidator(nil)` instance —
   needed because `hubBearer` forwards the *original* Google token
   downstream, not an exchanged Hub JWT.
4. New `serveHubBearerBridgeProcess` (factored out of `serveFullBridgeProcess`
   into a shared `serveScionA2ABridgeProcess` helper parameterized by
   `AuthConfig`) and a new harness mode, `hub-bearer-bridge`.
5. `TestHubBearerProcessPassthrough`: fake-Google → real-Hub → real-bridge
   (`hubBearer`) → real-Hub again. Asserts the JSON-RPC call succeeds, the
   Hub's exchange counter stays at **0** (no exchange ever happens), and the
   token the Hub receives on the downstream `/message` call is byte-for-byte
   the token the client originally presented.

## Acceptance rows → tests

| Row | Description | Test(s) |
|---|---|---|
| B1 | Google token admitted via `/auth/me`, forwarded verbatim downstream | `TestAuthMiddleware_AllSchemes/hubBearer/valid-google-token` (admission), `TestUATValidator_TokenTypeClassification` (classified `"bearer"`, `RawToken` untouched), `TestCallerHubClient_BearerTokenType` (forwarding case, mutation-sensitive to reverting `case "uat","bearer"` → `case "uat"`), `TestHubBearer_EndToEnd_PassThrough` (full production wiring: real `New()`/`Server`/executor/SDK handler, asserts the Hub's captured `Authorization` header on the downstream message-send call is byte-identical to the client's original token, and that zero exchange calls occurred) |
| B2 | `hubUAT` still rejects non-`scion_pat_` tokens (unchanged) | `TestAuthMiddleware_AllSchemes/hubUAT/not-pat-prefix` (pre-existing, still green), `TestAuthMiddleware_HubUATVsHubBearer_PrefixGuard/hubUAT_still_rejects` (new: identical token/Hub fixture as the `hubBearer` acceptance case in the same test, exact-byte body assertion) |
| B3 | Process integration test: bridge → Hub pass-through, test Google issuer | `TestHubBearerProcessPassthrough` (`extras/scion-a2a-bridge/integration/auth_transport_process_test.go`) |
| Empty token → 401, exact message | — | `TestAuthMiddleware_HubBearer_EmptyToken` (mock Hub is deliberately permissive — accepts even an empty token — so the test only passes if the bridge's own empty-token guard rejects before ever reaching the Hub; exact-byte body assertion) |
| Rejection message exact, no reason leak | — | `TestAuthMiddleware_HubBearer_RejectedByHub` (mock Hub's rejection body contains a sentinel string that must never appear in the bridge's response; exact-byte body assertion against `wantPlainErrorBody("unauthorized: token rejected by Scion Hub")`) |

Config-validation coverage: `TestValidateConfig_NewSchemes` gained
`hubBearer/valid`, `hubBearer/no-api-key-needed`, `hubBearer/with-ttl`,
`hubBearer/ttl-too-high` rows (mirroring the existing `hubUAT` rows).

## Mutation-resistance notes

Per the brief's quality bar (Phase 1's reviewers ran mutation checks):

- `TestAuthMiddleware_HubBearer_EmptyToken` uses a Hub mock that would
  **admit** an empty-token request if the bridge-side guard were deleted —
  so deleting `if token == "" { ... }` flips this test from pass to fail
  (200 instead of 401), not just from-pass-to-different-401.
- `TestAuthMiddleware_HubUATVsHubBearer_PrefixGuard` runs the identical
  credential and Hub fixture through both schemes in one test: reverting
  `hubBearer`'s case to reuse `hubUAT`'s prefix check fails the
  `hubBearer_accepts_same_token` subtest; accidentally dropping `hubUAT`'s
  own prefix check fails the `hubUAT_still_rejects` subtest.
- `TestCallerHubClient_BearerTokenType` / `TestHubBearer_EndToEnd_PassThrough`
  fail with `unknown token type: bearer` (surfaced as a JSON-RPC/500 error)
  if `bridge.go`'s `case "uat", "bearer":` is ever reverted to `case "uat":`
  alone — the same regression shape `TestCallerHubClient_GEExchangeTokenType`
  already guards for `"ge_exchange"`.
- `TestUATValidator_TokenTypeClassification` fails if the `scion_pat_*` /
  else split in `uatvalidator.go` is ever collapsed back to always `"uat"`.
- All exact-byte body assertions (`wantPlainErrorBody`, mirroring
  `pkg/hub/auth_external_bearer_test.go`'s `wantErrorBody` pattern, adapted
  to the bridge's plain-text `http.Error` responses rather than the Hub's
  JSON `ErrorResponse` shape) fail on a wording change or on an underlying
  error reason leaking into the response.

## Source material (`GoogleCloudPlatform/scion#1847`, closed upstream)

Used the durable copies (`/scion-volumes/scratchpad/projects/auth-passthrough/ref/1847.diff`,
branch `ref/upstream-pr-1847` head `4df5374`, read-only — never committed to
it). Re-applied by hand: `uatvalidator.go`'s `TokenType` split and
`bridge.go`'s `callerHubClient` `case "uat", "bearer":`. Both commits carrying
these hunks have the `Co-authored-by: Bobby Matthews <bobbymatthews@google.com>`
trailer. Did not port: the `hubUAT`-widening approach itself (implemented as
the new `hubBearer` scheme instead, per design §4.6), `Channel =
"a2a-bridge"`, the `securitySchemes`/agent-card block, or any v0.3
JSON-RPC/agent-card changes (all out of scope).

## Deviations / design questions

None requiring `ap-em`'s input — the one gap I found (`adminoverlay.go`'s
`BuildAuthValidators`, see above) was a straightforward correctness fix
within my owned files, not a design ambiguity, so I made the fix and I'm
recording it here rather than blocking on it.

## Verification

- `cd extras/scion-a2a-bridge && go build -buildvcs=false ./...` — clean.
- `gofmt -l .` (from `extras/scion-a2a-bridge/`) — clean (also fixed a
  pre-existing misalignment in `caller.go`'s struct tags that `gofmt` wanted
  reformatted once the `TokenType` field's comment grew to multiple lines).
- `go vet ./...` (module-scoped) — clean.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1
  ./...` (module-scoped; also re-run scoped to `./internal/bridge/...
  ./integration/...`) — 0 issues. (`upstream-main` fetched via `git fetch
  https://github.com/GoogleCloudPlatform/scion.git main:upstream-main`.)
- `go test ./...` (whole `extras/scion-a2a-bridge` module, includes
  `internal/bridge`, `integration`, `internal/state`) — all green, no
  regressions in any existing test (`TestGEEnvelopeCompatibility`,
  `TestColdReplicaAndRotation`, `TestControlPlanePrincipalIsolation`,
  `TestCombinedStartupMatrix`, `TestCredentialRedaction`, and the full
  `internal/bridge` suite all still pass unchanged).
- Bare-issue-number checks (both must print nothing; run with `/usr/bin/grep`
  directly, not the interactive shell's `grep`, per Phase 1's r3 finding
  about this environment's `grep` being a `ugrep` wrapper):
  `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'`
  and `git diff upstream-main..HEAD -- extras/scion-a2a-bridge |
  /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — both clean for my
  commits.

I did not run the full `pkg/hub` suite (`-timeout 40m`) — I did not touch
anything under `pkg/`, so it is out of my change's blast radius; the whole-
module `go build`/`go vet`/`golangci-lint` runs above are repo-scoped where
that matters (confirming I haven't broken cross-module compilation), and the
targeted `extras/scion-a2a-bridge` test/lint runs are exhaustive for the
files I actually touched.

---

## Fix round 1 (review `reviews/p5b-r1-ap-p5b-rev.md`, REQUEST CHANGES on `56674ec32`)

**Agent:** ap-p5b-dev-2 · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-24

All fixes are inside `extras/scion-a2a-bridge/**`, per the shared-branch
ownership split. Note the branch had gained Phase 2 commits
(`645168819`, `088772c0d`, `882e13d49`, Hub-side `pkg/**` work by `ap-p2-dev`)
since `56674ec32`; none of those touch `extras/scion-a2a-bridge/**`, so they
did not interact with this fix round.

### Disposition table

| # | Disposition | Change | Test |
|---|---|---|---|
| **R1** Production snapshot path (`BuildAuthValidators`) untested; mutant M8 survived | **Fixed.** Added the missing `hubBearer` row and a middleware test through the *actual* production wiring (`SetSnapshot`), not just the constructor-time fallback every other `hubBearer` test exercises. | `internal/bridge/adminoverlay_test.go`: `TestBuildAuthValidators_Schemes` gained `{"hubBearer", true, false}`. `internal/bridge/auth_test.go`: new `TestAuthMiddleware_HubBearer_ProductionSnapshotWiring` — builds `srv` via `NewServer`, then calls `srv.SetSnapshot(NewSnapshotHolder(BuildSnapshot(*cfg)))` exactly as `cmd/scion-a2a-bridge/main.go` does, with `Auth.Scheme: "hubBearer"` and a Hub-accepted non-`scion_pat_` token; asserts 200 and `token_type=bearer`. |
| **R2** README told operators `hubBearer` is in the admin-UI dropdown; it isn't and the overlay rejects it | **Fixed.** Restored the **Auth scheme** row's pre-change scheme list (`apiKey`, `bearer`, `none`, `hubUAT`, `hubJWT` — no `hubBearer`). Added a paragraph after the admin-UI settings table documenting `hubBearer` (and `geGoogle`, same precedent) as YAML-only, configured via `auth.scheme` in `scion-a2a-bridge.yaml`, not the dropdown. | Documentation-only; verified by inspection against `adminoverlay.go`'s `validAuthSchemes` (unchanged, still excludes `hubBearer`) and the admin-UI dropdown source the reviewer cited. No test applicable. |
| **N1** Duplicate test cases `hubBearer/valid` and `hubBearer/no-api-key-needed` (`auth_test.go:534-543`) | **Fixed.** Deleted `hubBearer/no-api-key-needed` (identical fixture/assertion to `hubBearer/valid`; `hubUAT`'s own table doesn't carry a parallel duplicate either). | `TestValidateConfig_NewSchemes` still covers `hubBearer/valid`, `hubBearer/with-ttl`, `hubBearer/ttl-too-high`. |
| **N2** `TokenType` doc comment (`caller.go:30-32`) omitted `ge_exchange` | **Fixed.** Added `"ge_exchange"` to the comment, matching `ge_exchange_validator.go:345`'s `TokenType: "ge_exchange"`. | Documentation-only; no behavior change. |
| **O1** Sample config said Google **ID token only** for `hubBearer` | **Fixed.** `scion-a2a-bridge.yaml.sample`'s `hubBearer` block now says "Google ID token or access token" instead of naming only `<google-id-token>`. | Documentation-only. |
| **F1-F3** | No action (per EM disposition). | — |
| **F4** Admin UI pushes `auth_scheme` from a dropdown lacking `hubBearer`/`geGoogle`, overwriting a YAML-configured scheme with the default | **This is a fail-open security bug, not just a UI inconsistency.** `admin-integrations.ts` seeds `editedSettings` with each field's `defaultValue`; for `auth_scheme` that default is `"none"`. `handleSaveConfig` sends the full `editedSettings` object, so saving *any* A2A setting (rate limit, external URL, projects — not just auth scheme) pushes `auth_scheme: "none"` unless an admin value was already stored. `ParseAdminOverlay`/`ApplyOverlay` then overwrite the YAML-configured `hubBearer` or `geGoogle` scheme with `none`, i.e. **the bridge starts accepting unauthenticated requests** (with the bridge's admin identity downstream, per the `none` scheme's behavior). Because `ParseAdminOverlay` rejects `hubBearer`/`geGoogle` as invalid `auth_scheme` values, the admin **cannot restore the original scheme from the UI** once this happens. Recovery is **not** editing the bridge YAML or clearing `admin-overlay.json` alone, and it differs by deployment mode (r4 correction — see fix round 4 below for the HA half, which r3's version of this row got wrong):
- **Self-managed (non-HA):** the Hub re-sends its stored `auth_scheme` on every Reconnect (`pkg/hub/handlers_integrations.go`'s `handleRestartIntegration` → `resolveIntegrationMergedConfig`, re-reading the Hub-side config file from disk → `pkg/plugin/manager.go`'s `RestartBrokerPlugin` → `loadPlugin` → `BrokerRPCClient.Configure`, applied and re-persisted by the bridge's own `BrokerServer.Configure`, `extras/scion-a2a-bridge/internal/bridge/broker.go:89`), so a YAML/overlay-only fix is undone on the next reconnect. Recovery requires removing the `auth_scheme` line from the Hub-side admin config (the integration's `config_file`; `~/.scion/scion-a2a-bridge-admin.yaml` for admin-UI installs, written by `pkg/hub/handlers_integrations.go`'s `createSelfManagedAdminConfig`), then clicking **Reconnect**.
- **HA:** the downgrade goes through a separate path, `applyRuntimeConfig` (`extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go:798`), which only *sets* `cfg.Auth.Scheme` when the runtime config's `auth_scheme` key is non-empty (`:802-804`) — it cannot *unset* a previously-applied `"none"` by the key simply being absent, so a running replica stays on `none` even after the Hub-side row is deleted. The admin **Restart** button in HA mode (labelled "Reconnect" only for self-managed/`external` plugins, per `web/src/components/pages/admin-integrations.ts:1947`) does not restart the bridge process either — for gRPC/HA adapters `RestartBrokerPlugin` just calls `adapter.Configure(cfg)` (`pkg/plugin/manager.go:515-525`), it does not kill and relaunch. Recovery requires deleting the `auth_scheme` key from the integration's settings in the Hub database, ensuring `A2A_AUTH_SCHEME` is unset on the bridge, and **restarting every bridge replica** (a fresh process reads the corrected config from scratch).

No code change made here (pre-existing, outside this delta's blast radius, admin-overlay/UI ownership; review r2 asked that `ap-em` file this as a tracked security follow-up rather than leave it as a log note only — see r2 disposition below). Candidate fixes: (a) the admin-integrations UI omits `auth_scheme` from its `Configure()`/runtime-config push when the currently-effective scheme isn't one it can represent (i.e. leave YAML-only schemes alone), or (b) the Hub/bridge overlay layer refuses to downgrade a YAML-only scheme. **(b) must cover both code paths**, not just `ApplyOverlay` (non-HA): `applyRuntimeConfig` (HA, cited above) is a separate function that never calls `ApplyOverlay` and currently has no way to revert a removed key at all. This is not this developer's or this PR's scope — it touches the admin-overlay/UI ownership area, not `extras/scion-a2a-bridge/**`'s bridge-side auth code. | — |

### Mutation M8 re-check

Re-ran the review's exact mutation by hand in the working tree (not a
throwaway worktree — verified `git diff` was clean before and after):
`internal/bridge/adminoverlay.go:330` `case "hubUAT", "hubBearer":` →
`case "hubUAT":`.

- `TestBuildAuthValidators_Schemes` → **FAIL**: `scheme "hubBearer": UATValidator present = false, want true`.
- `TestAuthMiddleware_HubBearer_ProductionSnapshotWiring` → **FAIL**: `status = 500, want 200 (through production snapshot wiring); body=internal server error` — the exact production symptom the review described (a real `hubBearer` deployment 500s on every request without this case).

Reverted the mutation (`git diff --stat internal/bridge/adminoverlay.go`
empty afterward) and re-ran both tests: **PASS**. M8 is now killed.

### Gates (from `extras/scion-a2a-bridge/`, `GOTMPDIR` set to an owned scratch
dir, deleted after; `GOCACHE=/scion-volumes/gocache`)

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `gofmt -l .` — clean (empty output).
- `go test ./...` — all green: `integration`, `internal/bridge` (37.8s,
  includes both new tests plus the full pre-existing suite), `internal/state`.
  No regressions.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./...`
  — first pass found 1 new issue (`errcheck` on the new test's
  `defer store.Close()`); fixed by wrapping it as
  `defer func() { _ = store.Close() }()`, matching the existing pattern at
  `newHubBearerMiddleware` (`auth_test.go:596`). Second pass: **0 issues**.
- Did not run the full `pkg/hub` suite (per the brief's explicit instruction
  not to, and because this fix round touches nothing under `pkg/`).

### Bare-issue-number grep (both must print nothing)

- `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty (this round added no new commits carrying a message at the time of this log entry; re-verified after committing, see report to `ap-em`).
- `git diff upstream-main..HEAD -- extras/scion-a2a-bridge | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty.

### Deviations / design questions

None. All disposed items were either straightforward test/doc fixes within
owned files or explicitly "no action" per the EM's disposition table.

---

## Fix round 2 (review `reviews/p5b-r2-ap-p5b-rev-2.md`, REQUEST CHANGES on `2b713460d`)

**Agent:** ap-p5b-dev-2 · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-24

Relayed via `auth-passthrough-lead` on `ap-em`'s behalf (`ap-em`'s direct
messages weren't arriving). Scope per that relay: do R1 (README caution +
F4 log wording) now, implement O1 (cheap), hold any *code* fix for F4 itself
— `ptone` is deciding whether that lands in this PR, and `ap-em` will route
it as a tracked security follow-up.

### Disposition table

| # | Disposition | Change | Test |
|---|---|---|---|
| **R1** (Required) README's r1-added sentence claimed a YAML `hubBearer` bridge "keeps working normally" against the admin UI. False: a routine admin-UI save pushes `auth_scheme: "none"` (fail-open, unauthenticated) and the UI cannot restore `hubBearer`/`geGoogle` afterward. | **Fixed.** Replaced the sentence at `README.md:119` with the reviewer's suggested caution: saving any A2A setting via the admin UI pushes the dropdown's default `auth_scheme` (`none`), overriding YAML; the UI can't switch it back. (r3 correction: this row's own recovery text was itself wrong — see fix round 3 below — editing YAML/clearing `admin-overlay.json` does **not** recover, because the Hub re-pushes its stored `auth_scheme` on reconnect; the fix is removing `auth_scheme` from the Hub-side admin config, then Reconnect.) | Documentation-only; the failure mode was independently confirmed by the reviewer's probe test (not re-verified by me — it's the same pre-existing `adminoverlay.go`/`admin-integrations.ts` behavior r1's F4 already flagged, just under-described). |
| **R1** (Required, same finding) — F4 project-log row understated the severity | **Fixed.** Rewrote the F4 row: states explicitly that the overwrite value is `"none"` (auth disabled, fail-open), that it can be triggered by saving *any* A2A setting (not just the auth scheme field), that the UI cannot restore `hubBearer`/`geGoogle` afterward because `ParseAdminOverlay` rejects them as invalid, and that recovery requires editing YAML/restarting or clearing the overlay file. Kept candidate fixes (a)/(b) and the ownership boundary. Did **not** make the code change — `ptone`/`ap-em` are deciding whether it lands in this PR. (r3 correction, same as the row above: this recovery description was also wrong for the same reason — editing YAML/clearing the overlay file does **not** recover, because the Hub re-pushes its stored `auth_scheme` on reconnect; see fix round 3 below for the corrected text, and fix round 4 below for the further HA-mode correction.) | — |
| **O1** (Optional, implemented per relay instruction: "it's cheap") No test asserted the bearer token is absent from the rejection log (M9 survived) | **Implemented.** `TestAuthMiddleware_HubBearer_RejectedByHub` now builds the server directly (not via `newHubBearerMiddleware`) with a `slog.NewTextHandler` writing to a `bytes.Buffer`, and after asserting the existing exact-body check, asserts the captured log contains a rejection line and does **not** contain the raw token. | `TestAuthMiddleware_HubBearer_RejectedByHub` (extended) |

### O1 mutation re-check (M9)

Applied the review's mutation by hand: `server.go:575`
`s.log.Info("bearer token rejected by Scion Hub", "error", err)` →
`s.log.Info("bearer token rejected by Scion Hub", "error", err, "token", token)`.

- `TestAuthMiddleware_HubBearer_RejectedByHub` → **FAIL**: `rejection log leaked the bearer token: "...token=some-token-the-hub-rejects\n"`.

Reverted (`git diff --stat internal/bridge/server.go` empty afterward), test
passes again. **M9 is now killed.**

### F4 — not fixed here, by design

Per the relay's explicit instruction, I did not touch `adminoverlay.go`,
`ApplyOverlay`, or anything under `web/`. `ptone` is deciding whether a code
fix for F4 lands in this PR; `ap-em` will route that decision (and file the
tracked security follow-up issue the review recommended). This developer's
scope for this round was doc wording (R1) and the log-redaction test (O1)
only.

### Gates (from `extras/scion-a2a-bridge/`, `GOTMPDIR` owned scratch dir,
deleted after; `GOCACHE=/scion-volumes/gocache`)

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `gofmt -l .` — clean.
- `go test ./...` — all green: `integration` (12.2s), `internal/bridge`
  (37.3s, includes the extended `RejectedByHub` test plus the full existing
  suite), `internal/state`. No regressions.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./...`
  — 0 issues.
- Did not run the `pkg/hub` suite (unchanged instruction; nothing under
  `pkg/` touched this round).

### Bare-issue-number grep (both must print nothing)

- `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
- `git diff upstream-main..HEAD -- extras/scion-a2a-bridge | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty.

### Deviations / design questions

None beyond the explicit scope limit above (F4's code fix intentionally held
for `ptone`/`ap-em`).

---

## Fix round 3 (review `reviews/p5b-r3-ap-p5b-rev-3.md`, REQUEST CHANGES on `86fba2eb5`)

**Agent:** ap-p5b-dev-2 · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-24

Per `ap-em`'s fix brief (`briefs/ap-p5b-dev-2-fix-r3.md`): one Required,
doc-only finding. r2-O1 (log redaction) was independently re-verified by
this review (M9/M9b/M9c all killed) — no action needed there. All B-row
mutants (20/20) remain killed. `ap-em` rebased the branch onto upstream
`main` and force-pushed between rounds 2 and 3; I ran the required
`git fetch origin && git reset --hard origin/scion/auth-passthrough` (clean
working tree, nothing unpushed) before starting this round, per `ap-em`'s
instruction — new head at the time was `86fba2eb5`.

### Disposition table

| # | Disposition | Change | Verified against source |
|---|---|---|---|
| **R1** (Required) The r2 README caution's recovery step ("clear the persisted admin overlay and restart") does not actually recover a `hubBearer`/`geGoogle` bridge whose `auth_scheme` was overwritten to `none`: the Hub keeps `auth_scheme: "none"` in its own admin config and re-pushes it on every reconnect, undoing a YAML/overlay-only fix. The same wrong recovery text appeared in this log's F4 row and r2 disposition row. | **Fixed (doc-only).** Replaced the recovery clause in `README.md:121` with correct guidance: remove the `auth_scheme` line from the **Hub-side** admin config (`~/.scion/scion-a2a-bridge-admin.yaml`; in HA mode, the integration's settings in the Hub database), then click **Reconnect** — clearing the bridge's own `admin-overlay.json` or editing its YAML is not enough. Corrected the matching text in this file's F4 row and the r2-R1 disposition row above. | Read (not copied blindly, per the brief's instruction) before writing: `pkg/hub/handlers_integrations.go` — `handleUpdateIntegrationConfig` (func at `:446`, HA/YAML branch starts `:516`) writes the Hub-side settings; `resolveIntegrationMergedConfig` (`:1573`) and `mgr.ReplaceBrokerConfig` (`pkg/plugin/manager.go:549`) push the merged map to the plugin; `createSelfManagedAdminConfig` (`:1068` area, called at `:983`) is what seeds `auth_scheme: "none"` into `~/.scion/scion-a2a-bridge-admin.yaml` (path built at `:970` as `"~/.scion/scion-" + name + "-admin.yaml"`). Reconnect path: `Manager.Reconnect` (`pkg/plugin/manager.go:416`) → `loadPlugin` (`:224`) → `BrokerRPCClient.Configure` (`pkg/plugin/broker_plugin.go:279`); on the bridge side, `BrokerServer.Configure` (**`extras/scion-a2a-bridge/internal/bridge/broker.go:89`** — inside my own owned package, not a `pkg/plugin` file as the review's shorthand `broker.go:100-119` could be misread) applies the overlay and re-persists it via `PersistOverlay`. Boot-time overlay load confirmed at `extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go:113-125` (`LoadPersistedOverlay`/`ApplyOverlay`), matching the review's claim that a bridge restart briefly restores `hubBearer` before the next reconnect re-pushes `none`. |
| F1-F4 | No action. | — | — |

### Gates (from `extras/scion-a2a-bridge/`, `GOTMPDIR` owned scratch dir,
deleted after; `GOCACHE=/scion-volumes/gocache`)

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `gofmt -l .` — clean.
- `go test ./...` — all green: `integration` (11.6s), `internal/bridge`
  (37.3s), `internal/state`. No regressions (doc-only change, as expected).
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./...`
  — 0 issues.
- Did not run the `pkg/hub` suite (unchanged instruction; this round is
  doc-only and touches nothing under `pkg/`).

### Bare-issue-number grep (both must print nothing)

- `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
- `git diff upstream-main..HEAD -- extras/scion-a2a-bridge | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty.

### Deviations / design questions

None. As instructed, no code changes to `adminoverlay.go`/UI — F4's code fix
remains with `ptone`/`ap-em` to route.

---

## Fix round 4 (review `reviews/p5b-r4-ap-p5b-rev-4.md`, REQUEST CHANGES on `7f18a3728`)

**Agent:** ap-p5b-dev-2 · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-24

Per `ap-em`'s fix brief (`briefs/ap-p5b-dev-2-fix-r4.md`): doc-only round, code
unchanged and clean (22/22 mutants killed, independently re-verified by this
review). The reviewer said it would approve once R1 lands.

### Disposition table

| # | Disposition | Change | Verified against source before writing |
|---|---|---|---|
| **R1** (Required) The r3 HA recovery clause was wrong: "delete the key from the Hub DB, then click Reconnect" doesn't recover an HA bridge. Running replicas stay on `none` until every process restarts. | **Fixed.** Split `README.md:121`'s recovery paragraph by mode: non-HA keeps the Reconnect flow (corrected in r3); HA now says to delete the `auth_scheme` key from the Hub database, ensure `A2A_AUTH_SCHEME` is unset on the bridge, and restart **every bridge replica** — explaining that a running replica can't unset an applied scheme via reload and that the HA "Restart" button doesn't restart the process. Mirrored in the F4 row and the r3-R1 disposition row (`:309`, N1). | `extras/scion-a2a-bridge/cmd/scion-a2a-bridge/main.go`: `applyRuntimeConfig` (`:798`) only sets `cfg.Auth.Scheme` when `rtCfg["auth_scheme"] != ""` (`:802-804`) — an absent key is a no-op, confirming a removed DB row cannot revert an already-applied `none`. The reconfigure callback (`:511-518`) calls `applyRuntimeConfig(cfg, newCfg)` on the same mutated `*cfg` and rebuilds the snapshot from it, so the downgrade persists across runtime reconfigures. `web/src/components/pages/admin-integrations.ts:1947`: `restartLabel = mode === 'external' ? 'Reconnect' : 'Restart'` — confirms HA shows "Restart", not "Reconnect". `pkg/plugin/manager.go:515-525` (`RestartBrokerPlugin`'s doc comment and body): "For gRPC/HA adapters, it pushes the config directly" — `if isGRPC { return adapter.Configure(cfg) }`, no process kill/relaunch, confirming the HA Restart button doesn't restart the bridge process. |
| **O1** (Optional, folded in) Non-HA recovery also needs `external_url` set/cleared, or the first Reconnect fails `ValidateConfig`. | **Fixed.** Added the same-file `external_url` step to the non-HA recovery sentence. | `pkg/hub/handlers_integrations.go:1082`: the admin-UI-install seed file writes `external_url: ""`. `extras/scion-a2a-bridge/internal/bridge/server.go:139-140`: `ValidateConfig` rejects an empty `Bridge.ExternalURL` with `"bridge.external_url is required"`. |
| **O2** (Optional, folded in) F4's candidate fix (b) named only `ApplyOverlay`, missing the HA `applyRuntimeConfig` path. | **Fixed.** Added a clause to the F4 row: candidate (b) must cover both `ApplyOverlay` (non-HA) and `applyRuntimeConfig` (HA), and the latter currently has no way to revert a removed key at all. | Same `applyRuntimeConfig` citation as R1 above; confirmed it never calls `ApplyOverlay` (`grep` inside the function body — only direct field assignments on `*bridge.Config`). |
| **N1** (Nit) The second r3-R1-adjacent row (`:309` at the time) still had the pre-r3 recovery wording, uncorrected. | **Fixed.** Added the same "(r3 correction …)" parenthetical there, cross-referencing both the r3 and r4 corrections. | — |
| **N2** (Nit) README hard-coded `~/.scion/scion-a2a-bridge-admin.yaml` as *the* Hub-side path; it's only the admin-UI-install default. | **Fixed.** Non-HA recovery now says "the integration's `config_file`; `~/.scion/scion-a2a-bridge-admin.yaml` for admin-UI installs". | `pkg/hub/handlers_integrations.go:541-554`: a `settings.yaml`-registered (non-admin-UI-installed) integration resolves its own `config_file` from `mgr.GetPluginConfigFile`/`GetPluginConfig` or `settingsEntry.ConfigFile`, not the hardcoded admin-install path. |
| F1-F3 | No action. | — |

### Gates

Doc-only round (no Go files touched — confirmed via `git status` before
committing). Per the brief, `gofmt`/`go test` are not required when no Go
files change; ran the bare-`#NNN` greps only, from `/workspace`:

- `git log upstream-main..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
- `git diff upstream-main..HEAD -- extras/scion-a2a-bridge | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty.

### Deviations / design questions

None. No code changes to `adminoverlay.go`/`main.go`/UI this round either —
F4/O2's code fix remains with `ptone`/`ap-em` to route.

---

## F4 (§4.6b): pinned YAML auth scheme

**Agent:** ap-p5b-dev-2 · **Branch:** `scion/auth-passthrough` · **Date:** 2026-09-24

Implements `impl-design.md` §4.6b: option (a) for the F4 fail-open bug
(admin-pushed config could replace a YAML-only `auth.scheme` — `hubBearer`
or `geGoogle` — with the admin dropdown's default, and the UI had no way to
push it back). This closes the gap at the bridge, the enforcement point,
rather than in the admin overlay's validation or the Hub-side storage —
per the design, both of those are left untouched.

### Design decisions

- **Capture storage: `Config.Auth.YAMLScheme` (field on the struct), not a
  parameter threaded through every caller.** The design offered either
  choice. A field is simpler here because `Config` is already copied by
  value at every merge point (`ApplyOverlay` takes `base Config` by value;
  `applyRuntimeConfig` takes `*Config` but never replaces the struct), so
  the captured value survives every copy automatically with no extra
  plumbing. `yaml:"-"` keeps it non-configurable — it always mirrors
  `Scheme` at the moment `loadConfig` parses the YAML file, before any
  overlay or runtime config touches `cfg`.
- **`ApplyOverlay` gained a `log *slog.Logger` parameter.** `EffectiveAuthScheme`
  needs somewhere to log the warning, and using the bridge's own configured
  logger (not a package-level default) keeps the warning attributable and
  testable. All three production call sites already had a logger in scope
  (`BrokerServer.b.log`, `main()`'s `log`), so this was a mechanical change;
  the five existing unit-test call sites were updated to pass a test logger.
- **`applyRuntimeConfig` also gained a `log *slog.Logger` parameter**, for
  the same reason, on the same basis (the caller in `serveStandalone` and
  the reconfigure callback both already have `log` in scope).

### Row → test → mutant table

| Row | Test(s) | Mutation → result |
|---|---|---|
| F4-1: YAML `hubBearer`, overlay `auth_scheme: none` → effective `hubBearer`, warning logged | `TestApplyOverlay_PinsYAMLOnlyScheme`, `TestEffectiveAuthScheme/pinned_scheme,_none_push_is_ignored_(F4-1_shape)` | Remove the `ApplyOverlay` call site (assign `overlay.AuthScheme` directly) → **failed** `TestApplyOverlay_PinsYAMLOnlyScheme`, `TestApplyOverlay_PinnedSchemeSurvivesBuildSnapshot`, `TestConfigure_PinsYAMLOnlyScheme` |
| F4-2: YAML `hubBearer`, `applyRuntimeConfig{"auth_scheme":"none"}` → `hubBearer` | `TestApplyRuntimeConfig_PinsYAMLOnlyScheme` | Remove the `applyRuntimeConfig` call site → **failed** `TestApplyRuntimeConfig_PinsYAMLOnlyScheme`, `…GeGoogleSchemeAlsoPinned`, `…PinSurvivesReconnect`, `…UsesCapturedYAMLScheme_NotCurrent` |
| F4-3: two successive `applyRuntimeConfig` calls (reconnect) → still `hubBearer` | `TestApplyRuntimeConfig_PinSurvivesReconnect` (second call passes an empty map — no `auth_scheme` key at all, the actual reconnect shape) | Covered by the same call-site-removal mutation above |
| F4-4: YAML `geGoogle`, same as F4-1/F4-2 → `geGoogle` | `TestApplyOverlay_GeGoogleSchemeAlsoPinned`, `TestApplyRuntimeConfig_GeGoogleSchemeAlsoPinned`, `TestEffectiveAuthScheme/pinned_geGoogle_scheme,…` | Same call-site-removal mutations; also killed by the "pin everything" mutation below (proves geGoogle needs the `validAuthSchemes` gate too, not just hubBearer) |
| F4-5: YAML `apiKey`, pushed `none` → `none` (guards over-pinning) | `TestApplyOverlay_DoesNotPinUIRepresentableScheme`, `TestApplyRuntimeConfig_DoesNotPinUIRepresentableScheme`, `TestEffectiveAuthScheme/UI-representable…` | Pin everything (drop the `!validAuthSchemes[yamlScheme]` check from `EffectiveAuthScheme`) → **failed** both `DoesNotPinUIRepresentableScheme` tests and the `TestEffectiveAuthScheme` subtest |
| F4-6: snapshot/`BuildAuthValidators` after a pinned push yields the `hubBearer` validator (production wiring) | `TestApplyOverlay_PinnedSchemeSurvivesBuildSnapshot`, `TestConfigure_PinsYAMLOnlyScheme` (full `BrokerServer.Configure` → snapshot route) | Either call-site-removal mutation above fails these; also covered by the capture-drop mutation below (a wrong pin decision reaches `BuildSnapshot` the same way) |
| Hub-driven Restart/`Configure` push through `ApplyOverlay` (p5b review r5, F2) | `TestConfigure_PinsYAMLOnlyScheme` — exercises `BrokerServer.Configure` end to end (this is the real route for both a non-HA admin-UI save and an HA Restart push that carries `auth_scheme`), and additionally re-applies the persisted overlay after the push to prove a "restart" can't unpin it either | Same mutations as F4-1/F4-6 |
| Startup with a persisted overlay containing `auth_scheme: none` → still pinned | `TestApplyOverlay_PersistedOverlayCannotUnpinYAMLScheme` (round-trips `PersistOverlay`/`LoadPersistedOverlay`/`ApplyOverlay`, the exact sequence `main()` runs at boot) | Same call-site-removal mutation |
| Capture correctness: must pin against the captured YAML value, not the live "current" scheme | `TestApplyOverlay_UsesCapturedYAMLScheme_NotCurrent`, `TestApplyRuntimeConfig_UsesCapturedYAMLScheme_NotCurrent` (both construct a `Config` where `Auth.Scheme` deliberately differs from `Auth.YAMLScheme`) | Drop the capture (pass `cfg.Auth.Scheme`/`base.Auth.Scheme` instead of `…YAMLScheme` as the first `EffectiveAuthScheme` argument) → **failed** both tests |
| Empty pushed scheme keeps current, not cleared (behavior change from before this fix) | `TestEffectiveAuthScheme/pinned_scheme,_empty_push_keeps_current…`, `TestParseAdminOverlay_EmptyStringFieldsStillPresent` (updated — see below) | — |

All four mutations the brief asked for were run by hand in the working
tree (not a throwaway worktree — verified `git diff --stat` was empty after
each revert): remove the `ApplyOverlay` call site, remove the
`applyRuntimeConfig` call site, drop the capture, and pin everything. Each
failed the tests listed above; all were reverted and the full suite re-run
green before continuing.

### Pre-existing test updated (behavior change, not a regression)

`TestParseAdminOverlay_EmptyStringFieldsStillPresent` asserted that pushing
`auth_scheme: ""` (present, but empty) cleared `Auth.Scheme` to `""`. §4.6b
intentionally changes this: `EffectiveAuthScheme` treats an empty pushed
scheme as "no scheme in this push" and keeps the current scheme, for every
YAML scheme (pinned or not) — an admin clearing the field is not itself a
downgrade request. Updated the assertion to expect the base value (`apiKey`)
unchanged, with a comment explaining why, and cross-referenced from
`TestEffectiveAuthScheme`.

### FYI (not fixed, out of scope for this task)

`BrokerServer.Configure` logs `"admin config applied", "auth_scheme",
overlay.AuthScheme, …` at the end of a successful push — this is the raw
pushed value, not the effective (possibly pinned) scheme, so a pinned push
logs `auth_scheme=none` even though the effective scheme stayed `hubBearer`.
This is pre-existing (the line logs the raw overlay for every field, not
just auth_scheme) and not a secret leak or incorrect claim about what was
*received*; flagging it here rather than changing it, since the brief scoped
this task to `EffectiveAuthScheme`/its two call sites and didn't ask for
changes to that log line.

### README / docs-site

Collapsed the multi-paragraph HA/non-HA recovery caution (added across
fix rounds 2-4) to one line describing the new, permanent behavior, plus one
line for bridges built before this protection existed (no version named),
per the brief. `docs-site/src/content/docs/hosted/user/a2a-bridge.md` does
not carry this caution (it only cross-references the Hub's external-bearer
docs for `hubBearer` and marks `geGoogle` deprecated) — no edit needed there.

### Gates (from `extras/scion-a2a-bridge/`, `GOTMPDIR` owned scratch dir,
deleted after; `GOCACHE=/scion-volumes/gocache`)

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `gofmt -l .` — clean.
- `go test ./...` (full module, includes the new `cmd/scion-a2a-bridge`
  package tests) — all green.
- `go build -buildvcs=false ./cmd/scion-a2a-bridge/...` — clean.
- `GOGC=40 golangci-lint run --new-from-rev=a53175c23 --concurrency=1 ./...`
  — 0 issues.
- Did not run the `pkg/hub` suite (out of scope; nothing under `pkg/`
  touched).

### Bare-issue-number grep (base `a53175c23`, both must print nothing)

- `git log a53175c23..HEAD --format=%B | /usr/bin/grep -nE '(^|[^/A-Za-z])#[0-9]+'` — empty.
- `git diff a53175c23 HEAD | /usr/bin/grep -nE '^\+.*(^|[^/A-Za-z0-9])#[0-9]{3,}'` — empty.

### Deviations / design questions

None requiring `ap-em`'s input. The `ApplyOverlay`/`applyRuntimeConfig`
logger-parameter addition was a mechanical consequence of the design's
pseudocode (which logs from inside the merge), not a design choice on my
part.
