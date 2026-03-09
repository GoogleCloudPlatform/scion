# QA Walkthrough: Telemetry Pipeline with Google Cloud

**Created:** 2026-02-19
**Status:** Ready for QA
**Goal:** Validate end-to-end telemetry flow from agent container through
sciontool to Google Cloud Observability (Cloud Trace + Cloud Monitoring).

This walkthrough covers the "Ready" scenarios in
[metrics-system.md](../hosted/metrics-system.md) section 13.4, specifically
the settings-driven configuration path enabled by the section 13.1
implementation.

---

## Prerequisites

- Go 1.21+
- Docker (or macOS `container` CLI)
- A GCP project with billing enabled
- `gcloud` CLI installed and authenticated
- (Optional) `otel-cli` for sending manual OTLP spans

---

## 1. GCP Project Setup

### 1.1 Enable required APIs

```bash
export GCP_PROJECT="your-project-id"

gcloud services enable cloudtrace.googleapis.com \
  monitoring.googleapis.com \
  --project "$GCP_PROJECT"
```

### 1.2 Authenticate with Application Default Credentials

The sciontool exporter uses standard Google Cloud ADC. For local QA the
simplest path is user credentials:

```bash
gcloud auth application-default login --project "$GCP_PROJECT"
```

For CI or remote brokers, use a service account with the `roles/cloudtrace.agent`
and `roles/monitoring.metricWriter` roles (this matches the `scion-demo-sa`
created by `scripts/starter-hub/gce-demo-provision.sh`):

```bash
gcloud iam service-accounts create scion-demo-sa \
  --display-name "Scion Demo Service Account"

gcloud projects add-iam-policy-binding "$GCP_PROJECT" \
  --member "serviceAccount:scion-demo-sa@${GCP_PROJECT}.iam.gserviceaccount.com" \
  --role "roles/cloudtrace.agent"

gcloud projects add-iam-policy-binding "$GCP_PROJECT" \
  --member "serviceAccount:scion-demo-sa@${GCP_PROJECT}.iam.gserviceaccount.com" \
  --role "roles/monitoring.metricWriter"
```

---

## 2. Build Scion

```bash
# From the scion source root
mkdir -p ../scion-qa-telemetry
go build -buildvcs=false -o ../scion-qa-telemetry/scion ./cmd/scion/
cd ../scion-qa-telemetry
```

---

## 3. Configure Telemetry via Settings

### 3.1 Initialize a test grove

```bash
./scion grove init
```

**Verification:** Confirm `.scion/` exists in the current directory and
`~/.scion/settings.yaml` exists in your home directory.

### 3.2 Set global telemetry settings

Edit `~/.scion/settings.yaml` to add a telemetry block. This is the
"global scope" layer — it will flow through to every agent container via
the settings-to-env bridge (`ConvertV1TelemetryToAPI` + `TelemetryConfigToEnv`).

```yaml
schema_version: "1"
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
    protocol: "grpc"
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
```

### 3.3 (Optional) Override at grove scope

Create `.scion/settings.yaml` in the test grove to override specific fields.
Grove-scope settings merge on top of global settings.

```yaml
schema_version: "1"
telemetry:
  cloud:
    batch:
      max_size: 256
      timeout: "5s"
  local:
    enabled: true
```

---

## 4. Verify Environment Variable Injection

Start an agent with `--no-auth` and inspect the container environment to
confirm the settings bridge is working. Use Docker to inspect env vars
before the agent does any real work.

```bash
./scion start "hello" --name qa-telem --no-auth
```

```bash
docker inspect qa-telem --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -E "SCION_TELEMETRY|SCION_OTEL"
```

**Expected output** (values depend on your settings):

```
SCION_TELEMETRY_ENABLED=true
SCION_TELEMETRY_CLOUD_ENABLED=true
SCION_OTEL_ENDPOINT=cloudtrace.googleapis.com:443
SCION_OTEL_PROTOCOL=grpc
SCION_TELEMETRY_FILTER_EXCLUDE=agent.user.prompt
SCION_TELEMETRY_REDACT=prompt,user.email,tool_output,tool_input
SCION_TELEMETRY_HASH=session_id
```

If you added grove-level local debug settings, also check:

```
SCION_TELEMETRY_LOCAL_ENABLED=true
SCION_TELEMETRY_DEBUG=true
SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE=256
SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT=5s
```

Also verify harness-specific telemetry env vars are present. These direct
the harness's native telemetry to the local OTLP collector:

```bash
docker inspect qa-telem --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -E "GEMINI_TELEMETRY"
```

**Expected output** (for Gemini harness):

```
GEMINI_TELEMETRY_ENABLED=true
GEMINI_TELEMETRY_TARGET=local
GEMINI_TELEMETRY_USE_COLLECTOR=true
GEMINI_TELEMETRY_OTLP_ENDPOINT=http://localhost:4317
GEMINI_TELEMETRY_OTLP_PROTOCOL=grpc
GEMINI_TELEMETRY_LOG_PROMPTS=false
```

For Claude harness agents, check for `CLAUDE_CODE_ENABLE_TELEMETRY=1` and
`OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317` instead.

**Key verification points:**

- Settings-level telemetry fields appear as container env vars.
- Harness-specific telemetry env vars are injected when telemetry is enabled.
- The `GCP_PROJECT` env var (if needed via `SCION_GCP_PROJECT_ID`) is only
  required when using project-scoped Cloud Trace endpoints; the standard
  `cloudtrace.googleapis.com:443` endpoint infers the project from ADC.

Clean up the test agent:

```bash
./scion stop qa-telem --rm
```

---

## 5. Verify Telemetry Disabled at Grove Scope

This confirms that setting `telemetry.enabled: false` at the grove scope
suppresses telemetry collection in the agent container.

### 5.1 Set grove-level override

Write `.scion/settings.yaml` in the test grove:

```yaml
schema_version: "1"
telemetry:
  enabled: false
```

### 5.2 Start agent and inspect

```bash
./scion start "disabled test" --name qa-telem-off --no-auth
```

```bash
docker inspect qa-telem-off --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep SCION_TELEMETRY_ENABLED
```

**Expected:** `SCION_TELEMETRY_ENABLED=false`

Inside the container, sciontool's `telemetry.LoadConfig()` will read this
value and `Pipeline.New()` will return nil, skipping all collection.

Clean up:

```bash
./scion stop qa-telem-off --rm
```

Restore the grove settings to `enabled: true` (or remove the override)
before proceeding to the next sections.

---

## 6. End-to-End Cloud Trace Verification

This section verifies the full pipeline: agent hook events are converted to
OTLP spans by sciontool's `TelemetryHandler`, forwarded through the pipeline's
filter and cloud exporter, and appear in Google Cloud Trace.

### 6.1 Start an agent with telemetry enabled

```bash
./scion start "trace test task" --name qa-trace --no-auth
```

### 6.2 Trigger tool executions

Attach to the agent and interact with it to generate hook events. Each tool
invocation produces `agent.tool.call` and `agent.tool.result` spans:

```bash
./scion attach qa-trace
# Ask the agent to run a simple command, e.g. "list files in /tmp"
# Detach with Ctrl+B then D
```

### 6.3 Check Cloud Trace

Allow 30-60 seconds for spans to flush, then query Cloud Trace:

```bash
# Open Cloud Trace in the browser
echo "https://console.cloud.google.com/traces/list?project=${GCP_PROJECT}"
```

Or query via gcloud:

```bash
gcloud traces list --project "$GCP_PROJECT" \
  --filter "rootSpan.name:agent" \
  --limit 10
```

**Verification:**

- Spans with names like `agent.tool.call`, `agent.turn.start`,
  `agent.session.start` appear in the trace list.
- Span attributes include `agent.name`, `tool_name`, `model`, etc.
- `agent.user.prompt` spans are **absent** (filtered by default exclude).
- Attributes like `prompt` show `[REDACTED]` (redaction filter active).
- `session_id` values are SHA-256 hashes (hash filter active).

### 6.4 Check Cloud Monitoring (metrics)

The `TelemetryHandler` records OTel metric instruments (`gen_ai.tokens.input`,
`agent.tool.calls`, etc.). In GCP-native mode, metrics reach Cloud Monitoring
via the SDK MeterProvider (configured in `providers.go`), **not** through the
OTLP pipeline forwarding path. The pipeline's `ExportProtoMetrics` is a no-op
for the GCP exporter because it receives OTLP proto types which cannot be
converted to the SDK metricdata types required by the GCP metric exporter.
This means metrics are exported directly by each agent's own MeterProvider:

```bash
echo "https://console.cloud.google.com/monitoring/metrics-explorer?project=${GCP_PROJECT}"
```

Search for metrics with the `custom.googleapis.com/` prefix or the
`gen_ai.tokens.input` name.

Clean up:

```bash
./scion stop qa-trace --rm
```

---

## 7. Privacy Filtering Verification

### 7.1 Verify default exclude list

By default, `agent.user.prompt` events are excluded. In section 6.3 above,
confirm these spans do not appear in Cloud Trace.

### 7.2 Test custom include list

Override the filter to only include specific event types:

```yaml
# ~/.scion/settings.yaml or .scion/settings.yaml
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "cloudtrace.googleapis.com:443"
    protocol: "grpc"
  filter:
    events:
      include:
        - "agent.session.start"
        - "agent.session.end"
```

Start an agent, trigger several tool calls, then verify in Cloud Trace that
only `agent.session.start` and `agent.session.end` spans appear — no tool
call spans.

### 7.3 Verify redaction

Inspect span attributes in Cloud Trace. Fields listed in the `redact`
configuration should appear as `[REDACTED]`, while fields in the `hash`
configuration should appear as hex-encoded SHA-256 digests.

---

## 8. Settings Hierarchy Merge Verification

This confirms the merge priority chain: global settings < grove settings
< template `scion-agent.yaml` < explicit env vars.

### 8.1 Set conflicting values at different scopes

**Global** (`~/.scion/settings.yaml`):

```yaml
telemetry:
  enabled: true
  cloud:
    endpoint: "global-endpoint.example.com:4317"
```

**Grove** (`.scion/settings.yaml`):

```yaml
schema_version: "1"
telemetry:
  cloud:
    endpoint: "grove-endpoint.example.com:4317"
```

### 8.2 Start and inspect

```bash
./scion start "merge test" --name qa-merge --no-auth
docker inspect qa-merge --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep SCION_OTEL_ENDPOINT
```

**Expected:** `SCION_OTEL_ENDPOINT=grove-endpoint.example.com:4317`

The grove scope overrides the global scope, and `MergeScionConfig` ensures
template/agent-level values would override both.

Clean up:

```bash
./scion stop qa-merge --rm
```

### 8.3 Explicit env override

Pre-set an env var that the bridge should not overwrite:

```bash
SCION_OTEL_ENDPOINT="explicit-override.example.com:4317" \
  ./scion start "override test" --name qa-override --no-auth

docker inspect qa-override --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep SCION_OTEL_ENDPOINT
```

**Expected:** `SCION_OTEL_ENDPOINT=explicit-override.example.com:4317`

The injection logic in `run.go` skips keys already present in `opts.Env`.

Clean up:

```bash
./scion stop qa-override --rm
```

---

## 9. Cleanup

```bash
# Remove all scion test containers
docker rm -f $(docker ps -a -q --filter "label=scion.agent=true") 2>/dev/null

# Remove the test grove
cd ..
rm -rf scion-qa-telemetry
```

---

## Related Documentation

| Document | Relevance |
|----------|-----------|
| [metrics-system.md](../hosted/metrics-system.md) | Full metrics architecture and QA gap tracker |
| [sciontool-overview.md](../sciontool-overview.md) | Sciontool architecture and lifecycle |
| [scion-local.md](scion-local.md) | Local CLI QA walkthrough |
