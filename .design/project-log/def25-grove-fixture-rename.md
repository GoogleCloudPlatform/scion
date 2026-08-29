# DEF-25: Rename grove-* fixture project IDs in test files

**Date:** 2026-08-27
**Agent:** dev-def25-compat
**Branch:** scion/ca-msg-em6

## Summary

Renamed 11 test fixture project IDs across two test files from legacy `grove-*`
vocabulary to `proj-*` vocabulary, making the `compat-literals` CI gate pass
(exit 0) on this branch.

## Changes

### `cmd/message_deprecation_test.go` (7 renames)

| Before | After |
|---|---|
| grove-depr-bcast | proj-depr-bcast |
| grove-depr-in | proj-depr-in |
| grove-depr-at | proj-depr-at |
| grove-depr-bcast-works | proj-depr-bcast-works |
| grove-depr-notify-works | proj-depr-notify-works |
| grove-depr-plain-works | proj-depr-plain-works |
| grove-depr-channel-works | proj-depr-channel-works |

### `cmd/broadcast_test.go` (4 renames)

| Before | After |
|---|---|
| grove-bcast-project | proj-bcast-project |
| grove-bcast-all | proj-bcast-all |
| grove-bcast-empty | proj-bcast-empty |
| grove-bcast-interrupt | proj-bcast-interrupt |

## Verification

- `bash hack/check-project-compat-literals.sh` exits 0
- `go test ./cmd/ -run 'TestBroadcastCmd|TestDeprecation' -count=1` — all tests pass
- No `grove` literals remain in either file
- These were internal test fixture IDs with no external dependencies
