# Release Notes (2026-09-22)

Introduced binary auto-update infrastructure and headless deployment for single-node VMs, alongside a security-critical permission split preventing cross-member secret exposure. Responsive header redesign, profile timezone support, and a comprehensive deploy friction-log sweep rounded out a broad day of platform hardening.

## ⚠️ BREAKING CHANGES
* **Agent permission model** (#1838): Splits `agent.lifecycle` (start/stop/suspend/restart/restore) out of `agent.attach` to prevent project owners/admins from attaching to other members' agents and accessing their user-scoped secrets. Owners/admins retain lifecycle and messaging but lose `attach` and `port_access` on members' agents. Project-owner/admin roles move to revision 3.

## 🚀 Features
* **Binary auto-update for VM deployments** (#1824): Adds deployment_tier, release_channel, update_policy, and check_interval_hours to maintenance config. Hub check-updates API dispatches by tier: binary-tier uses LATEST.json manifest and GitHub Releases API, source-tier continues using git.
* **Headless deploy via --config flag** (#1823): Adds `--config` flag to deploy.sh so all wizard prompts can be pre-answered via YAML, enabling agent-driven non-interactive installs.
* **Profile timezone with TZ injection** (#1822): First-class timezone setting on profiles with TZ injection into agent containers. Validates IANA names; precedence: profile timezone > profile env TZ > hub default_timezone > UTC.
* **Responsive header redesign** (#1820): Three-tier layout: full labeled above 1100 px, icon-only segments 1100-768 px, dropdown with hamburger below 768 px. Fixes crowding and destructive hiding of inbox/notifications.
* **Terminal URL layout encoding** (#1816): URL-based terminal layout persistence enabling shareable/bookmarkable multi-pane configurations.

## 🔒 Security
* **Cross-member secret exposure** (#1838): Project owners/admins could attach, exec, read env, and reach ports on any member's agent, exposing that member's user-scoped secrets. Fixed by splitting `agent.lifecycle` from `agent.attach`; roles revised accordingly.

## 🐛 Fixes
* **Deploy friction-log sweep** (#1836, #1827): Comprehensive 12-item fix from live agent runbook testing: cloud-init dash/bash, Phase 3b root permissions and idempotency, SSH fd leak and readiness window, PyYAML PEP 668, quota command, API list, /healthz GFE, Ubuntu pin, shellcheck, cross-org IAP, plus build-on-VM wording clarification and auto-detected release channel.
* **Cloud Run Instances runtime** (#1817): Resolves RESOURCE_PROJECT_INVALID via GCE metadata auto-discovery, clamps instance IDs to 49 chars, wraps harness in tmux for PTY, resolves localhost hub endpoint, adds USING clause to jsonb migration.
* **Container image base chain reconciliation** (#1831, #1839): Moves core-base to node:24-trixie-slim (Debian 13) with vendored Chainguard git 2.55.0 and reconciles both image chains against one asserted contract, eliminating 4 known drifts.
* **Group conversation creation** (#1846): Routes CLI group conversation create through the topic-creation path so web chat shows the group. Adds shared thread-name rule and 409/400 error handling.
* **Skill file upload/download for local storage** (#1844): Adds missing `files` route for local-storage skill upload/download with per-version URLs.
* **Project skill publish and broker audit** (#1843): Sets scopeId on project-scoped skill publish (400 on missing) and adds audit records for broker authz denies.
* **Project capabilities on empty list** (#1835): Reports project scope capabilities even when the user has no projects, restoring the Create Project button.
* **Web chat improvements** (#1826, #1829, #1821): Directory paths no longer auto-linked; file paths inside backticks now link correctly; delivery status moved below attachment preview; members sidebar toolbar made sticky.
* **Header and layout fixes** (#1834, #1833): Stops header actions overlapping mode switch on narrow viewports; removes nested scrollbar from project detail agents section.
* **Update-available route registration** (#1840): Registers the new update-available route in classification test and authorization catalog.
* **Telemetry docs and card overflow** (#1837): Adds missing gcp_project_id to telemetry config examples; stops long agent names overflowing cards.

## 🔧 CI & Infrastructure
* **PR-based manifest update** (#1830): Converts update-manifest job from direct push (blocked by branch protection) to PR creation with auto-merge.

## 📖 Docs
* **Agent deployment runbook** (#1825): Structured agent-oriented deployment runbook for single-node VM tier covering GCP preflight, conversational prompts, config generation, and troubleshooting.
* **Nightly doc update** (#1818): Nightly update for 2026-09-21 covering persistent terminal workspace, global skill creation, mode switcher, and feature flags reference.
