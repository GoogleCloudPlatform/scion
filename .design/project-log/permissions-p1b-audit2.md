# Permissions Phase 1B — Security Audit Round 2

Date: 2026-08-22

Verdict: **REQUEST CHANGES**

The round-two audit confirms the round-one federation-admin escalation is blocked in the policy engine and federation configuration: user issuers reject `default_role: admin`, claim extraction defangs that value, and the reviewed Phase 1B shortcuts use `IsUnscopedLocalPlatformAdmin`.

One Medium required finding remains. `Server.requireAdmin` (`pkg/hub/authorize.go`) independently checks only `UserIdentity.Role() == admin`; it rejects scoped UATs but accepts a `FederatedUserIdentity` with role `admin`. Because this wrapper gates the admin and policy routes, it must use `IsUnscopedLocalPlatformAdmin` and gain federated/scoped-deny plus local-admin-allow regressions.

Federated-service authorization remains explicitly fail-closed in `AuthzService.Decide`; that residual risk is accepted pending a supported service surface. Focused Hub and config tests passed. `govulncheck` was unavailable in the audit environment.
