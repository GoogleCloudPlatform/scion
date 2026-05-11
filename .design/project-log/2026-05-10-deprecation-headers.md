# Project Log - Add Deprecation Headers to Grove Endpoints

## Task Overview
Added deprecation headers to the old `/api/v1/groves/` endpoints to inform clients to migrate to `/api/v1/projects/`. Also updated the `hubclient` to detect these headers and log a warning during fallback.

## Changes

### pkg/hub/server.go
- Implemented `deprecateGroveEndpoint(h http.HandlerFunc) http.HandlerFunc` middleware.
- This middleware sets the following headers:
    - `Deprecation: true`
    - `Sunset: Sun, 01 Nov 2026 00:00:00 GMT`
    - `Link: </api/v1/projects/>; rel="successor-version"`
- Wrapped the following route registrations with the middleware:
    - `/api/v1/groves`
    - `/api/v1/groves/register`
    - `/api/v1/groves/`

### pkg/hubclient/client.go
- Added `checkForDeprecation(resp *http.Response)` method that logs a `util.Debugf` warning if the `Deprecation` header is set to `true`.
- Updated fallback logic in `getWithQuery`, `post`, `put`, `patch`, and `delete` to call `checkForDeprecation` after falling back to the legacy grove path.

## Verification Results
- `go build ./...` passed.
- `pkg/hubclient` tests passed.
- `pkg/hub` tests failed with SQL errors unrelated to the changes (confirmed by reverting and re-running).
