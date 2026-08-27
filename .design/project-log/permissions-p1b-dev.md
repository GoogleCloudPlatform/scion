# Permissions Phase 1B Development Log

## Baseline

- Target branch: `scion/permissions-p1b`.
- Current main baseline: `7d171a503fbc01f950513e599e44dc4088eae0d2`.
- Accepted Phase 1A baseline: `origin/scion/permissions-p1a` at
  `15833c477dbbf23362d429ef4c22a7716e1e696f` (including implementation fix
  `56c9b34747cb49529a8ee3c83e3062f38e4d92b7`).
- Reconciliation: both branches share merge base
  `feb3e188f147c52d760d3530710a1f72eb7062b7`. The initial shallow clone hid
  this ancestry; after authorized `git fetch --unshallow origin`, the selected
  Phase 1A branch can be cleanly merged onto current main without using
  unrelated-history merging.

## Implementation

- Added `PrincipalContext`, `CredentialContext`, `AuthzRequest`,
  `AuthzRequestFromContext`, and request-based `AuthzService.Decide` /
  `DecideFromContext`. `CheckAccess` remains a compatibility adapter.
- Extended decisions with principal, credential, and matched-policy/grant
  metadata for future audit emission.
- Auth middleware now carries credential context for interactive users, UATs,
  agent JWTs, federation tokens, development auth, and broker HMAC auth.
  UAT credentials retain their stored token ID; agent JWTs use their JTI.
- Explicitly map federated users and agents into the decision pipeline, deny
  federated services, and reject any attempted principal-kind relabeling.
- Closed scoped-admin bypasses in maintenance admission, broker secret
  rotation, group role escalation, and global/project stop-all. Unscoped admin
  behavior is unchanged.

## Verification

- Focused request, credential, federation, UAT, and scoped-admin bypass tests.
- `go test ./pkg/hub -timeout=600s -count=1`.
- `make ci`.

## Residual Risks

- Federated service identities are deliberately denied until a supported hub
  surface and policy model are specified. No Phase 1C/1D ordering or route
  guard work was introduced.

## Round 1 Security Audit Remediation

- Added `IsUnscopedLocalPlatformAdmin`, the single predicate for local
  platform-admin bypasses. It rejects both UAT-scoped and federated users.
- Applied the predicate to policy authorization plus the Phase 1B direct
  shortcuts: maintenance admission, broker rotation, group hierarchy, and
  global/project stop-all.
- Federation config now rejects `issuer_type: user` with `default_role: admin`.
  Identity construction also normalizes that invalid role to `viewer` as a
  defense in depth.
- Added regressions for a federated admin decision, maintenance, global
  stop-all, broker rotation, and invalid federation configuration.

## Round 2 Audit Remediation

- Updated `requireAdmin` to use `IsUnscopedLocalPlatformAdmin` after its
  user-identity assertion, preserving the existing scoped-token and ordinary
  non-admin audit reasons while denying federated admin identities.
- Added `requireAdmin` coverage for federated-admin denial alongside existing
  scoped-admin denial and local-admin allow coverage.
- Added branch-level regressions proving a policy-authorized federated admin
  cannot escalate a group member and a non-member federated admin cannot use
  project-scoped stop-all.

## Round 3 Security Audit Remediation

- Updated hub pre-start-hook mutation and script-visibility checks to use
  `IsUnscopedLocalPlatformAdmin`, denying both scoped and federated admin
  identities while preserving unscoped local-admin access.
- Redacted hub hook scripts from both list and detail responses for readers
  that are not unscoped local platform admins.
- Added a federated-admin regression covering create, update, activate,
  delete, list, and detail; unscoped local-admin create/list/detail coverage
  remains in the handler suite.

## Round 4 Code Review Remediation

- Updated `authorizePortRegistration` to use
  `IsUnscopedLocalPlatformAdmin` for its direct port-management bypass while
  retaining the existing scoped-token denial response.
- Added a regression where a federated admin has an ordinary policy grant for
  `agent:port_access` but is still forbidden from port registration/tunneling.
  The same suite explicitly retains unscoped local-admin access.
