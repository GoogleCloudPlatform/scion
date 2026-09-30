# Experiments Phase 1a-ii — Go API and authorization wiring (ptone/scion#2217)

Branch: `scion/experiments-1a-ii`, based on `scion/experiments-1a-i` (fork PR
ptone/scion#2276).

## Scope

Both HTTP endpoints and their authorization wiring, on top of the 1a-i Go
core (registry, opsettings section, resolution, `requireExperiment`). No
change to `pkg/hub/web.go` and no web changes.

- `GET /api/v1/experiments` (`pkg/hub/handlers_experiments.go`),
  `RouteAuthenticated`, plus its authzop catalog entry.
- `GET|PUT|DELETE /api/v1/admin/experiments`
  (`pkg/hub/admin_experiments.go`), with the compare-and-set write algorithm,
  the malformed-row policy, name validation, retired-name pruning, and
  preservation of pattern-valid names this binary does not know.
- Route metadata, the `hub.experiments.update` permission (permissions
  registry and authzop catalog), and route-classification-table entries.
- Generic per-section reset (`DELETE
  /api/v1/admin/server-config/sections/{name}`) now rejects
  `name == "experiments"` before any store call, with a test.
- Regenerated `.design/authorization-operation-catalog.md`.

No changes to 1a-i code were needed for the initial submission. One followed
from review: `pkg/experiments/registry.go` gained `Experiment.ReviewOverdue(now
time.Time) bool`, replacing a duplicated layout constant and function that had
lived in `pkg/hub/admin_experiments.go` and had an off-by-one (it treated the
`ReviewBy` day itself as already overdue). Giving it a home next to
`HasLayer`, in the package that owns `Experiment` and its `reviewByLayout`
constant, was the natural fix and removes the duplication at the same time.

## Decisions / notes for reviewers

- **Error codes are literal strings, not the shared `ErrCodeXxx`
  constants.** `revision_conflict`, `experiments_malformed`,
  `settings_unavailable`, and `validation_failed` are passed directly to
  `writeError` as the `code` argument, matching the precedent in
  `project_messaging_policy.go` and the generic-reset-rejection wording.
  This keeps the response codes exactly as specified rather than mapping
  them onto the closest existing `ErrCodeXxx`, which would have picked
  different strings.
- **`unknown_overrides` also excludes pattern-invalid keys defensively.**
  Such a key should never reach storage (PUT drops it with a warning), but
  the GET/PUT/DELETE response builder filters it out anyway rather than
  assuming the store can't be written to directly outside the handler.
- **PUT request decoding uses `map[string]*bool` for `overrides`.** A JSON
  `null` decodes to a nil pointer while an omitted name is simply absent
  from the map, so presence-vs-null is distinguished by the map itself
  without a second presence-parsing pass over that sub-object. A
  non-boolean value (e.g. a string) fails JSON decoding for the whole
  request body, which is surfaced as a 400 the same as any other malformed
  body.
- **Response attribution after a write.** `Update()` refreshes the writing
  replica's cache synchronously, so the post-write response normally reads
  `updated_at`/`updated_by` from a fresh `ExperimentsSnapshot()`. If that
  snapshot's revision doesn't match the revision the write just produced
  (a later refresh landed in between), the response falls back to the
  caller's identity and the current time, per the design's tie-break rule.
- **Audit logging.** One `slog.Info` per name the request actually changed
  (comparing the merged result against the pre-write authoritative read),
  and one `slog.Warn` for a reset-all, plus a `slog.Warn` when a
  pattern-invalid key is dropped from a stored document during a PUT.

## Testing notes (full-chain infrastructure)

- All full-chain tests live in `pkg/hub/experiments_api_test.go` and run
  through `srv.Handler()` with a real sqlite-backed store, following the
  `testServer`/`doRequest`/`doRequestAsUser` conventions already used
  elsewhere in the package. `pkg/hub.New()` does not itself wire
  `OperationalSettings` (that happens in `cmd/server_foreground.go` in
  production); the test helper `testServerWithOps` replicates that wiring
  so `GetOperationalSettings()` is non-nil, matching how every driver is
  wired in production.
- **The real store rejects genuinely invalid JSON at write time** (ent's
  sqlgraph layer runs `encoding/json.Marshal` over the raw value before
  persisting it, which fails for non-JSON bytes). A malformed row can
  therefore not be seeded through `UpsertHubSetting`. The malformed-row
  tests instead use a `malformedSectionStore` that wraps the real store and
  overrides only the read paths (`GetHubSetting`, `ListHubSettings`) for
  one section, so the handler chain is exercised against a malformed
  document without asking the real store to hold invalid JSON. Its fake
  revision is `0`, matching the underlying store's true "absent" state, so
  the recovery write (`DELETE {confirm_reset_malformed:true}`) lands as a
  real create against the store.
- **The store-race test** (`TestHandleAdminExperiments_StoreRace`) uses a
  `failingUpsertStore` wrapper whose `UpsertHubSetting` unconditionally
  returns `store.ErrRevisionConflict` for the `experiments` section,
  independent of whether a real revision mismatch occurred, to prove the
  handler maps that error to 409 rather than 500.
- **Both store wrappers forward `DB() *sql.DB`** to the wrapped store when
  it implements that interface. `New()`'s D4 membership-index migration
  type-asserts the store for raw DB access and fails closed otherwise;
  without the forwarding method, wrapping the store for either test above
  broke server construction entirely.
- **The stale-replica test** writes directly through the real store
  (bypassing the server's own `OperationalSettings` cache) to simulate a
  second replica's write, then asserts that a PUT using the store's true
  revision succeeds while one using the server's stale cached revision
  gets 409 — proving the write path reads authoritatively rather than
  trusting the cache.
- Added one HA-propagation case,
  `TestSubscription_ExperimentsPropagatedAcrossReplicas` in
  `pkg/hub/operational_settings_propagation_test.go`, following the
  existing messaging/maintenance propagation tests in that file: it proves
  the new section rides the existing LISTEN/NOTIFY-style propagation path
  with no section-specific code of its own.
- **Error-code assertions found a real shape bug.** Decoding the standard
  `{"error":{"code":...}}` envelope in the conflict/malformed/validation
  test cases (rather than asserting on HTTP status alone) turned up one
  response that didn't carry a `code` field at all: the `opsettings.Validate`
  failure branch in the PUT handler had copied `admin_messaging.go`'s flat
  `{"error":"validation_failed","errors":[...]}` shape, which has no nested
  `code`. It now goes through `writeError` like every other response in this
  handler. This branch is close to unreachable in practice (the merge/prune
  step already filters out anything `Validate` would reject), but a 1b
  client branching on `error.code` would have silently misparsed it.

## Verification

- `gofmt -l` on all touched files: clean.
- `go build ./...`: pass.
- `go vet ./pkg/hub/...`: clean.
- `go test -p 2 ./pkg/experiments/... ./pkg/config/opsettings/...`: pass.
- `go test -p 2 -run 'Experiment' ./pkg/hub/...`: pass (full-chain
  experiments suite, including the authorization matrix, stale replica,
  store race, malformed row, key retention, single unknown removal, and
  generic-reset rejection).
- `go test -p 2 -run 'TestRegisteredRoutes|TestRouteMetadata|TestRouteGuard|TestHubAdminRoutes|TestScopedAdmin|TestRouteGuardOpsPermissions' ./pkg/hub/...`:
  pass.
- `go test -p 2 ./pkg/hub/authzop/...`: pass, including
  `TestCatalogReportStaleness` on the regenerated
  `.design/authorization-operation-catalog.md`.
- `go test -p 2 ./pkg/hub/permissions/...`: pass, including the new
  `hub.experiments.update` entries in `project_applicability.go` and
  `collection_target_classes.go`.
- Not run: the full `pkg/hub/...` suite (the project's own
  `test-hub-sqlite` gate). Per the brief, `TestBackupBinary`,
  `TestBackupAndRestoreRoundtrip`, and the flaky
  `TestReincarnateAgent_WorkerStepsBumpRecordUpdatedAt` are known
  pre-existing failures on the 1a-i base, unrelated to this change; the
  full suite can exceed the sandbox's ~25-minute budget. The targeted runs
  above cover every package and test name this change touches.
