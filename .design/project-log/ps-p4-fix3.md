# Phase 4 Third-Pass Review Fixes (ps-p4-fix3)

Branch: scion/project-skills-phase2  
Date: 2026-07-22

## Fixes Applied

### NEW-1 (MEDIUM): isSkillURI validation in user skills add
**File:** `cmd/user_skills.go:146-150`

Added `isSkillURI` guard at the top of `runUserSkillsAdd` before the hub service call:
```go
if !isSkillURI(skillURI) {
    return fmt.Errorf("skill URI is required (expected format containing ://), got %q", skillURI)
}
```
Also added a corresponding test `TestRunUserSkillsAdd_NoURIError` in `cmd/user_skills_test.go`.

### NEW-2 (LOW): Register user.skills.* and project.skills.* in cli_mode.go
**File:** `cmd/cli_mode.go:90-99`

Added 10 entries to `agentAllowed` covering both parent commands and all leaf subcommands:
- `user`, `user.skills`, `user.skills.list`, `user.skills.add`, `user.skills.remove`
- `project`, `project.skills`, `project.skills.list`, `project.skills.add`, `project.skills.remove`

Parent commands must be explicitly listed because `removeCommands` evaluates each path individually; unlisted parents are pruned even if their children are allowed.

### NEW-3 (LOW): Replace grove_id with project_id in test fixtures
**File:** `cmd/project_skills_test.go:168`  
**File:** `cmd/user_skills_test.go:92`

Replaced `"grove_id"` key with `"project_id"` in both test fixture `settings` maps, per the CLAUDE.md guardrail: "project_id, not legacy grove_id".

### INFO-1 (non-blocking): Fix misleading error message in runProjectSkillsAdd
**File:** `cmd/project_skills.go:225-227`

Changed error message from:
```go
return fmt.Errorf("expected a skill URI (containing ://), got %q", projectArg)
```
to:
```go
return fmt.Errorf("skill URI is required (expected format containing ://), got %q", args[0])
```
Uses `args[0]` explicitly (the actual user input, not the parsed project variable) and aligns the message format with the NEW-1 user-skills error and the general pattern in the codebase.

Updated corresponding test assertion in `cmd/project_skills_test.go:373` to match.

### INFO-2 (non-blocking): Replace isUUIDLike with uuid.Parse
**File:** `cmd/project_skills.go:171-175` (after edit)

Replaced the hand-rolled hex/dash UUID validator with:
```go
func isUUIDLike(s string) bool {
    _, err := uuid.Parse(s)
    return err == nil
}
```
Added `"github.com/google/uuid"` import (same package used by `cmd/hub_env.go`). This eliminates the QF1001 lint finding (De Morgan negation) and deduplicates UUID parsing logic.

### Additional lint fixes (from CI feedback)
**QF1002**: Converted three untagged `switch { case r.URL.Path == ...: }` handlers to tagged `switch r.URL.Path { case ...: }` in:
- `cmd/project_skills_test.go`: `TestRunProjectSkillsList_Empty` (line 257), `TestRunProjectSkillsList_APIError` (line 448)
- `cmd/user_skills_test.go`: `TestRunUserSkillsList_Empty` (line 178), `TestRunUserSkillsList_APIError` (line 354)

## Verification
- `go build ./...` — clean
- `gofmt -l .` — empty (no formatting issues)
- `go test -tags sqlite ./cmd/... ./pkg/hubclient/...` — all pass
