# Release Notes (Feb 27, 2026)

This release focuses on refining the hub-managed grove experience, enhancing the web terminal's usability, and introducing new workspace management capabilities via the Hub API.

## 🚀 Features
* **Workspace Management:** Added new Hub API endpoints for downloading individual workspace files and generating ZIP archives of entire groves, facilitating easier data export and backup.
* **Broker Detail View:** Launched a comprehensive broker detail page in the Web UI, providing a grouped view of all active agents by their respective groves for improved operational visibility.
* **Deployment Automation:** Enhanced GCE deployment scripts with new `fast` and `full` modes, streamlining the process of updating Hub and Broker instances in production environments.
* **Iconography Standardization:** Established a centralized icon reference system and updated the web interface to use consistent iconography for resources like groves, templates, and brokers.

## 🐛 Fixes
* **Hub-Managed Path Resolution:** Resolved several critical issues where hub-managed groves incorrectly inherited local filesystem paths from the Hub server. Broker-side initialization of `.scion` directories and explicit path mapping now ensure consistent workspace behavior across distributed brokers.
* **Terminal & Clipboard UX:** Enabled native clipboard copy/paste support in the web terminal and relaxed availability checks to allow terminal access during agent startup and transition states.
* **Real-time Data Integrity:** Fixed a bug in the frontend state manager where SSE delta updates could merge incorrectly; the manager is now reliably seeded with full REST data upon page load.
* **Slug & Case Sensitivity:** Normalized agent slug lookups to lowercase and implemented stricter name validation to prevent routing collisions and inconsistent dispatcher behavior.
* **Environment & Harness Config:** Improved the reliable propagation of harness configurations and environment variables from Hub storage to the runtime broker during both initial agent start and subsequent restarts.
* **UI Refinement:** Replaced text-based labels with intuitive iconography on agent cards to optimize space and improved contrast for neutral status badges.
