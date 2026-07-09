# Release Notes (2026-07-08)

A feature-rich day: model UX gained alias tiers and a CLI `--model` flag, a two-tier settings architecture landed for HA deployments, image resolution was reworked with three-entity visibility, and the hub learned self-healing hash-mismatch repair for multi-node GCS sync.

## 🚀 Features
* **[Model UX]:** Alias tiers, CLI flag, project defaults, and web UI dropdowns — added `extra-large`/`xl` model alias tier, `--model` flag on `scion start`, `DefaultModel` in `ProjectSettings` for project-level defaults, and replaced free-text model inputs with dropdown selectors in the web UI (#639).
* **[Hub]:** Two-tier settings architecture for HA multi-node deployments — new `HubSetting` Ent schema with CAS upsert semantics (`SELECT FOR UPDATE` on Postgres, single-writer on SQLite), `HubSettingStore` interface, and `pkg/config/opsettings` section registry for Layer-1 operational settings (#640).
* **[Harness]:** Two-phase image resolution with three-entity visibility — prefer local short-form images over registry, `CheckAll()` returns status for local/registry/config entities, enhanced image panel in web UI with delete-local and pull-image endpoints (#644).
* **[Hub]:** Self-healing DB-GCS sync for harness-config and template hash mismatches — dispatch intercept detects stale DB manifests, syncs from GCS, and retries once. Startup reconciliation runs `SyncAllHarnessConfigsFromStorage` on boot. Generalized to cover both templates and harness-configs (#643).
* **[Onboarding]:** Image pull progress (X of N) in onboarding wizard with progress bar and per-image status list (#645).

## 🐛 Fixes
* **[Agent]:** Honor explicit `--workspace` over git auto-detection — prevents mount widening to the whole repository when workspace sits inside a git repo. Also fixed on resume/restart where `opts.Workspace` was empty (#642).
* **[Store]:** Honor cursor in `ProjectStore.ListProjects` — added keyset pagination ordered by `(created DESC, id DESC)`, matching the pattern used by other stores (#641).
