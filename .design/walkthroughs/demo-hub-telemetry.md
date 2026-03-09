# Walkthrough: Enabling Full OTEL Telemetry on Demo Hub GCE Instance

**Created:** 2026-02-26
**Status:** Blocked (agent-side cloud telemetry auth)
**Goal:** Enable and verify end-to-end OpenTelemetry on the `scion-demo` GCE
instance, covering both Hub-side telemetry (Cloud Logging, Cloud Trace) and
agent-side telemetry (settings-driven OTLP export).

This walkthrough covers the operational steps for the demo instance
(`scion-demo` in project `deploy-demo-test`). For general telemetry
architecture, see [metrics-system.md](../hosted/metrics-system.md). For
local development telemetry QA, see [telemetry-gcp.md](telemetry-gcp.md).

---

## Prerequisites

- GCP project `deploy-demo-test` (or your target project)
- `gcloud` CLI installed and authenticated with project access
- SSH access to the `scion-demo` instance (`gcloud compute ssh scion-demo --zone us-central1-a`)
- The instance was provisioned with `scripts/starter-hub/gce-demo-provision.sh`

---

## Blocking Conditions

The following issues must be resolved before agent-side cloud telemetry
export can work end-to-end. Hub-side Cloud Logging (via `cloud_handler.go`)
is unaffected — it uses the GCP client library which handles ADC natively.

### B1: OTLP exporter lacks GCP credential injection

**Affects:** Agent-side cloud export (sciontool), Hub-side OTel trace/log
export (`otel_provider.go`)

**Problem:** The cloud exporter in `pkg/sciontool/telemetry/exporter.go`
creates a plain `otlptracegrpc` exporter:

```go
exporter, err := otlptracegrpc.New(context.Background(), opts...)
```

This establishes a TLS gRPC connection but does **not** inject OAuth2
bearer tokens. Google Cloud's OTLP endpoint at
`cloudtrace.googleapis.com:443` requires OAuth2 authentication on every
RPC. The standard OTel OTLP gRPC SDK does not use GCP Application Default
Credentials — that is a convention honored only by GCP-specific client
libraries (e.g., `cloud.google.com/go`).

The same gap exists in the Hub-side OTel log provider
(`pkg/util/logging/otel_provider.go`), which also uses a plain
`otlploggrpc` exporter.

**Design reference:** [metrics-system.md §11.8](../hosted/metrics-system.md)
marks credential injection as "out of scope" and defers to the runtime
broker, but the broker does not currently address this.

**Resolution options:**

1. **Use `grpc.WithPerRPCCredentials`** — Fetch an OAuth2 token from ADC
   (via `golang.org/x/oauth2/google.DefaultTokenSource`) and inject it as
   a per-RPC credential on the gRPC dial options. This is the minimal fix.
2. **Use Google's `opentelemetry-operations-go` exporter** — The
   `github.com/GoogleCloudPlatform/opentelemetry-operations-go` package
   wraps the OTLP exporter with native GCP auth handling.
3. **Route through an OTel Collector sidecar** — Deploy an OTel Collector
   on the instance that receives OTLP locally (no auth required) and
   exports to Cloud Trace using its GCP exporter (which handles auth). This
   avoids modifying sciontool code but adds operational complexity.

**Who needs to act:** Sciontool telemetry implementation (exporter.go and
otel_provider.go). The runtime broker should also ensure ADC is available
inside agent containers — the harness already propagates
`GOOGLE_APPLICATION_CREDENTIALS` and mounts the credential file, but this
only helps if the exporter is updated to use it.

### B2: Hub OTEL env vars should use hub.env

**Affects:** Hub-side configuration management

**Problem:** Hub-side OTEL environment variables (`SCION_CLOUD_LOGGING`,
`SCION_OTEL_ENDPOINT`, etc.) are hardcoded as `Environment=` directives in
the systemd unit template within `gce-start-hub.sh`. The `hub.env` file
pattern is already implemented (the systemd unit uses
`EnvironmentFile=/home/scion/.scion/hub.env`), so these should be managed
there instead for easier configuration changes without re-deploying the
service file.

**Resolution:** Move the OTEL `Environment=` lines out of the systemd unit
template in `gce-start-hub.sh` and into `hub.env.example` as uncommented
defaults. Section 2.1 below documents the target state.

---

## 1. Verify GCP Configuration

### 1.1 Check enabled APIs

The provisioning script enables these APIs. Verify they're active:

```bash
export PROJECT_ID="deploy-demo-test"

gcloud services list --enabled --project "$PROJECT_ID" \
  --filter "name:(cloudtrace.googleapis.com OR monitoring.googleapis.com OR logging.googleapis.com)" \
  --format "table(name, title)"
```

**Expected:** All three APIs listed. If any are missing, enable them:

```bash
gcloud services enable \
    cloudtrace.googleapis.com \
    cloudmonitoring.googleapis.com \
    logging.googleapis.com \
    --project "$PROJECT_ID"
```

### 1.2 Check service account IAM roles

The service account `scion-demo-sa` needs these roles for telemetry:

```bash
SA_EMAIL="scion-demo-sa@${PROJECT_ID}.iam.gserviceaccount.com"

gcloud projects get-iam-policy "$PROJECT_ID" \
  --flatten="bindings[].members" \
  --filter="bindings.members:serviceAccount:${SA_EMAIL}" \
  --format="table(bindings.role)"
```

**Required roles for telemetry:**

| Role | Purpose |
|------|---------|
| `roles/cloudtrace.agent` | Write trace spans to Cloud Trace |
| `roles/monitoring.metricWriter` | Write metrics to Cloud Monitoring |
| `roles/logging.logWriter` | Write structured logs to Cloud Logging |

If any are missing, add them individually:

```bash
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member "serviceAccount:${SA_EMAIL}" \
    --role "roles/cloudtrace.agent" > /dev/null

gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member "serviceAccount:${SA_EMAIL}" \
    --role "roles/monitoring.metricWriter" > /dev/null

gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member "serviceAccount:${SA_EMAIL}" \
    --role "roles/logging.logWriter" > /dev/null
```

---

## 2. Configure Hub-Side Telemetry

Hub-side telemetry activates Cloud Logging (via `cloud_handler.go`) and the
OTel log bridge (via `otel_provider.go`) for the `scion server` process
itself.

### 2.1 Environment variables

The following environment variables control Hub-side telemetry. They should
be managed in the `hub.env` file (see `scripts/starter-hub/hub.env.example` for the
template). The systemd unit loads this file via
`EnvironmentFile=/home/scion/.scion/hub.env`.

To set up, copy the example and fill in values:

```bash
cp scripts/starter-hub/hub.env.example .scratch/hub.env
# Edit .scratch/hub.env with your values
```

The `gce-start-hub.sh` script automatically uploads `.scratch/hub.env` to
the instance at `/home/scion/.scion/hub.env`.

**Required telemetry variables in hub.env:**

```bash
# Core Hub configuration (required)
SCION_HUB_STORAGE_BUCKET=scion-demo-templates
SESSION_SECRET=<generate-with-openssl-rand-base64-32>

# Telemetry (OTEL)
SCION_CLOUD_LOGGING=true
SCION_GCP_PROJECT_ID=deploy-demo-test
GOOGLE_CLOUD_PROJECT=deploy-demo-test
SCION_OTEL_ENDPOINT=cloudtrace.googleapis.com:443
SCION_OTEL_LOG_ENABLED=true
```


### 2.2 Retrofit an existing instance

If the instance was deployed before the hub.env pattern was adopted, update
the configuration manually:

```bash
gcloud compute ssh scion-demo --zone us-central1-a --command '
    sudo systemctl stop scion-hub

    # Create or update hub.env with telemetry variables
    sudo tee /home/scion/.scion/hub.env > /dev/null <<EOF
SCION_HUB_STORAGE_BUCKET=scion-demo-templates
SESSION_SECRET=<your-session-secret>
SCION_CLOUD_LOGGING=true
SCION_GCP_PROJECT_ID=deploy-demo-test
GOOGLE_CLOUD_PROJECT=deploy-demo-test
SCION_OTEL_ENDPOINT=cloudtrace.googleapis.com:443
SCION_OTEL_LOG_ENABLED=true
EOF

    sudo chown scion:scion /home/scion/.scion/hub.env
    sudo chmod 600 /home/scion/.scion/hub.env
    sudo systemctl daemon-reload
    sudo systemctl start scion-hub
'
```

Alternatively, re-run `scripts/starter-hub/gce-start-hub.sh` which will upload your local
`.scratch/hub.env` and regenerate the systemd unit.

### 2.3 Verify Hub telemetry initialized

Check the journal for Cloud Logging and OTel initialization messages:

```bash
gcloud compute ssh scion-demo --zone us-central1-a --command \
    'sudo journalctl -u scion-hub --since "5 minutes ago" | grep -iE "cloud.logging|otel|telemetry|trace"'
```

**Expected:** Messages indicating Cloud Logging handler and OTel trace
provider initialized successfully. No errors about missing credentials
or project ID.

---

## 3. Configure Agent-Side Telemetry

Agent-side telemetry is driven by `~/.scion/settings.yaml` on the instance.
The settings-to-env bridge injects telemetry configuration as environment
variables into each agent container.

### 3.1 Deploy settings.yaml

The `gce-start-hub.sh` script deploys this automatically (only if the file
doesn't already exist). For manual creation or updates:

```bash
gcloud compute ssh scion-demo --zone us-central1-a --command '
cat > /home/scion/.scion/settings.yaml <<EOF
schema_version: "1"
hub:
  endpoint: "https://hub.demo.scion-ai.dev"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
    protocol: "grpc"
    batch:
      max_size: 256
      timeout: "5s"
  local:
    enabled: true
  filter:
    events:
      exclude:
        - "agent.user.prompt"
    attributes:
      redact:
        - "prompt"
        - "user.email"
        - "tool_output"
        - "tool_input"
      hash:
        - "session_id"
EOF
'
```

> **Note:** Cloud export from agent containers requires GCP credential
> injection into the OTLP exporter, which is not yet implemented. See
> [Blocking Condition B1](#b1-otlp-exporter-lacks-gcp-credential-injection)
> above. The settings-to-env bridge and local telemetry pipeline work
> correctly; only the cloud export leg is blocked.

### 3.2 Verify env var injection

Start a test agent and inspect the container environment to confirm
telemetry settings are being injected:

```bash
# On the instance (via SSH)
scion start "telemetry test" --name test-telem --no-auth

docker inspect test-telem --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -E "SCION_TELEMETRY|SCION_OTEL"
```

**Expected output:**

```
SCION_TELEMETRY_ENABLED=true
SCION_TELEMETRY_CLOUD_ENABLED=true
SCION_OTEL_ENDPOINT=cloudtrace.googleapis.com:443
SCION_OTEL_PROTOCOL=grpc
SCION_TELEMETRY_FILTER_EXCLUDE=agent.user.prompt
SCION_TELEMETRY_REDACT=prompt,user.email,tool_output,tool_input
SCION_TELEMETRY_HASH=session_id
SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE=256
SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT=5s
```

Clean up: `scion stop test-telem --rm`

---

## 4. End-to-End Verification

### 4.1 Check Cloud Trace for Hub server spans

The Hub server emits spans for API requests when `SCION_OTEL_ENDPOINT` is set.

```bash
# Open Cloud Trace in browser
echo "https://console.cloud.google.com/traces/list?project=deploy-demo-test"
```

Or query via CLI:

```bash
gcloud traces list --project "deploy-demo-test" --limit 10
```

**Verification:** Look for spans from the Hub server process (HTTP handler
spans, gRPC spans, etc.).

### 4.2 Check Cloud Logging for structured logs

```bash
# Open Cloud Logging in browser
echo "https://console.cloud.google.com/logs/query?project=deploy-demo-test"
```

Filter by the scion-hub service:

```bash
gcloud logging read 'resource.type="gce_instance" AND jsonPayload.service="scion-hub"' \
    --project "deploy-demo-test" \
    --limit 10 \
    --format json
```

**Verification:** Structured JSON log entries appear with proper severity
levels and fields.

### 4.3 Check Cloud Monitoring for agent metrics

```bash
echo "https://console.cloud.google.com/monitoring/metrics-explorer?project=deploy-demo-test"
```

Search for metrics with the `custom.googleapis.com/` prefix or names like
`gen_ai.tokens.input`, `agent.tool.calls`.

### 4.4 Start a test agent and verify agent telemetry

```bash
# On the instance
scion start "trace test task" --name qa-trace --no-auth
scion attach qa-trace
# Interact with the agent to generate tool events, then detach (Ctrl+B, D)
```

Wait 30-60 seconds for spans to flush, then check Cloud Trace for agent
spans (`agent.tool.call`, `agent.turn.start`, `agent.session.start`).

Clean up: `scion stop qa-trace --rm`

---

## 5. Troubleshooting

### Missing credentials / ADC errors

**Symptom:** Log messages about "could not find default credentials" or
"transport: authentication handshake failed".

**Fix:** The GCE instance should use the service account's metadata-based
credentials automatically. Verify the instance has the correct service
account attached:

```bash
gcloud compute instances describe scion-demo --zone us-central1-a \
    --format "value(serviceAccounts[0].email)"
```

Expected: `scion-demo-sa@deploy-demo-test.iam.gserviceaccount.com`

### Wrong project ID

**Symptom:** Traces/logs appear in the wrong project or not at all.

**Fix:** Verify both `SCION_GCP_PROJECT_ID` and `GOOGLE_CLOUD_PROJECT` are
set correctly in hub.env:

```bash
gcloud compute ssh scion-demo --zone us-central1-a --command \
    'grep -E "PROJECT" /home/scion/.scion/hub.env'
```

### Cloud Logging API not enabled

**Symptom:** Errors about `logging.googleapis.com` being disabled.

**Fix:**

```bash
gcloud services enable logging.googleapis.com --project deploy-demo-test
```

### No agent spans appearing

**Symptom:** Hub spans appear but no agent-side spans in Cloud Trace.

**Checks:**
1. Verify `settings.yaml` exists: `ls -la /home/scion/.scion/settings.yaml`
2. Verify env var injection (see section 3.2)
3. Check agent container logs for OTEL initialization errors
4. Ensure the agent ran long enough for spans to flush (batch timeout is 5s)

### Service won't start after adding OTEL vars

**Symptom:** `scion-hub` service fails to start.

**Fix:** Check for syntax errors in the systemd unit:

```bash
gcloud compute ssh scion-demo --zone us-central1-a --command '
    sudo systemd-analyze verify /etc/systemd/system/scion-hub.service 2>&1
    sudo journalctl -u scion-hub -n 30 --no-pager
'
```

---

## 6. Quick Reference

### Hub-side environment variables (via hub.env)

| Variable | Value | Purpose |
|----------|-------|---------|
| `SCION_CLOUD_LOGGING` | `true` | Enable direct Cloud Logging via `cloud_handler.go` |
| `SCION_GCP_PROJECT_ID` | `deploy-demo-test` | GCP project for Cloud Logging client |
| `GOOGLE_CLOUD_PROJECT` | `deploy-demo-test` | Standard GCP project env var |
| `SCION_OTEL_ENDPOINT` | `cloudtrace.googleapis.com:443` | OTel exporter target for traces |
| `SCION_OTEL_LOG_ENABLED` | `true` | Enable OTel log bridge |

### Agent-side settings.yaml fields

| Field | Value | Purpose |
|-------|-------|---------|
| `telemetry.enabled` | `true` | Master switch for agent telemetry |
| `telemetry.cloud.enabled` | `true` | Enable cloud export |
| `telemetry.cloud.endpoint` | `cloudtrace.googleapis.com:443` | Cloud Trace endpoint |
| `telemetry.cloud.protocol` | `grpc` | Export protocol |
| `telemetry.cloud.batch.max_size` | `256` | Max spans per batch |
| `telemetry.cloud.batch.timeout` | `5s` | Batch flush interval |
| `telemetry.local.enabled` | `true` | Enable local debug telemetry |
| `telemetry.filter.events.exclude` | `[agent.user.prompt]` | Suppress user prompt spans |
| `telemetry.filter.attributes.redact` | `[prompt, user.email, ...]` | Redact sensitive attributes |
| `telemetry.filter.attributes.hash` | `[session_id]` | Hash for correlation without exposure |

### Relevant scripts

| Script | Purpose |
|--------|---------|
| `scripts/starter-hub/gce-demo-provision.sh` | Create GCE instance with APIs and IAM roles |
| `scripts/starter-hub/gce-start-hub.sh` | Build, deploy, and start Hub with OTEL config |
| `scripts/starter-hub/hub.env.example` | Template for `.scratch/hub.env` |

---

## Related Documentation

| Document | Relevance |
|----------|-----------|
| [metrics-system.md](../hosted/metrics-system.md) | Full metrics architecture and telemetry pipeline design |
| [telemetry-gcp.md](telemetry-gcp.md) | Local development telemetry QA walkthrough |
| [scion-local.md](scion-local.md) | Local CLI QA walkthrough |
