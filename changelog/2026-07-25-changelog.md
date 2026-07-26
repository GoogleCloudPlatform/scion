# Release Notes (2026-07-25)

Workspace sharing mode is now surfaced to agents via environment variables, private `gh://` skill resolution was fixed to eliminate unauthenticated double-downloads, and the Discord observe mode filter was rewritten to fail closed using proper thread-to-parent channel link resolution.

## 🚀 Features
* **[Agent]:** Surface workspace sharing mode to agents via `SCION_WORKSPACE_MODE` and `SCION_WORKSPACE_GIT` environment variables — injected at both hub dispatch and broker start layers so agents can adapt behavior to exclusive/shared/git workspace configurations (#873).

## 🐛 Fixes
* **[Skills]:** Eliminate unauthenticated double-download causing 404 on private `gh://` skills — the resolver was fetching once with credentials then re-downloading without, failing on private repos (#865).
* **[Skills]:** Repair CI breakage from skill URI validation — fix `ParseSkillURI` grammar to handle edge cases and update stale test fixtures (#874).
* **[Discord]:** Fail-closed observe mode filter using `resolveChannelLink` — the previous implementation looked up thread IDs directly against the store, but channel links are only persisted against parent channels, causing the filter to block all thread messages. Now uses the existing thread-to-parent fallback with inactive link handling (#872).
* **[CLI]:** Update `scion message --attach` help text with accurate path roots and failure mode (#871).
