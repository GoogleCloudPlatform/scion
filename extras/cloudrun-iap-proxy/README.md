# Cloud Run IAP Reverse Proxy

A lightweight Go reverse proxy designed to run on Google Cloud Run to front internal VPC services (such as a Scion Hub instance running on a GCE VM) while preserving Identity-Aware Proxy (IAP) headers and supporting HTTP/2 streaming.

## Features

- **IAP Header Preservation:** Forwards and logs incoming Google Identity-Aware Proxy assertion headers:
  - `X-Goog-IAP-JWT-Assertion`
  - `X-Goog-Authenticated-User-Email`
  - `X-Goog-Authenticated-User-Id`
- **Host Header Rewriting:** Sets `req.Host` to the target host to properly route to virtual hosts on the backend.
- **HTTP/2 (H2C) Support:** Uses `golang.org/x/net/http2/h2c` to support HTTP/2 cleartext multiplexing on Cloud Run, crucial for high-concurrency event streams and Server-Sent Events (SSE).
- **Health Check Endpoint:** Provides `/proxy-healthz` for Cloud Run and external uptime checks without proxying back to the target service.
- **Direct VPC Egress Ready:** Easily deployed with Cloud Run Direct VPC Egress to reach internal compute instances on private RFC 1918 IPs.

## Architecture

```
[User Browser / Client]
          │
          ▼ (HTTPS)
 [Cloud Run (IAP Proxy)]
          │
          ▼ (Direct VPC Egress / Private IP)
   [Scion Hub (GCE VM)]
```

## Configuration

| Environment Variable | Description | Default | Required |
|----------------------|-------------|---------|----------|
| `TARGET_URL` | Destination backend URL (e.g., `http://10.128.0.2:8080`) | — | **Yes** |
| `PORT` | Listening port for the proxy server | `8080` | No |

## Deployment

### Prerequisites

- `gcloud` CLI authenticated with permissions to deploy Cloud Run services and configure VPC access.
- An existing internal service (e.g. Scion Hub on GCE) with internal IP and port reachable within the VPC network.

### Using `deploy.sh`

```bash
export TARGET_URL="http://<INTERNAL_IP>:8080"
export PROJECT_ID="my-gcp-project"
export REGION="us-central1"
export SERVICE_NAME="scion-hub-iap-proxy"

./deploy.sh
```

### Manual Deployment via `gcloud`

```bash
gcloud run deploy scion-hub-iap-proxy \
    --project="my-gcp-project" \
    --region="us-central1" \
    --source="." \
    --set-env-vars="TARGET_URL=http://<INTERNAL_IP>:8080" \
    --network="default" \
    --subnet="default" \
    --vpc-egress="all-traffic" \
    --allow-unauthenticated \
    --port=8080
```

## Development and Testing

Run unit tests locally:

```bash
go test -v ./...
```

Run proxy locally against a local backend:

```bash
TARGET_URL="http://localhost:8080" PORT=9000 go run .
```

## Forwarded headers

The proxy is a trust boundary for the Scion Hub: it removes every `Forwarded` and `X-Forwarded-*` header a client sends and states only what it observed. `X-Forwarded-Proto` is the scheme the proxy served on that connection: `https` only when it terminated TLS itself, otherwise `http`. `X-Forwarded-For` is its immediate peer, and `X-Forwarded-Host` is the host it served.

On Cloud Run the platform front end terminates TLS and the proxy serves plain HTTP, so it reports `http`. The proxy cannot verify the front end's TLS, so a Hub that lists this proxy in `server.trusted_proxies` refuses Runtime Broker registration and join-token minting, join, secret rotation and broker connections that come through it. Agent and web traffic are not affected. Runtime Brokers authenticate with HMAC and normally reach the Hub directly, not through the IAP front door.
