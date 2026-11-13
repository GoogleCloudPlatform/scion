# Hub: global skill update/delete twin permissions (ptone/scion#1713 Phase 2, slice 1)

**Date:** 2026-11-12
**Branch:** scion/global-authz
**Issue:** ptone/scion#1713

## Behaviour

- Added `skill.update_global` (`skill:update_global`) and `skill.delete_global` (`skill:delete_global`) to `pkg/hub/permissions/registry.go` (`ExcludeFromManageAlias: true`, `CapabilityKind: CapabilityNone`, Hub boundary only, `GlobalCatalog` target class, empty collection target classes) and defined `ActionUpdateGlobal` / `ActionDeleteGlobal` in both `pkg/hub/authz_kernel_vocab.go` and `pkg/hub/permissions/registry.go`.
- Added kind-aware `catalogWriteAction(kind, scope string, a Action) Action` in `pkg/hub/authz_catalog_scope.go` (replacing `globalWriteAction`). In Phase 1, `kind = skill` maps `scope = global` `create` / `update` / `delete` to `create_global` / `update_global` / `delete_global`, keeps `scope = core` on its interim mapping (`create -> create_global`, `update` / `delete -> plain`) pending the `core` retirement phase, and leaves `kind = template` unchanged for Phase 2.
- Wired all seven skill record-write HTTP handlers (`updateSkill`, `deleteSkill`, `deprecateSkillVersion`, `publishSkillVersion`, `handleSkillUpload`, `handleSkillFinalize` in `pkg/hub/skill_handlers.go`; `handleSkillFileWrite` in `pkg/hub/skill_file_handlers.go`), create call sites (`canCreateSkillInScope`, `createSkill`, `authorizeTokenSkillDiscover`), and draft/archived version visibility (`skillVersionsAllStatuses` in `pkg/hub/skill_version_status.go`, skipping the `OwnerID`/`CreatedBy` shortcut when `skill.Scope == store.SkillScopeGlobal`) to `catalogWriteAction` on the stored skill scope.
- Updated `pkg/hub/permissions/relationship_policy.go` to remove unreachable `skill.create_global` from the `owner/user/skill` row and keep `skill.update_global` / `skill.delete_global` off all relationship rows. Creators of global skills whose `global-catalog-author` role is later revoked lose write and draft-visibility authority despite `OwnerID`/`CreatedBy` remaining stamped on the skill record for audit.
- Updated `pkg/hub/capabilities.go` (`ComputeCapabilities`, `ComputeCapabilitiesBatch`, `ComputeCapabilitiesForActions`, `computeCapabilitiesWithContext`) to evaluate `catalogWriteAction(resource.Type, resource.ScopeKind, action)` while reporting capability keys under the stable plain names (`read`, `update`, `delete`).
- Bumped seeded role revisions in `pkg/hub/seed.go`: `global-catalog-author` `1 -> 2` (`skill.create_global`, `skill.delete_global`, `skill.update_global` in Phase 1; template twins follow in Phase 2), `hub-admin` `10 -> 11`, `super-admin` `10 -> 11`; classified both twins as `testAdminPolicyAllow` in `pkg/hub/test_admin_policy.go`.
- Added `skill.global.update` and `skill.global.delete` (`Variant: "scope=global"`, `AdmitOn(BearerTargetCatalogRecord, BearerBoundaryHub)`) to `pkg/hub/authzop/catalog_catalog_resources.go`, copying `AuditObligation` from `skill.delete` onto `skill.global.delete`, and regenerated `.design/authorization-operation-catalog.md`.

## Notes and Design Clarifications

- `pkg/hub/permissions/legacy_ceiling.go` was intentionally left untouched: `legacyUATScopeToPermissionID` is a frozen 2026-09-30 snapshot (`TestLegacyUATScopeToPermissionID_Golden`) for `CeilingVersionUnspecified` tokens minted before `CeilingVersionPermissionIDs`. Live selector surfaces were updated in `expectedSelectorRegistry` (`pkg/hub/permissions/registry_test.go`) and `FALLBACK_SCOPES` (`web/src/components/shared/token-list.ts`).
- `CapabilityKind` on `skill.update_global` and `skill.delete_global` is `CapabilityNone` rather than `CapabilityResource` so `permissions.ResourceActions()["skill"]` stays `["read", "update", "delete"]` and never emits `"update_global"` / `"delete_global"` on project, user, or global skills. `CapabilityKind` does not gate selector resolution (`buildSelectorRegistry`), picker options (`UATScopeOptions`), `/api/v1/permissions` (`listPermissions`), or custom-role permission validation (`validatePermissionIDs`).
- `DowngradeFloor` (`pkg/version`) needs no bump:
  - `pkg/ent/schema/roledefinition.go:46-47` stores `permissions` as `field.JSON("permissions", []string{})`; `pkg/store/entadapter/role_store.go:93-107, 134-204` reads `rd.Permissions` verbatim without registry lookup.
  - `reconcileBuiltInRoles` (`pkg/hub/seed.go:724-760, 825-833`) skips built-in roles when `applied.Revision > role.Revision` with a single warning (`TestReconcileBuiltInRoles_DoesNotDowngrade`, `pkg/hub/seed_roles_test.go:347-372`) and never validates custom-role permission arrays on startup.
  - Role read/list/delete/export handlers (`pkg/hub/handlers_roles.go:487-520, 652-687, 789-881`) do not re-validate stored IDs; `validateRolePermissionIDs` (`pkg/hub/handlers_roles.go:1996-2028`) runs only on create/update/import/duplicate writes (400, not 500).
  - Request authorization (`pkg/hub/authz_list.go:388-405`, `pkg/hub/authz_kernel.go:83-105, 223-249`), token minting (`pkg/hub/authz_boundary.go:1225-1293, 1827-1875`), and `CanDelegate` (`pkg/hub/authz_candelegate.go:836-903`) load stored role permission strings into `map[string]struct{}` sets where unknown IDs sit inertly.

## Tests

- `pkg/hub/skill_global_authz_test.go`: `TestCatalogWriteAction_MapsCorrectly`, `TestGlobalSkillUpdate_CatalogAuthorAllowed`, `TestGlobalSkillDelete_CatalogAuthorAllowed`, `TestGlobalSkillUpdate_UngrantedMemberDenied`, `TestGlobalSkillUpdate_OwnerWithoutRoleDenied` (including non-creator author draft visibility vs ex-author creator denial), `TestGlobalSkillUpdate_HubAdminAllowed`, `TestGlobalSkillUpdate_PlainSkillUpdateDeniedOnGlobal`, `TestGlobalCatalogAuthor_CannotWriteProjectOrUserSkills`, `TestGlobalSkill_HubAdminAndSuperAdminHaveTwins`, `TestSeedReconcile_GlobalCatalogAuthorR2AppliesOnUpgrade`, `TestSkillGlobalTwins_PermissionRegistered`, `TestGlobalSkillCapabilities_CatalogAuthorVsMember` (including system-role holder on a project skill).
- `pkg/hub/catalog_global_token_test.go`: `TestGlobalSkillWrite_RequiresTwinSelectorAndHubBoundary`, `TestCatalogAuthorHubToken_SkillPublishJourney` (end-to-end hub UAT publish journey with exact CI recipe selectors).
- `pkg/hub/authz_relationship_characterization_test.go`, `pkg/hub/authz_relationship_policy_test.go`, `pkg/hub/authz_sqlite_helpers_test.go`: global-skill owner characterization and allowlist/exclusion updates.
- `pkg/hub/authzop_sqlite_helpers_test.go`, `pkg/hub/authzop_catalog_method_inventory_test.go`, `pkg/hub/web_api_compress_catalog_test.go`, `pkg/hub/project_create_token_test.go`: project-scoped skill fixtures for plain `skill.update`/`skill.delete`, trailing-segment and compress-bypass entries for `skill.global.update`, and `super-admin` revision 11 assertion.

## Verification

- `go test -buildvcs=false -count=1 ./pkg/hub/permissions/...` — PASS.
- `go test -buildvcs=false -count=1 ./pkg/hub/authzop/...` — PASS.
- `make check-authorization-catalog` and `make check-route-authz-manifest` — PASS (388 catalog operations, 242 routes).
- `make fmt-check`, `make lint-ci`, `make vet-integration-ci`, `make check-custom`, `make build` — PASS.
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/...` — PASS (0 issues).
- `make vet-hub-shard SHARD=1..6` — PASS.
- `make test-fast-ci` and `make test-hub-shard SHARD=1..6` — PASS across all 6 `pkg/hub` shards and non-hub packages.
- Web checks (`make web-typecheck`, `make web-lint`, `make web-test`) were not run because `web/node_modules` is not preinstalled in this sandbox; `TestTokenScopeSurfacesDoNotExposeStaleUATScopes` verified `FALLBACK_SCOPES` in `web/src/components/shared/token-list.ts` against `permissions.UATScopeOptions()`.
