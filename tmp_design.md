# Scion Hub on Cloud Run: HA Deployment Design

## 1. Overview

This document defines the production target for running the Scion Hub on Google Cloud Run.

The target is a fully stateless, horizontally scalable Cloud Run service:

```text
User / agent / CLI
  -> Cloud Run native IAP
  -> scion server process
       - Hub API
       - Web UI
       - co-located stateless Runtime Broker
  -> CloudSQL Postgres, GCS, Cloud Run Instances, Filestore
```

The deployment intentionally supports more than one Cloud Run container serving Hub API traffic concurrently. CloudSQL Postgres is part of the baseline from the first production phase; SQLite and local filesystem storage are demo-only and are not valid for this HA design.

## 2. Load-Bearing Decisions

### 2.1 Co-located stateless broker

The Runtime Broker is not a separate Cloud Run service, VM, or process for this deployment. It is enabled in the same `scion` binary and Cloud Run service as the Hub:

```bash
scion server start --foreground --production \
  --enable-hub --enable-runtime-broker --enable-web --web-port 8080
```

The broker is a stateless API adapter to Cloud Run Instances. Any Hub replica can service operations for any agent by reading shared Postgres state and calling GCP APIs. Agent create, stop, delete, log, and exec paths must not rely on sticky sessions, a process-local broker owner, or a long-lived broker control channel owned by one Hub replica.

The design goal is one logical broker identity for the Cloud Run Instances runtime capability, shared by all Hub replicas. Broker identity is user-facing routing/configuration state, not replica ownership. Per-replica health can exist as internal telemetry, but it must not affect whether a specific replica can service an agent lifecycle request.

Remote or single-node stateful brokers connecting to this HA Hub are deferred. The Postgres command bus and durable `broker_dispatch` path are relevant to that later milestone, not a blocker for the initial Cloud Run Instances HA deployment.

### 2.2 Postgres from the start

CloudSQL Postgres is the production source of truth for Hub state and cross-replica realtime event delivery. The production Cloud Run service must not use SQLite.

Postgres-backed event delivery uses `PostgresEventPublisher` / `LISTEN` / `NOTIFY`. The deployment must fail closed or produce an explicit blocking warning if HA is enabled and the server falls back to the in-process `ChannelEventPublisher`.

### 2.3 Cloud Run native IAP

Use Cloud Run's native IAP integration directly. Do not introduce an External HTTPS Load Balancer, serverless NEG, managed certificate, or backend-service IAP unless a separate custom-domain/static-IP/Cloud Armor requirement is added.

The IAP audience for Cloud Run native IAP is:

```text
/projects/<PROJECT_NUMBER>/locations/<REGION>/services/<SERVICE_NAME>
```

This differs from the backend-service audience used by load-balancer IAP.

### 2.4 Direct-to-storage artifacts

GCS is required for production template/workspace artifact storage and signed URL flows. Local storage fallback is not HA-safe because each Cloud Run instance has independent ephemeral storage.

### 2.5 Cloud Run Instances runtime

The initial production runtime target is Cloud Run Instances, reached through the co-located stateless broker. GKE broker deployment scripts are demo or alternate-runtime material and should not be mixed into this production HA plan.

## 3. Architecture Details

### 3.1 Cloud Run service

- **Service:** one Cloud Run service running the combined Hub/Web/Broker process.
- **Region:** colocated with CloudSQL, Filestore, and Cloud Run Instances, initially `us-central1`.
- **Scaling:** `min-instances >= 2` for production HA after the smoke phase; set max instances from DB pool and runtime API quota limits.
- **CPU:** CPU must remain allocated for background work such as Postgres listeners, token minting, runtime health checks, and graceful shutdown handling.
- **Timeout:** use a long request timeout such as `3600s` for WebSocket/SSE/long-running operations where needed.
- **Ingress:** Cloud Run service protected by native IAP and `--no-allow-unauthenticated`.

### 3.2 Process model

Run the single binary with Hub, Web, and Runtime Broker enabled:

```bash
scion server start \
  --foreground \
  --production \
  --enable-hub \
  --enable-runtime-broker \
  --enable-web \
  --web-port 8080
```

The co-located broker's endpoint is internal to the same service/process topology. It should not be exposed as an independently deployed broker API.

### 3.3 Database

Use CloudSQL Postgres via Cloud Run's native Cloud SQL integration.

Example DSN shape:

```text
postgres://scion:<PASSWORD>@/scionhub?host=/cloudsql/<PROJECT>:<REGION>:<INSTANCE>
```

Deployment must configure the Cloud SQL instance attachment, map the DSN/password from Secret Manager, and set Postgres connection pool limits based on:

```text
max Cloud Run instances * per-instance DB pool size <= CloudSQL connection budget
```

Startup must handle concurrent replica starts and migrations safely. If migration locking is not already guaranteed for Postgres, add it before enabling multi-replica rollout.

### 3.4 Realtime events

Postgres-backed `LISTEN` / `NOTIFY` is the HA event fanout design. Acceptance must verify:

- a state change written through replica A is observed by SSE/Web clients connected to replica B;
- the active event publisher is `PostgresEventPublisher`;
- no production HA deployment silently falls back to in-process channels.

### 3.5 Runtime operations

The co-located broker calls Cloud Run Instances APIs directly. Required Cloud Run service account permissions include:

- `roles/run.admin` for Cloud Run Instances lifecycle;
- `roles/iam.serviceAccountUser` to attach the runtime service account to instances;
- `roles/logging.viewer` for Cloud Logging-backed logs;
- `roles/iap.tunnelResourceAccessor` for IAP exec/attach;
- `roles/iam.serviceAccountTokenCreator` on the dedicated transport-auth service account.

Runtime configuration must define the Cloud Run target:

```yaml
runtimes:
  cloudrun:
    type: cloudrun
    cloudrun:
      project_id: "<gcp-project>"
      location: "us-central1"
      service_account: "<agent-runtime-sa>@<project>.iam.gserviceaccount.com"
      network: "<vpc>"
      subnetwork: "<subnet>"
      nfs_server: "<filestore-ip>"
      nfs_export: "/<export>"
profiles:
  default:
    runtime: cloudrun
```

Agent operations must be idempotent against stable GCP resource names and shared Postgres records so any Hub replica can perform the operation.

### 3.6 Workspace and NFS

Cloud Run Instances do not support `subPath`. Isolation must come from server-side NFS paths:

```text
<export>/projects/<project-id>/workspace
<export>/projects/<project-id>/agents/<agent-id>/home
<export>/projects/<project-id>/agents/<agent-id>/secrets
```

The plan must provide a concrete provisioning mechanism for those directories before instance creation:

- mount the Filestore export into the Hub Cloud Run service and let the stateless broker provision guarded paths; or
- run a Cloud Run Job / separate provisioner with NFS access; or
- move provisioning into another durable storage/bootstrap path.

The selected mechanism must enforce that the emitted NFS path is strictly below the export root and never mounts the export root into an agent.

### 3.7 GCS storage

Configure GCS for templates and direct-to-storage sync. The Cloud Run service account needs permissions to create signed URLs and read/write the configured bucket paths.

The production config must set storage explicitly; no production deployment may rely on the local storage fallback.

### 3.8 Authentication

Human web auth uses proxy mode with IAP:

```yaml
server:
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: "/projects/<PROJECT_NUMBER>/locations/<REGION>/services/<SERVICE_NAME>"
```

Agent/service ingress through IAP uses dual-layer auth:

- platform layer: Google OIDC token for the IAP audience;
- app layer: Scion agent JWT in `X-Scion-Agent-Token`.

The Hub middleware must treat app-layer credentials as authoritative when present. IAP assertions for service accounts are transport only and must not create human user records.

Transport token issuance is configured under `server.auth.transport`, not `server.transport`:

```yaml
server:
  auth:
    transport:
      mode: iap
      oidc_audience: "/projects/<PROJECT_NUMBER>/locations/<REGION>/services/<SERVICE_NAME>"
      platform_auth_sa: "scion-transport@<project>.iam.gserviceaccount.com"
```

The Hub Cloud Run service account must be able to mint ID tokens as `platform_auth_sa`.

### 3.9 Correct versioned settings keys

Use the implemented versioned-settings keys:

```yaml
schema_version: "1"
server:
  mode: hosted
  database:
    driver: postgres
    url: "<postgres-dsn>"
  storage:
    provider: gcs
    bucket: "<bucket>"
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: "<cloud-run-iap-audience>"
    transport:
      mode: iap
      oidc_audience: "<cloud-run-iap-audience>"
      platform_auth_sa: "<transport-sa>"
  broker:
    enabled: true
```

Do not use `server.runtimeBroker` in versioned settings; the versioned schema uses `server.broker`. Do not use `server.transport`; transport auth is nested under `server.auth.transport`.

## 4. Non-Goals

- Supporting remote or single-node stateful Runtime Brokers against this HA Hub in the initial rollout.
- Using SQLite or local filesystem storage for the production Cloud Run service.
- Adding a load balancer solely for IAP when Cloud Run native IAP satisfies the ingress requirement.
- Supporting multiple runtime targets in the initial production deployment; Cloud Run Instances is the target.

## 5. Rollout Plan

### Phase 1: Image and HA baseline

- Build the combined Cloud Run image with embedded web assets.
- Deploy the Cloud Run service with native IAP.
- Use CloudSQL Postgres from the first production deployment.
- Configure durable session/signing secrets from Secret Manager.
- Start with Hub, Web, and Runtime Broker enabled in the same process.
- Verify basic health and IAP human login.

### Phase 2: Storage and database hardening

- Configure Cloud SQL instance attachment and Postgres DSN secret.
- Configure GCS storage bucket and permissions.
- Verify migrations, DB connectivity, signed URL generation, template upload/download, and restart persistence.
- Validate `PostgresEventPublisher` is active.

### Phase 3: Co-located Cloud Run Instances broker

- Configure `runtime.type: cloudrun` with project, region, runtime service account, network, and NFS settings.
- Grant required IAM roles to the Hub Cloud Run service account.
- Use one logical broker identity for the Cloud Run Instances runtime capability, shared by all replicas. Per-replica health can be internal telemetry only.
- Dispatch one test agent and verify create, status, logs, message, stop, and delete.

### Phase 4: IAP transport auth

- Configure `server.auth.transport`.
- Grant the Hub service account `serviceAccountTokenCreator` on the transport service account.
- Verify dispatch injects the transport token and audience into agent env.
- Verify agent callbacks traverse IAP and authenticate as agents, not as human proxy users.

### Phase 5: NFS workspace productionization

- Implement or configure the chosen NFS directory provisioning mechanism.
- Validate server-side project/agent path isolation.
- Validate UID/GID ownership and restart behavior.
- Confirm no agent can mount or traverse the Filestore export root.

### Phase 6: Multi-replica HA validation

- Run at least two Cloud Run instances.
- Verify browser/SSE clients on one replica observe events produced by another replica.
- Verify lifecycle operations succeed regardless of which replica receives the request.
- Verify no operation depends on replica-local broker ownership, sticky sessions, or a local broker control channel.
- Restart/roll a Cloud Run revision while agents exist and verify state recovery from Postgres and Cloud Run Instances APIs.

### Phase 7: Operational hardening

- Add alerts for Postgres event publisher failures, CloudSQL pool exhaustion, GCP token mint failures, Cloud Run Instances API failures, and NFS provisioning failures.
- Tune Cloud Run concurrency, min/max instances, CPU allocation, and timeout.
- Document rollback to the previous Cloud Run revision and any database migration constraints.

## 6. Acceptance Criteria

- The design doc no longer describes a standalone Runtime Broker deployment.
- Production config uses CloudSQL Postgres and GCS from the start.
- Cloud Run native IAP is the documented ingress path and uses the Cloud Run service audience.
- Versioned settings examples use `server.auth.transport` and `server.broker`.
- At least two Cloud Run instances can serve Hub API traffic concurrently.
- Cross-replica event delivery works through Postgres.
- Agent lifecycle operations through the Cloud Run Instances runtime are replica-independent.
- Remote/stateful broker support is explicitly deferred rather than implied.

## 7. Implemented Work That May Need Rework

Track these implementation areas during Phase 5 rather than letting the revised design hide already-landed assumptions:

- `.design/hub-cloudrun-deployment.md` previously described a standalone broker; any scripts/docs copied from that model must be removed or rewritten.
- `scripts/cloudrun/hub-settings-template.yaml` currently mixes demo GKE runtime settings with the production Cloud Run Instances target. It should either become a demo-only GKE template or be rewritten for `runtime.type: cloudrun`.
- `scripts/cloudrun/hub-settings-template.yaml` uses stale keys for production HA: `server.transport` should move to `server.auth.transport`, and `server.runtimeBroker` should become `server.broker`.
- `scripts/cloudrun/deploy.sh` and `scripts/cloudrun/README.md` currently describe SQLite and `max-instances=1` demo behavior. Production HA needs CloudSQL Postgres, GCS, and multi-replica validation from the start.
- Co-located broker registration code should be checked for assumptions that a broker ID belongs to one process. The target is one logical Cloud Run Instances broker shared by all replicas.
- Any code path using broker control-channel ownership, `connected_hub_id`, command bus ownership, or durable `broker_dispatch` should be scoped to the deferred remote/stateful broker milestone, not required for Cloud Run Instances lifecycle operations.
- Cloud Run Instances runtime NFS provisioning should be checked for local filesystem assumptions. The production design needs an explicit NFS provisioning mechanism that works from a stateless Cloud Run Hub service.
- HA startup should fail closed or surface a blocking deployment error if Postgres event publishing, GCS storage, durable signing/session secrets, or CloudSQL configuration are missing.
