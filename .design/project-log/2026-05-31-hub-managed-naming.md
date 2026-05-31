# Hub-managed naming convergence

**Date:** 2026-05-31
**PR:** #110
**Issue:** #96

## What

Converged all "hub-native" / "Hub Workspace" naming to the canonical "hub-managed" across the entire codebase: Go identifiers, comments, TypeScript types, web UI labels, documentation, design docs, and changelogs.

## Scope

- 65 files changed across Go source, TypeScript, and markdown
- Renamed 5 Go functions/identifiers, 1 constant (with value change), updated ~100 comments
- Changed `ProjectType` wire value from `"hub-native"` to `"hub-managed"`
- Updated web UI label from "Hub Workspace" to "Hub-managed Workspace"

## Key decisions

- **Wire value changed**: The `projectType` API field changes from `"hub-native"` to `"hub-managed"`. This is safe because the value is computed (not stored in DB), and the project is in alpha with no backward compatibility requirement.
- **Test fixture names updated**: Test data using "Hub Native" in project names was updated to "Hub Managed" to ensure auto-derived slugs match expected values.
- **Generic "hub workspace" left alone**: References like `cmd/sync.go`'s "Pull hub workspace to local" refer to the generic concept (workspace on the hub), not the product term, and were left unchanged.

## Process notes

- Bulk `replace_all` edits worked well for identifier renames within files
- Careful to verify test fixture strings that were bulk-renamed didn't break slug derivation (caught one test failure: `TestCreateProject_HubManaged_NoGitRemote`)
- Pre-existing CI format issues in `extras/` files are unrelated to this change
