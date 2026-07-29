# Release Notes (2026-07-28)

Hub-scoped pre-start hooks shipped with web UI and CLI, the GitHub skill resolution cache was promoted to a broker-level singleton with extended TTLs, `scion resume --force` was added to recover crashed agents from the error phase, and the hub gained a concurrency guard against concurrent maintenance operations after a production outage.

## 🚀 Features
* **[Hub]:** Hub-scoped pre-start hooks — extends `ProjectPreStartHook` entity with `scope` enum (project/hub), adds web UI components (Pre-Start Hooks tab in Hub Resources and Project Settings), CLI via `scion hub hook` subcommand, and project-overrides-hub resolution (#888).
* **[Agent]:** Broker-singleton GitHub skill resolution cache — converts from per-request ephemeral to broker-level singleton, increases default TTL from 5m to 30m, adds 24h TTL for SHA resolution (Phase 1 of cache durability) (#885).
* **[CLI]:** `scion resume --force` to recover crashed agents from the error phase — permits in-place restart with harness resume flag so interrupted sessions continue rather than starting fresh (#895).

## 🐛 Fixes
* **[Hub]:** Concurrency guard for maintenance operations — reject concurrent runs with 409 Conflict, preventing pile-up of `go build` invocations that caused the July 28 hub outage. Also fixes resume error text to recommend `scion resume` over `scion start` (#894).
* **[Agent]:** Use resolved `agentHome` from `GetAgent` in provision hook staging — raw `opts.ProjectPath` broke non-git project paths (#897).
* **[Hub]:** Fix fixturegen schema (remove non-existent `type` column, add 5 missing table fixtures) and align `detectHarnessType` gemini inference with `gemini-cli` rename (#893).
* **[CI]:** Three rounds of pre-start-hook CI fixes — missing mock methods in templatecache tests (#891), 4 test failures from path/harness/store assumptions (#892), and gofmt formatting (#898).

## 📖 Docs
* **[Skills]:** Troubleshooting enhancements — operator actions, observables, and diagnostics for signing key, `no_runtime_broker`, and duplicate sciontool symptoms with code-verified corrections (#890).
