# Release Notes (2026-04-22)

This update focuses on improving the workspace browsing experience and enhancing UI accessibility, alongside key fixes for template management.

## 🚀 Features
* **Workspace File Browser:** Added a dot-file visibility toggle. Hidden files (starting with `.`) are now filtered out by default to provide a cleaner workspace view. A new "show .dot files" checkbox allows users to reveal hidden configuration files as needed.

## 🐛 Fixes
* **Template Management:** Resolved an issue where the template harness type remained stale after editing `scion-agent.yaml` via the web UI. The harness type is now correctly re-detected upon file updates.
* **UI Accessibility:** Improved status badge contrast in the light theme. Component colors now correctly respect the application's active theme rather than the system's preferred color scheme and have been darkened to meet WCAG accessibility standards.
