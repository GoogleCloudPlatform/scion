# Project Log: ps-dirbatch-p2-polish

**Date:** 2026-07-30
**Branch:** `scion/project-skills-dirbatch`
**Commit:** 0a44334
**Agent:** ps-dirbatch-p2-polish

## Summary

Phase 2 polish pass on the CLI `--from-directory` flag for `scion project skills add`
and `scion user skills add`. All findings from the Phase 2 quality gate reviews
(code, test, security) are addressed in a single commit.

## Changes

### Security fixes
- **Strip userinfo from directory URLs** (LOW-2): URLs like
  `https://token:secret@github.com/...` now have credentials removed before the
  discover request is sent. Applied to both `runProjectSkillsFromDirectory` and
  `runUserSkillsFromDirectory`.
- **Client-side URL validation** (LOW-1): New `looksLikeGitHubDirectoryURL()` helper
  rejects non-GitHub and non-HTTPS URLs locally before making a network round-trip.
  Table-driven test covers valid/invalid cases.

### Bug fixes
- **Context lifetime** (I-1): Replaced the single 60s context with separate 30s
  (discover) and 60s (add) contexts. The add timeout starts after the user responds
  to the prompt, not at function entry.
- **Nil-error wrapping** (M-1): Split the `err != nil || hubCtx == nil` check into
  two branches to avoid `fmt.Errorf("...: %w", nil)` producing a `<nil>` message.
- **Total failure error** (M-3): When all `svc.Add()` calls fail, the command now
  returns an error instead of exiting 0 with "Added 0 of N".

### UX improvements
- **Flag conflict checks** (M-2): `--as` and `--optional` with `--from-directory`
  now produce a clear error instead of being silently ignored.
- **Positional URI conflict** (M-4): Providing both a skill URI argument and
  `--from-directory` now errors instead of silently discarding the URI.

### Tests added
- `TestLooksLikeGitHubDirectoryURL` — table-driven validation helper test
- `TestRunProjectSkillsFromDirectory_StripsUserinfo` — asserts mock server receives
  no credentials
- `TestRunProjectSkillsFromDirectory_InvalidURL` — non-GitHub URL rejected locally
- `TestRunProjectSkillsAdd_FromDirConflictWithAs` — --as + --from-directory error
- `TestRunProjectSkillsAdd_FromDirConflictWithOptional` — --optional + --from-directory error
- `TestRunProjectSkillsAdd_FromDirConflictWithSkillURI` — positional URI + --from-directory error
- `TestRunProjectSkillsFromDirectory_TotalFailure` — all adds fail → error
- `TestRunUserSkillsFromDirectory_TTY_Yes_AddsAll` — autoConfirm=true bypasses prompt
- `TestRunUserSkillsFromDirectory_DiscoverError` — 500 from discover → error
- `TestRunUserSkillsFromDirectory_StripsUserinfo` — user-scope userinfo stripping
- `TestRunUserSkillsFromDirectory_InvalidURL` — user-scope URL validation
- `TestRunUserSkillsAdd_FromDirConflictWithAs` — user-scope flag conflict
- `TestRunUserSkillsFromDirectory_TotalFailure` — user-scope total failure

### Documentation
- Updated design doc `DiscoverSkillsResponse` Go type to include `Skipped` field.
- Added CLI section note about skipped directory count display.

## Verification

```
go build ./...          # pass
go vet ./cmd/...        # pass
go test ./cmd/ -count=1 # pass (5.7s)
go test ./pkg/hubclient/ -run TestDiscoverSkills -v # pass
```

## Files changed

- `cmd/project_skills.go` — fixes 1–7 + looksLikeGitHubDirectoryURL helper
- `cmd/project_skills_test.go` — 7 new tests + TestLooksLikeGitHubDirectoryURL
- `cmd/user_skills.go` — fixes 1–6 (user-scope mirrors)
- `cmd/user_skills_test.go` — 7 new tests
