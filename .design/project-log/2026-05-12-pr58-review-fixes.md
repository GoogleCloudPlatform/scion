# PR #58 Review Feedback Fixes

**Date:** 2026-05-12
**PR:** #58 (feat: add download as ZIP for shared directories)
**Branch:** scion/fix-issue-39

## What changed

Addressed two medium-severity review comments from Gemini Code Assist and scion-gteam:

1. **Extracted `writeDirectoryToZip` helper** — The zip streaming logic in `handleProjectWorkspaceArchive` and `handleProjectSharedDirArchive` was nearly identical. Extracted it into a shared `writeDirectoryToZip(zw *zip.Writer, dirPath string) error` function to reduce duplication and improve maintainability.

2. **Added `slog.WarnContext` on archive streaming errors** — Both archive handlers now log a warning when `WalkDir` fails mid-stream. Since HTTP headers are already sent at that point, the client receives a truncated ZIP with no way to report the error via HTTP status. Server-side logging ensures these failures are visible for debugging.

## Verification

- `go build ./...` — passes
- `go test ./pkg/hub/...` — all tests pass
