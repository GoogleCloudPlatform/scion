# Design: Broker Label & Type Display

**Project:** broker-label  
**Status:** Draft  
**Author:** Architect Agent  
**Date:** 2026-07-10

## Problem & Goals

The `/brokers` page displays the co-located Cloud Run broker with an opaque hostname (the Cloud Run instance ID, e.g., `instance-abc123def`). Users cannot distinguish between hosted (hub-embedded) and external (remote) brokers. The broker has a stable logical UUID already, but its `Name` field stores the ephemeral hostname.

### Success Criteria

1. The co-located broker displays as **"Hosted Broker"** instead of the opaque instance ID.
2. Both the brokers list and broker detail pages show a **type badge** ("Hosted" or "External") derived from structured labels.
3. External brokers registered via the join flow are auto-labeled as "external" (overridable).
4. The vestigial `type` column on the `RuntimeBroker` ent schema is removed.
5. No data loss or behavioral regression for existing deployments — existing brokers are updated on next startup/reconnect.

## Non-Goals

- **Runtime profile display**: The broker's runtime profiles (cloudrun, docker, kubernetes, GKE, apple) are already tracked in the `Profiles` field. This design does not add platform-level labels — "Hosted" is the hosting model, not the platform.
- **Broker renaming UI**: We are not adding a UI to let users rename brokers. The name is set by the system (or by the registration request for external brokers).
- **Label management UI**: We are not building a general-purpose label editor. Labels are set programmatically or via the existing PATCH API.
- **Multi-broker-per-hub**: There is exactly one hosted broker per hub control plane (even in multi-node HA). This design does not address hypothetical multi-broker scenarios.

## Design Decisions

These decisions were confirmed with the project owner:

| # | Question | Decision | Rationale |
|---|----------|----------|-----------|
| 1 | Display label text | **"Hosted"** for co-located brokers, **"External"** for remote brokers | Generic enough for future hosting models. Platform detail (Cloud Run, GKE) is already in Profiles. |
| 2 | Storage mechanism | **Labels map** (`scion.io/broker-type`) | Follows existing K8s-style label conventions in the codebase. More extensible than a fixed column. |
| 3 | Broker name for Cloud Run | **"Hosted Broker"** | Stable, human-readable, unique per hub control plane. Replaces opaque `os.Hostname()` fallback. |
| 4 | External broker labels | **Auto-default to "external"**, allow override via registration request | `CreateBrokerRegistrationRequest` already has a `Labels` field. Default gives consistent UX; users can override. |

## Proposed Design

### Architecture Overview

```
Server Startup (co-located broker)
  ├── resolveBrokerName() → adds Cloud Run / co-located fallback → "Hosted Broker"
  └── registerGlobalProjectAndBroker()
        └── Sets Labels: {"scion.io/broker-type": "hosted"}
        └── Creates/updates broker in DB

External Broker Registration (join flow)
  └── CreateBrokerRegistration()
        └── If no "scion.io/broker-type" in request labels → defaults to "external"
        └── Creates broker in DB with labels

API: GET /api/v1/runtime-brokers
  └── Returns store.RuntimeBroker (labels already serialized)

Frontend
  ├── brokers.ts → reads broker.labels["scion.io/broker-type"] → renders badge
  └── broker-detail.ts → same label → renders badge in header
```

### Label Convention

**Key:** `scion.io/broker-type`  
**Values:** `"hosted"` | `"external"`

This follows the existing convention in the codebase where the global project uses `"scion.io/system": "true"` and `"scion.io/global": "true"`.

### Backend Changes

#### 1. `cmd/server_foreground.go` — `resolveBrokerName()`

Add a new early fallback for the co-located/stateless Cloud Run case. The function currently has this priority chain:

```
1. vsBroker.BrokerNickname
2. vsBroker.BrokerName
3. settings.Hub.BrokerNickname
4. cfg.RuntimeBroker.BrokerName
5. os.Hostname()        ← fires on Cloud Run
6. "runtime-broker"
```

**Change:** The caller in the co-located registration flow (around line 1784) already knows whether this is a co-located broker. Rather than modifying `resolveBrokerName()` itself (which is also used for non-co-located scenarios), add a **post-resolution override** in the caller:

```go
// Pseudocode — after resolveBrokerName() returns:
brokerName := resolveBrokerName(cfg, brokerSettings, vsBroker)

// If no explicit name was configured and this is a co-located broker,
// use a stable human-readable name instead of the hostname fallback.
if colocatedBroker && brokerName == hostname {
    brokerName = "Hosted Broker"
}
```

The check `brokerName == hostname` ensures that if a user has explicitly configured a broker nickname via settings, that configured name is respected. Only the `os.Hostname()` fallback is overridden.

**Alternative considered:** Adding a new priority level inside `resolveBrokerName()`. Rejected because `resolveBrokerName` is a general-purpose function used in non-co-located contexts too; adding co-located awareness there would leak deployment-model concerns into a utility function.

#### 2. `cmd/server_broker.go` — `registerGlobalProjectAndBroker()`

In the new-broker creation path (around line 91), add labels:

```go
// Pseudocode — when creating a new broker:
broker := &store.RuntimeBroker{
    // ... existing fields ...
    Labels: map[string]string{
        "scion.io/broker-type": "hosted",
    },
}
```

In the existing-broker update path (around line 112), ensure labels are set on update as well, so that existing brokers from pre-label deployments get backfilled on next startup:

```go
// Pseudocode — when updating an existing broker:
if broker.Labels == nil {
    broker.Labels = make(map[string]string)
}
broker.Labels["scion.io/broker-type"] = "hosted"
// ... then call s.UpdateRuntimeBroker(ctx, broker)
```

**Note on the function signature:** `registerGlobalProjectAndBroker` currently takes scalar parameters (`brokerID, brokerName, endpoint string`). The labels are hardcoded inside the function for the co-located case — no signature change needed.

#### 3. `pkg/hub/brokerauth.go` — `CreateBrokerRegistration()`

In the external broker registration handler (around line 250, after validation and before `s.CreateRuntimeBroker`), auto-default the broker-type label if not provided:

```go
// Pseudocode:
if req.Labels == nil {
    req.Labels = make(map[string]string)
}
if _, exists := req.Labels["scion.io/broker-type"]; !exists {
    req.Labels["scion.io/broker-type"] = "external"
}
// ... then use req.Labels when creating the broker
```

This respects user-provided labels (option C from the design discussion) while ensuring a consistent default.

#### 4. `pkg/ent/schema/runtimebroker.go` — Remove vestigial `type` field

Remove the `type` field from the schema:

```go
// REMOVE this line:
field.String("type").Optional(),
```

This will cause `ent generate` to produce a migration that drops the `type` column. Since the column is `Optional()`, was never written by any code path, and is never read, dropping it is safe.

**Migration note:** The generated migration must be tested against both SQLite (tests) and Postgres (production). The column drop is a non-destructive operation (no data loss — the column was always NULL).

#### 5. `pkg/store/entadapter/project_store.go` — Adapter cleanup

The `entBrokerToStore()` function may reference the `type` field from the ent model. If so, remove that reference. Similarly, check `CreateRuntimeBroker()` and `UpdateRuntimeBroker()` for any `SetType()` calls (unlikely since the field was never written, but verify).

### Frontend Changes

#### 6. `web/src/shared/types.ts` — Add `labels` to `RuntimeBroker`

```typescript
export interface RuntimeBroker {
  // ... existing fields ...
  labels?: Record<string, string>;  // ADD
}
```

The API already returns labels in the JSON response (via `store.RuntimeBroker` → `RuntimeBrokerWithCapabilities` marshaling). The frontend type just needs to declare the field.

#### 7. `web/src/components/pages/brokers.ts` — Add type badge

**Grid view** (`renderBrokerCard`, around line 318): Add a type badge next to or below the broker name:

```typescript
// Pseudocode:
const brokerType = broker.labels?.['scion.io/broker-type'];
// Render a badge/tag: "Hosted" or "External" (or nothing if unlabeled)
```

The badge should use existing tag/badge component patterns in the codebase. Suggested styling:
- **"Hosted"** → a subdued/info-style badge (e.g., blue or gray)
- **"External"** → a neutral badge

**Table view** (`renderBrokerRow`, around line 397): Add a "Type" column to the table, or append the badge inline with the name.

#### 8. `web/src/components/pages/broker-detail.ts` — Show type in header

In the broker detail header (around line 594), add the type badge next to the broker name or in the subtitle line alongside version and endpoint.

### Data Flow Summary

| Scenario | Name | Labels | Display |
|----------|------|--------|---------|
| Co-located broker (Cloud Run) — new | "Hosted Broker" | `{scion.io/broker-type: hosted}` | Name: "Hosted Broker", Badge: "Hosted" |
| Co-located broker — existing (upgrade) | Updated to "Hosted Broker" on restart | Backfilled to `{...: hosted}` on restart | Same as above after restart |
| External broker — new (no labels in request) | User-provided name | `{scion.io/broker-type: external}` (auto-defaulted) | Name: user's name, Badge: "External" |
| External broker — new (with labels) | User-provided name | User-provided labels (may override broker-type) | Name: user's name, Badge: from labels |
| External broker — existing (pre-label) | Unchanged | Empty (until user patches or re-registers) | Name: existing, Badge: none |

## Alternatives Considered

### A1: Repurpose the vestigial `type` column

Store the broker type ("hosted"/"external") directly in the existing `type` column on `RuntimeBroker`.

**Pros:** Simpler — direct string field, no JSON serialization, already exists in schema.  
**Rejected because:** Changes the semantics of an existing column (even if unused). Less extensible — adding more metadata later would require more schema changes. Doesn't follow the K8s-style label convention already established in the codebase.

### A2: Add a new dedicated `broker_type` column

Add a new, properly-named column to the ent schema.

**Pros:** Type-safe, queryable, clear semantics.  
**Rejected because:** Requires a DB migration for a new column. Adds schema surface area for a single metadata value that labels can already handle. The Labels infrastructure is already fully wired end-to-end.

### A3: Infer type from `AutoProvide` flag in the frontend

The `autoProvide: true` flag already distinguishes hosted from external brokers at the data level. The frontend could use this as a heuristic.

**Pros:** Zero backend changes.  
**Rejected because:** `AutoProvide` is a provisioning behavior flag, not a type indicator. Its semantics could change independently. An explicit label is more intentional and self-documenting.

## Migration / Rollout

### Backward Compatibility

- **Existing co-located brokers:** On next server restart, the update path in `registerGlobalProjectAndBroker()` will backfill both the name ("Hosted Broker") and labels (`scion.io/broker-type: hosted`). No manual intervention needed.
- **Existing external brokers:** Labels remain empty until the user patches them or re-registers. The frontend should handle missing labels gracefully (no badge, not a crash).
- **API consumers:** The `labels` field is already returned by the API. Adding values to it is additive — no breaking change.
- **Vestigial `type` column removal:** The generated ent migration drops the column. Since no code reads or writes it, this is safe. The migration should be verified in both SQLite and Postgres environments.

### Rollout Order

The changes should land in a single commit since they're small and tightly coupled. However, the logical order of development is:

1. Schema change (remove `type` field) + generate ent code + migration
2. Backend: broker name override + label population
3. Frontend: type declaration + badge rendering

## Open Questions

None — all design questions have been resolved with the project owner.

## Implementation Phases

This is an XS project. All changes can land in a single commit, but the developer should work in this order:

### Phase 1: Backend — Schema & Registration (single commit)

**Files to modify:**

1. **`pkg/ent/schema/runtimebroker.go`**
   - Remove the `field.String("type").Optional()` line
   - Run `go generate ./pkg/ent/...` to regenerate ent code
   - Generate and verify the migration (column drop)

2. **`pkg/store/entadapter/project_store.go`**
   - Remove any references to the `type` field in `entBrokerToStore()`, `CreateRuntimeBroker()`, `UpdateRuntimeBroker()` (verify — may not exist since the field was never written)

3. **`cmd/server_foreground.go`**
   - In the co-located broker registration block (around line 1784): after `resolveBrokerName()` returns, if the result equals `os.Hostname()` and this is a co-located broker, override to `"Hosted Broker"`

4. **`cmd/server_broker.go`**
   - In `registerGlobalProjectAndBroker()` — new broker creation: add `Labels: map[string]string{"scion.io/broker-type": "hosted"}`
   - In `registerGlobalProjectAndBroker()` — existing broker update: backfill the label if missing

5. **`pkg/hub/brokerauth.go`**
   - In `CreateBrokerRegistration()`: default `scion.io/broker-type` to `"external"` if not in request labels

### Phase 2: Frontend — Type Display (same or next commit)

**Files to modify:**

6. **`web/src/shared/types.ts`**
   - Add `labels?: Record<string, string>` to `RuntimeBroker` interface

7. **`web/src/components/pages/brokers.ts`**
   - Grid view (`renderBrokerCard`): add type badge derived from `broker.labels?.["scion.io/broker-type"]`
   - Table view (`renderBrokerRow`): add type badge (inline with name or new column)

8. **`web/src/components/pages/broker-detail.ts`**
   - Header area: add type badge next to broker name

### Phase 3: Verify

- Run ent generation and verify migration
- Run existing tests (`go test ./cmd/... ./pkg/...`)
- Manual verification of the brokers page with a co-located broker

## Acceptance Criteria

1. **Co-located broker name**: On a fresh deployment or restart, the co-located broker's `name` in the DB is `"Hosted Broker"` (not the hostname), unless the user has configured an explicit broker nickname.
2. **Co-located broker label**: The co-located broker has `labels: {"scion.io/broker-type": "hosted"}` in the DB.
3. **External broker label**: A newly registered external broker (via join flow) has `labels: {"scion.io/broker-type": "external"}` unless the registration request provided a different value for that key.
4. **Frontend badge — list**: The brokers list page shows a "Hosted" or "External" badge/tag for each broker (in both grid and table views).
5. **Frontend badge — detail**: The broker detail page shows the type badge in the header area.
6. **Vestigial column removed**: The `type` field is no longer in `pkg/ent/schema/runtimebroker.go` and the generated ent code does not reference it.
7. **Upgrade path**: An existing broker from a pre-label deployment gets its name and labels updated on next server restart (no manual intervention).
8. **No badge for unlabeled brokers**: Brokers without `scion.io/broker-type` in their labels show no type badge (graceful degradation, no crash).
9. **Configured name respected**: If a user has configured `server.broker.broker_nickname` or similar, that name is used instead of "Hosted Broker" — the override only replaces the `os.Hostname()` fallback.
10. **Tests pass**: All existing Go tests and any frontend build checks pass.
