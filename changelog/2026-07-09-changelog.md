# Release Notes (2026-07-09)

Thinking-level controls arrived across the stack: a 0-100 slider in the web UI, per-harness bucket mapping, and schema support. Antigravity was bumped and gained model range aliases, integration config architecture got several critical fixes, and the docs received Skill Bank and harness lifecycle coverage.

## 🚀 Features
* **[Agent]:** Thinking level field + model choice UX refactor — added a 0-100 thinking level slider (checkbox-toggled, unset = harness default) to agent edit and project defaults. Model choice removed from agent creation form (now agent-level setting). `ThinkingLevel *int` wired through create, update, and project defaults apply paths with server-side 0-100 validation (#662).
* **[Antigravity]:** Model range with thinking-level bucket mapping — `small`/`medium` → Flash, `large`/`extra-large` → Pro. Provisioner maps `AGY_THINKING_LEVEL` (0-100) to named buckets: Flash low/medium/high, Pro low/high (#660).
* **[Config]:** Added `thinking_budget` fields (`ThinkingBudgetMap`, `ThinkingBudgetFlag`, `ThinkingBudgetConfigKey`) to harness config JSON schema — previously rejected by `additionalProperties: false` (#661).

## 🐛 Fixes
* **[Chat Admin]:** Integration config architecture fixes (R0-R3) — `config_file` now propagated into plugin manager config map at all three entry points, HA config pushes gated to prevent race with DB-backed reload, GET endpoint reads from correct provider (#663).
* **[Hub]:** Drop GCS-absent files from manifest in `syncResourceFromStorage` — missing files were logged but still included, causing broker 404s during dispatch (#656).
* **[Server]:** Honor explicit `--dev-auth=false` in workstation daemon mode — the daemon arg builder was dropping negated flags, causing the child process to re-enable dev-auth via `applyWorkstationDefaults` (#652).
* **[Web]:** Correct `AgentWithConfig` interface extension using `Omit<Agent, 'appliedConfig'>` to avoid TS2430 (#664).
* **[Web]:** Removed unused `resolvedImage` and `isRemoteImage` from harness-config-detail (#654).
* **[CI]:** `gofmt` fix for `pkg/hub/imagecheck/checker.go` (#658).

## 🔧 Chores
* **[Antigravity]:** Bumped to version 1.1.0 (#659).

## 📖 Docs
* **[Docs]:** Added Skill Bank (authoring, publishing, registry, federation) and harness lifecycle documentation — covers Copilot, Hermes, Antigravity harnesses, container-script provisioning model, and glossary entries (#653).
