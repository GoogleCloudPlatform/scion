# Release Notes (Feb 23, 2026)

This period focused on major architectural expansions, introducing multi-hub connectivity for runtime brokers and "hub-managed" groves that decouple workspace management from external Git repositories.

## 🚀 Features
* **Multi-Hub Broker Architecture:** Completed a major refactor of the Runtime Broker to support simultaneous connections to multiple Hubs. This includes a new multi-credential store, per-connection heartbeat management, and a "combo mode" that allows a broker to be co-located with one Hub while serving others remotely.
* **Hub-Managed Groves:** Launched "Hub-Managed" groves, enabling the creation of project workspaces directly through the Hub API and Web UI without an external Git repository. These groves are automatically initialized with a seeded `.scion` structure and managed locally by the Hub.
* **Streamlined Workspace Creation:** Introduced a new grove creation interface in the Web UI that supports both Git-based repositories and Hub-managed workspaces, including direct Git URL support for quick onboarding.
* **Improved Agent Configuration:** Enhanced the agent creation form with optimized dropdowns and more intuitive labeling, including renaming "Harness" to "Type" for better clarity.

## 🐛 Fixes
* **Web UI Asset Reliability:** Resolved several issues with Shoelace icon rendering by correctly synchronizing the icon manifest, fixing asset serving paths in the Go server, and updating CSP headers to allow data-URI system icons.
* **Template Flexibility:** Updated the template push logic to make the harness type optional, facilitating the use of more generic or agnostic agent templates.
* **Codex Harness Refinement:** Improved the Codex integration by isolating harness documentation into a dedicated `.codex/` subdirectory and removing unnecessary system prompt prepending.
