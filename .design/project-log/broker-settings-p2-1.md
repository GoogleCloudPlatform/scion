# Broker settings P2.1: per-broker `maxAgents` vertical slice (ptone/scion#2061 P2, ptone/scion#2177)

Base: started stacked on PR ptone/scion#2168 (frozen at `e8013da1` on `origin/feat/provider-capacity`);
that PR has since landed upstream as GoogleCloudPlatform/scion#2097, and this branch is rebased onto
`upstream-main` on top of it. Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md`
§5, especially §5.1–§5.4, §5.6, §5.7 P2.1, §5.9. Branch `scion/broker-settings-p2-1`.

## What shipped

- **Ent schema + generated code**: new `broker_settings` table (`pkg/ent/schema/brokersetting.go`),
  one JSON document per runtime broker with an `int64` revision column for compare-and-set, mirroring
  `HubSetting`'s existing pattern rather than inventing a new one. `go generate ./pkg/ent` output
  committed.
- **Store layer**: `store.BrokerSettings{MaxAgents *int64}` / `store.BrokerSettingsRecord`, the
  `BrokerSettingStore` interface (`Get`/`Put` with CAS — `expectedRevision 0` = create-only,
  `store.ErrRevisionConflict` on mismatch — and `Delete`), and the `entadapter.BrokerSettingStore`
  implementation. `DeleteRuntimeBroker` now explicitly deletes the settings row (no FK edge exists
  between `runtime_brokers` and `broker_settings` by design, so heartbeats never contend with
  settings writes).
- **Key registry** (`pkg/hub/brokersettings`): `maxAgents` is the first and only P2.1 key —
  `>= 0` (0 = unlimited), declares `quota.update` as its write permission. Unknown keys in a PUT
  are rejected with 400 via `brokersettings.Lookup`.
- **HTTP API**: `GET`/`PUT /api/v1/runtime-brokers/{id}/settings`, dispatched from the existing
  `/api/v1/runtime-brokers/` mux pattern (string-split subpath routing, same shape as the existing
  `env`/`secrets` subresources) rather than a new registered route — this meant no new
  `routeMetadataTable` entry was needed, only extending the existing `broker.read` and
  `quota.update` authzop catalog entries with a new `EntryPoint` each (`.design/authorization-operation-catalog.md`
  regenerated via `go test ./pkg/hub/authzop/...`). GET requires `broker.read` on the broker
  resource (so an owner can always see their own broker's settings, even read-only); PUT requires
  `broker.read` as a precondition, plus a stale-revision check *before* authorization (a caller's
  `expectedRevision` must match the freshly-read current revision, or 409), plus the declared
  permission of every key whose value actually *changes* between the stored document and the new
  one — not just the keys present in the request, since PUT is a full replace and an omitted or
  `null` key clears it. For `maxAgents` that's `quota.update` on `Resource{quota, hub}`, hub-admin
  only, matching P2-D3. A no-op write (nothing changes) skips the store entirely: no revision bump,
  no `updatedBy` rewrite, no audit event, no row created for a broker that never had one.
- **One read model, shared by enforcement and every read path** (design.md §5.9, AC-P2-10):
  `effectiveBrokerLimit`/`brokerCapacity` (`pkg/hub/broker_capacity.go`) implement the P2-D2
  precedence — broker setting > entitlement binding > hub-wide default. `QuotaService` gained a
  minimal `limitOverride` hook (`quota.go`), wired in `server.go` to `brokerSettingLimitOverride`,
  consulted by `Reserve` before `ResolveEffectiveLimit`; the existing providers-listing helper
  `resolveBrokerCapacity` (from PR ptone/scion#2168) became a thin wrapper that also surfaces an
  additive, `omitempty` `agentLimitSource` field, per small-issues-lead-2's agreement recorded in
  design.md §5.9. `ResolveEffectiveLimit` itself is unchanged for existing callers; its logic was
  split into an internal `resolveEffectiveLimitWithSource` so the new code can tell "resolved from
  an entitlement binding" from "resolved from the hub-wide default" without duplicating the merge
  rule.
- **Web**: hand-written `BrokerSettings`/`BrokerSettingsResponse`/`EffectiveSetting` TypeScript types
  in `web/src/shared/types.ts` mirroring the Go JSON tags exactly (no generator exists — this is the
  one place JSON-tag drift has to be caught by review). `broker-detail.ts` gained a "Settings" card:
  "Use hub default (N)" / "Custom" radio (0 = unlimited), a live usage line (counted agents +
  effective source), Save gated on `_capabilities.update`, and a 409 handler that reloads and shows
  "changed by someone else". Did not touch the unrelated `createdAt` drift noted in findings.

## Surprises / notes for review

- **`brokerCapacity` looked unused at first.** `effectiveBrokerLimit` alone was enough to satisfy
  every P2.1 call site (Reserve's override hook, the providers listing, and the settings response),
  so the `BrokerCapacity{Limit, Count, Source}` struct and its constructor were briefly dead code —
  `golangci-lint` caught this immediately (`unused`). Fixed by routing the settings GET/PUT response
  through `brokerCapacity` instead of `effectiveBrokerLimit` directly, which is what design.md §5.9
  asks for anyway ("the broker settings GET returns `effective` built from `brokerCapacity`") and
  incidentally means a resolution failure now degrades the settings response the same way it already
  degrades the providers listing (value left unset, not a 500), rather than introducing a second,
  stricter failure convention for the same underlying computation.
- **Two different "unlimited" conventions had to be reconciled deliberately.** `BrokerCapacity.Limit`
  is `nil` for unlimited (the existing providers-listing convention: omit the JSON key entirely). The
  broker-settings API instead always shows a concrete number (`0` for unlimited) per design.md §5.4's
  illustrative response. `buildBrokerSettingsResponse` converts explicitly at the boundary rather
  than picking one convention and forcing the other caller to match it.
- **Heartbeat can't be exercised through HTTP as a "dev" user token.** `handleBrokerHeartbeat`'s
  user-identity path checks `Resource{Type: "runtime_broker", ...}`, a resource-type string that has
  no matching entry in the permissions registry (only `Resource{Type: "broker", ...}` does, via
  `brokerResource()`) — so even the super-admin dev token gets a real 403 there. This looks like a
  pre-existing inconsistency unrelated to this change (heartbeats work fine in production via the
  broker's own HMAC identity, which bypasses the permission check entirely). Rather than route around
  it with an HMAC test fixture, `TestBrokerSettings_HeartbeatLeavesSettingsUnchanged` calls
  `store.UpdateRuntimeBrokerHeartbeat` directly — the property under test (a `runtime_brokers` write
  never touches the separate `broker_settings` row) is a store-layer property by construction, since
  the two live in different tables. Flagged to the EM/reviewers rather than silently worked around.
- **P1b (the enforcement switch, PR ptone/scion#2270) is not wired.** Per the brief, `quota.go`
  changes are kept minimal and localized (the `limitOverride` hook is additive, no existing behavior
  changed) specifically so that rebase is a small diff: add one more precedence check ahead of the
  broker-settings override, and one more `BrokerLimitSource*` value (`not_enforced`, already declared
  as a constant in `broker_capacity.go` with a comment marking it unused until then).

## Verification

- `go build ./...` — clean.
- `go vet ./...` — clean.
- `golangci-lint run ./pkg/hub/... ./pkg/store/...` — 12 pre-existing findings, all in files this PR
  does not touch (verified via `git diff --name-only`); zero findings in any file this PR adds or
  changes.
- `make test-hub-sqlite` — full pass (`pkg/hub`, `pkg/hub/auth`, `pkg/hub/authzop`, `pkg/hub/githubapp`,
  `pkg/hub/imagecheck` all `ok`), ~932s.
- `make test-fast` — `pkg/hub`, `pkg/store`, `pkg/store/entadapter`, `pkg/ent` all `ok`. Pre-existing
  failures in `cmd`, `pkg/agent`, `pkg/config`, `pkg/harness`, `pkg/runtime`, `pkg/runtimebroker`,
  `pkg/sciontool/supervisor` — none touched by this PR, consistent with sandbox/environment
  limitations for those subprocess/provisioning-heavy suites rather than a regression here.
- **Postgres**: installed PostgreSQL 15 locally (`apt-get install postgresql`) since no external
  instance was provided, started it, and ran the `entadapter` package's `-tags integration` suite
  against it end to end (`SCION_TEST_POSTGRES_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
  go test -tags integration -timeout 30m ./pkg/store/entadapter/...`), which provisions a fresh
  ephemeral database and a schema-per-test — this is the same path `pkg/store/enttest` documents for
  CI-style Postgres verification. All `BrokerSettingStore`/`DeleteRuntimeBroker` tests
  (`TestGetBrokerSettings_*`, `TestPutBrokerSettings_*`, `TestDeleteBrokerSettings_*`,
  `TestDeleteRuntimeBroker_*`) pass against real Postgres, exercising the `SELECT ... FOR UPDATE` CAS
  path that SQLite's single-writer lock never touches. The full `entadapter` package run has 4
  failures — `TestBackfillAgentIdentityKeys_{EarlierSlugBeatsLaterDisplayKey,LaterSlugSurvivesEarlierDisplayKey,DisplayVsDisplayKeepsFirst}`
  and `TestUpsertConversationByExternalRef_FieldClassification` — confirmed (by an independent
  reviewer, running in parallel against the same server) to fail identically on the unmodified base
  `e8013da1`; they are pre-existing and unrelated to this change.
- `npx tsc --noEmit` (web/) — clean.
- `hack/check-authz-guards.sh` — no violations.
- `go test ./pkg/hub/authzop/...` — regenerated and validated `.design/authorization-operation-catalog.md`.

## Deliverables

- PR ptone/scion#2275 against `ptone/scion` `main` from `scion/broker-settings-p2-1`, marked ready
  for review. PR ptone/scion#2168 (GoogleCloudPlatform/scion#2097) squash-merged upstream and this
  branch is rebased onto `upstream-main` on top of it.
- This log entry.
- `scion message` to `broker-settings-em` with PR number, head SHA, and the test evidence above.
