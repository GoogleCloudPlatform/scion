# Hub: global template create/update/delete twin permissions (ptone/scion#1713 Phase 2, slice 2)

**Date:** 2026-11-13
**Branch:** scion/global-authz-templates (stacked on scion/global-authz, fork PR ptone/scion#7786)
**Issue:** ptone/scion#1713

## Behaviour

- Added `template.create_global` (`template:create_global`), `template.update_global` (`template:update_global`), and `template.delete_global` (`template:delete_global`) to `pkg/hub/permissions/registry.go` (`ExcludeFromManageAlias: true`, `CapabilityKind: CapabilityNone`), with `ProjectTargetApplicability = false`, `PermissionAllowedBoundaries = {BoundaryKindHub}`, `SupportedTargetClasses = {HubResource}` (`create_global`) / `{GlobalCatalog}` (`update_global`, `delete_global`), and `collectionTargetClasses` entries (`{TargetClassKindHubResource}` on `create_global`, `{}` on `update_global` and `delete_global`).
- Extended `catalogWriteAction` in `pkg/hub/authz_catalog_scope.go` with the `permissions.ResourceTemplate` arm mapping `store.TemplateScopeGlobal` `create` / `update` / `delete` to `ActionCreateGlobal` / `ActionUpdateGlobal` / `ActionDeleteGlobal` while leaving project and user template scopes on plain `ActionCreate` / `ActionUpdate` / `ActionDelete`.
- Wired all global template write and capability paths to the new twins:
  - `createTemplateV2` (`pkg/hub/template_handlers.go`): global branch uses `ActionCreateGlobal` with `hubCollectionEvidence("template.create_global")`, stamps `CreatedBy` with the caller's user ID, and leaves `OwnerID` empty (audit-only provenance, no ownership grant).
  - `updateTemplateV2`, `patchTemplateV2`, `handleTemplateUpload`, `handleTemplateFinalize` (`pkg/hub/template_handlers.go`), and `handleTemplateFiles` (`pkg/hub/template_file_handlers.go`): authorize `templateResource(existing)` with `catalogWriteAction(permissions.ResourceTemplate, existing.Scope, ActionUpdate)`.
  - `deleteTemplateV2` (`pkg/hub/template_handlers.go`): authorizes `templateResource(existing)` (fixing the prior record-ID/record-field asymmetry where `templateScopeResource(existing.Scope, existing.ScopeID)` omitted `existing.ID` and `existing.OwnerID`) with `catalogWriteAction(permissions.ResourceTemplate, existing.Scope, ActionDelete)`.
  - `handleTemplateClone` (`pkg/hub/template_handlers.go`), `handleTemplateReimport` (`pkg/hub/template_reimport.go`), `globalImportDecision` (`pkg/hub/handlers_resource_import.go`), and `templatePersistence.Create` (`pkg/hub/resource_store.go`): global destinations require `template.create_global` with `hubCollectionEvidence("template.create_global")` and stamp `CreatedBy` without setting `OwnerID` (reimport preserves the existing `CreatedBy`).
  - `listTemplatesV2` (`pkg/hub/template_handlers.go`): added `templateListCapabilities` / `canCreateTemplateInScope` emitting top-level `_capabilities.actions: ["create"]` when the caller may create in the requested scope (`template.create_global` for `scope=global`), matching the `skillListCapabilities` / `skill-create.ts` `canCreateGlobal` precedent.
  - `handleAuthzExplain` (`pkg/hub/audit_authz.go`): loads the stored template record (`GetTemplate` -> `templateResource(tmpl)`) when `resource_type == "template"` and `resource_id != ""` so `POST /api/v1/authz/explain` canonicalizes `update`/`delete` on global templates to `template.update_global`/`template.delete_global`.
- Kept `owner/user/template` in `pkg/hub/permissions/relationship_policy.go` unchanged (plain `template.*` only; no `*_global` twin appears on any relationship row).
- Bumped seeded role revisions in `pkg/hub/seed.go`:
  - `global-catalog-author` `2 -> 3` with description `"Creates and maintains global catalog skills and templates"` and exact sorted 6-permission set (`skill.create_global`, `skill.delete_global`, `skill.update_global`, `template.create_global`, `template.delete_global`, `template.update_global`).
  - `hub-admin` `11 -> 12` adding `template.create_global`, `template.delete_global`, `template.update_global` and keeping plain `template.*` excluded so `hub-admin` gains global template authority without gaining template write reach into non-member projects.
  - `super-admin` `11 -> 12` via dynamic `allPermissionIDs()`.
  - Added all three template twins as `testAdminPolicyAllow` in `pkg/hub/test_admin_policy.go`.
- Added `template.global.create`, `template.global.update`, `template.global.delete`, `template.global.clone`, and `template.global.reimport` (`Variant: "scope=global"`) and updated `template.import.global` (`BasePermission: "template.create_global"`) in `pkg/hub/authzop/catalog_catalog_resources.go`, regenerating `.design/authorization-operation-catalog.md`.

## Notes and Investigation Outcomes

- **Capability keys:** All three template twins use `CapabilityKind: CapabilityNone` so per-record `_capabilities.actions` stays under the stable plain keys `["read", "update", "delete"]` (evaluated through `catalogWriteAction`). List-level `_capabilities.actions` on `GET /api/v1/templates` uses `"create"`, matching `skillListCapabilities` (`pkg/hub/skill_handlers.go:466-495`) and `web/src/components/pages/skill-create.ts:476-492` (`GET /api/v1/skills?scope=global&limit=1` -> `can(data._capabilities, 'create')`).
- **Template draft / version-visibility investigation:** Unlike skills (`SkillVersion` with `skillVersionsAllStatuses` gating draft/archived version reads on `ActionUpdate`), templates have no version table (`pkg/ent/schema/template.go` stores a single active/pending content manifest on the template row) and no `ActionUpdate`-gated read path. Every template read handler (`listTemplatesV2`, `getTemplateV2`, `handleTemplateDownload`, `handleTemplateValidate`, and `GET /api/v1/templates/{id}/files[/...]`) gates solely on `ActionRead`. No draft-visibility routing change is needed for templates.
- **Frozen legacy ceiling & revision monotonicity:** `pkg/hub/permissions/legacy_ceiling.go` was left untouched; live selector surfaces were updated in `expectedSelectorRegistry` (`pkg/hub/permissions/registry_test.go`) and `FALLBACK_SCOPES` (`web/src/components/shared/token-list.ts`). If Phase 1 and Phase 2 land upstream together, `reconcileBuiltInRoles` only requires monotonic revisions (`applied.Revision < role.Revision` or `PermHash` mismatch), so jumping directly `1 -> 3` on `global-catalog-author` and `10 -> 12` on `hub-admin`/`super-admin` reconciles cleanly alongside `2 -> 3` / `11 -> 12`.

## Tests

- `pkg/hub/template_global_authz_test.go`: `TestTemplateGlobalTwins_PermissionRegistered`, `TestSeedReconcile_GlobalCatalogAuthorR3AppliesOnUpgrade`, `TestTemplateAuthz_GlobalCRUD_CatalogAuthorAllowed`, `TestTemplateAuthz_CatalogAuthorDeniedOnProjectAndUserTemplates`, `TestGlobalTemplateCreate_StampsCreatedByNotOwner`, `TestGlobalTemplate_CreatorWithoutRoleDenied`, `TestGlobalTemplateDelete_AuthorizesRecord`, `TestGlobalTemplate_PlainPermissionsDeniedOnGlobalAllowedOnProject`, `TestGlobalTemplate_HubAdminGlobalOnlyNotNonMemberProject`, `TestGlobalTemplateCapabilities_RecordAndList`, `TestExplainAPI_GlobalTemplateWriteUsesTwin`.
- `pkg/hub/catalog_global_token_test.go`: `TestGlobalTemplateWrite_RequiresTwinSelectorAndHubBoundary`, `TestCatalogAuthorHubToken_TemplateSyncJourney`, plus updated `TestGlobalResourceImport_ResolvesHubThroughEvidence`, `TestCatalogCloneToken_RequiresReadAndCreate`, and `TestCatalogReimportToken_RequiresReadAndCreate`.
- Updated `pkg/hub/skill_global_authz_test.go`, `pkg/hub/seed_roles_test.go`, `pkg/hub/project_create_token_test.go`, `pkg/hub/authz_boundary_test.go`, `pkg/hub/authz_relationship_policy_test.go`, `pkg/hub/authz_relationship_characterization_test.go`, `pkg/hub/authzop_sqlite_helpers_test.go`, `pkg/hub/authzop_catalog_method_inventory_test.go`, `pkg/hub/bearer_disposition_matrix_test.go`, and `pkg/hub/user_scope_writes_token_test.go`.

## Verification

- `make compat-literals` — PASS.
- `go test -buildvcs=false -count=1 ./pkg/hub/permissions/... ./pkg/hub/authzop/...` — PASS.
- `make check-authorization-catalog` and `make check-route-authz-manifest` — PASS (393 catalog operations, 242 routes).
- `make fmt-check`, `make lint-ci`, `make vet-integration-ci`, `make check-custom`, `make check-agent-mutators`, `make build` — PASS.
- `GOGC=40 golangci-lint run --new-from-rev=origin/scion/global-authz --concurrency=1 ./pkg/hub/permissions/... ./pkg/hub/authzop/...` and `--tests=false ./pkg/hub` — PASS (0 issues).
- `make vet-hub-shard SHARD=1..6` — PASS.
- `(cd web && npm ci) && make web-typecheck && make web-lint && make web-test` — PASS.
- `make test-fast-ci` and `make test-hub-shard SHARD=1..6` — PASS across all 6 `pkg/hub` shards (including `TestTemplateResourceLiterals_AllUseCanonicalConstructor`) and all non-hub packages.
