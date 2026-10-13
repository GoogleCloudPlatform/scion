# Visitor route allow-list in the route guard (phase 1, item 4)

**Date:** 2026-10-13
**Stacked on:** project visitor grants by email and first contact
**Tracked in:** ptone/scion#4603

## What changed

- **Route metadata.** `RouteMetadata.VisitorAllowed` (bool, default false).
  `routeGuard` admits a request whose identity is a `VisitorIdentity` only
  on a route that sets it (`refuseVisitorRequest(w, r, meta)`); every other
  route, whatever its classification, answers `403` with
  `visitorRouteRefusalMessage` before the handler runs. An admitted visitor
  then passes the route's classification guard like any identity. Requests
  with any other identity, including an unlinked `FederatedUserIdentity`,
  are not affected.
- **Startup check.** `validateVisitorAllowedRoutes` (route_metadata_check.go)
  accepts the flag only on `authenticated` and `policy` routes; `New`
  refuses to start otherwise, as it does for an invalid session-only route.
- **Flagged routes.** Only `/api/v1/auth/me`: `handleAuthMe` returns the
  caller's own ID, email (the issuer-verified email for a visitor), display
  name, role (`viewer`) and preferences, and writes nothing for any method.

## Decisions

- The design names `GET /api/v1/users/me`; no such route exists.
  `/api/v1/users/{id}` (`handleUserByID`) also serves `PATCH`, `DELETE` and
  `revoke-sessions`, so it is not flagged. `/api/v1/auth/me` is the own
  profile route, and the one the design's bridge reads for the visitor's
  local ID.
- The flag is per pattern and admits every method the pattern serves (most
  patterns carry no method). Its doc comment says to set it only where the
  handler is safe for a visitor on every method.
- A route listing the visitor's own grants does not exist. Adding it means
  a new route, catalog operation and inventory entries, so it is left to
  the messaging item.

## Tests

- `TestRouteGuard_AdmitsVisitorOnlyOnVisitorAllowedRoutes`: every route in
  `routeMetadataTable` (kept equal to the registered set by
  `TestRegisteredRoutesHaveRouteMetadata`) and method refuses a visitor
  with the uniform 403 unless the route is in `expectedVisitorAllowedRoutes`;
  the set of flagged routes must equal that list.
- `TestRouteGuard_ProfileWriteRoutesRefuseVisitor`: user-scope env and
  secrets, user templates, injected skills, chat preferences, presence,
  attachments, the user record, user access credentials, delegations and
  the terminal workspace are registered and refuse a visitor.
- `TestRouteGuard_VisitorRefusedUnlessRouteSetsVisitorAllowed`: the
  default refuses on all eight classifications; the flag admits.
- `TestRouteGuard_VisitorRefusalLeavesOtherIdentities`: an unlinked
  federated user never receives the visitor refusal on any route.
- End to end (project_visitor_grants_test.go): a linked visitor reads its
  own profile on `/api/v1/auth/me` and is refused on `/api/v1/users/me/groups`;
  with `allow_visitor_grants` off it is refused on `/api/v1/auth/me` too;
  an unlinked federated user gets identical responses with the experiment
  on and off.
- `TestValidateVisitorAllowedRoutes`, `TestRouteMetadataTableVisitorAllowedRoutesAreValid`,
  `TestNewRefusesInvalidVisitorAllowedRoute`.

## Follow-ups

- Messaging item: make the message, DM read and agent list/read handlers
  visitor-safe, then flag them (several sit under catch-all patterns such as
  `/api/v1/projects/`, so admission there needs a sub-route check, like the
  delegated-credential admission in agent_delegation_routes.go, rather than
  the per-pattern flag alone).
- A `GET` of the visitor's own grants.
