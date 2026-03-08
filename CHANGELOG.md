# Release Notes (Mar 7, 2026)

This release marks a major leap in agent observability with the launch of the Cloud Log Viewer and structured messaging pipeline. It also introduces significant UI overhauls for agent management, enhanced GCP integration, and a new workstation-class daemon mode for the Scion server.

## 🚀 Features
* **Cloud Log Viewer & Structured Messaging (Phases 1-5):** Completed the end-to-end implementation of the Cloud Log Viewer and structured message pipeline. This includes a new Hub API for log retrieval, a dedicated "Messages" tab in the Web UI, and a multi-stage message broker adapter for reliable delivery and external notifications.
* **Agent Detail UI Overhaul:** Re-architected the agent detail page into a high-density tabbed layout featuring dedicated "Status", "Configuration", and "Messages" tabs. Added a new telemetry configuration card, breadcrumb navigation improvements, and a back button for the configuration flow.
* **Workstation & Daemon Management:** Introduced a workstation-optimized "daemon" mode for `scion server`. This allows the server to run as a persistent background process with integrated lifecycle management, simplified configuration, and automated combined-server detection for local brokers.
* **GCP & Metrics Integration:** Enhanced Google Cloud visibility with a native Cloud Monitoring exporter, trace-log correlation across logging pipelines, and automated injection of `SCION_GROVE_ID` and GCP labels (agent/grove) into all log streams.
* **Image Management & Build Automation:** Consolidated image build scripts and introduced support for custom `image_registry` settings. Added GitHub Actions workflows for automated building and delivery of Scion harness images.
* **Security & Authorization Hardening:** Strengthened the security posture by enforcing per-agent authorization for workspace routes, mandatory read authorization for all resource endpoints, and nonce-based HMAC validation for broker communication.
* **First-Run Experience:** Added a new `scion install` command and a streamlined first-run experience to simplify initial project setup and dependency verification.
* **Bulk Operations:** Added a "Stop All" button to the Web UI for efficient bulk shutdown of all agents within a grove.
* **Harness Capability Gating:** Introduced capability-based gating for advanced agent configuration, ensuring only supported features are exposed based on the selected harness.

## 🐛 Fixes
* **UI Performance & Reliability:** Optimized the agent detail page by parallelizing API fetches and eliminating redundant data loads. Resolved rendering issues in the messages tab and added handling for null entries in message logs.
* **Auth & Environment Injection:** Fixed multiple issues with environment variable and profile injection, specifically resolving signing errors in combined-server mode and ensuring profile variables are applied before auth overlays.
* **Runtime & Broker Stability:** Improved Podman error handling and force-deletion reliability. Fixed a bug where `agent-limits.json` lacked correct permissions after creation and ensured `InlineConfig` is correctly propagated during agent restarts.
* **Logging Precision:** Established a dedicated HTTP request log stream using the standard `HttpRequest` format and removed misleading debug logs when running in GCP-native mode.
* **Build System:** Fixed a race condition in `make all` by ensuring web assets are fully built before the Go binary compilation begins.

# Release Notes (Mar 6, 2026)

This release introduces Just-In-Time (JIT) agent configuration, an advanced agent creation interface, and native GCP telemetry integration, while centralizing profile management at the global level.

## ⚠️ BREAKING CHANGES
* **Global Profile Management:** Runtime `profiles` and `runtimes` are no longer supported in grove-level `settings.yaml`. These must now be managed exclusively at the global/broker level (`~/.scion/settings.yaml`). Existing grove-specific profiles must be migrated to the global configuration.

## 🚀 Features
* **Just-In-Time (JIT) Agent Configuration:** Completed Phases 1 & 2 of the inline agent configuration refactor. Agents now support dynamic, late-bound configuration overrides at runtime, enabling more flexible and adaptive agent behavior.
* **Advanced Agent Creation Form:** Launched a comprehensive advanced configuration interface in the Web UI. This allows for granular control over agent parameters, including model selection, resource limits, and specific harness settings during creation.
* **GCP-Native Telemetry Integration:** Introduced native support for Google Cloud Trace and Cloud Logging telemetry exporters. The system now automatically detects GCP credentials and configures the appropriate exporter mode, facilitating seamless observability in Google Cloud environments.
* **Enhanced Developer Workflow:** Improved the developer experience with automated mounting of the `sciontool` binary and a dedicated `SCION_DEV_BINARIES` directory, enabling rapid iteration and testing of local changes within agent containers.
* **Branding & UI Refresh:** Updated the application branding with a new seedling logo and favicon, and added detailed visibility of the resolved harness authentication method in the agent detail view.
* **Local Networking Automation:** Automated the computation of the `ContainerHubEndpoint` for Podman and Docker when running in combined hub-broker mode, simplifying local setup and networking.

## 🐛 Fixes
* **Telemetry & Auth Propagation:** Resolved several issues where telemetry settings, harness authentication, and configuration overrides were not consistently propagated through all broker and agent startup paths.
* **Agent Lifecycle Stability:** Fixed a bug where provisioning agents were not correctly cleaned up after an aborted environment-gathering session.
* **Claude Harness Authentication:** Corrected Vertex AI authentication detection for the Claude harness when using file-based secrets.
* **Data Integrity:** Fixed a bug in the advanced agent creation form where the applied configuration was not correctly populated with resolved values.

# Release Notes (Mar 5, 2026)

This release introduces a major overhaul of the agent authentication pipeline, automated token refresh, and critical stability fixes for container removal and terminal reliability.

## ⚠️ BREAKING CHANGES
* **Credential Key Migration:** The internal secret key `OAUTH_CREDS` has been renamed to `GEMINI_OAUTH_CREDS`. Users must migrate existing secrets to this new key to maintain Gemini harness functionality.
* **Harness Auth Refactor:** Legacy harness-specific authentication methods have been retired in favor of a unified `ResolvedAuth` pipeline. Custom harness implementations or manual environment overrides may require updates to align with the new late-binding logic.

## 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor of the agent authentication pipeline. Agents now support a variety of resolved auth types (API Key, Vertex AI, ADC, OAuth) with late-binding overrides available via the CLI (`--harness-auth`) and the agent creation form.
* **Agent Token Refresh:** Implemented an automated token refresh mechanism to ensure long-running agents maintain valid authorization throughout extended tasks.

## 🐛 Fixes
* **Apple-Container Stability:** Resolved critical hangs during container removal on macOS by implementing automated cleanup and blocking of problematic debug symlinks (e.g., `.claude_debug`).
* **Terminal UX & Reliability:** Improved error visibility by skipping terminal reset sequences on attachment failures.
* **Workspace & Git Integrity:** Hardened workspace file collection by skipping symlinks and ensured `git clone` operations correctly use the `scion` user when the broker runs as root.
* **Auth Precision & Validation:** Fixed several authentication regressions, including incorrect Vertex AI region projections, false API key requirements during environment gathering, and improper leakage of host settings into agent containers.

# Release Notes (Mar 4, 2026)

This period focuses on the foundational implementation of the unified harness authentication pipeline and enhances infrastructure visibility within the Web UI.

## ⚠️ BREAKING CHANGES
* **Harness Authentication Pipeline:** The implementation of the unified `ResolvedAuth` model (Phases 1-7) replaces legacy harness-specific authentication methods. While finalized in the Mar 5 release, the core architectural shift and retirement of legacy methods occurred in this period.

## 🚀 Features
* **Unified Harness Authentication:** Completed a multi-phase refactor (Phases 1-7) of the agent authentication pipeline. Introduced centralized `AuthConfig` gathering, per-harness `ResolveAuth` logic, and a unified `ValidateAuth` phase, enabling more robust credential resolution across all harnesses.
* **Broker Visibility & Infrastructure Metadata:** Enhanced the Web UI to display runtime broker information on agent cards, grove detail pages, and agent detail headers, providing clearer insight into distributed execution.
* **Default Notification Triggers:** Expanded the notification system to include `stalled` and `error` as default trigger states, improving proactive monitoring of agent health.

## 🐛 Fixes
* **Workspace Permissions:** Hardened the workspace provisioning flow by ensuring `git clone` operations run as the `scion` user when the broker is executing as root.
* **UI Navigation & UX:** Fixed back-link routing for agent creation and detail pages to consistently return users to the parent grove. Improved terminal accessibility by disabling the terminal button for offline agents.
* **Config & Environment Propagation:** Resolved issues with `harnessConfig` propagation during the environment-gathering finalization flow and refined Hub endpoint bridging to only target `localhost` endpoints.
* **Server Reliability:** Applied default `StalledThreshold` values for agent health monitoring and improved status badge readability.

# Release Notes (Mar 3, 2026)

This release introduces hierarchical subsystem logging, an integrated browser push notification system, and native support for GKE runtimes and OTLP telemetry.

## 🚀 Features
* **Structured Subsystem Logging:** Introduced a hierarchical, subsystem-based structured logging framework across the Hub and Runtime Broker. This enables more granular observability and easier troubleshooting by isolating logs for specific components like the scheduler, dispatcher, and runtimes.
* **Agent Notifications & Browser Push:** Launched an integrated notification system with real-time SSE delivery and agent-scoped filtering. Features include a new notification tray in the Web UI, opt-in checkboxes for agent creation, and native browser push notification support.
* **Telemetry & OTLP Pipeline:** Added native support for OTLP log receiving and forwarding. The system now supports automated telemetry export with GCP credential injection, manageable via new CLI flags (`--enable-telemetry`) and UI toggles.
* **Stalled Agent Detection:** Implemented a new monitoring system to detect agents that have stopped responding (heartbeat timeout). Stalled agents are now flagged in the UI and can trigger automated notification events.
* **GKE Runtime Support:** Added native support for Google Kubernetes Engine (GKE) runtimes, including cluster provisioning scripts and Workload Identity integration for secure, distributed agent execution.
* **Layout & View Toggles:** Enhanced the Web UI with card/list view toggles for Groves, Agents, and Brokers pages, improving resource visibility for both small and large deployments.
* **Broker Access Control:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.

## 🐛 Fixes
* **Terminal UX:** Fixed double-paste and selection-copy bugs in the web terminal.
* **UI Responsiveness:** Resolved an issue where the agent list could incorrectly clear during real-time SSE updates and improved status badge readability.
* **Agent Provisioning:** Prevented root-owned directories in agent home by pre-creating secret and gcloud mount-point directories.
* **Administrative Security:** Hardened the Hub by restricting access to global settings and sensitive resource management (env/secrets) to administrative users.
* **Server Stability:** Fixed scheduler startup in combined mode and resolved heartbeats from defeating stalled agent detection.
* **CLI UX:** Standardized CLI scope flags and corrected secret set syntax for hub-scoped resources.

# Release Notes (Mar 2, 2026)

This release focuses on refining the agent lifecycle experience with an overhauled status and activity tracking system, enhanced grove-level configuration, and improved CLI flexibility for remote operations.

## 🚀 Features
* **Status & Activity Tracking Overhaul:** Replaced the generic `STATUS` with a more precise `PHASE` column across the CLI and Web UI. Introduced "sticky" activity logic to ensure significant agent actions remain visible during transitions, and enabled real-time status broadcasting via SSE for broker heartbeats.
* **Grove Environment & Secret Management:** Launched a dedicated configuration interface for managing grove-scoped environment variables and secrets. Includes a new "Injection Mode" selector (Always vs. As-Needed) for granular control over agent environment population.
* **Remote Grove Targeting:** Enhanced the `--grove` flag to natively accept grove slugs and git URLs in Hub mode, streamlining operations on remote workspaces without requiring local configuration.
* **Unified Configuration UX:** Consolidated grove-specific configuration into a centralized settings page in the Web UI, utilizing shared components for environment and secret management.

## 🐛 Fixes
* **Container Runtime Compliance:** Fixed an issue where secret volume mounts were incorrectly ordered in container run commands, ensuring reliable mounting across different runtimes.
* **Agent Identity Reliability:** Resolved bugs preventing the consistent propagation of `SCION_AGENT_ID` during restarts and specific dispatch paths, fixing broken notification subscriptions.
* **Linked-Grove Pathing:** Corrected workspace resolution for linked groves without git remotes by ensuring fallback to the provider's local filesystem path.
* **UI State Resolution:** Fixed a bug where hub agents would occasionally show an "unknown" phase by ensuring the UI correctly reads the unified Phase and Activity fields.
* **UX Refinements:** Improved the `scion list` output to use human-friendly template names and fixed dynamic label mapping in secret configuration forms.
* **Stability:** Suppressed spurious errors during graceful server shutdown and resolved potential issues with higher-priority environment variable leakage in tests.

# Release Notes (Mar 1, 2026)

This release introduces strict runtime enforcement for agent resource limits and includes several critical stability and performance improvements across the server and build pipeline.

## 🚀 Features
* **Agent Resource Limits Enforcement:** Implemented strict runtime enforcement for agent constraints, including `max_turns`, `max_model_calls`, and `max_duration`. Agents exceeding these limits are now automatically transitioned to a `LIMITS_EXCEEDED` state and terminated.

## 🐛 Fixes
* **Bundle Size Optimization:** Implemented vendor chunk splitting in the Vite build process to resolve bundle size warnings and improve frontend load performance.
* **Server Stability:** Resolved a critical panic that occurred during double-close operations in the combined Hub+Web server shutdown sequence.
* **Secret Mapping:** Corrected the mapping of secret type fields and standardized dynamic key/name labels to ensure consistency with backend providers.

# Release Notes (Feb 28, 2026)

This release marks a major milestone with the completion of the canonical agent state refactor and the launch of the Hub scheduler system, alongside significant enhancements to real-time observability and broker security.

## ⚠️ BREAKING CHANGES
* **Unified State Model:** The legacy `Status` and `SessionStatus` fields have been fully retired in favor of a canonical, layered agent state model. Downstream consumers of the Hub API or `sciontool` status outputs must update to the new schema.
* **Notification Triggers:** In alignment with the state refactor, notification `TriggerStatuses` have been renamed to `TriggerActivities`.

## 🚀 Features
* **Canonical Agent State Refactor:** Completed a comprehensive, multi-phase overhaul of the agent state system across the Hub, Store, Runtime Broker, CLI, and Web UI. This ensures a consistent, high-fidelity representation of agent activity throughout the entire lifecycle.
* **Hub Scheduler & Timer Infrastructure:** Launched a unified scheduling system for recurring Hub tasks and one-shot timers. This includes automated heartbeat timeout detection for "zombie" agents and a new CLI/API for managing scheduled maintenance and lifecycle events.
* **Real-time Debug Observability:** Introduced a full-height debug panel in the Web UI, providing a real-time stream of SSE events and internal state transitions for advanced troubleshooting and observability.
* **Enhanced Web UI Feedback:** Added emoji-based status badges to agent cards and list views, providing more intuitive visual indicators of agent health and activity.
* **Broker Authorization & Identity:** Strengthened security by enforcing dispatch authorization checks and resolving creator identities for all registered runtime brokers.
* **Automated Grove Cleanup:** Hardened the hub-native grove lifecycle by implementing cascaded directory cleanup on remote brokers whenever a grove is deleted via the Hub.
* **CLI Enhancements:** Added a new `-n/--num-lines` flag to the `scion look` command, enabling tailored views of agent terminal output.

## 🐛 Fixes
* **Notification Dispatcher:** Fixed a bug where the notification dispatcher failed to start when the Hub was running in combined mode with the Web server.
* **Environment Variable Standardization:** Renamed `SCION_SERVER_AUTH_DEV_TOKEN` to `SCION_AUTH_TOKEN` and introduced `SCION_BROKER_ID` and `SCION_TEMPLATE` variables for better debugging and interoperability.
* **Local Secret Storage:** Resolved issues with local secret storage and added diagnostics for environment-gathering resolution.

# Release Notes (Feb 27, 2026)

This release focuses on refining the hub-native grove experience, enhancing the web terminal's usability, and introducing new workspace management capabilities via the Hub API.

## 🚀 Features
* **Workspace Management:** Added new Hub API endpoints for downloading individual workspace files and generating ZIP archives of entire groves, facilitating easier data export and backup.
* **Broker Detail View:** Launched a comprehensive broker detail page in the Web UI, providing a grouped view of all active agents by their respective groves for improved operational visibility.
* **Deployment Automation:** Enhanced GCE deployment scripts with new `fast` and `full` modes, streamlining the process of updating Hub and Broker instances in production environments.
* **Iconography Standardization:** Established a centralized icon reference system and updated the web interface to use consistent iconography for resources like groves, templates, and brokers.

## 🐛 Fixes
* **Hub-Native Path Resolution:** Resolved several critical issues where hub-native groves incorrectly inherited local filesystem paths from the Hub server. Broker-side initialization of `.scion` directories and explicit path mapping now ensure consistent workspace behavior across distributed brokers.
* **Terminal & Clipboard UX:** Enabled native clipboard copy/paste support in the web terminal and relaxed availability checks to allow terminal access during agent startup and transition states.
* **Real-time Data Integrity:** Fixed a bug in the frontend state manager where SSE delta updates could merge incorrectly; the manager is now reliably seeded with full REST data upon page load.
* **Slug & Case Sensitivity:** Normalized agent slug lookups to lowercase and implemented stricter name validation to prevent routing collisions and inconsistent dispatcher behavior.
* **Environment & Harness Config:** Improved the reliable propagation of harness configurations and environment variables from Hub storage to the runtime broker during both initial agent start and subsequent restarts.
* **UI Refinement:** Replaced text-based labels with intuitive iconography on agent cards to optimize space and improved contrast for neutral status badges.

# Release Notes (Feb 26, 2026)

This release introduces a robust capability-based access control system, a dedicated administrative management suite, and critical session management upgrades to support larger authentication payloads.

## 🚀 Features
* **Capability-Based Access Control:** Implemented a comprehensive capability gating system across the Hub API and Web UI. Resource responses now include `_capabilities` annotations, enabling granular UI controls and API-level enforcement for resource operations.
* **Administrative Management Suite:** Launched a new Admin section in the Web UI, providing centralized views for managing users, groups, and brokers. Includes a maintenance mode toggle for Hub and Web servers to facilitate safe infrastructure updates.
* **Advanced Environment & Secret Management:** Introduced a profile-based settings section for managing user-scoped environment variables and secrets. Secrets are now automatically promoted to the configured backend (e.g., GCP Secret Manager) with standardized metadata labels.
* **SSR Data Prefetching:** Improved initial page load performance and eliminated "flash of unauthenticated content" by prefetching critical user and configuration data into the HTML payload via `__SCION_DATA__`.
* **Hub Scheduler Design:** Completed the technical specification for a new Hub scheduler and timer system to manage long-running background tasks and lifecycle events.
* **Enhanced Real-time Monitoring:** Expanded Server-Sent Events (SSE) support to the Brokers list view, ensuring infrastructure status is reflected in real-time without manual refreshes.

## 🐛 Fixes
* **Filesystem Session Store:** Replaced cookie-based session storage with a filesystem-backed store to resolve "400 Bad Request" errors caused by cookie size limits (4096 bytes) during large JWT/OAuth exchanges.
* **Hub-Native Grove Reliability:** Fixed critical 503 errors and path resolution issues during agent creation in hub-native groves by correctly propagating grove slugs to runtime brokers.
* **Agent Deletion Cleanup:** Hardened the agent deletion flow to ensure that stopping and removing an agent in the Hub correctly dispatches cleanup commands to the associated runtime broker and removes local workspace files.
* **Environment Validation:** Improved agent startup safety by treating missing required environment variables as fatal errors (422), preventing agents from starting in incomplete states.
* **Terminal Responsiveness:** Resolved several layout bugs in the web terminal, ensuring it correctly resizes with the viewport and fits within the application shell.
* **Group Persistence:** Fixed synchronization issues between the Hub's primary database and the Ent-backed authorization store, ensuring grove-scoped groups and policies are preserved during recreation.

# Release Notes (Feb 25, 2026)

This release focuses on hardening the agent provisioning pipeline, streamlining template management through automatic bootstrapping, and enhancing the web authentication experience.

## 🚀 Features
* **Template Bootstrapping:** Local agent templates are now automatically bootstrapped into the Hub database during server startup, ensuring all defined templates are consistently available across the system.
* **Custom ADK Runner Entrypoint:** Introduced a specialized runner entrypoint for Agent Development Kit (ADK) agents with native support for the `--input` flag, facilitating more robust automated execution.
* **Wildcard Subdomain Authorization:** Expanded security configuration to support wildcard subdomain matching in `authorized-domains`, allowing for more flexible deployment architectures.

## 🐛 Fixes
* **Agent Provisioning & Creation:** Resolved multiple issues in the Hub-dispatched agent creation flow, including a 403 authorization fix, rejection of duplicate agent names, and a critical fix for container image resolution.
* **Instruction Injection Logic:** Improved the reliability of agent instructions by implementing auto-detection for `agents.md` and ensuring stale instruction files (e.g., lowercase `claude.md`) are removed during provisioning.
* **Web UI & Auth Persistence:** Fixed a bug where the authenticated user wasn't correctly fetched on page load, ensuring the profile and sign-out options are always visible in the header.
* **Pathing & Scoping:** Corrected path resolution logic to prevent local-path groves from incorrectly using hub-native paths, and refined the `scion delete --stopped` command to strictly scope to the active grove.
* **Environment Gathering:** Fixed a regression in the `env-gather` finalize-env flow to ensure the template slug is correctly preserved throughout the entire provisioning pipeline.
* **Configuration Schema:** Added `task_flag` support to the settings schema and Hub configuration, improving the tracking and validation of agent task states.

# Release Notes (Feb 24, 2026)

This release introduces a robust policy-based authorization system, a comprehensive agent notification framework, and significant enhancements to hub-native groves and schema validation.

## ⚠️ BREAKING CHANGES
* **Policy-Based Authorization:** Strictly enforced authorization for agent operations. Agent creation now requires grove membership, while interaction (PTY, messaging) and deletion are restricted to the agent's owner (creator) or system administrators.

## 🚀 Features
* **Agent Notifications System:** Launched a multi-phase notification framework enabling real-time subscriptions to agent status events. This includes a new notification dispatcher, Hub API endpoints, and a `--notify` flag in the CLI for status tracking.
* **Harness-Agnostic Templates:** Introduced support for role-based, harness-agnostic agent templates. New fields for `agent_instructions`, `system_prompt`, and `default_harness_config` allow templates to be defined by their role rather than specific LLM implementations.
* **GKE Security Enhancements:** Added a dedicated `gke` runtime configuration option to enable GKE-specific features like Workload Identity, streamlining secure deployments on Google Kubernetes Engine.
* **Hub-Native Workspace Management:** Advanced hub-native grove capabilities (Phase 3) with new support for direct workspace file management via the Hub API, reducing reliance on external Git repositories.
* **ADK Agent Integration:** Added a specialized example and Docker template for Agent Development Kit (ADK) agents, facilitating the development of custom autonomous agents within the Scion ecosystem.
* **Infrastructure & Models:** Upgraded the default agent model to `gemini-3-flash-preview` and introduced Cloud Build configurations for automated image delivery.

## 🐛 Fixes
* **Schema & Config Synchronization:** Conducted a comprehensive audit and sync between Go configuration structs and JSON schemas. This fixes field naming inconsistencies (e.g., camelCase for `runtimeClassName`) and improves cross-platform validation.
* **Environment Variable Passthrough:** Corrected environment handling to treat empty variable values as implicit host environment passthroughs.
* **Per-Agent Hub Overrides:** Enabled agents to specify custom Hub endpoints directly in their configuration, providing flexibility for agents to report to different Hubs than their parent grove.
* **Soft-Delete Configuration:** Added explicit server-side settings for soft-delete retention periods and workspace file preservation.

# Release Notes (Feb 23, 2026)

This period focused on major architectural expansions, introducing multi-hub connectivity for runtime brokers and "hub-native" groves that decouple workspace management from external Git repositories.

## 🚀 Features
* **Multi-Hub Broker Architecture:** Completed a major refactor of the Runtime Broker to support simultaneous connections to multiple Hubs. This includes a new multi-credential store, per-connection heartbeat management, and a "combo mode" that allows a broker to be co-located with one Hub while serving others remotely.
* **Hub-Native Groves:** Launched "Hub-Native" groves, enabling the creation of project workspaces directly through the Hub API and Web UI without an external Git repository. These groves are automatically initialized with a seeded `.scion` structure and managed locally by the Hub.
* **Streamlined Workspace Creation:** Introduced a new grove creation interface in the Web UI that supports both Git-based repositories and Hub-native workspaces, including direct Git URL support for quick onboarding.
* **Improved Agent Configuration:** Enhanced the agent creation form with optimized dropdowns and more intuitive labeling, including renaming "Harness" to "Type" for better clarity.

## 🐛 Fixes
* **Web UI Asset Reliability:** Resolved several issues with Shoelace icon rendering by correctly synchronizing the icon manifest, fixing asset serving paths in the Go server, and updating CSP headers to allow data-URI system icons.
* **Template Flexibility:** Updated the template push logic to make the harness type optional, facilitating the use of more generic or agnostic agent templates.
* **Codex Harness Refinement:** Improved the Codex integration by isolating harness documentation into a dedicated `.codex/` subdirectory and removing unnecessary system prompt prepending.

# Release Notes (Feb 22, 2026)

This period introduced significant data management features, including agent soft-delete and centralized harness configuration storage, while advancing the secrets management and execution limits infrastructure.

## 🚀 Features
* **Agent Soft-Delete & Restore:** Implemented a complete soft-delete lifecycle for agents. This includes Hub-side archiving, a new `scion restore` command, list filtering for deleted agents, and an automated background purge loop for expired records.
* **Secrets-Gather & Interactive Input:** Enhanced the environment gathering pipeline to support "secrets-gather." Templates can now define required secrets, and the CLI provides interactive prompts to collect missing values, which are then securely backed by the configured secret provider.
* **K8s Native Secret Mounting:** Completed Phase 4 of the secrets strategy, enabling native secret mounting for agents running in Kubernetes. This includes support for GKE CSI drivers and robust fallback paths.
* **Harness Config Hub Storage:** Added Hub-resident storage for harness configurations. This enables centralized management (CRUD), CLI synchronization, and ensures configurations are consistently propagated to brokers during agent creation.
* **Agent Execution Limits:** Introduced Phase 1 of the agent limits infrastructure, including support for `max_turns` and `max_duration` constraints and a new `LIMITS_EXCEEDED` agent state.
* **CLI UX Improvements:** Added a `--all` flag to `scion stop` for bulk agent termination, introduced Hub auth verification with version reporting, and enhanced `scion look` with better visual padding and borders.
* **Web UI & Real-time Updates:** Launched a new "Create Agent" UI, optimized frontend performance by moving to explicit component imports, and enabled real-time grove list updates via Server-Sent Events (SSE).

## 🐛 Fixes
* **Provisioning Robustness:** Improved cleanup of provisioning agents during failed or cancelled environment gathering sessions to prevent stale container accumulation.
* **Sync & State Consistency:** Fixed a race condition where Hub synchronization could remove freshly created agents and ensured harness types are correctly propagated during agent sync.
* **Deployment Pipeline:** Corrected the build sequence in GCE deployment scripts to ensure web assets are fully compiled before the Go binary is built.
* **Config Resolution:** Fixed several configuration issues, including profile runtime application, grove flag resolution in subdirectories, and Hub environment variable suppression when the Hub is disabled.

# Release Notes (Feb 21, 2026)

This period heavily focused on implementing the end-to-end "env-gather" flow to manage environment variables safely, alongside several CLI improvements and runtime fixes.

## 🚀 Features
* **Env-Gather Flow Pipeline:** Implemented a comprehensive environment variable gathering system across the CLI, Hub, and Broker. This includes harness-aware env key extraction, Hub 202 handling with submission endpoints, and broker-side evaluation to finalize the environment prior to agent creation.
* **Agent Context Threading:** Threaded the CLI hub endpoint directly to agent containers and added support for environment variable overrides.
* **Agent Dashboard Enhancements:** The agent details page now displays the `lastSeen` heartbeat as a relative time format.
* **Template Pathing:** Added support for `SCION_EXTRA_PATH` to optionally include template bin directories in the system `PATH`.
* **Build System Upgrades:** Overhauled the Makefile with new standard targets for build, install, test, lint, and web compilation.

## 🐛 Fixes
* **Env-Gather Safety & UX:** Added strict rejection of env-gather in non-interactive modes to prevent unsanctioned variable forwarding. Improved confirmation messaging and added dispatch support for grove-scoped agent creation.
* **CLI Output Formatting:** Redirected informational CLI output to `stderr` to ensure `stdout` can be piped cleanly as JSON.
* **Podman Performance:** Fixed slow container provisioning on Podman by directly editing `/etc/passwd` instead of using `usermod`.
* **Profile Parameter Routing:** Corrected the threading of the profile parameter from the CLI through the Hub to the runtime broker.
* **Hub API Accuracy:** The Hub API now correctly surfaces the `harness` type in responses for agent listings.
* **Docker Build Context:** Fixed an issue where the `scion-base` Docker image build was missing the web package context.

# Release Notes (Feb 20, 2026)

This period focused heavily on unifying the Hub API and Web Server architectures, refactoring the agent status model, and enhancing the web frontend experience with new routing and pages.

## ⚠️ BREAKING CHANGES
* **Status Model:** Consolidated the `SessionStatus` field into the primary `Status` field across the codebase (API, Database, UI). The `WAITING_FOR_INPUT` and `COMPLETED` states are now treated as "sticky" statuses.
* **Server Architecture:** Combined the Hub API and Web server to serve on a single port (`8080`) when both are enabled. API traffic is now routed to `/api/v1/`, resolving CORS issues and simplifying local deployment.

## 🚀 Features
* **Web Frontend Enhancements:** Added a new Brokers list page, implemented full client-side routing for the Vite dev server, and unified OAuth provider detection via a new `/auth/providers` endpoint.
* **Agent Environment:** Added support for injecting harness-specific telemetry and hub environment variables directly into agent containers based on grove settings.
* **Git Operations:** Added cloning status indicators and improved git clone config parity during grove-scoped agent creation.

## 🐛 Fixes
* **Real-time UI Updates:** Fixed the Server-Sent Events (SSE) format to ensure real-time UI updates correctly broadcast agent state changes.
* **Routing & Port Prioritization:** Fixed port prioritization to use the web port for broker hub endpoints in combined mode, and ensured unhandled `/api/` routes return proper JSON 404 responses.
* **OAuth & Login:** Fixed conditional rendering for the `/login` route and correctly populated OAuth provider attributes during client-side navigation.
* **Container Configuration:** Fixed container image resolution from on-disk harness configurations and normalized YAML key parsing.
* **Status Reporting:** Ensured Hub status reporting correctly respects and preserves the newly unified, sticky statuses.

# Release Notes (Feb 19, 2026)

This period represented a major architectural shift, consolidating the web server into a single Go binary, removing dependencies like NATS and Koa, and introducing hub-first remote workspaces via Git.

## ⚠️ BREAKING CHANGES
* **Secrets Management:** The system now strictly requires a configured production secret backend (e.g., `gcpsm`) for any secret Set operations across user, grove, and runtime broker scopes. Plaintext fallbacks have been removed. Read, list, and delete operations remain functional locally to support data migration.
* **Server Architecture:** The Node.js Koa server and NATS message broker dependencies have been completely retired. The Scion Hub now natively handles web frontend serving, SPA routing, and Server-Sent Events (SSE) via a consolidated Go binary.

## 🚀 Features
* **Hub-First Git Workspaces:** Implemented end-to-end support for creating remote workspaces directly from Git URLs. This integration enables git clone mode across `sciontool init` and the runtime broker pipeline.
* **Web Server & Auth Integration:** Introduced native session management and OAuth routing within the Go web server, alongside a new EventPublisher for real-time SSE streaming.
* **Telemetry & Settings:** Added telemetry injection to the `v1` settings schema. Telemetry configuration now supports hierarchical merging and is automatically bridged into the agent container's environment variables.
* **CLI Additions:** Introduced the `scion look` command for non-interactive terminal viewing. Project initialization now automatically sets up template directories and requires a global grove.

## 🐛 Fixes
* **Lifecycle Hooks:** Relocated the cleanup handler to container lifecycle hooks to guarantee reliable execution upon container termination.
* **Settings Overrides:** Fixed configuration parsing to ensure environment variable overrides are correctly applied when loaded from `settings.yaml`.
* **CLI Defaults:** Ensured the `update-default` command consistently targets the global grove, and introduced a new `--force` flag.
* **Frontend Assets:** Resolved static asset serving issues by removing an erroneous `StripPrefix` in the router, and fixed client entry point imports.
