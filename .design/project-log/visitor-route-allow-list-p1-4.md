# Visitor route allow-list in the route guard (phase 1, item 4)

**Date:** 2026-10-13
**Stacked on:** project visitor grants by email and first contact
**Tracked in:** ptone/scion#4603

## What changed

- **Route metadata.** `RouteMetadata.VisitorMethods` (a list of HTTP
  methods, empty by default). `routeGuard` admits a request whose identity
  is a `VisitorIdentity` only when its method is in the route's list
  (`refuseVisitorRequest(w, r, meta)`); every other request, whatever the
  route's classification, answers `403` with `visitorRouteRefusalMessage`
  before the handler runs. An admitted visitor
  then passes the route's classification guard like any identity. Requests
  with any other identity, including an unlinked `FederatedUserIdentity`,
  are not affected.
- **Startup check.** `validateVisitorRoutes` (route_metadata_check.go)
  accepts a list only on `authenticated` and `policy` routes, and only
  known methods, each listed once, that the pattern serves (a
  method-qualified pattern such as `GET /x` serves its method, plus `HEAD`
  for `GET`); `New` refuses to start otherwise, as it does for an invalid
  session-only route.
- **Listed routes.** Only `/api/v1/auth/me`, with `GET` and `HEAD`:
  `handleAuthMe` returns the caller's own ID, email (the issuer-verified
  email for a visitor), display name, role (`viewer`) and preferences. The
  pattern has no method, so the mux serves `HEAD` and the write methods
  too; a visitor gets `403` on `POST`, `PUT`, `PATCH` and `DELETE`.

## Decisions

- The design names `GET /api/v1/users/me`; no such route exists.
  `/api/v1/users/{id}` (`handleUserByID`) also serves `PATCH`, `DELETE` and
  `revoke-sessions`, so it is not flagged. `/api/v1/auth/me` is the own
  profile route, and the one the design's bridge reads for the visitor's
  local ID.
- Admission is per method, not per pattern. Most patterns carry no method,
  so a per-pattern flag would admit every method the handler serves; a
  method list keeps a visitor to the methods named, and the messaging item
  reuses it for its `POST` message routes. An earlier per-pattern flag
  admitted every method of `/api/v1/auth/me`; the handler only reads, but
  the list makes the admitted methods explicit.
- A route listing the visitor's own grants does not exist. Adding it means
  a new route, catalog operation and inventory entries, so it is left to
  the messaging item.

## Tests

- `TestRouteGuard_AdmitsVisitorOnlyOnListedMethods`: every route in
  `routeMetadataTable` (kept equal to the registered set by
  `TestRegisteredRoutesHaveRouteMetadata`) and method refuses a visitor
  with the uniform 403 unless the (pattern, method) pair is in
  `expectedVisitorRouteMethods`; the pairs the routes list must equal that
  set.
- `TestRouteGuard_ProfileWriteRoutesRefuseVisitor`: user-scope env and
  secrets, user templates, injected skills, chat preferences, presence,
  attachments, the user record, user access credentials, delegations and
  the terminal workspace are registered and refuse a visitor.
- `TestRouteGuard_VisitorRefusedUnlessMethodListed`: the empty list
  refuses on all eight classifications; a list admits its methods only.
- `TestRouteGuard_VisitorRefusalLeavesOtherIdentities`: an unlinked
  federated user never receives the visitor refusal on any route.
- End to end (project_visitor_grants_test.go): a linked visitor reads its
  own profile on `/api/v1/auth/me` and is refused on `/api/v1/users/me/groups`;
  it is admitted on `GET` and `HEAD` of `/api/v1/auth/me` and refused on
  `POST`, `PUT`, `PATCH` and `DELETE`;
  with `allow_visitor_grants` off it is refused on `/api/v1/auth/me` too;
  an unlinked federated user gets identical responses with the experiment
  on and off.
- `TestValidateVisitorRoutes`, `TestValidateVisitorRoutesMethods` (a method
  the pattern does not serve, an unknown method, a repeated method),
  `TestRouteMetadataTableVisitorRoutesAreValid`,
  `TestNewRefusesInvalidVisitorRoute`, `TestNewRefusesVisitorMethodNotServedByPattern`.

## Follow-ups

- Messaging item: make the message, DM read and agent list/read handlers
  visitor-safe, then list their methods (several sit under catch-all patterns such as
  `/api/v1/projects/`, so admission there needs a sub-route check, like the
  delegated-credential admission in agent_delegation_routes.go, rather than
  the per-pattern method list alone).
- A `GET` of the visitor's own grants.

## Merge forward

- 2026-10-15: merged upstream main forward; the visitor grant code now comes from upstream, and this item's changes are unchanged.
- 2026-10-17: merged the visitor rule and federation principal check work, then upstream main, forward; visitors added by a domain rule get the same per-route admission as visitors granted by email, and this item's changes are unchanged.
- 2026-10-19: merged upstream main forward (to ecb55e3b7700). Upstream now
  makes `/api/v1/auth/me` an entry point of the `user.self.profile` catalog
  operation: `handleAuthMe` answers `405` to any method but `GET`, then
  admits only the credentials the operation declares (`selfReadCredential`).
  The federation credential kind was not among them, so a visitor got `403`
  on its own profile. See "Own profile read under the catalog credentials".

## Own profile read under the catalog credentials

- **Catalog.** A new catalog credential kind, `federated_user`
  (`authzop.CredentialFederatedUser`): a trusted issuer's credential for
  one of its users, presented as the federation credential. Only
  `user.self.profile` declares it; it maps to the request kind
  `CredentialKindFederation` in `catalogCredentialRequestKinds`.
- **Pairing.** `selfReadCredential` admits the federation kind only with a
  federated user identity (`isFederatedUserIdentity`: a `VisitorIdentity`
  or an unlinked `FederatedUserIdentity`), and such an identity only with
  the federation kind, the way a user access token kind is paired with its
  token identity. A federated agent or service identity, or a local user
  with the federation kind, is refused. No exemption is added.
- **Unlinked federated user.** Admitted too: it reads its verified
  identity as before upstream's change, and gets the same answers with the
  visitor experiment on and off.
- **HEAD.** The route still lists `GET` and `HEAD` in `VisitorMethods`;
  `HEAD` now reaches the handler's method check and answers `405`, as for
  every caller. The visitor tests that expected `200` on `HEAD` now expect
  `405`. `POST`, `PUT`, `PATCH` and `DELETE` are still refused for a
  visitor in the route guard.
- **Other routes.** Only `/api/v1/auth/me` lists `VisitorMethods`; every
  other route, including the ones upstream moved under catalog credential
  rules, still refuses a visitor in the route guard before its handler.

## Startup refusal tests

- 2026-10-15: `startNewWithRouteTable` (used by `TestNewRefusesInvalidVisitorRoute`
  and `TestNewRefusesVisitorMethodNotServedByPattern`) now requires New to
  refuse with exactly the `validateVisitorRoutes` error for the table, and
  turns a panic in New into a test failure. Without this, removing the
  visitor route check from New let New run on and panic on the nil store,
  which no assertion reported. Test-only change.
- 2026-10-17: two full-handler tests on `/api/v1/auth/me` for visitors that
  the email grant tests do not cover: a visitor whose account a domain rule
  created (`TestVisitorRoute_AuthMeAdmitsRuleVisitorOnReadMethodsOnly`) and
  a peer hub user access credential bound by a rule
  (`TestFederationPeerUserToken_RuleVisitorAuthMeAdmitsReadMethodsOnly`).
  Each checks the request identity is the visitor's federated account, that
  GET and HEAD answer 200, and that POST and a route with no
  VisitorMethods answer 403 with the uniform refusal. Test-only change.
