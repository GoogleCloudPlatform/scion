# Release Notes (2026-06-10)

A sweeping day of project compatibility refactoring, security hardening, and a major new Discord integration. The codebase-wide rename from "grove" to "project" landed across endpoints, config, runtime labels, and CI guardrails. The test-login endpoint was secured with challenge tokens, harness config hydration was fixed end-to-end, and a standalone Discord bot with gRPC support shipped.

## 🚀 Features
* **[Discord]:** Standalone Discord bot with gRPC for HA deployment — the Discord plugin can now run as an independent process communicating with the hub via gRPC, enabling horizontal scaling and high-availability setups. Includes a new `DiscordPendingLink` entity, gRPC broker adapter, and a dedicated Dockerfile (#395).
* **[Scheduler]:** Enabled scheduler write commands (`create-recurring`, `pause`, `resume`, `delete`) in agent mode, removing the `dispatch_` prefix gate that previously blocked agents from managing schedules (#378).
* **[Auth]:** Added challenge token authentication to the test-login endpoint — callers must present a short-lived JWT (5min TTL) signed with the hub's user signing key and scoped to the `scion-test-login` audience. Includes case-insensitive Bearer parsing and explicit `exp` claim enforcement (#382, #392).
* **[Compatibility]:** Project/grove compatibility boundary — added a comprehensive migration layer with legacy `/groves` route wrappers, request body field adapters, and centralized compatibility key mapping to support clients using the old naming while the codebase transitions (#380, #387, #388).

## 🐛 Fixes
* **[Hub]:** Fixed harness config hydration pipeline — `populateAgentConfig()` now stamps `HarnessConfigID`/`Hash` on agent records, `RemoteAgentConfig` threads these fields to the broker, and database errors during slug lookup are now logged instead of silently discarded (#389).
* **[UI]:** Fixed file badge size inconsistency (computed from filtered list instead of unfiltered total) and added missing settings page titles for harness-configs and templates detail routes (#390).
* **[UI]:** Applied agent list UI improvements to the project detail view (#383).
* **[Runtime]:** Fixed recursive glob in sparse checkout to include `home/` subdirectory by switching from `/*` to `/**` pattern (#381).
* **[Compatibility]:** Used canonical project endpoints across integrations, test fixtures, and mocks — replacing remaining `/groves` references (#384, #385).
* **[Compatibility]:** Centralized runtime project label compatibility, replacing scattered literal strings (#386).
* **[CI]:** Fixed compat-literals CI step failing due to missing ripgrep — installs `rg` in the runner and degrades gracefully when unavailable (#397).

## 🔧 Chores
* **[CI]:** Enforced project compatibility literal guardrail in GitHub Actions with a documented contributor rule (#391, #393).
* **[Docs]:** Changelog backfill for June 8-9 entries merged to main (#396).
