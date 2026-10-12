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
  The JWT row block refuses a hub-issued JWT whose subject is a federated
  row.
- **Identity.** `VisitorIdentity` (ID = local account ID, Type "visitor",
  carries issuer, subject, federated email) implements `UserIdentity` and
  `FederatedIdentity`; credential kind `federation`; principal kind
  `visitor`, normalized to store principal type `user`. A visitor delegates
  nothing.
- **Auth middleware.** After federation verification, when the experiment
  `federation.project_visitors` (server layer, default off) is on and the
  federation credential's (issuer, subject) links to an active federated row, the identity
  becomes `VisitorIdentity`. Every other case keeps `FederatedUserIdentity`.
  A lookup error answers 503.
- **Interim route guard.** `routeGuard` refuses every request whose identity
  is a `VisitorIdentity` with one 403, until per-route visitor admission
  (item 4) replaces the check.
- **One federation link per account.** A partial unique index on
  `external_identities(user_id)` where `provider = 'federation'` holds "one
  federation link per account" in the database, so the later of two
  concurrent links returns `ErrAlreadyExists` (the read before the insert
  stays, for a clear error). `UpdateUser` refuses invite fields on a
  federated row, matching `CreateFederatedUser`. Additive: the provider is
  new, so no existing rows can conflict with the index.

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

## Merge-forward (2026-10-12)

- Merged upstream main (4cefb876a) into the branch. Conflicts were limited
  to `pkg/experiments/registry.go` and `registry_test.go` (both experiments
  kept) and `pkg/ent/mutation.go` (regenerated with `go generate ./pkg/ent`;
  `make ent-check` clean). Upstream's removal of committed hub test shard
  tags carried over; the test files this branch adds carry none.
- Second merge-forward: merged upstream main (d1bd29925, then d01cdee82;
  the latter merged cleanly). Upstream added a
  user `issuer_delegation_id` column and index and a workload issuer field
  in `AuthConfig`; each conflict was two adjacent additions, so both sides
  were kept (`pkg/ent/schema/user.go`, `pkg/store/models.go`,
  `pkg/hub/auth.go`, `pkg/hub/server.go`) and the generated ent code was
  regenerated (`make ent-check` clean). `CreateFederatedUser` now also
  refuses the new `IssuerDelegationID` field, like every other test identity
  field. No test file this branch adds or changes carries a shard tag.
