# I-1: Fix Broken Deprecation Warning Replacements

**Date:** 2026-08-27
**Branch:** scion/dev-i1-warnings (based on scion/ca-msg-em5)
**Issue:** Three deprecation warnings in `cmd/message.go` name replacements that do not exist in the shipped binary (violates AC-15a).

## Problem

1. `--in` warning said "use 'scion schedule message'" — `schedule` has no `message` subcommand
2. `--at` warning said "use 'scion schedule message'" — same
3. `--cc` warning said "use --to instead" — `--to` is not a registered flag

## Changes

### cmd/message.go
- Line 81: `"use 'scion schedule message' instead"` → `"use 'scion schedule create --in' instead"`
- Line 84: `"use 'scion schedule message' instead"` → `"use 'scion schedule create --at' instead"`
- Line 93: `"use --to instead"` → `"--cc is deprecated and will be removed"`
- Line 1199: flag help text updated to match (`scion schedule create --in`)
- Line 1200: flag help text updated to match (`scion schedule create --at`)
- Line 1206: flag help text updated to match (deprecated, will be removed)

### cmd/message_deprecation_test.go
- `TestDeprecatedFlag_In`: assertion updated from `"scion schedule message"` to `"scion schedule create"`
- `TestDeprecatedFlag_At`: same
- `TestDeprecatedFlag_CC`: assertion updated from `"use --to instead"` to `"deprecated and will be removed"`
- Added `TestDeprecationWarnings_ReplacementsExist`: validates every `'scion ...'` reference in deprecation warnings resolves via `rootCmd.Find()`
- Added `catches_nonexistent_replacement` mutation subtest (Rule 10)

### Documentation
- `docs-site/src/content/docs/reference/cli.md`: updated `--in`, `--at`, `--cc` entries
- `docs-site/src/content/docs/hosted/user/messaging.md`: updated deprecation table
- `resources/platform_skills/scion-messaging/SKILL.md`: updated deprecation table and self-callback tip

## Verification
- `go test -count=1 ./cmd/ -v -run 'TestDeprecation|TestDeprecatedFlag'` — all 18 tests pass
- `go test -count=1 ./cmd/` — full suite passes
