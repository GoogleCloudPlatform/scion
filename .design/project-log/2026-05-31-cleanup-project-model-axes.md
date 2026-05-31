# Cleanup: project model axes — stale comments and ProjectType

**Date:** 2026-05-31
**Issue:** #97
**PR:** #103

## What was done

Explored the project data model to confirm five problems described in the cleanup brief:

1. **Stale deterministic-UUID language** — Confirmed in 6 locations across `settings.go`, `sync.go`, `hub.go`, `hub_secret_test.go`, and `git.go`. All references to "deterministic UUID v5" project IDs were outdated; `GenerateProjectID()` already produces random UUIDs.

2. **Muddled ProjectType** — Confirmed two separate `ProjectType` systems: local discovery (`global`/`git`/`external` in `project_discovery.go`) vs Hub model (`linked`/`hub-native` in `models.go`). The `Project.ProjectType` field comment incorrectly listed `"git"` as a possible value for the Hub model.

3. **Conflated axes** — Confirmed workspace ownership, git-backing, and workspace sharing are tangled. `IsSharedWorkspace()` only returns true for git+shared projects, even though hub-managed non-git projects also share a workspace.

4. **Runtime git-backing inference** — Confirmed code branches on `.git` directory presence at runtime in `provision.go` and `init.go`, rather than using a declared project attribute.

5. **Non-universal workspace sharing** — Confirmed workspace mode label is only applied for git projects in `handlers.go`.

## Deliverables

- **Design issue #97** — Full three-axis design proposal covering workspace ownership, git-backing as explicit metadata, and universal workspace sharing modes with migration strategy.
- **PR #103** — Phase 1 implementation: comment-only cleanup removing stale deterministic-UUID language and fixing the ProjectType field comment. No behavioral changes.

## Observations

- `HashProjectID` is retained for cache-key derivation despite no longer being used for project IDs. The function itself is clean; only its comments needed updating.
- The `populateProjectType()` function in `sqlite.go` already correctly produces only `"linked"` or `"hub-native"` — the stale `"git"` value in the field comment was the only discrepancy.
- Several pre-existing test failures exist on `main` in `pkg/config`, `pkg/hubsync`, and `cmd` packages (environment-dependent tests). These are unrelated to this change.
