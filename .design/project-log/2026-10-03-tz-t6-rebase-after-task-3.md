# tz-refactor task 6: rebase onto upstream main after task 3 merged

Date: 2026-10-03. Refs ptone/scion#2499, ptone/scion#2457.

## What changed

tz-refactor task 3 (GoogleCloudPlatform/scion#2289) landed upstream as one
squash commit, 8ab2e62e4. Its content matches the four pre-squash commits
this branch carried. The branch was rebased with
`git rebase --onto <upstream main 18cd8a51> 8f0e162`, so only the 13 task 6
commits were replayed. None of task 3's code was re-added.

- 12 of 13 commits are identical in `git range-diff`.
- `feat(cmd): repair unreadable SQLite timestamps at hub start` had one
  conflict in `cmd/migration_markers.go`. Upstream had added
  `MigrationEmptyPerAgentLegacyReport` at the same place. Both markers are
  kept: upstream's comes first, then `MigrationUTCTimestampRepair`, and
  `isKnownMigration` lists both. This is a mechanical merge with no
  behaviour change.

## Checks

- Migration ordering: the timestamp repair still runs before
  `migrateStore`, and its marker is written right after it. Upstream's
  empty-per-agent report runs inside `runBootDataMigrations`, so the two do
  not interact. `utc-timestamp-normalize` keeps its place in the built-in
  maintenance list.
- No duplication. The move into `pkg/store/storedtime` still deletes the
  `pkg/hub/time_string_parse.go` that task 3 added. Nothing else references
  `parseGoTimeString`.
- Build, vet, gofmt and scoped golangci-lint are clean.
- These tests pass under both TZ=Asia/Tokyo and TZ=Asia/Kathmandu:
  `pkg/store/storedtime`, `pkg/store/entadapter`, the targeted `cmd` tests,
  and the targeted `pkg/hub` tests (normalize, webchat time, chat search,
  maintenance).

## Follow-ups

None.

## Addendum: second rebase onto upstream main c0a69140

Upstream main moved 15 commits. Among them are tz-refactor task 18
(GoogleCloudPlatform/scion#2388, the `applied-config-tz-cleanup`
maintenance migration) and tz-refactor task 22 (GoogleCloudPlatform/scion#2377,
CLI time zones).

- Conflicts, all in `feat(hub): add the utc-timestamp-normalize maintenance
  migration`:
  - `pkg/hub/admin_maintenance.go`: both executor cases are kept, with
    `applied-config-tz-cleanup` first.
  - `pkg/store/entadapter/maintenance_store.go`: both seed entries are
    kept, with `applied-config-tz-cleanup` first and then
    `utc-timestamp-normalize`.
- The later re-runnable commit differs only in context lines. The other
  commits are identical in `git range-diff`.
- Interaction check: the two migrations write disjoint columns.
  - `applied-config-tz-cleanup` rewrites `agents.applied_config` through
    `UpdateAgent`.
  - `utc-timestamp-normalize` rewrites time columns, `agents.exposed_ports`
    and `access_policies.conditions` with compare-and-set updates.
  - Neither run order matters, and they share no helper.
  - Task 18 has no startup check.
  - Only `utc-timestamp-normalize` is in `rerunnableMigrations`. Task 18
    keeps upstream's guard against re-running a completed migration.
- `hack/check-cli-time-zones.sh` from task 22 passes on the branch.
  `pkg/clitime` only formats times for display and does not duplicate
  `pkg/store/storedtime`.
- Tests pass under both TZ=Asia/Tokyo and TZ=Asia/Kathmandu:
  `pkg/store/storedtime`, `pkg/store/entadapter`, the targeted `cmd` tests,
  and the targeted `pkg/hub` tests (adding `AppliedConfigTZCleanup` and
  `Maintenance`, 67 passed). Build, vet, gofmt and scoped golangci-lint
  are clean.
