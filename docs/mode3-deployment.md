# Mode 3 (HA) Deployment Guide

Mode 3 runs chat integrations as standalone services on separate compute, with
Postgres-backed state and advisory-lock failover. The hub manages them via gRPC
and a shared Postgres signal plane.

## Prerequisites

- Hub running with `database.driver: postgres`
- Integration configured with `mode: grpc` and its `address` pointing to the
  standalone service
- Shared Postgres instance reachable by both hub and integration

## Kubernetes Deployment

### Integration Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: scion-discord-ha
spec:
  replicas: 2
  selector:
    matchLabels:
      app: scion-discord
  template:
    metadata:
      labels:
        app: scion-discord
    spec:
      containers:
        - name: discord
          image: scion-discord:latest
          args: ["--standalone"]
          ports:
            - containerPort: 9090
              name: grpc
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: scion-postgres
                  key: url
          livenessProbe:
            grpc:
              port: 9090
            initialDelaySeconds: 10
          readinessProbe:
            grpc:
              port: 9090
            initialDelaySeconds: 5
```

### Integration Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: scion-discord
spec:
  selector:
    app: scion-discord
  ports:
    - port: 9090
      targetPort: grpc
      protocol: TCP
```

### Hub Configuration

In the hub's `settings.yaml`:

```yaml
plugins:
  broker:
    discord:
      mode: grpc
      address: scion-discord:9090
```

### Advisory Lock Failover

Both replicas attempt to acquire a Postgres advisory lock. Exactly one holds it
and opens the Discord Gateway WebSocket. The other waits in standby. If the
primary dies, its Postgres session closes, the lock releases, and the standby
acquires it within 30-60 seconds.

## Cloud Run Deployment

### Required Settings

Cloud Run's serverless model conflicts with persistent connections (Gateway
WebSocket, LISTEN/NOTIFY, advisory locks). These settings are mandatory:

| Setting | Value | Why |
|---------|-------|-----|
| `--min-instances` | `1` | Advisory lock requires a live session; scale-to-zero drops it |
| `--cpu-always-allocated` | (flag) | Background goroutines (Gateway, LISTEN loop) starve under request-only CPU |
| `--session-affinity` | (flag) | Prevents mid-conversation request routing away from the lock holder |

### Deploy Command

```bash
gcloud run deploy scion-discord \
  --image gcr.io/PROJECT/scion-discord:latest \
  --min-instances 1 \
  --cpu-always-allocated \
  --session-affinity \
  --port 9090 \
  --use-http2 \
  --set-env-vars "DATABASE_URL=..." \
  --args "--standalone"
```

### gRPC Health Probes

Cloud Run uses gRPC health checks automatically when `--use-http2` is set. The
standalone integration serves `grpc.health.v1.Health` on the same port.

### Hub-to-Cloud-Run Auth

When the hub dials a Cloud Run service, configure IAM-based per-call
authentication. The hub's service account needs the `roles/run.invoker` role on
the integration's Cloud Run service.

In `settings.yaml`:

```yaml
plugins:
  broker:
    discord:
      mode: grpc
      address: scion-discord-HASH-uc.a.run.app:443
```

## Update Flow

1. Admin clicks "Update" in the UI
2. Hub inserts an `integration_updates` row and sends a NOTIFY
3. The integration's signal listener picks up the update signal
4. The integration executes its `update_hook` (or exits for platform restart)
5. After restart, the integration reconnects via gRPC
6. Hub compares the new version against the pre-update version
7. If changed: marks update as `completed`; if unchanged after 10 minutes:
   marks as `failed`

## Troubleshooting

**Integration not connecting**: verify the `address` in hub settings resolves to
the integration's gRPC port. For Cloud Run, ensure `--use-http2` is set.

**Failover too slow**: check Postgres TCP keepalives. The advisory lock releases
when the Postgres session closes, which depends on TCP timeout detection.

**Update stuck in "requested"**: the integration's LISTEN connection may have
dropped. It will re-scan on reconnect, but verify the integration is running and
has Postgres connectivity.
