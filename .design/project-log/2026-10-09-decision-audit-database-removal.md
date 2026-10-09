# Remove routine decision audit persistence

Routine authorization decisions no longer enqueue or write database audit records.
The in-memory decision record and emitter seam remains for decision sampling,
performance counters and the existing default-off admission behavior. Its production
fallback is inert. The former queue, writer workers, retry and drain logic, writer
metrics and legacy writer health check are removed.

The decision persistence store API and Ent schema are removed. After automatic
schema migration succeeds, SQLite and PostgreSQL execute a synchronous transaction
containing `DROP TABLE IF EXISTS decision_audits`. Failure is returned to startup
before Hub construction. The operation removes the legacy table and its rows,
is safe to repeat, and never uses `CASCADE` or changes foreign-key enforcement.
Fresh schema creation does not create the table; regenerated schema creation
cannot recreate it after an upgrade.

Mutation audits, transactional history, other tables, authorization results,
configured logging and NEW health classification are preserved. Existing mutation
retention remains; no decision-table retention or logging replacement is introduced.
Deployment and any live operational action are separate from this code change.

Source baseline: `1fb0ac82463d3ac61073759519006e616c1e54e6`.
Public tracking: https://github.com/ptone/scion/issues/2379.

Validation checkpoint: Ent regeneration and Go formatting completed through the
WL queue. Focused tests passed for `pkg/ent/entc` (0.504s) and
`pkg/store/entadapter` (0.811s), using `-vet=off -p 1 -count=1 -timeout 5m`
and the selector `^(TestDecisionAuditDrop_.*|TestMutationAuditStore_NewFields.*)$`.
The adapter test initially compared internal Ent configuration as well as persisted
fields; it now compares serialized user/project fields and passed on rerun.
The checks cover fresh schema, populated upgrade, repeat migration/reopen,
transaction failures, preserved mutation/history records and unrelated rows.
Static checks find no routine production decision persistence calls or writer
installation; the generated diff removes only the retired entity surfaces.

Hub and command compilation/tests remain held for a separate fresh resource
authorization with at least 60 GiB available, or upstream CI; they have not run
locally. PostgreSQL integration has not run because the isolated fixture URL is
unavailable. The PostgreSQL transaction command is covered by a dialect test;
actual PostgreSQL upgrade validation remains required.
No full suite, vet, lint, npm, race checks or live migrations ran at this checkpoint.
