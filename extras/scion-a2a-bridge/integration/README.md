# GE/A2A deterministic auth + transport integration

This directory is the test-only composition point for the combined #1620
suite. All fake endpoints and process helpers are in `_test.go` files, so no production binary,
flag, environment variable, route, validator override, or public configuration is
added by this package.

The executable auth+transport phase provides:

- real subprocess topology with distinct PIDs, deterministic reserved loopback
  ports, TCP readiness, shared cancellation, process reaping, sanitized logs, and
  structured observations;
- an HTTP reverse-proxy process that alternates new sequential requests across two
  backends while a streaming response remains on the backend selected when that
  request began;
- synthetic identity and A2A envelope category fixtures with their schema sources;
- credential redaction for bearer values and their stable SHA-256 encodings;
- unique PostgreSQL run/schema naming and reverse-order teardown hooks; and
- a machine-readable status map for the eight planned deterministic test layers.

The real-process tests compose a production Hub exchange handler over durable
SQLite identity bindings, a pinned fake Google JWKS process, two independent
bridge processes, the production A2A SDK handler/executor, authenticated gRPC
control transport, and the deterministic alternator. Run the proven layers with:

```sh
go test ./integration -run 'Test(GEEnvelopeCompatibility|ColdReplicaAndRotation|ControlPlanePrincipalIsolation|CombinedStartupMatrix|CredentialRedaction)$'
```

Run the foundation without cloud credentials:

```sh
go test ./integration
```

The PostgreSQL socket test is optional and skips unless `TEST_DATABASE_URL` is set:

```sh
TEST_DATABASE_URL='postgres://...' go test ./integration -run TestPostgreSQLSchemaAllocator
```

That test creates and drops only its uniquely named schema. `DatabaseName` is a
deterministic name available to a future database-level provisioner; this foundation
does not assume permission to create databases. The schema models bridge task/event
storage only. Hub identity persistence remains an independent dependency and must not
be inferred to share this connection, schema, transaction, or lifecycle.

`testdata/acceptance_layers.json` records local deterministic integration proof separately
from external-live work. With PRs #1741 (Auth), #1742 (HA), and #1743 (Transport) merged into
main, the lifecycle (`TestTwoReplicaUserLifecycle`), stream cursor (`TestCrossReplicaStreamCursor`),
crash/lease (`TestCrashLeaseBoundary`), and startup matrix (`TestCombinedStartupMatrix`) layers
are fully passing and proven on real PostgreSQL. The `TestGEEnvelopeCompatibility` parent row
remains false/partial solely because its `actual-ge-capture` sublayer requires live external
Gemini Enterprise environment access.
