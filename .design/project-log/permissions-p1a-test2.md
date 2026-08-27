# Permissions Phase 1A QA Round 2 Log

Date: 2026-08-22
Branch: scion/permissions-p1a
Commit tested: d93ba151cb9902387c15b10e03a3ae4ab54e654b

## Summary

- Verified round 1 fixes for permissions-foundation Phase 1A at `d93ba15`.
- Verdict: `APPROVE`.
- Wrote full report to `/scion-volumes/scratchpad/projects/auth-refactor/reports/pf-1a-test2.md`.

## Verification

- `go test ./pkg/hub -run 'TestPermissionRegistryEntriesDeclareCurrentUse|TestCapabilityActionMapsAreRegistryDerived|TestUATScopesAreRegistryDerived|TestAgentTokenScopesMapToRegistry|TestTokenScopeSurfacesDoNotExposeStaleUATScopes|TestAuthz_ScopedAdminUATProjectUpdateRequiresIndependentGrant|TestAuthz_AdminBypass|TestAuthz_ProjectOwnerBypass|TestCreateToken|TestValidateToken|TestExpandScopes|TestScopedUserIdentity|TestResourceActions_AgentLifecycleUsesAttachPermission' -timeout=120s -count=1`: passed.
- `go test ./cmd -run '^TestHubTokenCreateHelpUsesRegistryScopes$' -timeout=120s -count=1`: passed.
- `go test ./pkg/hub -timeout=600s -count=1`: passed.
- `npm ci` in `web/`: passed with existing vulnerability warnings.
- `npm run typecheck` in `web/`: passed.
- `npx prettier --check src/components/shared/token-list.ts` in `web/`: passed after `npm ci`.
- `npm run lint` in `web/`: failed with existing repo-wide lint debt; confirmed non-blocking for Phase 1A.
- `make ci`: passed.

## Disposition

- Web `AVAILABLE_SCOPES` exact registry equality including aliases is covered by the Hub drift test and passed.
- Direct CLI help test `TestHubTokenCreateHelpUsesRegistryScopes` exists and passed.
- Scoped-admin UAT admin-bypass regression coverage exists and passed.
- Exact agent token scope-to-permission registry mapping coverage exists and passed.
- Developer-reported checks are reproducible enough for Phase 1A. Remaining web lint and dependency audit issues are external debt, not Phase 1A blockers.
