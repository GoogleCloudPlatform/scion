---
title: Release Notes
---

## Mar 17, 2026

This release introduces a major new GCP Identity implementation allowing agents to authenticate via metadata server emulation, alongside comprehensive new Grove Settings and Agent Limits configurations in the UI.

### 🚀 Features
* **GCP Identity & Metadata Emulation:** Implemented end-to-end GCP identity assignment for agents using metadata server emulation and token brokering. This includes a new Web UI for Service Account management, iptables interception, per-agent rate limiting, audit logging, and telemetry metrics (consolidated from commits 2ac33bb, 961653a, d37a79c, d11318f, a5f457a, d187838, 8df2a04, 34c7056, 401a178, 52f6838).
* **Grove Settings & Agent Limits:** Introduced a comprehensive Grove Settings UI organized into General, Limits, and Resources tabs. Administrators can now configure default agent limits at both the hub and grove levels, which automatically pre-populate when creating new agents (consolidated from commits c7d9585, aa5c2ff, 2ffdff8, 8f0263f, 0d87a17, 07714a1, 906a88d).
* **Workspace Content Previews:** Added content preview capabilities for workspace files directly within the UI (commit 53cea7c).
* **CLI Enhancements:** Added a `-r`/`--running` flag to the `scion list` command to easily filter for active agents (commit 7001035).

### 🐛 Fixes
* **Grove & Membership Synchronization:** Resolved multiple issues with grove linking and membership backfills, including fixing unique constraints on grove IDs, ensuring proper legacy owner role assignments, and correctly including auto-provide brokers (consolidated from commits 4af2662, 307fb85, cb22a18, 79cc591, 1f6f16f, e14ec95).
* **Storage & ID Consistency:** Fixed global grove ID bleed-through issues and unified agent split storage paths under `.scion/` for deterministic behavior across hub-managed and external groves. Ensured cascading cleanups of templates and configs when a grove is deleted (consolidated from commits fea4588, 6bb2348, a97ebd7, 023a089, 6eaf8dc, 221c736, 75bfcc0, c9d8ddf).
* **GCP Validation & Logging:** Improved debug logging for 4xx errors and enhanced GCP Service Account validation messages, including returning capabilities in the list API response (consolidated from commits e060664, d65dc09).
* **Container Lifecycle Management:** Ensured agent containers are gracefully stopped before removal to prevent shared-directory mount errors (commit 8a0fabc).
* **Template Synchronization:** Fixed an issue where template synchronization was blocked by setting a default image for the generic harness config (commit 816c960).
* **Web UI Consistency:** Fixed layout issues such as status column widths in agent tables and exposed Scion version information on the admin config page (consolidated from commits 53f55b5, 7536c59).

## Mar 16, 2026

This release focuses on a major overhaul of user group and membership management with new authorization rules and UI enhancements, alongside significant improvements to the OpenTelemetry metrics pipeline.

### 🚀 Features
* **Group & Membership Management:** Overhauled the group management system by introducing human-friendly member editing, user search autocomplete in the add dialog, and strict enforcement of group ownership and authorization rules (consolidated from commits 1ae6d03, 454c80e, 5e32c9e, c2fa624).
* **Telemetry & Metrics Pipeline:** Enhanced the observability pipeline by exporting OTLP metrics through GCP, restoring Gemini token metric hooks, and covering Gemini native OTEL metrics (consolidated from commits 721da2b, 5e752f8, 28a9877, 4321775).

### 🐛 Fixes
* **Group Constraint Fixes:** Resolved multiple backend issues related to group creation and loading, including fixing dev-user UUID mapping for workstation mode, backfilling grove member groups, and ensuring SQLite constraint compatibility (consolidated from commits 1993892, 4628e5f, e5c1eba, 6a4f843, 1a779c8, cb7c932).
* **Agent Lifecycle:** Implemented proper agent resume and restart dispatch logic on the hub (commit 30a1b74).
* **Grove Synchronization:** Fixed an issue with re-linking stale hub groves by ensuring the grove ID is regenerated from the marker file, and updated the UI to conditionally show the branch field only for git-based groves (consolidated from commits 39e0025, 2bab781).
* **Container Prune Operations:** Fixed the container runtime image pruning by removing the unsupported `-f` flag (commit ad9f486).
* **Environment & Security:** Improved GCE certificate checks (commit 8904e76).

## Mar 15, 2026

This release significantly hardens the agent workspace provisioning process for Hub-linked environments, introduces interactive terminal toolbar toggles for the web UI, and improves overall deterministic Grove ID generation and management.

### 🚀 Features
* **Hub-Linked Workspace Provisioning:** Transitioned hub-linked groves to strictly use a robust `git init` + `git fetch` strategy instead of standard cloning or local worktrees. This allows provisioning into workspaces that already contain `.scion` metadata or `.scion-volumes` directories while properly clearing out stale artifacts before initialization (consolidated from commits 3852e6a, 51be1e6, 118b518, 2f6b877, 2f59410, 4497a86, 6cc4487).
* **Terminal Toolbar Enhancements:** Added web toolbar toggles for managing tmux windows (seamlessly switching between agent and shell) and controlling mouse/clipboard behavior. Also fixed mouse-drag text selection and improved the robustness of the window controls using direct tmux key bindings (consolidated from commits 8c07a48, b83ba8c, 8027407, 6140111).
* **Deterministic Grove ID & Synchronization:** Enhanced deterministic Grove ID generation during hub-link sync. Git URL user info is now cleanly normalized to ensure UUID v5 grove IDs match regardless of the protocol (e.g., `https://` vs `git@`), and stale grove links are automatically detected and synchronized (consolidated from commits 6a52952, 1bfa95d, ed2b2c5).
* **Template Sync Improvements:** Enabled the template sync command to update existing templates without requiring the `--force` flag. Local templates now automatically sync on hub startup and intelligently bypass cache when running in co-located (hub-broker combo) mode (consolidated from commits ff14bd9, e0cf52d).
* **Web UI Flow & Performance:** Introduced route-based code splitting to significantly reduce the web bundle size. Additionally, refined the grove settings page by adding a "Done" button, hiding unnecessary registration options for git-backed groves, and introducing a confirmation dialog when creating a grove for an existing git repository (consolidated from commits 62e3e36, bd9f40e, b6d2afe, f715bf0).

### 🐛 Fixes
* **Grove Deletion Cleanup:** Fixed an issue where environment variables, secrets, and harness configs were left orphaned; the system now performs a proper cascade delete when a grove is removed (commit 834bae9).
* **Agent Path & Directory Routing:** Corrected the routing of agent directories for git-backed groves to correctly use the grove-specific path instead of the global directory. Also properly resolved shared directory mount paths for git-based workspaces (consolidated from commits e38cf92, fbb9056).
* **Hub Unlinking & Local State:** Fixed a bug where the hub status erroneously showed "linked" after unlinking by verifying the local enabled state instead of mutating the global `grove_id`. The provider's `localPath` is also now properly preserved when re-registering an existing grove (consolidated from commits 0857553, 54fe4b8, 4db9253, d4828f1).
* **Agent Container Lifecycle:** Ensured that the agent container stops correctly and cleanly when the underlying agent process exits within the tmux session (commit 535ebbd).
* **Configuration & Path Resolution:** Fixed resolution logic for split-storage paths when writing grove settings, and ensured `git check-ignore` runs from the repository root so that broker `.gitignore` checks function correctly (consolidated from commits 7a4cd3c, f934b9a).
* **Web Assets & UI Styling:** Restored dark mode logic for Shoelace form components after implementing code splitting, and fixed the serving of root-level public assets (like the notification icon) from the Go backend (consolidated from commits ca1343f, b3d9484).
* **Network & Harness Config:** Preserved the hub port when applying container bridge endpoint overrides, and returned a synthetic harness config for "generic" agents to unblock template synchronization (consolidated from commits ff1635d, 713c3ab).

## Mar 14, 2026

This release introduces the foundational infrastructure for the Scion plugin system, adds comprehensive support for syncing grove-level templates, and unifies all Grove IDs to a standard UUID format.

:::danger[BREAKING CHANGES]
* **Grove ID Format Unification:** All Grove IDs have been standardized to a unified UUID format. Git-backed groves now use a deterministic UUID v5 (based on the namespace and normalized URL) instead of a 16-character hex hash, while non-git and hub-managed groves continue using UUID v4. Existing git-backed groves may need to be re-linked, and any integrations relying on the old hex format must be updated (commit e896693).

:::

### 🚀 Features
* **Plugin System Infrastructure:** Introduced the core architecture for a new Scion plugin system using `hashicorp/go-plugin`, complete with reference implementations for message broker and agent harness plugins (consolidated from commits 6c543d0, b1a5ae1, 22991ec).
* **Grove Template Sync & Management:** Implemented capabilities for syncing grove-level templates with the Hub. This includes new API endpoints (`POST /api/v1/groves/{groveId}/sync-templates`), CLI commands (`scion templates sync --all`, `scion templates status`), and a dedicated Web UI for managing synced templates. Additionally, machine-specific settings for git-backed groves are now externalized, while templates remain in-repo to support version control (consolidated from commits d0507b1, 3c9cb4b, 0cf62d7, ef4f208, 56df5b4).
* **CLI Navigation Commands:** Added `config dir`, `cd-config`, and `cd-grove` commands to simplify locating and navigating to configuration and workspace directories (commit 596295d).

### 🐛 Fixes
* **Agent Git Cloning:** Resolved an issue where git clones would hang indefinitely when authentication was required but no token was present. Added proper error state reporting upon clone failures, and corrected the `agent-info.json` path to correctly use the `scion` user's home directory (consolidated from commits 93dfdcd, 7ec5eb2).
* **Image Builds:** Fixed the Google Cloud SDK installation in the build environment by explicitly using `apt-get` (commit d76197c).

## Mar 13, 2026

This release focuses on improving agent specialization with harness skills, resolving critical routing and identification issues in multi-hub and linked git environments, and adding a new satellite service for documentation agents.

:::danger[BREAKING CHANGES]
* **Linked Git Grove IDs:** Linked groves backed by a git remote now use deterministic 16-character hex hash IDs (e.g., generated via `HashGroveID()`) instead of the raw, normalized git URL. This resolves severe web routing and API path parsing issues caused by slashes in the URL. If you had existing linked groves, they may need to be re-linked, and any scripts relying on the raw git URL as the Grove ID will need to be updated (commit 05e0c7a).

:::

### 🚀 Features
* **Harness Skills for Templates:** Implemented robust support for harness skills within agent templates. Skills defined in `harness-configs` and templates are now automatically merged and mounted into the appropriate harness-specific directory (e.g., `.claude/skills`, `.gemini/skills`) during agent provisioning (consolidated from commits efefc44, 2a086ac, 5b54c66).
* **Docs-Agent Satellite Service:** Introduced a new `docs-agent` satellite service to provide dedicated documentation capabilities alongside agent workflows (consolidated from commits 092ffde, 58f21c2, fd1b1e2).
* **Shared Directory Management UI:** Added web UI support for managing and viewing grove shared directories (commit 7d7acfb).
* **Terminal & UX Enhancements:** Enabled tmux mouse mode by default for better terminal interactivity and introduced a custom Scion bell icon for browser notifications (commits c915da9, 343382e).

### 🐛 Fixes
* **Multi-Hub Routing & Dispatch:**
    * Resolved an issue where brokers connected to multiple hubs would route agents to the wrong local hub endpoint by correctly resolving the endpoint from the control channel connection header.
    * Enabled control-channel-only brokers to successfully dispatch agent operations (consolidated from commits dd5581f, 1bdc31d).
* **Agent Creation Context:**
    * Ensured grove shared directories are properly passed from the hub to the broker during agent creation.
    * Fixed an issue where `agentDir` was omitted during harness provisioning and setting overlays (consolidated from commits a5cac3b, c550865).
* **Documentation & Web Hosting:** Corrected site base URLs, configured Astro for GitHub Pages deployment, fixed markdown links to use relative paths, and updated the README to point to the rendered site (consolidated from commits 35eee03, 8ca4a96, a7dc580, e133647, 2467d89).
* **Maintenance:** Internal refactoring analysis for `server.go` and documentation updates for recent feature releases (commits d3484d4, 33ee10e).

## Mar 12, 2026

This release focuses on enhancing persistent storage and system observability. It introduces **Grove Shared Directories**, enabling agents within a grove to share and persist mutable state via the filesystem (with native Kubernetes support). Additionally, the metrics pipeline has been significantly enriched with labels for harness type, model, and grove ID, providing deeper insights into agent performance and costs.

### 🚀 Features
* **Grove Shared Directories (Phase 1 & 2):** Introduced a persistent, mutable storage layer shared between agents within a single grove.
    * Added support for both local filesystem storage and Kubernetes PersistentVolumeClaims (PVCs) with grove-scoped lifecycle management.
    * New CLI commands added: `scion shared-dir list`, `create`, `remove`, and `info` for managing shared volumes.
    * Shared volumes can be mounted at standard paths (`/scion-volumes/<name>`) or within the workspace (`/workspace/.scion-volumes/<name>`) (consolidated from commits 838b1b9, a8d50f8, 8b860c0).
* **Enhanced Telemetry & Metrics Pipeline:** Major overhaul of the metrics pipeline for improved observability and aggregation.
    * Enriched OTel resource attributes with `scion.harness`, `scion.model`, `scion.broker`, and `grove_id`.
    * Expanded Codex-specific telemetry to capture tool usage, tool input/output, and detailed token counts (input, output, cached).
    * Injected `SCION_HARNESS` and `SCION_MODEL` environment variables into agent containers to enable harness-aware telemetry (consolidated from commit 8246a76).

### 🐛 Fixes
* **Metrics & Telemetry Reliability:**
    * Resolved an issue where tool and API metrics were not recorded from unpaired end events.
    * Corrected the wiring of token and model metrics in the hook-to-OTel pipeline (consolidated from commits 2a64f02, 43f1bf0).
* **Agent Lifecycle & Configuration:**
    * Corrected an issue where custom branch names were not properly passed during the final environment setup path of agent creation (commit 46eee6d).
    * Updated the default model configuration for the Codex harness to `gpt-5.4` (commit fbfc950).
* **Maintenance:** Fixed broken documentation links in the repository README (commit 0f55876).

## Mar 11, 2026

This release focuses on improving agent lifecycle flexibility and enhancing the web-based terminal experience. It introduces support for targeting specific git branches during agent creation and provides better visibility into template versions, alongside critical fixes for runtime stability and authentication.

### 🚀 Features
* **Custom Branch Targeting:** Added a branch name field to the agent creation flow and enabled cloning of agent branches from origin. This allows users to direct agents to specific branches immediately upon creation, improving workflow flexibility (consolidated from commits 182c323, 2d50def, 11c36a8).
* **Web Terminal & Tmux Interactivity:** Introduced a tmux mouse toggle (via `C-b m`) and a toolbar button in the web terminal. This release also resolves persistent copy-paste issues in the web interface and adds comprehensive documentation for terminal options (consolidated from commits 9a41138, 9371859, 616250a).
* **Enhanced Template Traceability:** Updated the CLI and Web UI to display template IDs and hashes, providing clear visibility into the exact configuration version associated with each agent.

### 🐛 Fixes
* **Runtime & Broker Stability:**
    * **Podman Reliability:** Resolved an issue where Podman containers would fail to restart correctly from the Hub or Broker.
    * **Double-Daemonization:** Prevented the broker from double-daemonizing during start or restart operations.
* **Agent Attachment Reliability:** Added a readiness check for tmux sessions before attachment, ensuring more reliable connections when attaching to running agents.
* **Authentication & Secret Injection:** Corrected a bug where environment-type secrets were not properly injected into the execution environment during authentication resolution.
* **Grove & Workspace Management:**
    * **Multi-Hub Compatibility:** Fixed a regression where git-based groves were incorrectly rejected in multi-hub environments.
    * **Cleanup & Resolution:** Improved hub-managed grove path resolution during agent deletion and enhanced detection of orphaned grove configurations.
* **Configuration & Compatibility:**
    * **Legacy Key Support:** Updated `config get` to support legacy v1 settings keys like `image_registry`.
    * **Fallback Logic:** Improved `env-gather` and harness configuration to correctly fall back to global settings when local context is missing.
* **Documentation & Polish:** Performed final pre-launch polish on philosophical documentation and refined the agent creation UX by defaulting runtime profiles to "Use broker default."

## Mar 10, 2026

This release focuses on streamlining system administration and enhancing visibility into agent operations. It introduces a comprehensive Web-based server configuration editor and a native runtime profile selector for agent creation, alongside critical improvements to telemetry reliability and Hub connectivity.

### 🚀 Features
* **Web Admin Server Configuration Editor:** Launched a full-featured settings editor at `/admin/server-config` (admin-only). This allows administrators to view and modify the global `settings.yaml` through the Web UI with support for tabbed navigation, sensitive field masking, and hot-reloading of key settings like log levels, telemetry defaults, and admin emails.
* **Runtime Profile Selector:** Added a dynamic profile selector to the agent creation form. After selecting a broker, users can now choose from the available runtime profiles defined on that broker, simplifying execution environment selection.
* **Standardized Issue & Feedback Templates:** Introduced official bug report and feature request templates to the repository to improve the quality and consistency of community contributions.

### 🐛 Fixes
* **Telemetry Configuration Reliability:** Corrected an issue where the telemetry opt-in checkbox on the agent configuration page wouldn't correctly reflect the global settings defaults.
* **Hub Connectivity Precision:** Enhanced agent startup logic to prioritize Hub-dispatched endpoints over local broker configuration, ensuring correct Hub communication in distributed and multi-hub environments.
* **Logging Observability & Traceability:**
    * **Agent Lifecycle Traceability:** Added `agent_id` to all broker-side agent lifecycle log events to improve cross-traceability and audit capabilities.
    * **Connectivity Debugging:** Stopped redacting `SCION_HUB_ENDPOINT` and `SCION_HUB_URL` in agent environment logs to facilitate easier debugging of connectivity issues.
* **Documentation & Licensing:** Restructured internal documentation for improved clarity, updated the installation guide, and completed the application of standard license headers across all source files.

## Mar 9, 2026

This release marks a significant milestone with the official transition of the project to the Google Cloud Platform organization, including a full module rename. It also introduces critical enhancements for agent autonomy with the enablement of the Scion CLI inside agent containers, alongside major improvements to administrative observability and real-time event reliability.

:::danger[BREAKING CHANGES]
* **Project Rebranding & Module Rename:** The Go module has been renamed from `github.com/ptone/scion-agent` to `github.com/GoogleCloudPlatform/scion`. All internal package imports and external references have been updated to reflect the transition to the Google Cloud Platform organization.

:::

### 🚀 Features
* **Autonomous In-Container CLI:** Enabled the Scion CLI within agent containers, providing agents with the ability to interact with the Hub API natively using their provisioned authenticated service context.
* **Admin User Activity Tracking:** Introduced "Last Seen" timestamps and sortable columns to the Admin Users dashboard to improve system administration and audit capabilities.
* **Enhanced Event Integrity:** Refined the Server-Sent Event (SSE) pipeline to ensure full agent snapshots are sent in `created` events, preventing incomplete UI states during high-concurrency creation.

### 🐛 Fixes
* **Log Query Precision:** Optimized agent log retrieval by filtering out internal HTTP request logs from the primary agent cloud logging view.
* **Infrastructure & Connectivity:**
    * Prioritized public Hub endpoints for production dispatches, reducing reliance on local network bridges.
    * Implemented defensive fallbacks for Hub environment variables within agent containers.
    * Resolved IAM role assignment issues for Hub service accounts.
* **UI/UX Consistency:**
    * Enforced name slugification across all CLI and Web input boundaries to prevent routing collisions.
    * Eliminated "white-flash" artifacts during OAuth redirects for users in dark mode.
    * Implemented automatic scrolling to error banners on form submissions.
    * Switched to SPA-native navigation for terminal back-links, improving navigation responsiveness.
* **System Stability:**
    * Resolved directory creation and path resolution bugs in split-storage (git-grove) configurations.
    * Fixed `lstat` errors for non-existent grove configuration files in containerized environments.
    * Corrected image registry resolution logic to prevent redundant prompts when already configured.
    * Resolved test failures across four critical categories on the main branch.
* **Harness Improvements:** Refined the Codex harness with improved configuration formatting and support for sandbox/bypass-approval flags.

## Mar 8, 2026

This release delivers a complete maturation of the Kubernetes runtime, introduces significant architectural enhancements for agent isolation and security, and drastically improves Web UI performance with optimistic updates and connection pooling.

:::danger[BREAKING CHANGES]
* **Kubernetes Mutagen Sync Removal:** Mutagen synchronization support has been entirely removed from the Kubernetes runtime in favor of native implementations as part of the Stage 1 Parity rollout.

:::

### 🚀 Features
* **Kubernetes Runtime Maturation (Stages 1-3):** Successfully implemented Parity, Production Hardening, and Launch Readiness for the Kubernetes runtime, establishing it as a fully-supported, robust platform for agent execution.
* **Agent Isolation & Grove Security:** Enhanced agent security by externalizing non-git grove data and agent home directories. Introduced tmpfs shadow mounts to definitively prevent agents from cross-accessing `.scion` configuration data or other agents' workspaces within the same grove.
* **Web UI Performance & Responsiveness:** Drastically improved the frontend experience by implementing optimistic UI updates and background data refreshes. Re-architected the application shell to reuse components on navigation and consolidated Server-Sent Event (SSE) connections to prevent browser connection pool exhaustion.
* **Contextual Agent Instructions:** Added support for conditional instruction extensions (`agents-git.md` and `agents-hub.md`), allowing agents to receive tailored operational context based on their specific workspace type.
* **Hub API & Infrastructure:** Completed Phase 5 of the Hub API consolidation with full mode awareness and isolation. Enabled HTTP/2 cleartext (h2c) support on the web server, and introduced new grove management CLI commands (`list`, `prune`, `reconnect`).
* **Agent Configuration & Execution:** Enabled `max_duration` limits universally across all harnesses, added a `--notify` flag to the CLI message command, and introduced a required `image_registry` prompt during workstation initialization.
* **Codex Harness Enhancements:** Stabilized the Codex integration with telemetry reconciliation, proper `auth.json` generation for API key workflows, and unified flag formatting.
* **UI Quality of Life:** Added a card/list view toggle to the grove detail agent list and introduced a power-user shortcut (Alt/Option-click) to bypass delete confirmation dialogs globally.

### 🐛 Fixes
* **Hub/Broker Synchronization:** Resolved critical sync issues by tracking synced agents to correctly detect hub-side deletions, preventing deleted agents from being incorrectly re-proposed for registration.
* **Agent Lifecycle Cleanup:** Fixed cleanup routines to correctly stop agent containers before removing orphaned configs, and ensured broker-side files are meticulously cleaned if a hub dispatch fails.
* **Configuration & Auth Propagation:** Corrected the application order of `--harness-auth` before provisioning to prevent stale environment warnings, and ensured template telemetry configs are properly merged into the applied agent config.
* **Messaging Integrity:** Fixed a bug in `handleAgentMessage` to ensure structured messages are correctly constructed from plain text, and updated the messages tab query to include agent-sent communications.
* **Health & Security:** Exempted health check endpoints from broker auth middleware during strict mode enforcement to prevent false-positive failures in distributed deployments.

## Mar 7, 2026

This release marks a major leap in agent observability with the launch of the Cloud Log Viewer and structured messaging pipeline. It also introduces significant UI overhauls for agent management, enhanced GCP integration, and a new workstation-class daemon mode for the Scion server.

### 🚀 Features
* **Cloud Log Viewer & Structured Messaging (Phases 1-5):** Completed the end-to-end implementation of the Cloud Log Viewer and structured message pipeline. This includes a new Hub API for log retrieval, a dedicated "Messages" tab in the Web UI, and a multi-stage message broker adapter for reliable delivery and external notifications.
* **Agent Detail UI Overhaul:** Re-architected the agent detail page into a high-density tabbed layout featuring dedicated "Status", "Configuration", and "Messages" tabs. Added a new telemetry configuration card, breadcrumb navigation improvements, and a back button for the configuration flow.
* **Workstation & Daemon Management:** Introduced a workstation-optimized "daemon" mode for `scion server`. This allows the server to run as a persistent background process with integrated lifecycle management, simplified configuration, and automated combined-server detection for local brokers.
* **GCP & Metrics Integration:** Enhanced Google Cloud visibility with a native Cloud Monitoring exporter, trace-log correlation across logging pipelines, and automated injection of `SCION_GROVE_ID` and GCP labels (agent/grove) into all log streams.
* **Image Management & Build Automation:** Consolidated image build scripts and introduced support for custom `image_registry` settings. Added GitHub Actions workflows for automated building and delivery of Scion harness images.
* **Security & Authorization Hardening:** Strengthened the security posture by enforcing per-agent authorization for workspace routes, mandatory read authorization for all resource endpoints, and nonce-based HMAC validation for broker communication.
* **First-Run Experience:** Added a new `scion install` command and a streamlined first-run experience to simplify initial project setup and dependency verification.
* **Bulk Operations:** Added a "Stop All" button to the Web UI for efficient bulk shutdown of all agents within a grove.
* **Harness Capability Gating:** Introduced capability-based gating for advanced agent configuration, ensuring only supported features are exposed based on the selected harness.

### 🐛 Fixes
* **UI Performance & Reliability:** Optimized the agent detail page by parallelizing API fetches and eliminating redundant data loads. Resolved rendering issues in the messages tab and added handling for null entries in message logs.
* **Auth & Environment Injection:** Fixed multiple issues with environment variable and profile injection, specifically resolving signing errors in combined-server mode and ensuring profile variables are applied before auth overlays.
* **Runtime & Broker Stability:** Improved Podman error handling and force-deletion reliability. Fixed a bug where `agent-limits.json` lacked correct permissions after creation and ensured `InlineConfig` is correctly propagated during agent restarts.
* **Logging Precision:** Established a dedicated HTTP request log stream using the standard `HttpRequest` format and removed misleading debug logs when running in GCP-native mode.
* **Build System:** Fixed a race condition in `make all` by ensuring web assets are fully built before the Go binary compilation begins.

## Mar 6, 2026

This release introduces Just-In-Time (JIT) agent configuration, an advanced agent creation interface, and native GCP telemetry integration, while centralizing profile management at the global level.

:::danger[BREAKING CHANGES]
* **Global Profile Management:** Runtime `profiles` and `runtimes` are no longer supported in grove-level `settings.yaml`. These must now be managed exclusively at the global/broker level (`~/.scion/settings.yaml`). Existing grove-specific profiles must be migrated to the global configuration.

:::

### 🚀 Features
* **Just-In-Time (JIT) Agent Configuration:** Completed Phases 1 & 2 of the inline agent configuration refactor. Agents now support dynamic, late-bound configuration overrides at runtime, enabling more flexible and adaptive agent behavior.
* **Advanced Agent Creation Form:** Launched a comprehensive advanced configuration interface in the Web UI. This allows for granular control over agent parameters, including model selection, resource limits, and specific harness settings during creation.
* **GCP-Native Telemetry Integration:** Introduced native support for Google Cloud Trace and Cloud Logging telemetry exporters. The system now automatically detects GCP credentials and configures the appropriate exporter mode, facilitating seamless observability in Google Cloud environments.
* **Enhanced Developer Workflow:** Improved the developer experience with automated mounting of the `sciontool` binary and a dedicated `SCION_DEV_BINARIES` directory, enabling rapid iteration and testing of local changes within agent containers.
* **Branding & UI Refresh:** Updated the application branding with a new seedling logo and favicon, and added detailed visibility of the resolved harness authentication method in the agent detail view.
* **Local Networking Automation:** Automated the computation of the `ContainerHubEndpoint` for Podman and Docker when running in combined hub-broker mode, simplifying local setup and networking.

### 🐛 Fixes
* **Telemetry & Auth Propagation:** Resolved several issues where telemetry settings, harness authentication, and configuration overrides were not consistently propagated through all broker and agent startup paths.
* **Agent Lifecycle Stability:** Fixed a bug where provisioning agents were not correctly cleaned up after an aborted environment-gathering session.
* **Claude Harness Authentication:** Corrected Vertex AI authentication detection for the Claude harness when using file-based secrets.
* **Data Integrity:** Fixed a bug in the advanced agent creation form where the applied configuration was not correctly populated with resolved values.

## Mar 5, 2026

This release introduces a major overhaul of the agent authentication pipeline, automated token refresh, and critical stability fixes for container removal and terminal reliability.

:::danger[BREAKING CHANGES]
* **Credential Key Migration:** The internal secret key `OAUTH_CREDS` has been renamed to `GEMINI_OAUTH_CREDS`. Users must migrate existing secrets to this new key to maintain Gemini harness functionality.
* **Harness Auth Refactor:** Legacy harness-specific authentication methods have been retired in favor of a unified `ResolvedAuth` pipeline. Custom harness implementations or manual environment overrides may require updates to align with the new late-binding logic.

:::

### 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor of the agent authentication pipeline. Agents now support a variety of resolved auth types (API Key, Vertex AI, ADC, OAuth) with late-binding overrides available via the CLI (`--harness-auth`) and the agent creation form.
* **Agent Token Refresh:** Implemented an automated token refresh mechanism to ensure long-running agents maintain valid authorization throughout extended tasks.

### 🐛 Fixes
* **Apple-Container Stability:** Resolved critical hangs during container removal on macOS by implementing automated cleanup and blocking of problematic debug symlinks (e.g., `.claude_debug`).
* **Terminal UX & Reliability:** Improved error visibility by skipping terminal reset sequences on attachment failures.
* **Workspace & Git Integrity:** Hardened workspace file collection by skipping symlinks and ensured `git clone` operations correctly use the `scion` user when the broker runs as root.
* **Auth Precision & Validation:** Fixed several authentication regressions, including incorrect Vertex AI region projections, false API key requirements during environment gathering, and improper leakage of host settings into agent containers.

## Mar 4, 2026

This period focuses on the foundational implementation of the unified harness authentication pipeline and enhances infrastructure visibility within the Web UI.

:::danger[BREAKING CHANGES]
* **Harness Authentication Pipeline:** The implementation of the unified `ResolvedAuth` model (Phases 1-7) replaces legacy harness-specific authentication methods. While finalized in the Mar 5 release, the core architectural shift and retirement of legacy methods occurred in this period.

:::

### 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor (Phases 1-7) of the agent authentication pipeline. Introduced centralized `AuthConfig` gathering, per-harness `ResolveAuth` logic, and a unified `ValidateAuth` phase, enabling more robust credential resolution across all harnesses.
* **Broker Visibility & Infrastructure Metadata:** Enhanced the Web UI to display runtime broker information on agent cards, grove detail pages, and agent detail headers, providing clearer insight into distributed execution.
* **Default Notification Triggers:** Expanded the notification system to include `stalled` and `error` as default trigger states, improving proactive monitoring of agent health.

### 🐛 Fixes
* **Workspace Permissions:** Hardened the workspace provisioning flow by ensuring `git clone` operations run as the `scion` user when the broker is executing as root.
* **UI Navigation & UX:** Fixed back-link routing for agent creation and detail pages to consistently return users to the parent grove. Improved terminal accessibility by disabling the terminal button for offline agents.
* **Config & Environment Propagation:** Resolved issues with `harnessConfig` propagation during the environment-gathering finalization flow and refined Hub endpoint bridging to only target `localhost` endpoints.
* **Server Reliability:** Applied default `StalledThreshold` values for agent health monitoring and improved status badge readability.

## Mar 3, 2026

This release introduces hierarchical subsystem logging, an integrated browser push notification system, and native support for GKE runtimes and OTLP telemetry.

### 🚀 Features
* **Structured Subsystem Logging:** Introduced a hierarchical, subsystem-based structured logging framework across the Hub and Runtime Broker. This enables more granular observability and easier troubleshooting by isolating logs for specific components like the scheduler, dispatcher, and runtimes.
* **Agent Notifications & Browser Push:** Launched an integrated notification system with real-time SSE delivery and agent-scoped filtering. Features include a new notification tray in the Web UI, opt-in checkboxes for agent creation, and native browser push notification support.
* **Telemetry & OTLP Pipeline:** Added native support for OTLP log receiving and forwarding. The system now supports automated telemetry export with GCP credential injection, manageable via new CLI flags (`--enable-telemetry`) and UI toggles.
* **Stalled Agent Detection:** Implemented a new monitoring system to detect agents that have stopped responding (heartbeat timeout). Stalled agents are now flagged in the UI and can trigger automated notification events.
* **GKE Runtime Support:** Added native support for Google Kubernetes Engine (GKE) runtimes, including cluster provisioning scripts and Workload Identity integration for secure, distributed agent execution.
* **Layout & View Toggles:** Enhanced the Web UI with card/list view toggles for Groves, Agents, and Brokers pages, improving resource visibility for both small and large deployments.
* **Broker Access Control:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.

### 🐛 Fixes
* **Terminal UX:** Fixed double-paste and selection-copy bugs in the web terminal.
* **UI Responsiveness:** Resolved an issue where the agent list could incorrectly clear during real-time SSE updates and improved status badge readability.
* **Agent Provisioning:** Prevented root-owned directories in agent home by pre-creating secret and gcloud mount-point directories.
* **Administrative Security:** Hardened the Hub by restricting access to global settings and sensitive resource management (env/secrets) to administrative users.
* **Server Stability:** Fixed scheduler startup in combined mode and resolved heartbeats from defeating stalled agent detection.
* **CLI UX:** Standardized CLI scope flags and corrected secret set syntax for hub-scoped resources.

## Mar 2, 2026

This release focuses on refining the agent lifecycle experience with an overhauled status and activity tracking system, enhanced grove-level configuration, and improved CLI flexibility for remote operations.

### 🚀 Features
* **Status & Activity Tracking Overhaul:** Replaced the generic `STATUS` with a more precise `PHASE` column across the CLI and Web UI. Introduced "sticky" activity logic to ensure significant agent actions remain visible during transitions, and enabled real-time status broadcasting via SSE for broker heartbeats.
* **Grove Environment & Secret Management:** Launched a dedicated configuration interface for managing grove-scoped environment variables and secrets. Includes a new "Injection Mode" selector (Always vs. As-Needed) for granular control over agent environment population.
* **Remote Grove Targeting:** Enhanced the `--grove` flag to natively accept grove slugs and git URLs in Hub mode, streamlining operations on remote workspaces without requiring local configuration.
* **Unified Configuration UX:** Consolidated grove-specific configuration into a centralized settings page in the Web UI, utilizing shared components for environment and secret management.

### 🐛 Fixes
* **Container Runtime Compliance:** Fixed an issue where secret volume mounts were incorrectly ordered in container run commands, ensuring reliable mounting across different runtimes.
* **Agent Identity Reliability:** Resolved bugs preventing the consistent propagation of `SCION_AGENT_ID` during restarts and specific dispatch paths, fixing broken notification subscriptions.
* **Linked-Grove Pathing:** Corrected workspace resolution for linked groves without git remotes by ensuring fallback to the provider's local filesystem path.
* **UI State Resolution:** Fixed a bug where hub agents would occasionally show an "unknown" phase by ensuring the UI correctly reads the unified Phase and Activity fields.
* **UX Refinements:** Improved the `scion list` output to use human-friendly template names and fixed dynamic label mapping in secret configuration forms.
* **Stability:** Suppressed spurious errors during graceful server shutdown and resolved potential issues with higher-priority environment variable leakage in tests.

## Mar 1, 2026

This release introduces strict runtime enforcement for agent resource limits and includes several critical stability and performance improvements across the server and build pipeline.

### 🚀 Features
* **Agent Resource Limits Enforcement:** Implemented strict runtime enforcement for agent constraints, including `max_turns`, `max_model_calls`, and `max_duration`. Agents exceeding these limits are now automatically transitioned to a `LIMITS_EXCEEDED` state and terminated.

### 🐛 Fixes
* **Bundle Size Optimization:** Implemented vendor chunk splitting in the Vite build process to resolve bundle size warnings and improve frontend load performance.
* **Server Stability:** Resolved a critical panic that occurred during double-close operations in the combined Hub+Web server shutdown sequence.
* **Secret Mapping:** Corrected the mapping of secret type fields and standardized dynamic key/name labels to ensure consistency with backend providers.

## Feb 28, 2026

This release marks a major milestone with the completion of the canonical agent state refactor and the launch of the Hub scheduler system, alongside significant enhancements to real-time observability and broker security.

:::danger[BREAKING CHANGES]
* **Unified State Model:** The legacy `Status` and `SessionStatus` fields have been fully retired in favor of a canonical, layered agent state model. Downstream consumers of the Hub API or `sciontool` status outputs must update to the new schema.
* **Notification Triggers:** In alignment with the state refactor, notification `TriggerStatuses` have been renamed to `TriggerActivities`.

:::

### 🚀 Features
* **Canonical Agent State Refactor:** Completed a comprehensive, multi-phase overhaul of the agent state system across the Hub, Store, Runtime Broker, CLI, and Web UI. This ensures a consistent, high-fidelity representation of agent activity throughout the entire lifecycle.
* **Hub Scheduler & Timer Infrastructure:** Launched a unified scheduling system for recurring Hub tasks and one-shot timers. This includes automated heartbeat timeout detection for "zombie" agents and a new CLI/API for managing scheduled maintenance and lifecycle events.
* **Real-time Debug Observability:** Introduced a full-height debug panel in the Web UI, providing a real-time stream of SSE events and internal state transitions for advanced troubleshooting and observability.
* **Enhanced Web UI Feedback:** Added emoji-based status badges to agent cards and list views, providing more intuitive visual indicators of agent health and activity.
* **Broker Authorization & Identity:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.
* **Automated Grove Cleanup:** Hardened the hub-managed grove lifecycle by implementing cascaded directory cleanup on remote brokers whenever a grove is deleted via the Hub.
* **CLI Enhancements:** Added a new `-n/--num-lines` flag to the `scion look` command, enabling tailored views of agent terminal output.

### 🐛 Fixes
* **Notification Dispatcher:** Fixed a bug where the notification dispatcher failed to start when the Hub was running in combined mode with the Web server.
* **Environment Variable Standardization:** Renamed `SCION_SERVER_AUTH_DEV_TOKEN` to `SCION_AUTH_TOKEN` and introduced `SCION_BROKER_ID` and `SCION_TEMPLATE` variables for better debugging and interoperability.
* **Local Secret Storage:** Resolved issues with local secret storage and added diagnostics for environment-gathering resolution.

## Feb 27, 2026

This release focuses on refining the hub-managed grove experience, enhancing the web terminal's usability, and introducing new workspace management capabilities via the Hub API.

### 🚀 Features
* **Workspace Management:** Added new Hub API endpoints for downloading individual workspace files and generating ZIP archives of entire groves, facilitating easier data export and backup.
* **Broker Detail View:** Launched a comprehensive broker detail page in the Web UI, providing a grouped view of all active agents by their respective groves for improved operational visibility.
* **Deployment Automation:** Enhanced GCE deployment scripts with new `fast` and `full` modes, streamlining the process of updating Hub and Broker instances in production environments.
* **Iconography Standardization:** Established a centralized icon reference system and updated the web interface to use consistent iconography for resources like groves, templates, and brokers.

### 🐛 Fixes
* **Hub-Managed Path Resolution:** Resolved several critical issues where hub-managed groves incorrectly inherited local filesystem paths from the Hub server. Broker-side initialization of `.scion` directories and explicit path mapping now ensure consistent workspace behavior across distributed brokers.
* **Terminal & Clipboard UX:** Enabled native clipboard copy/paste support in the web terminal and relaxed availability checks to allow terminal access during agent startup and transition states.
* **Real-time Data Integrity:** Fixed a bug in the frontend state manager where SSE delta updates could merge incorrectly; the manager is now reliably seeded with full REST data upon page load.
* **Slug & Case Sensitivity:** Normalized agent slug lookups to lowercase and implemented stricter name validation to prevent routing collisions and inconsistent dispatcher behavior.
* **Environment & Harness Config:** Improved the reliable propagation of harness configurations and environment variables from Hub storage to the runtime broker during both initial agent start and subsequent restarts.
* **UI Refinement:** Replaced text-based labels with intuitive iconography on agent cards to optimize space and improved contrast for neutral status badges.

## Feb 26, 2026

This release introduces a robust capability-based access control system, a dedicated administrative management suite, and critical session management upgrades to support larger authentication payloads.

### 🚀 Features
* **Capability-Based Access Control:** Implemented a comprehensive capability gating system across the Hub API and Web UI. Resource responses now include `_capabilities` annotations, enabling granular UI controls and API-level enforcement for resource operations.
* **Administrative Management Suite:** Launched a new Admin section in the Web UI, providing centralized views for managing users, groups, and brokers. Includes a maintenance mode toggle for Hub and Web servers to facilitate safe infrastructure updates.
* **Advanced Environment & Secret Management:** Introduced a profile-based settings section for managing user-scoped environment variables and secrets. Secrets are now automatically promoted to the configured backend (e.g., GCP Secret Manager) with standardized metadata labels.
* **SSR Data Prefetching:** Improved initial page load performance and eliminated "flash of unauthenticated content" by prefetching critical user and configuration data into the HTML payload via `__SCION_DATA__`.
* **Hub Scheduler Design:** Completed the technical specification for a new Hub scheduler and timer system to manage long-running background tasks and lifecycle events.
* **Enhanced Real-time Monitoring:** Expanded Server-Sent Events (SSE) support to the Brokers list view, ensuring infrastructure status is reflected in real-time without manual refreshes.

### 🐛 Fixes
* **Filesystem Session Store:** Replaced cookie-based session storage with a filesystem-backed store to resolve "400 Bad Request" errors caused by cookie size limits (4096 bytes) during large JWT/OAuth exchanges.
* **Hub-Managed Grove Reliability:** Fixed critical 503 errors and path resolution issues during agent creation in hub-managed groves by correctly propagating grove slugs to runtime brokers.
* **Agent Deletion Cleanup:** Hardened the agent deletion flow to ensure that stopping and removing an agent in the Hub correctly dispatches cleanup commands to the associated runtime broker and removes local workspace files.
* **Environment Validation:** Improved agent startup safety by treating missing required environment variables as fatal errors (422), preventing agents from starting in incomplete states.
* **Terminal Responsiveness:** Resolved several layout bugs in the web terminal, ensuring it correctly resizes with the viewport and fits within the application shell.
* **Group Persistence:** Fixed synchronization issues between the Hub's primary database and the Ent-backed authorization store, ensuring grove-scoped groups and policies are preserved during recreation.

## Feb 25, 2026

This release focuses on hardening the agent provisioning pipeline, streamlining template management through automatic bootstrapping, and enhancing the web authentication experience.

### 🚀 Features
* **Template Bootstrapping:** Local agent templates are now automatically bootstrapped into the Hub database during server startup, ensuring all defined templates are consistently available across the system.
* **Custom ADK Runner Entrypoint:** Introduced a specialized runner entrypoint for Agent Development Kit (ADK) agents with native support for the `--input` flag, facilitating more robust automated execution.
* **Wildcard Subdomain Authorization:** Expanded security configuration to support wildcard subdomain matching in `authorized-domains`, allowing for more flexible deployment architectures.

### 🐛 Fixes
* **Agent Provisioning & Creation:** Resolved multiple issues in the Hub-dispatched agent creation flow, including a 403 authorization fix, rejection of duplicate agent names, and a critical fix for container image resolution.
* **Instruction Injection Logic:** Improved the reliability of agent instructions by implementing auto-detection for `agents.md` and ensuring stale instruction files (e.g., lowercase `claude.md`) are removed during provisioning.
* **Web UI & Auth Persistence:** Fixed a bug where the authenticated user wasn't correctly fetched on page load, ensuring the profile and sign-out options are always visible in the header.
* **Pathing & Scoping:** Corrected path resolution logic to prevent local-path groves from incorrectly using hub-managed paths, and refined the `scion delete --stopped` command to strictly scope to the active grove.
* **Environment Gathering:** Fixed a regression in the `env-gather` finalize-env flow to ensure the template slug is correctly preserved throughout the entire provisioning pipeline.
* **Configuration Schema:** Added `task_flag` support to the settings schema and Hub configuration, improving the tracking and validation of agent task states.

## Feb 24, 2026

This release introduces a robust policy-based authorization system, a comprehensive agent notification framework, and significant enhancements to hub-managed groves and schema validation.

:::danger[BREAKING CHANGES]
* **Policy-Based Authorization:** Strictly enforced authorization for agent operations. Agent creation now requires grove membership, while interaction (PTY, messaging) and deletion are restricted to the agent's owner (creator) or system administrators.

:::

### 🚀 Features
* **Agent Notifications System:** Launched a multi-phase notification framework enabling real-time subscriptions to agent status events. This includes a new notification dispatcher, Hub API endpoints, and a `--notify` flag in the CLI for status tracking.
* **Harness-Agnostic Templates:** Introduced support for role-based, harness-agnostic agent templates. New fields for `agent_instructions`, `system_prompt`, and `default_harness_config` allow templates to be defined by their role rather than specific LLM implementations.
* **GKE Security Enhancements:** Added a dedicated `gke` runtime configuration option to enable GKE-specific features like Workload Identity, streamlining secure deployments on Google Kubernetes Engine.
* **Hub-Managed Workspace Management:** Advanced hub-managed grove capabilities (Phase 3) with new support for direct workspace file management via the Hub API, reducing reliance on external Git repositories.
* **ADK Agent Integration:** Added a specialized example and Docker template for Agent Development Kit (ADK) agents, facilitating the development of custom autonomous agents within the Scion ecosystem.
* **Infrastructure & Models:** Upgraded the default agent model to `gemini-3-flash-preview` and introduced Cloud Build configurations for automated image delivery.

### 🐛 Fixes
* **Schema & Config Synchronization:** Conducted a comprehensive audit and sync between Go configuration structs and JSON schemas. This fixes field naming inconsistencies (e.g., camelCase for `runtimeClassName`) and improves cross-platform validation.
* **Environment Variable Passthrough:** Corrected environment handling to treat empty variable values as implicit host environment passthroughs.
* **Per-Agent Hub Overrides:** Enabled agents to specify custom Hub endpoints directly in their configuration, providing flexibility for agents to report to different Hubs than their parent grove.
* **Soft-Delete Configuration:** Added explicit server-side settings for soft-delete retention periods and workspace file preservation.

## Feb 23, 2026

This period focused on major architectural expansions, introducing multi-hub connectivity for runtime brokers and "hub-managed" groves that decouple workspace management from external Git repositories.

### 🚀 Features
* **Multi-Hub Broker Architecture:** Completed a major refactor of the Runtime Broker to support simultaneous connections to multiple Hubs. This includes a new multi-credential store, per-connection heartbeat management, and a "combo mode" that allows a broker to be co-located with one Hub while serving others remotely.
* **Hub-Managed Groves:** Launched "Hub-Managed" groves, enabling the creation of project workspaces directly through the Hub API and Web UI without an external Git repository. These groves are automatically initialized with a seeded `.scion` structure and managed locally by the Hub.
* **Streamlined Workspace Creation:** Introduced a new grove creation interface in the Web UI that supports both Git-based repositories and Hub-managed workspaces, including direct Git URL support for quick onboarding.
* **Improved Agent Configuration:** Enhanced the agent creation form with optimized dropdowns and more intuitive labeling, including renaming "Harness" to "Type" for better clarity.

### 🐛 Fixes
* **Web UI Asset Reliability:** Resolved several issues with Shoelace icon rendering by correctly synchronizing the icon manifest, fixing asset serving paths in the Go server, and updating CSP headers to allow data-URI system icons.
* **Template Flexibility:** Updated the template push logic to make the harness type optional, facilitating the use of more generic or agnostic agent templates.
* **Codex Harness Refinement:** Improved the Codex integration by isolating harness documentation into a dedicated `.codex/` subdirectory and removing unnecessary system prompt prepending.

## Feb 22, 2026

This period introduced significant data management features, including agent soft-delete and centralized harness configuration storage, while advancing the secrets management and execution limits infrastructure.

### 🚀 Features
* **Agent Soft-Delete & Restore:** Implemented a complete soft-delete lifecycle for agents. This includes Hub-side archiving, a new `scion restore` command, list filtering for deleted agents, and an automated background purge loop for expired records.
* **Secrets-Gather & Interactive Input:** Enhanced the environment gathering pipeline to support "secrets-gather." Templates can now define required secrets, and the CLI provides interactive prompts to collect missing values, which are then securely backed by the configured secret provider.
* **K8s Native Secret Mounting:** Completed Phase 4 of the secrets strategy, enabling native secret mounting for agents running in Kubernetes. This includes support for GKE CSI drivers and robust fallback paths.
* **Harness Config Hub Storage:** Added Hub-resident storage for harness configurations. This enables centralized management (CRUD), CLI synchronization, and ensures configurations are consistently propagated to brokers during agent creation.
* **Agent Execution Limits:** Introduced Phase 1 of the agent limits infrastructure, including support for `max_turns` and `max_duration` constraints and a new `LIMITS_EXCEEDED` agent state.
* **CLI UX Improvements:** Added a `--all` flag to `scion stop` for bulk agent termination, introduced Hub auth verification with version reporting, and enhanced `scion look` with better visual padding and borders.
* **Web UI & Real-time Updates:** Launched a new "Create Agent" UI, optimized frontend performance by moving to explicit component imports, and enabled real-time grove list updates via Server-Sent Events (SSE).

### 🐛 Fixes
* **Provisioning Robustness:** Improved cleanup of provisioning agents during failed or cancelled environment gathering sessions to prevent stale container accumulation.
* **Sync & State Consistency:** Fixed a race condition where Hub synchronization could remove freshly created agents and ensured harness types are correctly propagated during agent sync.
* **Deployment Pipeline:** Corrected the build sequence in GCE deployment scripts to ensure web assets are fully compiled before the Go binary is built.
* **Config Resolution:** Fixed several configuration issues, including profile runtime application, grove flag resolution in subdirectories, and Hub environment variable suppression when the Hub is disabled.

## Feb 21, 2026

This period heavily focused on implementing the end-to-end "env-gather" flow to manage environment variables safely, alongside several CLI improvements and runtime fixes.

### 🚀 Features
* **Env-Gather Flow Pipeline:** Implemented a comprehensive environment variable gathering system across the CLI, Hub, and Broker. This includes harness-aware env key extraction, Hub 202 handling with submission endpoints, and broker-side evaluation to finalize the environment prior to agent creation.
* **Agent Context Threading:** Threaded the CLI hub endpoint directly to agent containers and added support for environment variable overrides.
* **Agent Dashboard Enhancements:** The agent details page now displays the `lastSeen` heartbeat as a relative time format.
* **Template Pathing:** Added support for `SCION_EXTRA_PATH` to optionally include template bin directories in the system `PATH`.
* **Build System Upgrades:** Overhauled the Makefile with new standard targets for build, install, test, lint, and web compilation.

### 🐛 Fixes
* **Env-Gather Safety & UX:** Added strict rejection of env-gather in non-interactive modes to prevent unsanctioned variable forwarding. Improved confirmation messaging and added dispatch support for grove-scoped agent creation.
* **CLI Output Formatting:** Redirected informational CLI output to `stderr` to ensure `stdout` can be piped cleanly as JSON.
* **Podman Performance:** Fixed slow container provisioning on Podman by directly editing `/etc/passwd` instead of using `usermod`.
* **Profile Parameter Routing:** Corrected the threading of the profile parameter from the CLI through the Hub to the runtime broker.
* **Hub API Accuracy:** The Hub API now correctly surfaces the `harness` type in responses for agent listings.
* **Docker Build Context:** Fixed an issue where the `scion-base` Docker image build was missing the web package context.

## Feb 20, 2026

This period focused heavily on unifying the Hub API and Web Server architectures, refactoring the agent status model, and enhancing the web frontend experience with new routing and pages.

:::danger[BREAKING CHANGES]
* **Status Model:** Consolidated the `SessionStatus` field into the primary `Status` field across the codebase (API, Database, UI). The `WAITING_FOR_INPUT` and `COMPLETED` states are now treated as "sticky" statuses.
* **Server Architecture:** Combined the Hub API and Web server to serve on a single port (`8080`) when both are enabled. API traffic is now routed to `/api/v1/`, resolving CORS issues and simplifying local deployment.

:::

### 🚀 Features
* **Web Frontend Enhancements:** Added a new Brokers list page, implemented full client-side routing for the Vite dev server, and unified OAuth provider detection via a new `/auth/providers` endpoint.
* **Agent Environment:** Added support for injecting harness-specific telemetry and hub environment variables directly into agent containers based on grove settings.
* **Git Operations:** Added cloning status indicators and improved git clone config parity during grove-scoped agent creation.

### 🐛 Fixes
* **Real-time UI Updates:** Fixed the Server-Sent Events (SSE) format to ensure real-time UI updates correctly broadcast agent state changes.
* **Routing & Port Prioritization:** Fixed port prioritization to use the web port for broker hub endpoints in combined mode, and ensured unhandled `/api/` routes return proper JSON 404 responses.
* **OAuth & Login:** Fixed conditional rendering for the `/login` route and correctly populated OAuth provider attributes during client-side navigation.
* **Container Configuration:** Fixed container image resolution from on-disk harness configurations and normalized YAML key parsing.
* **Status Reporting:** Ensured Hub status reporting correctly respects and preserves the newly unified, sticky statuses.

## Feb 19, 2026

This period represented a major architectural shift, consolidating the web server into a single Go binary, removing dependencies like NATS and Koa, and introducing hub-first remote workspaces via Git.

:::danger[BREAKING CHANGES]
* **Secrets Management:** The system now strictly requires a configured production secret backend (e.g., `gcpsm`) for any secret Set operations across user, grove, and runtime broker scopes. Plaintext fallbacks have been removed. Read, list, and delete operations remain functional locally to support data migration.
* **Server Architecture:** The Node.js Koa server and NATS message broker dependencies have been completely retired. The Scion Hub now natively handles web frontend serving, SPA routing, and Server-Sent Events (SSE) via a consolidated Go binary.

:::

### 🚀 Features
* **Hub-First Git Workspaces:** Implemented end-to-end support for creating remote workspaces directly from Git URLs. This integration enables git clone mode across `sciontool init` and the runtime broker pipeline.
* **Web Server & Auth Integration:** Introduced native session management and OAuth routing within the Go web server, alongside a new EventPublisher for real-time SSE streaming.
* **Telemetry & Settings:** Added telemetry injection to the `v1` settings schema. Telemetry configuration now supports hierarchical merging and is automatically bridged into the agent container's environment variables.
* **CLI Additions:** Introduced the `scion look` command for non-interactive terminal viewing. Project initialization now automatically sets up template directories and requires a global grove.

### 🐛 Fixes
* **Lifecycle Hooks:** Relocated the cleanup handler to container lifecycle hooks to guarantee reliable execution upon container termination.
* **Settings Overrides:** Fixed configuration parsing to ensure environment variable overrides are correctly applied when loaded from `settings.yaml`.
* **CLI Defaults:** Ensured the `update-default` command consistently targets the global grove, and introduced a new `--force` flag.
* **Frontend Assets:** Resolved static asset serving issues by removing an erroneous `StripPrefix` in the router, and fixed client entry point imports.
