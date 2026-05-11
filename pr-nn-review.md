# Code Review: Grove-to-Project Rename (v3)

## Review Summary

**Verdict:** REQUEST CHANGES

**Overview:** The rename is very comprehensive and well-implemented across the core codebase. Backward compatibility for JSON, REST paths, and events is robust. However, two critical omissions in the database migration and the A2A bridge, along with some missing legacy fields in API responses, must be addressed.

**Findings Count:**
- CRITICAL: 2
- HIGH: 1
- MEDIUM: 2
- LOW: 2
- INFO: 1

---

### Critical Issues

- **[pkg/store/sqlite/sqlite.go:1155 (migrationV48)]** The `gcp_service_accounts` table is missing from the column rename list. 
  - **Issue:** The `grove_id` column is not renamed to `project_id`. However, the Go code in `pkg/store/sqlite/gcp_service_account.go` has been updated to use `project_id` in all queries.
  - **Impact:** Any operation involving GCP service accounts (creation, listing, deletion) will fail with "no such column: project_id" on upgraded installations.
  - **Suggested Fix:** Add `ALTER TABLE gcp_service_accounts RENAME COLUMN grove_id TO project_id;` to `migrationV48`. Also, drop and recreate the index: `DROP INDEX IF EXISTS idx_gcp_sa_grove; CREATE INDEX IF NOT EXISTS idx_gcp_sa_project ON gcp_service_accounts(project_id);`

- **[extras/scion-a2a-bridge/]** The entire A2A bridge component was missed in the rename.
  - **Issue:** Files like `internal/state/state.go` still create tables (`tasks`, `contexts`) with `grove_id` columns. Config and logic still use "grove" terminologies and env vars.
  - **Impact:** Inconsistency across the codebase. If the bridge is part of the Scion ecosystem, it should align with the "project" terminology to avoid confusion and potential integration breakages where IDs are passed.
  - **Suggested Fix:** Perform a rename pass on the `extras/scion-a2a-bridge` directory, including its internal SQLite schema and configuration structures.

---

### High Issues

- **[pkg/hub/handlers.go:2845, 2881]** `ListProjectsResponse` and `RegisterProjectResponse` are missing the legacy `groves` / `grove` fields.
  - **Issue:** While `hubclient` has been updated to handle both, older clients (or older versions of the library) talking to a newer hub will fail to find the project data because the hub only sends the `projects` or `project` key.
  - **Suggested Fix:**
    - Update `ListProjectsResponse` struct to include `LegacyGroves []ProjectWithCapabilities `json:"groves,omitempty"``.
    - Update `RegisterProjectResponse` struct to include `LegacyProject *store.Project `json:"grove,omitempty"``.
    - Populate these fields in `listProjects` and `handleProjectRegister` respectively.

---

### Medium Issues

- **[pkg/projectsync/projectsync.go:138]** The WebDAV URL still uses the `/api/v1/groves/` path.
  - **Issue:** `return fmt.Sprintf("%s/api/v1/groves/%s/dav", base, groveID)`. 
  - **Impact:** While the hub supports this via an alias, the updated CLI should prefer the new `/projects/` path for consistency.
  - **Suggested Fix:** Change to `%s/api/v1/projects/%s/dav`.

- **[pkg/api/types.go:375]** `api.ResolvedSecret.UnmarshalJSON` is missing dual-field support for the `source` value.
  - **Issue:** `UnmarshalJSON` correctly maps `"source": "grove"` to `"project"`, but `MarshalJSON` will only ever send `"project"`.
  - **Impact:** If an old client strictly checks for `"source": "grove"`, it may break. Given the "dual-field" mandate, we should consider if this value change constitutes a breaking change for subscribers.

---

### Low Issues

- **[pkg/hub/project_webdav.go:77]** Typo in comment: `// The full URL path is /api/v1/{projects|projects}/{id}/dav/...`. Should be `{projects|groves}`.
- **[pkg/projectsync/projectsync.go:68, 86]** Inconsistent naming and error messages. The `Sync` function still refers to "grove ID" in error messages and the parameter name in `buildWebDAVURL` is `groveID`.

---

### What's Done Well

- **Comprehensive Fallbacks:** The fallback logic in `pkg/config/project_discovery.go` and `pkg/hubclient/client.go` is excellent and ensures a smooth transition for local files and REST clients.
- **Dual Event Publishing:** Implementing dual-publishing for SSE events ensures that existing real-time subscribers continue to function.
- **CLI Aliases:** Adding `grove` as an alias for the `project` command ensures muscle memory and scripts are not broken immediately.

---

### Verification Story

- **Tests reviewed:** Yes. `pkg/api/types_test.go` and `pkg/hubclient/projects_test.go` verify the dual-field JSON marshaling.
- **Build verified:** N/A (Manual review focused on logic and renames).
- **Security checked:** Yes. Verified that column renames in SQLite migration use the correct `project_id` name that matches the updated Ent schema and hub handlers.
- **Migration logic:** SQL renames were verified against the table list. The missing `gcp_service_accounts` table was identified by cross-referencing all `CREATE TABLE` statements with `V48`.
