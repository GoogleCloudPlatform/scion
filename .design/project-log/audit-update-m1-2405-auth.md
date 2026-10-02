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

Review round 1 found that unified authentication unwrapped the normalizer when
it delegated a broker request, before `BrokerAuthMiddleware` or
`AuditableBrokerAuthMiddleware` had validated the HMAC signature and optional
on-behalf-of identity. The corrected handoff keeps the normalizer active through
both broker middleware variants. Each variant now unwraps only after broker HMAC
and OBO authentication succeeds, immediately before calling valid downstream
code. Broker/OBO authentication rejections (400/401/403) use the canonical 404;
infrastructure 5xx responses remain unchanged.

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
- forged broker HMAC, unsupported/unknown/suspended OBO rejection, and valid
  broker/OBO context through full unified-plus-broker chains for both ordinary
  and auditable middleware;
- original response-writer identity and unchanged downstream 401/403/404 after
  successful broker/OBO authentication;
- preservation of auditable broker failure/success events without exposing
  their details in rejected HTTP responses;
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

## Review round 1 verification

- Focused normal broker/direct-auth regressions with `go test -count=1 -p 2
  ./pkg/hub` and an exact test-name filter — PASS.
- The same focused broker/direct-auth regressions with `go test -race -count=1
  -p 2 ./pkg/hub` — PASS.
- `go vet -p 2 ./pkg/hub` — PASS.
- `go build -buildvcs=false -p 2 ./pkg/hub` — PASS.
- `golangci-lint run --new-from-rev=9e15dd892fa341dce5f9b795606a89b52cc03717
  --concurrency=1 ./pkg/hub/...`, with `GOGC=40` and a hard 10-minute timeout —
  PASS with `0 issues` inside the bound.
- `gofmt` over the changed Go files and `git diff --check
  9e15dd892fa341dce5f9b795606a89b52cc03717..HEAD` — PASS.

`make ci` and `make ci-full` were not run because the campaign workload rule
prohibits them; the bounded targeted gates above were used instead.
