# Independent Code Review v6: Grove-to-Project Rename

**Verdict:** REQUEST CHANGES
**Risk Level:** HIGH (due to CLI configuration regression)

## Overview
This review covers the large-scale rename of "grove" to "project" across the Scion codebase. While the migration is generally well-executed with impressive attention to backward compatibility in the API and Hub client, a critical regression was found in the CLI configuration path for versioned settings.

## Summary of Findings
- **CRITICAL/HIGH Issues:** 1
- **MEDIUM Issues:** 1
- **LOW/INFO Issues:** 2

---

## Critical Issues

### 1. Missing `project_id` and `hub.projectId` support in `UpdateVersionedSetting`
- **File:** `pkg/config/settings_v1.go`
- **Description:** The `UpdateVersionedSetting` and `GetVersionedSettingValue` functions (used by `scion config set/get`) do not support the new `project_id` or `hub.projectId` keys. They only support the legacy `grove_id` and `hub.groveId` keys.
- **Impact:** Once a project is initialized or migrated to the `v1` schema, users cannot set their project ID using the command `scion config set project_id <id>`. This is a significant regression as "project" is now the promoted nomenclature.
- **Suggested Fix:** Add cases for the new keys in the switch statements of both functions.
  ```go
  // pkg/config/settings_v1.go
  
  func UpdateVersionedSetting(dir string, key string, value string) error {
      // ...
      switch key {
      case "project_id", "grove_id": // Support both
          if vs.Hub == nil {
              vs.Hub = &V1HubClientConfig{}
          }
          vs.Hub.ProjectID = value
      case "hub.projectId", "hub.groveId": // Support both
          if vs.Hub == nil {
              vs.Hub = &V1HubClientConfig{}
          }
          vs.Hub.ProjectID = value
      // ...
  }
  ```

---

## Medium Issues

### 1. Duplicate cases in `resolveAPIPath`
- **File:** `pkg/hub/web.go` (lines 787-802)
- **Description:** The `resolveAPIPath` function contains duplicate `case` blocks for `/projects` and `/projects/{id}`.
- **Impact:** Code bloat and potential confusion. It also suggests that a intended case (perhaps for legacy `/groves` path SSR prefetching) was missed during the rename.
- **Suggested Fix:** Remove duplicates and consider adding legacy `/groves` support if SSR backward compatibility is desired.
  ```go
  case p == "/projects", p == "/groves": // Combine if needed
      return "/api/v1/projects"
  ```

---

## Suggestions & Observations

### 1. Outdated Comments
- **File:** `pkg/hubclient/types.go` (RegisterProjectRequest struct)
- **File:** `pkg/config/settings.go` (GetHubProjectID function)
- **Description:** Some comments still refer to "grove_id setting" or "grove ID".
- **Suggestion:** Update these comments to reflect the new "project" terminology.

### 2. `ResolvedSecret.MarshalJSON` Value Compatibility
- **File:** `pkg/api/types.go`
- **Observation:** The custom marshaler adds a field named `"grove": "grove"` when the source is `"project"`. However, the original field was `"source": "grove"`. 
- **Context:** Old clients checking `if (secret.source === 'grove')` will still fail because they will see `"source": "project"`. Adding a new field named `"grove"` only helps if those clients were updated to look for it, which defeats the purpose of seamless backward compatibility. This is not a blocker but an observation on the effectiveness of the compatibility shim.

---

## What's Done Well
- **API Backward Compatibility:** The Hub API handlers correctly handle both `/projects` and `/groves` endpoints and provide legacy fields in JSON responses.
- **Hub Client:** The `hubclient` package handles the transition smoothly, preferring the new fields but falling back to legacy ones.
- **Project Discovery:** `DiscoverProjects` robustly scans both `project-configs` and `grove-configs` directories, ensuring that existing user installations are not broken.
- **Build Quality:** The branch compiles cleanly and passes `go vet`, which is impressive for such a massive refactor.

---

## Verification Story
- **Build Verified:** `go build ./...` passes.
- **Lint/Vet Verified:** `go vet ./...` passes.
- **Code Audit:** Conducted deep dive into JSON marshaling logic and CLI configuration handlers. Verified `project_id` absence in `settings_v1.go` via `grep`.
- **Backward Compatibility:** Confirmed Hub API handlers maintain `/groves` routes and `groves` JSON keys.
