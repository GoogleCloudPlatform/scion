# Task 25: Fix DiscoverLinkLocalAddress Multi-Address Handling

**Date:** 2026-08-26
**Branch:** `scion/dev-linklocal-fix`
**Status:** Complete

## Problem

`DiscoverLinkLocalAddress()` in `pkg/sciontool/metadata/server.go` returned an
error when multiple IPv4 link-local addresses were found on the host. Cloud Run
Instances always have three link-local addresses (e.g. 169.254.8.1,
169.254.9.1, 169.254.169.1), so auto-discovery always failed on the target
platform. This meant `deploy_instance.go` could never set
`SCION_METADATA_BIND_ADDRESS` automatically, breaking every deployed Instance
out of the box.

## Root Cause

The `default:` branch of the `switch len(found)` statement unconditionally
returned an error for `len(found) > 1`, treating multiple addresses as
ambiguous. In practice, all link-local addresses on the launcher are equivalent
(all return HTTP 200 from sandboxes against 0.0.0.0), so the function refused
to choose when every choice was correct.

## Fix

- Changed the `default:` case to sort the found addresses lexicographically
  (`sort.Strings`) and return `found[0]` (the lowest), making the selection
  deterministic and stable across restarts.
- Added a log line indicating which address was selected and that alternatives
  existed.
- Extracted the selection logic into a separate `selectLinkLocalAddress()`
  function for testability.
- Updated the function's doc comment to reflect the new behavior.
- Preserved the zero-match error case unchanged.

## Files Changed

- `pkg/sciontool/metadata/server.go` — fix + `"sort"` import
- `pkg/sciontool/metadata/server_test.go` — 4 new test cases

## Verification

- `go build ./...` — pass
- `go test ./pkg/sciontool/metadata/...` — pass
- `golangci-lint run ./pkg/sciontool/metadata/...` — 0 issues
- `make fmt-check` — pass
