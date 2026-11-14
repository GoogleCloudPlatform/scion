# Docs: global catalog authoring permissions, CI token recipe, core scope retirement (ptone/scion#1713 Phase 2, slice 5)

**Date:** 2026-11-14
**Branch:** scion/global-authz-docs (stacked on scion/global-authz-core, phase 3, which is on scion/global-authz-templates, phase 2)
**Issue:** ptone/scion#1713

## What changed

Docs only (`docs-site/src/content/docs/`):

- `hosted/ha/permissions.md`: `global-catalog-author` row (six `*_global` IDs), `hub-admin` revision text (r11 skill twins, r12 template twins, no plain `template.*`), new "Global catalog authoring" section (permission table, twin model, ownership grants nothing, custom roles, token scopes, hub-wide injected list caution), delegated `scope=core` text fixed (400 on the skill list, for every caller).
- `hosted/user/personal-access-tokens.md`: new "Hub-boundary tokens for global catalog authoring" (CI recipe, scope table, CLI usage, import/reimport overwrite with `template:create_global`, re-mint note); "Global catalog and skill registries" bullet updated (global import needs `template:create_global`, not `template:create`).
- `local/skills.md`: global URI alias row, `core` retirement note, `--scope` publish lookup, global permissions note, injected-list caution.
- `local/templates.md`: "Global template authoring permissions", refresh permission, `--scope global --hub` example.
- `hosted/single-node/skill-registry.md`: scope list, draft visibility for global skills, global catalog authoring paragraph.
- `reference/cli.md`, `reference/api.md`, `reference/agent-delegation.md`: `core` removed; `templates update` added to the CLI reference.
- `release-notes.md`: new top section, following the convention of recent feature PRs (for example GoogleCloudPlatform/scion#4713 and #4698 added their sections there). `changelog/` is a daily generated summary and `.design/backlog/RELEASENOTES.md` is rarely used.

## Findings (code differs from design or brief)

- Role revisions on the base are `global-catalog-author` r3, `hub-admin` r12, `super-admin` r12 (the design said r2/r11).
- `scion --global templates clone` copies a Hub template to the local global templates directory (needs only `template:read`); it does not call the Hub clone endpoint. The docs say so; the Hub clone into global (web UI, `POST /api/v1/templates/{id}/clone`) needs `template:read` + `template:create_global`.
- `scion hub token create` is hidden in agent mode; `--help` was checked with an isolated environment (`env -i HOME=...`).
- Without a terminal, `templates delete` answers No to its confirmation unless `--yes` / `--non-interactive` is passed; the CI recipe uses `--non-interactive`.

## Verification

- Every permission ID, selector, role and revision: `pkg/hub/permissions/registry.go`, `project_applicability.go`, `pkg/hub/seed.go`.
- Write sites: `catalogWriteAction` call sites in `pkg/hub` (skills, templates, reimport, import, discover, version visibility).
- CLI flags: `go build -buildvcs=false ./cmd/scion`, then `--help` for `hub token create`, `skills publish`, `templates sync|push|delete|clone|update|import`, global flags.
- `core` retirement: `pkg/api/skill_uri.go`, `pkg/hub/skill_handlers.go`, `cmd/skills.go`, `pkg/ent/entc/migrate_skill_core_scope.go`; no change under `pkg/version` or `pkg/ent/schema`.
- `cd docs-site && npm ci && npm run build` with d2 v0.9.0: build complete, "All internal links are valid."

Not verified in code: phase 4 behaviour (not on the base), a CLI journey against a live Hub with a non-admin token, and whether the Hub injected list accepts an exact-version or content-hash pin.
