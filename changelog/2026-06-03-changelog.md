# Release Notes (2026-06-03)

This release focuses on improving Web UI usability, enhancing infrastructure provisioning flexibility, and providing comprehensive documentation for advanced deployment scenarios like multi-broker setups and external channel integrations.

## 🚀 Features

* **Web UI Enhancements:**
    * **Terminal Connectivity:** Added a prominent "DISCONNECTED" overlay to the web terminal. This full-terminal indicator provides immediate visual feedback when the WebSocket connection drops, replacing the subtle toolbar-only signal.
    * **Agent Management:** Introduced sorting and filtering capabilities to the agent list view, making it easier to manage and locate specific agents in larger environments.
* **Infrastructure & Provisioning:**
    * **Starter Hub Flexibility:** Added support for `MACHINE_TYPE` overrides in the `starter-hub` provisioning scripts, allowing for more granular control over GCE resource allocation.
* **Documentation:**
    * **Advanced Guides:** Published new documentation covering Multi-Broker setups, GCE Hub provisioning, and External Channel integrations (Telegram, Discord, and A2A protocol bridges).

## 🐛 Fixes

* **Stability:** Resolved a nil-pointer panic in the `harness-config` command that occurred when the Hub was disabled.
* **Setup Scripts:** Fixed a permission issue in `gce-demo-setup-repo.sh` by ensuring `sudo` is used for repository path existence checks.
* **Documentation:** Updated the `starter-hub` README to document `REGION` and `ZONE` override support.
