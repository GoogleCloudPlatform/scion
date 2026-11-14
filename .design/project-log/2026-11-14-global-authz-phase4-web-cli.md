# Web + CLI: global catalog capability gating, scoped skill publish, hub-token templates update (ptone/scion#1713 Phase 2, slice 4)

**Date:** 2026-11-14
**Branch:** scion/global-authz-webcli (stacked on scion/global-authz-core, fork PR ptone/scion#7824, on scion/global-authz-templates, fork PR ptone/scion#7815)
**Issue:** ptone/scion#1713

## Behaviour

- `web/src/components/pages/settings.ts`: the Hub Resources Templates tab reads `_capabilities` from `GET /api/v1/templates?scope=global&limit=1`. `create` gates the import form and row Clone, failing closed. `scion-resource-list` gains `recordCapabilities`, which offers Delete per row only when the record's `_capabilities` include `delete`.
- `cmd/skills.go`: `findPublishTargetSkill` sends `--scope` (and the linked project as `scopeId` for project scope) to the skill list and checks each returned record's scope and project (Q6.3). Without `--scope`, the lookup is name-only as before. A `--scope global` publish connects with SkipProjectLink.
- `cmd/templates_update.go`: root `--global` maps to `--scope global`, and a conflicting `--scope` is a usage error. A global-scope run connects with SkipProjectLink and skips `GetProjectID`.
- `cmd/project_gate.go`: `isGlobalTemplatesUpdate` exempts `templates update --scope global` from the project requirement.

## Findings

- With a hub token lacking `project:read`, `skills publish --scope global` inside a linked project failed on `GET /api/v1/projects/{id}` (404) before reaching the skill API. That is the same cause as G1 for `templates update`, and both now connect with SkipProjectLink for the global scope only.
- The brief's item 5 (the `template.reimport` wording) was a no-op: `template.user.reimport` exists (phase 2, Variant `scope=user`, `template.update`), so the description was accurate.
- The hub skill list endpoint already filtered on `scope` and `scopeId`, so no hub change was needed for the scoped lookup.
- Test harness note: driving `rootCmd.Execute()` from cmd tests needs the subcommand's flags and the root `--global`/`--hub`/`--yes`/`--non-interactive` flags reset (value and `Changed`) between runs. The helpers in `cmd/templates_update_global_test.go` and `cmd/skills_publish_global_test.go` do this.

## Limitations / follow-ups

- `skills publish --scope global` outside a project still needs `--global` because of the project gate.
- A `--scope user` publish lookup has no user ID filter. Admins who can list other users' user skills could match one of theirs.
- Templates tab "Refresh All from Source" and Rename, and the global Harness Configs tab (child issue C2), are not capability-gated.

## Verification

All run in the agent container with `SCION_*` env stripped:
- `make fmt-check`, `make lint-ci`, `make check-custom` (includes `check-authorization-catalog` and `check-route-authz-manifest`: 244 routes, 244 manifest entries, no violations), `make build`, `make test-fast-ci`: PASS.
- `go test -buildvcs=false ./cmd/...`: PASS.
- `GOGC=40 golangci-lint run --new-from-rev=f825eb4 --concurrency=1 ./cmd/...`: 0 issues.
- `GOOS=windows go build -buildvcs=false ./cmd/scion`: PASS. `hack/winsec-guard.sh`: ok (3143 files).
- Web, after `rm -rf build web/public/shoelace`: `npm run typecheck` PASS; `npm run lint` 0 errors (warnings only, all pre-existing); `npx prettier --check src` PASS; `npm test` 530 files, 12901 passed, 5 skipped.
- Hub shards: not run. No pkg/hub code or tests changed (and item 5 turned out to need no authzop change).
