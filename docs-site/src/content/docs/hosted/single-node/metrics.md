---
title: Metrics & OpenTelemetry
description: Collecting and forwarding operational metrics with sciontool telemetry.
---

Scion provides built-in telemetry collection via `sciontool`, which runs as the init process in agent containers. The telemetry pipeline acts as an **OTLP Forwarder**: it receives data from agents locally and forwards it to a central cloud observability backend.

## Telemetry Flow

1.  **Agent (The Source)**: Emits OTLP data (traces/metrics) or harness hook events.
2.  **sciontool (The Forwarder)**: 
    - Receives OTLP via gRPC (port 4317) or HTTP (port 4318).
    - Normalizes harness hooks into standard OTLP spans.
    - Applies privacy filters (redaction/hashing).
3.  **Cloud Backend (The Destination)**: Receives the processed telemetry from `sciontool`.

## Configuration

Telemetry is configured through `settings.yaml` (for global and project-level defaults) and `scion-agent.yaml` (for per-template and per-agent overrides). Environment variables provide the highest-priority override.

### Configuration Hierarchy

Telemetry settings resolve across scopes using **last-write-wins** semantics:

1.  **Global settings** (`~/.scion/settings.yaml`) — Organization-wide defaults.
2.  **Project settings** (`.scion/settings.yaml`) — Project-level overrides.
3.  **Template config** (`scion-agent.yaml` in template) — Role-specific overrides.
4.  **Agent config** (`scion-agent.yaml` in agent home) — Per-agent overrides.
5.  **Environment variables** (`SCION_TELEMETRY_*`, `SCION_OTEL_*`) — Highest priority.

At each scope, only the fields you specify are overridden; unset fields inherit from the previous scope.

### Settings File Configuration

The `telemetry` block can appear in any `settings.yaml` (global or project) or `scion-agent.yaml` (template or agent):

```yaml
# In settings.yaml or scion-agent.yaml
telemetry:
  enabled: true

  cloud:
    enabled: true
    endpoint: "monitoring.googleapis.com:443"
    protocol: grpc
    headers:
      Authorization: "Bearer ${OTEL_API_KEY}"
    tls:
      enabled: true
      insecure_skip_verify: false
    batch:
      max_size: 512
      timeout: "5s"

  hub:
    enabled: true
    report_interval: "30s"

  local:
    enabled: false
    file: ""
    console: false

  filter:
    enabled: true
    respect_debug_mode: true
    events:
      include: []
      exclude:
        - "agent.user.prompt"
    attributes:
      redact:
        - "prompt"
        - "user.email"
        - "tool_output"
      hash:
        - "session_id"
    sampling:
      default: 1.0
      rates: {}

  resource:
    service.name: "scion-agent"
```

See the [Orchestrator Settings Reference](/scion/reference/orchestrator-settings/#telemetry-configuration-telemetry) for the full field reference.

### Environment Variable Overrides

Environment variables override any settings file value and are the most convenient option for CI or hosted deployments.

| Variable | Settings Path | Default | Description |
|----------|--------------|---------|-------------|
| `SCION_TELEMETRY_ENABLED` | `telemetry.enabled` | `true` | Enable/disable collection entirely |
| `SCION_TELEMETRY_CLOUD_ENABLED` | `telemetry.cloud.enabled` | `true` | Enable forwarding to cloud backend |
| `SCION_OTEL_ENDPOINT` | `telemetry.cloud.endpoint` | (required) | Cloud OTLP endpoint URL |
| `SCION_OTEL_PROTOCOL` | `telemetry.cloud.protocol` | `grpc` | Protocol: `grpc` or `http` |
| `SCION_OTEL_INSECURE` | `telemetry.cloud.tls.insecure_skip_verify` | `false` | Skip TLS verification (dev only) |
| `SCION_TELEMETRY_HUB_ENABLED` | `telemetry.hub.enabled` | `true` | Enable Hub reporting |
| `SCION_TELEMETRY_DEBUG` | `telemetry.local.enabled` | `false` | Enable local debug output |
| `SCION_GCP_PROJECT_ID` | — | (auto) | GCP project ID for Google Cloud backends |
| `SCION_OTEL_GCP_CREDENTIALS` | — | (auto) | Path to a GCP service account key JSON file; set automatically by the broker from the `scion-telemetry-gcp-credentials` secret |
| `SCION_TELEMETRY_CLOUD_PROVIDER` | — | (auto) | Cloud backend: `gcp` for GCP-native export; auto-detected when credentials file is present |

### Local Receiver Settings (For Agents)

These settings control the ports where `sciontool` listens for data from the agent processes *inside* the container.

| Variable | Default | Description |
|----------|---------|-------------|
| `SCION_OTEL_GRPC_PORT` | `4317` | Local gRPC receiver port |
| `SCION_OTEL_HTTP_PORT` | `4318` | Local HTTP receiver port |

## Google Cloud Setup (Recommended)

When deploying on Google Cloud, `sciontool` can forward directly to Cloud Trace and Cloud Logging using the standard OTLP endpoint.

### 1. Configure the Forwarder

Set these environment variables in your Hub settings (Project or Broker level):

```bash
# Direct OTLP ingestion for Google Cloud
export SCION_OTEL_ENDPOINT="monitoring.googleapis.com:443"
export SCION_OTEL_PROTOCOL="grpc"
export SCION_GCP_PROJECT_ID="your-project-id"
```

### 2. Configure the Agent (Native OTel)

If your agent harness supports native OpenTelemetry (e.g., `opencode`), configure it to point to the `sciontool` forwarder running on localhost:

```bash
# Tell the agent to send to sciontool
export OTEL_EXPORTER_OTLP_ENDPOINT="localhost:4317"
```

*Note: Most standard OTel SDKs default to `localhost:4317`, so explicit configuration may not be required.*

### 3. IAM Permissions

Ensure the environment where the agent container runs (GKE Pod, Cloud Run, etc.) has a service account with:
- `roles/logging.logWriter`
- `roles/cloudtrace.agent`
- `roles/monitoring.metricWriter`

### 4. GCP Credentials for Agent Containers (Non-ADC Environments)

When agents run outside of GKE or Cloud Run — where [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials) are not automatically available — you must supply a GCP service account key file. Scion uses a **well-known secret** to provision this credential into every agent container automatically.

| Property | Value |
|----------|-------|
| **Secret name** | `scion-telemetry-gcp-credentials` |
| **Secret type** | `file` |
| **Target path** | `~/.scion/telemetry-gcp-credentials.json` |
| **Env var set by broker** | `SCION_OTEL_GCP_CREDENTIALS` |

Register the secret once via the Hub:

```bash
scion hub secret set \
  --type file \
  --target ~/.scion/telemetry-gcp-credentials.json \
  scion-telemetry-gcp-credentials @/path/to/sa-key.json
```

When an agent starts, the broker:
1. Writes the credential file to `~/.scion/telemetry-gcp-credentials.json` in the agent's home directory.
2. Sets `SCION_OTEL_GCP_CREDENTIALS` to that path.
3. Auto-sets `SCION_TELEMETRY_CLOUD_PROVIDER=gcp` if not already configured.
4. Reads `project_id` from the credentials JSON to populate `SCION_GCP_PROJECT_ID` if not set explicitly.

:::note[Fallback probe]
`sciontool` also probes `~/.scion/telemetry-gcp-credentials.json` at startup even when `SCION_OTEL_GCP_CREDENTIALS` is not set — for example, when the file is placed via a volume mount or template provisioning. If the file exists, all of the above auto-detection applies.
:::

## Native Metrics Pipeline

Scion includes a native OTel metrics pipeline that captures operational data from agent sessions. This data is recorded as counters and histograms, providing a time-series view of agent performance. 

To enable harness-aware telemetry, Scion automatically injects `SCION_HARNESS` and `SCION_MODEL` environment variables into all agent containers.

### Enriched Resource Attributes

All metrics and traces emitted by Scion are enriched with context-aware OpenTelemetry resource attributes to allow for precise filtering and aggregation in your cloud backend:

- `scion.harness`: The type of harness running the agent (e.g., `gemini`, `claude`, `codex`).
- `scion.model`: The specific LLM model being used.
- `scion.broker`: The ID of the Runtime Broker executing the agent.
- `project_id`: The ID of the agent's parent project.

### Automated Metrics Collection

When harness events occur (via hooks), sciontool automatically records the following metrics:

| Metric | Type | Unit | Description |
|--------|------|------|-------------|
| `gen_ai.tokens.input` | Counter | tokens | Number of input tokens processed |
| `gen_ai.tokens.output` | Counter | tokens | Number of output tokens generated |
| `gen_ai.tokens.cached` | Counter | tokens | Number of tokens retrieved from cache |
| `agent.tool.calls` | Counter | calls | Total number of tool executions |
| `agent.tool.duration` | Histogram | ms | Latency of tool executions |
| `agent.session.count` | Counter | sessions | Total number of agent sessions |
| `gen_ai.api.calls` | Counter | calls | Total number of LLM API requests |
| `gen_ai.api.duration` | Histogram | ms | Latency of LLM API requests |

*(Note: The Codex harness has been expanded to capture comprehensive telemetry including tool usage, detailed tool input/output, and granular token counts for input, output, and cached tokens).*

### Correlated Logs

For every significant lifecycle event (session start/end, tool use, model call), sciontool emits an OTel log record that is automatically correlated with the active trace. This means when viewing a trace waterfall in your observability backend (like Google Cloud Trace), you can click directly through to the specific logs associated with each span.

## Hub Infrastructure Metrics

The Scion Hub maintains internal operational metrics for infrastructure monitoring. These are available via the `/api/v1/admin/metrics` endpoint (requires `hub:admin` role) and can be exported to standard monitoring tools.

### GCP Token Metrics

With the introduction of GCP Identity emulation, the Hub tracks the health and performance of the token brokering pipeline:

| Metric | Description |
|--------|-------------|
| `accessTokenRequests` | Total number of GCP Access Token requests from agents. |
| `accessTokenSuccesses` | Number of successfully brokered access tokens. |
| `accessTokenFailures` | Number of failed access token requests (e.g., IAM permission errors). |
| `idTokenRequests` | Total number of GCP Identity Token requests. |
| `rateLimitRejections` | Number of token requests rejected due to per-agent rate limiting. |
| `iamLatencyP50Ms` | Median latency of IAM API calls to Google Cloud. |
| `iamLatencyP95Ms` | 95th percentile latency of IAM API calls. |

### Broker Authentication Metrics

Monitors the security and connectivity of Runtime Brokers:

- `authAttempts`: Total broker authentication attempts.
- `connectedBrokers`: Current number of active Runtime Brokers connected to the Hub.
- `dispatchFailures`: Number of failed agent dispatch commands to brokers.

## Privacy Filtering

By default, sciontool excludes `agent.user.prompt` events to protect user privacy. Filtering is configured via the `telemetry.filter` block in `settings.yaml` or `scion-agent.yaml`, or via environment variables.

### Via Settings File

```yaml
telemetry:
  filter:
    events:
      exclude:
        - "agent.user.prompt"
        - "agent.tool.result"
    attributes:
      redact:
        - "prompt"
        - "user.email"
        - "tool_output"
      hash:
        - "session_id"
    sampling:
      default: 1.0
      rates:
        "agent.tool.call": 0.5
```

### Via Environment Variables

```bash
# Exclude multiple event types
export SCION_TELEMETRY_FILTER_EXCLUDE="agent.user.prompt,agent.tool.result"

# Only forward specific event types
export SCION_TELEMETRY_FILTER_INCLUDE="agent.session.start,agent.session.end,agent.tool.call"
```

## Attribute Redaction

Beyond event filtering, sciontool provides field-level attribute redaction for sensitive data. This allows telemetry to flow while protecting specific values.

### Redacted Fields

Redacted fields have their values replaced with `[REDACTED]`:

```bash
# Default redacted fields
export SCION_TELEMETRY_REDACT="prompt,user.email,tool_output,tool_input"
```

### Hashed Fields

Hashed fields are replaced with their SHA256 hash, allowing correlation without exposing the original value:

```bash
# Default hashed fields
export SCION_TELEMETRY_HASH="session_id"
```

## Hook-to-Span Conversion

Harness hook events are automatically converted to OTLP spans:

| Hook Event | Span Name | Attributes |
|------------|-----------|------------|
| `session-start` | `agent.session.start` | session_id, source |
| `session-end` | `agent.session.end` | session_id, reason, tokens_*, duration_ms |
| `tool-start` | `agent.tool.call` | tool_name, tool_input |
| `tool-end` | `agent.tool.result` | tool_name, success, duration_ms |
| `prompt-submit` | `agent.user.prompt` | prompt |
| `model-start` | `gen_ai.api.request` | model |
| `model-end` | `gen_ai.api.response` | success |

### Session Metrics (Gemini)

For Gemini CLI agents, session-end events include aggregated metrics from the session file:

- Token counts: `tokens_input`, `tokens_output`, `tokens_cached`
- Session info: `turn_count`, `duration_ms`, `model`
- Per-tool statistics: `tool.<name>.calls`, `tool.<name>.success`, `tool.<name>.errors`

Session files are automatically parsed from `~/.gemini/sessions/`.


## Implementation Details

The telemetry pipeline is implemented in `pkg/sciontool/telemetry/`:

- `config.go` - Configuration loading from environment variables
- `filter.go` - Event type filtering (include/exclude) and attribute redaction
- `exporter.go` - Cloud OTLP exporter (gRPC and HTTP)
- `receiver.go` - OTLP gRPC/HTTP receiver
- `pipeline.go` - Main orchestration (Start/Stop lifecycle)

Hook-to-span conversion is in `pkg/sciontool/hooks/handlers/`:

- `telemetry.go` - TelemetryHandler for converting hooks to spans
- Session parsing in `pkg/sciontool/hooks/session/parser.go`

The pipeline is integrated into the init command (`cmd/sciontool/commands/init.go`) and starts after user setup, before lifecycle hooks.
