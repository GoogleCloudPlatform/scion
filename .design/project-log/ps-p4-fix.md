# P4 Review Fix Log — PR #544

Date: 2026-07-22
Branch: scion/project-skills-phase2

## Fixes Applied

### MEDIUM-1: Missing url.PathEscape in Remove methods
**File:** `pkg/hubclient/injected_skills.go` (lines 110, 152)

Both `projectInjectedSkillsService.Remove` and `userInjectedSkillsService.Remove` concatenated
`entryID` directly into the URL path. Wrapped both with `url.PathEscape()` to match every
other per-item DELETE in the package. Added `"net/url"` to imports.

```
- resp, err := s.c.delete(ctx, s.basePath()+"/"+entryID, nil)
+ resp, err := s.c.delete(ctx, s.basePath()+"/"+url.PathEscape(entryID), nil)
```

### MEDIUM-2: Timeout context created after hub probe
**File:** `cmd/user_skills.go` (functions `runUserSkillsList`, `runUserSkillsAdd`, `runUserSkillsRemove`)

All three `runUserSkills*` functions called `resolveUserSkillsService()` before creating the
30s timeout context, leaving the hub probe (EnsureHubReady) with no deadline.

- Changed `resolveUserSkillsService()` → `resolveUserSkillsService(ctx context.Context)`
  to accept the bounded context from the caller (consistent with `resolveProjectSkillsClient`).
- Moved `context.WithTimeout` creation to the top of each run function, before the service
  resolver call.

`cmd/project_skills.go` already creates ctx before calling `resolveProjectSkillsClient`, so
no change needed there.

### LOW-3: resolveInjectedSkillEntryID missing UUID guard
**File:** `cmd/project_skills.go` (line ~315, `resolveInjectedSkillEntryID`)

The passthrough branch silently forwarded any non-URI string as a raw URL path segment.
Added an `isUUIDLike()` guard; non-URI, non-UUID strings now return a descriptive error:

```go
if !isUUIDLike(ref) {
    return "", fmt.Errorf("invalid skill entry ID: %q (expected UUID or skill URI)", ref)
}
```

### LOW-4: Misleading error message for plain-slug args
**File:** `cmd/project_skills.go` (line ~229, `runProjectSkillsAdd`)

When a single non-URI, non-UUID arg was passed to `add`, it was silently treated as the
project name and the error `"skill URI is required"` gave no hint that the arg was
misinterpreted. New message:

```go
return fmt.Errorf("expected a skill URI (containing ://), got %q", projectArg)
```

Updated `TestRunProjectSkillsAdd_NoURIError` in `project_skills_test.go` to match.

### LOW-5a: isSkillURI() reimplementation
**File:** `cmd/project_skills.go` (line ~166, `isSkillURI`)

Replaced 7-line manual byte loop with `strings.Contains`:

```go
func isSkillURI(s string) bool {
    return strings.Contains(s, "://")
}
```

Added `"strings"` to imports.

### LOW-5b: Unused package-level flag vars
**Files:** `cmd/user_skills.go`, `cmd/project_skills.go`

Package-level vars `userSkillsAs`/`userSkillsOptional` and `projectSkillsAs`/`projectSkillsOptional`
were bound via `Flags().StringVar`/`BoolVar` but the run functions then called
`cmd.Flags().GetString`/`GetBool` instead, making the vars dead writes.

Switched the run functions to use the bound vars directly:
```go
- as, _ := cmd.Flags().GetString("as")
- optional, _ := cmd.Flags().GetBool("optional")
+ as := userSkillsAs      // or projectSkillsAs
+ optional := userSkillsOptional  // or projectSkillsOptional
```

## Verification

- `go build ./...` — clean
- `gofmt -l` — no output (clean formatting)
- `go test -tags sqlite ./cmd/... ./pkg/hubclient/...` — all pass
