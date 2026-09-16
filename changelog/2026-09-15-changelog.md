# Release Notes (2026-09-15)

Largest single-day merge — 36 PRs spanning Cloud Run sandbox observability, chat conversation export, release management automation, opencode harness resume, and a second code-quality cleanup batch. Session secret exposure fixed.

## 🔒 Security
* **Session secret removed from /proc/pid/cmdline (#1602):** `--session-secret` flag in systemd `ExecStart` was visible to any local user via `ps(1)`. Removed — `resolveSessionSecret()` already reads from the environment via `EnvironmentFile`.
* **Helm credential guard fixes (#1624):** `assertNoCredentialTree` missed map keys (not just values), and the URL password pattern's character class didn't allow slashes, enabling bypass.

## 🚀 Features
* **Cloud Run sandbox log observability (#1616, #1610, #1611):** Host-side entrypoint log tailer streams logs in real time with per-agent goroutine cleanup. When the sandbox is dead, logs are served from the host with per-case source labels. Logs are now preserved across restarts via rotation instead of truncation.
* **Chat conversation export (#1636):** Download as Markdown, print/save as PDF, and copy to clipboard (HTML + plain text). Export dropdown in chat header, all content HTML-escaped.
* **Release management scripts (#1631):** `scripts/release/` with `cut-preview.sh`, `bump-preview.sh`, and `promote-stable.sh` — dry-run mode, interactive confirmation, and bash 3.2 compatibility.
* **Profile template management UI (#1634):** Create, clone, rename, and detail view for user-scoped templates. Actions dropdown replacing single delete icon, with displayName/description/harness preserved on rename.
* **Opencode hook bridge plugin (#1620):** Hook bridge plugin and dialect enabling opencode harness to communicate with the Scion hub via the standard hook bridge pattern.
* **Notification retry sweep (#1622):** Periodic sweep for undispatched notifications with broker-connect hook for retry on reconnect.
* **`scion message --body-file` (#1625):** Read message body from a file instead of inline, useful for long messages and scripted workflows.
* **Agent messaging mode management (#1633):** Full-role agents can now call `set_message_mode` (D7 amendment). Adds `--message-mode` CLI flag and `scion set-message-mode` command.
* **Individual image build targeting (#1619):** Build specific images by step ID instead of full groups, with `--continue-on-error` to avoid aborting on single image failure.
* **Unrecognized settings key warnings (#1614):** Logs warnings for unrecognized keys during settings load (typos, outdated keys) without failing. Includes `TextUnmarshallerHookFunc` fix.
* **CI custom linter framework (#1606):** `hack/LINT-CONVENTIONS.md`, `check-custom` Makefile meta target, and CI workflow integration for project-specific linting beyond golangci-lint.
* **Clone GCP SA associations during project clone (#1646):** Copies project-scoped SA registrations with `Verified=false` (force re-verification), following the existing `cloneProjectEnvVars` pattern with rollback support.

## 🐛 Fixes
* **Workspace-mode label backfill (#1653):** Boot-time migration stamps `scion.dev/workspace-mode: per-agent` on git projects created before the 2026-08-31 fix. Without it, `ResolveWorkspaceSharingMode("")` returned `shared-plain` regardless of actual mode.
* **Group hierarchy BFS → recursive CTEs (#1639):** Replaced 4 N+1 BFS query functions in `group_store.go` with single `WITH RECURSIVE` CTE queries, eliminating per-level round trips. Works on both PostgreSQL and SQLite.
* **Opencode resume with synthetic prompt (#1638):** Agents exited within ~15-20s after suspend/resume because `--continue` ran without `--prompt`. Injects a synthetic prompt for harnesses with `task_flag` (opencode, gemini-cli, copilot, antigravity).
* **Chat #general thread deletable (#1641):** Replaced `is_general` guard with transactional count check enforcing at-least-one-thread invariant. Allowed rename, removed lazy re-creation.
* **Gemini CLI small model alias (#1637):** `small` mapped to invalid `gemini-flash-lite` — updated to `gemini-3.5-flash-lite`.
* **Hub-members group gated to member role (#1579):** Viewer-role users were incorrectly added to hub-members group. Gates all four login paths and adds removal path for viewer demotion with startup reconciliation.
* **Conversation-routed attachment delivery (#1598):** Attachments were dropped for `conv:`/email/thread recipients — added `msgAttach` to `sendMessageViaConversation`.
* **Starter-hub buildability and script fixes (#1626, #1617):** Policy enforcement fixes, Go version update to 1.26.1, removed unwanted git push step and incorrect default_runtime logic.
* **Code-quality cleanup batch 2 (#1630):** ~30-commit continuation centralizing Hub, runtime, CLI, and persistence paths. Includes fix for broker message delivery metadata preservation.
* **Harness embed wildcard (#1609), grok-build GROK_HOME (#1605), hub endpoint env var rename (#1604), gh CLI pinned v2.97.0 (#1618), secret-resolution logging (#1644), dead RefreshAgentToken removal (#1642), scratchpad path error reporting (#1599).**

## 📖 Docs
* **Single-node VM guide and Developer Hub rebrand (#1652):** Deployment guide for single-node-vm tier, choosing-a-mode comparison doc, starter-hub rebranded to Developer Hub.
* **Workspace sharing boundaries (#1647), OIDC redirect URI (#1613), Cloud Run tier refresh (#1608), messaging skill conv: emphasis (#1627), nightly doc update (#1629).**
