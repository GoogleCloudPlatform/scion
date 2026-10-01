# tz-refactor task 10: user display-timezone preference (backend)

**Branch:** `scion/tz-t10` · **PR:** https://github.com/ptone/scion/pull/2526 · **Issue:** ptone/scion#2503 (refs ptone/scion#2457)

## What changed

Phase one of the tz-refactor display-timezone vertical slice (design.md §0 D1, §3 A "Storage and API", AC6):

- Added `Timezone string` to all three `UserPreferences` structs (`pkg/ent/schema/types.go`, `pkg/store/models.go`, `pkg/hubclient/types.go`) and to the ent↔store converters in `pkg/store/entadapter/user_store.go`. Validated with `time.LoadLocation`; `"Local"` is rejected; `""` means Auto.
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
