# Permissions Foundation Phase 1A Round 3 QA

**Agent:** Codex
**Role:** Test Engineer
**Branch:** `scion/permissions-p1a`
**Commit tested:** `56c9b34747cb49529a8ee3c83e3062f38e4d92b7`
**Date:** 2026-08-22
**Verdict:** `APPROVE`

## Summary

Verified the round 3 Phase 1A head that fixes the remaining scoped port-registration admin bypass from the round 2 audit.

The specific regression is `TestAuthorizePortRegistrationRejectsScopedHubAdmin`. It constructs a hub-admin user narrowed through `ScopedUserIdentity` with `agent:port_access`, verifies the caller gets past the underlying port-access shape, and asserts `authorizePortRegistration` denies before the raw admin management shortcut.

## Verification

Commands passed:

```bash
go test ./pkg/hub -run '^TestAuthorizePortRegistrationRejectsScopedHubAdmin$' -timeout=120s -count=1
go test ./pkg/hub -run 'TestAuthorizePortRegistrationRejectsScopedHubAdmin|TestPermissionRegistryEntriesDeclareCurrentUse|TestCapabilityActionMapsAreRegistryDerived|TestUATScopesAreRegistryDerived|TestAgentTokenScopesMapToRegistry|TestTokenScopeSurfacesDoNotExposeStaleUATScopes|TestAuthz_ScopedAdminUATProjectUpdateRequiresIndependentGrant|TestAuthz_AdminBypass|TestAuthz_ProjectOwnerBypass|TestCreateToken|TestValidateToken|TestExpandScopes|TestScopedUserIdentity|TestResourceActions_AgentLifecycleUsesAttachPermission' -timeout=120s -count=1
go test ./cmd -run '^TestHubTokenCreateHelpUsesRegistryScopes$' -timeout=120s -count=1
go test ./pkg/hub -timeout=600s -count=1
cd web && npm ci
cd web && npm run typecheck
cd web && npx prettier --check src/components/shared/token-list.ts
make ci
```

Key results:

- Scoped port-registration regression: `ok github.com/GoogleCloudPlatform/scion/pkg/hub 0.203s`.
- Focused registry/authz/UAT suite: `ok github.com/GoogleCloudPlatform/scion/pkg/hub 1.114s`.
- CLI help coverage: `ok github.com/GoogleCloudPlatform/scion/cmd 0.111s`.
- Full Hub package: `ok github.com/GoogleCloudPlatform/scion/pkg/hub 205.092s`.
- Web typecheck: exit 0.
- Token-list Prettier check: `All matched files use Prettier code style!`.
- `make ci`: `CI passed.`

## Non-blocking Notes

`cd web && npm run lint` still fails with the known broad lint debt:

```text
2307 problems (719 errors, 1588 warnings)
```

The classes match the prior disposition: test files outside the typed-lint tsconfig, unsafe `any`, missing return types, and existing `unbound-method` findings in `token-list.ts`. This is not a Phase 1A blocker because the focused web typecheck and token-list formatting gates pass.

`npm ci` still reports 5 dependency vulnerabilities; this remains separate dependency triage.

This workspace only has `scion/permissions-p1a` and `origin/scion/permissions-p1a` refs, so no local `main` comparison was available without fetching.

Full report: `/scion-volumes/scratchpad/projects/auth-refactor/reports/pf-1a-test3.md`.
