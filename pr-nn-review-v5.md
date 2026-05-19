# Code Review v5: Grove-to-Project Rename Strategy

## Review Summary

**Verdict:** REQUEST CHANGES

**Overview:** While the rename strategy has progressed significantly, this review identified several **CRITICAL** bugs that break core functionality (settings loading, A2A messaging) and several **HIGH** severity omissions in backward compatibility for the Hub API. The branch is currently unstable as evidenced by multiple test failures in `pkg/config`.

**Findings Count:**
- CRITICAL: 2
- HIGH: 4
- MEDIUM: 1
- LOW/INFO: 2

---

### Critical Issues

#### 1. Settings Loading for Project ID is Broken [CRITICAL]
- **File:** `pkg/config/koanf.go` (Multiple lines)
- **Issue:** The remapping logic in `LoadSettingsKoanf` still uses `grove_id` and `hub.groveId` as target keys, but the `Settings` and `HubClientConfig` structs have been updated to use `project_id` and `projectId` tags.
- **Impact:** Project IDs are not correctly loaded from `.scion/project-id` files or environment variables like `SCION_HUB_GROVE_ID`. This causes `scion` to fail to recognize linked projects.
- **Evidence:** Multiple tests in `pkg/config/koanf_test.go` fail with `expected ProjectID ..., got ''`.
- **Suggested Fix:**
  - Update `koanf.go` to use `project_id` and `hub.projectId` in all `confmap.Provider` remapping calls.
  - Ensure `SCION_HUB_PROJECT_ID` environment variable is also supported.

#### 2. A2A Bridge Messaging is Broken [CRITICAL]
- **File:** `extras/scion-a2a-bridge/internal/bridge/bridge.go:270, 935`
- **Issue:** The A2A bridge was updated to subscribe to `scion.project.*` topics, but the Hub and Broker (in `pkg/broker/broker.go`) still publish exclusively to `scion.grove.*` topics.
- **Impact:** The A2A bridge will never receive messages from agents, breaking the entire protocol flow.
- **Suggested Fix:** 
  - Update `pkg/broker/broker.go` to dual-publish to both `scion.project` and `scion.grove` topics, or keep the bridge on `scion.grove` until the wire protocol is officially migrated.

---

### High Issues

#### 3. Hub API Missing Backward Compatibility for Projects & Agents [HIGH]
- **File:** `pkg/store/models.go`
- **Issue:** The core `Project` and `Agent` structs are missing `MarshalJSON` implementations to provide legacy `groveId`, `groveName`, and `grove` fields.
- **Impact:** REST clients (like older CLI versions) expecting these fields in Hub responses will break or show missing data.
- **Evidence:** Custom integration test `pkg/hub/compat_test.go` failed (Response missing `groveId` and `grove`).
- **Suggested Fix:** Implement `MarshalJSON` and `UnmarshalJSON` for `store.Project` and `store.Agent` similar to how it was done for `Schedule` and `Message`.

#### 4. Project Registration Broken for Old Clients [HIGH]
- **File:** `pkg/hub/handlers.go:2864`
- **Issue:** `RegisterProjectRequest` struct uses `json:"id"` but lacks custom unmarshaling for the legacy `groveId` or `grove_id` keys.
- **Impact:** Old CLI versions performing `hub register` will fail to pass the project ID to the Hub.
- **Suggested Fix:** Add custom `UnmarshalJSON` to `RegisterProjectRequest`.

#### 5. ProjectProvider Backward Compatibility Missing [HIGH]
- **File:** `pkg/store/models.go:266`
- **Issue:** `ProjectProvider` (formerly `GroveContributor`) uses `json:"projectId"` but lacks custom marshaling for the legacy `groveId` field.
- **Impact:** Inconsistency in API responses for broker/project links.

#### 6. Project Initialization Botches V1 Settings [HIGH]
- **File:** `pkg/config/init.go:516`
- **Issue:** `writeProjectSettings` writes `project_id` into the `hub` section for V1 settings, but `V1HubClientConfig` (in `pkg/config/settings_v1.go`) still uses `koanf:"grove_id"`.
- **Impact:** Newly initialized V1 projects won't load their Hub Project ID correctly.
- **Suggested Fix:** Synchronize the key name used in `init.go` with the tags in `settings_v1.go`.

---

### Medium Issues

#### 7. Redundant Index Operations in Migration V48 [MEDIUM]
- **File:** `pkg/store/sqlite/sqlite.go:1155`
- **Issue:** Migration V48 attempts to `DROP INDEX IF EXISTS ..._grove` and `CREATE INDEX IF NOT EXISTS ..._project` for several tables (e.g., `messages`, `groups`) where the indexes were ALREADY named `..._project` in previous migrations (e.g., V18, V31).
- **Impact:** The `DROP` fails silently (correct), and the `CREATE` does nothing because the index exists. While harmless, it's confusing and sloppy.
- **Suggested Fix:** Verify index names in earlier migrations and only drop/recreate those that actually contain "grove" in the DB schema.

---

### Low / Info Issues

#### 8. Bug in ResolvedSecret.MarshalJSON [LOW]
- **File:** `pkg/api/types.go:680` (added in commit `c4b316ce`)
- **Issue:** The code sets `grove = "project"` if `s.Source == "project"`.
- **Suggested Fix:** It should set `grove = "grove"`.

#### 9. Leftover 'g' Receivers [LOW]
- **File:** `pkg/store/models.go:204`
- **Issue:** Method `IsSharedWorkspace` still uses `g *Project` as receiver.

---

### What's Done Well

- **Comprehensive CLI Aliases:** The addition of `cd-grove` and persistence of `-g` flag for `--project` is a great UX touch.
- **Dual Event Publishing:** Internal Hub events (`events.go`) correctly dual-publish to both `project.*` and `grove.*` subjects.
- **A2A Bridge Porting:** The rename within the A2A bridge (except for the topic mismatch) was very thorough, covering metrics and internal state.

---

### Verification Story
- **Tests reviewed:** YES. Identified multiple failures in `pkg/config/koanf_test.go` confirming regression.
- **Build verified:** YES. Verified compilation of Hub and CLI.
- **API Compatibility:** YES. Verified via a custom test (`pkg/hub/compat_test.go`) that `Project` and `Agent` objects are missing legacy fields.
- **Messaging checked:** YES. Identified topic mismatch between A2A bridge and Hub Broker.
