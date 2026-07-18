---
title: Proxy Auth (Google IAP)
description: Deploying the Scion Hub behind Google IAP with transport auth for agents.
---

This guide covers deploying a Scion Hub behind **Google Cloud Identity-Aware Proxy (IAP)**, using IAP for human authentication and hub-minted OIDC tokens for agent transport auth.

## Authentication modes

The Hub supports three **mutually exclusive** human authentication modes, selected by `auth.mode`:

| Mode | Use case |
|------|----------|
| `oauth` (default) | Hub runs its own OAuth flows (Google / GitHub). |
| `proxy` | Hub sits behind a trusted authenticating proxy (Google IAP, Cloudflare Access, etc.). |
| `dev` | Single-user local development with auto-generated dev tokens. |

Only one mode is active at a time. When `auth.mode` is `proxy`, the OAuth login UI, `/auth/providers`, and device-flow handlers are disabled. Human identity is derived entirely from the proxy's verified assertion.

Choose **proxy / IAP** when the Hub is already fronted by IAP (e.g., on Cloud Run with IAP enabled, or behind a GCE/GKE IAP-protected backend service) and you want to eliminate a separate OAuth integration.

## Inbound: human IAP authentication

### How it works

1. A user's browser request passes through IAP, which authenticates the user and injects a **signed JWT** in the `X-Goog-IAP-JWT-Assertion` header.
2. The Hub verifies the JWT signature (ES256, via Google's JWKS endpoint), validates `iss`, `aud`, and `exp` claims, then extracts the user's email from the verified assertion.
3. On first verified request, the Hub **provisions** the user — applying the same access controls as the OAuth path (`user_access_mode`, `authorized_domains`, `admin_emails`). If the user is not permitted, the request is rejected with 403.
4. Suspended users are rejected regardless of IAP status.

The unsigned convenience headers `X-Goog-Authenticated-User-Email` and `X-Goog-Authenticated-User-Id` are **ignored** — only the cryptographically signed assertion is trusted.

### Middleware precedence

The proxy authenticator runs **after** higher-priority app-layer credentials:

1. Agent token (`X-Scion-Agent-Token` / agent JWT)
2. Broker HMAC (`X-Scion-Broker-ID`)
3. Bearer token (dev token / UAT / user JWT)
4. **Proxy authenticator** (IAP assertion) — runs only when no app-layer credential matched

This means agents and brokers traversing IAP are identified by their own credentials, not by the IAP service-account assertion.

### Configuration

In `settings.yaml` (under the `server` key):

```yaml
server:
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        # MANDATORY — the IAP audience for your backend.
        # GCE/GKE backend service format:
        #   /projects/<PROJECT_NUMBER>/global/backendServices/<BACKEND_SERVICE_ID>
        # App Engine format:
        #   /projects/<PROJECT_NUMBER>/apps/<PROJECT_ID>
        audience: "/projects/123456789/global/backendServices/987654321"

        # Optional overrides (defaults are correct for production IAP):
        # issuer: "https://cloud.google.com/iap"
        # jwks_url: "https://www.gstatic.com/iap/verify/public_key-jwk"

      # Optional defense-in-depth: also verify source IP is a trusted proxy.
      # Uses the existing trusted_proxies CIDR list.
      require_trusted_proxy_ip: false

    # Access controls — same as for OAuth mode:
    user_access_mode: domain_restricted  # open | domain_restricted | invite_only
    authorized_domains:
      - example.com
    # admin_emails is set at the hub level:
  hub:
    admin_emails:
      - admin@example.com
```

#### IAP audience format

The `audience` value must match the audience claim (`aud`) in the IAP-signed JWT. The format depends on the backend type:

- **GCE/GKE backend service**: `/projects/<PROJECT_NUMBER>/global/backendServices/<BACKEND_SERVICE_ID>`
- **App Engine**: `/projects/<PROJECT_NUMBER>/apps/<PROJECT_ID>`

You can find this value in the Google Cloud Console under **Security → Identity-Aware Proxy** → select your backend → **Signed Header JWT Audience**.

#### Issuer and JWKS overrides

The defaults match Google's production IAP:

| Field | Default |
|-------|---------|
| `issuer` | `https://cloud.google.com/iap` |
| `jwks_url` | `https://www.gstatic.com/iap/verify/public_key-jwk` |

Override these only for testing with a mock IAP issuer.

### User provisioning

Provisioning in proxy mode works identically to OAuth — lazy, allow-list-gated, auto-create on first verified request:

- **`open`**: any verified email is allowed.
- **`domain_restricted`**: email domain must be in `authorized_domains`.
- **`invite_only`**: email must be pre-registered (via admin invite-code flow).
- Emails in `admin_emails` are always allowed and auto-promoted to admin role.
- If not permitted, the request returns **403**.
- Suspended users are rejected even though IAP authenticates them upstream.

A **60-second resolution cache** (keyed by verified email) avoids a database lookup on every request. The JWT signature is verified on every request — only the provisioning/store lookup is cached.

### Logout behavior

In proxy mode, the Hub does not own the session. The `/auth/logout` endpoint:

- **Browser requests**: redirect to `/_gcp_iap/clear_login_cookie` (IAP's cookie-clearing endpoint).
- **API requests**: return `200 OK` with `{"success": true, "message": "proxy mode: session is managed by the authenticating proxy"}`.

## Outbound: agent transport auth

When the Hub is behind IAP (or a Cloud Run invoker-only service), agents need a way to reach the Hub through the platform guard. This is solved with a **dual-layer credential model**:

| Layer | Header | Purpose |
|-------|--------|---------|
| **Outer (transport)** | `Authorization: Bearer <Google OIDC ID token>` | Satisfies the platform guard (IAP or Cloud Run invoker IAM check). |
| **Inner (app)** | `X-Scion-Agent-Token: <scion JWT>` | Existing Hub agent authentication. Carried as a custom header so it never collides with the outer `Authorization`. |

### How it works

1. **Cold start (dispatch)**: The Hub mints an initial Google OIDC ID token (impersonating a dedicated transport service account) and includes it in the agent's dispatch payload as environment variables.
2. **Steady-state refresh**: The agent piggybacks on its existing scion-token refresh cycle. The refresh response includes a `tokens[]` array with both the new scion access token and a fresh OIDC transport token. The agent applies each token to the appropriate layer.
3. **Background ticker**: The agent-side client drives refresh on the shortest-lived token (transport tokens have a 5-minute refresh margin vs. the ~1h Google ID token TTL).

### Dispatch environment variables

When transport auth is configured, the Hub injects these environment variables into the agent container at dispatch time:

| Variable | Description |
|----------|-------------|
| `SCION_TRANSPORT_TOKEN` | Initial Google OIDC ID token for the transport layer. |
| `SCION_TRANSPORT_AUDIENCE` | Audience the transport token was minted for (IAP client ID or hub URL). |
| `SCION_TRANSPORT_TOKEN_EXPIRY` | Token expiry in RFC 3339 format. |

### Refresh response: `tokens[]` array

The agent token refresh endpoint (`POST /api/v1/agents/{id}/token/refresh`) returns a generalized `tokens[]` array alongside the legacy single-token fields for backward compatibility:

```json
{
  "token": "...",
  "expires_at": "2026-06-05T12:00:00Z",
  "tokens": [
    {
      "layer": "app",
      "type": "scion_access",
      "value": "...",
      "expiresIn": 900
    },
    {
      "layer": "transport",
      "type": "google_oidc",
      "value": "...",
      "expiresIn": 3600,
      "audience": "1234567890.apps.googleusercontent.com"
    }
  ]
}
```

The `transport` entry is only present when `auth.transport` is configured on the Hub. Old clients ignore `tokens[]`; new clients consume both layers.

### Agent-side token source selection

The agent (`pkg/sciontool/hub`) selects an OIDC token source automatically:

1. **`SCION_TRANSPORT_TOKEN` env var set** → **Injected mode**: uses the hub-provided token from dispatch, refreshed via `tokens[]` on subsequent refresh calls.
2. **Running on GCP (metadata server available)** → **Metadata mode**: fetches OIDC from the GCE metadata server using the ambient SA identity (the PR #307 pattern). Audience is set via `SCION_HUB_OIDC_AUDIENCE` or defaults to the hub URL.
3. **Neither** → No OIDC transport (agent uses plain HTTP).

Injected mode (option 1) is the recommended path for IAP deployments — it decouples agent transport auth from the agent's own GCP identity.

### Transport configuration

```yaml
server:
  auth:
    transport:
      # Transport auth mode:
      #   none (default) — no transport tokens issued
      #   cloudrun_invoker — audience = hub URL
      #   iap — audience = IAP OAuth client ID
      mode: iap

      # OIDC audience for the transport token.
      # For IAP:              the IAP OAuth client ID (e.g., "1234567890.apps.googleusercontent.com")
      # For cloudrun_invoker: the hub URL (auto-derived from hub.public_url if empty)
      oidc_audience: "1234567890.apps.googleusercontent.com"

      # Dedicated service account for transport-layer auth.
      # The hub's runtime SA impersonates this SA to mint OIDC ID tokens.
      platform_auth_sa: "scion-transport@my-project.iam.gserviceaccount.com"
```

#### What audience to set

| Transport mode | `oidc_audience` value |
|---------------|----------------------|
| `iap` | The **IAP OAuth client ID** (found in Cloud Console → Security → IAP → your backend → OAuth client). Format: `<client-id>.apps.googleusercontent.com` |
| `cloudrun_invoker` | The **Hub's URL** (e.g., `https://hub.example.com`). If left empty, derived from `hub.public_url`. |

:::note
When both IAP and Cloud Run invoker guards are present on the same service, the IAP service agent carries the Cloud Run invoker role automatically. Agents send a single outer token targeting the IAP audience — no three-layer case.
:::

### Hub-managed transport SA (Option C)

The Hub uses a dedicated service account solely for transport-layer auth. The Hub's runtime SA impersonates this SA via the IAM Credentials API (`generateIdToken`) to mint OIDC ID tokens for agents. This design:

- Keeps the auth-grade minting capability in the Hub only — agents hold no SA credential.
- Works regardless of the agent's GCP metadata mode (`block`, `passthrough`, or `assign`).
- Avoids distributing service account key files.

**Required IAM bindings:**

| Principal | Role | Target |
|-----------|------|--------|
| Hub's runtime SA | `roles/iam.serviceAccountTokenCreator` | Transport SA (`platform_auth_sa`) |
| Transport SA | IAP-secured web user **or** Cloud Run invoker | The Hub's backend service |

## Security notes

1. **Only the signed assertion is trusted.** The unsigned `X-Goog-Authenticated-User-Email` and `X-Goog-Authenticated-User-Id` headers are completely ignored.
2. **Audience binding is mandatory.** Without it, a JWT minted for a different IAP-protected service would be accepted. The `auth.proxy.iap.audience` field must always be set.
3. **The Hub must be reachable only through IAP for the human surface.** Any path that reaches the Hub directly could bypass proxy authentication. The verified-JWT path is safe against header spoofing (forged assertions fail the signature check), but direct access bypasses IAP entirely. Use VPC networking, firewall rules, or Cloud Run ingress settings to enforce this.
4. **JWKS key rotation** is handled automatically: keys are cached with hourly background refresh and on-miss refresh for rotated key IDs. Transient JWKS endpoint failures are tolerated by serving the last-good key set.
5. **Clock skew** of ±30 seconds is allowed on `exp` and `iat` claims.
6. **Suspended users** are rejected at the provisioning layer even though IAP still authenticates them upstream.

## End-to-end GCP setup checklist

### Prerequisites

- A GCP project with billing enabled.
- The Hub deployed on Cloud Run (or behind a GCE/GKE load balancer).
- `gcloud` CLI configured with appropriate permissions.

### 1. Enable IAP and create an OAuth consent screen

```bash
# Enable the IAP API
gcloud services enable iap.googleapis.com

# Configure the OAuth consent screen (if not already done)
# Go to: Console → APIs & Services → OAuth consent screen
```

### 2. Enable IAP on the backend service

```bash
# For Cloud Run behind a load balancer:
gcloud iap web enable \
  --resource-type=backend-services \
  --service=YOUR_BACKEND_SERVICE_NAME
```

Note the **IAP OAuth client ID** (found in Console → Security → IAP → your backend → click the three dots → Edit OAuth Client). You will need it for both `auth.proxy.iap.audience` and `auth.transport.oidc_audience`.

Note the **signed header JWT audience** (found in Console → Security → IAP → your backend). This goes into `auth.proxy.iap.audience`.

### 3. Create the transport service account

```bash
# Create a dedicated SA for transport auth
gcloud iam service-accounts create scion-transport \
  --display-name="Scion Transport Auth"

# Grant the Hub's runtime SA permission to impersonate the transport SA
gcloud iam service-accounts add-iam-policy-binding \
  scion-transport@PROJECT_ID.iam.gserviceaccount.com \
  --member="serviceAccount:HUB_RUNTIME_SA@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/iam.serviceAccountTokenCreator"
```

### 4. Grant the transport SA access to the platform guard

For **IAP**:
```bash
# Grant IAP-secured web user access to the transport SA
gcloud iap web add-iam-policy-binding \
  --resource-type=backend-services \
  --service=YOUR_BACKEND_SERVICE_NAME \
  --member="serviceAccount:scion-transport@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/iap.httpsResourceAccessor"
```

For **Cloud Run invoker**:
```bash
gcloud run services add-iam-policy-binding YOUR_SERVICE_NAME \
  --member="serviceAccount:scion-transport@PROJECT_ID.iam.gserviceaccount.com" \
  --role="roles/run.invoker" \
  --region=YOUR_REGION
```

### 5. Configure the Hub

Create or update the `settings.yaml`:

```yaml
schema_version: "1"
server:
  mode: hosted
  hub:
    public_url: "https://hub.example.com"
    admin_emails:
      - admin@example.com
  auth:
    mode: proxy
    proxy:
      provider: iap
      iap:
        audience: "/projects/123456789/global/backendServices/987654321"
    transport:
      mode: iap
      oidc_audience: "1234567890.apps.googleusercontent.com"
      platform_auth_sa: "scion-transport@my-project.iam.gserviceaccount.com"
    user_access_mode: domain_restricted
    authorized_domains:
      - example.com
  database:
    driver: postgres
    url: "postgres://..."
```

### 6. Verify

1. Access the Hub URL in a browser — IAP should prompt for Google login, then the Hub should show your identity.
2. Dispatch an agent and verify it can communicate back to the Hub (check agent logs for OIDC transport messages).
3. Check Hub logs for `Proxy auth configured: provider=iap` and `Transport auth configured: mode=iap` at startup.

### Reference scripts

The `scripts/cloudrun/` directory on the `pr/cloudrun-hub` branch contains reference deployment scripts (deploy.sh, entrypoint.sh, hub-settings-template.yaml) for a Cloud Run + IAP topology that can serve as a starting point.

## Brokers behind IAP

When the Hub is behind IAP, **Runtime Brokers** must also carry a transport-layer OIDC token on every request — just like agents do. However, brokers are long-lived *originators*: nothing injects a transport token into them, so they must mint their own OIDC tokens from their runtime identity (GKE Workload Identity or ambient GCE service account).

This section covers the deployment and configuration steps to connect brokers to an IAP-protected Hub.

### Custom OAuth 2.0 Client ID requirement

The Google-managed OAuth client that Cloud Run auto-provisions when IAP is enabled **does not support programmatic (service-account) authentication**. This means brokers (and any other machine client) cannot use it to mint OIDC tokens.

You must:

1. **Create a custom OAuth 2.0 Client ID** in the Google Cloud Console under **APIs & Services → Credentials → Create Credentials → OAuth client ID** (application type: Web application).
2. **Bind the custom client ID to the IAP settings** for your Hub's backend service (Console → Security → IAP → select backend → Edit OAuth Client → Use custom client).

The custom client ID (e.g., `1234567890-abc.apps.googleusercontent.com`) becomes the **OIDC audience** for all machine clients — both agents and brokers.

:::note
This is the same audience value used in `auth.transport.oidc_audience` in the Hub's `settings.yaml`. See [Transport configuration](#transport-configuration) above.
:::

### Broker Workload Identity setup

Brokers running in GKE use [Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity) to mint OIDC tokens from the GCE metadata server.

1. **Create a Google Service Account (GSA)** for brokers:
   ```bash
   gcloud iam service-accounts create scion-broker \
     --display-name="Scion Broker"
   ```

2. **Grant the GSA access to traverse the platform guard.** The required role depends on the transport mode:

   | Transport mode | Role | Target |
   |---|---|---|
   | `iap` | `roles/iap.httpsResourceAccessor` | Hub backend service |
   | `cloudrun_invoker` | `roles/run.invoker` | Hub Cloud Run service |

   For IAP:
   ```bash
   gcloud iap web add-iam-policy-binding \
     --resource-type=backend-services \
     --service=YOUR_BACKEND_SERVICE_NAME \
     --member="serviceAccount:scion-broker@PROJECT_ID.iam.gserviceaccount.com" \
     --role="roles/iap.httpsResourceAccessor"
   ```

3. **Bind the broker's Kubernetes Service Account (KSA) to the GSA** via the Workload Identity annotation:
   ```bash
   # Allow the KSA to impersonate the GSA
   gcloud iam service-accounts add-iam-policy-binding \
     scion-broker@PROJECT_ID.iam.gserviceaccount.com \
     --member="serviceAccount:PROJECT_ID.svc.id.goog[NAMESPACE/BROKER_KSA_NAME]" \
     --role="roles/iam.workloadIdentityUser"

   # Annotate the KSA
   kubectl annotate serviceaccount BROKER_KSA_NAME \
     --namespace NAMESPACE \
     iam.gke.io/gcp-service-account=scion-broker@PROJECT_ID.iam.gserviceaccount.com
   ```

The broker can now mint OIDC ID tokens for the configured audience via the metadata server, which the transport layer uses automatically.

### Broker transport configuration

Broker transport auth has two configuration layers: **environment variables** (for containerized brokers in Kubernetes) and **credentials-file fields** (for per-connection config).

#### Environment variables

Set these on the broker's Kubernetes Deployment:

| Variable | Description |
|---|---|
| `SCION_TRANSPORT_MODE` | Transport mode: `iap` or `cloudrun_invoker` |
| `SCION_TRANSPORT_AUDIENCE` | OIDC audience — the custom OAuth 2.0 Client ID (for `iap` mode) or the Hub URL (for `cloudrun_invoker` mode) |

```yaml
env:
  - name: SCION_TRANSPORT_MODE
    value: "iap"
  - name: SCION_TRANSPORT_AUDIENCE
    value: "1234567890-abc.apps.googleusercontent.com"
```

#### Credentials-file fields

The broker credentials file (written by `scion hub brokers register`) can also store transport settings per hub connection:

```json
{
  "brokerId": "...",
  "secretKey": "...",
  "hubEndpoint": "https://hub.example.com",
  "transportMode": "iap",
  "transportAudience": "1234567890-abc.apps.googleusercontent.com"
}
```

**Environment variables override credentials-file values.** This allows Kubernetes Deployment manifests to set transport config declaratively while the credentials file retains the values persisted at registration time.

:::tip[Multi-hub brokers]
Per-connection placement in the credentials file exists for the **multi-hub scenario**: each hub connection can have its own `transportMode` and `transportAudience` (different IAP OAuth client IDs). A single broker can serve both IAP-protected and plain hubs simultaneously. See [Multi-Broker Setup](/scion/hosted/ha/multi-broker/) for details.
:::

### Broker registration without PAT (proxy-auth mode)

With transport auth configured, `scion hub brokers register` works through IAP natively — no Personal Access Token (PAT) or hub token is needed. The broker authenticates via the IAP assertion of its service account identity.

When the Hub is in `proxy` auth mode and the broker has a valid transport token source (Workload Identity), the registration command:

1. Sends the OIDC transport token in the `Authorization` header to traverse IAP.
2. IAP verifies the token and passes the request through to the Hub.
3. The Hub identifies the caller via the IAP assertion (`X-Goog-IAP-JWT-Assertion`) and completes the registration.

This retires the manual `install-broker.sh` curl-from-a-pod workaround.

The `register` command also persists `transportMode` and `transportAudience` into the credentials file automatically, so the broker daemon inherits them on startup.

### Registration Job manifest

Instead of manual shell scripts, use a Kubernetes Job to register the broker. The Job runs with the broker's KSA (which has Workload Identity configured) and the transport environment variables:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: register-broker
  namespace: scion
spec:
  template:
    metadata:
      labels:
        app: scion-broker-register
    spec:
      serviceAccountName: scion-broker  # KSA with Workload Identity annotation
      restartPolicy: Never
      containers:
        - name: register
          image: YOUR_SCION_IMAGE
          env:
            # Env vars enable the scion binary to traverse IAP for the
            # registration HTTP request itself.
            - name: SCION_TRANSPORT_MODE
              value: "iap"
            - name: SCION_TRANSPORT_AUDIENCE
              value: "1234567890-abc.apps.googleusercontent.com"
          volumeMounts:
            - name: broker-credentials
              mountPath: /home/scion/.scion
          command:
            - scion
            - hub
            - brokers
            - register
            - --name
            - my-broker
            # CLI flags ensure transport values are persisted to the
            # credentials file for the broker daemon to inherit.
            - --transport-mode
            - iap
            - --transport-audience
            - "1234567890-abc.apps.googleusercontent.com"
            - https://hub.example.com
      volumes:
        # The credentials file must persist beyond the Job pod so the
        # broker Deployment can read it. Use a PVC, a Secret, or any
        # shared volume accessible to the broker Deployment.
        - name: broker-credentials
          persistentVolumeClaim:
            claimName: broker-credentials  # replace with your PVC
  backoffLimit: 2
```

After the Job completes, the credentials file is written to the shared volume. The broker Deployment (mounting the same volume, with the same KSA and transport env vars) picks up the credentials on startup.

### GKE deployment summary

| Step | What |
|---|---|
| 1 | Create a custom OAuth 2.0 Client ID; bind it to the Hub service's IAP settings |
| 2 | Create a broker GSA; grant `roles/iap.httpsResourceAccessor` on the Hub backend service |
| 3 | Bind KSA ↔ GSA via Workload Identity annotation on the broker's Kubernetes service account |
| 4 | Broker Deployment env: `SCION_TRANSPORT_MODE=iap`, `SCION_TRANSPORT_AUDIENCE=<custom client id>` |
| 5 | One-time registration Job (same KSA) runs `scion hub brokers register` — no curl scripts |
