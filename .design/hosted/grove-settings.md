# Grove Settings UI

## Status: Draft

## Problem

The server config admin page (`/admin/server-config`) exposes the full `settings.yaml` schema as an editable UI. This is powerful for server operators, but grove owners need a way to configure agent defaults for their grove without needing server-level access. Today the grove settings page only covers env vars, secrets, shared directories, templates, and members. Key operational settings like default template, default harness config, and image registry are not editable through the UI.

## Goal

Expose a reasonable subset of the server settings schema as grove-level configuration in the grove settings UI. This gives grove owners self-service control over how agents behave within their grove, without requiring admin privileges or direct file access.

## Current State

### Server Config (Admin)
The admin server config page edits `~/.scion/settings.yaml` and exposes:
- Top-level: `active_profile`, `default_template`, `default_harness_config`, `image_registry`, `workspace_path`
- `server.*` (hub, broker, database, auth, oauth, storage, secrets, logging, notifications)
- `telemetry.*`
- `runtimes.*`
- `harness_configs.*`
- `profiles.*`

### Grove Settings (Current)
The grove settings page (`/groves/:id/settings`) currently shows:
- Templates (read-only list with sync action)
- Members (group editor)
- Environment variables
- Secrets
- Shared directories
- Danger zone (delete)

### Hub API (Current)
`GET/PUT /api/v1/groves/:id/settings` operates on `GroveSettings`:
```go
type GroveSettings struct {
    ActiveProfile   string
    DefaultTemplate string
    Bucket          *BucketConfig
    Runtimes        map[string]interface{}
    Harnesses       map[string]interface{}
    Profiles        map[string]interface{}
}
```

### Settings Scope Hierarchy
Settings are resolved with local-first precedence:
1. Agent/template level (most specific)
2. Profile level
3. Grove level (`.scion/settings.yaml` in project)
4. Global level (`~/.scion/settings.yaml`)
5. Embedded defaults

## Proposed Grove Settings Subset

### Tier 1: Include in Initial Implementation

These settings are directly useful to grove owners and have clear grove-level semantics.

#### 1. Default Template
- **Field:** `default_template`
- **UI:** Dropdown/select populated from the grove's synced templates
- **Rationale:** Most common thing a grove owner wants to set. Determines which template is used when `scion start` is run without `--template`.

#### 2. Default Harness Config
- **Field:** `default_harness_config`
- **UI:** Dropdown/select populated from available harness configs (grove-level + inherited from server)
- **Rationale:** Controls which harness (Claude, Gemini, etc.) and its configuration is used by default. Often the first thing users want to customize.

#### 3. Telemetry Toggle
- **Field:** `telemetry_enabled`
- **UI:** Toggle switch (on/off)
- **Rationale:** Grove owners should be able to opt in or out of telemetry for their grove's agents without requiring server admin access. The full telemetry configuration (endpoints, exporters, etc.) remains server-level.

#### 4. Default Runtime Broker
- **Field:** `defaultRuntimeBrokerId` (grove metadata, not settings.yaml)
- **UI:** Dropdown/select from grove providers
- **Rationale:** Already exists as grove metadata. Multi-broker groves need to select a preferred broker. Could be surfaced alongside the new settings for discoverability.

### Tier 2: Include When Profiles Are Editable

These require more complex UI (nested maps, lists) but are natural extensions.

#### 6. Active Profile
- **Field:** `active_profile`
- **UI:** Dropdown/select from defined profiles
- **Rationale:** Profiles bundle runtime, harness overrides, env, and resource settings. Selecting the active profile is a common operation.
- **Note:** Profile is primarily a broker-level concern. When exposed at grove level, this acts as a _preference_ that brokers may apply, not a strict binding. In multi-broker groves, each broker may have different profiles available. The grove-level active profile serves as a hint that brokers apply if the named profile exists locally.

#### 7. Image Registry
- **Field:** `image_registry`
- **UI:** Text input
- **Rationale:** Groves that use custom or private images may want to set this at grove level.
- **Note:** For custom agent images, the preferred approach is to specify the image in a harness config entry. A grove-level `image_registry` is useful only as a global prefix/override. This should be deferred until harness config editing (below) is available, so users can set images in the right place first.

#### 8. Harness Configs (Grove-Scoped)
- **Field:** `harness_configs`
- **UI:** Named config editor with fields for: harness type, image, user, model, args, env, auth type, secrets
- **Rationale:** Lets grove owners define custom harness configurations without touching files. The server config UI already has this pattern.
- **Complexity:** Map of named entries, each with multiple fields. Needs add/remove/edit UX.

#### 9. Profiles (Grove-Scoped)
- **Field:** `profiles`
- **UI:** Named profile editor with fields for: runtime, default_template, default_harness_config, image_registry, env, volumes, resources, harness_overrides
- **Rationale:** Profiles are the primary customization mechanism. Being able to define them from the UI closes the loop on self-service configuration.
- **Complexity:** Highest complexity item - nested maps within maps, resource specs, volume mounts.

### Tier 3: Exclude from Grove Settings

These are server-level concerns and should remain admin-only.

| Setting | Reason for Exclusion |
|---------|---------------------|
| `server.*` (hub, broker, database, auth, oauth) | Infrastructure config, not grove-scoped |
| `server.storage.*` | Server-level blob storage backend |
| `server.secrets.*` | Server-level secrets backend |
| `server.notifications.*` | Server-level notification channels |
| `workspace_path` | Filesystem path, broker-specific |
| `runtimes.*` | Runtime definitions are infrastructure; grove chooses via profile |
| `telemetry.*` (full config) | Observability infrastructure (endpoints, exporters) not per-grove; enable/disable toggle is exposed in Tier 1 |
| `hub.*` (client config) | Connection config, managed by CLI |
| `cli.*` | CLI behavior preferences, not grove-scoped |
| `bucket.*` | Workspace sync config, operational |

## API Changes

### Expand `GroveSettings` Type

The existing `GroveSettings` struct uses `map[string]interface{}` for harnesses and profiles. For Tier 1, we need typed fields:

```go
type GroveSettings struct {
    ActiveProfile        string                         `json:"activeProfile,omitempty"`
    DefaultTemplate      string                         `json:"defaultTemplate,omitempty"`
    DefaultHarnessConfig string                         `json:"defaultHarnessConfig,omitempty"`
    TelemetryEnabled     *bool                          `json:"telemetryEnabled,omitempty"`
    Bucket               *BucketConfig                  `json:"bucket,omitempty"`
    Runtimes             map[string]interface{}          `json:"runtimes,omitempty"`
    Harnesses            map[string]interface{}          `json:"harnesses,omitempty"`
    Profiles             map[string]interface{}          `json:"profiles,omitempty"`
}
```

The key addition is `DefaultHarnessConfig` and `TelemetryEnabled`, which are currently absent from the hub-side `GroveSettings` type but are present in (or derivable from) the on-disk `VersionedSettings`.

### Relationship to `settings.yaml` and Agent Provisioning

These grove settings are **not** a parallel mechanism to the existing `settings.yaml` file. Instead, hub-stored grove settings are written back to the grove's `.scion/settings.yaml` on the broker filesystem. During agent provisioning, the broker reads `settings.yaml` as it does today - the hub API is just a remote editing interface for the same file.

The flow is:
1. User edits grove settings via the hub UI/API
2. Hub persists the values and notifies the broker (or the broker pulls on next sync)
3. Broker writes/merges the values into the grove's `.scion/settings.yaml`
4. Agent provisioning reads `.scion/settings.yaml` as normal - no new code path

This ensures a single source of truth per grove and avoids any divergence between hub-stored metadata and the on-disk settings that the broker actually uses.

### Hub Storage

For **linked groves**, grove settings live on the broker's filesystem (`.scion/settings.yaml`). The hub stores the values and syncs them to the broker, which owns the file on disk.

For **hub-managed groves**, settings live in `~/.scion/groves/<slug>/.scion/settings.yaml` on each provider broker. The hub writes these directly during grove provisioning.

## UI Design

### Layout

Add a new "Configuration" section to the grove settings page, positioned above the existing env vars section. This section contains the Tier 1 fields as a simple form:

```
[Back to Grove]

Settings icon  <grove-name> Settings

+-------------------------------------------------+
| Configuration                                   |
| Grove-level defaults for agent creation.         |
|                                                  |
| Default Template    [  dropdown v ]              |
| Default Harness     [  dropdown v ]              |
| Telemetry           [  toggle on/off ]           |
|                                                  |
|                          [ Save Configuration ]  |
+-------------------------------------------------+

[Templates section - existing]
[Members section - existing]
[Env Vars section - existing]
[Secrets section - existing]
[Shared Dirs section - existing]
[Danger Zone section - existing]
```

### Behavior
- Load current values from `GET /api/v1/groves/:id/settings` on page load
- Dropdowns populated from available options (templates from grove scope, harness configs from settings, profiles from settings)
- Save sends `PUT /api/v1/groves/:id/settings` with updated fields
- Only fields that differ from server defaults are stored (sparse/override model)
- Requires `update` or `manage` capability on the grove

### Capability Gating
- Read-only view for users with `read` access (shows current values but no edit controls)
- Edit controls for users with `update` or `manage` capability

## Implementation Plan

### Phase 1: Tier 1 Settings
1. Add `DefaultHarnessConfig` and `TelemetryEnabled` to `hubclient.GroveSettings`
2. Update hub `GET/PUT` grove settings handlers to include the new fields
3. Ensure hub settings changes are synced back to the broker's `.scion/settings.yaml` (not stored as parallel metadata)
4. Add "Configuration" section to `grove-settings.ts` with form fields
5. Populate dropdowns from existing API endpoints (templates, harness configs)
6. Wire save button to `PUT` endpoint

### Phase 2: Tier 2 Settings (Future)
1. Add active profile selector and image registry field
2. Build reusable harness config editor component
3. Build profile editor component
4. Add tabs or expandable sections for harness configs and profiles
5. Update API to accept typed harness config and profile structures

## Open Questions

1. **Inheritance display:** Should the UI show which values are inherited from the server vs explicitly set at grove level? (e.g., greyed-out placeholder showing the server default)
2. **Validation:** Should the hub validate that referenced templates/harness configs actually exist, or is it purely a named reference?
3. **Profile/harness config creation:** Should grove owners be able to create new profiles and harness configs, or only select from those defined at the server level?
