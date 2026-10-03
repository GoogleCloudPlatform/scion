# Deployment & Live Environment Qualification Evidence Template

This template records evidence for staging and production qualification runs of the Scion A2A Bridge across Google Cloud Run and Kubernetes clusters.

---

## 1. Metadata
- **Date & Time (UTC):** `[YYYY-MM-DDTHH:MM:SSZ]`
- **Operator / Agent:** `[Identifier]`
- **Target Platform:** `[Cloud Run | Kubernetes / GKE]`
- **Target Git SHA:** `[Commit SHA]`
- **Container Image Digest:** `[gcr.io/...@sha256:...]`
- **PostgreSQL Version & Host:** `[PostgreSQL 15.x on Cloud SQL / AlloyDB / RDS]`

---

## 2. Pre-Deployment Configuration Audit
- [ ] Database URL verified reachable with active schema migrations
- [ ] Minimum replica / instance count >= 2 verified
- [ ] `GRPC_AUTH_MODE=google_id_token` configured
- [ ] `GRPC_AUTH_AUDIENCE` matches deployment endpoint exactly
- [ ] `GRPC_AUTH_SUBJECTS` restricts control RPCs to Hub SA only
- [ ] GE invoker SA granted platform invocation only (`roles/run.invoker`)
- [ ] Short-lived tokens only (no refresh tokens configured)

---

## 3. Deployment Execution Log
```
[Paste gcloud run deploy or kubectl apply output here]
```

---

## 4. Live Health & Traffic Verification
- **Probe Check:**
  ```
  [Paste curl /healthz or probe status output]
  ```
- **Dual-Header Verification:**
  ```
  [Paste verification proving X-Serverless-Authorization stripped and Authorization verified]
  ```
- **gRPC Principal Isolation Check:**
  - Call from Hub SA: `OK`
  - Call from GE Invoker SA: `PermissionDenied`
  - Call without Auth: `Unauthenticated`

---

## 5. Failover & Lease Handover Demonstration
- Kill active instance / replica claiming execution lease
- Observe passive replica detecting stale lease after cutoff
- Verify state transition to `TASK_STATE_FAILED` with zero replay

---

## 6. Rollback & Cleanup Demonstration
- **Rollback execution:**
  ```
  [Paste rollback command and traffic confirmation]
  ```
- **Cleanup verification:**
  ```
  [Paste resource cleanup and database sentinel checks]
  ```
