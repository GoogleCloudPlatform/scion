# Rebase and review fixes for grove-to-project PR #281

**Date:** 2026-05-31
**Branch:** scion/cleanup-grove-to-project
**PR:** https://github.com/GoogleCloudPlatform/scion/pull/281

## Summary

Rebased the grove-to-project rename branch onto latest upstream main and addressed PR review feedback.

## Review comments addressed

1. **cmd/sciontool/commands/init_test.go** — Comment on line 18 still referenced `TestInitGroveDataIsolation` but the function had been renamed to `TestInitProjectDataIsolation`. Updated the comment to match.

2. **pkg/secret/localbackend_test.go** — Function name `TestLocalBackend_ResolveProgeny_GroveOverridesProgeny` and its comment still used "Grove" instead of "Project". Renamed to `TestLocalBackend_ResolveProgeny_ProjectOverridesProgeny`.

## Rebase conflict resolution

One conflict in `cmd/server_dispatcher.go` where upstream's "hub-native → hub-managed" rename (PR #280) overlapped with our "grove → project" rename. Resolved by combining both changes: using "hub-managed" (from upstream) and "project slug" (from our branch).

## Verification

- Build passes (`go build ./...`)
- No remaining conflict markers
- 14 commits cleanly rebased onto upstream/main
- Force-pushed to origin
