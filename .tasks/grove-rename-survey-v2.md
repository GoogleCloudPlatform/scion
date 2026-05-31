# Grove-to-Project Rename Survey v2

**Date:** 2026-05-29
**Branch surveyed:** `main` (at commit `2c03b71`)
**Scope:** All remaining "grove" references in tracked source files (excluding `.git/`, `node_modules/`, `.scion/`)

---

## Executive Summary

The codebase contains **~4,075 grove references in Go source** (1,933 in production code, 2,142 in tests), plus **~140 in changelogs**, **~120 in documentation**, and **~6 in embedded YAML configs**. The web frontend has **zero** remaining grove references.

The references fall into two distinct categories:

1. **Intentional backward-compatibility shims** (~40% of production code references) — JSON marshal/unmarshal pairs, deprecated CLI flags, legacy API endpoint aliases, and wire-protocol dual-field support. These exist *by design* and must remain until a breaking-change version boundary.

2. **Internal naming that could be renamed** (~60% of production code) — Go local variables, function names, struct field names in YAML tags, container labels, environment variable names, NATS topic prefixes, SQL schema identifiers, filesystem path conventions, and telemetry attributes. These are candidates for incremental rename.

---

## Reference Counts by Category

| Category | Production Code | Test Code | Notes |
|---|---|---|---|
| **JSON struct tags** (`json:"groveId"`, `json:"grove"`, etc.) | 162 | ~80 | Backward-compat shims for API wire format |
| **Go local variable names** (`groveID`, `grovePath`, `groveSettings`, etc.) | ~200 | ~300 | Internal naming, safe to rename |
| **Container labels** (`scion.grove`, `scion.grove_id`, `scion.grove_path`) | 78 | ~40 | Cross-system contract with running containers |
| **Environment variables** (`SCION_GROVE_ID`, `SCION_GROVE`, `SCION_GROVE_PATH`) | 22 | ~15 | Injected into agent containers |
| **NATS topic strings** (`scion.grove.<id>.*`) | 12 | ~8 | Message bus topic prefix |
| **CLI flags** (`--grove` deprecated aliases) | 45 | ~30 | All marked deprecated+hidden |
| **SQL/database schema** (`groves` table, `grove_id` columns, `grove_contributors`) | 117 | ~50 | Schema migration territory |
| **Filesystem paths** (`grove-configs/`, `.scion.groves/`, `grove-id`, `grove-workspace`) | 27 | ~15 | On-disk directory conventions |
| **Embedded YAML configs** | 6 | 0 | File `default_grove_settings.yaml` |
| **Telemetry attributes** (`grove_id`, `scion.grove.id`) | 14 | ~5 | Observability labels |
| **Function/method names** | 4 | ~10 | `deprecateGroveEndpoint`, `MigrateGroveToProjectData`, etc. |
| **Type/struct definitions** | 2 | ~2 | `GroveDiscovery`, `GroveConfig` (alias) |
| **Design docs** (`.design/`) | N/A | N/A | 15 files with "grove" in name |
| **Files with "grove" in filename** | 21 | (included) | Source + docs + config |
| **Changelogs** | ~140 | N/A | Historical, should not change |

---

## Detailed Breakdown by Category

### 1. JSON Wire-Format Compatibility (Backward-Compat Shims)

These are `MarshalJSON`/`UnmarshalJSON` method pairs that emit and accept legacy `groveId`, `groveName`, `grove`, `grovePath`, `groveSlug` fields alongside the canonical `projectId`/`projectName` fields. **These are intentional and should remain until a breaking API version change.**

**Files (production):**
- `pkg/store/models.go` — 93 refs: Agent, Project, ProjectContributor, Template, Schedule, SubscriptionTemplate, Notification, ScheduledEvent, Message, ScheduleDetail (10 model types with marshal/unmarshal pairs)
- `pkg/hubclient/types.go` — 57 refs: AgentInfo, ProjectInfo, BrokerProjectInfo, TemplateInfo
- `pkg/hubclient/notifications.go` — 37 refs: NotificationInfo, SubscribeRequest, NotificationSubscription, NotificationTrigger, NotificationTemplate
- `pkg/hubclient/projects.go` — 17 refs: RegisterProjectRequest, UnregisterProjectRequest
- `pkg/hubclient/agents.go` — 8 refs: AgentCreateRequest
- `pkg/hubclient/templates.go` — 15 refs: TemplateListRequest, TemplateImportRequest
- `pkg/hubclient/tokens.go` — 14 refs: TokenCreateRequest, TokenInfo
- `pkg/hubclient/schedules.go` — 7 refs: Schedule
- `pkg/hubclient/scheduled_events.go` — 7 refs: ScheduledEvent
- `pkg/hubclient/messages.go` — 7 refs: Message
- `pkg/hubclient/runtime_brokers.go` — 22 refs: RuntimeBrokerInfo (Groves array), ProjectHeartbeat
- `pkg/api/types.go` — 24 refs: AgentInfo marshal/unmarshal, SecretSource legacy "grove" value
- `pkg/runtimebroker/types.go` — 52 refs: RuntimeBrokerInfo, AgentCreateRequest, StartContextResult, BrokerAgentInfo
- `pkg/wsprotocol/protocol.go` — 22 refs: ConnectMessage (Groves), StreamOpenMessage (GroveID)
- `pkg/hub/handlers.go` — ~15 refs: RegisterProjectRequest, ProjectListResponse (LegacyGroves), heartbeat unmarshal
- `pkg/hub/handlers_auth.go` — 2 refs: GCPServiceAccountResponse (groveId)
- `pkg/hub/handlers_notifications.go` — 6 refs: SubscriptionCreateRequest, query param fallback
- `pkg/hub/template_handlers.go` — 8 refs: TemplateImportRequest, TemplateListRequest
- `pkg/hub/response_types.go` — 24 refs: various response wrappers
- `pkg/hub/events.go` — 29 refs: event type compat

### 2. Container Labels

Labels applied to Docker/Podman/K8s containers. Changing these requires a migration strategy for existing running containers.

| Label | Used In | Count |
|---|---|---|
| `scion.grove` | common.go, docker.go, podman.go, apple_container.go, k8s_runtime.go, agent/run.go, agent/list.go, provision.go, server_dispatcher.go, fs-watcher | ~30 |
| `scion.grove_id` | common.go, docker.go, podman.go, apple_container.go, k8s_runtime.go, agent/run.go, runtimebroker/handlers.go, server.go | ~25 |
| `scion.grove_path` | common.go, docker.go, podman.go, apple_container.go, k8s_runtime.go, agent/run.go | ~15 |

**Key files:**
- `pkg/runtime/common.go` — Lines 286-288, 393, 397: env injection + label creation
- `pkg/runtime/docker.go` — Lines 177-224: label reads for filter matching
- `pkg/runtime/podman.go` — Lines 303-305: parsing labels from container list
- `pkg/runtime/apple_container.go` — Lines 210-212: parsing labels
- `pkg/runtime/k8s_runtime.go` — Lines 676-756, 1615-1708: label creation + PVC selectors + pod queries
- `pkg/agent/run.go` — Lines 895-914: label assignment during agent start
- `pkg/agent/list.go` — Lines 42-64: filter matching by label
- `pkg/agent/provision.go` — Line 190: label during provision
- `cmd/server_dispatcher.go` — Lines 61, 95, 156: hub dispatcher label injection

### 3. Environment Variables

| Variable | Description | Files |
|---|---|---|
| `SCION_GROVE` | Project name injected into container | `pkg/runtime/common.go:286` |
| `SCION_GROVE_ID` | Project UUID injected into container | `pkg/runtime/common.go:288`, `pkg/runtimebroker/start_context.go:280`, `pkg/hub/httpdispatcher.go:953,1112`, `pkg/agent/run.go:68-71,903` |
| `SCION_GROVE_PATH` | Project filesystem path | `pkg/runtimebroker/start_context.go:284` |
| `SCION_HUB_GROVE_ID` | Hub project ID override (env-to-config mapping) | `pkg/config/koanf.go:91-92` |

**Also referenced in:**
- `pkg/runtimebroker/hubenv.go:33-34` — allowlist of passthrough env vars
- `pkg/sciontool/telemetry/providers.go:66` — telemetry attribute source
- `pkg/sciontool/telemetry/gcp_exporter.go:92` — GCP metrics labels
- `pkg/sciontool/hooks/handlers/telemetry.go:496` — hook telemetry
- `pkg/config/project_marker.go:185-189` — container detection logic
- `extras/scion-a2a-bridge/cmd/main.go:204` — A2A bridge config

### 4. NATS Topic Prefix (`scion.grove.<id>.*`)

The message broker uses `scion.grove.<id>` as the topic namespace. This is a **wire protocol** concern.

**Files:**
- `pkg/broker/broker.go` — Lines 21-86: 5 topic-building functions (AgentMessageTopic, BroadcastTopic, AgentWildcardTopic, UserMessageTopic, UserWildcardTopic) all produce `scion.grove.*` topics
- `extras/scion-a2a-bridge/internal/bridge/bridge.go:275` — subscribes to `scion.grove.*` 
- `extras/scion-chat-app/cmd/scion-chat-app/main.go:249` — subscribes to `scion.grove.*`
- `extras/scion-chat-app/internal/chatapp/commands.go:618,638` — subscribes to `scion.grove.*`
- `extras/scion-chat-app/internal/chatapp/notifications.go:49` — comment documenting topic format
- `extras/scion-telegram/internal/telegram/broker_v2.go:451,663,2198` — parses `scion.grove.*` topics
- `cmd/scion-broker-repl/main.go:25-27` — example NATS commands in comments

### 5. SQL/Database Schema

The SQLite schema uses `grove`-based naming for tables, columns, indexes, and foreign keys. This is the **most migration-sensitive** area.

**Tables with "grove" in name:**
- `groves` — primary project table (CREATE TABLE + 3 indexes + multiple ALTER TABLEs in migrations)
- `grove_contributors` — project-broker association table
- `grove_sync_state` — hub sync state table

**Columns named `grove_id` across tables:**
- `grove_contributors.grove_id`
- `agents.grove_id`
- `templates.grove_id`
- `notification_subscriptions.grove_id`
- `notifications.grove_id`
- `scheduled_events.grove_id`
- `groups.grove_id`
- `schedules.grove_id`
- `subscription_templates.grove_id`
- `user_access_tokens.grove_id` (inferred from rename migration)
- `messages.grove_id`
- `gcp_service_accounts.grove_id` (inferred from rename migration)

**Indexes with "grove":**
- `idx_groves_slug`, `idx_groves_git_remote`, `idx_groves_owner`, `idx_groves_default_runtime_broker`
- `idx_agents_grove_slug`, `idx_agents_grove`
- `idx_grove_sync_state_project` (on `grove_sync_state(grove_id)`)
- Multiple `idx_*_project` indexes on `grove_id` columns

**Rename migration exists at:**
- `pkg/store/sqlite/sqlite.go` lines 1236-1250: Migration V50 renames `grove_id` → `project_id` across 12 tables, `grove_contributors` → `project_contributors`, `grove_sync_state` → `project_sync_state`
- But the **initial schema** (V1) and **all intermediate migrations** (V2-V49) still reference grove naming — these are historical DDL and cannot be changed

### 6. CLI Flags & Commands

All `--grove` flags are properly deprecated with `MarkDeprecated` + `MarkHidden`. The `project` command has `grove` as an alias.

| Location | Type | Detail |
|---|---|---|
| `cmd/root.go:228-230` | Persistent flag | `--grove` deprecated alias for `--project` |
| `cmd/root.go:186-194` | Early arg parsing | `--grove` / `-g` / `--grove=` pre-parse |
| `cmd/project.go:42-43` | Command alias | `Aliases: []string{"grove", "group"}` |
| `cmd/broker.go:350-361` | Flag | `--grove` on `provide`/`withdraw` |
| `cmd/notifications.go:165-185` | Flags | `--grove` on subscribe/unsubscribe/subscriptions |
| `cmd/hub_env.go:168-193` | Flag | `--grove` on env commands |
| `cmd/hub_secret.go:181-206` | Flag | `--grove` on secret commands |
| `cmd/hub_token.go:150-165` | Flags | `--grove` on token create/list |
| `cmd/list.go:573` | Help text | "across all groves" |
| `cmd/stop.go:525` | Help text | "in the current grove" |
| `cmd/suspend.go:433` | Help text | "in the current grove" |
| `cmd/message.go:683-684` | Help text | "current grove" / "all groves" |
| `cmd/server.go:262` | Help text | "new groves" |
| `cmd/hub.go:309-353` | Subcommand | `hub groves` command (Use: "groves", Aliases: ["grove"]) |
| `cmd/config.go:209,365` | Label | `"grove"` label for project dir |
| `cmd/template_resolution.go:71-373` | Scope name | `"grove"` as scope value |
| `cmd/completion_helper.go:95-96` | Flag read | reads `"grove"` flag for completions |

### 7. Filesystem Path Conventions

| Path Pattern | Used For | Files |
|---|---|---|
| `grove-configs/` | Legacy project configs dir | `pkg/config/paths.go:32`, `pkg/config/project_marker.go:93`, `pkg/config/project_discovery.go:97,359`, `pkg/config/shared_dirs.go:47` |
| `.scion.groves/<slug>/` | Hub-managed project workspace | `pkg/runtimebroker/start_context.go:81,100,115`, `pkg/runtimebroker/handlers.go:603,892` |
| `grove-id` | Legacy project ID file | `pkg/config/project_marker.go:219-231`, `pkg/config/settings.go:552-557` |
| `grove-workspace` | Shared workspace path segment | `pkg/storage/storage.go:255` |
| `groves/` | Legacy projects directory | `pkg/config/paths.go:33`, `pkg/runtimebroker/server.go:870`, `pkg/runtimebroker/start_context.go:100` |
| `templates/groves/` | Storage path for project templates | `pkg/storage/storage.go:210` |
| `harness-configs/groves/` | Storage path for project harness configs | `pkg/storage/storage.go:231` |
| `default_grove_settings.yaml` | Embedded default config | `pkg/config/embeds/default_grove_settings.yaml`, `pkg/config/koanf.go:261` |

### 8. Telemetry & Observability

| Attribute/Label | Files |
|---|---|
| `scion.grove.id` | `pkg/sciontool/telemetry/providers.go:70,81` |
| `grove_id` (GCP label) | `pkg/sciontool/telemetry/gcp_exporter.go:95,102` |
| `grove_id` (hook attr) | `pkg/sciontool/hooks/handlers/telemetry.go:497,500` |
| `grove_id` (log field) | `pkg/agent/msgbuffer.go:129`, `pkg/util/logging/logging.go:76` |
| `grove_id` (cloud label) | `pkg/util/logging/cloud_handler.go:153`, `pkg/util/logging/gcp_handler.go:107` |
| `GroveIdx` (struct field) | `pkg/util/logging/request_log.go:234` |
| Log debug strings | `pkg/hubsync/sync.go:324,656,807,814,924` and many others using `grove_id` in structured log attrs |

### 9. Go Internal Variable & Parameter Names

These are the **largest** category (~200 production, ~300 test). Key patterns:

| Variable Pattern | Approximate Count | Key Files |
|---|---|---|
| `groveID` / `groveId` | ~80 | Widespread across hubsync, runtimebroker, config, hub |
| `grovePath` / `grovePaths` | ~40 | runtimebroker/handlers.go, server.go, config/settings.go |
| `groveSettings` | ~10 | runtimebroker/hubenv.go, server.go |
| `groveName` | ~15 | runtimebroker, agent/manager, k8s_runtime |
| `groveSlug` | ~5 | agent/provision.go, runtimebroker |
| `grovesToScan` | ~5 | agent/list.go |
| `groveFilter` | ~3 | runtimebroker/hub_connection.go, server.go |
| `grovePattern` | ~2 | scion-a2a-bridge |
| `deletionGroveName` | 3 | agent/manager.go |
| `groveParent` | 2 | runtimebroker/workspace_handlers.go |

### 10. Files with "grove" in Filename

**Source files (6):**
- `pkg/ent/entc/migrate_grove_to_project.go` — Migration logic (keeping name is appropriate)
- `pkg/ent/entc/migrate_grove_to_project_nosqlite.go` — No-op stub for non-SQLite builds
- `pkg/ent/entc/migrate_grove_to_project_test.go` — Tests for migration
- `extras/fs-watcher-tool/pkg/fswatcher/grove.go` — GroveDiscovery struct + container label queries
- `extras/fs-watcher-tool/pkg/fswatcher/grove_test.go` — Tests for above
- `pkg/config/embeds/default_grove_settings.yaml` — Embedded default settings

**Design docs (15):**
- `.design/grove-dirs.md`
- `.design/git-grove-duplicates.md`
- `.design/grove-level-templates.md`
- `.design/grove-to-project-rename.md`
- `.design/grove-mount-protection.md`
- `.design/hosted/grove-settings.md`
- `.design/hosted/hub-groves.md`
- `.design/hosted/git-groves.md`
- `.design/project-log/2026-05-12-ent-grove-to-project-data-migration.md`
- `.design/project-log/2026-05-13-fix-grove-bugs.md`
- `.design/project-log/2026-05-13-fix-hub-env-test-groveid-to-projectid.md`
- `.design/project-log/2026-05-13-fix-hub-test-grove-to-project.md`
- `.design/project-log/2026-05-13-fix-delete-test-grove-to-project.md`
- `.design/project-log/2026-05-13-fix-list-test-grove-to-project.md`
- `.design/project-log/2026-05-13-rebase-grove-v2.md`

### 11. Extras / Satellite Projects

| Project | Refs (production) | Key Issues |
|---|---|---|
| `extras/scion-chat-app/` | ~215 | SQL schema with `grove_id` columns, NATS topics, command handlers, state management |
| `extras/scion-a2a-bridge/` | ~80 | Config type alias (`GroveConfig`), NATS topics, bridge server routes |
| `extras/fs-watcher-tool/` | ~40 | `GroveDiscovery` struct, Docker label queries, `--grove` flag |
| `extras/scion-telegram/` | ~12 | NATS topic parsing, broker commands |
| `extras/agent-viz/` | ~2 | Log label parsing (`grove_id`) |
| `extras/scion-broker-log/` | ~3 (README only) | Example NATS topics in docs |

### 12. Hub API Legacy Endpoints

Deprecated `/api/v1/groves/*` routes are aliased to `/api/v1/projects/*` handlers:

```
/api/v1/groves           → handleProjects           (wrapped with deprecateGroveEndpoint)
/api/v1/groves/register  → handleProjectRegister     (wrapped with deprecateGroveEndpoint)
/api/v1/groves/          → handleProjectRoutes       (wrapped with deprecateGroveEndpoint)
```

Query parameter fallbacks: `groveId` → `projectId` in notification and template handlers.

Web UI route alias: `/groves` → `/projects` in `pkg/hub/web.go:786`.

Broker endpoint: `/api/v1/workspace/grove-upload` in `pkg/runtimebroker/server.go:1452`.

### 13. Settings/Config Key Mapping

- `grove_id` setting key accepted as alias for `project_id` in `pkg/config/settings.go:619,758` and `pkg/config/settings_v1.go:1705,1810`
- `hub.groveId` / `hub.grove_id` accepted as alias for `hub.projectId` / `hub.project_id` in `pkg/config/settings.go:657,794` and `pkg/config/settings_v1.go:1729,1836`
- V1 settings struct: `ProjectID string json:"grove_id"` in `pkg/config/settings_v1.go:440`
- Koanf loading: `hub.grove_id` remapping logic in `pkg/config/koanf.go:82-120`

---

## Categorization for Rename Priority

### Tier 1: Safe to Rename (Internal Only)

These changes have no external contract implications:

- **Go local variable names** — `groveID` → `projectID`, `grovePath` → `projectPath`, etc.
- **Go function names** — `deprecateGroveEndpoint`, `hubEndpointFromProjectSettings(grovePath)`, etc.
- **Go struct field names with YAML tags** — `GroveID string yaml:"grove-id"` in project_marker.go
- **Comments and log messages** containing "grove"
- **Test function names** — 51 test functions with "grove" in name
- **Internal file names** — `extras/fs-watcher-tool/pkg/fswatcher/grove.go`, `default_grove_settings.yaml`
- **Design docs** — historical but could be updated

**Estimated scope:** ~1,200 lines in production code, ~2,100 lines in tests

### Tier 2: Requires Coordinated Migration

These have cross-system contracts that require careful phasing:

- **Container labels** (`scion.grove`, `scion.grove_id`, `scion.grove_path`) — running containers use these labels; needs dual-label period or version bump
- **Environment variables** (`SCION_GROVE_ID`, `SCION_GROVE`, `SCION_GROVE_PATH`) — injected into containers; agents and harness tools read them
- **NATS topic prefix** (`scion.grove.<id>.*`) — wire protocol between broker, hub, and satellite apps
- **Filesystem paths** (`grove-configs/`, `.scion.groves/`, `grove-id` file) — on-disk format with fallback reads already implemented
- **Telemetry attributes** (`grove_id`, `scion.grove.id`) — downstream dashboards/queries may reference these
- **Storage paths** (`templates/groves/`, `harness-configs/groves/`, `workspaces/<id>/grove-workspace`) — GCS/local storage paths

**Estimated scope:** ~200 lines in production code

### Tier 3: Must Remain (Backward Compatibility)

These should stay as-is until a major version boundary:

- **JSON struct tags** (`json:"groveId"`, `json:"grove"`, `json:"groves"`) — wire format compat for clients
- **Deprecated CLI flags** (`--grove`) — properly marked deprecated, will be removed in a future major version
- **Hub API legacy endpoints** (`/api/v1/groves/*`) — deprecated with headers
- **SQL migration history** (V1-V49 DDL) — immutable historical migrations
- **Settings key aliases** (`grove_id`, `hub.groveId`) — config file backward compat
- **Query parameter fallbacks** (`groveId` → `projectId`) — API compat
- **Ent migration file** (`migrate_grove_to_project.go`) — the migration itself references grove by necessity

**Estimated scope:** ~500 lines in production code

---

## Top 15 Files by Grove Reference Count (Production)

| # | File | Refs | Primary Category |
|---|---|---|---|
| 1 | `pkg/runtimebroker/handlers.go` | 172 | Variables, labels, log attrs |
| 2 | `pkg/hubsync/sync.go` | 152 | Variables, env vars, settings keys |
| 3 | `pkg/store/sqlite/sqlite.go` | 117 | SQL schema DDL |
| 4 | `pkg/store/models.go` | 93 | JSON marshal/unmarshal compat |
| 5 | `pkg/hubsync/prompt.go` | 79 | Variables, user prompts |
| 6 | `extras/scion-chat-app/internal/chatapp/commands.go` | 67 | NATS topics, variables |
| 7 | `pkg/hubclient/types.go` | 57 | JSON compat shims |
| 8 | `pkg/runtimebroker/types.go` | 52 | JSON tags, struct fields |
| 9 | `extras/scion-chat-app/internal/state/state.go` | 47 | SQL schema, column refs |
| 10 | `pkg/runtimebroker/server.go` | 44 | Variables, dir scanning |
| 11 | `pkg/hubclient/notifications.go` | 37 | JSON compat shims |
| 12 | `pkg/config/settings.go` | 34 | Config key aliases, migration |
| 13 | `pkg/runtimebroker/start_context.go` | 32 | Path resolution, env injection |
| 14 | `pkg/runtime/k8s_runtime.go` | 31 | Labels, PVC selectors |
| 15 | `pkg/hub/events.go` | 29 | Event type compat |

---

## Observations

1. **Migration V50 exists** (`pkg/store/sqlite/sqlite.go:1236-1250`) and handles the SQL schema rename (`grove_id` → `project_id`, `grove_contributors` → `project_contributors`, etc.). However, all initial schema DDL and intermediate migrations (V1-V49) naturally retain grove naming as historical DDL.

2. **The extras/ directory is under-migrated.** The chat app, A2A bridge, and fs-watcher tool have significant grove references in both code and schema that haven't been updated.

3. **Dual-field JSON marshaling is thorough** — virtually every API-facing struct in `pkg/store/models.go`, `pkg/hubclient/`, and `pkg/runtimebroker/types.go` has proper backward-compat shims.

4. **The NATS topic prefix `scion.grove.*`** is the most architecturally sensitive remaining reference — it's a cross-service wire protocol used by the broker, hub, chat app, A2A bridge, and Telegram bot. Changing this requires either dual-subscription during a transition period or a coordinated version bump.

5. **Container labels** (`scion.grove`, `scion.grove_id`, `scion.grove_path`) are similarly sensitive — the system uses these labels for container discovery, filtering, and lifecycle management across Docker, Podman, Apple Virtualization, and Kubernetes runtimes.

6. **The web frontend has zero grove references** — this is fully migrated.

7. **Config key aliases are well-implemented** — `grove_id`, `hub.groveId`, and env vars like `SCION_HUB_GROVE_ID` all correctly map to their project equivalents through the koanf loading layer.
