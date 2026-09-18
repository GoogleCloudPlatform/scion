# A2A Bridge Production Deployment Guide

This guide details the deployment architectures, configuration requirements, security invariants, and operational procedures for deploying the Scion A2A Bridge in high-availability (HA) multi-instance topologies across Google Cloud Run and Kubernetes.

---

## 1. Cloud Run Multi-Instance Deployment

### Architecture & Networking
- **Platform Ingress:** Terminated at Cloud Run's external HTTPS load balancer.
- **Container Listener:** Single port cleartext `h2c` (`HTTP/2 cleartext`) via `PORT=8080` and `MUX_PORTS=true`.
- **Multiplexing:** The single `h2c` listener multiplexes incoming gRPC (`BrokerService`) from the Hub and JSON-RPC / SSE over HTTP from external Gemini Enterprise clients.
- **Minimum Scaling:** Set `autoscaling.knative.dev/minScale: "2"` to guarantee active multi-replica presence for immediate lease handover and failover resilience.

### Dual-Header Authentication Semantics
Cloud Run enforces a two-tier identity boundary:
1. `X-Serverless-Authorization: Bearer <ID_TOKEN>`: Evaluated and consumed by Cloud Run's platform ingress for `roles/run.invoker` IAM checks. Cloud Run strips this header before dispatching the request to the container.
2. `Authorization: Bearer <TOKEN>`: Passes through directly to the container application:
   - For gRPC control RPCs: Evaluated by the bridge's `UnaryAuthInterceptor` against `GRPC_AUTH_SUBJECTS`.
   - For client JSON-RPC requests: Evaluated by the bridge's A2A bearer token middleware.

### Principal Separation Invariant
- **Hub Service Account (`hub-sa@...`):** Authorized in `GRPC_AUTH_SUBJECTS` to invoke control methods (`Configure`, `Publish`, `Subscribe`, `Unsubscribe`, `HealthCheck`, `GetInfo`).
- **GE Discovery Engine Invoker (`ge-invoker@...`):** Granted Cloud Run `roles/run.invoker` to reach the HTTPS endpoint, but strictly NOT listed in `GRPC_AUTH_SUBJECTS`. Attempting to invoke gRPC control RPCs with the GE invoker identity is rejected with `codes.PermissionDenied`.

### Shared Storage
All instances connect to a central PostgreSQL 15 database instance (e.g. Cloud SQL with Cloud SQL Auth Proxy sidecar or private IP VPC connector). Leases and state are managed via `a2a_sdk_tasks`.

### Health, Rollback, and Cleanup
- **Probes:** Configured via `startupProbe` and `livenessProbe` querying `GET /healthz` on port 8080.
- **Rollback:**
  ```bash
  gcloud run services update-traffic scion-a2a-bridge \
    --region=us-central1 \
    --to-revisions=PREV_REVISION=100
  ```
- **Cleanup:**
  ```bash
  gcloud run services delete scion-a2a-bridge --region=us-central1 --quiet
  ```

---

## 2. Kubernetes Multi-Replica Deployment

### Architecture & Service Definition
- **Deployment:** Defined with `replicas: 2` (or greater) using `RollingUpdate` strategy (`maxSurge: 1`, `maxUnavailable: 0`).
- **Service:** Headless or ClusterIP Service exposing port 8080 with `appProtocol: kubernetes.io/h2c`.
- **Ingress:** Ingress controller (GKE Ingress, NGINX, or Istio) terminating TLS and routing to Service.

### Credentials & Identity
- **PostgreSQL Connection:** Secret `a2a-postgres-secret` containing `database-url`.
- **Identity Options:**
  - Google ID Token validation via GKE Workload Identity.
  - Native mTLS with certificates mounted from secrets (`GRPC_TLS_CERT`, `GRPC_TLS_KEY`, `GRPC_TLS_CLIENT_CA`) when running dedicated gRPC listeners.

### Health, Rollback, and Cleanup
- **Probes:** `readinessProbe` and `livenessProbe` checking HTTP `/healthz`.
- **Rollback:**
  ```bash
  kubectl rollout undo deployment/scion-a2a-bridge -n scion
  kubectl rollout status deployment/scion-a2a-bridge -n scion
  ```
- **Cleanup:**
  ```bash
  kubectl delete -f deploy/kubernetes/deployment.yaml -n scion
  ```

---

## 3. Hub Exchange & OAuth Client Configuration

When configuring Google credential exchange in `pkg/hub`:

### Client IDs & Redirect URIs
- `allowed_client_ids`: Explicit allowlist of OAuth 2.0 Web / Desktop Client IDs issued by Google Cloud Console. Wildcards or empty values fail closed at startup.
- `redirect_uris`: Approved redirect URIs configured on Google Cloud Console and matching Hub origin.

### Token Lifetime & No Refresh Tokens
- **Short-Lived Access/ID Tokens Only:** Hub Google exchange validates incoming Google credentials and issues scoped, short-lived Hub JWTs (default 1h, max 24h).
- **NO Refresh Tokens:** The A2A Bridge and Hub exchange deliberately DO NOT support, store, or issue OAuth refresh tokens. Clients must rotate short-lived credentials upstream through Google STS / OAuth rather than caching persistent refresh tokens in bridge storage.

---

## 4. Verification Boundary: Dry-Run vs. Live Deployment

| Target | Local Deterministic / Dry-Run | External Live Validation |
|--------|-------------------------------|--------------------------|
| Cloud Run Manifest | PASS (`TestCloudRunManifestValidation`) | `false` (Requires live Cloud Run deployment) |
| Kubernetes Manifest | PASS (`TestKubernetesManifestValidation`) | `false` (Requires live GKE cluster) |
| PostgreSQL Multi-Process | PASS (`go test ./integration`) | `true` (Runs against real PostgreSQL 15) |
| Google Token Exchange | PASS (Deterministic fake Google JWKS) | `false` (Requires live Google STS / GE environment) |

Live qualification runs must record output according to `docs/evidence-template.md`.
