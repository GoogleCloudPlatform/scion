# Design: healthz-proxy-fix

**Author:** auth-arch-843  
**Date:** 2026-07-23  
**Branch:** scion/auth-arch-843  
**Scope:** XS — client-side only, no server changes  
**Investigator findings:** `/scion-volumes/scratchpad/projects/healthz-proxy-fix/findings.md`

---

## Problem & Goals

Google Cloud Run's Google Front End (GFE) intercepts `GET /healthz` before the
request reaches the Scion Hub server and returns `200 OK` with a plain-text body
(`"OK"`, `Content-Type: text/plain`). `pkg/hubclient/client.go Health()` only
falls back from `/healthz` to `/health` on HTTP 404; the GFE-intercepted 200
bypasses the fallback, `apiclient.DecodeResponse` fails to JSON-decode the
plain-text body, and the error is silently dropped at `cmd/hub.go:838`, causing
`scion hub status` to print `"Connection: failed"` with no explanation.

`pkg/brokerclient/client.go Health()` has no fallback at all and the same
decode failure mode.

**Goals**

1. Make `hubclient.Health()` succeed when deployed behind a GFE-style proxy
   that intercepts `/healthz`.
2. Make `brokerclient.Health()` produce a clear, actionable error when the same
   proxy pattern occurs (no second endpoint to fall back to).
3. Improve user-visible error messages so a proxy interception is diagnosable
   without reading source code.
4. Add tests that cover the previously-untested fallback paths.

---

## Non-Goals

- Server-side changes: the hub web server already registers `/healthz` and
  `/health` to the same handler. No server changes are needed or in scope.
- Changing `apiclient.DecodeResponse`: the shared helper is correct for its
  general purpose. Health-check resilience belongs at the call site, not in the
  generic decoder.
- Adding a `/readyz` fallback: the investigator confirmed GFE intercepts the
  canonical `/healthz` path only; a `/readyz` fallback adds complexity for zero
  real-world benefit (see Alternatives § 3).
- Fixing every misleading caller message (e.g. all uses of `"hub is not
  responding"`): that is a broader UX cleanup. Only the most prominent path
  (`cmd/hub.go:838`) is addressed here; the rest are noted as a follow-up.

---

## Proposed Design

### Core principle

The root problem is that GFE returns a *semantically misleading* response: `200
OK` with non-JSON content. The right detection point is the `Content-Type`
response header — not a decode attempt. Checking the header is more decisive
(no wasted parse) and clearly distinguishes the proxy-interception case from
a genuine JSON parse failure on the real server.

### 1. Hub client — `pkg/hubclient/client.go`

#### Fallback trigger: keep 404, add Content-Type check

```go
// isProxyIntercepted reports whether a 2xx response looks like a proxy
// intercept (non-JSON content type). GFE on Cloud Run returns "text/plain".
func isProxyIntercepted(resp *http.Response) bool {
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return false
    }
    ct := resp.Header.Get("Content-Type")
    // If Content-Type is absent, pass through to DecodeResponse (which will
    // succeed if the body is valid JSON, or fail with its own error).
    return ct != "" && !strings.HasPrefix(ct, "application/json")
}

func (c *client) Health(ctx context.Context) (*HealthResponse, error) {
    resp, err := c.get(ctx, "/healthz", nil)
    if err != nil {
        return nil, err
    }
    if resp.StatusCode == 404 || isProxyIntercepted(resp) {
        _ = resp.Body.Close()
        resp, err = c.get(ctx, "/health", nil)
        if err != nil {
            return nil, err
        }
    }
    return apiclient.DecodeResponse[HealthResponse](resp)
}
```

**Decision: trigger order**  
Check `StatusCode == 404` first (existing semantics, documented behavior),
then `isProxyIntercepted`. Both close the body and retry `/health` before
calling `DecodeResponse`. Order is immaterial for correctness but 404 first
preserves reading intent.

**Decision: Content-Type, not decode-error, as the fallback trigger**  
See Alternatives § 1. Summary: decode-error fallback triggers too late (after
a wasted parse), risks masking real decode errors on the `/health` endpoint
itself, and provides no earlier signal than Content-Type checking does.

**Decision: no decode-error-triggered fallback as secondary catch**  
If `/health` also returns non-JSON content, `DecodeResponse` will fail and the
error will propagate. That is correct behavior (both endpoints are broken) and
will be wrapped with a proxy-hint message (see § 3 below).

**Decision: absent Content-Type passes through**  
If `Content-Type` is not set at all, the code falls through to `DecodeResponse`.
If the body is valid JSON the call succeeds; if not, a decode error is returned.
This avoids false-positive fallbacks against servers that omit the header but
return JSON.

**`isProxyIntercepted` placement:** Inline in `hubclient` package as an
unexported function. The broker client needs analogous logic but broker's
remediation is different (no fallback), so sharing the function via `apiclient`
would create coupling for a trivial predicate.

---

### 2. Broker client — `pkg/brokerclient/client.go`

The broker server registers only `/healthz` (not `/health`), so there is no
second endpoint to fall back to. The fix is early detection + clear error.

```go
func (c *client) Health(ctx context.Context) (*runtimebroker.HealthResponse, error) {
    resp, err := c.transport.Get(ctx, "/healthz", nil)
    if err != nil {
        return nil, err
    }
    ct := resp.Header.Get("Content-Type")
    if ct != "" && !strings.HasPrefix(ct, "application/json") {
        _ = resp.Body.Close()
        return nil, fmt.Errorf(
            "broker health endpoint returned %s (Content-Type: %s); "+
                "this may indicate a reverse proxy (e.g. Google Cloud Run GFE) "+
                "is intercepting /healthz — check your deployment configuration",
            resp.Status, ct,
        )
    }
    return apiclient.DecodeResponse[runtimebroker.HealthResponse](resp)
}
```

**Broker deployment priority:** The brief asks whether the broker runs behind
a GFE-style proxy. Based on the codebase (Cloud Run mentions in
`runtimebroker/server.go:1789`, stateless broker control channels), the broker
*can* run on Cloud Run. The fix is low-effort here (a few lines) and prevents
a confusing decode error if it does. Recommend including it in this PR.

---

### 3. Error message clarity — `cmd/hub.go` and `pkg/hubsync/sync.go`

**In-scope for this PR: the primary user-visible path.**

`cmd/hub.go:838` currently silences the Health() error with `health, _ =
client.Health(ctx)`. When `health == nil`, line 905 prints `"Connection:
failed"` with no explanation. Proposed change:

```go
var healthErr error
health, healthErr = client.Health(ctx)
// ... (later, in the display block)
if health == nil {
    msg := "Connection: failed"
    if healthErr != nil {
        msg = fmt.Sprintf("Connection: failed (%s)", healthErr)
    }
    fmt.Println(msg)
}
```

The error from `Health()` after this fix will be either:
- A decode error from `DecodeResponse` (rare — would mean both endpoints broken)
- The network-level error from `c.get`

When both endpoints fail, `DecodeResponse` returns `"failed to decode response:
..."`. To make this more actionable, add a sentinel check in the display path
(not in `wrapHubError`, which is `hubsync`-specific):

```go
// hintProxyError wraps a Health() error with a proxy-interception hint
// when the error pattern suggests a non-JSON 200 from both health paths.
func hintProxyError(err error) error {
    if err == nil {
        return nil
    }
    msg := err.Error()
    if strings.Contains(msg, "failed to decode response") {
        return fmt.Errorf("%w\n(Hint: a reverse proxy may be intercepting "+
            "/healthz and /health — check your Cloud Run or GFE configuration)", err)
    }
    return err
}
```

Called at the `cmd/hub.go:838` display site and at the `pkg/hubsync` callers
that use `wrapHubError("hub at %s is not responding: %w")`. The `wrapHubError`
function itself need not change — the hint is added when building the error
message passed to it.

**Not in scope for this PR:** Fixing every caller that says `"hub is not
responding"`. After the fallback fix, those callers will only see that string
for genuine connectivity failures (network unreachable, timeouts), where it is
accurate. A follow-up UX pass can relabel them. Noted in Open Questions.

---

### 4. Summary of file changes

| File | Change |
|------|--------|
| `pkg/hubclient/client.go` | Add `isProxyIntercepted()`, update `Health()` fallback logic |
| `pkg/brokerclient/client.go` | Add Content-Type check + descriptive error in `Health()` |
| `cmd/hub.go` | Capture `healthErr` at line 838; call `hintProxyError` in display |
| `pkg/hubsync/sync.go` | Call `hintProxyError` when wrapping Health() errors (lines 296–297) |
| `pkg/hubclient/client_test.go` | New test cases (see Test Plan) |
| `pkg/brokerclient/client_test.go` | New test cases (see Test Plan) |

---

## Alternatives Considered

### 1. Decode-error as fallback trigger instead of Content-Type check

`Health()` could attempt `DecodeResponse`, and on JSON parse failure, retry
`/health`. This is what the investigator's "recommended approach" note gestured
toward.

**Rejected because:**
- It wastes the parse attempt on a body that is provably wrong before reading it.
- It triggers a `/health` retry for *any* decode failure, including a malformed
  JSON response from the real hub server. A retry does not fix malformed JSON;
  it just adds a second failing request and makes debugging harder.
- If `/health` is also returning non-JSON for some reason, the retry gives no
  additional diagnostic signal — the second decode error is equally cryptic.
- Content-Type is the correct layer for this signal. Proxies declare what they
  return; we should read that declaration.

**Kept as defense-in-depth:** A decode-error fallback is *not* added as a
secondary catch. Errors from `DecodeResponse` on the `/health` response (after
a correct Content-Type fallback) are a separate issue and should surface clearly.

### 2. Fix in `apiclient.DecodeResponse` — add Content-Type guard

Adding a `Content-Type: application/json` check to the shared `DecodeResponse`
function would make all clients (not just health) resilient to proxy
interception.

**Rejected because:**
- It conflates two concerns: the shared decoder correctly handles what it is
  given. Deciding *whether to retry a different endpoint* is caller-specific
  knowledge (hub has `/health`; broker does not; most other endpoints have no
  alternate URL at all).
- A generic "bail on non-JSON content-type" would change behaviour for any
  caller that legitimately sends a request expected to return JSON but against
  a server that omits the Content-Type header (valid in practice).
- This approach would not fix the broker client, where the issue is the error
  message, not the retry logic.

### 3. Adding `/readyz` to the fallback chain

The hub API server registers `/readyz` at `server.go:2667`. A triple fallback
`/healthz` → `/health` → `/readyz` could theoretically help if a proxy
intercepts both `/healthz` and `/health`.

**Rejected because:**
- GFE intercepts the canonical health path (`/healthz`) specifically; there is
  no evidence it also intercepts `/health` or `/readyz`.
- The hub web server (the public-facing endpoint that Cloud Run exposes) does
  not register `/readyz`, only the hub API server does. Clients that cannot
  reach `/healthz` are almost certainly talking to the web server.
- Three fallback hops for a health check is overengineering; two is already a
  concession to deployment reality.

### 4. Server-side: remove `/healthz`, use only `/health`

Force GFE to intercept a path that doesn't conflict.

**Rejected because:** Not all deployments run on Cloud Run. Removing `/healthz`
would break clients and health check configurations that depend on it. Server is
explicitly out of scope per investigator findings.

---

## Migration / Rollout

This is a purely additive client-side change. The fallback to `/health` already
exists for the 404 case; this PR adds a second trigger for the non-JSON-200
case. No config changes, no database migrations, no API version bumps.

**Forward compatibility:** Servers that return proper JSON on `/healthz` are
unaffected; the Content-Type check is only hit when CT is present and non-JSON.

**Backward compatibility:** The `/health` path has been server-side equivalent
to `/healthz` since the hub web server was introduced. Falling back to it is
safe for all existing hub deployments. For broker deployments, there is no
fallback, only a better error message — existing behavior (opaque decode error)
is strictly improved.

**Risk:** Negligible. The only behavior change in the success path is that
clients behind GFE now succeed where they previously failed. The error path
improves the message fidelity.

---

## Open Questions

1. **Other GFE-intercepted paths (non-health):** Are there other endpoints
   (e.g., `/api/v1/...`) that GFE might intercept? If so, a more general
   approach in `apiclient` may be warranted. This is a monitoring question, not
   a blocker for this fix. Recommend filing a follow-up issue.

2. **Broker on Cloud Run confirmation:** The brief asks whether the broker is
   ever deployed on Cloud Run. The brokerclient fix is still recommended (it's
   low-effort and defensive), but explicit confirmation would let us close the
   question. No decision required before implementation.

3. **Broader "hub is not responding" message cleanup:** After this fix, the
   message is only ever shown for genuine network failures — which is accurate.
   But the phrasing is still misleading for cases like auth failures. A broader
   UX cleanup of all `wrapHubError` callers is left to a follow-up.

4. **`cmd/hub.go:838` silent discard:** The brief did not ask for a detailed UX
   redesign of `scion hub status`. The minimal change (capture + display the
   error) is proposed. A richer status display (e.g., distinguishing "proxy
   detected" from "network unreachable") is a follow-up.

---

## Test Plan

All tests use `httptest.NewServer` with a handler that switches behavior by
request path, matching existing test style in `pkg/hubclient/client_test.go`.

### `pkg/hubclient/client_test.go`

| Test name | Setup | Expected outcome |
|-----------|-------|-----------------|
| `TestHealth_GFEInterception_FallsBackToHealth` | `/healthz` → `200 text/plain "OK"`; `/health` → `200 application/json {status:"ok"}` | Returns `HealthResponse{Status:"ok"}`, nil error |
| `TestHealth_404_FallsBackToHealth` | `/healthz` → `404`; `/health` → `200 application/json {status:"ok"}` | Returns `HealthResponse{Status:"ok"}`, nil error (existing fallback, first test coverage) |
| `TestHealth_HappyPath` | `/healthz` → `200 application/json` | Succeeds, no fallback attempted (existing test already covers this; keep it) |
| `TestHealth_BothEndpointsIntercepted` | `/healthz` → `200 text/plain`; `/health` → `200 text/plain` | Returns non-nil error containing "failed to decode response" |
| `TestHealth_AbsentContentType_JSON` | `/healthz` → `200`, no Content-Type, valid JSON body | Succeeds (passes through to decoder) |
| `TestHealth_AbsentContentType_NonJSON` | `/healthz` → `200`, no Content-Type, plain-text body | Returns decode error (not a fallback — CT absent, so no isProxyIntercepted trigger) |

### `pkg/brokerclient/client_test.go`

| Test name | Setup | Expected outcome |
|-----------|-------|-----------------|
| `TestBrokerHealth_HappyPath` | `/healthz` → `200 application/json` | Returns `HealthResponse`, nil error |
| `TestBrokerHealth_GFEInterception` | `/healthz` → `200 text/plain "OK"` | Returns nil, error containing "reverse proxy" or "GFE" |
| `TestBrokerHealth_ServerError` | `/healthz` → `503` | Returns nil, APIError with status 503 |

---

## Implementation Phases

Given XS scope this is a single developer phase with four logical commits.

**Phase 1** (single PR): Fix + tests, all in one branch `scion/healthz-proxy-fix`

| Commit | Files | Description |
|--------|-------|-------------|
| 1 | `pkg/hubclient/client.go` | Add `isProxyIntercepted()`, update `Health()` to trigger fallback on non-JSON CT in addition to 404 |
| 2 | `pkg/brokerclient/client.go` | Add CT check in `Health()`, return descriptive proxy-hint error |
| 3 | `cmd/hub.go`, `pkg/hubsync/sync.go` | Capture and display `healthErr` at primary display site; add `hintProxyError` wrapper |
| 4 | `pkg/hubclient/client_test.go`, `pkg/brokerclient/client_test.go` | Add all new test cases from test plan |

Commit 4 can be combined with its corresponding source commit (1+4 together,
2+4 together) if the developer prefers test-alongside-change.

Commit order is not load-bearing — each commit compiles independently and the
tests in commit 4 will fail against the old code, which is the correct
test-driven signal.

---

## Acceptance Criteria

The following must hold before this is considered done. QA tester should verify:

1. **Happy path unchanged:** `scion hub status` against a correctly-deployed hub
   (not behind GFE) still shows `Connection: ok` and hub version/status fields.

2. **GFE interception fixed:** `scion hub status` against a Cloud Run hub where
   `/healthz` is intercepted by GFE shows `Connection: ok` (falling back to
   `/health` successfully), not `Connection: failed`.

3. **GFE interception message improved:** If *both* `/healthz` and `/health` are
   broken (e.g., health check disabled entirely), the error message includes a
   hint about reverse proxy interception rather than the opaque
   `"failed to decode response: invalid character..."` text.

4. **Broker error clarity:** Calling broker status against a GFE-intercepted
   broker produces an error message containing language about "reverse proxy" or
   "GFE" rather than a raw JSON decode error.

5. **Tests pass:** `go test ./pkg/hubclient/... ./pkg/brokerclient/...` passes
   with all new test cases (listed in Test Plan) green. The two previously-
   untested fallback paths (`TestHealth_404_FallsBackToHealth`,
   `TestHealth_GFEInterception_FallsBackToHealth`) must be present and passing.

6. **No regression in existing tests:** Full `go test ./...` passes.

7. **Design doc committed:** `.design/healthz-proxy-fix.md` is present in the
   PR diff.
