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
- `release-notes.md`: new top section, following the convention of recent feature PRs (for example GoogleCloudPlatform/scion#4713 and GoogleCloudPlatform/scion#4698 added their sections there). `changelog/` is a daily generated summary and `.design/backlog/RELEASENOTES.md` is rarely used.

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

- Injected-list pin advice: `PUT /api/v1/hub/settings/injected-skills` stores `user_defined` refs unvalidated (`pkg/hub/handlers_skills_injection.go:792`); `mergeInjectedSkills` copies them into `InlineConfig.Skills` (`pkg/hub/handlers_agent_create_helpers.go:1026`); dispatch pre-resolution parses the URI (`pkg/hub/skill_dispatch_resolve.go:171`, `:189`) and resolves `uri.Version` (`pkg/hub/skill_handlers.go:1859`) through `resolveSkillVersion` (`pkg/hub/skill_content_hash.go:83`) and the store's exact semver / `sha256:` lookup (`pkg/store/entadapter/skill_store.go:517`). Published versions are immutable (`publishSkillVersion` answers 409). Deleting archives the skill (`skill_store.go:201`), and `GetSkillBySlug` matches only active skills, so a pinned injection then no longer resolves.
- Stored `core` URIs on reprovision: start and restart send the stored config plus creator pre-resolution (`pkg/hub/httpdispatcher.go:4001`, `:4298`). `isHubRegistrySkillURI` drops the unparseable `core` URI from pre-resolution (`skill_dispatch_resolve.go:171-184`). The broker's `PreResolvedSkillResolver` forwards the rest (`pkg/agent/pre_resolved_skill_resolver.go:112`) to the routing resolver, which sends `skill://` to the Hub resolver (`pkg/agent/routing_skill_resolver.go:181`, `POST /api/v1/skills/resolve`). There, `ParseSkillURI` fails with `invalid_uri` and the retired-scope message (`pkg/hub/skill_handlers.go:1781-1785`). Provisioning fails for a required reference and skips an optional one with a warning (`pkg/agent/provision.go:5396-5403`). The release note was narrowed to this; reincarnation was not traced.

Not verified in code: phase 4 behaviour (not on the base) and a CLI journey against a live Hub with a non-admin token.
