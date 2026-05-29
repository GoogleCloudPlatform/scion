# Release Notes (2026-04-30)

Today's updates introduce significant improvements to the file browsing experience with hybrid search and improved visibility into update results.

## 🚀 Features
* **Smart File List with Hybrid Search:** Overhauled the workspace and shared directory file browsers. The new "smart" listing defaults to showing the 500 most recently modified files and includes a debounced hybrid search (regex with fuzzy fallback) that queries the backend while merging with local results. (Commit: `163e5a6b`)
* **Enhanced Update Visibility:** The `pull-latest` results in the web UI now display a structured list of commits, providing immediate feedback on what was changed during an update. (Commit: `babb9aaf`)
* **Deployment Preflight Validation:** Added a preflight validation step to the `starter-hub` deployment pipeline to catch configuration errors early. (Commit: `b3c34b0c`)
* **Team-Creation Skill Extension:** Streamlined the team-creation skill and added support for extending existing teams. (Commit: `d7271f36`)
* **Web UI Enhancements:** Added a direct GitHub App link to the grove creation form for easier integration. (Commit: `c6be89f2`)

## 🐛 Fixes
* **Harness Image List:** Included `opencode` and `codex` in the default `pull-images` harness list. (Commit: `e256b999`)
* **Web UI - SA Verification:** Added a note regarding IAM propagation delay to the Service Account verification dialog to improve user clarity. (Commit: `03be867f`)
* **Sciontool Provisioning:** Resolved a `HOME` directory mismatch during container-script harness provisioning. (Commit: `600f75d7`)
