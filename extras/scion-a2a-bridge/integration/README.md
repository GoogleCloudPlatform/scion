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
- unique PostgreSQL run/schema naming and reverse-order teardown hooks;
- deterministic deployment manifest validation for Cloud Run (`deploy/cloudrun/service.yaml`)
  and Kubernetes (`deploy/kubernetes/deployment.yaml`); and
- a machine-readable status map (`testdata/acceptance_layers.json`) for the eleven
  planned deterministic test layers and external-live qualification boundaries.

The real-process tests compose a production Hub exchange handler over durable
SQLite identity bindings, a pinned fake Google JWKS process, two independent
bridge processes, the production A2A SDK handler/executor, authenticated gRPC
control transport, and the deterministic alternator.

Run the suite with automated PostgreSQL 15 provisioning and fail-closed checks:

```sh
make test-a2a-integration
# or directly:
./scripts/run-integration-ci.sh
```

Run the deterministic suite locally without real PostgreSQL:

```sh
go test ./integration
```

### Fail-Closed Database Policy

When running in CI (`CI=true`) or when `TEST_REQUIRE_DATABASE=1` is specified,
the suite enforces a strict fail-closed policy: if `TEST_DATABASE_URL` is empty or
PostgreSQL 15 is unreachable, the tests fail immediately (`t.Fatal`) rather than
silently skipping.

In local developer mode (when neither `CI` nor `TEST_REQUIRE_DATABASE` is set),
PostgreSQL-dependent tests skip gracefully with `t.Skip` if `TEST_DATABASE_URL` is unset.

### Acceptance Layers & Deployment Boundaries

`testdata/acceptance_layers.json` records local deterministic integration proof separately
from external-live work. With PRs #1741 (Auth), #1742 (HA), and #1743 (Transport) merged into
main:
- The lifecycle (`TestTwoReplicaUserLifecycle`), stream cursor (`TestCrossReplicaStreamCursor`),
  crash lease boundary (`TestCrashLeaseBoundary`), cold replica rotation (`TestColdReplicaAndRotation`),
  control plane principal isolation (`TestControlPlanePrincipalIsolation`), combined startup matrix
  (`TestCombinedStartupMatrix`), credential redaction (`TestCredentialRedaction`), and CI integration
  runner (`CIAutomatedPostgresIntegration`) are fully passing.
- `TestGEEnvelopeCompatibility` parent row remains false/partial solely because its `actual-ge-capture`
  sublayer requires live external Gemini Enterprise environment access (`passing: false`).
- `CloudRunDeploymentConfig` and `KubernetesDeploymentConfig` parent rows have `passing: false` because
  their live deployment sublayers require live Cloud Run and Kubernetes infrastructure, while their
  local dry-run manifest validation sublayers pass deterministically.
