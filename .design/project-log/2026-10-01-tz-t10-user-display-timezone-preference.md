# tz-refactor task 10: user display-timezone preference (backend)

**Branch:** `scion/tz-t10` · **PR:** https://github.com/ptone/scion/pull/2526 · **Issue:** ptone/scion#2503 (refs ptone/scion#2457)

## What changed

Phase one of the tz-refactor display-timezone vertical slice (design.md §0 D1, §3 A "Storage and API", AC6):

- Added `Timezone string` to all three `UserPreferences` structs (`pkg/ent/schema/types.go`, `pkg/store/models.go`, `pkg/hubclient/types.go`) and to the ent↔store converters in `pkg/store/entadapter/user_store.go`. Validated with `time.LoadLocation` plus a denylist of non-portable zoneinfo names (`Local`, `localtime`, `posixrules`, `Factory` — each resolves under `LoadLocation` but does not name a portable IANA zone); `""` means Auto.
- `PATCH /api/v1/users/{id}` now merges `preferences` per-key (decoding as `map[string]json.RawMessage`) instead of replacing the whole struct, so `PATCH {theme}` no longer clears an already-set `timezone`.
- Both `/auth/me` endpoints (web session, `pkg/hub/web.go`, and Hub API, `pkg/hub/handlers_auth.go`) now read the user live from the store on every request (no session caching) and include `preferences`.
- `listUsers`/`getUser` strip `preferences` from the response unless the caller is that same user or the capability set already computed for that user resource includes `update` (`user.update` — the same permission that already gates a cross-user PATCH). The first version called `IsUnscopedLocalPlatformAdmin` inline (tripped `TestBypassCensus`), then re-ran `Decide` per user (duplicate deny-audit noise, review round 1 R1-2); the final version reuses the `*Capabilities` each handler already computes for its own authorization pass.

## Why

task 11 (web `time.ts`, Display timezone card) builds directly on this field name, its validation, and the `/auth/me` response shape, so they are documented in the PR body as a frozen contract for that task.

## Test evidence

- New tests: `pkg/store/entadapter/user_store_timezone_test.go` (store round trip, runs against Postgres too under `enttest`'s `-tags integration` path), `pkg/hub/handlers_users_timezone_test.go` (PATCH per-key merge, validation, visibility), `pkg/hub/auth_me_timezone_test.go` (both `/auth/me` endpoints, live-read proof).
- Ran targeted packages (`pkg/hub`, `pkg/store/entadapter`, `pkg/hubclient`) under both `TZ=Asia/Tokyo` and `TZ=Asia/Kathmandu`. Tokyo: all green. Kathmandu: every test that goes through the full SQLite migration path (`testServer`/`createTestStore`, and `enttest.NewClient`) fails with a pre-existing baseline error — `empty agent role backfill: sql: Scan error on column ... create_time/created: unsupported Scan, storing driver.Value type string into type *time.Time` — confirmed to reproduce identically on unmodified `origin/main` (checked via a detached worktree, with `TestPromoteUser_CreatesRoleBinding` in `pkg/hub` and `TestUserStore_EmailCaseInsensitive` in `pkg/store/entadapter`). Not caused by this change; tz-em identified this as a known, tracked gap (task 2 scope).
- `make ci` (`GOFLAGS="-buildvcs=false -p=2"`): `fmt-check`, `lint`, `check-custom` all pass. `test-fast` passes for every package this PR touches (`pkg/hub`, `pkg/store`, `pkg/store/entadapter`, `pkg/hubclient`, `pkg/ent/entc`). It is red overall, but only from pre-existing failures in packages this PR does not modify at all (`cmd`: `TestHubAllOrOneActions`, `TestReincarnateHandoffTemplate_WorksAnywhere`; `pkg/config`: `TestLoadSettingsKoanfV1LegacyEnvNeverAdopted`; `pkg/harness`: `TestNativeTelemetryProvisionedChildEnv`). The `pkg/config` one is an ordinary `SCION_PROJECT_ID` leak: it passes with just that one var unset (`env -u SCION_PROJECT_ID go test ./pkg/config/ -run TestLoadSettingsKoanfV1LegacyEnvNeverAdopted`), confirmed on both this branch and unmodified `main`; not caused by this change, and not evidence of any leak path beyond `SCION_*`. The remaining `cmd`/`pkg/harness` failures were not re-diagnosed to the same level; left for the fork PR's real CI run to confirm clean.
- `go build -buildvcs=false -p 2 ./...` and `gofmt -l` on all changed files: clean.

## Follow-ups / adjacent notes

- None beyond the pre-existing, out-of-scope failures noted above.

## Upstream Gemini review (GoogleCloudPlatform/scion#2241)

After the fork PR (ptone/scion#2526) was accepted (review round 3, CLEAN) and handed upstream as GoogleCloudPlatform/scion#2241, `gemini-code-assist[bot]` left 2 comments:

- **G1** (`handlers_users_core.go` ~454, two halves):
  - **"reject unknown preference keys with 400"**: Declined. The published task-11 API contract (PR body, "Clearing" section) states unknown keys inside `preferences` are ignored (200, no change) — round 1 reviewed and accepted this, and it keeps older hubs compatible with newer clients that may send keys a given hub version does not yet recognize. Added an explicit `default:` case documenting the choice in code (previously implicit via the missing `default`).
  - **"avoid unnecessary DB writes / preferences initialization when no valid fields are present"**: **Real bug, fixed.** `prefsPatch = patch` was unconditional once the `"preferences"` key was present at all, even when the inner object was `{}`, `null`, or contained only unknown keys. That made `needsUpdate` true, which (a) forced a `tx.UpdateUser` write and, worse, (b) initialized `txUser.Preferences = &store.UserPreferences{}` for a user that previously had `nil` preferences — a real state change from a request that should be a no-op. Fixed by tracking `hasFields` across the per-key loop and only assigning `prefsPatch` when at least one recognized key was present. Verified with a failing-first test (`TestUpdateUser_Preferences_EmptyObjectIsNoop`, `TestUpdateUser_Preferences_UnknownKeysOnlyIsNoop`) that reproduced the bug before the fix and passes after.
  - Cross-user authz note: before the fix, a non-self `{"preferences":{}}` PATCH required `user.update` (403 without it) purely because `needsCrossUserUpdate` keyed off the now-removed unconditional `prefsPatch`. After the fix it is a true no-op and needs no permission (`TestUpdateUser_Preferences_EmptyObjectCrossUserNoAuthzRequired`), and `TestUpdateUser_Preferences_CrossUserPatchForbidden` (an actual value change) still asserts 403.
- **G2** (`handlers_users_core.go` ~221, nil `cap` before `capabilityAllows`): `capabilityAllows` already treats `cap == nil` as "no actions allowed" (`capabilities.go:369-372`), so no panic was possible. `cap` genuinely can be `nil` on this call site's `identity == nil` branch (`listUsers`/`getUser` only compute `Cap` `if identity != nil`), but every route reaching these handlers requires authentication at the hub's global auth middleware (confirmed empirically: an unauthenticated request gets `401 missing authorization header` before the handler runs), and `ComputeCapabilities`/`ComputeCapabilitiesBatch` never return a nil element for a non-nil identity — so this is not reachable on any currently live HTTP path. Added the one-line `cap != nil &&` guard anyway (zero risk, zero behavior change) because this codebase has a known history of a typed-nil identity reaching handler code despite a non-nil interface value (see `identity_typed_nil_test.go` and the "treat a typed-nil identity as missing" hardening), and a direct unit test, `TestStripPreferencesForViewer_NilCapDoesNotPanic`, pins the nil-cap behavior regardless of how it might be reached in the future.

**New commit:** see branch head after this round. Changed files: `pkg/hub/handlers_users_core.go` (the two fixes), `pkg/hub/handlers_users_timezone_test.go` (+5 tests), this log.

**Tests:** `TZ=Asia/Tokyo`, targeted `pkg/hub` selection (same as round 3): all green, including the 5 new tests. `TZ=Asia/Kathmandu`, same selection: the branch's failing set is exactly the pre-existing baseline set (54, confirmed by re-running the identical selection on unmodified `origin/main` at the merge-base `e761178b`) plus the same 15 `testServer`-backed preference tests that already failed this way before this round (12 from round 3, now 15 with the 3 new ones) — all with the identical known `create_time` Scan-error migration baseline, none a new failure mode. `TestStripPreferencesForViewer_NilCapDoesNotPanic` avoids the migrated store and passes under both zones. `go build -buildvcs=false -p 2 ./pkg/hub/... ./pkg/store/... ./pkg/hubclient/...`, `gofmt -l`, and `go vet` on the changed packages: clean. `GOGC=40 golangci-lint run --new-from-rev=e761178b --concurrency=1 ./pkg/hub/...`: 0 issues. Per the dev-common override for this round, `make ci`/`ci-full` were not run locally; fork/upstream CI covers the full suite.

## Review round 4

The PATCH 200 response applies the same preferences visibility rule as GET.

**Tests:** `TestUpdateUser_Preferences_EmptyObjectCrossUserNoAuthzRequired`, `TestUpdateUser_Preferences_ResponseVisibilityMatrix`.
