# Env Writer Inventory — #127 P3a

Every writer into `ResolvedEnv` (hub→broker wire, `map[string]string`) and
`ResolvedSecrets` (`[]api.ResolvedSecret`), plus broker-side and runtime-side
env writers that add keys after the wire crossing.

Classification kinds per §3.2.1 (updated with Q2 bootstrap kind):
- **plain** — non-sensitive operational metadata, delivered via `--env KEY=VALUE`
- **secret-fetchable** — lives in the secret store, can be fetched via P2 endpoint
- **secret-injected** — produced at dispatch time, in no store, delivery channel TBD (P3b)
- **secret-bootstrap** — credential that MUST stay in argv because it bootstraps the delivery channel itself. Exposure managed by lifetime (single-use JWT, short-lived OIDC), not concealment. See §3.4/§3.4.1.

---

## Hub-side writers into `req.ResolvedEnv` (buildCreateRequest)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| H1 | `httpdispatcher.go:521` | `*` (all of `agent.AppliedConfig.Env`) | Config-level env (template, agent config) | mixed — see H1a |
| H1a | — | User-defined keys via `AppliedConfig.Env` | Template `.env` or agent config | **plain** for most; **secret-fetchable** if the key maps to a stored secret (env-type secrets are pre-merged here by the hub; original comes from storage) |
| H2 | `httpdispatcher.go:547` | `SCION_MODEL` | `agent.AppliedConfig.Model` | **plain** |
| H3 | `httpdispatcher.go:548` | `SCION_THINKING_LEVEL` | `agent.AppliedConfig.ThinkingLevel` | **plain** |
| H4 | `httpdispatcher.go:553` | `SCION_HUB_NAME` | `d.hubName` (hub server name) | **plain** |
| H5 | `httpdispatcher.go:566-571` | `*` (storage env vars) | `resolveEnvFromStorage` — user/project/broker scopes | mixed — see H5a |
| H5a | — | User-stored env vars (e.g. `GEMINI_API_KEY`) | Hub env var storage | **secret-fetchable** (they are in the secret store / env var store) |
| H6 | `httpdispatcher.go:617` | `GITHUB_TOKEN` | NoAuth path: project-scoped secret from `secretBackend.Get` | **secret-fetchable** (it IS in the secret store) |
| H7 | `httpdispatcher.go:636` | `GITHUB_TOKEN` | NoAuth path: user-scoped secret fallback from `secretBackend.Get` | **secret-fetchable** |
| H8 | `httpdispatcher.go:671-672` | `*` (env-type secrets from `resolveSecrets`) | Secret store, env-type secrets injected into ResolvedEnv | **secret-fetchable** |
| H9 | `httpdispatcher.go:705,710` | `SCION_USER_GITHUB_TOKEN`, `SCION_GITHUB_APP_ENABLED` | Flags: user has existing GITHUB_TOKEN from secrets | **plain** (boolean flags, not credentials) |
| H10 | `httpdispatcher.go:728` | `GITHUB_TOKEN` | GitHub App: `MintGitHubAppTokenForProject` — ephemeral token | **secret-injected** |
| H11 | `httpdispatcher.go:729` | `SCION_GITHUB_APP_ENABLED` | Literal `"true"` | **plain** |
| H12 | `httpdispatcher.go:730` | `SCION_GITHUB_TOKEN_EXPIRY` | GitHub App token expiry timestamp | **plain** |
| H13 | `httpdispatcher.go:731` | `SCION_GITHUB_TOKEN_PATH` | Literal `"/tmp/.github-token"` | **plain** |
| H14 | `httpdispatcher.go:816` | `SCION_DEV_TOKEN` | `d.devAuthToken` (dev-mode auth token) | **secret-injected** |
| H15 | `httpdispatcher.go:830-834` | `SCION_TRANSPORT_TOKEN`, `SCION_TRANSPORT_AUDIENCE`, `SCION_TRANSPORT_TOKEN_EXPIRY`, `SCION_TRANSPORT_MODE` | `transportMinter.MintIDToken` — IAP/Cloud Run invoker token | **secret-bootstrap** (token), **plain** (audience, expiry, mode) |

## Hub-side writers into `req.ResolvedEnv` (DispatchAgentStart)

Same keys as buildCreateRequest, but constructed independently (not via buildCreateRequest):

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| S1 | `httpdispatcher.go:1840-1843` | `*` (all of `agent.AppliedConfig.Env`) | Config-level env | mixed (same as H1/H1a) |
| S2 | `httpdispatcher.go:1845` | `SCION_MODEL` | `injectModelEnv` | **plain** |
| S3 | `httpdispatcher.go:1846` | `SCION_THINKING_LEVEL` | `injectThinkingLevelEnv` | **plain** |
| S4 | `httpdispatcher.go:1855-1862` | `*` (storage env vars) | `resolveEnvFromStorage` | mixed (same as H5/H5a) |
| S5 | `httpdispatcher.go:1875-1880` | `*` (env-type secrets) | Secret store env-type injection | **secret-fetchable** |
| S6 | `httpdispatcher.go:1889` | `SCION_AGENT_ID` | `agent.ID` | **plain** |
| S7 | `httpdispatcher.go:1891-1892` | `SCION_GROVE_ID`, `SCION_PROJECT_ID` | `agent.ProjectID` | **plain** |
| S8 | `httpdispatcher.go:1895` | `SCION_AGENT_SLUG` | `agent.Slug` | **plain** |
| S9 | `httpdispatcher.go:1900` | `SCION_HUB_ENDPOINT` | `d.hubEndpoint` | **plain** |
| S10 | `httpdispatcher.go:1902` | `SCION_HUB_NAME` | `d.hubName` | **plain** |
| S11 | `httpdispatcher.go:1917-1932` | `SCION_WORKSPACE_MODE`, `SCION_WORKSPACE_GIT` | Resolved workspace config | **plain** |
| S12 | `httpdispatcher.go:1941-1947` | `SCION_METADATA_MODE`, `SCION_METADATA_SA_EMAIL`, `SCION_METADATA_PROJECT_ID` | GCP identity config | **plain** (SA email is a public identifier, not a secret) |
| S13 | `httpdispatcher.go:1956` | `SCION_AUTH_TOKEN` | `tokenGenerator.GenerateAgentToken` — agent JWT | **secret-bootstrap** |
| S14 | `httpdispatcher.go:1968-1972` | `SCION_TRANSPORT_TOKEN`, `SCION_TRANSPORT_AUDIENCE`, `SCION_TRANSPORT_TOKEN_EXPIRY`, `SCION_TRANSPORT_MODE` | Transport token minting | **secret-bootstrap** (token), **plain** (rest) |
| S15 | `httpdispatcher.go:1993-2003` | `GITHUB_TOKEN`, `SCION_GITHUB_APP_ENABLED`, `SCION_GITHUB_TOKEN_EXPIRY`, `SCION_GITHUB_TOKEN_PATH` | GitHub App minting | **secret-injected** (GITHUB_TOKEN), **plain** (rest) |
| S16 | `httpdispatcher.go:2008` | `SCION_USER_GITHUB_TOKEN`, `SCION_GITHUB_APP_ENABLED` | User-GITHUB_TOKEN precedence flags | **plain** |

## Hub-side writers into `req.ResolvedEnv` (DispatchResumeWithReAuth / DispatchAgentRestart)

Mirrors DispatchAgentStart. Same keys as S1–S16 at corresponding line ranges (~2100–2280).

## Hub-side writers into `req.ResolvedEnv` (DispatchFinalizeEnv)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| F1 | `httpdispatcher.go:1209-1213` | `*` (caller-provided env map merged in) | Caller passes env map | mixed — depends on caller |
| F2 | `httpdispatcher.go:1237` | `*` (as-needed env vars) | `resolveAsNeededForKeys` | **secret-fetchable** |

## Hub-side writers into `req.AgentToken` (separate field, not ResolvedEnv)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| T1 | `httpdispatcher.go:454` | `AgentToken` (becomes `SCION_AUTH_TOKEN` at broker) | `tokenGenerator.GenerateAgentToken` | **secret-bootstrap** |

## Hub-side writers into `req.ResolvedSecrets`

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| RS1 | `httpdispatcher.go:655` | `*` (all resolved secrets) | `resolveSecrets` from secret store | **secret-fetchable** |
| RS2 | `httpdispatcher.go:599` | `nil` (cleared under NoAuth) | — | — |

---

## Broker-side writers into env (buildStartContext)

These write into the local `env map[string]string` which becomes `opts.Env`:

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| B1 | `start_context.go:217` | `*` (all of `in.ResolvedEnv`) | Hub wire — passthrough | mixed (inherits hub classification) |
| B2 | `start_context.go:226-229` | `*` (Config.Env overrides) | Agent config env list | mixed (same as H1) |
| B3 | `start_context.go:241` | `SCION_AUTH_TOKEN` | `in.AgentToken` (dedicated field from hub) | **secret-bootstrap** |
| B4 | `start_context.go:245-248` | `SCION_AUTH_TOKEN` | Kept from resolvedEnv (start/resume path) | **secret-bootstrap** |
| B5 | `start_context.go:251` | `SCION_AUTH_TOKEN` | Broker's own dev token (`os.Getenv`) — last resort | **secret-bootstrap** |
| B6 | `start_context.go:308-309` | `SCION_HUB_ENDPOINT`, `SCION_HUB_URL` | Resolved hub endpoint | **plain** |
| B7 | `start_context.go:326-327` | `SCION_AGENT_SLUG` | `in.Slug` | **plain** |
| B8 | `start_context.go:329` | `SCION_AGENT_ID` | `in.AgentID` | **plain** |
| B9 | `start_context.go:332-333` | `SCION_GROVE_ID`, `SCION_PROJECT_ID` | `in.ProjectID` | **plain** |
| B10 | `start_context.go:336-337` | `SCION_GROVE_PATH`, `SCION_PROJECT_PATH` | `in.ProjectPath` | **plain** |
| B11 | `start_context.go:345` | `SCION_WORKSPACE_MODE` | Resolved workspace sharing mode | **plain** |
| B12 | `start_context.go:354` | `SCION_BROKER_NAME` | `s.config.BrokerName` | **plain** |
| B13 | `start_context.go:357` | `SCION_BROKER_ID` | `s.config.BrokerID` | **plain** |
| B14 | `start_context.go:360` | `SCION_CREATOR` | `in.CreatorName` | **plain** |
| B15 | `start_context.go:364` | `SCION_DEBUG` | Literal `"1"` | **plain** |
| B16 | `start_context.go:384-402` | `SCION_METADATA_MODE`, `SCION_METADATA_PORT`, `SCION_METADATA_SA_EMAIL`, `SCION_METADATA_PROJECT_ID`, `GCE_METADATA_HOST`, `GCE_METADATA_ROOT` | GCP metadata server config | **plain** |
| B17 | `start_context.go:532` | `SCION_SHARED_WORKSPACE` | Literal `"true"` (deprecated) | **plain** |
| B18 | `start_context.go:545` | `SCION_GIT_CLONE_URL` | `gc.URL` from GitClone config | **secret-injected** (URL may contain embedded credentials — this is the original #127 bug) |
| B19 | `start_context.go:547` | `SCION_GIT_BRANCH` | `gc.Branch` | **plain** |
| B20 | `start_context.go:549` | `SCION_GIT_DEPTH` | `gc.Depth` (int→string) | **plain** |
| B21 | `start_context.go:552` | `SCION_AGENT_BRANCH` | `in.Config.Branch` | **plain** |
| B22 | `start_context.go:583` | `SCION_WORKSPACE_GIT` | Literal `"true"` | **plain** |
| B23 | `start_context.go:590-591` | `SCION_TELEMETRY_ENABLED` | Read from env, passed through | **plain** |

## Broker-side writers into `opts.ResolvedSecrets` (buildStartContext)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| BR1 | `start_context.go:600` | `*` (passthrough from hub) | `in.ResolvedSecrets` | **secret-fetchable** |
| BR2 | `start_context.go:618-625` | `gcloud-adc` (auto-injected) | Host ADC file (colocated mode) | **secret-injected** (file content, from broker host, not in any store) |

---

## Runtime-side writers into `config.Env` (post-wire)

These operate on `RunConfig.Env []string` which already contains the broker's output:

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| R1 | `docker.go:60` / `podman.go:149` / `apple_container.go:59` | `SCION_STAGED_SECRETS` | `serializeSecrets(config.ResolvedSecrets)` — base64-encoded secret blob | **secret-injected** (contains all file+env secrets serialized). **BYPASS CHANNEL**: this single key carries every secret value regardless of what P3b does for the other six. Flag only — do not solve here. |
| R2 | `docker.go:66` / `podman.go:155` / `apple_container.go:65` | `SCION_OTEL_GCP_CREDENTIALS` | `findGCPTelemetryCredentialPath` — path to GCP telemetry cred | **plain** (it's a file path, not a credential value) |
| R3 | `common.go:354-359` | `*` (config.Env passthrough) | Broker output → `-e KEY=VALUE` args | mixed (inherits) |
| R4 | `common.go:364-366` | `*` (env-type ResolvedSecrets) | `-e KEY=VALUE` for env-type secrets | **secret-fetchable** |

## Cloudrun-sandbox runtime writers (envFor)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| CS1 | `cloudrun_sandbox_runtime.go:466` | `PATH` | Literal default PATH | **plain** |
| CS2 | `cloudrun_sandbox_runtime.go:470-474` | `*` (config.Env passthrough) | Broker output | mixed (inherits) |
| CS3 | `cloudrun_sandbox_runtime.go:483-487` | `*` (harness env) | `cfg.Harness.GetEnv(...)` | **plain** |
| CS4 | `cloudrun_sandbox_runtime.go:492-497` | `SCION_PROJECT`, `SCION_GROVE`, `SCION_PROJECT_ID`, `SCION_GROVE_ID` | Config fields | **plain** |
| CS5 | `cloudrun_sandbox_runtime.go:508` | `SCION_WORKSPACE_PATH` | Literal `/workspace` | **plain** |
| CS6 | `cloudrun_sandbox_runtime.go:513` | `SCION_WORKSPACE_BACKEND` | Config field | **plain** |
| CS7 | `cloudrun_sandbox_runtime.go:517-518` | `SCION_HOST_UID`, `SCION_HOST_GID` | `os.Getuid()`, `os.Getgid()` | **plain** |
| CS8 | `cloudrun_sandbox_runtime.go:527-529` | `HOME`, `USER`, `LOGNAME` | Literal sandbox paths | **plain** |

## Agent-side writers (pkg/agent/run.go)

| # | File:Line | Key(s) | Value origin | Kind |
|---|-----------|--------|-------------|------|
| A1 | `run.go:870` | `*` (buildAgentEnv merges scionCfg.Env + opts.Env) | Template config + resolved env | mixed (inherits) |
| A2 | `run.go:556` | `opts.ResolvedSecrets` (filtering) | Filters secrets for resolved auth | **secret-fetchable** (subset of RS1) |

---

## Summary of secret-bootstrap keys (bootstrap the delivery channel)

These credentials bootstrap the delivery channel itself, so they cannot be
delivered through that channel. P3b performs no routing for these keys.

| Key | Origin | Delivery | Notes |
|-----|--------|----------|-------|
| `SCION_AUTH_TOKEN` | `GenerateAgentToken` (JWT) | **NOT in argv**: diverted to `~/.scion/scion-token` by `pkg/agent/run.go:761-777` (temp+rename, 0600), deleted from `opts.Env` at :777. Read by `pkg/hubsync/sync.go:1329`. | Single-use (§3.4) |
| `SCION_TRANSPORT_TOKEN` | `MintIDToken` (IAP/Cloud Run) | **IN argv**: no diversion exists. Accepted exposure. | Google-signed OIDC, 1h, lifetime NOT boundable (`GenerateIdTokenRequest` has no `Lifetime` field, unlike `GenerateAccessTokenRequest` which sets 300s). See §3.4.1. |

## Summary of secret-injected keys (the P3b problem set)

These are values produced at dispatch time, in no store, that cannot be delivered
via `SCION_SECRET_KEYS` + fetch. P3b must find an alternative delivery channel.

| Key | Origin | Notes |
|-----|--------|-------|
| `GITHUB_TOKEN` (from GitHub App) | `MintGitHubAppTokenForProject` | Ephemeral, expires, not in secret store |
| `SCION_DEV_TOKEN` | `d.devAuthToken` (dev mode only) | Dev-mode fallback |
| `SCION_GIT_CLONE_URL` | `gc.URL` from GitClone config | May embed credentials — the original #127 bug |
| `SCION_STAGED_SECRETS` | `serializeSecrets(ResolvedSecrets)` | Base64 blob of all secrets — injected at runtime level |
| `gcloud-adc` (ResolvedSecret) | Host ADC file (colocated broker) | **FILE**, not an env var. Content from host ADC, not in hub secret store |
