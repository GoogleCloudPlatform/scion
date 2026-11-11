# Release Notes (2026-09-21)

Major persistent terminal workspace feature landed behind a feature flag, alongside critical authorization fixes for template handlers and SSE wildcard subscriptions. Cross-project messaging underwent a comprehensive 4-phase cleanup, and multiple web UI regressions were resolved.

## 🚀 Features
* **Persistent terminal workspace** (#1795): Adds a multi-pane terminal workspace with cross-tab ownership, reconnect controls, account teardown and scoped PTY cleanup. Feature flag `terminal_workspace` defaults OFF.
* **Global skill creation permission** (#1803, #1807): Introduces `skill.create_global` permission and a system-scoped `global-catalog-author` role for non-admin global catalog writes, decoupling global skill authoring from full hub-admin authority.
* **Mode switcher prominence** (#1802): Switches header layout to three-column CSS grid with centered mode switcher, adds text labels (Dashboard, Chat, Terminal) visible above 768 px with clearer active/hover states.

## 🔒 Security
* **Template handler authorization** (#1804): Adds missing `authorize()` gates to six template handlers (`updateTemplateV2`, `patchTemplateV2`, `handleTemplateUpload`, `handleTemplateFinalize`, `handleTemplateDownload`, `handleTemplateValidate`) that previously allowed any authenticated user to modify or access any template including global ones.
* **Project wildcard SSE authorization** (#1809): Expands `project.>` SSE subscriptions into caller-authorized project subjects with deduplication, closing a fail-open gap in wildcard subscription handling.

## 🐛 Fixes
* **Cross-project messaging cleanup** (#1808): Comprehensive 4-phase, 12-task CPM cleanup covering admission-gap authorization (outbound DM, authoritative settings, attachment limits), truthful delivery (shared DM ops, outcomes, audit, wake), converged send API (hubclient contract, CLI cutover, delete endpoint), and acceptance-test verification.
* **RuntimeBroker labels/annotations migration** (#1796): Migrates labels and annotations columns from text to jsonb with a pre-migration hook to NULL empty strings, enabling Postgres `@>` jsonb containment queries for `FindEmbeddedBroker`.
* **Tmux title synchronization** (#1806): Activates per-session tmux title sync on attach for both existing and newly provisioned sessions while preserving OSC 7337 fallback.
* **Terminal multi-pane layout preservation** (#1805): Fixes empty multi-pane placeholders, preserves active layouts during navigation, and places newly opened terminals into available slots.
* **Pane focus outline cleanup** (#1812): Scopes focused-pane outline to effective multi-pane layouts so switching to Single removes the stale outline without changing terminal selection.
* **Shoelace toast removal race** (#1815): Removes app-owned `sl-after-hide` cleanup that raced with Shoelace's toast stack, suppresses expected invite-stats denial toast for members.
* **Chat file path auto-linking** (#1794): Excludes trailing sentence-punctuation periods from auto-linked file paths using a lookbehind assertion.
* **Postgres CTE UUID casting** (#1810): Casts CTE seed parameters to uuid type to prevent `42883` operator-not-found errors in group lookups. *(Contributor: sal-m2026)*
* **Doctor connectivity check** (#1780): Passes stored hub auth tokens and normalizes URLs in the `scion doctor` connectivity check. *(Contributor: G. Hussain Chinoy)*

## 🧪 Tests
* **Hub test suite fixes** (#1799): Resolves 25 previously-failing `pkg/hub` tests across 7 root-cause clusters including fixture entity types, role binding setup, subject kind migration, denial code constants, and SQLite connection pool isolation.

## 📖 Docs
* **Nightly and weekly notes** (#1801): Nightly doc update for 2026-09-20 plus weekly release notes for September 14–20 covering cross-project messaging, native chat overhaul, A2A standalone HA, and three-channel release model.
