# gRPC Transport Authentication — Hub-to-Bridge

This document describes how to configure authenticated gRPC transport between
the Scion Hub and the A2A bridge in standalone (HA) mode. It covers Cloud Run
and Kubernetes deployments.

## Overview

When the bridge runs in standalone mode (`--standalone` or `A2A_STANDALONE=true`),
the Hub connects to it over gRPC using `mode: grpc` in the plugin configuration.
The Hub is the gRPC **client**; the bridge is the gRPC **server**.

Three network paths exist in the system:

| Path | Protocol | Identity |
|------|----------|----------|
| Client → Bridge | A2A JSON-RPC over HTTP | End-user credential |
| Bridge → Hub | REST | Hub-issued user credential |
| **Hub → Bridge** | **gRPC BrokerService** | **Hub service identity** |

This document covers the third path. Service invoker credentials (Hub→Bridge
gRPC) are distinct from end-user OAuth credentials and must remain separate.

## Hub Configuration (`settings.yaml`)

### Basic gRPC Mode

```yaml
server:
  message_broker:
    enabled: true
    types:
      - a2a-bridge          # must list the plugin name

  plugins:
    broker:
      a2a-bridge:
        mode: grpc
        address: "bridge.example.com:443"
```

### With Google ID Token Authentication (Cloud Run)

```yaml
server:
  plugins:
    broker:
      a2a-bridge:
        mode: grpc
        address: "bridge-abc123-uc.a.run.app:443"
        auth_type: google_id_token
        auth_audience: "https://bridge-abc123-uc.a.run.app"
```

The Hub uses the GCE metadata server (on GCE) or Application Default
Credentials (off-GCE) to obtain audience-correct Google OIDC ID tokens.
Tokens are cached and refreshed automatically before expiry — no restart
or secret rotation is needed.

### With TLS for Kubernetes

```yaml
server:
  plugins:
    broker:
      a2a-bridge:
        mode: grpc
        address: "a2a-bridge.scion.svc.cluster.local:9090"
        auth_type: google_id_token
        auth_audience: "https://a2a-bridge.scion.svc.cluster.local"
        # TLS fields for verifying the bridge's server certificate
        tls_ca_file: "/etc/scion/certs/ca.pem"
        # For mTLS — client cert presented by the Hub
        tls_cert_file: "/etc/scion/certs/hub-client.pem"
        tls_key_file: "/etc/scion/certs/hub-client-key.pem"
```

## Configuration Reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mode` | string | yes | Must be `"grpc"` for standalone gRPC mode. |
| `address` | string | yes | gRPC server address (host:port). |
| `auth_type` | string | no | Per-RPC auth mode: `""` / `"none"` (no auth), `"google_id_token"`. |
| `auth_audience` | string | conditional | Target audience for ID tokens. Required when `auth_type` is `"google_id_token"`. |
| `tls_ca_file` | string | no | CA certificate for verifying the server's TLS certificate. |
| `tls_cert_file` | string | no | Client TLS certificate for mTLS. |
| `tls_key_file` | string | no | Client TLS private key for mTLS. |
| `tls_skip_verify` | bool | no | Disable TLS certificate verification (**development only**). |

## Cloud Run Deployment

### Architecture

On Cloud Run, TLS is terminated by the platform at the ingress layer.
The bridge listens on a single port using h2c (HTTP/2 cleartext), and
Cloud Run's load balancer multiplexes both gRPC and HTTP traffic on the
same endpoint.

```
┌──────┐  TLS  ┌─────────────┐  h2c  ┌────────┐
│ Hub  │──────→│ Cloud Run   │──────→│ Bridge │
│(gRPC)│       │ Ingress/LB  │       │ Server │
└──────┘       └─────────────┘       └────────┘
```

### Key Points

1. **Audience**: Set `auth_audience` to the Cloud Run service URL
   (e.g., `https://bridge-abc123-uc.a.run.app`). The Hub mints an ID token
   with this audience, and Cloud Run validates it as the invoker credential.

2. **X-Serverless-Authorization**: This header is used by the GE HTTP ingress
   path (client→bridge) and is consumed by Cloud Run's platform. It is
   **distinct** from the gRPC `authorization` metadata used by the Hub→Bridge
   path. Do not conflate them.

3. **IAM invoker permission**: The Hub's service account must have the
   `roles/run.invoker` role on the bridge's Cloud Run service.

4. **No TLS fields needed**: Cloud Run terminates TLS. The Hub connects
   using the default system TLS for non-local addresses (no `tls_ca_file`
   or `tls_cert_file` needed).

5. **Single-port mux**: The bridge's h2c mux (selected via `K_SERVICE` or
   `MUX_PORTS`) routes gRPC and HTTP on the same port. Both paths are
   independently authenticated — gRPC via the bearer token interceptor,
   HTTP via the bridge's own auth middleware.

### Cloud Run Configuration Checklist

- [ ] Set `auth_type: google_id_token` in Hub settings
- [ ] Set `auth_audience` to the Cloud Run service URL
- [ ] Grant `roles/run.invoker` to the Hub's service account
- [ ] Enable `server.message_broker.types` with the plugin name
- [ ] Bridge's gRPC server validates tokens from incoming RPCs

## Kubernetes Deployment

### Architecture

On Kubernetes, TLS must be provided either by the bridge natively or by
an ingress controller/sidecar (e.g., Istio, Linkerd, or an nginx ingress).

```
Option A: Native TLS
┌──────┐  mTLS  ┌────────┐
│ Hub  │───────→│ Bridge │
│(gRPC)│        │(native)│
└──────┘        └────────┘

Option B: Ingress/Sidecar TLS
┌──────┐  TLS  ┌─────────┐  h2c  ┌────────┐
│ Hub  │──────→│ Ingress │──────→│ Bridge │
│(gRPC)│       │/ Sidecar│       │ Server │
└──────┘       └─────────┘       └────────┘
```

### Option A: Native Server TLS

The bridge starts its gRPC server with TLS certificates:

```bash
./scion-a2a-bridge --standalone \
  --grpc-tls-cert /etc/certs/server.pem \
  --grpc-tls-key /etc/certs/server-key.pem \
  --grpc-tls-client-ca /etc/certs/ca.pem   # for mTLS
```

Hub settings:

```yaml
a2a-bridge:
  mode: grpc
  address: "a2a-bridge.scion.svc.cluster.local:9090"
  auth_type: google_id_token
  auth_audience: "https://a2a-bridge.scion.svc.cluster.local"
  tls_ca_file: "/etc/scion/certs/ca.pem"
  tls_cert_file: "/etc/scion/certs/hub-client.pem"   # mTLS
  tls_key_file: "/etc/scion/certs/hub-client-key.pem" # mTLS
```

### Option B: Ingress/Sidecar TLS

When using a service mesh or ingress controller that terminates TLS:

1. Configure the ingress/sidecar to terminate TLS and forward traffic
   to the bridge's plaintext gRPC port.
2. The bridge runs without native TLS.
3. The Hub's `tls_ca_file` should reference the ingress/sidecar's CA.

### Security Requirements

The following failures must be **fail-closed** (connection refused, not
silently degraded):

| Scenario | Expected Behavior |
|----------|-------------------|
| Wrong CA certificate | TLS handshake fails |
| Wrong server identity (CN/SAN mismatch) | TLS handshake fails |
| Wrong client certificate (mTLS) | Server rejects connection |
| Missing client certificate (when mTLS required) | Server rejects connection |
| No TLS for remote address | Hub defaults to system TLS; plaintext rejected |
| Missing/wrong bearer token | gRPC returns `Unauthenticated` / `PermissionDenied` |

### Kubernetes Configuration Checklist

- [ ] Provision server TLS certificate (cert-manager, manual, or sidecar)
- [ ] Provision client TLS certificate for the Hub (if using mTLS)
- [ ] Set `tls_ca_file` on the Hub to the correct CA
- [ ] Set `auth_type: google_id_token` and `auth_audience`
- [ ] Verify fail-closed behavior for wrong CA/cert/token

## Bridge-Side Server Authentication

The bridge's gRPC server must validate incoming tokens on all control methods:

- **Configure** — administrative; must be authenticated
- **Publish** — delivers messages; must be authenticated
- **GetInfo** — returns metadata; must be authenticated
- **HealthCheck** — returns status; must be authenticated
- **Subscribe/Unsubscribe** — manages subscriptions; must be authenticated

When a `TokenValidator` is configured on the bridge's gRPC server, the
`UnaryAuthInterceptor` and `StreamAuthInterceptor` extract the bearer
token from the `authorization` metadata header and validate it before
allowing the RPC to proceed. Single-port h2c mode does not create an
unauthenticated branch — both the gRPC and HTTP paths are independently
protected.

## Credential Lifecycle

- **Refresh**: The Hub's `TokenSourceCredentials` wraps a
  `transportauth.TokenSource` that handles automatic refresh. The
  `MetadataSource` (GCE) and `ADCSource` (off-GCE) both cache tokens
  and refresh them before expiry.
- **Rotation**: When the underlying credential (SA key, Workload Identity
  binding) rotates, the token source automatically picks up new tokens
  on the next refresh. No restart required.
- **Reconnection**: On gRPC reconnect (server restart, network glitch),
  the adapter re-dials with the same `PerRPCCredentials`. Fresh tokens
  are obtained per-RPC, so reconnection is seamless.
- **No secret logging**: Token values are never logged. The adapter logs
  connection events (address, audience) but not token contents.

## Locally Verified vs. Unperformed Validation

| Behavior | Status |
|----------|--------|
| Auth interceptor rejects missing/wrong tokens | ✅ Verified (unit tests) |
| All 6 control methods protected | ✅ Verified (unit tests) |
| Token rotation without restart | ✅ Verified (unit tests) |
| Reconnect preserves auth | ✅ Verified (unit tests) |
| TLS with correct CA | ✅ Verified (unit tests) |
| mTLS with correct client cert | ✅ Verified (unit tests) |
| Wrong CA fails closed | ✅ Verified (unit tests) |
| Wrong client cert fails closed | ✅ Verified (unit tests) |
| Missing client cert fails closed | ✅ Verified (unit tests) |
| Factory wires auth from config | ✅ Verified (unit tests) |
| Cloud Run audience-correct ID token | ⚠️ Not live-validated (requires deployed Cloud Run service) |
| Cloud Run h2c mux routing | ⚠️ Not live-validated |
| Kubernetes native TLS end-to-end | ⚠️ Not live-validated (requires K8s cluster with certs) |
| GCE metadata server token fetch | ⚠️ Not live-validated (requires GCE instance) |
| ADC token fetch | ⚠️ Not live-validated (requires gcloud ADC login) |
