# Phase 2 Dev: CLI --from-directory flag for skill batch-add

**Date:** 2026-07-30
**Branch:** `scion/project-skills-dirbatch`
**Agent:** ps-dirbatch-p2-dev

## Summary

Added the `--from-directory` CLI flag to both `scion project skills add` and
`scion user skills add`, enabling batch discovery and addition of skills from a
GitHub directory URL.

## Changes

### New files
- `pkg/hubclient/skill_discover.go` — client-side types and implementation for
  `POST /api/v1/skills/discover-directory`
- `pkg/hubclient/skill_discover_test.go` — happy path, error, and projectID
  forwarding tests

### Modified files
- `pkg/hubclient/client.go` — added `DiscoverSkillsDirectory` to the `Client`
  interface
- `cmd/project_skills.go` — `--from-directory` flag, `runProjectSkillsFromDirectory`
  function with TTY prompt, partial-failure handling
- `cmd/user_skills.go` — mirror of the project-scope version for user scope
  (no projectID in request, uses `UserInjectedSkills()`)
- `cmd/project_skills_test.go` — 7 new tests: flag registration, non-TTY adds-all,
  TTY+yes adds-all, TTY abort, no-skills, partial failure, discover error
- `cmd/user_skills_test.go` — 5 new tests: flag registration, non-TTY adds-all,
  TTY abort, no-skills, partial failure

## Verification

- `go build ./...` — clean
- `go vet ./cmd/... ./pkg/hubclient/...` — clean
- `go test ./pkg/hubclient/` — all pass (including 3 new discover tests)
- `go test ./cmd/` — all pass (including 12 new from-directory tests)
