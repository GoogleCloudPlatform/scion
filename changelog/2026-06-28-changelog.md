# Release Notes (2026-06-28)

A large batch of platform improvements landed: Apple Container gained build support and DNS connectivity, a new Hermes Agent harness shipped, Cloud Build became an alternative image builder, and the auth pipeline was decoupled from harness-specific Go code. Chat integrations received several reliability fixes.

## 🚀 Features
* **[Runtime]:** Apple Container added as a build-capable runtime on macOS — `DetectContainerRuntime()` now discovers the `container` binary (Apple Virtualization framework), and tag/push commands use the cross-runtime `image` subcommand form (#509).
* **[Runtime]:** Apple Container DNS support for Hub connectivity — when using the `container` runtime, `EnsureAppleDNS` creates a DNS rule so agent containers can resolve the Hub API endpoint. Onboarding UI triggers setup automatically when Apple Container is selected (#528).
* **[Harness]:** Hermes Agent harness bundle — complete scaffold with API key auth (Anthropic > OpenAI > Google AI Studio precedence), instruction projection into `AGENTS.md`, MCP server config, model alias resolution, capture-auth for no-auth flow, and 16 provision tests (#519).
* **[Build]:** Google Cloud Build support for harness-config image builds — new `CloudBuildHarnessConfigExecutor` uploads build context to GCS, submits multi-arch builds (`linux/amd64,linux/arm64`), and streams logs. CLI gains `--builder cloud-build` flag. System status API exposes Cloud Build availability for the frontend (#521).
* **[Auth]:** Decoupled auth pipeline from harness-specific Go code (Phases 1-2) — harness-config hydration now runs during the env-gather pre-check so config-driven auth metadata is available before the broker requests env vars. Generic `EnvVars` map on `AuthConfig` replaces hardcoded paths; the Copilot harness's GitHub tokens now flow through config alone (#516).
* **[Agent]:** Retry with exponential backoff and two-layer TTL cache for the GitHub skill resolver (#525).
* **[Web]:** Moved Capture Auth button to the terminal page for easier access (#520).

## 🐛 Fixes
* **[Hub]:** Admin role re-evaluated on every login and token refresh — previously only set at user creation, so config changes to admin emails had no effect on existing users (#530).
* **[Discord]:** Require thread ID for forum channels to prevent broadcast — messages to forum-type channels without a thread ID are now rejected with an actionable error instead of broadcasting to all threads (#522).
* **[Telegram]:** Restore backtick code spans stripped by Telegram's entity parser — the broker now reconstructs inline code and code blocks from `code`/`pre` entities, and `stripMentions` preserves whitespace and indentation (#518).
* **[Chat]:** Propagate hub errors (403, 404, 500) back to chat channels with user-facing messages instead of swallowing them silently. Discord broker pre-validates target agents against the cached agent list to catch deleted-agent routing immediately (#517).
* **[Runtime]:** Replace bind-mount secret staging with env-var pipeline for stateless brokers, fixing credential delivery when the filesystem is read-only (#523).
* **[Message]:** Enforce 2000-character limit on messages with an actionable error for oversized payloads (#524).
* **[Web]:** Detect incomplete embedded assets and serve a helpful error page instead of a blank screen (#526).

## 🔧 Chores
* **[CI]:** Added `handlers_projects_core.go` and `handlers_runtime_brokers.go` to the compat-literals allowlist for legitimate legacy grove literals (#527).
