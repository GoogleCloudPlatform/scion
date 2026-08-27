# Permissions Phase 1A Developer Log

Date: 2026-08-22
Branch: scion/permissions-p1a

## Summary

- Added `pkg/hub/permissions` as the canonical Phase 1A permission/resource registry.
- Derived Hub `ResourceActions`, `ScopeActions`, store UAT validation, store `agent:manage` expansion, and CLI UAT scope help from the registry.
- Removed stale `agent:start`, `agent:stop`, `agent:message`, and `agent:dispatch` from newly-created UAT vocabulary and capability projections while retaining legacy constants for old token metadata references.
- Added valid `project:update` and `agent:port_access` coverage across registry-derived server/CLI surfaces and the web token UI.
- Added registry drift tests covering capability maps, UAT scopes, agent token scopes, CLI help, and the web static scope list.
- Added `.agents/` to `.gitignore` so local orchestration state is not accidentally tracked.

## Notes

- Phase 1A does not introduce PolicyBoundary or centralize route enforcement. Registry `Enforcement` and `NonRouteUse` metadata document current use sites so drift tests can fail when exposed vocabulary has no matching use.
- Current agent lifecycle capability exposure uses `agent:attach`; `agent:start`, `agent:stop`, and `agent:message` remain as legacy action constants but are not exposed as independent capabilities or valid new UAT scopes.
- The web scope list remains static in this phase. It is guarded by a Hub drift test that parses `AVAILABLE_SCOPES` and compares it exactly to registry UAT scopes, including aliases.

## Round 1 Audit And QA Disposition

- Medium audit finding fixed: `AuthzService.CheckAccess` now prevents `ScopedUserIdentity` credentials from using the role-only hub admin bypass after UAT project/scope constraints pass. Scoped UATs continue through owner, project owner/admin membership, hub-member baselines, and policy grants.
- Regression coverage added: scoped admin UAT with `project:update` is denied when the underlying admin role is the only authority, and allowed when the underlying user has an explicit policy grant or project admin membership.
- Low audit finding strengthened in scope: registry `Enforcement` entries now must point at an existing source file, and `file.go:symbol` entries must contain the referenced symbol.
- Required code review finding fixed: `agent:port:forward` is mapped only to `agent.port_forward` and no longer appears on user-facing `agent.port_access`.
- Agent token drift coverage strengthened: known agent token scopes now assert exact registry permission ID mappings, including the intentional `project:agent:lifecycle` mapping to `agent.attach` and `agent.delete`.
- Medium QA follow-up strengthened in scope: the web token scope list drift test now performs exact registry equality rather than only stale/required substring checks.
- Low QA follow-up fixed in scope: `cmd` now has a focused `TestHubTokenCreateHelpUsesRegistryScopes` test for direct command help coverage.
- `npm audit` high `js-yaml` advisory is outside Phase 1A permission-registry scope. Recommended owner: web/dependency maintenance. Risk: `web/src/components/pages/skill-create.ts` imports `js-yaml`; reachability and upgrade impact should be triaged in a separate dependency security task.
- `npm run lint` remains a non-blocking repo-wide/pre-existing web lint debt item; the Phase 1A edit did not introduce a new lint category.

## Verification

- Baseline focused authz tests before implementation: passed.
- Focused registry/UAT/capability tests: passed.
- `go test ./pkg/hub -run '^TestHandleAgentMessage_BrokerTimeout504$' -timeout=60s -count=1`: passed.
- `go test ./pkg/hub -timeout=600s -count=1`: passed.
- `go test ./pkg/store -timeout=120s -count=1`: passed.
- `go test ./cmd -timeout=120s -count=1`: passed.
- `npm run typecheck` in `web/`: passed.
- `npx prettier --check src/components/shared/token-list.ts` in `web/`: passed.
- `npm run build` in `web/`: passed.
- `make ci`: passed.

Round 1 fix verification:

- `go test ./pkg/hub -run 'TestPermissionRegistryEntriesDeclareCurrentUse|TestTokenScopeSurfacesDoNotExposeStaleUATScopes|TestAuthz_ScopedAdminUATProjectUpdateRequiresIndependentGrant|TestAuthz_AdminBypass|TestAuthz_ProjectOwnerBypass' -count=1`: passed.
- `go test ./cmd -run '^TestHubTokenCreateHelpUsesRegistryScopes$' -count=1`: passed.
- `go test ./pkg/hub -run 'TestAgentTokenScopesMapToRegistry|TestPermissionRegistryEntriesDeclareCurrentUse|TestTokenScopeSurfacesDoNotExposeStaleUATScopes' -count=1`: passed.
- `go test ./pkg/hub -run 'TestPermissionRegistryEntriesDeclareCurrentUse|TestCapabilityActionMapsAreRegistryDerived|TestUATScopesAreRegistryDerived|TestAgentTokenScopesMapToRegistry|TestTokenScopeSurfacesDoNotExposeStaleUATScopes|TestAuthz_ScopedAdminUATProjectUpdateRequiresIndependentGrant|TestAuthz_AdminBypass|TestAuthz_ProjectOwnerBypass|TestCreateToken|TestValidateToken|TestExpandScopes|TestScopedUserIdentity|TestResourceActions_AgentLifecycleUsesAttachPermission' -timeout=120s -count=1`: passed.
- `go test ./cmd -run '^TestHubTokenCreateHelpUsesRegistryScopes$' -timeout=120s -count=1`: passed.
- `go test ./pkg/hub -timeout=600s -count=1`: passed.
- `npm run typecheck` in `web/`: passed.
- `npx prettier --check src/components/shared/token-list.ts` in `web/`: passed.
- `make ci`: passed.

`npm run lint` was attempted after installing web dependencies and failed on existing repo-wide lint debt unrelated to the Phase 1A scope edit, including test files outside the frontend tsconfig and pre-existing strict eslint violations.

## Round 2 Audit Disposition

- Medium audit finding fixed: `authorizePortRegistration` now rejects `ScopedUserIdentity` before the raw hub-admin shortcut can grant exposed-port registration, deletion, clearing, or tunnel management. Unscoped hub admins retain the existing behavior, and agent identities still require `agent:port:forward`.
- Regression coverage added: `TestAuthorizePortRegistrationRejectsScopedHubAdmin` gives a scoped hub-admin UAT `agent:port_access` plus independent project/agent ownership, verifies the access check passes far enough to reach registration authorization, and asserts the scoped credential is forbidden.
- The `js-yaml` npm advisory remains separate web/dependency triage as previously recorded; no Phase 1A registry or auth-surface expansion was made.

Round 2 verification:

- `go test ./pkg/hub -run 'TestAuthorizePortRegistrationRejectsScopedHubAdmin|TestAgentPortRegistrationLifecycle|TestPermissionRegistryEntriesDeclareCurrentUse|TestAuthz_ScopedAdminUATProjectUpdateRequiresIndependentGrant' -count=1`: passed.
- `go test ./pkg/hub -timeout=600s -count=1`: passed.
