# Grove → Project Rename: Strategy Document

**Date:** 2025-05-05
**Status:** Proposal
**Author:** Scion Agent (rename-strategy)

---

## 1. Executive Summary

Rename the internal concept "grove" to "project" across the Scion codebase. This affects ~22,000 references across ~689 files spanning Go backend, Ent ORM, SQLite schema, REST API paths, WebSocket protocol, CLI commands/flags, web frontend, container labels, environment variables, filesystem paths, telemetry attributes, docs, and design documents.

The rename is a high-risk, high-coordination change. The recommended approach is **phased incremental** — internal plumbing first (with backward-compatible shims), then API/CLI surface, then database, then docs — rather than a big-bang rewrite.

---

## 2. Scope Assessment

### 2.1 Quantitative Summary

| Category | Files | Approx. Refs | Risk |
|---|---|---|---|
| Go symbols (types, funcs, vars, consts) | ~300 | ~8,000 | Medium |
| Ent ORM (generated + schema) | 32 | ~1,382 | High |
| Hub server (handlers, authz, cache, settings) | 102 | ~6,866 | High |
| CLI commands & flags (`cmd/`) | 67 | ~2,438 | High |
| Runtime broker | 20 | ~1,039 | High |
| Config package (paths, settings, discovery, marker) | 32 | ~1,556 | High |
| API/JSON struct tags (`json:"grove*"`) | ~40 fields | ~50 | **Critical** |
| REST API paths (`/api/v1/groves/...`) | ~35 routes | ~60 | **Critical** |
| WebSocket protocol fields | 2 | ~12 | **Critical** |
| SQLite schema (tables, columns, indexes, FKs) | 1 | ~80 | **Critical** |
| SQLite migrations (V1–V40+) | 1 | ~40 | **Critical** |
| Store interface & models | 21 | ~1,415 | High |
| Container labels (`scion.grove*`) | ~5 | ~25 | High |
| Environment variables (`SCION_GROVE*`) | ~8 vars | ~30 | **Critical** |
| Filesystem paths (`groves/`, `grove-configs/`, `grove-id`) | ~15 refs | ~30 | High |
| YAML/koanf config keys (`grove_id`, `groveId`) | ~6 | ~15 | **Critical** |
| Hub client (`pkg/hubclient/groves.go`) | 17 | ~308 | High |
| Web frontend (TypeScript/JS) | 52 | ~1,107 | Medium |
| Agent package | 15 | ~369 | Medium |
| Runtime package | 12 | ~134 | Medium |
| Secret backend | 5 | ~85 | Low |
| Util/logging | 11 | ~81 | Low |
| grovesync package | 2 | ~35 | Medium |
| Extras (chat-app, fs-watcher, agent-viz) | 20 | ~290 | Low |
| Docs site | 33 | ~375 | Low |
| Design docs | 147 | ~3,444 | Low |
| Shell scripts | ~10 | ~252 | Low |
| Examples | 4 | ~8 | Low |
| JSON schemas | 2 | ~10 | Medium |

**Grand Total: ~22,084 references across ~689 files**

### 2.2 Files/Directories Named with "grove"

Key files and directories that need renaming:

```
cmd/grove.go, grove_list.go, grove_prune.go, grove_reconnect.go,
    grove_service_accounts.go, grove_test.go
pkg/config/grove_discovery.go, grove_marker.go, grove_discovery_test.go,
    grove_marker_test.go
pkg/config/embeds/default_grove_settings.yaml
pkg/ent/grove.go, grove/ (directory), grove_create.go, grove_delete.go,
    grove_query.go, grove_update.go, schema/grove.go
pkg/grovesync/ (entire package)
pkg/hub/grove_cache.go, grove_settings_handlers.go, grove_webdav.go,
    grove_workspace_handlers.go (+ tests)
pkg/hubclient/groves.go
pkg/store/sqlite/grove_sync_state.go (+ test)
extras/fs-watcher-tool/pkg/fswatcher/grove.go
web/src/components/pages/grove-create.ts, grove-detail.ts,
    grove-schedules.ts, grove-settings.ts, groves.ts
docs-site/src/content/docs/hub-user/git-groves.md
.design/grove-dirs.md, grove-level-templates.md, grove-mount-protection.md,
    hosted/git-groves.md, hosted/grove-settings.md, hosted/hub-groves.md
```

---

## 3. High-Risk Areas (Detailed)

### 3.1 REST API Paths (Critical — External Compatibility)

All hub routes are under `/api/v1/groves/...`:

```
/api/v1/groves                          — List/Create
/api/v1/groves/register                 — Register (upsert)
/api/v1/groves/{id}                     — Get/Update/Delete
/api/v1/groves/{id}/agents              — List agents
/api/v1/groves/{id}/agents/{agentId}    — Get/Delete agent
/api/v1/groves/{id}/settings            — Get/Update settings
/api/v1/groves/{id}/providers           — Manage broker providers
/api/v1/groves/{id}/broadcast           — Broadcast message
/api/v1/groves/{id}/workspace/cache/*   — Cache operations
/api/v1/groves/{id}/dav                 — WebDAV
/api/v1/groves/{id}/gcp-service-accounts — GCP SA management
/api/v1/groves/{id}/schedules           — Schedules
/api/v1/groves/{id}/scheduled-events    — Scheduled events
/api/v1/workspace/grove-upload          — Workspace upload (broker)
/api/v1/runtime-brokers/{id}/groves     — Broker's groves
```

**Impact:** Broker ↔ Hub communication, CLI ↔ Hub communication, web frontend ↔ Hub. Any rename breaks all clients that aren't updated simultaneously.

### 3.2 JSON Wire Format (Critical — API Compatibility)

Struct tags that define the wire format:

```go
// pkg/api/types.go
Grove     string `json:"grove"`
GroveID   string `json:"groveId,omitempty"`
GrovePath string `json:"grovePath,omitempty"`

// pkg/runtimebroker/types.go — 10+ fields
GroveID, GroveName, GroveSlug, GrovePath, etc.

// pkg/hubclient/types.go — 8+ fields
GroveID, Grove, GroveType, Groves, GroveName, etc.

// pkg/wsprotocol/protocol.go
Groves    []string `json:"groves,omitempty"`
GroveID   string   `json:"groveId,omitempty"`

// pkg/config/settings.go / settings_v1.go
GroveID with json/yaml/koanf tags: "grove_id" / "groveId"
```

### 3.3 Database Schema (Critical — Data Migration)

**Tables:**
- `groves` — Primary entity table (14 columns)
- `grove_contributors` — Broker↔grove relationships
- `grove_sync_state` — Workspace sync metadata

**Columns referencing grove in other tables:**
- `agents.grove_id` (FK to groves)
- `groups.grove_id`
- `notification_subscriptions.grove_id`
- `notifications.grove_id`
- `scheduled_events.grove_id`
- `schedules.grove_id`
- `env_vars.grove_id` (via scope)
- `templates.grove_id` (via scope)
- `messages.grove_id`

**Indexes:**
- `idx_groves_slug`, `idx_groves_git_remote`, `idx_groves_owner`
- `idx_groves_default_runtime_broker`
- `idx_agents_grove_slug`, `idx_agents_grove`
- `idx_notification_subs_grove`, `idx_notifications_grove`
- `idx_scheduled_events_grove`, `idx_groups_grove`

**Migration history:** 35+ migrations, many referencing grove tables/columns. V40 drops and recreates the `groves` table.

**Ent ORM schema:** `pkg/ent/schema/grove.go` defines the Grove entity. All generated code in `pkg/ent/grove/` must be regenerated after schema rename.

### 3.4 Environment Variables (Critical — Container Interface)

```
SCION_GROVE        — Grove name (set by agent/run.go)
SCION_GROVE_ID     — Grove UUID (set by hub dispatcher, broker start_context)
SCION_HUB_GROVE_ID — Maps to settings grove_id via koanf
```

Used by: sciontool telemetry, harness settings (Gemini allowlist), agent runtime, hub dispatcher, hub sync.

### 3.5 Container Labels (High — Runtime Interface)

```
scion.grove         — Grove name
scion.grove_id      — Grove UUID
scion.grove_path    — Filesystem path
```

Used by: Docker, Podman, Apple Container, K8s runtimes for agent discovery, filtering, and PVC management.

### 3.6 Filesystem Paths (High — Data on Disk)

```
~/.scion/groves/{slug}/           — Hub-native grove workspaces
~/.scion/grove-configs/{slug}__<uuid>/.scion/  — External config
.scion/grove-id                   — Grove ID marker file
```

### 3.7 CLI Commands & Flags (High — User Interface)

```
scion grove          — Top-level command group
scion grove init     — Initialize grove
scion grove list     — List groves
scion grove prune    — Remove orphans
scion grove reconnect — Reconnect
scion init           — Alias for grove init
scion config cd-grove — Open grove config dir

Flags:
  --grove              (hub_token, completion_helper)
  --template-scope grove (create, start)
```

### 3.8 Authorization Scope Values (Critical — Security)

The string `"grove"` is used as an enum value in:
- Policy scope types: `"hub" | "grove" | "resource"`
- Template/HarnessConfig scopes: `"grove" | "global" | "user"`
- Notification subscription scopes: `"agent" | "grove"`
- Env var / secret scopes: `"grove"`
- Ent enum: `ScopeTypeGrove = "grove"`
- Ent migration schema: `Enums: []string{"hub", "grove", "resource"}`
- SQLite enum check: `group_type ... 'grove_agents'`

### 3.9 WebSocket Protocol (Critical — Broker Communication)

```go
ConnectMessage.Groves    []string  — Grove IDs a broker serves
StreamOpenMessage.GroveID string   — For PTY streaming
```

---

## 4. Proposed Strategy

### 4.1 Approach: Phased Incremental with Backward-Compatible Shims

A big-bang rename would require updating all components (CLI, broker, hub, web) simultaneously and break all running deployments. Instead:

**Phase 0: Preparation (1-2 days)**
- Create comprehensive test coverage baseline (run all tests, record pass/fail)
- Write a rename validation script that grep-sweeps for residual `grove` references
- Set up CI to run the validation script

**Phase 1: Internal Go Symbols (~3-5 days)**
- Rename Go types, functions, variables, constants from `Grove*` → `Project*`
- Rename Go files: `grove_*.go` → `project_*.go`
- Rename packages: `pkg/grovesync` → `pkg/projectsync`
- Update Ent schema: rename entity from `Grove` → `Project`, regenerate
- Keep JSON/YAML/koanf struct tags unchanged (backward compat)
- Keep API paths unchanged
- Keep env vars unchanged
- Validation: `go build ./...`, `go test ./...`, grep sweep

**Phase 2: CLI Commands & Flags (~1-2 days)**
- Rename `scion grove` → `scion project` command group
- Add `grove` as a hidden alias for backward compatibility
- Rename `--grove` flags → `--project` (keep `--grove` as hidden alias)
- Update CLI help text and error messages
- Validation: CLI integration tests, manual testing

**Phase 3: API Paths & Wire Format (~3-5 days)**
- Register new routes under `/api/v1/projects/...`
- Keep old `/api/v1/groves/...` routes as aliases (permanent or deprecation period)
- Update JSON struct tags: add `project*` fields, keep `grove*` as aliases via custom marshaling OR use a versioned API approach
- Update WebSocket protocol fields with backward-compatible handling
- Update env vars: `SCION_PROJECT_ID` (keep `SCION_GROVE_ID` as fallback)
- Validation: API integration tests, broker ↔ hub handshake tests

**Phase 4: Database Schema (~2-3 days)**
- Write a new SQLite migration (V41+) that:
  - Renames `groves` → `projects` table
  - Renames `grove_contributors` → `project_contributors`
  - Renames `grove_sync_state` → `project_sync_state`
  - Renames `grove_id` columns → `project_id` in all tables
  - Updates enum values: `'grove'` → `'project'`, `'grove_agents'` → `'project_agents'`
  - Recreates affected indexes with new names
- SQLite supports `ALTER TABLE RENAME TO` but not column renames easily — may need table recreation pattern (like V40)
- Update all SQL queries in `pkg/store/sqlite/`
- Validation: Migration test (fresh + upgrade from V40), data integrity checks

**Phase 5: Container Labels & Filesystem (~1-2 days)**
- Update container label keys: `scion.grove*` → `scion.project*`
- Add backward-compatible label reading (check both old and new)
- Update filesystem paths: `groves/` → `projects/`, `grove-configs/` → `project-configs/`
- Write migration logic for existing filesystem state
- Update `grove-id` marker file → `project-id`
- Validation: Container lifecycle tests across Docker/Podman/K8s

**Phase 6: Web Frontend (~2-3 days)**
- Rename TypeScript types/interfaces: `Grove` → `Project`
- Update component files: `grove-*.ts` → `project-*.ts`
- Update API client calls to new paths
- Update URL routes in the web app
- Validation: Web app manual testing, screenshot comparison

**Phase 7: Docs, Design Docs, Scripts (~2-3 days)**
- Update docs-site content
- Rename `git-groves.md` → `git-projects.md`
- Update all design docs (low priority, ~147 files / ~3,444 refs)
- Update shell scripts and examples
- Update JSON schemas
- Validation: Docs site build, link checker

**Phase 8: Cleanup (~1-2 days)**
- Remove backward-compatible shims after deprecation period
- Remove old API route aliases
- Remove old env var fallbacks
- Final grep sweep for any residual `grove` references
- Update CHANGELOG / release notes

### 4.2 Alternative: Big-Bang Rename

Pros: Simpler, no shim code, no dual-path maintenance.
Cons: All components must deploy simultaneously. Breaks running brokers, CLI versions, saved configs. No rollback path.

**Not recommended** for a production system with external API consumers.

### 4.3 Alternative: API Versioning (v2)

Introduce `/api/v2/projects/...` alongside `/api/v1/groves/...`.
Pros: Clean separation, long deprecation period possible.
Cons: Doubles API surface maintenance, v2 implies broader changes than just a rename.

**Could be used for Phase 3** if a longer migration window is needed.

---

## 5. Tooling

### 5.1 Automated Rename Tools

| Tool | Use Case | Limitations |
|---|---|---|
| `gorename` / `gopls rename` | Go symbol renames (type-safe) | One symbol at a time; doesn't handle strings/comments |
| `sed -i` / `perl -pi -e` | Bulk text replacement | No semantic awareness; risk of false positives |
| `go generate` (ent) | Regenerate Ent ORM after schema change | Must update schema first |
| Custom Go AST tool | Rename struct tag values | Would need to be written |
| `find ... -exec mv` | File/directory renames | Simple but needs careful ordering |

### 5.2 Recommended Tooling Approach

1. **Go symbols**: Use `gopls rename` for exported types/functions where practical. Fall back to `sed` with careful patterns for bulk changes (e.g., `s/\bGrove\b/Project/g` with word boundaries).

2. **Struct tags**: Manual or custom script — must preserve exact JSON/YAML keys during Phase 1, then update in Phase 3.

3. **SQL**: Manual — each migration must be hand-written and tested.

4. **File renames**: Script using `git mv` to preserve history.

5. **Frontend**: `sed` with word boundaries, plus IDE refactoring.

6. **Validation script** (run after every phase):
   ```bash
   #!/bin/bash
   # Check for residual grove references (excluding known exceptions)
   grep -r -i 'grove' --include='*.go' --include='*.ts' --include='*.js' \
     --exclude-dir=vendor --exclude-dir=node_modules \
     | grep -v '_test.go' \
     | grep -v '// grove→project:' \  # Allow migration comments
     | grep -v 'backward compat'
   ```

---

## 6. Validation Workflow

### Per-Phase Validation

1. **Build**: `go build ./...` — must pass with zero errors
2. **Unit tests**: `go test ./...` — must match baseline pass rate
3. **Ent codegen**: `go generate ./pkg/ent/...` — must produce valid code
4. **Grep sweep**: Run validation script, verify only known exceptions remain
5. **Integration tests**: Run `scripts/*-integration-test.sh` where applicable
6. **Web build**: `cd web && npm run build` — must compile
7. **Docs build**: `cd docs-site && npm run build` — must succeed

### End-to-End Validation

- [ ] Fresh `scion init` creates a project (not grove)
- [ ] `scion project list` works
- [ ] Hub registration via new API paths
- [ ] Broker ↔ Hub heartbeat with new field names
- [ ] Agent create/start/stop/delete lifecycle
- [ ] WebSocket PTY streaming
- [ ] Web dashboard shows projects
- [ ] Database migration from V40 → V41+ succeeds
- [ ] Existing `grove-id` files are read correctly
- [ ] Container labels are set correctly
- [ ] Telemetry attributes use new names
- [ ] Old CLI commands (`scion grove`) still work during transition

---

## 7. Migration & Compatibility Concerns

### 7.1 Existing Deployments

- **Running brokers** use `SCION_GROVE_ID` env var — must support both old and new
- **Saved settings files** contain `grove_id` / `groveId` keys — must read both
- **SQLite databases** have `groves` table — migration must be additive, not destructive
- **Container labels** on running containers use `scion.grove*` — runtime must read both
- **Filesystem**: `~/.scion/groves/` and `~/.scion/grove-configs/` directories exist on disk — need rename or symlink

### 7.2 Version Skew

During rollout, older CLI versions will send `grove`-based requests to newer hubs, and vice versa. The hub API must accept both path prefixes and both field names during the transition window.

### 7.3 Rollback Plan

If issues arise mid-rollout:
- Phase 1 (Go symbols only): `git revert` — no external impact
- Phase 2 (CLI): Hidden aliases mean old commands still work
- Phase 3 (API): Old routes still registered — revert is just removing new routes
- Phase 4 (DB): Migration must be reversible (add a down migration)

---

## 8. Effort Estimate

| Phase | Description | Effort |
|---|---|---|
| Phase 0 | Preparation, test baseline, validation scripts | 1-2 days |
| Phase 1 | Go symbols, file renames, Ent schema | 3-5 days |
| Phase 2 | CLI commands & flags | 1-2 days |
| Phase 3 | API paths, wire format, env vars | 3-5 days |
| Phase 4 | Database schema migration | 2-3 days |
| Phase 5 | Container labels, filesystem paths | 1-2 days |
| Phase 6 | Web frontend | 2-3 days |
| Phase 7 | Docs, design docs, scripts | 2-3 days |
| Phase 8 | Cleanup (post-deprecation) | 1-2 days |
| **Total** | | **16-27 days** |

This estimate assumes a single developer working full-time. Phases 1, 6, and 7 can be parallelized across developers. The critical path is Phases 3-5 (API + DB + container), which must be sequential.

---

## 9. Open Questions

1. **Deprecation timeline**: How long should `grove` aliases persist? Suggested: 2 release cycles minimum.
2. **API versioning**: Should this be bundled with an API v2, or just add aliases on v1?
3. **Ent entity name**: Should the Ent entity be `Project` (clean) or keep `Grove` in the ORM layer with a `projects` table name annotation?
4. **Design docs**: Should old design docs be updated, or just annotated with a note about the rename?
5. **External consumers**: Are there any external systems (beyond brokers/CLI) that depend on the `grove` API paths or field names?
6. **"grove-id" marker file**: Rename to `project-id`, or use a different mechanism entirely?

---

## 10. Appendix: Key Files by Phase

### Phase 1 (Go Symbols) — Top Priority Files

```
pkg/config/grove_discovery.go → project_discovery.go
pkg/config/grove_marker.go → project_marker.go
pkg/config/init.go (20+ Grove* functions)
pkg/config/paths.go (5+ Grove* functions)
pkg/config/settings.go, settings_v1.go
pkg/ent/schema/grove.go → project.go
pkg/grovesync/ → pkg/projectsync/
pkg/hub/grove_cache.go → project_cache.go
pkg/hub/grove_settings_handlers.go → project_settings_handlers.go
pkg/hub/grove_webdav.go → project_webdav.go
pkg/hub/grove_workspace_handlers.go → project_workspace_handlers.go
pkg/hubclient/groves.go → projects.go
pkg/store/models.go (Grove struct, GroveStore interface)
pkg/store/store.go (GroveStore, GroveProviderStore, GroveSyncStateStore)
pkg/store/sqlite/grove_sync_state.go → project_sync_state.go
cmd/grove.go → project.go
cmd/grove_list.go → project_list.go
cmd/grove_prune.go → project_prune.go
cmd/grove_reconnect.go → project_reconnect.go
cmd/grove_service_accounts.go → project_service_accounts.go
```

### Phase 3 (API) — Critical Paths

```
/api/v1/groves → /api/v1/projects
/api/v1/groves/{id}/agents → /api/v1/projects/{id}/agents
/api/v1/groves/{id}/settings → /api/v1/projects/{id}/settings
/api/v1/groves/{id}/providers → /api/v1/projects/{id}/providers
/api/v1/groves/{id}/dav → /api/v1/projects/{id}/dav
/api/v1/groves/{id}/schedules → /api/v1/projects/{id}/schedules
/api/v1/groves/{id}/scheduled-events → /api/v1/projects/{id}/scheduled-events
/api/v1/groves/{id}/gcp-service-accounts → /api/v1/projects/{id}/gcp-service-accounts
/api/v1/groves/{id}/broadcast → /api/v1/projects/{id}/broadcast
/api/v1/groves/{id}/workspace/cache/* → /api/v1/projects/{id}/workspace/cache/*
/api/v1/workspace/grove-upload → /api/v1/workspace/project-upload
/api/v1/runtime-brokers/{id}/groves → /api/v1/runtime-brokers/{id}/projects
```

### Phase 4 (Database) — Tables/Columns

```sql
-- Table renames
groves → projects
grove_contributors → project_contributors
grove_sync_state → project_sync_state

-- Column renames (in multiple tables)
grove_id → project_id  (agents, groups, notification_subscriptions,
                         notifications, scheduled_events, schedules, messages)

-- Enum value changes
'grove' → 'project'         (scope_type, template/config scopes)
'grove_agents' → 'project_agents'  (group_type)
```
