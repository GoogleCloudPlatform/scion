# Secret ID Hub Refactor: Multi-Hub GCP Secret Manager Namespacing

**Created:** 2026-03-30
**Status:** Proposed
**Related:** `hosted/hosted-architecture.md`, `pkg/secret/gcpbackend.go`, `pkg/store/models.go`

---

## 1. Overview

When multiple Scion Hub instances share a single GCP project for Secret Manager, hub-scoped secrets collide because the current naming scheme uses a hardcoded `"hub"` sentinel as the scope ID. Since `sha256("hub")` is constant, every hub produces the same GCP SM secret name for a given key.

**Example of the collision:**
```
Hub A sets GITHUB_APP_PRIVATE_KEY →
  gcpsm:projects/deploy-demo-test/secrets/scion-hub-08d33503ee27-GITHUB_APP_PRIVATE_KEY

Hub B sets GITHUB_APP_PRIVATE_KEY →
  gcpsm:projects/deploy-demo-test/secrets/scion-hub-08d33503ee27-GITHUB_APP_PRIVATE_KEY  ← same!
```

Hub B's write silently overwrites Hub A's secret value (new GCP SM version), and Hub A reads the wrong value on next access.

### Goals

1. Hub-scoped secrets are unique per hub instance within a shared GCP project.
2. Grove, user, and broker-scoped secrets also incorporate hub identity to prevent cross-hub collisions.
3. The hub instance ID is deterministic and config-driven, defaulting to a hash of the machine hostname.
4. Human readability in the GCP console is preserved via labels, not by embedding slugs in secret names.

### Non-Goals

- Automated migration of existing secrets from the old naming scheme to the new scheme. That will be addressed separately as part of a future admin maintenance tooling effort.
- Changes to the local secret backend (values stored in the Hub database). The local backend is unaffected by GCP SM naming, though it will adopt the new `ScopeIDHub` value for DB-level consistency.
- Changes to the GCS bucket naming scheme for templates/workspaces (separate effort).

---

## 2. Current State

### Secret Naming Scheme

```
GCP SM Secret ID: scion-{scope}-{sha256(scopeID)[:12]}-{name}
```

- `scope`: one of `hub`, `user`, `grove`, `runtime_broker`
- `scopeID`: identifier for the scoped entity
- `name`: the secret key (e.g., `GITHUB_APP_PRIVATE_KEY`)

**File:** `pkg/secret/gcpbackend.go:287-291`

### Hub Scope ID

```go
const ScopeIDHub = "hub"  // pkg/store/models.go:698
```

Every hub instance uses this identical sentinel, producing identical hashes.

### Where `ScopeIDHub` Is Used

| Location | Usage |
|----------|-------|
| `pkg/secret/gcpbackend.go:194` | `Resolve()` — hub secrets as baseline scope |
| `pkg/secret/localbackend.go:91` | `Resolve()` — same pattern in local backend |
| `pkg/hub/server.go:668,679` | Loading signing keys (`agent_signing_key`, `user_signing_key`) |
| `pkg/hub/handlers_github_app.go` | GitHub App secret storage/retrieval |
| `pkg/hub/httpdispatcher.go:703,798` | Hub-scoped env var listing |
| `pkg/hub/handlers.go:5722` | Scope resolution for secret API |
| Various test files | Assertions against `store.ScopeIDHub` |

---

## 3. Design

### 3.1 Hub Instance ID

Each hub instance will have a unique, stable identifier derived from configuration.

**Generation strategy:** SHA-256 hash of the machine hostname, truncated to 12 hex characters.

```go
// pkg/config/hub_config.go or pkg/hub/identity.go

func DefaultHubID() string {
    hostname, err := os.Hostname()
    if err != nil {
        hostname = "unknown"
    }
    h := sha256.Sum256([]byte(hostname))
    return hex.EncodeToString(h[:6]) // 12 hex chars
}
```

**Configuration:** Operators can override via config or environment variable:

```yaml
# settings.yaml
server:
  hub:
    hubId: "my-prod-hub-01"
```

```bash
SCION_SERVER_HUB_HUBID=my-prod-hub-01
```

If not explicitly set, the auto-generated hostname hash is used. The resolved value is logged at startup for operator visibility.

**Config struct change:**

```go
// pkg/config/hub_config.go
type HubServerConfig struct {
    // ... existing fields ...

    // HubID is a unique identifier for this hub instance.
    // Used to namespace secrets and other hub-scoped resources in shared GCP projects.
    // Defaults to sha256(hostname)[:12] if not set.
    HubID string `json:"hubId" yaml:"hubId" koanf:"hubId"`
}
```

The `envKeyToConfigKey` mapping in `hub_config.go` already handles `hubid` → `hubId` camelCase conversion via the existing pattern; add it to the `camelCaseFields` map.

### 3.2 Replace `ScopeIDHub` Sentinel

The constant `ScopeIDHub = "hub"` will be replaced with a dynamic value — the resolved hub instance ID.

**Change `store/models.go`:**

```go
// Remove the constant:
// const ScopeIDHub = "hub"

// Replace with a documentation comment:
// ScopeIDHub was previously a fixed sentinel "hub". It is now the hub's
// instance ID, resolved at startup from config or hostname hash.
// All call sites must pass the resolved hub ID instead of this constant.
```

All call sites that currently reference `store.ScopeIDHub` will instead receive the hub ID from the server/backend context. This is a compile-time-breakage-driven refactor — removing the constant forces all callers to be updated.

### 3.3 Threading the Hub ID

The hub ID needs to reach the secret backend and all code that references hub-scoped secrets.

**Option: Pass through `GCPBackend` (and `LocalBackend`)**

Add a `hubID` field to both backends:

```go
type GCPBackend struct {
    store     store.SecretStore
    smClient  SMClient
    projectID string
    hubID     string  // NEW
}
```

Update `Resolve()` to use `b.hubID` instead of `store.ScopeIDHub`:

```go
scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
```

Update the `SecretBackend` interface to expose the hub ID for callers that need it:

```go
type SecretBackend interface {
    // ... existing methods ...
    HubID() string
}
```

The `hub.Server` already holds the `secretBackend` and can call `secretBackend.HubID()` wherever it currently uses `store.ScopeIDHub`.

### 3.4 GCP SM Naming Scheme (Updated)

The naming function changes to incorporate the hub ID into the hash for **all scopes**:

```go
func (b *GCPBackend) gcpSecretName(name, scope, scopeID string) string {
    // Combine hubID with scopeID to ensure uniqueness across hubs
    combined := b.hubID + ":" + scopeID
    hash := sha256.Sum256([]byte(combined))
    shortHash := hex.EncodeToString(hash[:6])
    return sanitizeSecretID(fmt.Sprintf("scion-%s-%s-%s", scope, shortHash, name))
}
```

**Result for hub-scoped secrets:**

```
Hub A (hubID: "a1b2c3d4e5f6"):
  combined = "a1b2c3d4e5f6:a1b2c3d4e5f6"  (hubID is now the scopeID too)
  hash = sha256(combined)[:12]
  → scion-hub-{unique_hash}-GITHUB_APP_PRIVATE_KEY

Hub B (hubID: "f6e5d4c3b2a1"):
  combined = "f6e5d4c3b2a1:f6e5d4c3b2a1"
  hash = sha256(combined)[:12]
  → scion-hub-{different_hash}-GITHUB_APP_PRIVATE_KEY  ← no collision
```

**Result for grove-scoped secrets:**

```
Hub A, Grove X (uuid: "abc-123"):
  combined = "a1b2c3d4e5f6:abc-123"
  → scion-grove-{hash_A}-SECRET_NAME

Hub B, Grove X (same uuid: "abc-123"):
  combined = "f6e5d4c3b2a1:abc-123"
  → scion-grove-{hash_B}-SECRET_NAME  ← no collision
```

### 3.5 GCP Labels for Readability

Add a `scion-hub-hostname` label to all secrets for human filtering in the GCP console:

```go
func buildLabels(input *SetSecretInput, target, hubHostname string) map[string]string {
    labels := map[string]string{
        "scion-scope":        sanitizeLabel(input.Scope),
        "scion-scope-id":     sanitizeLabel(input.ScopeID),
        "scion-type":         sanitizeLabel(input.SecretType),
        "scion-name":         sanitizeLabel(input.Name),
        "scion-target":       sanitizeLabel(target),
        "scion-hub-hostname": sanitizeLabel(hubHostname),  // NEW
    }
    if input.Scope == ScopeUser && input.UserEmail != "" {
        labels["scion-userid"] = sanitizeLabel(input.UserEmail)
    }
    return labels
}
```

The `hubHostname` is the raw hostname (not the hash), truncated/sanitized to fit GCP's 63-char label value limit. This allows operators to filter secrets by hub in the GCP console:

```
gcloud secrets list --filter="labels.scion-hub-hostname=prod-hub-west"
```

### 3.6 Local Backend Consistency

The local backend (`localbackend.go`) stores metadata in the Hub database. It will also adopt the hub ID as the scope ID for hub-scoped secrets, ensuring DB records are consistent regardless of backend:

```go
func (b *LocalBackend) Resolve(ctx context.Context, userID, groveID, brokerID string) ([]SecretWithValue, error) {
    // ...
    scopes = append(scopes, scopeEntry{scope: store.ScopeHub, scopeID: b.hubID})
    // ...
}
```

This means existing DB rows with `scope_id = "hub"` will no longer match queries using the new hub ID. This is acceptable because:
- New deployments start clean.
- Existing deployments adopting this change will need to update their DB rows as part of the future migration tooling.

---

## 4. Affected Files

| File | Change |
|------|--------|
| `pkg/config/hub_config.go` | Add `HubID` field to `HubServerConfig`, add `hubid` to `camelCaseFields`, add `DefaultHubID()` |
| `pkg/store/models.go` | Remove `ScopeIDHub` constant (compile-time break to catch all callers) |
| `pkg/secret/gcpbackend.go` | Add `hubID` field, update `gcpSecretName()` to combine hub ID, update `Resolve()`, update `buildLabels()` |
| `pkg/secret/localbackend.go` | Add `hubID` field, update `Resolve()` |
| `pkg/secret/backend.go` | Pass hub ID through `NewBackend()`, add `HubID()` to interface |
| `pkg/secret/secret.go` | Add `HubID()` to `SecretBackend` interface |
| `pkg/hub/server.go` | Resolve hub ID from config, pass to secret backend, replace `store.ScopeIDHub` references |
| `pkg/hub/handlers_github_app.go` | Replace `store.ScopeIDHub` with `s.secretBackend.HubID()` |
| `pkg/hub/handlers.go` | Replace `store.ScopeIDHub` in scope resolution |
| `pkg/hub/httpdispatcher.go` | Replace `store.ScopeIDHub` in env var listing |
| `cmd/server_foreground.go` | Resolve/log hub ID, pass to secret backend constructor |
| `cmd/hub_secret_migrate.go` | Accept hub ID flag for new naming (future migration work) |
| `pkg/secret/gcpbackend_test.go` | Update tests for new naming, mock hub ID |
| `pkg/hub/resolve_secrets_test.go` | Update test expectations |
| `pkg/hub/handlers_envsecret_authz_test.go` | Update test expectations |

---

## 5. Rollout Considerations

### New Deployments
No special handling. The hub ID is auto-generated on first boot. All secrets are created with the new naming scheme.

### Existing Deployments
- **Old GCP SM secrets remain accessible** via their stored `SecretRef` in the DB (the ref is a full path, not computed from the naming function).
- **New or updated secrets** will be written under the new naming scheme, creating new GCP SM secrets alongside the old ones.
- **`Get()` by name** will attempt the new name, which won't exist for pre-existing secrets. This is the primary migration concern — `Get()` currently computes the name rather than using the stored `SecretRef`.
- **Deferred migration tooling** (out of scope for this change) will handle bulk rename/cleanup.

### Interim Compatibility Strategy
To avoid breaking `Get()` for existing secrets during the transition:
1. `Get()` should first attempt lookup using the stored `SecretRef` from the DB (if present), falling back to computed name only for secrets without a ref.
2. This is already partially the case — the DB stores `SecretRef`, but `Get()` currently ignores it and recomputes the name. Changing `Get()` to prefer the stored ref is a small but important fix that should be included in this refactor.

---

## 6. Future Work (Out of Scope)

- **Admin maintenance UI/CLI** for bulk secret migration between naming schemes. — Addressed by `ptone/scion#2152`; see Section 7.
- **GCS bucket namespacing** for templates and workspaces in shared GCP projects.
- **Hub ID display** in the web UI admin panel for operator reference.
- **Cross-hub secret sharing** — intentionally sharing specific secrets between hub instances.

## 7. Follow-up: `ptone/scion#2152` (hub-prefixed names for IAM scoping)

This refactor made hub-scoped secrets unique per hub (Section 3.1–3.4), but the
per-secret name still only namespaces by hashing `hubID:scopeID` — there is no
single, fixed-length prefix a hub's own secrets all share. That means an IAM
condition can scope a hub's own **hub**-scope secrets (since `scopeID ==
hubID` there), but not its **user**/**project**-scope secrets: the hash
covers `hubID:userID` / `hubID:projectID`, which differs per secret with no
common prefix to write a `startsWith()` condition against. In a GCP project
shared by several hubs, the only workaround was a project-wide
`roles/secretmanager.admin` grant.

`ptone/scion#2152` closes this gap by adding a hub-prefix layer on top of the
naming scheme in Section 3.4, for every scope including hub:

```
{secretNamePrefix}{scope}-{sha256(hubID:scopeID)[:12]}-{name}
secretNamePrefix = "scion-" + sha256(hubID)[:12 hex chars] + "-"
```

`secretNamePrefix` depends on `hubID` alone (not `scopeID`), so it is the same
across every secret a given hub writes, and a least-privilege IAM grant can
use:

```
resource.name.startsWith("projects/<PROJECT_NUMBER>/secrets/scion-<h12>-")
```

Key decisions (superseding anything to the contrary above; see
`/scion-volumes/scratchpad/projects/secret-prefix/formula-decision.md` for the
full record agreed with tf-lead):
- No prefix override or enable/disable setting — the prefix is on by default
  for every hub, computed the same way by Terraform (`substr(sha256(var.hub_id),
  0, 12)`) and by the Go backend, so IAM conditions and secret names never
  drift apart.
- **Backward compatibility**: secrets with a stored DB `SecretRef` keep
  resolving through it unchanged (Section 5's "Interim Compatibility
  Strategy" continues to apply). A DB-less computed-name lookup tries the
  prefixed name first, then the legacy pre-#2152 name, with a `WARN` log on a
  legacy hit.
- **Migration**: `scion hub secret migrate-names [--dry-run] [--delete-legacy]`
  (a new, separate subcommand from the DB-value migration `scion hub secret
  migrate`) copies each legacy-named secret's latest value forward to its
  prefixed name and updates the DB `SecretRef`. It is idempotent and never
  calls a GCP SM listing API — candidates come only from Hub DB records and a
  short list of known hub-scope signing-key names. `--delete-legacy` only
  deletes a legacy secret after verifying the prefixed copy is readable and
  matches. This is a human-mode-only CLI command (`cmd/cli_mode.go`
  `assistantDenied`) — not available to AI-assistant or in-container agent
  callers.
- **Hub-scope signing keys** (`agent_signing_key`, `user_signing_key`, and any
  future key synced via `syncSigningKeyToBackend`) get an additional,
  automatic copy-forward at hub startup (`GCPBackend.CopyHubSecretForward`,
  called from `ensureSigningKey`) rather than waiting for an operator to run
  `migrate-names` — losing a signing key invalidates every live session/agent
  token, so this can't wait on an operator's schedule the way ordinary
  secrets can.
- **Rollback**: rolling back to a pre-#2152 binary continues to resolve
  existing secrets via their stored `SecretRef` (unchanged by this feature
  unless `migrate-names` has run and rewritten it to the prefixed name, in
  which case the pre-#2152 binary still reads the ref value literally and
  works). Legacy secrets are never deleted except by an explicit
  `--delete-legacy` run, so a rollback window before that step is always
  safe. The legacy fallback path itself is intended to be removed a release
  after the migration tooling ships.

See `pkg/secret/gcpbackend.go` (`secretNamePrefix`, `gcpSecretName`,
`legacyGCPSecretName`, `MigrateNameForward`, `CopyHubSecretForward`) and
`cmd/hub_secret_migrate_names.go` for the implementation.
