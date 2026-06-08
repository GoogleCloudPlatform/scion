# M2 Lifecycle Hooks Port — Validation Library + Untrusted-Variable Guard

**Agent:** lh-port-m2  
**Date:** 2026-06-08  
**Branch:** scion/lifecycle-hooks-port  
**Commit:** 76c0c3bc  

## What was ported

4 files in `pkg/lifecyclehooks/`:

1. **validate.go** — Hook validation: triggers (running/suspended/stopped/error), action types (http/webhook), HTTP methods, URL well-formedness, headers (RFC 7230 token check), timeout (max 30s), on_error normalization, execution_identity resolution (GCP SA scope/verification). S2 security rule: http action type requires https:// scheme.

2. **varguard.go** — Untrusted-variable guard (security-critical). Trust classification (trusted vs untrusted vars), static validation at create/update time (untrusted vars forbidden in URL host/path/query and all headers, allowed only in body via AllowedUntrustedVars + must be inside JSON string literal), and runtime renderer with defense-in-depth (untrusted vars blanked in headers/URL path, JSON-encoded in body, percent-encoded in query, CR/LF stripped from all header values).

3. **validate_test.go** — Tests for hook validation: triggers, action types, methods, webhooks, URL, timeout, execution identity, header injection, on_error, nil action, S2 https rule, IsValidationError.

4. **varguard_test.go** — Tests for variable guard: ClassifyVar, SSRF/path injection, auth header injection, non-auth header injection, header name injection, body allow-list, body positional safety, cookie/set-cookie headers, render-time encoding (URL params, JSON body, headers, CR/LF sanitization, unresolved vars), end-to-end validate+render, extractVars, jsonEncodeValue, isInsideJSONString, renderTrustedSubstitution defense-in-depth.

## Test results

- 35 test cases, all passing
- `go build ./...` clean
- `go test ./pkg/lifecyclehooks/...` green

## Deviations from reference

None. Code is identical to the reference branch. Import path `github.com/GoogleCloudPlatform/scion/pkg/store` works unchanged — all required types and constants were present after M1.
