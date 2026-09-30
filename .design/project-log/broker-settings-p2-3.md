# Broker settings P2.3: binding migration, createEntitlement 400, docs (ptone/scion#2061 P2-D4, ptone/scion#2063 items 2 and 3)

Base: stacked on P2.1 (ptone/scion#2275, branch `scion/broker-settings-p2-1`), started at its head
`0875d543a658cd77e7a9ec92ba42436878aa0839`. Design: `/scion-volumes/scratchpad/projects/broker-settings/design.md`
§5.5, §5.7 (P2.3), §5.8 (AC-P2-5), §6. Branch `scion/broker-settings-p2-3`. PR: ptone/scion#2306
(draft, stacked on ptone/scion#2275 — **do not merge before #2275 lands**).

## What shipped

- **One-shot boot data migration `broker_quota_bindings_to_settings`**
  (`cmd/boot_broker_quota_bindings_to_settings.go`, marker in `cmd/migration_markers.go`, wired into
  `runBootDataMigrations` in `cmd/boot_data_migrations.go`). For every broker B with
  `max_agents_per_broker` entitlement bindings where `scopeType=broker, scopeId=B` — covering both
  historical workarounds identically (the user-subject hack and a `system_default` row with a
  non-empty subject; the migration groups purely on scope, not subject shape) — it writes the value
  the entitlement engine's "most generous wins" rule would have produced (0 if any binding is 0,
  otherwise the max) into that broker's `broker_settings` row via `store.BrokerSettingStore`, with
  `updatedBy: "migration:ptone/scion#2061"`. A broker that already has a `maxAgents` setting (even a
  future field other than `maxAgents`, since the write preserves the rest of the document) is left
  untouched. Bindings are never deleted — only shadowed by the new setting under the P2-D2
  precedence rule (broker setting beats bindings), and their IDs are logged. A binding whose broker
  no longer exists is skipped, logged, and counted in the marker's residual field — a deterministic,
  permanent outcome, not a run-level failure. Follows the M-1' marker pattern exactly as the other
  migrations in `cmd/boot_data_migrations.go` (e.g. `runBrokerOwnershipBackfill`): any run-level
  failure (list/read/write error other than "not found") aborts the whole pass without writing the
  marker, so the next boot retries from scratch; this migration does not do per-broker partial
  progress tracking (unlike the message backfill) because there's no expectation of a large enough
  row count to need a time budget.

- **`createEntitlement` / `updateEntitlement` reject the deprecated shape**
  (`pkg/hub/handlers_quota.go`): for `limit == max_agents_per_broker && scopeType == broker`, both
  now return 400 `"per-broker agent caps are set via PUT /api/v1/runtime-brokers/{id}/settings"`.
  `updateEntitlement` needed the identical check — the brief specifically asked to check the update
  path, and without it, PUT could reshape an existing binding into the exact rejected shape,
  bypassing the POST-time check. System-scoped bindings for this limit, and broker-scoped bindings
  on any other limit, are unaffected — confirmed with tests for both.

- **UI check, no change made**: the brief asked to check whether the web admin-quotas entitlement
  form offers broker scope for this limit and, if so, make the 400 surface cleanly. It doesn't —
  `web/src/components/pages/admin-quotas.ts`'s entitlement dialog's Scope Type `<sl-select>` only
  offers `system` and `project` options (no `broker` option exists at all), so no admin-quotas.ts
  edit was needed. (admin-quotas.ts is P2.2's file to edit in parallel per the brief, so this was
  worth confirming rather than assuming.)

- **Docs** (`docs-site/src/content/docs/reference/api.md`,
  `docs-site/src/content/docs/hosted/ha/multi-broker.md`): added a "Broker Settings" section to
  api.md documenting `GET`/`PUT /api/v1/runtime-brokers/{id}/settings` — request/response shape
  (`settings`, `effective.maxAgents.{value,source,count,inherited}`, `revision`, `updatedBy`,
  `updated`, `_capabilities`), status codes (400/403/404/409), and `quota.update` as the write
  permission — plus the precedence rule (broker setting > entitlement bindings, most generous wins >
  hub-wide default; 0 = unlimited), the migration's behavior, and the new 400. Replaced the sentence
  in both files that told operators to use a `system_default`/`scope=broker` entitlement binding for
  a per-broker override (the exact thing ptone/scion#2063 items 2/3 tracked as broken) with a
  pointer to the new settings API. Left the seed-default-number sentences (stating **12**) completely
  untouched in both files, per the brief — that text is P1a's (ptone/scion#2268), kept mechanical for
  its later rebase. Confirmed via `grep -rl max_agents_per_broker docs-site/` that no other page
  references the broken path; the only other hit is a dated release-notes entry, out of scope by
  instruction.

## Tests

- `cmd` package (migration), SQLite only — see "what I ran" below for why no Postgres run exists:
  `TestBrokerQuotaBindingsToSettingsMigration_UserHackBindingBecomesSetting`,
  `_TwoBindingsGiveMax`, `_AnyZeroGivesZero`, `_ExistingSettingUntouched`, `_Idempotent`,
  `_MissingBrokerSkipped`, `_NoLimitDefined` — all seven scenarios design §5.7 lists for the
  migration, plus a no-limit-defined edge case not in the list.
- `pkg/hub` (handler): `TestQuotaAPI_CreateEntitlement_MaxAgentsPerBrokerBrokerScoped_Rejected`,
  `_MaxAgentsPerBrokerSystemScoped_StillWorks`, `_BrokerScopedOtherLimit_Unaffected`, and the three
  `UpdateEntitlement` equivalents.
- `pkg/hub` end-to-end: `TestBrokerQuota_MigratedSettingEnforced` writes the exact
  `store.BrokerSettings` document the migration produces for a broker, then proves `Reserve`
  enforces it — over a hub-wide default 100x larger — through the real `POST /agents/:id/start` HTTP
  path. It does not literally invoke the `cmd`-package migration function (`pkg/hub` cannot import
  `cmd` without a cycle); the migration's own grouping/write logic is covered by the `cmd`-package
  tests above, and this test covers the integration point between "migration wrote a setting" and
  "Reserve enforces it," which P2.1 wired.

**What I ran, and why no Postgres run for the migration**: `make test-hub-sqlite` and `make
test-fast`, both with `SCION_*`/`CLAUDE_*` env vars scrubbed (`unset $(env | grep -E
'^(SCION_|CLAUDE_)' | cut -d= -f1)`) — both green. Without scrubbing, `make test-fast` fails
`TestNativeTelemetryPolicyEffectiveChildEnv` in `pkg/sciontool/supervisor` on ambient
`CLAUDE_CODE_ENABLE_TELEMETRY`; confirmed environment-only, not a regression. `go test -count=1
./cmd/...` has three failures (`TestHubAllOrOneActions`, `TestRequireImageRegistryForBroker_Settings`,
`TestInitPluginManager_MigratesConfigFileOnlyPlugin`) that I confirmed pre-exist on the P2.1 base
commit (`git stash -u` + re-run), unrelated to this PR. `go vet ./cmd/... ./pkg/hub/...` and `go vet
-tags no_sqlite` both clean. `golangci-lint run ./cmd/... ./pkg/hub/...` reports 15 pre-existing
issues, none in any file this PR touches. `tsc --noEmit` not run — web is untouched by this PR. The
brief asked to run the migration tests "on SQLite and on Postgres if the existing migration tests
support it": the precedent this migration follows (`boot_broker_ownership_backfill_test.go` and
every sibling in `cmd/`) is SQLite-only (`//go:build !no_sqlite`, `entc.OpenSQLite` directly) with
no Postgres harness in the `cmd` package; I did not build one for this PR and flagged that choice
explicitly to the EM rather than silently only running SQLite.

## Surprising / worth flagging

- `pkg/hub/broker_capacity.go`'s `brokerSettingLimitOverride`/`effectiveBrokerLimit` (from P2.1)
  already wires broker settings ahead of bindings in `Reserve` — this PR's migration only had to get
  the right value into the store; the enforcement side needed no changes at all. Worth confirming a
  reviewer doesn't expect a `Reserve`/`quota.go` diff in this PR — there isn't one, deliberately.
- The design doc's illustrative migration description didn't specify what "no maxAgents setting yet"
  means when a `broker_settings` row exists but its `maxAgents` field is nil (as opposed to no row at
  all). I treated "has a setting" as "row exists AND `MaxAgents != nil`," preserving any other stored
  fields on write — there are none today, but this keeps the migration correct if a second
  broker-settings key is added later.
