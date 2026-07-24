# Design: gh:// Private-Repo Skill Resolution

**Author:** ps-arch  
**Date:** 2026-07-24  
**Branch:** `scion/project-skills-gh-scheme`  
**Depends on:** `.design/project-skills.md` (Injected Skills feature)  
**Related issue:** ptone/scion#557 (`as_needed` enforcement bug — separate work)

---

## Problem & Goals

The Injected Skills feature allows `gh://owner/repo/skill-name[@ref]` URIs in a project's injected skill list. The `gh://` scheme and `GitHubSkillResolver` are fully implemented and work for public repos. The gap: `GitHubSkillResolver` is constructed with `os.Getenv("GITHUB_TOKEN")` — the broker process environment — not with the project's configured git credentials. Private repositories return 401/403.

**Goals:**
1. `gh://` URIs in injected skill lists resolve correctly when the target is a private GitHub repository using the project's configured credentials (GitHub App installation token or PAT).
2. A skill URI can name a specific project secret to use as the GitHub token — enabling per-URI credential selection and different credentials for different private repos.
3. Provision-time credentials are available to core provision code (the Go resolver) but **never** forwarded to the agent container env or harness scripts.
4. Failure is explicit: missing credentials produce a clear error; optional skills (`optional: true`) degrade gracefully.

**Non-Goals:**
- Fixing the `as_needed` enforcement bug broadly — tracked at ptone/scion#557. This design creates a new `ProvisionCredentials` channel that carries all project secrets to provision-time Go code; fixing which secrets land in the container is separate work.
- Supporting non-GitHub private registries — out of scope.
- Hub-side skill content fetching (hub fetches skill files, not broker).
- New URI schemes — `gh://` already exists and is routed correctly.

---

## Proposed Design

### Overview

Two complementary mechanisms:

1. **`ProvisionCredentials`** — a new field in the broker dispatch request carrying all project-scope secrets, available to core provision code only. Never forwarded to the container env.
2. **`?token=SECRET_NAME` URI query parameter** — an optional extension to `gh://` URIs that names which secret from `ProvisionCredentials` to use as the GitHub token for that specific resolver call.

These compose: a bare `gh://` URI uses the default credential; a `gh://...?token=SKILLS_TOKEN` URI uses the named secret. Either way, the credential is pulled from `ProvisionCredentials`, not from `ResolvedEnv`.

---

### 1. `ProvisionCredentials` in the Broker Dispatch Request

**File:** `pkg/runtimebroker/types.go`

```go
// CreateAgentRequest additions

// ProvisionCredentials carries project-scope secrets for use by core provision
// logic (skill resolution, URI variable substitution, credential helpers).
// These are NEVER forwarded to the agent container environment or harness scripts.
// Populated by the Hub from all project-scope secrets at dispatch time.
ProvisionCredentials map[string]string `json:"provisionCredentials,omitempty"`
```

**Hub population** (`pkg/hub/httpdispatcher.go`, `buildCreateRequest`):

After the existing `resolveSecrets` call, collect all project-scope secrets (all types) into `ProvisionCredentials`. The existing loop that puts environment-type secrets into `ResolvedEnv` is unchanged — `ProvisionCredentials` is additive, parallel to `ResolvedEnv`.

```pseudocode
// Collect all project-scope secrets for provision-time credential resolution.
// These are NOT merged into ResolvedEnv and will not appear in the container.
if agent.ProjectID != "" && d.secretBackend != nil {
    projectSecrets, _ := d.secretBackend.List(ctx, secret.Filter{
        Scope:   secret.ScopeProject,
        ScopeID: agent.ProjectID,
    })
    req.ProvisionCredentials = make(map[string]string, len(projectSecrets))
    for _, sm := range projectSecrets {
        if sm.SecretType == store.SecretTypeInternal {
            continue
        }
        sv, err := d.secretBackend.Get(ctx, sm.Name, secret.ScopeProject, agent.ProjectID)
        if err == nil && sv.Value != "" {
            req.ProvisionCredentials[sm.Name] = sv.Value
        }
    }
}
```

**Security invariant:** `ProvisionCredentials` is consumed by the broker's provision pipeline and then discarded. It is never written to disk, never placed in `req.ResolvedEnv`, and never passed to `provision.py` or harness containers.

---

### 2. `?token=SECRET_NAME` URI Query Parameter

**File:** `pkg/agent/github_uri.go`

Extend `GitHubSkillRef` with an optional `TokenSecretName` field:

```go
type GitHubSkillRef struct {
    Owner           string
    Repo            string
    SkillName       string
    Ref             string
    SkillPath       string
    Raw             string
    TokenSecretName string // Secret name from ?token= param; empty = use default credential
}
```

Extend `parseGHShorthand` to extract the `?token=SECRET_NAME` query parameter:

```pseudocode
// After splitting off @ref and before splitting on /:
// Check for ?token=SECRET_NAME query param in the rest string.
// The ref (@...) must be stripped first; then check if remaining path has '?'

// Supported forms:
//   gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]
// Note: ?token must come after @ref if both are present.
```

The query parameter name is `token`. The value is a bare secret name (e.g., `SKILLS_TOKEN`), not a shell variable reference. No `${}` syntax — the name IS the lookup key in `ProvisionCredentials`.

Full GitHub URL form (`https://github.com/...`): The `?token=` param is also supported if present.

**Validation:** `SECRET_NAME` must match `[A-Z][A-Z0-9_]*` (env-var-style name). Invalid names → parse error.

---

### 3. `GitHubSkillResolver` Credential Lookup

**File:** `pkg/agent/github_skill_resolver.go`

Add a new constructor that accepts both a default token and a credential map:

```go
// NewGitHubSkillResolverWithCredentials constructs a resolver with an
// explicit default token and a named-credential map for per-URI lookup.
// provisionCredentials maps secret name → secret value; may be nil.
func NewGitHubSkillResolverWithCredentials(
    defaultToken string,
    provisionCredentials map[string]string,
) *GitHubSkillResolver
```

Token resolution per-URI (in `Resolve` / `resolveOne`):

```pseudocode
func (r *GitHubSkillResolver) tokenForRef(ref *GitHubSkillRef) string {
    if ref.TokenSecretName != "" {
        if val, ok := r.provisionCredentials[ref.TokenSecretName]; ok && val != "" {
            return val
        }
        // Named secret not found — return empty string; caller handles error
        return ""
    }
    return r.token // default: GITHUB_TOKEN from ResolvedEnv or env var
}
```

When `tokenForRef` returns `""` for a required skill → resolution fails with:
```
failed to resolve gh://...: secret "SKILLS_TOKEN" not found in ProvisionCredentials; 
ensure the secret is set at project scope
```

`setAuthHeader` is updated to accept the token per-call (currently it uses `r.token` directly).

**Fallback chain for default token (no `?token=` param):**
1. `defaultToken` passed to the constructor (from `req.ResolvedEnv["GITHUB_TOKEN"]`)
2. `os.Getenv("GITHUB_TOKEN")` if `defaultToken` is empty (existing behavior, broker process env)

---

### 4. Broker Wiring

**File:** `pkg/runtimebroker/handlers.go`

```go
// Replace:
ghResolver := agent.NewGitHubSkillResolver()

// With:
defaultGHToken := req.ResolvedEnv["GITHUB_TOKEN"]
ghResolver := agent.NewGitHubSkillResolverWithCredentials(defaultGHToken, req.ProvisionCredentials)
```

`ProvisionCredentials` is passed directly to the resolver constructor. It is not stored in any context value — only the resolver itself holds it, in memory, for the duration of skill resolution.

---

### 5. CLI Wiring (local mode)

**File:** `cmd/create.go`

In local mode there is no hub-managed secrets store, so `ProvisionCredentials` is nil. The resolver falls back to `os.Getenv("GITHUB_TOKEN")` as before. Users who need private repos in local mode set `GITHUB_TOKEN` in their environment.

```go
// Replace (line ~146):
ghResolver := agent.NewGitHubSkillResolver()

// With:
ghToken := os.Getenv("GITHUB_TOKEN")
ghResolver := agent.NewGitHubSkillResolverWithCredentials(ghToken, nil)
```

No behavior change for CLI users.

---

### 6. URI Examples

```
# Public repo — no credential needed
gh://acme-corp/public-skills/my-skill

# Private repo — uses project's default GITHUB_TOKEN (App token or PAT)
gh://acme-corp/private-skills/my-skill

# Private repo — uses named project secret, not default GITHUB_TOKEN
gh://acme-corp/partner-skills/my-skill?token=PARTNER_GITHUB_TOKEN

# Private repo with pinned ref and named token
gh://acme-corp/partner-skills/my-skill@v1.2.3?token=PARTNER_GITHUB_TOKEN
```

---

### 7. Failure Handling

| Scenario | Behavior |
|---|---|
| Private repo, no project credentials | 401/403 from GitHub; clear error with "ensure GITHUB_TOKEN is set at project scope or a GitHub App is configured" |
| `?token=SKILLS_TOKEN` but secret not set | Error: "secret SKILLS_TOKEN not found in ProvisionCredentials" |
| Project App token scoped only to project repo, skill in different repo | 403 from GitHub; error message; user sets a PAT as `SKILLS_TOKEN` for that URI |
| Optional skill (`optional: true`) + any credential failure | Warning logged; skill skipped; provisioning continues |
| `ProvisionCredentials` not populated (broker without hub, local mode) | Falls back to default token from env var |

---

### 8. Security Properties

- `ProvisionCredentials` is a separate map, never merged into `ResolvedEnv`. It will not appear in the agent's container environment.
- `ProvisionCredentials` is not written to any disk path during normal provision flow.
- The resolver does not log credential values. Error messages reference secret names only, never values.
- The `?token=` query param value is a secret name (public), never the secret value itself — the URI is safe to store and display in the skill injection settings UI.
- Token scope: the default project GitHub App token is scoped to the project's git repo. Skills in other private repos require either a broader PAT (`GITHUB_TOKEN` set as a project secret) or a per-URI named secret (`?token=SECRET_NAME` where the secret has the appropriate repo scope). This is explicit and user-controlled.

---

## Alternatives Considered

### A. Pass existing `req.ResolvedEnv["GITHUB_TOKEN"]` only (no `ProvisionCredentials`)

**Rejected** because:
- GITHUB_TOKEN is already in the container env; this design gives users a way to use a skill-resolution credential that never reaches the container.
- No per-URI flexibility — all `gh://` skills share one credential.
- Token scope issue: the App token is scoped to the project's own repo and will 403 for cross-repo skills. Users would be forced to use PATs with broader scope as the project GITHUB_TOKEN.

This remains valid as a zero-extra-code first step (Phase 1), but the full design adds Phase 2.

### B. Hub-side resolution (hub fetches skill content directly)

Hub parses `gh://` URIs and fetches the content using its own GitHub credentials before dispatch. The broker receives pre-fetched content, not URIs to resolve.

**Rejected** because:
- Contradicts the existing architecture where the broker/provisioner owns skill resolution. Skills are installed into the agent's home directory by the provisioner, not by the hub.
- Hub-side fetching breaks the broker's caching layer (`CachingSkillResolver`).
- Creates a large hub-side code surface for something the agent package already handles correctly.

### C. On-demand hub callback from broker

Broker calls a new hub API endpoint to fetch a named secret value when resolving a `?token=` URI, rather than receiving all secrets upfront.

**Rejected** because:
- Adds latency (RPC per skill resolution)
- Requires a new authenticated API endpoint on the hub for secret value retrieval
- More complex error handling (network failure during provision is already handled; a new secret-fetch RPC adds another failure mode)
- The security gain (secrets not in-memory on broker until needed) is marginal given that `ProvisionCredentials` is an in-memory-only map already isolated from the container env

### D. New `provision_only` injection mode

Add a third injection mode value alongside `always` and `as_needed`. Secrets marked `provision_only` go to `ProvisionCredentials` only.

**Rejected** per user direction: avoid a third mode. Instead, the `ProvisionCredentials` mechanism naturally captures all project secrets at provision time; the "as needed" concept applies when a URI references a specific secret name. The `as_needed` enforcement bug (injecting secrets into containers that shouldn't be there) is tracked separately as ptone/scion#557.

---

## Migration / Rollout

- **Backward compatible.** All existing `gh://` URIs with no `?token=` param continue to work as before (fallback to env var token).
- **No schema changes.** `ProvisionCredentials` is a new JSON field on the wire format; brokers that don't recognize it ignore it.
- **Graceful degradation.** If hub doesn't populate `ProvisionCredentials` (older hub version), resolver falls back to default token. `?token=SECRET_NAME` URIs would fail with a clear "secret not found" error rather than silently ignoring the param.
- **No UI changes required.** The `?token=SECRET_NAME` syntax is a URI extension that the existing skill injection UI stores as-is in the URI field. Optionally, the UI can highlight it with a tooltip in a future iteration.

---

## Open Questions

None blocking this design. All design decisions have been resolved with the user.

**Tracked separately:**
- ptone/scion#557 — `as_needed` injection mode enforcement: `as_needed` secrets should not appear in `ResolvedEnv`. Fix in dedicated work.
- Token scope UX: when a user's App token is repo-scoped and a skill in another repo returns 403, the error message must be actionable. Exact wording TBD by developer.

---

## Implementation Phases

### Phase 1: Default credential injection (~20 lines)
**What:** Wire the existing `req.ResolvedEnv["GITHUB_TOKEN"]` into `GitHubSkillResolver` at broker construction time. No `ProvisionCredentials`, no URI query params.

**Changes:**
- `pkg/agent/github_skill_resolver.go`: add `NewGitHubSkillResolverWithToken(token string)` constructor
- `pkg/runtimebroker/handlers.go`: replace `NewGitHubSkillResolver()` with `NewGitHubSkillResolverWithToken(req.ResolvedEnv["GITHUB_TOKEN"])`
- `cmd/create.go`: `NewGitHubSkillResolverWithToken(os.Getenv("GITHUB_TOKEN"))` (no behavior change)

**Outcome:** Private repos accessible via the project's existing GitHub App token — provided the skill repo is in scope of that token (i.e., same repo as the project's git remote, or no git remote configured).

### Phase 2: `ProvisionCredentials` + URI `?token=` param (~130 lines)
**What:** The full design. Per-URI credential selection; credentials never in container env.

**Changes:**
- `pkg/runtimebroker/types.go`: add `ProvisionCredentials map[string]string`
- `pkg/hub/httpdispatcher.go`: populate `ProvisionCredentials` from project-scope secrets
- `pkg/agent/github_uri.go`: parse `?token=SECRET_NAME` into `GitHubSkillRef.TokenSecretName`; validate secret name format
- `pkg/agent/github_skill_resolver.go`: `NewGitHubSkillResolverWithCredentials(defaultToken, provisionCredentials)`, `tokenForRef(ref)`, per-call token threading through `setAuthHeader`
- `pkg/runtimebroker/handlers.go`: pass `req.ProvisionCredentials` to resolver constructor
- Tests: unit tests for URI parsing with `?token=`, resolver token selection, fallback behavior, missing-secret error

**Outcome:** Full feature. Any named project secret can be used as a GitHub token for a specific `gh://` skill URI. Credential never in container env.

---

## Acceptance Criteria

The QA tester should verify:

1. **Public repo, no credentials configured** — `gh://owner/public-repo/skill` resolves successfully and the skill is installed.

2. **Private repo, GitHub App configured** — `gh://org/same-repo-as-project/skill` resolves using the minted App token. The `GITHUB_TOKEN` value does NOT appear in any additionally-injected env var that wouldn't have been there before.

3. **Private repo, cross-repo, named PAT** — `gh://org/other-private-repo/skill?token=SKILLS_TOKEN` where `SKILLS_TOKEN` is a project secret resolves successfully. Verify that `SKILLS_TOKEN` does NOT appear in the agent container's environment (check `env | grep SKILLS_TOKEN` inside a started agent container returns nothing).

4. **Named secret missing** — `gh://...?token=MISSING_SECRET` with no `MISSING_SECRET` project secret → provisioning fails with an error message that names the missing secret and suggests setting it at project scope. Does NOT fail silently or use a wrong credential.

5. **Optional skill with missing credential** — `{uri: "gh://...?token=MISSING_SECRET", optional: true}` → provisioning succeeds; warning in logs; skill directory not present; no error returned to caller.

6. **`?token=` not in URI, no App token, no GITHUB_TOKEN env** — `gh://org/private-repo/skill` → fails with a 401-related error message pointing user to configure credentials.

7. **CLI local mode** — `scion create` (no hub) with `GITHUB_TOKEN` set in process env resolves public and private repos as before. No regression.

8. **Phased backward compatibility** — existing `gh://` URIs without `?token=` continue to work unchanged in all scenarios.

9. **URI validation** — `gh://owner/repo/skill?token=invalid-secret-name` (lowercase, hyphens) → parse error with clear message about valid secret name format.

10. **Cache correctness** — two `gh://` URIs for the same skill but different `?token=` values are treated as separate cache entries (cache key includes the token name, not the token value, to avoid leaking values into cache keys).
