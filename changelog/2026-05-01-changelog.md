# Release Notes (2026-05-01)

This period focused on improving the performance and reliability of the Hub's database layer, enhancing the file browsing experience, and refining the chat application and harness provisioning systems.

## 🚀 Features
* **Smart File Browser:** Implemented a new hybrid search system for the file browser (Phases 1 & 2), providing faster and more intuitive navigation within agent workspaces.
* **Enhanced Pull Visualization:** The "Pull Latest" action in the web UI now displays a structured commit list, giving users immediate visibility into changes fetched from remote repositories.
* **Chat App Visibility:** Sent messages are now explicitly displayed within the chat application, and card rendering has been improved to correctly handle newlines for better readability.
* **Team-Creation Skill Improvements:** Streamlined the team-creation skill and introduced extension support, simplifying the generation of complex, multi-agent templates.
* **Starter Hub Preflight:** Added automated preflight validation to the starter-hub deployment pipeline to catch configuration errors before deployment.
* **GitHub Integration:** Added a direct link to the GitHub App in the grove creation form to simplify the onboarding of app-configured repositories.

## 🐛 Fixes
* **SQLite Performance Overhaul:** Resolved major SPA responsiveness issues by eliminating a single-connection bottleneck. The Hub now uses a connection pool with WAL mode enabled, allowing concurrent readers and significantly reducing API latency during background tasks (like broker heartbeats).
* **Robust Harness Provisioning:**
    * Fixed `HOME` environment variable mismatches in `container-script` provisioning and enabled the `container-script` provisioner for Opencode by default.
    * Resolved a race condition in broker plugin startup.
    * Improved file permission management for `scion-token` and `agent-info.json` to ensure they remain readable by the `scion` user and broker after refreshes.
    * Ensured `container-script` bundles are reconciled on every agent start to maintain consistency.
* **Network & Connectivity:**
    * Fixed GCE hairpin NAT issues for co-located Docker bridge containers.
    * Co-located brokers now default to `localhost` for Hub communication, improving stability in complex network environments.
* **Cloud Build & CI/CD:**
    * Upgraded several CI actions and the web frontend to Node.js 24 and Vite 7-compatible configurations.
    * Fixed several Cloud Build issues related to Docker authentication, variable substitutions, and template variable declarations.
    * Updated `astro-d2` and `starlight-links-validator` for compatibility with Astro 6.x and Zod v4.
* **Web UI Refinements:**
    * Fixed Vite entry point and style import issues in the frontend.
    * Added IAM propagation delay notices to the Service Account verification dialog to manage user expectations during registration.
    * Fixed a bug where harness configurations were duplicated when listing by grove ID.

## 🛠️ Internal & Others
* **Discord Integration Design:** Finalized the architecture for the upcoming Discord chat adapter.
* **Documentation:** Added design documentation for Hub template administration and updated the starter-hub environment samples.
* **Maintenance Clarity:** Added a note to the Hub update dialog clarifying that active agents are not interrupted during Hub service updates.
* **Auth Investigation:** Documented an investigation into transient GCP authentication token failures.
