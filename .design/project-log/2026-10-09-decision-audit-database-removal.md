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
