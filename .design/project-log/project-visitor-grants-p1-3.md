# Project visitor grants by email and first contact (phase 1, item 3, part A)

**Date:** 2026-10-13
**Branch:** scion/mmr-fed-p1-3 (stacked on scion/mmr-fed-p1-2)
**Tracked in:** ptone/scion#4603

## What changed

- **Issuer flag.** `allow_visitor_grants` on a trusted issuer (default
  false, `issuer_type` `user` or `hub` only; validation refuses it on any
  other type). Schema, v1 conversion and the server config reference carry
  it. The federation claims keep `email_verified` as present/absent/value
  (`FederatedUserIdentity.EmailVerifiedClaim`).
- **Store.** `ListVisitorRoleBindings(projectID)`,
  `UpdateVisitorRoleBindingWindow(id, notBefore, expiresAt)` (refuses any
  binding that is not the project-visitor role) and
  `LinkPendingFederatedUser(userID, issuer, subject, email)`: links the
  subject and sets the pending account active in one transaction. Every
  "another first contact got there first" outcome (account already linked,
  pair already bound, either unique index, the conditional status update
  finding the account no longer pending) returns `ErrAlreadyExists`.
- **Routes** (`/api/v1/projects/{id}/visitors`, new project sub-route
  `visitors`): `GET` list, `POST` grant `{issuer, email, notBefore?,
  expiresAt?}`, `PATCH /{visitorId}` replace the window, `DELETE
  /{visitorId}` revoke. `{visitorId}` is the federated account ID. Order of
  checks: experiment (404 while `federation.project_visitors` is off),
  method (405), interactive session or dev credential
  (`requireSessionCredentialFor`, `GOV_PENDING`), project, then
  `project.manage_visitors` on the project. Grant and window change also
  apply non-amplification (the actor holds every permission of the
  `project-visitor` role on the project; an unscoped local platform admin
  passes, as in `CanDelegate`).
- **Grant.** Issuer must be trusted with `allow_visitor_grants` and a user
  or hub type; the email (lower-cased) must pass `allowed_emails` and
  `allowed_domains`; otherwise 400 with `details.field` and a neutral
  message. One transaction finds or creates the pending federated account,
  binds the role (`CreateVisitorRoleBinding`) and writes
  `project_visitor_grant`. A repeated grant is 409 (decision: conflict, not
  idempotent; change the window with PATCH). A suspended account gets 409.
  A concurrent create of the same account is retried once.
- **Revoke and window.** `DELETE` deletes the binding and writes
  `project_visitor_revoke` in one transaction; `PATCH` writes
  `project_visitor_update`. Authorization reads bindings per request, so a
  revoke or an expired window refuses the next request.
- **First contact.** `resolveFederatedVisitor` now tries
  `linkPendingVisitor` when the subject is not linked: the issuer must
  allow visitor grants, the token must carry an email, `email_verified`
  must be true when present, the email must pass the issuer's lists, and a
  pending account must exist for (issuer, email). Link, activation and the
  `visitor_linked` audit record are one transaction. A losing concurrent
  request maps `ErrAlreadyExists` to a re-read by subject.
- **Status policy** (request path): active acts as the visitor; suspended
  is refused with 403 at the auth middleware; pending never acts as a
  visitor until first contact activates it. A grant is refused for a
  suspended account.
- **Authorization catalog.** Operations `project.visitors.list`, `.add`,
  `.update`, `.remove` (all session only, `GOV_PENDING`). The item 2
  deferred entries for `project.manage_visitors` are gone; its registry row
  now records Enforcement in `handlers_project_visitors.go`. New audit
  scanner symbols `UpdateVisitorRoleBindingWindow` (change authority) and
  `LinkPendingFederatedUser` (change principal status); the call sites of
  these, `CreateVisitorRoleBinding` and the revoke `DeleteRoleBinding` are
  classified (first contact as an authentication-only exemption).
- **CLI.** `scion project visitors list | add --issuer --email [--expires]
  | remove <email|id> [--issuer] | extend <email|id> --expires`, through
  `hubclient.ProjectVisitors`. Not in `agentAllowed` (human mode only).
  `--domain` is not offered (domain grants are part B).
- Visitors are still refused on every route by the route guard; nothing
  here opens a route to a visitor.

## How part B (domain grants) plugs in

Rules get their own table and routes (`/visitors/rules`) and materialize
per-user bindings through the same `CreateVisitorRoleBinding`, tagged with
`source_rule_id`. First contact gains a second branch after the pending
email lookup: match an active rule for (issuer, verified domain), create or
link the account, and materialize the binding in the same transaction.
`ListVisitorRoleBindings` and the list response can then carry the source;
`DELETE /visitors/{id}` removes only bindings without a source (a rule
binding gets an exclusion row instead).

## Follow-ups

- Turning `allow_visitor_grants` off stops new grants and new first
  contacts, but an already linked visitor keeps resolving; whether the flag
  should also stop linked visitors at the request path is open.
- A pending account whose last grant is revoked stays (pending); first
  contact still links it, with no grant.
- Admin tooling to suspend a federated account does not exist yet.
