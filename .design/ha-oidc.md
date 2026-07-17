# Design: Unified OIDC Transport Auth for IAP-Protected Hubs

**Project:** ha-run (HA Cloud Run workstream)
**Author:** har-arch (architect agent)
**Date:** 2026-07-17
**Status:** Approved — all 6 open questions resolved with ptone on thread 5668, 2026-07-17
**References:** ptone/scion#488, [broker OIDC gist](https://gist.github.com/chiefkarlin/403fb9cfb493c0d2ad3fdd1c418cc0d2), `docs-site/src/content/docs/hosted/ha/auth-proxy-iap.md`

---

## 1. Problem Statement

When the Hub runs behind a platform guard (Google IAP or Cloud Run invoker-only IAM), every
inbound request must carry a **transport-layer** Google OIDC ID token
(`Authorization: Bearer <oidc>`) in addition to Scion's **app-layer** credential
(`X-Scion-Agent-Token`, broker HMAC headers, or user bearer token).

Only one client stack in the codebase implements the transport layer:

| Client stack | Used by | Transport OIDC | Behind IAP |
|---|---|---|---|
| `pkg/sciontool/hub.Client` | sciontool hooks (heartbeat, status, token refresh, gcp-token) | ✓ `configureOIDCTransport()` | **works** |
| `pkg/hubclient.Client` (via `pkg/apiclient.Transport`) | `scion` CLI, hubsync, broker REST, broker CLI, chat-app, a2a-bridge | ✗ none | **broken** |
| bare `http.Client` | `sciontool doctor` (2 sites) | ✗ none | **broken** |
| `wsprotocol.Dial` (broker control channel) | broker ↔ hub WebSocket | ✗ HMAC headers only | **broken** |
| `pkg/wsclient` PTY dialer | `scion attach` | ✗ app token occupies `Authorization` | **broken (doubly)** |

### Problem 1 — hubclient missing OIDC transport (issue #488)

Inside a GKE-dispatched agent, `SCION_TRANSPORT_TOKEN` is present and valid (the hub injects
it at dispatch; `pkg/hub/httpdispatcher.go` sets `SCION_TRANSPORT_TOKEN`,
`SCION_TRANSPORT_AUDIENCE`, `SCION_TRANSPORT_TOKEN_EXPIRY`). The sciontool stack consumes it,
so heartbeats work and the agent shows online. But `scion ls/start/stop` route through
`hubsync.createHubClient()` (`pkg/hubsync/sync.go:1335`) → `hubclient.New()` →
`apiclient.Transport`, which never reads the env var — IAP returns its HTML login page and the
CLI fails with `invalid character '<' looking for beginning of value`. `sciontool doctor`
builds two bare `http.Client`s (`cmd/sciontool/commands/doctor.go:203,234`) with the same
failure mode.

**Root cause:** transport-layer auth was implemented privately inside `pkg/sciontool/hub`
(`oidc.go`) rather than as a shared capability of the HTTP layer both stacks sit on. Every
other hub-bound path (11 distinct `hubclient.New()` call sites, 2 bare clients, 2 WebSocket
dialers) predates or bypasses it.

### Problem 2 — brokers lack OIDC support

Brokers talk to the hub over two channels, both IAP-blind:

1. **REST** (`pkg/runtimebroker/hub_connection.go:231`, `server.go:524,568`,
   `cmd/broker.go:1989`) — `hubclient.New()` with HMAC (`X-Scion-Broker-ID` +
   `X-Scion-Signature` etc.) or dev auth. HMAC headers are custom `X-Scion-*` headers and pass
   *through* IAP untouched — but without an OIDC bearer the request never reaches the hub.
2. **WebSocket control channel** (`pkg/runtimebroker/controlchannel.go:243`) — HMAC-signed
   headers on the upgrade request, no OIDC bearer, dropped by IAP at handshake.

There is also a bootstrap gap: broker registration (`scion hub brokers register` / two-phase
`POST /api/v1/brokers` + `/brokers/join`) can't traverse IAP either, which is why the gist's
deployment currently uses a manual `install-broker.sh` curl-from-a-pod workaround.

**Root cause:** brokers were designed with app-layer auth only (HMAC), and unlike agents they
are *originators* — nothing injects a transport token into them, so they must mint their own
OIDC tokens from their runtime identity (GKE Workload Identity / ambient SA).

### Additional constraint (from the gist, confirmed against Google docs)

The Google-managed OAuth client that Cloud Run provisions when IAP is enabled **does not
support programmatic (service-account) authentication**. Deployments must create a custom
OAuth 2.0 Client ID and bind it to the service's IAP settings; that client ID is the OIDC
`audience` for all machine clients. This is a deployment/documentation requirement, not a code
change, but the design must treat the audience as explicit configuration — never guessed.

---

## 2. Goals / Non-Goals

**Goals**

- G1. Every hub-bound HTTP request in the repo attaches a transport OIDC token whenever
  transport auth is configured (env or config), across both client stacks and both WebSocket
  dialers.
- G2. One shared implementation — eliminate the private copy in `pkg/sciontool/hub` rather
  than adding a second one.
- G3. Brokers (GKE/GCE) can mint their own OIDC tokens from ambient identity (Workload
  Identity metadata server) with configured audience, for REST, heartbeat, and the control
  channel, including reconnects with fresh tokens.
- G4. Broker registration works natively through IAP (retire `install-broker.sh`).
- G5. Non-IAP deployments are byte-for-byte unaffected: all new behavior is off unless
  explicitly enabled by env/config.

**Non-goals (deferred, see §8 open questions)**

- Human-interactive IAP browser flow for the CLI (workstation OAuth web flow).
- Changing the app-layer auth model (agent JWTs, HMAC, PATs stay as-is).
- Hub-side inbound changes — proxy auth (`X-Goog-IAP-JWT-Assertion`) already works.

---

## 3. Proposed Architecture

### 3.1 New shared package: `pkg/transportauth`

Extract the proven implementation from `pkg/sciontool/hub/oidc.go` into a standalone package
with no heavy dependencies (only `cloud.google.com/go/compute/metadata`, already in the tree):

```go
package transportauth

// TokenSource yields transport-layer Google OIDC ID tokens. Thread-safe.
type TokenSource interface {
    Token() (string, error)
    // SetToken lets refresh paths (hub tokens[] array) push new tokens in.
    SetToken(token string, expiry time.Time)
    // Expiry returns the current token expiry (zero if unknown).
    Expiry() time.Time
}
```

**Token sources** (moved nearly verbatim from `oidc.go`):

| Source | From | Used by |
|---|---|---|
| `InjectedSource` | `SCION_TRANSPORT_TOKEN` env (hub-minted at dispatch, refreshed via `tokens[]`) | agents (sciontool, and now the in-container `scion` CLI) |
| `MetadataSource{Audience}` | GCE/GKE metadata server `.../identity?audience=...` with cache + 5-min refresh margin | brokers (Workload Identity), GCE-hosted CLI |
| `ADCSource{Audience}` *(Phase 5, separate subpackage `transportauth/adcsource`)* | `google.golang.org/api/idtoken` over Application Default Credentials — works with `gcloud auth application-default login --impersonate-service-account=…` | workstation CLI |

The ADC source lives in a subpackage so the lean `sciontool` binary never links the Google API
client libraries.

**Environment resolution** (mirrors today's `configureOIDCTransport()` semantics exactly):

```go
// FromEnv returns (nil, nil) when transport auth is not configured — callers
// then behave exactly as before this change.
func FromEnv() (TokenSource, error) {
    // 1. SCION_TRANSPORT_TOKEN set          → InjectedSource
    // 2. on GCE && SCION_METADATA_MODE unset && audience configured
    //    (SCION_TRANSPORT_AUDIENCE or SCION_HUB_OIDC_AUDIENCE)
    //                                        → MetadataSource
    // 3. otherwise                           → nil (no transport auth)
}
```

Note one deliberate difference from the sciontool-internal behavior: in metadata mode the
generic `FromEnv()` requires an **explicit audience env var** and does not default the
audience to the hub URL. `pkg/sciontool/hub` keeps its hub-URL default (it knows its hub URL;
generic callers don't reliably), preserving the PR #307 behavior for agents. The
`SCION_METADATA_MODE` guard is kept: when the scion metadata server has hijacked
169.254.169.254, the real metadata server is unreachable and we must not attempt it.

**HTTP wiring:**

```go
// Wrap returns rt wrapped so each request carries the transport token.
func Wrap(rt http.RoundTripper, src TokenSource, mode HeaderMode) http.RoundTripper

// ApplyHeaders sets the transport token on h — for WebSocket dialers,
// which bypass RoundTrippers.
func ApplyHeaders(h http.Header, src TokenSource, mode HeaderMode) error
```

### 3.2 Header-placement policy (the `Authorization` collision)

IAP consumes `Authorization: Bearer <oidc>`. Scion's app-layer credentials use three shapes:

| App-layer credential | Header | Collision? |
|---|---|---|
| Agent JWT | `X-Scion-Agent-Token` | none — OIDC takes `Authorization` |
| Broker HMAC | `X-Scion-Broker-ID` / `X-Scion-Signature` / … | none |
| User OAuth / PAT / dev token | `Authorization: Bearer` | **yes** |

`HeaderMode` handles the collision:

- `HeaderAuthorization` (default): set `Authorization` only if empty — identical to the
  current sciontool RoundTripper, fully backward-compatible.
- `HeaderProxyAuthorization`: when the app layer occupies `Authorization`, send the OIDC token
  as `Proxy-Authorization: Bearer <oidc>`. Google IAP explicitly supports this for exactly
  this case and strips the header before forwarding.
- `HeaderServerlessAuthorization`: `X-Serverless-Authorization: Bearer <oidc>` — Cloud Run's
  documented equivalent when the guard is invoker IAM rather than IAP.

Mode selection: explicit `SCION_TRANSPORT_MODE` env (`iap` | `cloudrun_invoker`) /
config field when present; the hub dispatcher will be extended to inject
`SCION_TRANSPORT_MODE` alongside the existing three vars (it already knows
`cfg.Auth.Transport.Mode`). Absent any signal, default to `HeaderAuthorization`, which
covers all agent/broker paths (no collision) — the collision only exists for user-credential
CLI flows, which arrive in Phase 5.

⚠️ *Risk to verify on a live IAP deployment:* Google's documented lookup order when both
headers are present ("IAP checks `Authorization` first, then `Proxy-Authorization`") — we must
confirm IAP falls through to `Proxy-Authorization` when `Authorization` carries a non-Google
(scion) bearer token, rather than 401-ing. If it 401s, the fallback design for user-credential
flows is to move the *scion* token out of `Authorization` (the hub already accepts
`X-Scion-Agent-Token`; a sibling `X-Scion-User-Token` would be added). This only affects
Phase 5 scope.

### 3.3 Wiring into the `hubclient` stack (fixes #488)

Add one option and one auto-detection hook:

```go
// pkg/hubclient
func WithTransportAuth(src transportauth.TokenSource, mode transportauth.HeaderMode) Option
```

`hubclient.New()` calls `transportauth.FromEnv()` **by default** when no explicit
`WithTransportAuth` option was supplied, wrapping `transport.HTTPClient.Transport`. Because
`FromEnv()` returns nil unless `SCION_TRANSPORT_TOKEN` (or an explicit audience on GCE) is
set, all 11 existing call sites gain IAP support with zero call-site changes and zero behavior
change elsewhere. An explicit `WithTransportAuth(nil, …)` opts out.

Consequently fixed with no further code:

- `hubsync.createHubClient()` (`pkg/hubsync/sync.go:1335`) — `scion ls/start/stop` inside
  agents (the #488 headline symptom; the injected token is already in the environment).
- `cmd/hub.go getHubClient()`, `cmd/completion_helper.go`, `cmd/hub_auth.go`.
- Broker REST clients (they additionally get config-driven sources — §3.4).

`sciontool doctor` (`cmd/sciontool/commands/doctor.go:203,234`) is changed to build its
`http.Client`s via `transportauth.FromEnv()` + `Wrap()`. Doctor also gains an explicit
diagnostic line reporting which transport source is active (`injected` / `metadata` / `none`)
and the token's expiry — turning today's silent failure into a first-class check.

`pkg/sciontool/hub` is refactored to consume `pkg/transportauth` (delete
`oidc.go`'s private copies; keep `configureOIDCTransport()` as a thin adapter preserving the
hub-URL audience default and the `tokens[]` refresh integration via `SetToken`).

### 3.4 Broker transport auth

**Configuration.** Transport settings are *per hub connection* (brokers can serve multiple
hubs via the multistore). Two layers, env wins over file:

1. `BrokerCredentials` (`pkg/brokercredentials`) gains optional fields, persisted by
   `scion broker join` / `scion hub brokers register`:

   ```json
   {
     "brokerId": "…", "secretKey": "…", "hubEndpoint": "https://hub…",
     "transportMode": "iap",
     "transportAudience": "1234567890-abc.apps.googleusercontent.com"
   }
   ```

2. Env overrides for containerized brokers: `SCION_TRANSPORT_MODE`,
   `SCION_TRANSPORT_AUDIENCE` (same names the hub injects for agents — one vocabulary).

**Token acquisition.** Brokers are long-lived originators, so injected static tokens are
unsuitable; the source is `MetadataSource` (GKE Workload Identity / GCE ambient SA) with the
configured audience, cached with the existing 5-minute refresh margin. Off-GCP brokers behind
IAP are out of scope for now (would require ADC or a token-file source — see open question Q5).

**Wire-up points:**

- `buildHubClientOpts()` (`pkg/runtimebroker/hub_connection.go:252`) and the two
  `createHubConnectionFromConfig()` sites (`server.go:524,568`): append
  `hubclient.WithTransportAuth(...)` when the connection's transport config resolves. Covers
  REST + heartbeats + template/harness-config hydration.
- **Control channel** (`controlchannel.go`): `buildAuthHeaders()` additionally calls
  `transportauth.ApplyHeaders()`. The token is fetched *per dial attempt*, so every reconnect
  presents a fresh token (IAP validates at handshake; established WebSockets are not
  re-checked, so a mid-connection token expiry does not drop the channel — reconnect handles
  it).
- `cmd/broker.go getBrokerHubClient()` (broker CLI introspection): same option from the same
  per-connection config.
- HMAC remains the app-layer identity; it composes cleanly since it never touches
  `Authorization`.

**Registration bootstrap (retires `install-broker.sh`).** `scion hub brokers register` /
`broker join` run wherever the operator runs them. With Phase 2's `hubclient` auto-detection,
running them from a pod/VM with Workload Identity plus
`SCION_TRANSPORT_MODE=iap SCION_TRANSPORT_AUDIENCE=<client-id>` traverses IAP natively; the
command then persists `transportMode`/`transportAudience` into the credentials file so the
broker daemon inherits them. Additionally, per the gist's finding, the register/join commands
must not hard-fail when no PAT/hub-token is configured *and* transport auth is active with the
hub in proxy-auth mode (the two-phase join issues the HMAC secret; identity comes from the
IAP assertion of the SA). This is the one place `cmd/hub.go getAuthInfo()`-style enforcement
is relaxed: `MethodType == "none"` becomes acceptable **iff** a transport token source
resolved (a "proxy-auth" state, surfaced as such in `scion hub status`).

**Deployment (GKE), documented not coded:**

| Step | What |
|---|---|
| 1 | Create custom OAuth 2.0 Client ID; bind to the hub service's IAP settings (`gcloud iap settings set` / API). Google-managed client rejects SA tokens. |
| 2 | Create broker GSA; grant `roles/iap.httpsResourceAccessor` on the hub backend (or `roles/run.invoker` for invoker mode). |
| 3 | Bind KSA ↔ GSA (Workload Identity annotation on the broker's service account). |
| 4 | Broker Deployment env: `SCION_TRANSPORT_MODE=iap`, `SCION_TRANSPORT_AUDIENCE=<custom client id>`. |
| 5 | One-time registration job (same KSA) runs `scion hub brokers register …` — no curl scripts. |

### 3.5 What is deliberately *not* unified

Full merger of `pkg/sciontool/hub.Client` into `pkg/hubclient` was considered and rejected for
this workstream: the sciontool client's retry/refresh/heartbeat loops, token-file lifecycle,
and test-sandboxing guards are agent-runtime concerns with different failure semantics than
the CLI/broker REST client. Unifying the *transport layer* (this design) removes the actual
duplicated security-critical logic; unifying the app layers is a larger refactor with no IAP
payoff. Recorded as potential future work.

---

## 4. Affected-path coverage matrix

| Path | Today | After |
|---|---|---|
| sciontool heartbeat/status/refresh | ✓ (private impl) | ✓ (shared impl, same behavior) |
| `scion ls/start/stop` via hubsync | ✗ | ✓ Phase 2 (injected env) |
| `sciontool doctor` (2 checks) | ✗ | ✓ Phase 2 + new transport diagnostics |
| `cmd/hub.go` status/auth/health, completion helper | ✗ | ✓ Phase 2 (auto) |
| Broker REST + heartbeat (4 client sites) | ✗ | ✓ Phase 3 (metadata source) |
| Broker control channel WS | ✗ | ✓ Phase 3 (per-dial headers) |
| Broker registration CLI | ✗ (bypass script) | ✓ Phase 3/4 |
| `scion attach` PTY WS | ✗ (collision) | ✓ Phase 5 |
| Workstation CLI (ADC impersonation) | ✗ | ✓ Phase 5 |
| chat-app / a2a-bridge (extras) | ✗ | ✓ automatically via Phase 2 auto-detection |
| Signed-URL GCS transfers | n/a (auth in URL) | unchanged |

---

## 5. Migration & compatibility

- **Non-IAP deployments:** `FromEnv()` yields nil → wrapped transports are never installed →
  no header changes, no new network calls. The only unconditional change is the refactor of
  `pkg/sciontool/hub` onto the shared package, protected by porting its existing unit tests
  (`oidc_test.go`) unchanged.
- **Old agents / new hub & vice versa:** the env-var contract
  (`SCION_TRANSPORT_TOKEN/_AUDIENCE/_EXPIRY`, `tokens[]` refresh array) is already shipped and
  is not modified — only *consumed more widely*. New `SCION_TRANSPORT_MODE` injection is
  additive; old agents ignore it.
- **Old hubs + new brokers with transport config:** broker sends an extra `Authorization`
  header; hub middleware precedence (agent token → HMAC → bearer → proxy) identifies the
  broker by HMAC before ever considering the bearer — harmless. (Verified against the
  documented middleware precedence in `auth-proxy-iap.md`.)
- **Credentials file:** new JSON fields are optional; old files parse unchanged.
- **Rollback:** unset the env vars / config fields and behavior reverts; no data migration.

---

## 6. Security considerations

- Transport tokens continue to *only* satisfy the platform guard; app-layer identity is
  unchanged, so no privilege is derivable from a leaked transport token beyond door-opening
  (same posture as today's agent flow).
- Metadata-mode minting on brokers uses ambient identity — no SA keys distributed (consistent
  with the hub's "Option C" stance).
- `Proxy-Authorization` is stripped by IAP before reaching the hub; the hub never logs it.
  Ensure the new RoundTripper is excluded from any request-dump debug paths (mirror the
  existing token-redaction in apiclient error paths).
- The register-command relaxation (§3.4) must require *both* proxy-auth-capable transport AND
  hub proxy mode — never silently downgrade auth expectations for plain HTTP hubs.

---

## 7. Phased implementation plan

Each phase is independently reviewable, mergeable, and leaves the tree releasable.

**Phase 1 — Extract `pkg/transportauth` (S, no behavior change)**
- Move token sources, RoundTripper, expiry parsing from `pkg/sciontool/hub/oidc.go`; add
  `FromEnv()`, `ApplyHeaders()`, `HeaderMode`.
- Refactor `pkg/sciontool/hub` to consume it; port `oidc_test.go` + new unit tests for
  `FromEnv()` resolution and header modes.
- Exit criteria: `go test ./...` green; sciontool behavior byte-identical (existing tests
  prove injected/metadata/skip-on-SCION_METADATA_MODE semantics).

**Phase 2 — hubclient/doctor wiring: fixes #488 (M)**
- `hubclient.WithTransportAuth` + auto `FromEnv()` in `hubclient.New()`; wrap
  `apiclient.Transport.HTTPClient`.
- Rework `doctor.go` checks onto wrapped clients + add transport-source diagnostic output.
- Hub dispatcher additionally injects `SCION_TRANSPORT_MODE`.
- Tests: httptest middleware simulating IAP (reject requests lacking
  `Authorization: Bearer <expected>` with an HTML page) exercised against `hubclient` CRUD,
  hubsync `createHubClient`, and doctor; regression test that no `Authorization` header
  appears when env is unset.
- Exit criteria: reproduction from #488 (agent pod: `scion ls`, `sciontool doctor`) passes on
  the integration instance.

**Phase 3 — Broker support (M/L)**
- `BrokerCredentials` fields + env overrides; resolution helper shared by daemon and CLI.
- Wire REST sites (`hub_connection.go`, `server.go` ×2, `cmd/broker.go`) and
  `controlchannel.buildAuthHeaders()` (fresh token per dial).
- Register/join: persist transport fields; relax no-PAT enforcement under proxy-auth state.
- Tests: fake-IAP httptest for REST + WebSocket handshake incl. reconnect-with-refreshed-token;
  multistore round-trip of new fields.
- Exit criteria: GKE broker connects, heartbeats, and dispatches through an IAP-protected hub
  with no source patches (gist workaround §2 retired).

**Phase 4 — Deployment & docs (S)**
- `auth-proxy-iap.md`: new "Brokers behind IAP" section (custom OAuth client ID requirement,
  WI setup, registration job); update `kubernetes.md`, `multi-broker.md`.
- Optional: registration Job manifest snippet replacing `install-broker.sh`.
- Exit criteria: docs reviewed; end-to-end checklist validated on the reference deployment.

**Phase 5 — Workstation CLI + attach (M, confirmed in scope per Q1/Q3)**
- `transportauth/adcsource` (idtoken over ADC; supports SA impersonation and JSON SA keys via
  standard `GOOGLE_APPLICATION_CREDENTIALS`); CLI config via workstation `settings.yaml`
  (`hub.transport.mode` / `hub.transport.audience`) with env vars taking precedence (per Q6).
- Broker token-source resolution gains metadata → ADC fallback, covering off-GCP brokers with
  SA keys (per Q5) — no bespoke off-GCP mechanism.
- Collision handling live: `Proxy-Authorization` (IAP) / `X-Serverless-Authorization`
  (invoker) for OAuth-bearer CLI users; verify IAP header-precedence risk (§3.2).
- `scion attach`: transport token in `Authorization` at dial; scion user token moved to a
  dedicated header accepted by the hub attach endpoint.
- `getAuthInfo()`: surface "proxy-auth (transport)" state instead of "not authenticated".

Suggested sizing: Phases 1+2 = one developer PR each (or one combined PR if reviewers prefer);
Phase 3 = one PR; Phase 4 = docs PR; Phase 5 = 1–2 PRs after user sign-off on scope.

---

## 8. Open questions (for ptone, raised on thread 5668)

- **Q1 — Workstation CLI scope.** ~~Include ADC/impersonation support in this workstream?~~
  **RESOLVED (ptone, 2026-07-17): yes — ADC support is in scope. Phase 5 is confirmed part of
  this workstream.**
- **Q2 — Broker config surface.** **RESOLVED (ptone, 2026-07-17): Option A — per-connection
  credentials-file fields + env overrides. Docs must explain that per-connection placement
  exists for the multi-hub scenario (each hub has its own IAP OAuth client ID / audience; a
  broker may face an IAP hub and a plain hub simultaneously).**
- **Q3 — `scion attach` behind IAP.** **RESOLVED (ptone, 2026-07-17): in scope — folded into
  Phase 5 (hub-side header addition on the attach endpoint + client dialer change).**
- **Q4 — Test environment.** **RESOLVED (ptone, 2026-07-17): a live IAP-fronted hub exists in
  a separate environment. Validation process: each PR ships with a test plan; the PR is shared
  with agents running in the IAP environment (which hold the needed credential access) to
  execute the plan, and any issues come back as PR feedback. Developer briefs for Phases 2/3/5
  must therefore include an explicit test-plan section in the PR description; the
  `Proxy-Authorization` fall-through check (§3.2) goes in the Phase 5 test plan.**
- **Q5 — Non-GCP brokers behind IAP.** **RESOLVED (ptone, 2026-07-17): special support is out
  of scope. ADC handles the majority of off-GCP cases via JSON SA keys — i.e., the Phase 5
  `ADCSource` (which `idtoken` supports natively for SA-key ADC) should be wired as a broker
  token-source fallback when the metadata server is unavailable, rather than building any
  bespoke off-GCP mechanism. No dedicated config surface beyond the standard
  GOOGLE_APPLICATION_CREDENTIALS convention.**
- **Q6 — Auto-detection default.** **RESOLVED (ptone, 2026-07-17): auto-detection approved,
  with one addition — for workstation ADC human users, transport settings must also be
  configurable via a key in the workstation `settings.yaml` (e.g. `hub.transport.mode` /
  `hub.transport.audience`), since maintaining env vars across shells is inconvenient.
  Resolution order: env vars win over settings.yaml; either activates the transport layer.
  The settings-file path applies to the CLI (Phase 5); containerized agents/brokers keep
  env/credentials-file as primary.**

---

## 9. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| IAP rejects requests where `Authorization` holds a scion token and OIDC is in `Proxy-Authorization` | medium | Verify early on live IAP (Q4); fallback: move scion user token to `X-Scion-User-Token` (hub change) |
| Metadata-server audience minting unavailable in some GKE Autopilot configs | low | WI is a documented prerequisite; doctor diagnostic surfaces mint failures |
| Silent behavior change for GCE-hosted CLI users once auto-detection lands | low | metadata mode requires explicit audience env; injected mode requires hub-minted env — both operator-set |
| Refactor of `pkg/sciontool/hub` regresses agent auth (highest-blast-radius code) | medium | Phase 1 is mechanical extraction with ported tests; integration suite on staging before merge |
| Long-lived WebSocket vs 1h token expiry | low | IAP checks at handshake only; reconnect path mints fresh token per dial (tested) |
