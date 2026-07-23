# P4 Fix5: Phase 4 Fifth-Pass Review Fixes

Date: 2026-07-22
Branch: scion/project-skills-phase2
PR: #544

## Fixes Applied

### NEW-1 (MEDIUM): isSkillURI validation missing in 2-arg path of runProjectSkillsAdd

**Problem:** `scion project skills add my-project notauri` (2 args: project name + skill) extracted
`args[1]` as `skillURI` but did not validate it with `isSkillURI()`. The invalid string silently
reached the API. The 1-arg path (via `splitProjectSkillsArgs`) already protected against this by
treating non-URI, non-UUID 1-arg inputs as the project name (returning `skillURI = ""`), but the
2-arg path returned `args[1]` verbatim with no URI check.

**Fix:** Added `isSkillURI` guard immediately after the empty-check in `runProjectSkillsAdd`:

```go
if !isSkillURI(skillURI) {
    return fmt.Errorf("expected a skill URI (containing ://), got %q", skillURI)
}
```

This mirrors the validation already present in `runUserSkillsAdd`.

### NEW-2 (LOW): Error message quoted args[0] (project name) instead of skill URI

**Problem:** The new validation for the 2-arg invalid-URI path must quote the skill URI value
(`args[1]` / `skillURI`), not the project name (`args[0]`). If `args[0]` were used, the message
would mislead the user by showing the project name as the "bad" value.

**Fix:** The `isSkillURI` guard (from NEW-1) uses the `skillURI` variable (which equals `args[1]`
in the 2-arg case), so the error message naturally quotes the correct value.

## Test Added

`TestRunProjectSkillsAdd_NoURIError_TwoArgs` in `cmd/project_skills_test.go`:
- Invokes `runProjectSkillsAdd` with `["my-project", "notauri"]`
- Asserts an error is returned
- Asserts the error message contains `://` (URI format indicator) and `"notauri"` (the invalid arg), not the project name
- Asserts the hub mock server was never contacted

## Verification

- `go build ./...` — clean
- `gofmt -l .` — empty (no formatting issues)
- `go test -tags sqlite ./cmd/... ./pkg/hubclient/...` — all pass
