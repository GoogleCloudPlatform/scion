# Cleanup Candidates: Simplification Refactors

## Status
**Proposed** | March 2026

## Motivation

Several flows in the codebase have accumulated branching complexity as the system evolved from local-only to the hosted Hub/Broker architecture. The two areas below are the highest-value simplification targets — they cause the most debugging friction today and carry the highest regression risk when features are added.

---

## Candidate 1: Harness-Config Resolution Chain

### Problem

The "which harness-config am I using?" resolution logic is implemented **three separate times** with subtly different priority orders and fallback chains:

| Location | Function | Fallback chain |
|---|---|---|
| `pkg/agent/provision.go:406-434` | `ProvisionAgent` | CLI flag → template `default_harness_config` → template `harness_config` → profile default → settings default |
| `pkg/agent/run.go:139-164` | `Start` | CLI flag → stored config → template `default_harness_config` → harness name → profile default → settings default |
| `pkg/runtimebroker/handlers.go:1837-1887` | `resolveHarnessConfigName` | config field → template-matches-disk-dir → template-matches-settings-entry → profile default → settings default |

Each copy uses slightly different field names and fallback semantics. When `Start` is called for an existing agent (resume path), it re-derives the harness-config independently from `ProvisionAgent`, which can produce a different result if settings have changed since provisioning.

### Impact

- Bug fixes to the resolution chain must be applied in three places.
- Debugging "wrong harness-config selected" requires tracing through whichever of the three paths was hit.
- The `Start` function (`run.go:36-763`) is 727 lines, with ~80 lines (139-220) dedicated to re-deriving harness-config and image resolution that `ProvisionAgent` already computed.

### Proposed Refactor

Extract a single `ResolveHarnessConfig` function into `pkg/config/` (or `pkg/agent/resolve.go`) with an explicit, documented priority chain:

```go
type HarnessConfigResolution struct {
    Name           string // resolved harness-config name
    Source         string // for debug logging: "cli-flag", "template-default", etc.
    HarnessName    string // harness identity (e.g., "claude", "gemini")
    AuthType       string // auth_selected_type from harness-config
    Image          string // image from harness-config (base layer)
    User           string // container user from harness-config
}

func ResolveHarnessConfig(
    cliFlag string,
    templateCfg *api.ScionConfig,
    settings *VersionedSettings,
    profileName string,
    grovePath string,
    templatePaths []string,
) (*HarnessConfigResolution, error)
```

All three call sites (`ProvisionAgent`, `Start`, broker `resolveHarnessConfigName`) call this one function. The result is cached on the agent's `scion-agent.json` at provision time so `Start` on resume never re-derives — it reads the stored resolution.

### Scope

- New: `pkg/config/resolve_harness_config.go` (~80 lines + tests)
- Modified: `pkg/agent/provision.go` (remove ~30 lines), `pkg/agent/run.go` (remove ~80 lines), `pkg/runtimebroker/handlers.go` (remove `resolveHarnessConfigName` + `resolveHarnessIdentity`, ~90 lines)
- Net code reduction: ~120 lines

---

## Candidate 2: Broker `createAgent` / `startAgent` / `finalizeEnv` StartOptions Assembly

### Problem

The runtime broker's agent creation path builds an `api.StartOptions` struct and a merged environment map in **three separate handlers** that share ~70% of their logic:

| Handler | Lines | Path |
|---|---|---|
| `createAgent` | `handlers.go:230-824` | Full create: env merge → env-gather check → hydrate template → git-clone → workspace download → provision/start |
| `startAgent` | `handlers.go:1007-1185` | Start existing: env merge → hub endpoint → hydrate template → git-clone → start |
| `finalizeEnv` | `handlers.go:1932-2086` | Env-gather completion: env merge → hydrate template → git-clone → start |

Each handler independently:
1. Resolves the hub-managed grove path from `GroveSlug` (identical `~/.scion/groves/<slug>` block, duplicated at lines 323-350 and 1032-1054)
2. Builds the merged env map with hub endpoint, broker name, debug flag, auth tokens
3. Translates `SCION_TELEMETRY_ENABLED` into `TelemetryOverride`
4. Hydrates templates from Hub
5. Handles git-clone env injection
6. Passes through resolved secrets
7. Calls `resolveManagerForOpts` and `mgr.Start`

The `createAgent` handler alone is 594 lines. Much of that is env assembly and grove-path resolution that could be shared.

### Impact

- Adding a new env var or changing grove-path resolution requires updating three places.
- The env-gather `finalizeEnv` handler is essentially a stripped-down copy of `createAgent` — any fix to env merging in one must be mirrored in the other.
- The idempotency/dispatch-attempt tracking in `createAgent` is interleaved with the env assembly logic, making it harder to test either concern independently.

### Proposed Refactor

Extract a shared `buildStartContext` that handles everything up to the `mgr.Start` call:

```go
type startContext struct {
    Opts         api.StartOptions
    MergedEnv    map[string]string
    TemplateSlug string
    Manager      agent.Manager
}

func (s *Server) buildStartContext(ctx context.Context, r *http.Request, req CreateAgentRequest) (*startContext, error)
```

This function encapsulates:
- Hub-managed grove path resolution
- Env merging (resolved env + config env + auth tokens + hub endpoint + broker identity + debug)
- Template hydration
- Git-clone env injection
- Telemetry override translation
- Resolved secrets passthrough
- Manager resolution

Each handler becomes a thin wrapper:
- `createAgent`: dispatch-attempt tracking → `buildStartContext` → env-gather check → provision or start
- `startAgent`: `buildStartContext` (from start request) → start → forced heartbeat
- `finalizeEnv`: pending state lookup → merge gathered env → `buildStartContext` → start

### Scope

- New: `pkg/runtimebroker/start_context.go` (~150 lines + tests)
- Modified: `pkg/runtimebroker/handlers.go` — `createAgent` shrinks from ~594 to ~200, `startAgent` from ~178 to ~60, `finalizeEnv` from ~154 to ~60
- Net code reduction: ~350 lines

### Additional Benefits

The extracted `buildStartContext` becomes a natural place to add structured logging for the "what env did this agent actually get?" debugging question, with a single log point instead of scattered debug statements across three handlers.

---

## Candidate Evaluation

| Candidate | Complexity Reduced | Files Touched | Risk | Debugging Value |
|---|---|---|---|---|
| 1. Harness-config resolution | High (3 divergent chains → 1) | 4 | Low (pure function, easy to test) | High — most common "wrong config" issues |
| 2. Broker start context | Very High (3 handlers → shared core) | 2 | Medium (HTTP handler restructuring) | Very High — env/auth debugging is the #1 broker issue |

### Recommended Order

**Candidate 1 first** — it's a pure-function extraction with no HTTP concerns, lower risk, and makes the harness-config portion of Candidate 2 simpler (the broker handlers will just call the shared resolver instead of their own).

**Candidate 2 second** — larger payoff but benefits from Candidate 1 being done first.

---

## Related Prior Work

- `.design/dispatch-cleanup.md` — Covers Hub→Broker dispatch reliability (auth, state, retries). Phases 1-3 are marked complete. The refactors proposed here are complementary and address a different layer (StartOptions assembly vs dispatch transport).
- `.design/_archive/tech-debt-review.md` — Identified `Start` as a "god function" and harness abstraction leaks. Candidate 1 directly addresses the harness-config portion; Candidate 2 addresses the broker-side duplication that wasn't covered in that review.
- `.design/agent-state-refactor.md` — Completed. The layered phase/activity model it introduced simplified state handling but did not address the config-resolution or env-assembly duplication covered here.
