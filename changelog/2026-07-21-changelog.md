# Release Notes (2026-07-21)

A focused correctness day: a shared-reference mutation bug in `buildCreateRequest` was fixed to prevent env map corruption across dispatch calls, plugin installation now properly wires into message broker routing, and the Discord agent cache TTL was reduced to eliminate stale `/default` listings.

## 🐛 Fixes
* **[Broker]:** Clone `ResolvedEnv` map before mutation in `buildCreateRequest` — the shared reference allowed secret injection, storage env merge, and GitHub token writes to silently modify the agent's canonical config. Also injects `SCION_MODEL` from `AppliedConfig.Model` in all three dispatch functions (#832).
* **[Hub]:** Add plugin to `message_broker.types` on web UI install — `handleInstallIntegration()` loaded the plugin but never added it to the types list, excluding it from message routing (#833).
* **[Discord]:** Reduce agent cache TTL from 5 minutes to 30 seconds — new agents were invisible in `/default` and mention resolution for up to 5 minutes (#834).

## 🔧 Chores
* **[Deps]:** Bump astro from 7.0.9 to 7.1.3 in docs-site (#831).
