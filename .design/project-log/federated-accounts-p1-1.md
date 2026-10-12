# Federated accounts and visitor identity (phase 1, item 1)

**Date:** 2026-10-12
**Branch:** scion/mmr-fed-p1-1
**Tracked in:** ptone/scion#4603

## What changed

- **Schema.** `users.kind` gains `federated`; `users.status` gains `pending`
  (used only by federated rows). New immutable columns `federated_email`
  (lower case) and `home_issuer`, a unique index on
  `(home_issuer, federated_email)` where `kind = 'federated'`, and a CHECK that
  a federated row names both. Additive; existing rows are unchanged.
- **Store.** A dedicated federated account path on `store.Store`:
  `CreateFederatedUser` (pending or active, role viewer, synthetic email
  `fed+<id>@<issuer host>.invalid`), `GetFederatedUserByEmail`,
  `LinkFederatedIdentity` (an `ExternalIdentity` with provider `federation`,
  one per account, issuer must equal the home issuer) and
  `GetFederatedUserBySubject`. The general `CreateUser` refuses the kind, its
  fields, `pending` and the synthetic address shape.
- **Email-keyed lookups never return a federated row**: `GetUserByEmail`,
  `IsUserInvitedOrActive`, `ListEmailDomains`, the allow-list migration, and
  `ListUsers` (admin user lists, plus the startup role-grant and admin
  reconciliation loops) exclude them.
- **General grant paths refuse federated rows** at the store
  (`CreateRoleBinding`, `AddGroupMember` including hub-members,
  `CreateUserAccessToken`) and at the hub (CanDelegate grantee check,
  project member set, ownership transfer, broker email-resolved senders).
  The JWT row block refuses a hub-issued token whose subject is a federated
  row.
- **Identity.** `VisitorIdentity` (ID = local account ID, Type "visitor",
  carries issuer, subject, federated email) implements `UserIdentity` and
  `FederatedIdentity`; credential kind `federation`; principal kind
  `visitor`, normalized to store principal type `user`. A visitor delegates
  nothing.
- **Auth middleware.** After federation verification, when the experiment
  `federation.project_visitors` (server layer, default off) is on and the
  token's (issuer, subject) links to an active federated row, the identity
  becomes `VisitorIdentity`. Every other case keeps `FederatedUserIdentity`.
  A lookup error answers 503.
- **Interim route guard.** `routeGuard` refuses every request whose identity
  is a `VisitorIdentity` with one 403, until per-route visitor admission
  (item 4) replaces the check.

## Notes for later items

- Items 2/3 need to bind `project-visitor` to federated rows. The general
  `CreateRoleBinding` refusal stays; the visitor service should get a narrow
  store method (e.g. `CreateVisitorRoleBinding`) that runs the same
  validation, admits only the visitor role at project scope, and lifts the
  federated check only there.
- `requireActiveUser`'s "missing row passes" arm for `FederatedUserIdentity`
  still holds: that identity's ID is `<issuer>:<sub>`, never a local UUID.
- Display code must show `federated_email` with the issuer, never the
  synthetic address (not needed yet: visitors are refused everywhere).
