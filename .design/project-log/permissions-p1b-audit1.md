# Phase 1B Security Audit 1

Date: 2026-08-22

Verdict: **REQUEST CHANGES**

The security audit found one High-severity authorization issue: a federated user configured with `default_role: admin` enters the local user admin-bypass path. The federation middleware accepts the signed external token, `extractUserClaims` assigns the configured role, and both `AuthzService.Decide` plus direct admin-role guards can grant platform-admin access.

Report: `/scion-volumes/scratchpad/projects/auth-refactor/reports/pf-1b-audit1.md`

Required remediation: exclude federated identities from local platform-admin shortcuts (centralize the predicate), restrict or remove federated `default_role: admin`, and add end-to-end denials for authorization, stop-all, maintenance, and broker secret rotation.

Federated-service denial is intentionally retained as fail-closed residual risk until a supported service-identity surface is designed.
