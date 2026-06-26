# Release Notes (2026-06-25)

A major push on harness development — the Codex harness gained notification hooks, dialect YAML configuration, OTEL telemetry support, and template instructions. Antigravity was pinned to a specific release, briefly switched to ADC auth, then reverted. The Hub handlers were split by resource for maintainability.

## 🚀 Features
* **[Codex Harness]:** Notification hooks enabled — harness now fires lifecycle events and OTEL-escaped telemetry configuration for agent observability (#482, #488).
* **[Codex Harness]:** Dialect YAML configuration — maps model aliases and API conventions for the Codex harness (#484, #486).
* **[Codex Harness]:** Project template instructions — added instruction projection with hardened output and dropped unused system prompt file (#483).
* **[Codex Harness]:** Extended OTEL config support for richer telemetry integration (#494).
* **[Codex Harness]:** Updated model aliases (#481).
* **[Antigravity]:** Pinned CLI binary to v1.0.11 from GitHub Releases for build reproducibility, with `TARGETARCH` mapping for multi-platform support (#487).
* **[Sciontool]:** Hook support for bundled dialect overrides, allowing harness-specific model mapping to be shipped with the harness config (#485, #489).

## 🐛 Fixes
* **[Antigravity]:** Added missing field extractions (`session_id` from `.conversationId`, `tool_input` from `.toolCall.args`) and removed false `tool_name` extraction from `PostToolUse`. Declared `max_model_calls` as supported capability (#490).
* **[Antigravity]:** Disabled ADC auth for vertex-ai (USE_ADC not yet functional in AGY CLI) and reverted to requiring `AGY_TOKEN` with keyring injection. GCP location fallback and v1.0.11 pin preserved (#497).
* **[Codex Harness]:** Write instructions under container home directory (#491).
* **[Codex Harness]:** Make notify hook executable (#492).

## 🔧 Chores
* **[Hub]:** Split monolithic handlers.go by resource type for maintainability (#480).
* **[Docs]:** Changelog updates merged (#changelog).
