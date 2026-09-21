# Hosted Scion Metrics System Design

## Status
**In Progress** - Milestones 1-2.5 complete. Milestones 3-4 pending.

## 1. Overview

This document defines the metrics and observability architecture for the Hosted Scion platform. The design synthesizes research on LLM agent telemetry patterns (Codex, Gemini CLI, OpenCode) with the Hosted Scion architecture to create a unified observability strategy.

### Design Principles

1. **Sciontool as Primary Collector**: The `sciontool` binary running inside each agent container serves as the single point of telemetry collection, normalization, and forwarding.

2. **Cloud-Native Observability Backend**: Raw telemetry data (logs, traces, metrics) is forwarded to a dedicated cloud-based observability platform (e.g., Google Cloud Observability, Datadog, Honeycomb). The Hub does not become a general-purpose metrics or logging backend.

3. **Hub for High-Level Aggregates Only**: The Hub receives lightweight, pre-aggregated session and agent metrics for dashboard display, not raw telemetry streams. It can also fetch query-based aggregate data or recent logs from the cloud observability backend for presentation layer use.

4. **Configurable Filtering**: Sciontool provides event filtering to control volume, respect privacy settings, and honor debug mode configurations.

5. **Progressive Enhancement**: Initial implementation focuses on core metrics flow; advanced analytics via the web UI will come in a future phase.

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                           Agent Container                                   │
│                                                                             │
│  ┌─────────────────────┐                                                   │
│  │  Agent Process      │                                                   │
│  │  (Claude/Gemini)    │                                                   │
│  │                     │                                                   │
│  │  Emits:             │                                                   │
│  │  - OTLP (native)    │──────────┐                                        │
│  │  - JSON logs        │          │                                        │
│  │  - Hook events      │          │                                        │
│  └─────────────────────┘          │                                        │
│           │                       │                                        │
│           │ Hook calls            │ OTLP                                   │
│           ▼                       ▼                                        │
│  ┌─────────────────────────────────────────────────────────────┐           │
│  │                     Sciontool                                │           │
│  │                                                              │           │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │           │
│  │  │ Event        │  │ OTLP         │  │ Aggregation  │       │           │
│  │  │ Normalizer   │  │ Receiver     │  │ Engine       │       │           │
│  │  │              │  │ :4317        │  │              │       │           │
│  │  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘       │           │
│  │         │                 │                 │                │           │
│  │         └─────────────────┼─────────────────┘                │           │
│  │                           │                                  │           │
│  │                    ┌──────┴──────┐                          │           │
│  │                    │   Filter    │                          │           │
│  │                    │   Engine    │                          │           │
│  │                    └──────┬──────┘                          │           │
│  │                           │                                  │           │
│  │         ┌─────────────────┼─────────────────┐               │           │
│  │         │                 │                 │               │           │
│  │         ▼                 ▼                 ▼               │           │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐       │           │
│  │  │ Cloud        │  │ Hub          │  │ Local        │       │           │
│  │  │ Forwarder    │  │ Reporter     │  │ Debug        │       │           │
│  │  │              │  │              │  │ Output       │       │           │
│  │  └──────┬───────┘  └──────┬───────┘  └──────────────┘       │           │
│  │         │                 │                                  │           │
│  └─────────┼─────────────────┼──────────────────────────────────┘           │
│            │                 │                                              │
└────────────┼─────────────────┼──────────────────────────────────────────────┘
             │                 │
             │                 │
             ▼                 ▼
    ┌─────────────────┐  ┌─────────────────┐
    │ Cloud           │  │ Scion Hub       │
    │ Observability   │  │                 │
    │ Backend         │  │ Stores:         │
    │                 │  │ - Session       │
    │ - Full traces   │  │   summaries     │
    │ - All logs      │  │ - Agent metrics │
    │ - Raw metrics   │  │ - Activity      │
    │                 │  │                 │
    └─────────────────┘  └─────────────────┘
             │
             │ Query API
             ▼
    ┌─────────────────┐
    │ Web UI          │
    │ (Future)        │
    │                 │
    │ - Deep analytics│
    │ - Trace viewer  │
    │ - Log search    │
    └─────────────────┘
```

---

## 3. Sciontool as Primary Collector

### 3.1 Data Ingestion

Sciontool receives telemetry from agent processes through multiple channels:

| Channel | Source | Format | Example Events |
|---------|--------|--------|----------------|
| **OTLP Receiver** | Agents with native OTel (Codex, OpenCode) | OTLP gRPC/HTTP | Spans, metrics, logs |
| **Hook Events** | Harness hook calls | JSON via CLI args | `tool-start`, `tool-end`, `prompt-submit` |
| ~~**Session Files**~~ | ~~Gemini CLI session JSON~~ | ~~File watch/poll~~ | ~~Token counts, tool calls~~ (Removed — token metrics now sourced via native OTel) |
| **Stdout/Stderr** | Agent process output | Line-based text | Structured log lines |

### 3.2 Event Normalization

All ingested data is normalized to a common schema before processing. This enables harness-agnostic analytics.

#### Normalized Event Schema

```json
{
  "timestamp": "2026-02-02T10:30:00Z",
  "event_type": "agent.tool.call",
  "session_id": "uuid",
  "agent_id": "agent-abc123",
  "grove_id": "grove-xyz",

  "attributes": {
    "tool_name": "shell_execute",
    "duration_ms": 1250,
    "success": true,
    "model": "gemini-2.0-pro"
  },

  "metrics": {
    "tokens_input": 1500,
    "tokens_output": 450,
    "tokens_cached": 800
  }
}
```

#### Event Type Catalog

Based on the normalized metrics research, sciontool recognizes these event types:

| Event Type | Category | Description |
|------------|----------|-------------|
| `agent.session.start` | Lifecycle | Agent session initiated |
| `agent.session.end` | Lifecycle | Agent session completed |
| `agent.user.prompt` | Interaction | User input received |
| `agent.response.complete` | Interaction | Agent response finished |
| `agent.tool.call` | Tool Use | Tool execution started |
| `agent.tool.result` | Tool Use | Tool execution completed |
| `agent.approval.request` | Interaction | Permission requested from user |
| `gen_ai.api.request` | LLM | API call to LLM provider |
| `gen_ai.api.response` | LLM | Response received from LLM |
| `gen_ai.api.error` | LLM | API error occurred |

### 3.3 Dialect Parsing

Each harness emits events in its native format. Sciontool's dialect parsers translate these to the normalized schema.

```
┌──────────────────────────────────────────────────────────┐
│                    Dialect Parsers                       │
├──────────────────────────────────────────────────────────┤
│                                                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐      │
│  │ Claude      │  │ Gemini      │  │ OpenCode    │      │
│  │ Dialect     │  │ Dialect     │  │ Dialect     │      │
│  │             │  │             │  │             │      │
│  │ Parses:     │  │ Parses:     │  │ Parses:     │      │
│  │ - CC hooks  │  │ - Settings  │  │   JSON      │      │
│  │   events    │  │   JSON      │  │ - OTEL      │      │
│  │             │  │ - OTEL      │  │   events    │      │
│  │             │  │ - Session   │  │             │      │
│  │             │  │   Files     │  │             │      │
│  └─────────────┘  └─────────────┘  └─────────────┘      │
│         │                │                │              │
│         └────────────────┼────────────────┘              │
│                          ▼                               │
│              ┌─────────────────────┐                     │
│              │ Normalized Event    │                     │
│              │ Stream              │                     │
│              └─────────────────────┘                     │
└──────────────────────────────────────────────────────────┘
```

### 3.4 Harness Telemetry Injection

When `telemetry.enabled` is `true` in the scion configuration, each harness implementation injects configuration into the agent container to direct the harness's native telemetry to the sciontool OTLP collector. These settings are **hardcoded in the harness-specific implementation code** (`pkg/harness/`) and are distinct from sciontool's own forwarding configuration (Section 4/10), which controls how sciontool exports processed telemetry to the cloud.

**Key distinction:**
- **Harness config** (this section): Tells the agent process where to *emit* its native telemetry → sciontool collector at `localhost`
- **Sciontool config** (Section 10): Tells sciontool where to *forward* processed telemetry → cloud backend

The sciontool OTLP collector listens on:
- **gRPC**: `localhost:4317`
- **HTTP**: `localhost:4318`

There is no namespace collision between harness telemetry variables and sciontool's own configuration. Sciontool uses the `SCION_*` prefix (e.g., `SCION_OTEL_ENDPOINT`), while harnesses use their own namespaces (`GEMINI_TELEMETRY_*`, standard `OTEL_*`, or config files).

#### 3.4.1 Gemini CLI

Gemini CLI supports telemetry configuration via `GEMINI_TELEMETRY_*` environment variables. The harness injects the following:

| Environment Variable | Injected Value | Purpose |
|---|---|---|
| `GEMINI_TELEMETRY_ENABLED` | `true` | Enables Gemini's built-in telemetry |
| `GEMINI_TELEMETRY_TARGET` | `local` | Prevents Gemini from exporting directly to GCP |
| `GEMINI_TELEMETRY_USE_COLLECTOR` | `true` | Directs output to an external OTLP collector |
| `GEMINI_TELEMETRY_OTLP_ENDPOINT` | `http://localhost:4317` | Points to sciontool's gRPC receiver |
| `GEMINI_TELEMETRY_OTLP_PROTOCOL` | `grpc` | Uses gRPC transport |
| `GEMINI_TELEMETRY_LOG_PROMPTS` | `false` | Respects privacy defaults; prompts not forwarded |

**Notes:**
- `target=local` is critical — it prevents Gemini from attempting its own direct-to-GCP export, which would bypass sciontool's filtering and aggregation.
- `useCliAuth` is not set (defaults to `false`) since authentication to the cloud backend is handled by sciontool, not the harness.

#### 3.4.2 Claude Code

Claude Code uses standard OpenTelemetry environment variables for configuration. It supports both metrics and logs/events via OTLP.

| Environment Variable | Injected Value | Purpose |
|---|---|---|
| `CLAUDE_CODE_ENABLE_TELEMETRY` | `1` | Enables Claude Code's OTel instrumentation |
| `OTEL_METRICS_EXPORTER` | `otlp` | Routes metrics via OTLP |
| `OTEL_LOGS_EXPORTER` | `otlp` | Routes events/logs via OTLP |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | Uses gRPC transport |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | Points to sciontool's gRPC receiver |
| `OTEL_METRIC_EXPORT_INTERVAL` | `30000` | 30-second export interval (default 60s) |

**Not set (privacy defaults):**
- `OTEL_LOG_USER_PROMPTS` — defaults to disabled; prompt content is redacted (only length recorded).
- `OTEL_LOG_TOOL_DETAILS` — defaults to disabled; MCP server/tool names not logged.
- `OTEL_EXPORTER_OTLP_HEADERS` — no auth headers needed for localhost collector.

**Notes:**
- Claude Code emits both **metrics** (counters like `claude_code.token.usage`, `claude_code.session.count`) and **events** (structured logs like `claude_code.tool_result`, `claude_code.api_request`) as separate OTel signals. Both exporters must be enabled to capture the full telemetry picture.
- The `OTEL_*` variables are standard OTel SDK variables and do not collide with sciontool's `SCION_OTEL_*` namespace.
- Claude Code's metrics use `delta` temporality by default (`OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`), which is the preferred setting for the OTLP pipeline.

#### 3.4.3 Codex (OpenAI)

Codex uses a TOML configuration file (`~/.codex/config.toml`) rather than environment variables. The harness writes this file during container setup.

**Injected configuration:**

```toml
[otel]
exporter = { otlp-grpc = {
  endpoint = "http://localhost:4317"
}}
log_user_prompt = false
```

| Setting | Value | Purpose |
|---|---|---|
| `exporter` | `otlp-grpc` | Routes telemetry via gRPC to sciontool |
| `endpoint` | `http://localhost:4317` | Points to sciontool's gRPC receiver |
| `log_user_prompt` | `false` | Respects privacy defaults; prompts redacted |

**Notes:**
- Codex batches events and flushes on shutdown, so there is no configurable export interval.
- The `environment` field (`dev`/`staging`/`prod`) is not set; it is not required for local collector routing.
- Codex requires network access for OTel export. If the harness runs Codex with network disabled, the OTel export will silently fail. The harness must ensure localhost loopback is available.

#### 3.4.4 All Other Harnesses

Deferred. Future harnesses will follow the same pattern: inject configuration directing native telemetry to `localhost:4317` (gRPC) or `localhost:4318` (HTTP). Harness implementations should prefer gRPC when supported by the agent.

#### 3.4.5 Data Signal Summary

The following table summarizes what each harness emits natively and how it reaches the sciontool pipeline:

| Harness | Traces/Spans | Metrics | Logs/Events | Config Method |
|---|---|---|---|---|
| Gemini CLI | ✓ (OTel native) | ✓ (OTel native) | ✓ (OTel native) | Environment variables |
| Claude Code | — | ✓ (OTel native) | ✓ (OTel native) | Environment variables |
| Codex | — | — | ✓ (OTel logs) | TOML config file |

For harnesses that do not emit certain signal types natively (e.g., Claude Code does not emit traces), sciontool's hook-based normalization (Section 3.3) and the TelemetryHandler (Milestone 2) fill the gap by converting hook events into OTLP spans.

---

## 4. Data Destinations

### 4.1 Cloud Observability Backend (Primary)

The majority of telemetry data is forwarded to a cloud-based observability platform. This enables:

- Full-fidelity trace analysis
- Log search and aggregation
- Long-term metric storage
- Advanced querying and dashboards

**Supported Backends (Initial):**

| Backend | Protocol | Use Case |
|---------|----------|----------|
| Google Cloud Observability | OTLP | GCP-native deployments |
| Generic OTLP Collector | OTLP gRPC/HTTP | Self-hosted, multi-cloud |

#### Forward Configuration

```yaml
# sciontool config (injected via env or config file)
telemetry:
  cloud:
    enabled: true
    endpoint: "otel-collector.example.com:4317"
    protocol: "grpc"  # grpc, http
    headers:
      Authorization: "Bearer ${OTEL_API_KEY}"

    # Batch settings for efficiency
    batch:
      maxSize: 512
      timeout: "5s"

    # TLS configuration
    tls:
      enabled: true
      insecureSkipVerify: false
```

#### Data Forwarded to Cloud

| Data Type | Volume | Retention (typical) |
|-----------|--------|---------------------|
| Traces | All spans | 14-30 days |
| Logs | All agent logs | 30-90 days |
| Metrics | All counters/histograms | 13 months |

### 4.2 Hub Reporting (Aggregated)

The Hub receives only lightweight, pre-aggregated data for display in the web dashboard. This keeps the Hub focused on its core responsibility: state management.

**Data Sent to Hub:**

| Metric | Aggregation | Frequency |
|--------|-------------|-----------|
| Session summary | Per-session | On session end |
| Token usage | Per-session totals | On session end |
| Tool call counts | Per-session by tool | On session end |
| Agent status | Current state | On change |
| Error counts | Rolling 1-hour window | Every 5 minutes |

#### Hub Reporting Protocol

Sciontool reports to the Hub via the existing daemon heartbeat channel, extending the payload:

```json
{
  "type": "agent_metrics",
  "agent_id": "agent-abc123",
  "timestamp": "2026-02-02T10:35:00Z",

  "session": {
    "id": "session-uuid",
    "started_at": "2026-02-02T10:00:00Z",
    "ended_at": "2026-02-02T10:35:00Z",
    "status": "completed",
    "turn_count": 15,
    "model": "gemini-2.0-pro"
  },

  "tokens": {
    "input": 45000,
    "output": 12000,
    "cached": 30000,
    "reasoning": 5000
  },

  "tools": {
    "shell_execute": { "calls": 8, "success": 7, "error": 1 },
    "read_file": { "calls": 25, "success": 25, "error": 0 },
    "write_file": { "calls": 4, "success": 4, "error": 0 }
  },

  "languages": ["TypeScript", "Go", "Markdown"]
}
```

#### Hub Storage

The Hub stores these summaries in a dedicated table (not raw events):

```sql
CREATE TABLE agent_session_metrics (
    id              TEXT PRIMARY KEY,
    agent_id        TEXT NOT NULL,
    grove_id        TEXT NOT NULL,
    session_id      TEXT NOT NULL,

    started_at      TIMESTAMP NOT NULL,
    ended_at        TIMESTAMP,
    status          TEXT,

    turn_count      INTEGER,
    model           TEXT,

    tokens_input    INTEGER,
    tokens_output   INTEGER,
    tokens_cached   INTEGER,
    tokens_reasoning INTEGER,

    tool_calls      JSONB,  -- {"tool_name": {"calls": N, "success": N, "error": N}}
    languages       TEXT[], -- ["TypeScript", "Go"]

    -- cost_estimate   DECIMAL(10, 6), -- Postponed to future phase

    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

    FOREIGN KEY (agent_id) REFERENCES agents(id),
    FOREIGN KEY (grove_id) REFERENCES groves(id)
);

CREATE INDEX idx_session_metrics_agent ON agent_session_metrics(agent_id);
CREATE INDEX idx_session_metrics_grove ON agent_session_metrics(grove_id);
CREATE INDEX idx_session_metrics_time ON agent_session_metrics(started_at);
```

### 4.3 Local Debug Output

In debug mode or when cloud forwarding is disabled, sciontool can output telemetry locally for troubleshooting.

| Output | Trigger | Format |
|--------|---------|--------|
| Console (stderr) | `SCION_LOG_LEVEL=debug` | Structured text |
| File | `telemetry.local.file` configured | JSONL |
| Debug endpoint | `telemetry.local.endpoint` | OTLP to localhost |

---

## 5. Filtering and Sampling

Sciontool provides configurable filtering to manage telemetry volume and respect privacy requirements.

### 5.1 Filter Configuration

```yaml
telemetry:
  filter:
    # Global enable/disable
    enabled: true

    # Respect debug mode (SCION_LOG_LEVEL)
    respectDebugMode: true

    # Event type filtering
    events:
      # Include list (if set, only these are forwarded)
      include: []

      # Exclude list (these are never forwarded)
      exclude:
        - "agent.user.prompt"  # Privacy: don't forward user prompts by default

    # Attribute filtering
    attributes:
      # Fields to redact (replaced with "[REDACTED]")
      redact:
        - "prompt"
        - "user.email"
        - "tool_output"  # May contain sensitive file contents

      # Fields to hash (replaced with SHA256 hash)
      hash:
        - "session_id"  # For correlation without exposing raw IDs

    # Sampling (for high-volume events)
    sampling:
      # Default sample rate (1.0 = 100%)
      default: 1.0

      # Per-event-type rates
      rates:
        "gen_ai.api.request": 0.1  # Sample 10% of API requests
        "agent.tool.result": 0.5   # Sample 50% of tool results
```

### 5.2 Debug Mode Behavior

When debug mode is enabled (`SCION_LOG_LEVEL=debug`):

1. All filtering is bypassed for local output
2. Sampling rates are ignored for local output
3. Cloud forwarding still respects privacy filters (redaction)
4. Additional diagnostic events are emitted

### 5.3 Privacy Defaults

Out of the box, sciontool applies these privacy-preserving defaults:

| Data | Default Behavior | Rationale |
|------|------------------|-----------|
| User prompts | Redacted | May contain sensitive instructions |
| Tool output | Redacted | May contain file contents, credentials |
| User email | Redacted | PII |
| Session ID | Hashed | Allow correlation without exposure |
| Agent ID | Passed through | Required for routing |
| Token counts | Passed through | Non-sensitive, needed for cost tracking |

Users can opt-in to full prompt/output logging via configuration:

```yaml
telemetry:
  filter:
    attributes:
      # Override defaults to allow prompt logging
      redact: []  # Empty = no redaction
```

---

## 6. Hub Metrics API

The Hub exposes an API for retrieving aggregated metrics for display in the web UI.

### 6.1 Endpoints

#### Get Agent Metrics Summary

```
GET /api/v1/agents/{agentId}/metrics/summary
```

**Response:**
```json
{
  "agent_id": "agent-abc123",
  "period": "24h",

  "sessions": {
    "total": 15,
    "completed": 14,
    "errored": 1
  },

  "tokens": {
    "input": 450000,
    "output": 120000,
    "cached": 300000
  },

  "top_tools": [
    { "name": "read_file", "calls": 250, "success_rate": 1.0 },
    { "name": "shell_execute", "calls": 80, "success_rate": 0.95 },
    { "name": "write_file", "calls": 40, "success_rate": 1.0 }
  ],

  "languages": ["TypeScript", "Go", "Python"]
}
```

#### Get Grove Metrics Summary

```
GET /api/v1/groves/{groveId}/metrics/summary
```

Returns aggregated metrics across all agents in the grove.

#### Get Metrics Time Series

```
GET /api/v1/groves/{groveId}/metrics/timeseries?metric=tokens.input&period=7d&interval=1h
```

Returns time-bucketed metric values for charting.

### 6.2 What the Hub Does NOT Provide

The Hub explicitly does **not** provide:

- Raw log search/retrieval
- Trace viewing
- Full-fidelity metric queries
- Log aggregation pipelines

These capabilities are delegated to the cloud observability backend.

---

## 7. Future: Web UI Observability Features

In a future phase, the web UI will provide deeper observability by fetching data from the cloud backend.

### 7.1 Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        Web UI                               │
│                                                             │
│  ┌───────────────────┐  ┌───────────────────────────────┐  │
│  │ Dashboard         │  │ Deep Analytics (Future)       │  │
│  │                   │  │                               │  │
│  │ Data from: Hub    │  │ Data from: Cloud Backend      │  │
│  │                   │  │                               │  │
│  │ - Session counts  │  │ - Trace viewer                │  │
│  │ - Token totals    │  │ - Log search                  │  │
│  │ - Cost estimates  │  │ - Custom queries              │  │
│  │ - Agent status    │  │ - Anomaly detection           │  │
│  └───────────────────┘  └───────────────────────────────┘  │
│           │                          │                      │
└───────────┼──────────────────────────┼──────────────────────┘
            │                          │
            ▼                          ▼
     ┌─────────────┐          ┌─────────────────────┐
     │  Scion Hub  │          │ Cloud Observability │
     │  API        │          │ Query API           │
     └─────────────┘          └─────────────────────┘
```

### 7.2 Planned Features

| Feature | Data Source | Priority |
|---------|-------------|----------|
| Session list with metrics | Hub | P1 |
| Token usage charts | Hub | P1 |
| Cost tracking dashboard | Hub | P1 |
| Trace waterfall view | Cloud Backend | P2 |
| Log search | Cloud Backend | P2 |
| Tool execution timeline | Cloud Backend | P2 |
| Error analysis | Cloud Backend | P3 |
| Custom metric queries | Cloud Backend | P3 |

### 7.3 Cloud Backend Integration

The web UI will authenticate to the cloud backend using one of:

1. **Proxy through Hub**: Hub makes cloud queries on behalf of UI (simpler auth)
2. **Direct with short-lived tokens**: Hub issues tokens for UI to query cloud directly

The specific approach will be determined based on the chosen cloud backend.

---

## 8. Implementation Phases

### Phase 1: Core Telemetry Pipeline

**Goal:** Establish basic telemetry flow from agents to cloud backend.

| Task | Component | Notes |
|------|-----------|-------|
| OTLP receiver in sciontool | `pkg/sciontool/telemetry` | Receive from OTel-native agents |
| Cloud forwarder | `pkg/sciontool/telemetry` | OTLP export to cloud backend |
| Basic filtering | `pkg/sciontool/telemetry` | Event include/exclude |
| Configuration loading | `cmd/sciontool` | Environment + config file |

### Phase 2: Harness Integration

**Goal:** Capture telemetry from all harness types.

| Task | Component | Notes |
|------|-----------|-------|
| Hook event normalization | `pkg/sciontool/hooks` | Convert hook calls to events |
| ~~Gemini session file parsing~~ | ~~`pkg/sciontool/hooks/dialects`~~ | ~~Read session-*.json~~ (Removed) |
| Claude dialect parser | `pkg/sciontool/hooks/dialects` | Parse CC hook payloads |

### Phase 3: Hub Aggregation

**Goal:** Report session summaries to Hub.

| Task | Component | Notes |
|------|-----------|-------|
| In-memory aggregation engine | `pkg/sciontool/telemetry` | Per-session accumulators |
| Hub reporter | `pkg/sciontool/hub` | Extend heartbeat protocol |
| Hub metrics storage | `pkg/hub/store` | agent_session_metrics table |
| Hub metrics API | `pkg/hub/api` | Summary endpoints |

### Phase 4: Web UI Integration

**Goal:** Display metrics in web dashboard.

| Task | Component | Notes |
|------|-----------|-------|
| Session metrics component | `web/src/client` | Display session stats |
| Token usage charts | `web/src/client` | Visualization |
| Cost tracking | `web/src/client` | Aggregate cost display |

### Phase 5: Advanced Analytics (Future)

**Goal:** Deep observability via cloud backend.

| Task | Component | Notes |
|------|-----------|-------|
| Cloud backend query proxy | `pkg/hub/api` or Web | TBD |
| Trace viewer | `web/src/client` | Embedded trace UI |
| Log search | `web/src/client` | Query interface |

## 9. System Component Logging

While `sciontool` handles telemetry for agents, the Hub and Runtime Broker servers require a robust internal logging strategy for operational observability.

### 9.1 Structured Logging with slog

All backend components (Hub, Runtime Broker) must use the Go standard library's `log/slog` package for structured logging.

- **Standardization**: Consistent key names across all components (e.g., `msg`, `level`, `time`, `component`, `trace_id`).
- **Performance**: High-performance structured logging with minimal allocation overhead.
- **Interoperability**: Standard interface allowing for easy handler swaps.

### 9.2 Log Levels and Verbosity

Logs are emitted at several levels:
- `DEBUG`: Detailed information for troubleshooting. Only emitted when explicitly enabled.
- `INFO`: Normal operational events (startup, shutdown, significant state changes).
- `WARN`: Unexpected events that don't stop the service (e.g., transient network errors).
- `ERROR`: Critical failures requiring attention.

Debug logging can be enabled globally or per-component via the `SCION_LOG_LEVEL=debug` environment variable.

### 9.3 OTel Log Bridge Architecture

In an OpenTelemetry-native environment, we employ a "Log Bridge" approach instead of custom log exporters. We use the official OTel bridge to connect the standard `log/slog` API to the OpenTelemetry Logs SDK.

- **Concept**: `slog` acts as the "frontend" API that developers interact with, while the OTel SDK acts as the "backend" that handles batching, resource attribution, and exporting to the OTLP forwarder.
- **Implementation**: We utilize the `go.opentelemetry.io/contrib/bridges/otelslog` package.

#### Implementation Pattern

1.  **Configure OTel LoggerProvider**: Initialize the OTel SDK with an OTLP exporter (pointing to the Collector/Backend).
2.  **Create Bridge Handler**: Wrap the LoggerProvider in an `otelslog.Handler`.
3.  **Set Default Logger**: Replace the global default logger or inject the bridge logger into the application context.

```go
import (
    "context"
    "log/slog"
    "go.opentelemetry.io/contrib/bridges/otelslog"
    "go.opentelemetry.io/otel/log/global"
)

func main() {
    // 1. Setup your existing OTel LoggerProvider (which points to your forwarder)
    lp := setupOTelLoggerProvider()

    // 2. Create the slog handler using the bridge
    // The "scion-hub" string defines the Instrumentation Scope
    otlpHandler := otelslog.NewHandler("scion-hub", otelslog.WithLoggerProvider(lp))

    // 3. Set as default
    logger := slog.New(otlpHandler)
    slog.SetDefault(logger)

    // 4. Usage (Always use context-aware methods for trace correlation!)
    slog.InfoContext(ctx, "processed request", "bytes", 1024)
}
```

### 9.4 Contextual Metadata

To facilitate debugging across distributed components, the following fields should be included in log records where applicable:
- `grove_id`: The ID of the grove being processed.
- `agent_id`: The ID of the agent involved.
- `request_id`: A unique ID for the incoming API request.
- `user_id`: The ID of the authenticated user.

---

## 10. Configuration Reference

### 10.1 Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_OTEL_ENDPOINT` | Cloud OTLP endpoint | (required if cloud enabled) |
| `SCION_OTEL_PROTOCOL` | OTLP protocol (grpc, http) | `grpc` |
| `SCION_OTEL_HEADERS` | Additional headers (JSON) | `{}` |
| `SCION_OTEL_INSECURE` | Skip TLS verification | `false` |
| `SCION_TELEMETRY_ENABLED` | Enable telemetry collection | `true` |
| `SCION_TELEMETRY_CLOUD_ENABLED` | Forward to cloud backend | `true` |
| `SCION_TELEMETRY_HUB_ENABLED` | Report to Hub | `true` (if hosted mode) |
| `SCION_TELEMETRY_DEBUG` | Local debug output | `false` |
| `SCION_LOG_LEVEL` | Logging verbosity | `info` |

### 10.2 Full Configuration File

```yaml
telemetry:
  enabled: true

  # Cloud forwarding
  cloud:
    enabled: true
    endpoint: "${SCION_OTEL_ENDPOINT}"
    protocol: "grpc"
    headers:
      Authorization: "Bearer ${OTEL_API_KEY}"
    tls:
      enabled: true
      insecureSkipVerify: false
    batch:
      maxSize: 512
      timeout: "5s"

  # Hub reporting
  hub:
    enabled: true  # Auto-enabled in hosted mode
    reportInterval: "30s"

  # Local debug output
  local:
    enabled: false
    file: ""  # If set, write JSONL to file
    console: false  # If true, write to stderr

  # Filtering
  filter:
    enabled: true
    respectDebugMode: true

    events:
      include: []  # Empty = all
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

  # Resource attributes (added to all events)
  resource:
    service.name: "scion-agent"
    # Additional attributes from environment:
    # agent.id, grove.id, runtime.broker populated automatically
```

### 10.3 Implementation Notes: Settings Schema Integration

The telemetry configuration block from section 10.2 has been integrated into the
Scion v1 settings schema, enabling configuration at every scope in the hierarchy.

#### Field Naming Convention

The design doc (section 10.2) uses camelCase YAML keys (`insecureSkipVerify`,
`reportInterval`, `respectDebugMode`, `maxSize`). The v1 settings schema uses
snake_case consistently. The implementation normalizes all fields to snake_case:

| Design Doc (camelCase) | Settings Schema (snake_case) |
|------------------------|------------------------------|
| `insecureSkipVerify`   | `insecure_skip_verify`       |
| `reportInterval`       | `report_interval`            |
| `respectDebugMode`     | `respect_debug_mode`         |
| `maxSize`              | `max_size`                   |

#### Scope Hierarchy & Merge Order

Telemetry settings are resolved across four scopes using **last-write-wins**
semantics. Each scope can set any subset of the telemetry block; unset fields
inherit from the previous scope.

```
1. Embedded defaults (pkg/config/embeds/default_settings.yaml)
2. Global settings   (~/.scion/settings.yaml)         → telemetry.*
3. Grove settings    (.scion/settings.yaml)            → telemetry.*
4. Template config   (scion-agent.yaml in template)    → telemetry.*
5. Agent config      (scion-agent.yaml in agent home)  → telemetry.*
6. Environment vars  (SCION_TELEMETRY_*, SCION_OTEL_*) → highest priority
```

Scopes 1–3 use the `VersionedSettings.Telemetry` field (`V1TelemetryConfig`)
loaded via Koanf with automatic merging. Scopes 4–5 use the
`ScionConfig.Telemetry` field (`api.TelemetryConfig`) merged via
`MergeScionConfig` → `mergeTelemetryConfig`. Scope 6 applies via env var mapping
in Koanf's env provider.

#### Environment Variable Mapping

Telemetry env vars map to settings paths via `versionedEnvKeyMapper`:

| Environment Variable                          | Settings Path                                |
|------------------------------------------------|----------------------------------------------|
| `SCION_TELEMETRY_ENABLED`                      | `telemetry.enabled`                          |
| `SCION_TELEMETRY_CLOUD_ENABLED`                | `telemetry.cloud.enabled`                    |
| `SCION_TELEMETRY_CLOUD_TLS_INSECURE_SKIP_VERIFY` | `telemetry.cloud.tls.insecure_skip_verify` |
| `SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE`         | `telemetry.cloud.batch.max_size`             |
| `SCION_TELEMETRY_HUB_ENABLED`                  | `telemetry.hub.enabled`                      |
| `SCION_TELEMETRY_HUB_REPORT_INTERVAL`          | `telemetry.hub.report_interval`              |
| `SCION_TELEMETRY_LOCAL_ENABLED`                 | `telemetry.local.enabled`                    |
| `SCION_TELEMETRY_FILTER_ENABLED`                | `telemetry.filter.enabled`                   |
| `SCION_TELEMETRY_FILTER_RESPECT_DEBUG_MODE`     | `telemetry.filter.respect_debug_mode`        |
| `SCION_TELEMETRY_DEBUG`                         | `telemetry.local.enabled`                    |

The `SCION_OTEL_*` variables from section 10.1 are aliased into the
`telemetry.cloud` sub-tree:

| Environment Variable    | Settings Path                                |
|-------------------------|----------------------------------------------|
| `SCION_OTEL_ENDPOINT`  | `telemetry.cloud.endpoint`                   |
| `SCION_OTEL_PROTOCOL`  | `telemetry.cloud.protocol`                   |
| `SCION_OTEL_HEADERS`   | `telemetry.cloud.headers`                    |
| `SCION_OTEL_INSECURE`  | `telemetry.cloud.tls.insecure_skip_verify`   |

#### Files Modified

- `pkg/config/settings_v1.go` — `V1TelemetryConfig` and sub-structs; `Telemetry`
  field on `VersionedSettings`; `mapTelemetryEnvKey`, `mapOtelEnvKey` functions.
- `pkg/config/schemas/settings-v1.schema.json` — `telemetry` property and
  `$defs` for all sub-schemas (`telemetryConfig`, `telemetryCloud`, etc.).
- `pkg/config/schemas/agent-v1.schema.json` — `telemetry` property and
  `telemetryConfig` definition for template/agent-level overrides.
- `pkg/api/types.go` — `TelemetryConfig` and sub-structs; `Telemetry` field on
  `ScionConfig`.
- `pkg/config/templates.go` — `mergeTelemetryConfig` function and its invocation
  in `MergeScionConfig`.
- `pkg/config/settings_v1_test.go` — Round-trip, validation, hierarchy merge,
  and env override tests.
- `pkg/config/templates_test.go` — `MergeScionConfig` telemetry merge tests;
  agent config validation test.

#### Harness-Specific Env Vars

Harness-native telemetry env vars (e.g., `GEMINI_TELEMETRY_*`, standard
`OTEL_EXPORTER_*`) are injected at agent start time via the existing harness
`Env` mechanism. These tell the harness process where to emit raw OTLP data
(typically `localhost:4317` for the sciontool collector). They are not part of
this settings schema since they use provider-specific namespaces.

---

## 11. Open Questions

### 11.1 Cloud Backend Selection

**Decision:** Google Cloud Observability (Cloud Trace, Cloud Logging, Cloud Monitoring) is the primary target for the initial implementation.

**Options considered:**
1. **Google Cloud Observability** (Selected): Native GCP integration, unified with existing infra.
2. Generic OTLP Collector: Flexibility but higher operational overhead.
3. Honeycomb: Excellent UX but potential cost at scale.

**Impact:** Configuration and authentication will assume GCP-native identity (Workload Identity) or service account keys.

### 11.2 Prompt Logging Opt-In

**Decision:** Opt-in is managed at the **Grove** level by the grove administrator.

**Details:**
- Configured in the Grove settings on the Hub.
- When enabled, prompt and response logs are routed to a specific log destination (e.g., a restricted Cloud Logging bucket) to segregate sensitive content.

### 11.3 Cost Estimation Accuracy

**Decision:** Financial cost calculation is postponed. The system will track **token usage only** in the initial release.

**Rationale:**
- Pricing is complex and volatile.
- A future module may provide a price table function to convert token counts to approximate financial cost.

### 11.4 Session File Watching

**Decision:** **End-of-session parsing only** for Gemini CLI.

**Rationale:**
- Simpler implementation than real-time file watching.
- It is currently unclear if real-time session file parsing provides significant value over the OTel data stream.

### 11.5 Multi-Model Sessions

**Decision:** Metrics will be **broken down by model** within the session summary.

**Details:**
- The `agent_session_metrics` table and Hub API will support detailed breakdowns of token usage per model, rather than attributing everything to a single primary model.

### 11.6 Cross-Agent Correlation

**Decision:** Postponed.

**Details:**
- Initial implementation treats agents as independent.
- Future cross-agent correlation will likely be mediated by the Hub using shared identifiers when it orchestrates multi-agent workflows.

### 11.7 Retention and Archival

**Decision:** **Indefinite retention** of Hub-stored session summaries.

**Details:**
- The data volume for session summaries is low enough to retain indefinitely.
- Manual purge or cleanup scripts can be developed if storage becomes an issue.

### 11.8 Credential Injection for Agents

**Decision:** **Out of Scope**.

**Details:**
- We will assume that the key libraries will be able to load via the 'application default credentials' pattern.
- It will be up to the runtime broker design to ensure these are available to the sciontool environment.

### 11.9 Data Resiliency

**Decision:** **Configurable Flush Interval**.

**Details:**
- The flush interval will be made a configurable option with a sane default.
- Users who value metrics at the expense of load can choose a shorter interval to minimize data loss risk on crash.

### 11.10 Stdout/Stderr Handling

**Decision:** **Resolved**.

**Details:**
- This is now captured in Section 9.3 (OTel Log Bridge Architecture).

---

## 12. Engineering Milestones

### Milestone 1: Telemetry Foundation (Sciontool) ✅ COMPLETE

**Goal:** Enable `sciontool` to accept OTLP data and forward it to the Google Cloud backend.

**Status:** Completed 2026-02-05

**Deliverables:**
- [x] **OTLP Receiver**: Embedded receiver in `sciontool` listening on default ports (4317/4318).
- [x] **Cloud Forwarder**: Exporter for Google Cloud Trace/Monitoring/Logging.
- [x] **Configuration**: `telemetry` config block parsing and environment variable injection.
- [x] **Basic Filtering**: Implementation of include/exclude logic for event types.

**Test Criteria:**
- `sciontool` starts without errors with telemetry enabled.
- Can send dummy OTLP data (via `otel-cli` or similar) to localhost:4317.
- Dummy data appears in Google Cloud Console (Trace/Log Viewer).

#### Implementation Notes

**Package Structure:** `pkg/sciontool/telemetry/`

| File | Description |
|------|-------------|
| `config.go` | Configuration loading from env vars (SCION_TELEMETRY_*, SCION_OTEL_*) |
| `filter.go` | Include/exclude filtering with privacy default (agent.user.prompt excluded) |
| `exporter.go` | OTLP gRPC/HTTP exporter with raw proto forwarding (traces + metrics) |
| `receiver.go` | Embedded OTLP gRPC (4317) and HTTP (4318) receivers (TraceService + MetricsService) |
| `pipeline.go` | Main orchestration: Start/Stop lifecycle, span + metric handlers |
| `providers.go` | SDK TracerProvider, LoggerProvider, and MeterProvider initialization |
| `*_test.go` | Unit tests for config, filter, and pipeline |

**Key Design Decisions:**

1. **Environment-first configuration**: Follows `hub/client.go` pattern with `SCION_*` env vars.
2. **Non-blocking startup**: Telemetry failures log errors but don't block agent startup.
3. **Privacy default**: `agent.user.prompt` excluded by default.
4. **Raw proto forwarding**: Uses `ExportProtoSpans()` to forward OTLP data directly without SDK span conversion (avoids `ReadOnlySpan` private method constraint).
5. **Graceful shutdown**: The init telemetry Stop path allows 20 seconds after
   child exit for an in-flight metric export and one safe 15-second write slot;
   it reports residual state when that budget expires. External runtime stop
   deadlines can be shorter and may force termination first.

**Integration Point:** `cmd/sciontool/commands/init.go`
- Pipeline starts after `setupHostUser()` and before lifecycle hooks.
- Deferred shutdown ensures flush before container exit.

**Dependencies Added:**
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
- `go.opentelemetry.io/proto/otlp` (upgraded to v1.9.0)

### Milestone 2: Harness Data & Log Bridge ✅ COMPLETE

**Goal:** Normalize data from harnesses and system components into the telemetry stream.

**Status:** Completed 2026-02-06

**Deliverables:**
- [x] **Hook Normalization**: TelemetryHandler converts harness hooks to OTLP spans with `agent.*` naming.
- [x] ~~**Session Parsing**: Logic to parse Gemini CLI `session-*.json` files on session end.~~ Removed — Gemini session file parsing was harness-specific and never fully exercised; token metrics are now sourced via the native OTel pipeline.
- [x] **Log Bridge**: `otelslog` integration for Hub and Runtime Broker structured logging.
- [x] **Attribute Redaction**: Privacy filter implementation for sensitive fields (redact + hash).

**Test Criteria:**
- Run a Gemini agent session: tool calls appear as spans in GCP Trace.
- Agent logs (stdout/stderr) appear in GCP Logging with correct `agent_id` labels.
- Sensitive data (prompts) is redacted or absent based on config.

#### Implementation Notes

**Package Structure:**

| File | Description |
|------|-------------|
| `pkg/sciontool/hooks/handlers/telemetry.go` | TelemetryHandler converts hook events to OTLP spans |
| `pkg/sciontool/telemetry/filter.go` | Extended with `Redactor` for attribute redaction/hashing |
| `pkg/util/logging/otel.go` | Multi-handler and OTel bridge support |
| `pkg/util/logging/otel_provider.go` | LoggerProvider initialization for OTel log bridge |

**Hook-to-Span Mapping:**

| Hook Event | Span Name | Key Attributes |
|------------|-----------|----------------|
| `session-start` | `agent.session.start` | session_id, source |
| `session-end` | `agent.session.end` | session_id, reason |
| `tool-start` | `agent.tool.call` | tool_name, tool_input (redacted) |
| `tool-end` | `agent.tool.result` | tool_name, success, duration_ms |
| `prompt-submit` | `agent.user.prompt` | prompt (redacted) |
| `model-start` | `gen_ai.api.request` | model |
| `model-end` | `gen_ai.api.response` | success |

**Redaction Configuration:**

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `SCION_TELEMETRY_REDACT` | `prompt,user.email,tool_output,tool_input` | Fields replaced with `[REDACTED]` |
| `SCION_TELEMETRY_HASH` | `session_id` | Fields replaced with SHA256 hash |

**OTel Log Bridge Pattern:**

The Hub and Runtime Broker use a multi-handler approach:
1. Base handler (JSON or GCP-formatted) for local output
2. OTel bridge handler for forwarding to OTLP endpoint
3. Both handlers receive all log records simultaneously

Enabled via `SCION_OTEL_LOG_ENABLED=true` with endpoint in `SCION_OTEL_ENDPOINT`.

**Dependencies Added:**
- `go.opentelemetry.io/contrib/bridges/otelslog`
- `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc`
- `go.opentelemetry.io/otel/sdk/log`

### Milestone 2.5: OTel Metrics Pipeline & Correlated Logs ✅ COMPLETE

**Goal:** Extend the trace-only telemetry pipeline to support native OTel metrics (counters, histograms) and emit correlated log records alongside spans for hook events. Addresses the improvements outlined in `.design/hosted/metrics-improvements.md` (sections "Add logs with event data associated with spans" and "Include a proper metrics pipeline").

**Status:** Completed 2026-02-06

**Deliverables:**
- [x] **MeterProvider**: SDK `metric.MeterProvider` created alongside `TracerProvider` and `LoggerProvider` in `Providers` struct, using `otlpmetricgrpc` exporter with periodic reader.
- [x] **Metric Receiver**: `MetricsServiceServer` (gRPC) and `/v1/metrics` (HTTP) endpoints added to the embedded OTLP receiver for agents that natively emit OTel metrics.
- [x] **Metric Exporter**: `ExportProtoMetrics()` method on `CloudExporter` for raw proto metric forwarding to cloud endpoint, reusing the existing gRPC connection.
- [x] **Pipeline Wiring**: `handleMetrics()` method on `Pipeline`, forwarding received metrics to the cloud exporter (parallel to `handleSpans`).
- [x] **TelemetryHandler Instruments**: Eight OTel metric instruments recording counters and histograms on hook events.
- [x] **Correlated OTel Logs**: `otelslog`-bridged logger on `TelemetryHandler` emitting log records with trace/span correlation on every hook event.

**Test Criteria:**
- `go build` succeeds with all new metric types.
- Unit tests verify MeterProvider creation, metric instrument initialization, and metric recording on tool-end, model-end, and session-end events.
- Pipeline tests verify metric handler registration and forwarding path.

#### Implementation Notes

**Metric Instruments (TelemetryHandler):**

| Instrument | Type | Unit | Recorded On | Labels |
|---|---|---|---|---|
| `gen_ai.tokens.input` | Counter (Int64) | `{token}` | `session-end` | model, harness, agent_id |
| `gen_ai.tokens.output` | Counter (Int64) | `{token}` | `session-end` | model, harness, agent_id |
| `gen_ai.tokens.cached` | Counter (Int64) | `{token}` | `session-end` | model, harness, agent_id |
| `agent.tool.calls` | Counter (Int64) | `{call}` | `tool-end` | tool_name, status, harness |
| `agent.tool.duration` | Histogram (Float64) | `ms` | `tool-end` | tool_name, harness |
| `agent.session.count` | Counter (Int64) | `{session}` | `session-end` | harness, status |
| `gen_ai.api.calls` | Counter (Int64) | `{call}` | `model-end` | model, status |
| `gen_ai.api.duration` | Histogram (Float64) | `ms` | `model-end` | model |

Token counters (`gen_ai.tokens.*`) are populated via the native OTel metrics pipeline from harnesses that emit OTLP metrics (e.g., Claude Code, Gemini CLI). Gemini session file parsing was removed as it was harness-specific and redundant with the OTel path.

**Label Sources:**

Labels are derived from environment variables injected into the agent container:
- `agent_id` from `SCION_AGENT_ID`
- `harness` from `SCION_HARNESS`
- `model` from `SCION_MODEL`
- `tool_name` and `status` from the hook event data

**Architecture:**

The metrics pipeline mirrors the existing trace pipeline:
```
Hook Events → TelemetryHandler → MeterProvider → OTLP Metric Exporter → Cloud
                                                                          ↑
Agent (native OTLP) → Receiver (MetricsService) → Pipeline → Exporter ───┘
```

**Correlated Log Records:**

Each hook event emits a log record via `otelslog` with the span name as the log body and all event attributes as log attributes. The `otelslog` bridge automatically extracts `trace_id` and `span_id` from the span context, enabling click-through from trace waterfall to associated logs in GCP Console.

**Key Design Decisions:**

1. **Metric instruments on TelemetryHandler**: Instruments are defined on the handler rather than a separate metrics package, since the handler is the natural emission point (same pattern as spans).
2. **Variadic MeterProvider parameter**: `NewTelemetryHandler` accepts `mp ...metric.MeterProvider` to maintain backward compatibility with existing callers.
3. **No metric filtering**: All metrics are forwarded without filtering. Metric volume is inherently lower than trace volume, so filtering is unnecessary at this stage.
4. **Receiver options pattern**: `WithMetricHandler()` functional option on `NewReceiver` to avoid breaking the existing span-only constructor signature.
5. **Shared gRPC connection**: The metric client reuses the same `grpc.ClientConn` as the trace client in `CloudExporter`.

**Files Modified:**

| File | Changes |
|------|---------|
| `pkg/sciontool/telemetry/providers.go` | Added `MeterProvider` field, `otlpmetricgrpc` exporter, periodic reader, shutdown |
| `pkg/sciontool/telemetry/receiver.go` | Added `MetricHandler`, `MetricsServiceServer`, `/v1/metrics` HTTP handler, `ReceiverOption` |
| `pkg/sciontool/telemetry/exporter.go` | Added `MetricsServiceClient`, `ExportProtoMetrics()` method |
| `pkg/sciontool/telemetry/pipeline.go` | Added `handleMetrics()`, wired metric handler to receiver |
| `pkg/sciontool/hooks/handlers/telemetry.go` | Added 8 metric instruments, `initMetrics()`, `recordEndMetrics()`, `recordSessionMetrics()` (session count only; session file parsing removed), correlated log emission |
| `cmd/sciontool/commands/hook.go` | Pass `MeterProvider` to `NewTelemetryHandler` |
| `cmd/sciontool/commands/init.go` | Pass `MeterProvider` to `NewTelemetryHandler` |

**Dependencies Added:**
- `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc`
- `go.opentelemetry.io/otel/sdk/metric` (already indirect, now direct)
- `go.opentelemetry.io/otel/metric` (already indirect, now direct)
- `go.opentelemetry.io/proto/otlp/collector/metrics/v1` (from existing `proto/otlp` module)

### Milestone 3: Hub Reporting & Storage

**Goal:** Aggregate session data and persist it to the Hub for state management.

**Deliverables:**
- [ ] **Aggregation Engine**: In-memory accumulation of session stats in `sciontool` (token counts, tool usage).
- [ ] **Hub Protocol**: Extension of daemon heartbeat/status updates to carry metrics payloads.
- [ ] **Hub Database**: Schema migration for the `agent_session_metrics` table.
- [ ] **Hub Ingestion**: Logic in Hub to receive metrics payloads and write to DB.

**Test Criteria:**
- Upon agent session completion, a row is created in `agent_session_metrics`.
- Token counts and tool usage statistics in the DB match the actual session activity.

### Milestone 4: Hub API & Web UI

**Goal:** Expose and visualize metrics in the user interface.

**Deliverables:**
- [ ] **Hub API**: Endpoints for retrieving session (`GET /metrics/session/{id}`) and agent summaries.
- [ ] **Web UI Component**: Session detail view showing token usage and cost estimates.
- [ ] **Web UI Dashboard**: Agent list view showing aggregate activity stats.

**Test Criteria:**
- Web UI "Session" tab displays correct token usage for a completed session.
- Agent list displays accurate "Total Tokens" or "Last Active" metrics.

---

## 13. QA Readiness Gaps

This section tracks the remaining implementation gaps that must be resolved before
a full end-to-end QA test of the telemetry system can be executed. Last reviewed
2026-02-19.

### 13.1 Settings → Container Environment Bridge

**Status:** Complete
**Implemented:** 2026-02-19

Two conversion functions in `pkg/config/telemetry_convert.go` bridge the gap:

- `ConvertV1TelemetryToAPI()` — converts settings-level `V1TelemetryConfig` to
  `api.TelemetryConfig` with nil-safe field-by-field copy.
- `TelemetryConfigToEnv()` — converts a resolved `api.TelemetryConfig` into a
  `map[string]string` of `SCION_TELEMETRY_*` / `SCION_OTEL_*` env vars, emitting
  only non-nil/non-zero fields.

Integration points:

- `pkg/agent/provision.go` — settings telemetry is set on `settingsCfg.Telemetry`
  before `MergeScionConfig(settingsCfg, finalScionCfg)`, so template/agent-level
  telemetry fields correctly override settings-level values via the existing
  `mergeTelemetryConfig` logic.
- `pkg/agent/run.go` — after hub endpoint injection and before `buildAgentEnv()`,
  `TelemetryConfigToEnv()` is called and each resulting env var is added to
  `opts.Env` only if not already present, preserving explicit Hub/broker overrides.

Priority chain (lowest → highest): `scionCfg.Env` (template raw env) →
telemetry config vars → explicit `opts.Env` (Hub/broker/CLI).

Tests in `pkg/config/telemetry_convert_test.go` (9 cases) and
`pkg/agent/run_test.go` (2 cases) cover nil handling, full conversion, partial
structs, bool/CSV/JSON formatting, injection, and override-preservation.

### Cloud Monitoring normalized hook counter time policy

In GCP-native forwarding, the exact hook scope and six monotonic delta Int64
counters (`agent.tool.calls`, `agent.session.count`, `gen_ai.api.calls`, and
`scion.hook.tokens.input/output/cached`) form collector-observed cumulative
streams. Their output epoch is set at the first admitted addition; each new
immutable snapshot uses the actual collector observation time as its end.
Snapshots wait at least two milliseconds after their epoch so the Monitoring
SDK does not extend a short hook interval into a future mapped timestamp.
Source intervals remain separate for validation and bounded duplicate
suppression. Generic OTLP output, lifecycle-scope counters, and native metric
timestamps keep their source times. The scope/name selector is caller supplied
and conveys semantics, not authenticated origin.

The GCP intake rejects an entire request before ACK when a changed native
output point is known to end less than five seconds after the possibly written
point for the same full Cloud series. Exactly five seconds is allowed. The
comparison uses the pinned Monitoring SDK's mapped end: non-gauge intervals
shorter than two milliseconds map to start plus one millisecond. A paired hook
request containing a duration histogram can therefore be rejected as a
whole even when its normalized counter would otherwise be eligible. Watermarks
include pending and ambiguous writes, remain bounded by active series state,
and do not survive restart or idle eviction. External writers are outside this
local knowledge. One global immutable pending batch retains whole-admission
ownership; hook clock rollback holds the entire dirty batch before snapshot
transfer, bounded by original admission age and Stop deadline. Partial resource
group success remains terminal Unconfirmed and reports only bounded group
counts and fixed error classes, without raw backend text or labels.

### 13.2 Hub Metrics Reporting (Milestone 3)

**Status:** Not started
**Blocks:** QA of metrics persistence and Hub-side visibility

The sciontool `TelemetryHandler` records OTel metrics on hook events, but these
metrics are only forwarded to the cloud OTLP backend. There is no path for
reporting session-level metric summaries to the Scion Hub.

**Current state:**
- `StatusUpdate` struct (`pkg/sciontool/hub/client.go`) has status and session
  fields but no metrics payload.
- Heartbeat loop sends status only; no aggregated metrics.
- No Hub API endpoint to receive metrics.
- No database table for `agent_session_metrics`.

**Required work:**
- [ ] **Aggregation engine**: In-memory accumulation of session stats (token
  counts, tool usage) in sciontool, derived from `TelemetryHandler` counters or
  session file parsing.
- [ ] **Hub protocol**: Extend `StatusUpdate` (or define a new `MetricsPayload`)
  to carry session metrics. Send on `session-end` or as part of heartbeat.
- [ ] **Hub database**: Schema migration adding `agent_session_metrics` table with
  columns for token counts, tool stats, model, duration, and timestamps.
- [ ] **Hub ingestion**: API handler to receive metrics payloads, validate, and
  persist to the database.

### 13.3 Hub Metrics API & Web UI (Milestone 4)

**Status:** Not started
**Blocks:** QA of metrics visualization

No API endpoints or UI components exist for retrieving or displaying telemetry
data stored in the Hub.

**Required work:**
- [ ] **Hub API**: `GET /api/v1/agents/{id}/metrics/summary` and
  `GET /api/v1/metrics/session/{id}` endpoints.
- [ ] **Web UI**: Session detail view showing token usage; agent list view showing
  aggregate activity stats.

### 13.4 QA Test Matrix

The following matrix maps test scenarios to their blocking gaps:

| Test Scenario | Status | Blocker |
|:---|:---|:---|
| Sciontool pipeline: receive → filter → export to cloud | Ready | — |
| Hook events produce correct spans and metrics | Ready | — |
| Privacy filtering (redact/hash/exclude) | Ready | — |
| Correlated logs emitted with trace context | Ready | — |
| ~~Gemini session file parsing on session-end~~ | Removed | — |
| Settings.yaml telemetry merges across scopes (unit) | Ready | — |
| Settings.yaml telemetry flows into agent container | Ready | — |
| Telemetry disabled at grove scope disables agent collection | Ready | — |
| Session metrics reported to Hub on session-end | Blocked | §13.2 |
| Hub stores and returns metrics via API | Blocked | §13.2, §13.3 |
| Web UI displays token usage for a session | Blocked | §13.2, §13.3 |
