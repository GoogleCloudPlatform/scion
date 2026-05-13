# Telegram Bot Message Broker Plugin (Issue #43)

**Date:** 2026-05-13
**Branch:** scion/fix-issue-43
**Agent:** fix-issue-43

## What was done

Implemented a complete Telegram bot message broker plugin following the existing `refbroker` pattern.

### Files created

- `pkg/plugin/telegram/api.go` — Telegram Bot API HTTP client (getMe, getUpdates, sendMessage)
- `pkg/plugin/telegram/api_test.go` — API client tests with httptest mocks
- `pkg/plugin/telegram/format.go` — StructuredMessage to Telegram text formatting
- `pkg/plugin/telegram/format_test.go` — Formatting unit tests
- `pkg/plugin/telegram/telegram.go` — Core TelegramBroker implementing MessageBrokerPluginInterface
- `pkg/plugin/telegram/telegram_test.go` — Comprehensive unit tests
- `pkg/plugin/telegram/plugin_integration_test.go` — RPC integration tests
- `cmd/scion-plugin-telegram/main.go` — Plugin binary entry point

### Design decisions

1. **Plain text formatting** — Used plain text instead of MarkdownV2 for Telegram messages. Agent output frequently contains special characters that break MarkdownV2 escaping.

2. **No external dependencies** — Used `net/http` directly for Telegram Bot API calls. No third-party Telegram SDK needed.

3. **Chat-to-topic routing** — Three-tier routing: (1) direct via `telegram_chat_id` metadata, (2) configured `chat_routes` JSON map, (3) default topic `scion.telegram.chat.<chatID>.messages`. Reply routing works via metadata passthrough.

4. **Non-blocking test infrastructure** — Avoided blocking channel-based update delivery in tests (caused httptest.Server hangs). Used setUpdates() with non-blocking queue pattern instead.

### Test coverage

52 tests total:
- 8 API client tests
- 12 format tests
- 25 unit tests (configure, publish, subscribe, echo filtering, inbound delivery, hub API delivery, etc.)
- 7 RPC integration tests (full lifecycle over net/rpc transport)

### Observations

- The `make ci` format check shows many pre-existing formatting issues across the codebase (not introduced by this change).
- Pre-existing test failure in `pkg/runtime/k8s_shared_dirs_test.go` (label rename issue).
