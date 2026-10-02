# Audit update milestone 1 / #2405 shared-auth normalization

## Scope and threat boundary

The live access-constraint audit endpoint must not reveal whether a constraint
exists through authentication failures. Production unified authentication and
development authentication still execute their existing credential extraction,
validation, and context construction. A missing, malformed, rejected, expired,
or suspended credential never reaches downstream middleware or the endpoint.
Only the outward 401/403 response from those authentication paths is replaced
with the endpoint's canonical `Access Constraint not found` response.

Successful authentication unwraps the response normalizer before calling the
next middleware or handler. This preserves the authenticated request context
and ensures a downstream 401/403 is not rewritten as an authentication failure.
Non-authentication failures such as credential-store unavailability also retain
their existing status and body.

## Exact route matcher

`isConstraintAuditAuthFailureRoute` matches only case-sensitive
`GET /api/v1/admin/access-constraints/{id}/audit`, where `{id}` is one non-empty
path segment and is neither `.` nor `..`. Query parameters are ignored. The
matcher rejects every other method, empty IDs, extra or repeated segments,
prefixes, suffixes, trailing slashes, case variants, dot segments, and any
encoded path representation (including encoded and double-encoded slashes).

The matcher is shared by `UnifiedAuthMiddleware` and `DevAuthMiddleware`; it is
not an unauthenticated-route exception and does not alter credential parsing,
identity types, permissions, route metadata, or handler authorization.

## Regression coverage

Focused middleware tests pin:

- canonical 404 status, headers/content type, and body equivalence for missing,
  malformed, invalid, and expired credentials;
- no downstream handler invocation after failed authentication;
- valid dev credentials and identity/context propagation through both auth
  implementations;
- unchanged downstream responses after valid authentication;
- exact query-insensitive route matching and rejection of neighboring routes,
  methods, encoded variants, extra segments, prefixes, suffixes, case variants,
  and path-cleaning forms.

The focused endpoint privacy test now exercises a credential-less request
through the full production middleware chain and compares it with the validly
authenticated absent-resource response. Existing handler tests continue to pin
live-resource lookup, scoped `hub.audit.read` authorization, wrong-scope denial,
deleted-resource behavior, pagination, and store failures.

## Residual risk

This normalization is intentionally coupled to the canonical route literal and
the endpoint's `NotFound(..., "Access Constraint")` envelope. A future route
rename or not-found contract change must update both the matcher and equivalence
tests. Authentication implementations added outside the two covered middleware
paths must independently preserve the same anti-enumeration contract before
serving this route.
