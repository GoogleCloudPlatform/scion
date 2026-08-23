# Release Notes (2026-08-19)

Two targeted UX improvements: secret capture now prompts for scope (project vs. user), and navigating between dashboard and chat preserves project context.

## 🚀 Features
* **[Auth]:** Prompt user for secret scope in capture auth flow — adds a Shoelace dialog with project/user scope selection before capture auth exec. `capture_auth.py` accepts `--scope`, `sciontool secret set` gains `--scope` flag (project|user), and the hub API routes user-scoped secrets via agent JWT `OriginUserID`. Backward compatible — defaults to project when omitted (#1222).
* **[Chat]:** Preserve project context when toggling between dashboard and chat — switching views now maintains the current project instead of resetting to top-level. Includes 19 new unit tests for URL-parsing helpers (#1225).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 18 — expand popover for agent-agent messages, immediate SSE attachment rendering (#1224).
