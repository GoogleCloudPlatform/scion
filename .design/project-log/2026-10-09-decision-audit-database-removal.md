# Remove routine decision audit persistence

Routine authorization decisions no longer write database audit records. The
in-memory decision record and emitter remain for sampling and performance counters.
The fallback target is inert. The former writer, queue, retry logic, writer metrics
and legacy writer health check are removed. NEW admission teardown runs once in
server resource cleanup; it requires no deferred writer close or exit callback.

The decision persistence store API and Ent schema are removed. After automatic
schema migration succeeds, SQLite and PostgreSQL execute a synchronous transaction
containing `DROP TABLE IF EXISTS decision_audits`. Errors return to startup before
Hub construction. The drop removes the table, its indexes and rows and is safe to
repeat. Fresh schema creation does not create it. Mutation audits, transactional
history, unrelated tables, authorization results and configured logging remain.

The drop is irreversible: its data cannot be recovered by rolling back this code.
On PostgreSQL it takes an ACCESS EXCLUSIVE lock under the existing migration
context. During a mixed-version rollout, old replicas can see writer failures after
the table is dropped. Rolling back to an old binary can recreate the table during
schema migration. Deployment sequencing is separate from this code change.

Source baseline: `1fb0ac82463d3ac61073759519006e616c1e54e6`.
Public tracking: https://github.com/ptone/scion/issues/2379.

Ent generation and formatting completed. Focused SQLite tests passed for fresh
schema, populated upgrade, repeated migration and reopening, transaction failures,
mutation/history conservation and unrelated rows. The mutation audit roundtrip
tests also passed. A test assertion was corrected to compare persisted fields
instead of internal Ent configuration.

Review corrections remove the remaining deferred-close APIs and exit callbacks,
use one pointer identity for the inert target, simplify NEW health projection and
replace indirect test probes with router identity and table-absence assertions.
The corrected Hub and command tests remain uncompiled and unexecuted pending
separate resource authorization or CI. Real PostgreSQL integration also remains
unexecuted; its transaction command is covered by an isolated dialect test.
No deployment or live migration is included in this change.

The second conservation review aligns both performance-tracing references with
in-memory record counts and emitter-call timing. The metrics test now checks a
retained reaper instrument alongside absence of the retired instruments. Health
coverage uses the server health surface, and migration tests provide mutation and
history conservation evidence. Upgrade fixtures include the baseline's 25 columns,
eight secondary indexes and populated rows, with index-removal checks. The inert
target retains its singleton pointer and uses a blank byte field for nonzero size.
Fresh focused migration tests passed for `pkg/ent/entc` and
`pkg/store/entadapter`, including populated-table and named-index removal,
reopening and mutation/history conservation. Hub, command and real PostgreSQL
checks remain unexecuted for this revision.

Final emitter and failed-start cleanup comments now match retained behavior;
static diff and stale-text checks passed.

The third code review aligns the experiment description and its test pin with
non-persistence on admission, freshness or logging-health failure. Database
reference guidance now covers permanent removal, export, PostgreSQL locks,
mixed replicas and old-binary rollback. Pool-wait commentary and the negative
health-key sentinel comment match current behavior. Begin-transaction failure
coverage now requires zero commits and rollbacks. Focused Entc migration tests
passed; Hub, command, experiment-package, documentation-build and PostgreSQL
validation remain unexecuted for this revision.

The fourth code review documented direct `CompositeStore.Migrate` maintenance
callers, removed an orphaned test banner and reflowed the migration comment;
static source-call-site, exact-path, stale-text and diff checks passed. The fifth
review extends the caution to direct `entc.AutoMigrate` callers, including default
dry-run backfill and DM-key migration, and distinguishes destination migration
from the read-only source in `server migrate`. Source decision-audit rows were
already excluded from data copying in the baseline. Direct maintenance migrations
run outside the Hub advisory schema lock. This documentation correction changes
no command or dry-run behavior; no Go commands or new validation were run.

A local Hub compile at `354350b6c1ccd3e1dfdf035512a7ba601658f26a` failed in
`decision_audit_admission_test.go` on references to the deleted legacy writer
drain and abort-grace constants; 0 of 29 targeted tests ran (NOT RUN) and there
is no local PASS. The stale assertion now checks the retained NEW budgets
instead: 1s cancel and 2s complete, both relative to the original handoff. Close
adds no timeout of its own. The original-handoff, ownership, cancel and
reference-release assertions are unchanged. No production code changed. The
targeted Hub rerun for this correction has not been executed yet.
