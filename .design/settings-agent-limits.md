# Default Agent Limits & Resources at Hub and Grove Level

## Status: Proposed

## Problem

Agent limits (max turns, max model calls, max duration) and resource constraints (CPU, memory, disk) can only be set per-agent today via the agent configuration page. There is no way to establish default limits or resources at the grove or hub level, meaning every agent must be individually configured. Administrators and grove owners need a way to set sensible defaults that apply to all agents unless explicitly overridden.

## Goal

Add default agent limits and resource settings to both the hub-level (admin server config) and grove-level (grove settings) configuration surfaces. These defaults are applied when creating/starting agents that don't specify their own values.

## Current State

### Agent-Level Configuration

The agent configure page has a "Limits & Resources" tab with:

**Limits:**
- `max_turns` (int) — maximum conversation turns
- `max_model_calls` (int) — maximum LLM API calls
- `max_duration` (string, Go duration format) — maximum execution time

**Resources:**
- `resources.requests.cpu`, `resources.requests.memory`
- `resources.limits.cpu`, `resources.limits.memory`
- `resources.disk`

These are stored in `api.ScionConfig` and persisted as `AppliedConfig.InlineConfig` on the agent.

### Hub-Level Settings (Admin)

`ServerConfigResponse` / `settings.yaml` currently has top-level defaults for:
- `default_template`
- `default_harness_config`
- `image_registry`

No limit or resource defaults exist.

### Grove-Level Settings

`GroveSettings` (stored as grove annotations) currently supports:
- `defaultTemplate`
- `defaultHarnessConfig`
- `telemetryEnabled`
- `activeProfile`

No limit or resource defaults exist.

### Defaults Resolution Hierarchy

Existing defaults (template, harness config) follow a resolution chain:
1. Agent explicit config (highest priority)
2. Template config
3. Grove defaults (grove annotations)
4. Profile defaults
5. Hub global defaults (`settings.yaml`) (lowest priority)

## Proposed Changes

### 1. Settings Schema (`pkg/config/schemas/settings-v1.schema.json`)

Add top-level default limit and resource fields:

```json
"default_max_turns": {
  "type": "integer",
  "minimum": 1,
  "description": "Default max turns for new agents. Applied when no explicit value is set.",
  "x-scope": "any",
  "x-since": "1"
},
"default_max_model_calls": {
  "type": "integer",
  "minimum": 1,
  "description": "Default max model calls for new agents.",
  "x-scope": "any",
  "x-since": "1"
},
"default_max_duration": {
  "type": "string",
  "description": "Default max duration for new agents (Go duration, e.g. '2h', '30m').",
  "x-scope": "any",
  "x-since": "1"
},
"default_resources": {
  "$ref": "#/$defs/resourceSpec",
  "description": "Default resource requests/limits for new agents.",
  "x-scope": "any",
  "x-since": "1"
}
```

### 2. Go Backend — `VersionedSettings` (`pkg/config/settings_v1.go`)

Add fields to `VersionedSettings`:

```go
DefaultMaxTurns      int              `json:"default_max_turns,omitempty" yaml:"default_max_turns,omitempty" koanf:"default_max_turns"`
DefaultMaxModelCalls int              `json:"default_max_model_calls,omitempty" yaml:"default_max_model_calls,omitempty" koanf:"default_max_model_calls"`
DefaultMaxDuration   string           `json:"default_max_duration,omitempty" yaml:"default_max_duration,omitempty" koanf:"default_max_duration"`
DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty" yaml:"default_resources,omitempty" koanf:"default_resources"`
```

### 3. Go Backend — Grove Settings

#### Annotation keys (`pkg/hub/grove_settings_handlers.go`)

Add annotation keys for grove-level defaults, using individual flat keys for the structured resource spec:

```go
groveSettingDefaultMaxTurns          = "scion.io/default-max-turns"
groveSettingDefaultMaxModelCalls     = "scion.io/default-max-model-calls"
groveSettingDefaultMaxDuration       = "scion.io/default-max-duration"
groveSettingDefaultResourcesCPUReq   = "scion.io/default-resources-cpu-request"
groveSettingDefaultResourcesMemReq   = "scion.io/default-resources-memory-request"
groveSettingDefaultResourcesCPULim   = "scion.io/default-resources-cpu-limit"
groveSettingDefaultResourcesMemLim   = "scion.io/default-resources-memory-limit"
groveSettingDefaultResourcesDisk     = "scion.io/default-resources-disk"
```

Update `groveSettingsFromAnnotations()` and `applyGroveSettingsToAnnotations()` to read/write these.

#### `GroveSettings` type (`pkg/hubclient/types.go`)

```go
type GroveSettings struct {
    // ... existing fields ...
    DefaultMaxTurns      int            `json:"defaultMaxTurns,omitempty"`
    DefaultMaxModelCalls int            `json:"defaultMaxModelCalls,omitempty"`
    DefaultMaxDuration   string         `json:"defaultMaxDuration,omitempty"`
    DefaultResources     *ResourceSpec  `json:"defaultResources,omitempty"`
}
```

A `ResourceSpec` type will be needed in the `hubclient` package (or reuse `api.ResourceSpec`).

### 4. Go Backend — Hub Admin Settings (`pkg/hub/admin_settings.go`)

Add fields to `ServerConfigResponse` and `ServerConfigUpdateRequest`:

```go
DefaultMaxTurns      int              `json:"default_max_turns,omitempty"`
DefaultMaxModelCalls int              `json:"default_max_model_calls,omitempty"`
DefaultMaxDuration   string           `json:"default_max_duration,omitempty"`
DefaultResources     *api.ResourceSpec `json:"default_resources,omitempty"`
```

Wire through `applySettingsUpdates()` and the GET response mapping.

### 5. Go Backend — Defaults Resolution

When an agent starts without explicit limits/resources, apply defaults in priority order:

1. **Agent inline config** (explicit per-agent values) — highest priority
2. **Template config** (from the template's `ScionConfig`)
3. **Grove defaults** (from grove annotations via `GroveSettings`)
4. **Hub global defaults** (from `settings.yaml` top-level)

This resolution should be applied during agent provisioning. The appropriate location depends on whether the agent is started locally (`pkg/agent/run.go`) or via the hub/broker dispatch path (`pkg/runtimebroker/handlers.go`).

For limits, this means: if `ScionConfig.MaxTurns` is 0 after template merge, check grove defaults, then hub defaults, and populate the value before injecting `SCION_MAX_TURNS` env var.

For resources, the same pattern applies: if `ScionConfig.Resources` is nil or has empty fields after template merge, fill from grove defaults, then hub defaults.

### 6. Web Frontend — Grove Settings Page (`web/src/components/pages/grove-settings.ts`)

Add to the existing Configuration section (below the current Default Template, Default Harness Config, and Telemetry fields):

**Limits subsection:**
- Default Max Turns — `sl-input` type=number
- Default Max Model Calls — `sl-input` type=number
- Default Max Duration — `sl-input` type=text, placeholder="e.g. 2h, 30m"

**Resources subsection:**
- CPU Request — `sl-input` type=text
- Memory Request — `sl-input` type=text
- CPU Limit — `sl-input` type=text
- Memory Limit — `sl-input` type=text
- Disk — `sl-input` type=text

These get saved via the existing `PUT /api/v1/groves/:id/settings` endpoint.

### 7. Web Frontend — Admin Server Config (`web/src/components/pages/admin-server-config.ts`)

Add to the General tab (alongside existing Default Template, Default Harness Config, Image Registry fields):

Same limit and resource input fields as the grove settings page, but saving via `PUT /api/v1/admin/server-config`.

### 8. Web Frontend — Agent Create Page (`web/src/components/pages/agent-create.ts`)

When creating a new agent and selecting a grove, pre-populate the limits and resources fields from the grove's defaults (fetched via the existing `GET /api/v1/groves/:id/settings` call that already powers `selectDefaultTemplate()`).

## Design Decisions

### Flat annotation keys for resources

Grove settings are stored as `map[string]string` annotations. Rather than JSON-encoding a `ResourceSpec` into a single annotation value, we use individual keys (e.g., `scion.io/default-resources-cpu-request`). This keeps each value independently readable and editable, consistent with how the other grove settings annotations work.

### No profile-level support

Profile-level `V1ProfileConfig` already has a `Resources` field for resource constraints. Adding limit defaults at the profile level is out of scope for this change. Profiles serve as runtime environment bundles, and default limits are better expressed at the grove or hub level where they represent organizational policy.

## Files to Modify

| File | Change |
|------|--------|
| `pkg/config/schemas/settings-v1.schema.json` | Add `default_max_turns`, `default_max_model_calls`, `default_max_duration`, `default_resources` |
| `pkg/config/settings_v1.go` | Add fields to `VersionedSettings` |
| `pkg/hubclient/types.go` | Add fields to `GroveSettings` |
| `pkg/hub/grove_settings_handlers.go` | Add annotation keys, update read/write functions |
| `pkg/hub/grove_settings_handlers_test.go` | Test new annotation handling |
| `pkg/hub/admin_settings.go` | Add fields to response/request types, wire through |
| `pkg/agent/run.go` | Apply grove/hub default limits during local agent start |
| `pkg/runtimebroker/handlers.go` | Apply grove/hub default limits during broker dispatch |
| `web/src/components/pages/grove-settings.ts` | Add limit/resource form fields to Configuration section |
| `web/src/components/pages/admin-server-config.ts` | Add limit/resource form fields to General tab |
| `web/src/components/pages/agent-create.ts` | Pre-populate limits/resources from grove defaults |

## Open Questions

1. **Clearing defaults:** Should setting a grove-level default to 0 / empty explicitly clear an inherited hub default, or should it mean "no override, inherit from hub"? The current pattern (0 = unset/inherit) is simplest but doesn't allow groves to explicitly remove a hub-level limit.
2. **Display inheritance:** Should the settings UI show which values are inherited from the hub vs explicitly set at grove level (e.g., placeholder text showing the hub default)?
3. **Validation:** Should the hub validate duration format (e.g., reject "abc" for `default_max_duration`) or leave validation to the agent start path?
