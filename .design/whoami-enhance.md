# Design: Enhance `scion whoami` — Issue #277

**Author:** whoami-architect  
**Date:** 2026-07-23  
**Status:** Final  
**Issue:** #277 — Enhance `scion whoami` to return richer agent identity information

---

## Problem & Goals

Agents running inside scion containers currently have minimal self-awareness via `scion whoami`: only slug, name, and ID. Agents and scripts that need to know their project, harness, creator, model, or Hub URL must read env vars directly — knowledge that is scattered, undocumented, and fragile.

**Goals:**

1. Make `scion whoami --format json` the canonical, documented way for agents to discover their own identity and context.
2. Add all available env-var-based fields (Tier 1) with zero latency cost.
3. Add Hub API-sourced fields (Tier 2) behind an opt-in `--full` flag for agents that need phase, activity, labels, annotations, and ancestry.
4. Construct a self-link Hub URL from existing env vars (Tier 1 — no API call needed).
5. Maintain strict backward compatibility: existing 3 fields (`slug`, `name`, `id`) unchanged in position, type, and JSON key.
6. Never expose secrets (`SCION_AUTH_TOKEN`, `SCION_DEV_TOKEN`, `SCION_TRANSPORT_TOKEN`, etc.).

## Non-Goals

- **Agent role/capabilities modeling.** Role is not modeled on the agent object. We expose `template` (from `SCION_TEMPLATE_NAME`) as the current best proxy. Proper role modeling is deferred to a separate issue.
- **Tier 3 infrastructure.** No new fields on `store.Agent` or `api.AgentInfo`.
- **Changing the plain-text default output.** `scion whoami` (no flags) continues to print just the slug, matching existing behavior. New fields appear only in `--format json` and in a richer plain-text block via `--full`.
- **Full env var dump.** We use an explicit allowlist, never a glob over `SCION_*`.

---

## Proposed Design

### 1. Typed `WhoamiResult` Struct

Replace the inline `map[string]string` with a typed struct in `cmd/whoami.go`:

```go
// WhoamiResult is the JSON output shape for `scion whoami --format json`.
type WhoamiResult struct {
    // --- Tier 1: env-var fields (always populated, zero latency) ---
    Slug        string `json:"slug"`
    Name        string `json:"name"`
    ID          string `json:"id"`
    Project     string `json:"project,omitempty"`
    ProjectID   string `json:"projectId,omitempty"`
    Template    string `json:"template,omitempty"`
    Harness     string `json:"harness,omitempty"`
    Model       string `json:"model,omitempty"`
    Creator     string `json:"creator,omitempty"`
    BrokerName  string `json:"brokerName,omitempty"`
    BrokerID    string `json:"brokerId,omitempty"`
    CLIMode     string `json:"cliMode,omitempty"`
    HubEndpoint string `json:"hubEndpoint,omitempty"`
    HubURL      string `json:"hubUrl,omitempty"`      // constructed: {hubEndpoint}/agents/{id}

    // --- Tier 2: Hub API fields (only with --full, omitted otherwise) ---
    Phase       string            `json:"phase,omitempty"`
    Activity    string            `json:"activity,omitempty"`
    Labels      map[string]string `json:"labels,omitempty"`
    Annotations map[string]string `json:"annotations,omitempty"`
    Ancestry    []string          `json:"ancestry,omitempty"`
    TaskSummary string            `json:"taskSummary,omitempty"`
}
```

**Key decisions:**

- Top 3 fields (`slug`, `name`, `id`) remain first and non-`omitempty` for backward compatibility. The JSON test currently unmarshals into `map[string]string` and checks these three keys — they must not move or change type.
- All new Tier 1 fields use `omitempty` so that agents running outside a fully-provisioned environment get a clean, minimal response.
- Tier 2 fields are structurally present on the struct but only populated when `--full` is passed. With `omitempty`, they are absent from JSON output in the default path.
- `hubUrl` is constructed client-side: `fmt.Sprintf("%s/agents/%s", hubEndpoint, id)`. No API call needed. Only emitted when both `hubEndpoint` and `id` are non-empty.

### 2. Env Var Allowlist (Tier 1)

Explicit mapping from env var to struct field. This is the **security boundary** — only these vars are read:

| Env Var | Struct Field | Notes |
|---|---|---|
| `SCION_AGENT_SLUG` | `Slug` | Already used |
| `SCION_AGENT_NAME` | `Name` | Already used |
| `SCION_AGENT_ID` | `ID` | Already used |
| `SCION_PROJECT` | `Project` | |
| `SCION_PROJECT_ID` | `ProjectID` | |
| `SCION_TEMPLATE_NAME` | `Template` | Role proxy |
| `SCION_HARNESS` | `Harness` | |
| `SCION_MODEL` | `Model` | |
| `SCION_CREATOR` | `Creator` | |
| `SCION_BROKER_NAME` | `BrokerName` | |
| `SCION_BROKER_ID` | `BrokerID` | |
| `SCION_CLI_MODE` | `CLIMode` | |
| `SCION_HUB_ENDPOINT` | `HubEndpoint` | For Hub URL construction |

**Never-read list (defense in depth — these must never appear in the allowlist):**

- `SCION_AUTH_TOKEN`
- `SCION_DEV_TOKEN`
- `SCION_TRANSPORT_TOKEN`
- `SCION_METADATA_SA_EMAIL`
- `SCION_METADATA_PROJECT_ID`
- Any `*_KEY` or `*_TOKEN` env var

### 3. `--full` Flag (Tier 2)

A new boolean flag `--full` on the `whoami` command:

```go
whoamiCmd.Flags().BoolVar(&whoamiFull, "full", false,
    "Include enriched fields from the Hub (phase, activity, labels, ancestry)")
```

**Behavior when `--full` is set:**

1. Read all Tier 1 env vars (same as default).
2. Attempt to create a Hub client using the `sciontool/hub` package's `NewClient()` (which reads `SCION_HUB_ENDPOINT`, `SCION_AUTH_TOKEN`, `SCION_AGENT_ID` internally).
3. If the client is `nil` or not configured, emit a stderr warning ("Hub not available; --full fields omitted") and return Tier 1 fields only. This is **not** an error — the command succeeds with partial data.
4. If the client is available, call the Hub's agent self-fetch: `GET /api/v1/agents/{agentID}` using the existing `Agents().Get(ctx, agentID)` method (or equivalently, via the `sciontool/hub` client's own endpoint).
5. Populate `Phase`, `Activity`, `Labels`, `Annotations`, `Ancestry`, `TaskSummary` from the response.
6. Apply a 5-second context timeout to the Hub call.

**Why use `sciontool/hub.NewClient()` rather than `cmd.getHubClient()`:**

- `whoami` runs inside the agent container. The `sciontool/hub` client is designed for exactly this context — it reads the canonical token file and container env vars.
- `cmd.getHubClient()` is designed for the CLI-side (user workstation), with OAuth, dev tokens, and settings-file resolution. It would be the wrong abstraction here.
- However, there is no `GetSelf()` or `GetAgent()` on `sciontool/hub.Client` today. The Tier 2 implementation will need to add a small method or make a direct HTTP GET. The developer should check whether `sciontool/hub.Client` already has a suitable method, or add a minimal `GetSelf()` that hits `GET /api/v1/agents/{c.agentID}` and returns the relevant fields. This is a small, contained addition.

**Alternative considered:** Using `cmd.getHubClient()` + `hubclient.Agents().Get()`. Rejected because `getHubClient()` requires `*config.Settings` which is a CLI-side concern, and would pull in `hubsync` machinery that is unnecessary in-container. The `sciontool/hub` package is the correct layer.

### 4. Plain-Text Output

**Default (no `--full`):** Unchanged — prints the slug only.

```
$ scion whoami
my-agent
```

**With `--full` (plain text, no `--format json`):** Print a human-readable summary of all available fields:

```
$ scion whoami --full
Agent:    my-agent (My Agent)
ID:       abc-123-def
Project:  my-project
Template: developer
Harness:  claude
Model:    sonnet
Creator:  ptone
Broker:   my-broker
Phase:    running
Activity: working
Hub:      https://hub.example.com/agents/abc-123-def
```

Only non-empty fields are printed. Labels/annotations/ancestry are omitted from plain-text for readability (use `--format json --full` to see them).

### 5. Backward Compatibility

| Aspect | Guarantee |
|---|---|
| `scion whoami` (plain text) | Output unchanged: prints slug |
| `scion whoami --format json` | Same 3 fields (`slug`, `name`, `id`) at same keys and types. New fields are additive with `omitempty`. |
| System whoami fallback | Unchanged: when not in an agent container, delegates to system `whoami` |
| Exit codes | Unchanged |
| `--full` without Hub | Degrades gracefully — Tier 1 fields only, stderr warning, exit 0 |

**Load-bearing decision:** The first 3 JSON keys (`slug`, `name`, `id`) are used by existing scripts and agents. Changing their names or types would break consumers. This is why we use a struct with explicit json tags rather than relying on Go's alphabetical marshaling of maps.

**Easily reversible decision:** The names of new Tier 1 fields (`project`, `template`, `harness`, etc.) are chosen to match the `api.AgentInfo` JSON tags where possible. If we later decide to rename `template` to `role`, that's a one-field JSON tag change with a deprecation alias.

---

## Alternatives Considered

### A. Glob all `SCION_*` env vars

Read every env var with a `SCION_` prefix and dump them all. Rejected: **security risk**. This would expose `SCION_AUTH_TOKEN`, `SCION_DEV_TOKEN`, and other credentials. An explicit allowlist is the only safe approach.

### B. Always call the Hub API (no `--full` flag)

Make every `scion whoami --format json` call hit the Hub. Rejected: adds 100-300ms latency to every call, requires Hub reachability, and breaks the "zero-dependency introspection" use case. Many agents call `whoami` at startup for logging — latency matters.

### C. Introduce a `Role` field now

Add a `role` field backed by a new `Role` field on the agent model. Rejected: this is new infrastructure requiring changes to `store.Agent`, the Hub API, the create flow, and all broker implementations. Out of scope for this additive enhancement. `SCION_TEMPLATE_NAME` as `template` is an honest proxy that doesn't pretend to be something it isn't.

### D. Decode the JWT for ancestry instead of Hub API

The agent's JWT (`SCION_AUTH_TOKEN`) contains the ancestry chain in its claims. We could decode it locally without an API call. Rejected: reading the JWT puts `SCION_AUTH_TOKEN` in the code path of `whoami`, which is a security design smell — even if we only read claims and don't expose the token itself, it normalizes accessing the token in a user-facing command. The Hub API approach keeps the token handling contained in the `sciontool/hub` client where it belongs.

---

## Migration / Rollout

This is a purely additive change with no migration concerns:

- **No schema changes.** No database migrations, no API changes.
- **No breaking changes.** All existing output is preserved.
- **No new dependencies.** Tier 1 uses only `os.Getenv()`. Tier 2 uses the existing `sciontool/hub` client.
- **Feature flag:** The `--full` flag is the opt-in mechanism for Tier 2. It can be removed or changed without breaking the default path.
- **Forward compatibility:** The `WhoamiResult` struct can have fields added in future PRs without breaking consumers (JSON `omitempty` + additive-only policy).

---

## Open Questions

None remaining. All 3 user-facing design questions have been answered:

1. **Tier scope:** Tier 1 by default + Tier 2 via `--full` *(answered: option b)*
2. **Role:** `SCION_TEMPLATE_NAME` as `template` field *(answered: option b)*
3. **Ancestry:** Creator in Tier 1, full ancestry in `--full` *(answered: option b)*

**One implementation question for the developer:** The `sciontool/hub.Client` does not currently have a `GetSelf()` or `GetAgent()` method. The developer should check the current client surface and either:
- Use an existing HTTP helper on the client to `GET /api/v1/agents/{agentID}`
- Or add a small `GetSelf()` method that returns the fields needed by `--full`

The choice is left to the developer's judgment based on what's cleanest in the current codebase.

---

## Implementation Phases

### Phase 1: Tier 1 — Typed struct + env var fields (single commit)

1. Define `WhoamiResult` struct in `cmd/whoami.go`
2. Replace `map[string]string{...}` with struct population from env vars
3. Construct `hubUrl` from `SCION_HUB_ENDPOINT` + `SCION_AGENT_ID`
4. Update tests: change `map[string]string` unmarshaling to `WhoamiResult` struct, add assertions for new fields

### Phase 2: Tier 2 — `--full` flag + Hub API enrichment (single commit)

1. Add `--full` flag to `whoamiCmd`
2. When `--full` is set, create a `sciontool/hub` client and fetch self
3. Populate Tier 2 fields from Hub response
4. Handle Hub-unavailable case: stderr warning, return Tier 1 only
5. Add `--full` plain-text output format
6. Add tests: `--full` with mocked Hub response, `--full` without Hub (graceful degradation)

### Phase 3: Design doc in repo (single commit)

1. Commit `.design/whoami-enhance.md` to the repo per project standards

---

## Acceptance Criteria

The QA tester should verify:

1. **Backward compatibility (plain text):** `scion whoami` prints only the slug, identical to current behavior.
2. **Backward compatibility (JSON):** `scion whoami --format json` returns a JSON object containing at minimum `slug`, `name`, `id` with the same values as before.
3. **New Tier 1 fields:** `scion whoami --format json` includes `project`, `projectId`, `template`, `harness`, `model`, `creator`, `brokerName`, `brokerId`, `cliMode` when the corresponding env vars are set.
4. **Hub URL:** `scion whoami --format json` includes `hubUrl` constructed from `SCION_HUB_ENDPOINT` and `SCION_AGENT_ID` when both are present.
5. **`omitempty` behavior:** Fields for unset env vars are absent from JSON output (not present as empty strings).
6. **`--full` with Hub available:** `scion whoami --full --format json` includes Tier 2 fields: `phase`, `activity`, `labels`, `annotations`, `ancestry`, `taskSummary`.
7. **`--full` without Hub:** `scion whoami --full --format json` returns Tier 1 fields only, prints a warning to stderr, and exits 0.
8. **`--full` plain text:** `scion whoami --full` prints a human-readable multi-line summary.
9. **Security — no token exposure:** The output (both JSON and plain text) never contains `SCION_AUTH_TOKEN`, `SCION_DEV_TOKEN`, `SCION_TRANSPORT_TOKEN`, or any `*_KEY`/`*_TOKEN` value.
10. **Non-agent context:** `scion whoami` outside an agent container still falls back to system `whoami`.
11. **All existing tests pass** and new tests cover the added fields and the `--full` flag behavior.
