# Release Notes (2026-07-07)

A stabilization day focused on macOS runtime reliability, daemon process isolation, and chat interrupt correctness. Multiple fixes addressed Apple Container, Podman detection, and daemon credential prompt leaks.

## 🚀 Features
* **[Chat]:** Interrupt `!` processing for inbound broker path and `interrupt_sequence` support — messages via `/api/v1/broker/inbound` (Telegram) now have `!` prefix processed. New `interrupt_sequence` config field sends multiple keypresses (e.g. three Escapes for Claude Code) (#635).
* **[CLI]:** `scion version` now shows both tag and commit hash in the format `scion <version> (commit <hash>)` (#631).

## 🐛 Fixes
* **[Runtime]:** Prefer `podman > docker > apple-container` in macOS runtime auto-detection — Apple Container requires sudo for DNS, causing daemon crashes. Podman is now first choice on macOS (#632).
* **[Runtime]:** Detect Podman at non-standard macOS paths (`/opt/podman/bin`) (#636).
* **[Runtime]:** Remove Apple Container DNS sudo from server startup and onboarding; replaced with docs link (#637).
* **[Daemon]:** Detach daemon stdin from terminal to prevent credential prompt leaks from Docker's credential helper or macOS security APIs (#629).
* **[Daemon]:** Remove `Setsid` from daemon `SysProcAttr` — causes `EPERM` on macOS due to Homebrew entitlement restrictions. `Setpgid: true` alone is sufficient (#634).
* **[Hub]:** Archive obsolete bundled harness-configs on startup — only archives builtin-managed configs, always runs regardless of existing config count (#620).
* **[Sciontool]:** Redirect hook status messages from stdout to stderr, fixing Gemini CLI's JSON-only hook requirement. Added quiet mode for hook subcommands (#624).
* **[Server]:** Hosted mode runtime detection, Postgres schema migration, and build context fixes for standalone Discord multi-module Docker builds (#633).

## 📖 Docs
* **[Docs Site]:** Complete docs-site refresh continued — operational-mode axis (#627).
* **[Docs]:** Added Apple Container DNS setup page (#638).
* **[Docs]:** Moved broker-fanout design to `.design/`, added `docs/README.md` (#628).

## 🔧 Chores
* **[Deps]:** Bumped `golang.org/x/net` in scion-chat-app (#609).
