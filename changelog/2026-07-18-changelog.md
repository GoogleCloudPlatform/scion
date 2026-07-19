# Release Notes (2026-07-18)

A big messaging day: native @mention support shipped across Discord, Slack, and Telegram with a new position-aware mention message type, per-thread default agents landed for Discord, Telegram gained audio/video attachment downloads, and broker resilience improved with restart ID recovery, orphan re-assignment, and delivery error propagation.

## 🚀 Features
* **[Messaging]:** Position-aware @mention routing — new `TypeMention` message type enables mention-triggered agent dispatch with positional metadata (#809).
* **[Slack]:** Replace email references in outbound agent messages with native Slack @mentions, with duplicate-email handling and cached lookups (#808).
* **[Discord]:** Per-thread default agents — adds `thread_defaults` table with two-tier resolution and thread-aware `/setup`, `/default`, `/status` commands, mirroring the Telegram topic_defaults pattern (#807).
* **[Discord]:** Replace email references in outbound agent messages with native Discord `<@id>` mentions, ported from Telegram's `resolveOutboundMentions()` pattern (#806).
* **[Telegram]:** Download and deliver audio/video file attachments to agents (in addition to existing photo/document support), with configurable `downloads_path` and graceful sticker/animation skipping (#801).
* **[Web]:** QR code on Telegram setup step in workstation onboarding — generates client-side QR via `qrcode` package, updates dynamically as verification code is entered (#800).

## 🐛 Fixes
* **[Broker]:** Recover broker ID from DB on restart and re-assign orphaned agents — prevents UUID regeneration from orphaning agents when settings are lost (#805).
* **[Broker]:** Resolve attachment paths to host-side paths for host-process brokers — fixes container-internal `/scion-volumes/` paths being passed to Telegram/Discord plugins where they don't exist (#803).
* **[Broker]:** Auto-populate `message_broker.types` from plugin list, add gateway health reporting for Discord, and validate spoke wiring on restart (#804).
* **[Hub]:** Surface `dispatch_failure_reason` in CLI/API responses for failed messages, add participant-privacy integration tests, and fix stale doc-comment (#802).
* **[Telegram]:** Propagate 5xx delivery errors back to hub and CLI caller — previously swallowed alongside 429 rate-limits, causing silent delivery failures (#798).
* **[Discord/Telegram]:** Use `RecipientID` fallback in recipient resolution to fix outbound attachment routing (#799).
* **[Harness]:** Antigravity reads `SCION_THINKING_LEVEL` instead of `AGY_THINKING_LEVEL` with 4-tier thinking level support and backward-compat fallback (#793).
* **[Harness]:** Migrate default OOB harness from gemini to antigravity (#797).
* **[Build]:** Guard empty array expansions under `set -u` for bash < 4.4 compatibility across three image-build scripts (#795).
* **[WebSocket]:** Raise `MaxMessageSize` from 64KB to 1MB and add pre-send size guard — fixes failures with large `RemoteCreateAgentRequest` payloads carrying inline config and base64-encoded bodies (#796).
