# Release Notes (2026-05-03)

This release focuses on refining the Chat App user experience, introducing a high-performance hybrid search for the File Browser, and resolving critical networking and database bottlenecks within the Hub and Broker infrastructure.

## 🚀 Features
* **Enhanced Chat Application:** Significantly improved the messaging experience with visible "sent" states, better card rendering, and a default agent for new conversations. Fixed race conditions during plugin startup and improved key auto-discovery for message signing.
* **Smart File Browser Search:** Implemented the first two phases of "Smart File List" in the File Browser, introducing a hybrid search system that combines indexed and real-time results for faster navigation.
* **Pull-Latest Commit Visibility:** The Web UI now displays a structured, readable list of incoming commits when performing a "Pull Latest" operation, providing better context for updates.
* **Starter Hub Preflight Validation:** Added comprehensive preflight checks to the Starter Hub deployment pipeline to catch configuration errors before deployment.
* **Docs Site Improvements:** Launched a standalone landing page for the documentation site and upgraded several core components for Astro 6.x and Node.js 24 compatibility.
* **Extended Team-Creation Skill:** Streamlined the team-creation skill and added a new extension mechanism to allow for more flexible agent orchestration.

## 🐛 Fixes
* **Broker Networking & NAT:** Resolved a critical GCE hairpin NAT issue affecting co-located Docker bridge containers. Optimized Broker-to-Hub communication by defaulting to localhost for co-located instances.
* **Hub Performance:** Eliminated Single-Page Application (SPA) lag caused by an SQLite single-connection bottleneck.
* **Security & Permissions:** 
    * Fixed a leak where metadata sidecar iptables rules could affect the host namespace.
    * Corrected file ownership and permissions for `scion-token` and `agent-info.json` to ensure they are readable by the appropriate service users.
* **Cloud Build Robustness:** Fixed several Cloud Build template issues, including variable declaration mismatches and Docker auth configuration overwriting during registry verification.
* **Agent & Harness Reliability:**
    * Enabled the container-script provisioner for Opencode by default.
    * Improved diagnostic logging by piping `provision.py` output directly to `agent.log`.
    * Fixed several harness configuration discovery and reloading bugs during grove auto-selection.
* **Web UI Polish:** Resolved a Vite entry point mismatch, added IAM propagation delay warnings, and improved GitHub App integration in the grove creation form.

## 🛠️ Internal & Others
* Added comprehensive post-mortems and investigation docs for GCP auth failures and metadata server 502s.
* Finalized the Discord chat adapter design document and resolved all open design questions.
* Upgraded several CI/CD workflows and GitHub Actions to Node.js 24.
