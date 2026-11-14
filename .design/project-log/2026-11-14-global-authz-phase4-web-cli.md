# Web + CLI: global catalog capability gating, scoped skill publish, hub-token templates update (ptone/scion#1713 Phase 2, slice 4)

**Date:** 2026-11-14
**Branch:** scion/global-authz-webcli (stacked on scion/global-authz-core, fork PR ptone/scion#7824, on scion/global-authz-templates, fork PR ptone/scion#7815)
**Issue:** ptone/scion#1713

## Behaviour

- `web/src/components/pages/settings.ts`: the Hub Resources Templates tab reads `_capabilities` from `GET /api/v1/templates?scope=global&limit=1`. `create` gates the import form, row Clone and "Refresh All from Source" (`hideRefresh`), failing closed; a global reimport is authorized as `template.create_global`. `scion-resource-list` gains `recordCapabilities`, which offers Delete per row only when the record's `_capabilities` include `delete`.
- `cmd/skills.go`: `findPublishTargetSkill` sends `--scope` to the skill list, narrowed by `scopeId`: the linked project for project scope, or the caller's user ID from `GET /api/v1/auth/me` for user scope. It also checks each returned record's scope and scope ID (Q6.3). If the caller can't be identified, a user-scope publish stops. With `--scope`, a failed lookup is returned rather than creating a skill. Without `--scope`, the lookup is name-only as before. A `--scope global` publish connects with SkipProjectLink.
- `cmd/templates_update.go`: root `--global` maps to `--scope global`, and a conflicting `--scope` is a usage error. A global-scope run connects with SkipProjectLink and skips `GetProjectID`.
- `cmd/project_gate.go`: `isGlobalTemplatesUpdate` exempts `templates update --scope global` from the project requirement.
- `cmd/templates.go`, `cmd/template_helpers.go`: `templates delete` and `templates clone` connect with `requireHubForUpload` (SkipProjectLink under `--global`), and `findHubTemplates` skips `GetProjectID` for a global-only search. Before this, `--global` delete and clone in a fresh HOME generated a `project_id` for `~/.scion` and failed on `GET /api/v1/projects/<id>` (404) under a hub token holding only the template selectors.

## Findings

- `templates delete` and `templates clone` define their own boolean `--hub` ("only search Hub"), which shadows the root `--hub <url>`. With those commands, select the hub with `scion hub add` / `scion use`. `SCION_HUB_ENDPOINT` marks an agent hub context, which skips project checks entirely.
- With a hub token lacking `project:read`, `skills publish --scope global` inside a linked project failed on `GET /api/v1/projects/{id}` (404) before reaching the skill API. That is the same cause as G1 for `templates update`, and both now connect with SkipProjectLink for the global scope only.
- The brief's item 5 (the `template.reimport` wording) was a no-op: `template.user.reimport` exists (phase 2, Variant `scope=user`, `template.update`), so the description was accurate.
- The hub skill list endpoint already filtered on `scope` and `scopeId`, so no hub change was needed for the scoped lookup.
- Test harness note: driving `rootCmd.Execute()` from cmd tests needs the subcommand's flags and the root `--global`/`--hub`/`--yes`/`--non-interactive` flags reset (value and `Changed`) between runs. The helpers in `cmd/templates_update_global_test.go` and `cmd/skills_publish_global_test.go` do this.

## Limitations / follow-ups

- `skills publish --scope global` outside a project still needs root `--global` because of the project gate. Phase 5 documents `--global`.
- Per-row Rename on the Templates tab, the template detail page's "Refresh from Source" button, and the global Harness Configs tab (child issue C2) are not capability-gated.
- Still connecting with a project link (not part of the §10 journeys): `templates list`/`show`/`pull`/`status`, and `skills list`/`show`/`delete`/`deprecate`/`versions`/`resolve`. `--global templates clone` still searches project templates for the source, so inside a git checkout it may make a `GET /api/v1/projects?gitRemote=` list call (its error is ignored).

## Verification

All gates ran in the agent container with `SCION_*` env stripped.
- Initial head (on phase 3 f825eb4): `make fmt-check`, `make lint-ci`, `make check-custom` (includes `check-authorization-catalog` and `check-route-authz-manifest`), `make build`, `make test-fast-ci`, `go test ./cmd/...`, `GOOS=windows go build ./cmd/scion` and `hack/winsec-guard.sh` all PASS. Web typecheck, lint (0 errors), prettier and `npm test` (530 files, 12901 passed) all PASS.
- After rebasing onto phase 3 2a99cf7: the cmd build, the new cmd tests and both authz catalog checks PASS.
- Each review-fix commit: `make fmt-check`, `GOGC=40 golangci-lint run --new-from-rev=2a99cf7 --concurrency=1 ./cmd/...` (0 issues), `GOOS=windows go build ./cmd/scion`, `hack/winsec-guard.sh` and `go test ./cmd/...` PASS. Where web changed: typecheck, lint, prettier and the affected vitest files PASS.
- Mutation checks: each new test fails against the unfixed code. This includes the `findHubTemplates` GlobalOnly guard mutated to `if true {` and the scoped lookup-error return disabled.
- Hub shards: not run, because no pkg/hub code or tests changed.
