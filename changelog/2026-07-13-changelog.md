# Release Notes (2026-07-13)

A polish and correctness day: Claude skills are now installed as individual files instead of concatenated into CLAUDE.md, Copilot gained proper credential file capture, Discord received inbound attachment support, and the skill bank got URI display and permissions fixes.

## 🚀 Features
* **[Discord]:** Inbound attachment support — downloads Discord message attachments to the agent workspace and includes them in the `StructuredMessage` envelope, with path traversal sanitization and resource leak guards (#700).

## 🐛 Fixes
* **[Claude]:** Install skills to `.claude/skills/` not `CLAUDE.md` — pass `include_skills=False` to `project_instructions()` so skills remain as individual files where Claude Code discovers them natively, instead of being concatenated into a large CLAUDE.md (#705).
* **[Copilot]:** Capture `config.json` as `COPILOT_CONFIG` file secret and provision as credential file — also ensures workspace is in `trustedFolders` and fixes JSON validation to strip comment lines before parsing (#701).
* **[Copilot]:** Use `auth-file` instead of `config-file` as auth type name — the new type failed schema validation on production hubs (#703).
* **[Skill Bank]:** Fixed URI display (now shows canonical `skill://scion/scope/slug@latest`) and public skill permissions — public skills are now accessible to any authenticated user on all read-path handlers (#709).
* **[Hub]:** Surface schema validation errors in harness config import UI — previously returned silent HTTP 200 with empty list on failure (#704).
* **[Hub]:** Show integrations as Available when plugin binary is on `$PATH` — fixes Homebrew installs where `SCION_MAINTENANCE_REPO_PATH` is unset (#710).
* **[Web]:** Use textarea for file-type secret intake to support multi-line values (#708).
* **[Web]:** Sort template list — project templates first then global, both alphabetically (#707).
* **[Web]:** Added plant emoji favicon (#706).
* **[Claude]:** Changed extra-large model alias from opus to fable.

## 🔧 Chores
* **[Refactor]:** Removed legacy `agents-git.md` and `agents-hub.md` files — content moved to workspace skills.
