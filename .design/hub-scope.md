# Design: Hub Scope — Two-Identifier Hub Identity for HA Multi-Node Deployments

**Issue:** [#392](https://github.com/ptone/scion/issues/392)
**Status:** Implemented
**Author:** hs-arch agent (refined by hs-em per user direction)
**Date:** 2026-07-10

## Summary

Introduce a two-identifier hub identity model:

1. **hub_id (Layer-0, immutable):** Machine-readable identity for secret namespacing, telemetry, and infrastructure. Currently defaults to `SHA256(hostname)[:12]` (hex). **Refined:** accept any unique string slug as hub_id (not just hex), keeping hex as the suggested default. Changing hub_id effectively creates a new hub (orphans secrets, etc.).

2. **hub_name (Layer-1, mutable):** A new human-readable display name for the hub. Mutable via admin API, synced across replicas. Used in user-facing UI, log labels, and display contexts. Defaults to `os.Hostname()` for backward compatibility.

Hostname is retained for node-level diagnostics only.

---

## 1. Current State

### 1.1 Hub Identity Model

The hub currently has two identity mechanisms:

| Identifier | Source | Use |
|---|---|---|
| **HubID** | `SHA256(hostname)[:12]` or any explicit string slug | Secret namespacing, telemetry resource attribute |
| **InstanceID** | UUID (or POD_NAME + UUID) | Broker connection affinity, per-process tracking |

Both are set at startup and immutable for the process lifetime.

### 1.2 Hostname Used as Hub Identity

`os.Hostname()` is called directly in 6 locations where it serves as a **hub-level** identifier:

1. **Cloud Logging handler** (`pkg/util/logging/cloud_handler.go:118,189`) — `labels["hub"] = hostname`
2. **GCP log handler** (`pkg/util/logging/gcp_handler.go:60,111-113`) — `labels["hub"] = hostname` and `labels["hostname"] = hostname`
3. **Message log handler** (`pkg/util/logging/message_log.go:118-119`) — `labels["hub"] = hostname`
4. **GCP Secret Manager** (`pkg/secret/gcpbackend.go:173,466-473`) — `scion-hub-hostname` label on secrets
5. **Chat app** (`extras/scion-chat-app/cmd/scion-chat-app/main.go:453-462`) — key discovery by hostname label
6. **Telemetry** (`pkg/observability/hubmetrics/hubmetrics.go:89-93`) — `scion.hub.id` defaults from HubID which defaults from hostname

### 1.3 The HA Problem

In a multi-node deployment behind a load balancer:
- Each node has a different `os.Hostname()` (e.g., `hub-7f8b4-abc`, `hub-7f8b4-def`)
- Log labels diverge: filtering by `hub=hub-7f8b4-abc` misses logs from `hub-7f8b4-def`
- GCP secrets get different `scion-hub-hostname` labels depending on which node created them
- HubID diverges unless explicitly configured the same on all nodes
- Telemetry metrics scatter across different `scion.hub.id` resource attribute values

### 1.4 Hostname Used Correctly (Node-Level)

These uses are **node-level** and should remain unchanged:
- Broker join request hostname (`pkg/hubclient/runtime_brokers.go:221`)
- Broker status display (`cmd/broker.go:1418`)
- Broker name default (`cmd/project.go:389`, `pkg/hubsync/sync.go:1236`)
- Apple DNS configuration (`pkg/hub/system_handlers.go:1006`)

---

## 2. Hub Name Mechanism

### 2.1 Definition

Introduce `hub_name` — a stable, human-readable identifier for a logical hub.

```yaml
# settings.yaml
server:
  hub:
    hub_name: "production"
```

### 2.2 Properties

| Property | Value |
|---|---|
| **Type** | string |
| **Constraints** | Lowercase alphanumeric + hyphens, 1-63 chars, must start with a letter, must not end with a hyphen (DNS-label-safe) |
| **Default** | `os.Hostname()` (backward compatible with current behavior) |
| **Uniqueness** | Not enforced — operator responsibility (like `hub_id`) |
| **Immutability** | Mutable via admin API (Layer-1), but changes should be rare |

### 2.3 Naming Constraints Rationale

DNS-label rules (RFC 1123) are chosen because the hub name may appear in:
- GCP resource labels (max 63 chars, lowercase alphanumeric + hyphens)
- Cloud Monitoring metric labels (same constraints)
- Log filter queries (special characters complicate filtering)
- URLs in future federation scenarios

### 2.4 Resolution Order

```
env var (SCION_SERVER_HUB_HUB_NAME) > DB (hub_settings.endpoints) > settings.yaml > os.Hostname()
```

This follows the standard Layer-1 precedence from the settings-db design (§3.4).

---

## 2b. Hub ID Flexibility (User Refinement)

### 2b.1 Current State

`hub_id` currently defaults to `SHA256(hostname)[:12]` — a 12-character hex string. The `DefaultHubID()` function in `hub_config.go` enforces this format.

### 2b.2 Refinement

Allow `hub_id` to be set as **any unique string slug**, not just hex. The hex value remains the **suggested default** when no explicit value is provided, but operators can set descriptive slugs like `"prod-hub-west"` or `"staging"`.

### 2b.3 Changes Required

1. **Remove hex-only assumption:** `DefaultHubID()` continues to return hex, but explicit values are no longer validated as hex
2. **Validation:** Apply the same DNS-label-safe constraints as `hub_name` (lowercase alphanumeric + hyphens, 1-63 chars) to ensure compatibility with GCP labels and secret namespacing
3. **Schema:** Update the `hub_id` schema entry to reflect the relaxed format constraint
4. **Documentation:** Note that changing `hub_id` orphans existing secrets and effectively creates a new hub

### 2b.4 Immutability Note

`hub_id` remains Layer-0 (bootstrap). It cannot be changed via admin API. Changing it in config/env and restarting effectively creates a new logical hub — existing GCP secrets namespaced under the old hub_id become orphaned.

---

## 3. Settings Integration

### 3.1 Layer Classification

| Setting | Layer | Rationale |
|---|---|---|
| `hub_id` | **Layer-0** (unchanged) | Needed for secret backend init before DB. Machine-readable. Now accepts any string slug (not just hex). |
| `hub_name` | **Layer-1** (new) | Human-readable display identifier. Not needed at bootstrap. Can be synced across replicas via DB. |

### 3.2 Why Layer-1 for hub_name

The `hub_name` is used in:
- Log labels (set after handlers are created, can be hot-reloaded)
- Telemetry attributes (set after OTel provider init — could be plumbed)
- GCP Secret Manager labels (only on write, not on read/key derivation)
- Admin UI (runtime display)
- API responses (runtime)

None of these are in the bootstrap path. The one concern — log handler initialization — can be addressed by:
1. Initializing log handlers with `os.Hostname()` as default
2. Overwriting the hub label once DB-sourced `hub_name` is resolved
3. This means a brief window at startup where logs carry hostname instead of hub_name — acceptable since the hub is already starting up

### 3.3 Operational Settings Registration

Add `hub_name` to the existing `endpoints` section:

```go
// pkg/config/opsettings/registry.go
{
    Name:       "endpoints",
    KoanfPaths: []string{"server.hub.public_url", "server.hub.hub_name", "image_registry"},
    New:        func() any { return &EndpointsSettings{} },
},
```

```go
// pkg/config/opsettings/sections.go
type EndpointsSettings struct {
    PublicURL     string `json:"public_url,omitempty"`
    HubName       string `json:"hub_name,omitempty"`
    ImageRegistry string `json:"image_registry,omitempty"`
}
```

### 3.4 Config Structs

```go
// pkg/config/settings_v1.go — V1ServerHubConfig
type V1ServerHubConfig struct {
    // ... existing fields ...
    HubName  string `json:"hub_name,omitempty" yaml:"hub_name,omitempty" koanf:"hub_name"`
}
```

```go
// pkg/config/hub_config.go — HubServerConfig
type HubServerConfig struct {
    // ... existing fields ...
    HubName string `json:"hubName" yaml:"hubName" koanf:"hubName"`
}
```

### 3.5 Environment Variable

```
SCION_SERVER_HUB_HUBNAME → server.hub.hubName
```

Added to `envKeyToConfigKey` in `hub_config.go`.

### 3.6 Layer1Snapshot Extension

```go
// pkg/hub/operational_settings.go — Layer1Snapshot
type Layer1Snapshot struct {
    // ... existing fields ...

    // Endpoints
    PublicURL     string
    HubName       string  // NEW
    ImageRegistry string
}
```

### 3.7 Schema Extension

Add to `settings-v1.schema.json` under `server.hub`:

```json
"hub_name": {
    "type": "string",
    "pattern": "^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$",
    "description": "Human-readable hub identifier for HA deployments. All nodes in a cluster should share the same hub_name.",
    "x-env-var": "SCION_SERVER_HUB_HUBNAME",
    "x-scope": "global"
}
```

---

## 4. Migration Strategy

### 4.1 Backward Compatibility (Single-Node)

When `hub_name` is not explicitly set:
- Default to `os.Hostname()` (matching current behavior)
- Existing single-node deployments see no change in log labels or metrics
- No migration action required

### 4.2 HA Deployments

Operators setting up HA should:
1. Choose a hub name (e.g., `"production"`, `"staging"`, `"dev-hub"`)
2. Set it via one of:
   - Admin API: `PUT /api/v1/admin/server-config` with `{"hub_name": "production"}` in the endpoints section
   - Environment variable: `SCION_SERVER_HUB_HUBNAME=production` (on all nodes)
   - Settings file: `server.hub.hub_name: production` in `settings.yaml`

In Postgres mode, the DB value propagates automatically to all replicas. In file mode, operators must configure each node.

### 4.3 hub_id Alignment

Operators deploying HA should set `hub_id` explicitly to the same value on all nodes (it's Layer-0, so must be in config/env). With the relaxed format, operators can now use descriptive slugs like `"prod-hub"` instead of opaque hex strings. If not set, the default remains `SHA256(hostname)[:12]`.

**Important:** Changing `hub_id` orphans existing secrets. In HA deployments, all nodes must share the same `hub_id`.

### 4.4 GCP Secret Label Migration

Existing secrets in GCP Secret Manager have `scion-hub-hostname` labels with per-node hostnames. After adopting `hub_name`:
- New secrets get `scion-hub-name` (the hub_name) instead of `scion-hub-hostname`
- Existing secrets retain their old labels (immutable after creation in GCP SM)
- No automated migration needed for the hub itself — labels are informational, not used for lookup (secret names use HubID for scoping)
- Operators can re-label via GCP console/API if desired

**Exception: Chat App.** The chat app (`extras/scion-chat-app`) uses label-based discovery via `ListSecrets` with a filter to auto-discover the hub signing key. Unlike the hub, which looks up secrets by computed name, the chat app queries `labels.scion-hub-name=X`. To maintain backward compatibility with secrets created under the old `scion-hub-hostname` label, the chat app uses an OR filter: `(labels.scion-hub-name=X OR labels.scion-hub-hostname=X)`. This ensures discovery works for both pre-upgrade and post-upgrade secrets without requiring manual relabeling.

---

## 5. Surface Areas — Detailed Change Plan

### 5.1 Logging Labels

**Files:**
- `pkg/util/logging/cloud_handler.go`
- `pkg/util/logging/gcp_handler.go`
- `pkg/util/logging/message_log.go`

**Change:** Replace `hostname` field with `hubName` field. The `"hub"` label emits `hub_name` instead of hostname. Add a separate `"node"` label with hostname for node-level identification.

```go
// Before:
labels["hub"] = h.hostname

// After:
labels["hub"] = h.hubName
labels["node"] = h.hostname  // new: node-level identity
```

**Plumbing:** The log handlers are initialized in `cmd/server_foreground.go`. Add a `hubName` parameter. At startup, use `os.Hostname()` as default; once `Layer1Snapshot` is available (after DB init), update the hub name.

Since log handlers are immutable once created (slog.Handler is an interface without setters), the hub name must be resolved before handler creation. In Postgres mode, the startup sequence is:
1. Load config → resolve `hub_name` from file/env
2. Connect to DB → load Layer-1 settings → may override `hub_name`
3. Create log handlers with resolved `hub_name`

This works because log handlers are created AFTER DB init in `cmd/server_foreground.go`. For the brief early logging before handlers are created, Go's default logger uses stderr with no hub label — acceptable.

### 5.2 Telemetry — OTel Resource Attributes

**File:** `pkg/observability/hubmetrics/hubmetrics.go`

**Change:** Add `scion.hub.name` resource attribute alongside existing `scion.hub.id`.

```go
if o.hubName != "" {
    resAttrs = append(resAttrs, attribute.String("scion.hub.name", o.hubName))
}
```

Add `WithHubName(name string) Option` function.

### 5.3 GCP Secret Manager Labels

**File:** `pkg/secret/gcpbackend.go`

**Change:** Replace `scion-hub-hostname` label with `scion-hub-name`. Pass hub_name into the backend instead of calling `os.Hostname()`.

```go
// Before:
"scion-hub-hostname": sanitizeLabel(hubHostname),

// After:
"scion-hub-name": sanitizeLabel(hubName),
```

The optional `scion-hub-node` label for node-level identity on secrets was deferred — secret labels are informational and node identity is not needed for secret filtering.

Plumb `hubName` into `GCPBackend` via constructor or setter.

### 5.4 Admin UI

The admin settings UI already displays hub configuration. `hub_name` should appear in:
- Server info panel (alongside hub_id)
- Admin settings "endpoints" section (editable)

The web frontend already renders Layer-1 sections from the admin settings API response. No custom UI work needed — the field will appear automatically when added to the endpoints schema.

### 5.5 API Responses

**Hub info endpoint:** Add `hub_name` to the response of any system info or health endpoints. The existing `hub_id` stays.

**Agent metadata:** When agents receive hub info (via dispatch or status endpoints), include `hub_name` for display purposes.

### 5.6 ServerConfig Propagation

**File:** `pkg/hub/server.go`

Add `HubName string` to `ServerConfig`. Wire it through `ApplySnapshot()` in `operational_settings.go`:

```go
// In ApplySnapshot:
if snap.HubName != "" {
    s.config.HubName = snap.HubName
    applied = append(applied, "hub_name")
}
```

### 5.7 Chat App (Extras)

**File:** `extras/scion-chat-app/cmd/scion-chat-app/main.go`

Replace `scion-hub-hostname` label matching with `scion-hub-name`. This is a follow-up item — the chat app is in `extras/` and may lag behind core changes.

---

## 6. Phased Implementation Plan

### Phase 1: Config Foundation (No Behavior Change)

**Goal:** Add `hub_name` to config structs, schema, and settings. Update `hub_id` to accept any unique string slug. No runtime behavior changes.

**Changes:**
1. Add `HubName` field to `V1ServerHubConfig` (`settings_v1.go`)
2. Add `HubName` field to `HubServerConfig` (`hub_config.go`)
3. Add `ResolveHubName()` method that returns HubName or falls back to `os.Hostname()`
4. Add `hub_name` to settings-v1 JSON schema
5. Add `hubname` to `envKeyToConfigKey` mapping
6. Add `hub_name` to the `endpoints` opsettings section
7. Add `HubName` to `Layer1Snapshot`
8. Add `hub_name` to `ConvertV1ServerToGlobalConfig` and `ConvertGlobalConfigToV1Server`
9. Wire `hub_name` through `buildSnapshotFromKoanf`
10. Update `hub_id` schema to accept any string slug (relax hex-only constraint)
11. Update `hub_id` documentation/comments to reflect slug flexibility

**Tests:** Config loading, V1 conversion, schema validation, opsettings section registration.

### Phase 2: Server Plumbing

**Goal:** Make hub_name available throughout the hub server.

**Changes:**
1. Add `HubName string` to `hub.ServerConfig`
2. Set `HubName` in `cmd/server_foreground.go` from resolved config
3. Add `HubName()` accessor to `hub.Server`
4. Wire through `ApplySnapshot()` for runtime updates
5. Add `WithHubName()` option to `hubmetrics`
6. Pass `hubName` to secret backend constructor

**Tests:** Server initialization, snapshot application.

### Phase 3: Surface Area Migration

**Goal:** Replace hostname with hub_name in all hub-level identity sites.

**Changes:**
1. **Logging handlers** — replace `hostname` field with `hubName`, add `hostname` as `"node"` label
2. **Telemetry** — add `scion.hub.name` resource attribute
3. **GCP Secret Manager** — replace `scion-hub-hostname` with `scion-hub-name` label
4. **Admin API** — include `hub_name` in server info responses

**Tests:** Log label verification, telemetry attribute verification, GCP label verification.

### Phase 4: Documentation and Chat App

**Goal:** Document the migration path and update extras.

**Changes:**
1. Update HA deployment documentation
2. Add `hub_name` to workstation onboarding wizard defaults
3. Update chat app to use `scion-hub-name` label (extras)
4. Add migration guide for existing HA deployments

---

## 7. Open Questions

### 7.1 Layer-0 vs Layer-1 for hub_name (RAISED WITH USER)

**Recommendation:** Layer-1 (operational). The hub_name is not needed in the bootstrap path. It's used for display/correlation contexts that are all initialized after DB is available. See §3.2 for analysis.

**Risk:** If a future feature needs hub_name before DB init, it would need to fall back to config/env — which the Layer-1 precedence chain already supports.

### 7.2 Naming: hub_name vs hub_slug (RAISED WITH USER)

**Recommendation:** `hub_name` — more natural in YAML config, familiar pattern from `broker_nickname`. Apply slug-like constraints (DNS-label-safe) to the value.

### 7.3 Should hub_id Auto-Derive from hub_name?

Currently `hub_id` defaults to `SHA256(hostname)[:12]`. If `hub_name` is set, should `hub_id` default to `SHA256(hub_name)[:12]` instead?

**Decision:** No. Keep them independent. hub_id is now flexible (accepts any slug), so operators can choose a descriptive value directly. Auto-deriving would create dangerous coupling where changing `hub_name` (Layer-1, easy) silently changes `hub_id` (Layer-0, hard), orphaning secrets.

### 7.4 Log Handler Hot-Reload

slog.Handler instances are immutable. If `hub_name` changes via admin API (Layer-1), existing log handlers continue emitting the old name until the process restarts.

**Recommendation:** Accept this limitation. `hub_name` changes are rare. Document that log label updates require a rolling restart. A future enhancement could use an atomic string pointer for the hub label, but this adds complexity for a rare operation.

### 7.5 Relationship to Node Registry (#386)

Issue #386 proposes a node-level diagnostics endpoint with a node registry and heartbeat. The `"node"` label added in Phase 3 complements this — each node's hostname provides node-level identity while `hub_name` provides hub-level identity.

**Recommendation:** Implement #392 (this design) first. The `"node"` label in logs provides immediate value. #386's node registry can build on this later.

### 7.6 Endpoint Section vs New Section

Should `hub_name` go in the existing `endpoints` section or get its own `identity` section?

**Recommendation:** Use `endpoints`. The field is small, related to hub surface area, and adding a new section for one field adds unnecessary complexity. If hub identity grows (e.g., hub description, hub icon), a dedicated section can be created later.
