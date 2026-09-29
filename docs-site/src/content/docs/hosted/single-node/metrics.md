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
    # Required when exporting to Google Cloud. The metrics dashboard queries
    # Cloud Monitoring for this project, so without it the export has no
    # destination and the dashboard stays empty with no error shown.
    gcp_project_id: "my-gcp-project"
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

:::caution[`SCION_GCP_PROJECT_ID` is required for cloud export]
Cloud export is silently skipped when no GCP project ID is available. Set `SCION_GCP_PROJECT_ID` explicitly, or ensure the `scion-telemetry-gcp-credentials` service-account key file contains a `project_id` field so auto-detection can populate it. If export is enabled but no project ID is found, `sciontool` logs a warning at startup.
:::

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
- `scion.broker.name`: The name of the Runtime Broker executing the agent, when available.
- `scion.project.id`: The authoritative ID of the agent's parent project, when available.

#### Identity Enforcement

The `sciontool` receiver enforces authoritative identity on all incoming telemetry. Reserved identity attributes (`scion.agent.id`, `scion.agent.slug`, `scion.project.id`, `scion.harness`, `scion.model`, `scion.broker.name`, and related keys) are stripped from agent-submitted resource attributes and replaced with Hub-sourced values. This prevents agents from spoofing their identity in exported telemetry.

In addition to the resource attributes above, every exported metric **point** carries three canonical labels, stamped from that same authoritative identity by the exporter (GCP-native and generic OTLP alike), never by a producer:

- `scion_agent_id`
- `scion_project_id`
- `scion_agent_slug` — a display-friendly form of `scion_agent_id`, 1:1 with it, and read straight off the series (no Hub database lookup, so it survives agent deletion)

These are the labels the Hub dashboard filters and groups on (`metric.labels.scion_project_id` for the project view, `metric.labels.scion_agent_id` for agent grouping). A producer that sets one of these three keys itself is rejected. They are distinct from the pre-existing, non-canonical point labels `agent_id` and `project_id` that some hook metrics also carry for historical, descriptor-compatibility reasons; the dashboard does not read those.

### Native Event Name Normalization

When harnesses emit native OTLP log records or events, `sciontool` normalizes their harness-specific event names into the canonical `agent.*` namespace before forwarding. This ensures consistent filtering and querying across harnesses:

| Harness | Native event name | Normalized name |
|---------|------------------|-----------------|
| Claude Code | `user_prompt` (scope `com.anthropic.claude_code.events`) | `agent.user.prompt` |
| Codex | `codex.user_prompt` | `agent.user.prompt` |
| Codex | `codex.tool_result` | `agent.tool.result` |
| Gemini CLI | `gemini_cli.user_prompt` | `agent.user.prompt` |

Event names that do not match a known alias are forwarded unchanged. All log records carry a normalized `event.name` attribute after processing.

### Automated Metrics Collection

When harness events occur (via hooks), sciontool automatically records the following metrics:

| Metric | Type | Unit | Description |
|--------|------|------|-------------|
| `scion.usage.tokens` | Counter | tokens | Tokens reported by model-end hooks, broken down by the `token_type` label (see "Canonical usage contract" below) |
| `agent.tool.calls` | Counter | calls | Total number of tool executions |
| `agent.tool.duration` | Histogram | ms | Tool duration when paired start and end events are available in one process |
| `agent.session.count` | Counter | sessions | Session-end events emitted by each source |
| `gen_ai.api.calls` | Counter | calls | Total number of LLM API requests |
| `gen_ai.api.duration` | Histogram | ms | Model duration when paired start and end events are available in one process |

Token and API-call counters appear only when a hook provides them, and only
when the harness's usage source is `hooks` (see "Usage source" below) —
`gen_ai.api.calls` is gated the same way as `scion.usage.tokens`, not just the
token counters. The retired `scion.hook.tokens.{input,output,cached}` names
are rejected by the receiver on the hook scope, the same way `gen_ai.tokens.*`
is.

A hook-sourced harness's dialect can populate all five `token_type` values on
`scion.usage.tokens`, not just three. A Go dialect sets the `EventData`
fields `InputTokens`, `OutputTokens`, `CachedTokens`, `CacheWriteTokens` and
`ReasoningTokens`; a `dialect.yaml` `fields` mapping uses the YAML keys
`input_tokens` (→ `input`), `output_tokens` (→ `output`), `cached_tokens`
(→ `cache_read`), `cache_write_tokens` (→ `cache_write`) and
`reasoning_tokens` (→ `reasoning`, informational, already included in
`output` and never added again). `output_tokens`/`OutputTokens` must be the
*total* output including reasoning (canonical usage contract,
`.design/hosted/usage-telemetry.md` §3.2): a `dialect.yaml` `fields`
mapping is a pure path copy with no arithmetic, so a harness that reports
output and reasoning as exclusive values needs a Go dialect or bridge-side
summing to produce a combined `output_tokens` — the YAML mapping alone
cannot add them together. A point is emitted only for a token type
whose field was actually populated (a positive value); a
dialect that never maps a given field simply never emits that `token_type`.

#### Canonical usage contract: `gen_ai.api.calls` and `scion.usage.tokens`

The Hub dashboard reads exactly two usage metrics, regardless of source:

| Metric | Point labels | Meaning |
|--------|--------------|---------|
| `gen_ai.api.calls` | `harness`, `model`, `status` (`success`\|`error`), `scion_agent_id`, `scion_project_id`, `scion_agent_slug` | One completed model response, or a failed request where the source reports it. |
| `scion.usage.tokens` | `harness`, `model`, `token_type`, `scion_agent_id`, `scion_project_id`, `scion_agent_slug` | Tokens attributed to model requests. |

`token_type` is a closed enum: `input` (non-cached prompt tokens), `output` (generated tokens, including reasoning), `cache_read`, `cache_write`, and `reasoning` (an informational subset of `output`, already counted there — never add it to a total alongside `output`). `scion.usage.tokens` is the only token metric either source writes; the retired `scion.hook.tokens.*` family is rejected at admission (see above). A harness's native events and its hooks can in principle both populate it, but never both at once for the same harness (see "Usage source" below).

Each harness declares **one** usage source in its `provision.py`, via `SCION_USAGE_SOURCE=native|hooks`. When native, sciontool's receiver derives `gen_ai.api.calls`/`scion.usage.tokens` itself from the harness's own OTLP log events (for example Claude's `api_request`/`api_error`), so no hook needs to carry usage at all. Claude declares `SCION_USAGE_SOURCE=native`.

An unset `SCION_USAGE_SOURCE` means **no usage is published** from hooks for that harness (design D10, the vetting gate) — an unvetted guess is worse than a visible gap. A harness publishes hook-sourced usage only once its `provision.py` declares `SCION_USAGE_SOURCE=hooks`, behind a PR that checks in a captured fixture proving the mapping. As of this phase, no harness has opted in that way; declaring `hooks` for a given harness's `provision.py` is tracked per-harness follow-up work (for example opencode and antigravity). Tool, session, turn and every other hook metric, span and log is unaffected by `SCION_USAGE_SOURCE` in every case (design D4, narrow).

**Known gap: codex.** Codex's `model-end` hook already carries calls and tokens (`dialects/codex.go`), and today it is the harness's only usage source. With `SCION_USAGE_SOURCE` unset, codex's hook usage stops appearing once this gate lands and `scion-base` is rebuilt, and stays at zero until design phase 3c lands `SCION_USAGE_SOURCE=native` for codex (deriving usage from its own OTLP events instead). Until phase 3c, codex shows no calls or tokens on the dashboard. gemini-cli (deferred, D6), antigravity and muse-code have the same unset-by-default gap, each awaiting its own fixture-backed `provision.py` PR; codex is called out separately here because it otherwise reads as fully supported.

All of this — the deriver, the hook vetting gate, and the allowlist changes below — is sciontool-side value: it takes effect only after an operator rebuilds `scion-base` and then the harness images. An unrebuilt `scion-base` keeps today's behavior unchanged; it does not error, and it does not need a `provision.py` workaround.

For Cloud Monitoring, the normalized hook counters and the derived usage counters in this table use a
collector observation epoch and the time sciontool takes each cumulative
snapshot. A counter's Cloud point time therefore describes when this collector
observed the total, rather than the time of the last hook event. Retries keep
the original snapshot time. Generic OTLP forwarding and native metric points
keep their source timestamps through sciontool. The Monitoring SDK maps
non-gauge intervals shorter than two milliseconds to a one-millisecond
interval; admission checks use that mapped end. A native point known to be less than five seconds
after a possibly written point in the same Cloud series is rejected before
admission; a request containing that point is rejected as a whole. Scope and
metric names select the hook counter behavior, so local producers using those
reserved names also opt into it. This does not authenticate the producer.
Session-end totals do not add a second copy of model-end usage. Short-lived
hook processes normally cannot pair start and end events, so duration
histograms are not guaranteed. This does not establish native Codex token
emission.

`agent.session.count` has two distinct sources: harness `session-end` hooks and
the `sciontool init` lifecycle `session-end` event. They keep the same metric
name and unit, but use distinct instrumentation scopes. In Cloud Monitoring,
filter the `scion_metric_scope_id` label for the source you intend to inspect:
the hook scope is `github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers`,
and the init lifecycle scope adds `/lifecycle`. The Cloud label contains a
digest of the scope identity, so inspect a sample series to obtain its value.
These are source event counts; summing both does not produce a canonical count
of logical sessions. The Hub dashboard does not reconcile them.

The Hub dashboard reads only `gen_ai.api.calls` and `scion.usage.tokens` (never
`gen_ai.tokens.*` or `scion.hook.tokens.*`), and computes totals from each
series' cumulative increase per flush rather than summing raw points — summing
raw points on a CUMULATIVE counter over-counts by roughly the flush count.
A harness whose `provision.py` declares `SCION_USAGE_SOURCE=hooks` keeps
appearing on the dashboard from its hooks. Claude's usage instead comes from
its native events, since its `provision.py` declares `SCION_USAGE_SOURCE=native`;
see "Canonical usage contract" above. An as-yet-unvetted hook-sourced harness's
usage does not appear until its `provision.py` opts in under a fixture-backed
PR (the vetting gate described there).

### Correlated Logs

For every significant lifecycle event (session start/end, tool use, model call), sciontool emits an OTel log record that is automatically correlated with the active trace. This means when viewing a trace waterfall in your observability backend (like Google Cloud Trace), you can click directly through to the specific logs associated with each span.

## Hub Infrastructure Metrics

The Scion Hub maintains internal operational metrics for infrastructure monitoring. These are available via the `/api/v1/admin/metrics` endpoint (requires `hub-admin` role) and can be exported to standard monitoring tools.

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


## Hub-Backed Session Metrics (M3 & M4)

In addition to fanning out raw time-series metrics to standard OpenTelemetry destinations, Scion contains a native **Hub-Backed Session Metrics** engine. This system aggregates session telemetry inside the agent container and reports it directly back to the Hub on session completion for relational storage, SQL-level aggregation, and direct visualization in the Web Dashboard.

### 1. Ingestion Pipeline (M3)
At the end of an agent session, `sciontool`'s aggregation engine collects high-level statistics from the session run and sends them to the Hub over the agent status-update channel inside a JSON-serialized `MetricsPayload` structure:
- **Session context**: Unique session UUID, start/end timestamps, final run status, model identifier, and total turn count.
- **Token counters**: High-density counts of input, output, cached, and reasoning tokens.
- **Tool statistics**: A dictionary of used tools detailing total execution counts, successful runs, and errors.
- **Languages**: A list of detected languages utilized.

The Hub validates this payload, verifies that the requesting agent is authorized to write its own metrics (via secure `X-Scion-Agent-Token` authentication), and persists the record into the database-backed `agent_session_metrics` SQL table.

### 2. Query Endpoints & Authorization (M4)
To surface these relational metrics safely, the Hub provides a set of IDOR-safe API endpoints that perform automatic SQL-level aggregations (averaging session duration, fanning out top-used tools, and grouping most-used models):

- **Get Agent Summary**: `GET /api/v1/agents/{id}/metrics/summary`  
  Returns aggregate statistics for a specific agent across all of its sessions (e.g., total tokens, average turn count, most-used tools, and most-used models).
- **Get Project Summary**: `GET /api/v1/projects/{id}/metrics/summary`  
  Returns roll-up metrics for all agents registered within a specific project.
- **Get Individual Session Record**: `GET /api/v1/metrics/session/{id}`  
  Returns the exact payload recorded for a single session.

:::note[Access Authorization]
These API endpoints are strictly authorization-gated. A caller must possess read access permissions to the parent project (or the agent) to query its session summaries, protecting operational and cost data from unauthorized users.
:::

### 3. Dashboard Visualization
The aggregated relational metrics are exposed directly within the Hub's Web UI:
- **Agent Detail "Metrics" Tab**: Displays the agent's total sessions, detailed token usage charts, and ranked list of tool invocation success rates.
- **Agents List Stats Columns**: Displays high-level stats columns (such as total token consumption) directly on the main agents index table.
- **Project Summary Panel**: Provides an operational cost and model usage dashboard across all agents in the selected project.


## Upgrading

`sciontool`'s telemetry code (including usage-metrics fixes) is compiled into the `scion-base` image; harness and hub images only build on top of it. A `sciontool`-side fix reaches running agents only after `scion-base` is rebuilt, then the harness/hub images on top of it — rebuilding harnesses alone against an old `scion-base` keeps the old telemetry behavior. See [Build provenance and stale sciontool](https://github.com/GoogleCloudPlatform/scion/blob/main/image-build/README.md#build-provenance-and-stale-sciontool) for how to check which commit is embedded in a given image and the required rebuild order.

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
