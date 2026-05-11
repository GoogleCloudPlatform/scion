# Grove → Project Rename: Strategy Document

**Date:** 2026-05-09
**Status:** Proposal
**Author:** Scion Agent (rename-strategy)

---

## 1. Executive Summary

Rename the internal concept "grove" to "project" across the Scion codebase. This affects ~20,700 references across ~696 files spanning Go backend, Ent ORM, SQLite schema, REST API paths, WebSocket protocol, CLI commands/flags, web frontend, container labels, environment variables, filesystem paths, telemetry attributes, docs, and design documents.

The rename is a high-risk, high-coordination change. The recommended approach is **phased incremental** — internal plumbing first (with backward-compatible shims), then API/CLI surface, then database, then docs — rather than a big-bang rewrite.

---

## 2. Scope Assessment (Audited 2026-05-09)

### 2.1 Quantitative Summary

| Category | Files | Approx. Refs | Risk |
|---|---|---|---|
| Go symbols (types, funcs, vars, consts) | ~300 | ~8,000 | Medium |
| Ent ORM (generated + schema) | 32 | ~1,400 | High |
| Hub server (handlers, authz, cache, settings) | 105 | ~6,500 | High |
| CLI commands & flags (`cmd/`) | 68 | ~2,500 | High |
| Runtime broker | 22 | ~1,100 | High |
| Config package (paths, settings, discovery, marker) | 35 | ~1,600 | High |
| API/JSON struct tags (`json:"grove*"`) | ~50 fields | ~60 | **Critical** |
| REST API paths (`/api/v1/groves/...`) | ~40 routes | ~70 | **Critical** |
| WebSocket protocol fields | 3 | ~15 | **Critical** |
| SQLite schema (tables, columns, indexes, FKs) | 1 | ~100 | **Critical** |
| SQLite migrations (V1–V40+) | 1 | ~50 | **Critical** |
| Store interface & models | 22 | ~1,500 | High |
| Container labels (`scion.grove*`) | ~6 | ~30 | High |
| Environment variables (`SCION_GROVE*`) | ~10 vars | ~40 | **Critical** |
| Filesystem paths (`groves/`, `grove-configs/`, `.scion/grove-id`) | ~20 refs | ~40 | High |
| YAML/koanf config keys (`grove_id`, `groveId`) | ~8 | ~20 | **Critical** |
| Hub client (`pkg/hubclient/groves.go`) | 18 | ~350 | High |
| Web frontend (TypeScript/JS) | 55 | ~1,200 | Medium |
| Agent package | 15 | ~400 | Medium |
| Runtime package | 12 | ~150 | Medium |
| Docs site | 35 | ~400 | Low |
| Design docs | 150 | ~3,500 | Low |

**Grand Total: ~20,695 references across ~696 files**

### 2.2 Key Files and Directories

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
web/src/components/pages/grove-create.ts, grove-detail.ts,
    grove-schedules.ts, grove-settings.ts, groves.ts
.scion/grove-id (marker file)
```

---

## 3. High-Risk Areas & Mitigation Strategies

### 3.1 REST API & Wire Compatibility (Critical)
**Risk:** Rolling deployments (Hub updated, Broker/CLI not yet updated) will break if paths or fields are renamed without aliases.
**Mitigation:**
- Register both `/api/v1/groves` and `/api/v1/projects` on the Hub.
- Support both `groveId` and `projectId` in JSON requests via custom unmarshaling or dual-field structs.
- Brokers must be updated to prefer `project` but fallback to `grove` for older Hubs.

### 3.2 Database Migration (Critical)
**Risk:** SQLite `ALTER TABLE ... RENAME COLUMN` is only supported in versions 3.25.0+. Scion's `modernc.org/sqlite` supports it, but Ent's migration engine needs careful orchestration to avoid data loss during table renames.
**Mitigation:**
- Use a dedicated migration phase with hand-written SQL where Ent's default behavior is too aggressive (e.g., dropping and recreating tables).
- Verify foreign key constraints are correctly updated across the schema.

### 3.3 Environment Variables & Container Labels (High)
**Risk:** Tools like `sciontool` and the agent runtime rely on `SCION_GROVE_ID` and `scion.grove_id` for telemetry and discovery.
**Mitigation:**
- Export both old and new variables/labels for a transition period.
- Ensure `sciontool` probes for `SCION_PROJECT_ID` first, then `SCION_GROVE_ID`.

### 3.4 Filesystem State (High)
**Risk:** Existing projects have `.scion/grove-id` files and `~/.scion/grove-configs/` directories.
**Mitigation:**
- Add logic to `pkg/config` to look for `.scion/project-id` then `.scion/grove-id`.
- Auto-migrate local filesystem paths (or use symlinks) when a project is first accessed with the new version.

---

## 4. Phased Implementation Plan

### Phase 0: Preparation & Baseline
- **Goal:** Establish verification suite and validation tooling.
- **Tasks:**
  - Create a "Rename Validation Script" (`hack/validate-rename.sh`) that checks for residual `grove` strings while ignoring valid exceptions.
  - Snapshot existing integration tests and ensure they all pass.
- **Definition of Done:** `hack/validate-rename.sh` exists and reports all 20,700 current occurrences.

### Phase 1: Internal Go Plumbing (No Wire Changes)
- **Goal:** Rename Go-internal symbols while maintaining external compatibility.
- **Detailed Decomposition:** See [.scratch/phase1-decomposition.md](../.scratch/phase1-decomposition.md) for a full sub-task breakdown.
- **Tasks:**
  - Rename Go types, funcs, vars: `Grove` → `Project`, `GroveID` → `ProjectID`.
  - Rename packages: `pkg/grovesync` → `pkg/projectsync`.
  - **Constraint:** Keep `json`, `yaml`, `koanf` tags and API paths as `grove` for now.
- **Definition of Done:** `go build ./...` and `go test ./...` pass. No `Grove` symbols remain in Go code (except in tags/strings).

### Phase 2: CLI User Interface
- **Goal:** Expose the "Project" terminology to users.
- **Tasks:**
  - Rename `scion grove` → `scion project`.
  - Add `grove` as a hidden alias to all project commands.
  - Rename flags: `--grove` → `--project` (with hidden alias).
  - Update help text, error messages, and `scion init` aliases.
- **Definition of Done:** `scion project list` works. `scion grove list` still works but is hidden from help.

### Phase 3: API Surface & Wire Protocol (Dual-Support)
- **Goal:** Enable the new API while maintaining backward compatibility.
- **Tasks:**
  - Hub: Register `/api/v1/projects/...` routes as clones of `/api/v1/groves/...`.
  - Types: Add `Project*` JSON tags to structs. Use custom `UnmarshalJSON` to accept either `groveId` or `projectId`.
  - Environment: Set `SCION_PROJECT_ID` in addition to `SCION_GROVE_ID`.
- **Definition of Done:** Hub responds to both `/api/v1/groves` and `/api/v1/projects`. Broker can connect to Hub using either.

### Phase 4: Database Schema Migration
- **Goal:** Migrate the source of truth to the new naming.
- **Tasks:**
  - Write migration `V41`: Rename tables (`groves` → `projects`, etc.) and columns (`grove_id` → `project_id`).
  - Update Ent schema and regenerate: `Project` entity with `projects` table name.
- **Definition of Done:** Migration succeeds on fresh and existing databases. All `pkg/store` tests pass.

### Phase 5: Filesystem & Container Runtime
- **Goal:** Update the on-disk and containerized naming.
- **Tasks:**
  - Update container labels: `scion.project_id` (primary), `scion.grove_id` (secondary).
  - Update filesystem paths: `.scion/project-id` and `~/.scion/project-configs/`.
  - Implement "lazy migration" for existing local projects.
- **Definition of Done:** `scion init` creates a project with new path names. Existing groves are correctly discovered and handled.

### Phase 6: Web Frontend & Documentation
- **Goal:** Complete the user-facing rename.
- **Tasks:**
  - Web: Rename TS types and components. Update API calls to `/api/v1/projects`.
  - Docs: Bulk update `grove` → `project` in `.md` files. Rename doc files.
- **Definition of Done:** Web UI shows "Projects". No "Grove" mentions in user-facing documentation.

---

## 5. Recommended Sub-Design Docs

Before starting implementation, the following deep-dives are required:

1. **DB Migration Strategy:** Detailed SQL for SQLite table/column renames, including handling of foreign keys, indexes, and Ent ORM integration.
2. **Wire Protocol Versioning:** Specific plan for JSON alias support and Broker ↔ Hub handshake compatibility during the transition.
3. **Filesystem Migration Plan:** How to safely transition `~/.scion/grove-configs/` to `~/.scion/project-configs/` without breaking multi-version CLI usage.
4. **CLI Alias & Deprecation Policy:** Timeline for how long `scion grove` aliases will be maintained (e.g., "until v1.0.0").

---

## 6. Definition of Done (Overall)

The rename is considered complete when:
- [ ] `grep -r -i "grove"` returns 0 results (excluding the strategy doc and changelog).
- [ ] A fresh user can install Scion and use the `project` command exclusively.
- [ ] An existing user can upgrade Scion and their existing data is migrated or accessible.
- [ ] All CI checks pass, including integration tests spanning CLI, Broker, and Hub.
- [ ] The web UI and documentation are fully updated.
