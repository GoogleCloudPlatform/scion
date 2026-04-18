# Release Notes (2026-04-16)

This release introduces a new "stop-all" capability for grove members, an in-place server self-update mechanism, and significant improvements to agent lifecycle management and deployment reliability.

## ⚠️ BREAKING CHANGES
* **[API] Agent Restart Status Code:** The agent restart endpoint now returns `200 OK` instead of `201 Created` when restarting an existing agent, accurately reflecting that no new resource was created.
* **[Infrastructure] Agent Recreate Behavior:** The "recreate agent" operation now explicitly deletes the old container and creates a new one from scratch instead of attempting an in-place restart. This ensures a clean state but results in the loss of any non-persistent data stored within the container.

## 🚀 Features
* **[Web UI] Stop All Agents:** Added a "stop-all" button allowing grove members to terminate all active agents in a single action.
* **[Infrastructure] In-Place Server Rebuilds:** Introduced a "check-for-updates" mechanism that enables administrators to trigger a server rebuild and restart directly from the web interface.
* **[Web UI] Three-Way Grove Filtering:** Upgraded the grove filtering system to a new scope selector that allows users to easily toggle between Personal, Shared, and All groves.
* **[Harness] Claude Authentication:** Expanded Claude harness support to include OAuth tokens and credentials-file authentication methods.

## 🐛 Fixes
* **[Infrastructure] Server Maintenance Reliability:** Improved the reliability of server maintenance by automatically aborting stalled tasks on startup and replacing polkit with sudoers for more consistent service restarts.
* **[Infrastructure] Reliable Server Rebuilds:** Resolved issues with the server rebuild process, including eliminating file-in-use (`ETXTBSY`) errors by using staging paths and ensuring correct root ownership for sudoers rules.
* **[Kubernetes] GKE Attach Reliability:** Resolved pod name resolution and password prompt issues when using the `attach` command on Google Kubernetes Engine (GKE).
* **[Broker] Log Path Resolution:** Fixed a bug where incorrect `.scion` suffixes were appended to agent log paths, which previously prevented log retrieval in certain environments.
* **[Web UI] Administration & Stability:** Restored missing pagination to the admin users page and reduced DOM flickering during maintenance polling.
* **[Deploy] Pipeline Optimization:** Optimized deployment scripts by removing redundant SSH calls and improving shell escaping for complex configuration blocks.
* **[Harness] Claude Workspace Pathing:** Fixed an issue where Claude agents using git-clones failed to resolve the correct `/workspace` project path.
