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

Key decisions (superseding anything to the contrary above; formula agreed
with the Terraform module owner so the IAM condition and the Go naming
formula can never drift apart):
- No prefix override or enable/disable setting — the prefix is on by default
  for every hub, computed the same way by Terraform (`substr(sha256(var.hub_id),
  0, 12)`) and by the Go backend.
- **Backward compatibility**: secrets with a stored DB `SecretRef` keep
  resolving through it unchanged (Section 5's "Interim Compatibility
  Strategy" continues to apply). A DB-less computed-name lookup tries the
  prefixed name first, then the legacy pre-ptone/scion#2152 name, with a
  `WARN` log on a legacy hit.
- **Migration**: `scion hub secret migrate-names [--dry-run] [--delete-legacy]
  [--hub-id] [--timeout] [--config]` (a new, separate subcommand from the DB-value
  migration `scion hub secret migrate`) independently checks, per secret
  identity: whether it needs copying forward, whether its DB `SecretRef`
  needs repairing to the prefixed name (which can be true even without a
  copy this run — see the signing-key bullet below, and see "Resync and
  concurrency" below for what "needs repairing" actually means once a
  prefixed copy already exists), and — only once the ref already designates
  the prefixed name — whether `--delete-legacy` should remove the legacy
  copy. Doing these as three separately-triggered checks rather than one
  early-exit chain is what makes the documented two-step workflow (a plain
  run, then a later `--delete-legacy` run) actually reach the delete step on
  the second invocation. It is idempotent and never calls a GCP SM listing
  API — candidates come only from Hub DB records and a fixed list of known
  hub-scope signing-key names (`agent_signing_key`, `user_signing_key`,
  `oidc_signing_key`, `download_signing_key`).
  - **Hub ID resolution**: `migrate-names` resolves its hub ID exactly the
    way the running hub server does at startup (`--hub-id`, then settings
    `server.hub.hub_id` as loaded from `--config` (or the default settings
    file if `--config` is omitted), then the server's own environment/hostname
    fallback `HubServerConfig.ResolveHubID()`) — never a parallel resolution
    path — and prints the resolved ID and the prefix it produces before
    acting. Pass `--config` when the hub server runs with a non-default
    settings file, or the two can disagree on `hub_id` even though both look
    correct in isolation. It
    also refuses to proceed if the resolved ID disagrees with an existing
    hub-scope secret record's `ScopeID`, unless `--hub-id` was passed
    explicitly. Under `--dry-run`, the environment/hostname fallback is
    followed only when it can be without writing `~/.scion/hub-id` for the
    first time (`config.ResolveHubIDFromEnvReadOnly`); otherwise `--dry-run`
    fails closed and asks for `--hub-id` rather than derive a value that
    might not match what a real run would persist.
  - **Outcomes**: a candidate with a DB record whose ref isn't yet the
    prefixed name is classified as MIGRATE (prefixed copy created),
    RESYNC (prefixed copy existed but was stale — see below), REPAIR REF
    (prefixed copy already correct, only the ref moved), a skip with no
    output (no ref, and no value under the legacy name either — truly
    nothing to migrate), or a visible ORPHAN (a *stored* ref whose
    designated value is gone — reported for operator awareness, but not a
    failure: there is nothing to copy). An unreadable *stored* ref (e.g.
    `PermissionDenied` on a legacy name a record's ref still depends on) is
    a real, counted failure, and so is CONFLICT (a concurrent writer's ref
    update raced with this run's own write to the prefixed name — see
    "Resync and concurrency" below for the precise guarantee and what to do
    about it). **Known inconsistency, tracked in ptone/scion#2254:** a DB
    record with *no* stored ref at all (not yet migrated by `hub secret
    migrate`, or one with a non-`gcpsm:` ref) is treated identically to a
    no-DB-record candidate by the copy/resync step (`PermissionDenied` on
    the computed legacy name is absent, not fatal), but as a genuinely
    unmigrated record by `--delete-legacy`'s check (fatal once the legacy
    grant is narrowed). Pinned by `TestR7_Item6_NoStoredRefPermissionDenied`
    in `pkg/secret/gcpbackend_test.go`.
    The two steps disagree for this specific, narrow case; both fail
    closed, so no secret is put at risk, but the failure/skip classification
    differs depending which step encounters it first.
  - `--delete-legacy` deletes a legacy secret once the DB ref (if any)
    already designates the prefixed name and that prefixed copy is
    confirmed readable — it does **not** additionally require the legacy
    and prefixed values to match once a ref-based authority determination
    has been made; see "Resync and concurrency" for why. Value equality is
    used only for the no-DB-record path, where there is no ref to establish
    authority from. `--dry-run` and the real run share the identical
    decision (`canDeleteLegacyName`, via `PlanLegacyDeletion` and
    `DeleteLegacySecretName`), including how each GCP error code from
    reading the legacy name is classified (`legacyReadErrIsFatal`, shared
    with `migrationCheck` and `LegacyStillPresent` — round-4 review finding
    2): `NotFound`/`FailedPrecondition` are never fatal, and
    `PermissionDenied` is fatal only for a record whose ref genuinely still
    depends on reading the legacy name (not yet repaired). For the known
    hub-scope signing keys with no DB record — or any record whose ref
    already designates the prefixed name — a `PermissionDenied` legacy read
    (e.g. after the legacy IAM grant has been removed per the removal
    criterion below) is "nothing further to do," not a failure, in both
    modes.
  - This is a human-mode-only CLI command (`cmd/cli_mode.go`
    `assistantDenied`) — not available to AI-assistant or in-container agent
    callers.
- **Hub-scope signing keys** (`agent_signing_key`, `user_signing_key`,
  `oidc_signing_key`, `download_signing_key`, and any future key synced via
  `syncSigningKeyToBackend`) get an additional, automatic copy-forward at hub
  startup (`GCPBackend.CopyHubSecretForward`, called from `ensureSigningKey`
  and `OIDCKeyManager.loadOrCreateKey`) rather than waiting for an operator
  to run `migrate-names` — losing a signing key invalidates every live
  session/agent token, so this can't wait on an operator's schedule the way
  ordinary secrets can. `CopyHubSecretForward` also repairs the DB
  `SecretRef` to the prefixed name (not just the GCP SM value), and does so
  even when the prefixed copy already existed from an earlier boot or an
  operator's `migrate-names` run — otherwise the hub keeps reading the
  legacy copy through a stale ref indefinitely, silently defeating the
  copy-forward's whole purpose.
- **Resync and concurrency**: a DB record's `SecretRef` is the source of
  truth for which GCP SM copy is currently authoritative. Whenever the ref
  isn't yet the prefixed name, `planOrRepairRef` (behind
  `RepairRefToPrefixed`/`PlanRefRepair`) reads the value the ref actually
  designates and makes the prefixed name carry it *exactly* — creating it if
  missing, or overwriting it if it already exists with a different value —
  before ever repointing the ref. This is what makes it safe to call
  unconditionally at any point, including a mixed-version rolling deploy
  (an old replica still writing through the legacy name after a new replica
  already created a prefixed copy) or a rollback-then-roll-forward window:
  the stale prefixed copy gets resynced to the ref-designated value, not
  silently served or silently treated as "already migrated".
  - GCP Secret Manager has no conditional `AddSecretVersion`, so a
    concurrent write racing between the read of the authoritative value and
    the write to the prefixed name can't be prevented by the GCP SM API
    alone. Immediately before acting — including on the "repaired" path,
    which performs no GCP write at all (round-4 review finding 1(a)) —
    `planOrRepairRef` re-reads the DB record and compares `(SecretRef,
    Version)` against what it read at the start of the attempt; if either
    changed — a concurrent `Set()`, or an old binary re-asserting the *same*
    ref string with a new value (`Version` still bumps on every write, which
    is what catches this ABA case even though the ref text didn't change) —
    the attempt is discarded and retried with fresh reads (bounded,
    currently 3 attempts). The ref-update CAS
    (`store.UpdateSecretRefIfMatches`) is *also* keyed on that same
    `(SecretRef, Version)` pair, not the ref string alone (round-4 review
    finding 1(b)): a rotation landing during the labels/`AddSecretVersion`
    RPCs between the recheck and the CAS still fails the CAS's version
    predicate and is retried, rather than silently applying.
  - **What this guarantees, precisely**: whether a concurrent writer's own
    `AddSecretVersion` to the *prefixed* name is caught depends on **when
    its DB upsert lands relative to this attempt's recheck and CAS**, not
    on when its GCP write landed:
    - If the concurrent writer's DB upsert lands **between this attempt's
      recheck and its CAS** (or is otherwise visible in the DB row by CAS
      time), the CAS's version predicate fails to match. If this attempt
      had already written a version to the prefixed name, that is reported
      as `ErrConflictingWrite` — logged at `WARN` with the secret's identity
      only (no value, no full GCP path) — instead of silently retried
      (`migrate-names` reports this as a `CONFLICT` outcome; see below).
      Otherwise (the "repaired" path, no GCP write yet) the attempt is
      simply retried with fresh reads.
    - If the concurrent writer's DB upsert lands **after this attempt's CAS
      has already applied**, nothing detects it: the CAS already succeeded
      against the values this attempt read, no further recheck or CAS runs,
      and this attempt's now-stale value can be left as the prefixed name's
      latest version — silently, with no error and no log — until the key
      is next written. This is not "brief": nothing in this design repairs
      it on its own, and it can persist indefinitely.
    In this codebase only a new-binary `Set()` (or another replica's boot
    copy-forward) writes the prefixed name directly, so either interleaving
    requires exactly that kind of concurrent write on the same key; two
    cross-system operations (GCP SM and the DB) can't be made a single
    transaction, so neither case can be closed without a new mechanism.
  - **`ErrConflictingWrite` / `CONFLICT` remediation**: when `migrate-names`
    reports a candidate as `CONFLICT` (or hub-boot `CopyHubSecretForward`
    logs the same `WARN`), for a *true* conflict — the ref itself was
    repointed by a concurrent write — **re-running `migrate-names` does not
    detect or repair it**: by then the ref already matches whatever this
    run or the concurrent writer left behind, so a re-run takes the
    "nothing to do" path and the `CONFLICT` simply disappears from the
    report while the stale value remains. The correct remediation for a
    true conflict is to **re-set the secret directly to its intended
    value** through the normal secret-set path, which writes a fresh
    version and moves the ref forward unambiguously. (A `CONFLICT` can also
    be a *false positive*, where a re-run does help — see the "Known false
    positive" bullet below for how to tell the two apart.) For a hub-scope
    signing key specifically, a `CONFLICT` from two replicas of the *same*
    hub booting together and racing on the same key is benign — both wrote
    identical key material, the key is preserved, and startup continues;
    the `WARN` is noise in that case, not a sign of anything lost.
  - A record with a *stored* ref whose designated value is gone (`NotFound`)
    is reported as `ErrOrphanedRef`, distinct from `store.ErrNotFound` (no
    ref and no legacy-name value either — genuinely nothing to migrate).
    `migrate-names` treats both as non-failures, but reports the former as a
    visible ORPHAN line since it reflects a record making a claim that no
    longer holds.
  - **Known false positive, tracked in ptone/scion#2254:** any DB write
    that bumps `Version` without changing `SecretRef` — a metadata-only
    edit (`UpdateSecretMeta`), or an old-binary rotation through a name
    other than the prefixed one — can also make the CAS's version predicate
    fail, and is currently reported as the same `ErrConflictingWrite` /
    `CONFLICT` even though the ref never actually moved, and a plain retry
    (a single re-run of `migrate-names`, or — for the hub-scope signing keys
    only — the hub's next boot) converges to the correct value unless
    another concurrent write races it. This means the "re-set directly, a
    re-run will not help" remediation above is accurate only for a *true*
    conflict (the ref itself was repointed by a concurrent write); for this
    false-positive case a re-run does help. Since nothing here can tell the
    two apart without re-reading the record and checking whether its ref
    has actually become the prefixed ref — a behavior change tracked
    separately rather than made in this PR — the practical guidance is: a
    re-run **without `--delete-legacy`** is always safe to try first, and
    its result distinguishes the two cases — a MIGRATED, RESYNCED or
    REPAIRED REF line for this secret means it was a false positive and is
    now resolved; nothing further to do means it was a true conflict.
    `DELETED LEGACY` / `WOULD DELETE LEGACY` is **not** that signal: once
    the ref already designates the prefixed name (true for both a resolved
    false positive and an unresolved true conflict), `--delete-legacy`
    deletes the legacy copy regardless, so a re-run that includes
    `--delete-legacy` reports that action even for a true conflict, wrongly
    appearing to resolve it. Pinned by
    `TestSPREV6_MetaEditDuringCopyIsFalsePositiveConflict`,
    `TestR7_Item5_MetaEdit_RerunConverges`,
    `TestR7_Item5_OldBinaryRotationFalsePositive_RerunConverges`, and (for
    the `--delete-legacy` carve-out)
    `TestSPREV8_TrueConflictRerunWithDeleteLegacyReportsAction`.
- **Deploy ordering**: grant the new hub-prefixed IAM condition to a hub's
  service account *before* deploying a binary built from this change —
  every `Set` (new secret, new version, signing-key rotation) targets the
  prefixed name immediately, so writes get a permission error otherwise.
  Keep the legacy grant in place until `--delete-legacy` has been run and
  verified; only then remove it. `Delete` deletes the prefixed (in-use) name
  first (always fatal on failure), then treats a `PermissionDenied` on the
  legacy name as non-fatal best-effort cleanup only once the prefixed copy
  is confirmed authoritative (a DB record's ref, if any, already designates
  it) — for a secret that was never migrated, or whose ref still depends on
  the legacy name, any legacy-delete failure (including `PermissionDenied`)
  is fatal, so a delete can never silently drop a DB row while the secret's
  only copy survives untracked in GCP SM.
  - **`--delete-legacy` precondition (round-6 review finding 4):** GCP SM
    has no conditional delete, so `DeleteLegacySecretName`'s check-then-act
    is not atomic with the actual delete call. Run `--delete-legacy` only
    after every replica of this hub is running a binary that includes this
    change — i.e. no replica still writes legacy names. If an older binary
    is still a live writer during a mixed-version rolling deploy, its `Set`
    can land between the check and the delete, and the delete then destroys
    that write's only copy along with the legacy container it lived in.
    This cannot be closed in code without a new mechanism; it is fully
    avoided by the operator precondition above. Once satisfied, a delete
    cannot make a secret unreadable (the check above already guarantees
    that in the single-writer case).
- **Rollback**: rolling back to a pre-ptone/scion#2152 binary continues to
  resolve existing secrets via their stored `SecretRef` (unchanged by this
  feature unless `migrate-names` has run and rewritten it to the prefixed
  name, in which case the older binary still reads the ref value literally
  and works). Legacy secrets are never deleted except by an explicit
  `--delete-legacy` run, so a rollback window before that step is always
  safe for **reads**. Writes are a separate story: an old binary only ever
  writes the legacy name, so if the legacy IAM grant has already been
  removed (per the removal criterion below), rolling back and then calling
  `Set` — or generating a signing key — fails with `PermissionDenied` on a
  rolled-back binary. Restore the legacy grant before rolling back if it was
  already removed (round-5 review non-blocking finding 3). Rolling back,
  writing through the old binary (which always targets
  the legacy name and resets the ref to it), and then rolling forward again
  leaves a *stale* prefixed copy relative to the ref — this is exactly what
  the resync logic above corrects on the next `migrate-names` run or hub
  boot, not a manual "compare values" step for an operator to perform.
  - **Known gap (round-4 review Consider 5, low-impact):** an old binary's
    `Delete` during a rollback window removes the legacy name and the DB
    row, but has no notion of the prefixed name, so it leaves that copy
    behind. After rolling forward again, the no-record `Get` fallback
    (`accessSecretByComputedName`) can find that orphaned prefixed copy and
    serve a secret the user deleted. The blast radius is bounded — listings
    come from the DB, so the orphan is invisible to `scion secret list` and
    only reachable via a direct `Get` for that exact identity — but it is a
    real gap, not fixed here. A future `migrate-names --delete-legacy` run
    does not clean it up either, since there is no DB record to key off of.
- **Removing the legacy fallback**: tracked in ptone/scion#2180, to be done
  one release after `migrate-names` has shipped and been run in production,
  once a fleet-wide `--dry-run` reports zero failures and zero pending
  migrate/repair/resync plan lines. **Check this before removing the legacy
  IAM grant, not after** (round-4 review Consider 6): once the grant is gone,
  `migrate-names` can no longer read legacy names at all, and a clean
  `--dry-run` at that point only proves IAM is narrowed, not that every
  secret was actually migrated first.

See `pkg/secret/gcpbackend.go` (`secretNamePrefix`/`SecretNamePrefixForHubID`,
`gcpSecretName`, `legacyGCPSecretName`, `MigrateNameForward`,
`CopyHubSecretForward`, `RefPointsAtPrefixed`, `LegacyStillPresent`,
`RepairRefToPrefixed`, `PlanRefRepair`, `DeleteLegacySecretName`,
`PlanLegacyDeletion`, `ErrOrphanedRef`), `pkg/store/store.go` /
`pkg/store/entadapter/secret_store.go` (`UpdateSecretRefIfMatches`), and
`cmd/hub_secret_migrate_names.go` for the implementation.
