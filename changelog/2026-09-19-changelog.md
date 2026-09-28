# Release Notes (2026-09-19)

grok-build Vertex AI compatibility hardened (three fixes), chat DM unread/Seen receipts overhauled, and harness-config hash threaded through the full start path.

## 🐛 Fixes
* **grok-build Vertex AI compatibility (#1778, #1776, #1774):** grok CLI v1.0.34 auto-selects Responses API for grok-4.6, but Vertex AI returns usage data without `input_tokens`. Provisioner now sets `api_backend = "chat_completions"` and `supports_backend_search = false` in Vertex AI TOML blocks (#1778). Separately, `--disallowed-tools x_search` removes the unsupported hosted tool from API requests (#1776). Added `--trust` flag to suppress the directory trust prompt that blocks headless operation (#1774).
* **Chat DM unread dots and Seen receipt expiry (#1768):** Derives unread state from visible native history, ignoring external/missing/promoted/mention-only messages. Discards stale sidebar responses, matches server timestamp/UUID ordering with sub-millisecond precision, expires Seen at the exact five-minute boundary. Adds ordered lookup indexes and skips unused counts for efficient DM refreshes.
* **Harness-config hash threading through start path (#1777):** `HarnessConfigID` and `HarnessConfigHash` from `agent.AppliedConfig` now flow through the full `StartAgent` call chain (hub → broker) so `hydrateHarnessConfig` takes the hash-verified resolution path instead of falling back to potentially-stale on-disk search.
* **Attachments metadata cleanup (#1775):** Internal `metadata["attachments"]` transport key stripped after consumption at all 3 call sites (broker, agent direct-persist, chat v2) with `LogAttrs()` guard to prevent downstream leakage.
* **Terminal icon for new agents via SSE (#1773):** Terminal icon in members sidebar didn't appear for newly started agents until page reload because SSE `AgentCreatedEvent` doesn't carry `canAttach`. Now schedules a debounced 1s re-fetch of the members REST endpoint.
* **Mention autocomplete dedup (#1771):** Agent slugs now normalized (lowercase + space-to-hyphen) when building the dedup Set, preventing duplicate entries when slug differs only in case or whitespace.

## 📖 Docs
* **Cross-project messaging design docs (#1779):** Four design documents (1,555 lines) added to `.design/hosted/` — full design spec, current behavior analysis, 7-phase delivery plan, and confirmed decisions with mode-ceiling record.
* **Nightly doc update (#1770):** Cross-project messaging policies and denial codes, conversation get-message, version --check, A2A auth/durable state/gRPC docs, codex quartile mapping, glossary updates.
