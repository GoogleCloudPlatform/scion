---
title: Observability
description: Monitoring agents with logs and metrics.
---

Scion provides comprehensive observability for agent containers and system components through the `sciontool` telemetry pipeline and OpenTelemetry log bridging. This guide covers how to monitor agent activity, collect logs, and integrate with cloud-native observability platforms like Google Cloud Logging and Trace.

## Architecture Overview

Scion's observability architecture follows a "forwarder" pattern where `sciontool` acts as a local collector inside each agent container, and system components (Hub and Broker) bridge their logs directly to a central backend.

```
┌─────────────────────────────────────────┐
│           Agent Container               │
│                                         │
│  ┌─────────────┐                       │
│  │   Agent     │ OTLP (localhost:4317) │
│  │  (Claude/   │───────┐               │
│  │   Gemini)   │       │               │
│  └─────────────┘       │               │
│                        ▼               │
│              ┌─────────────────┐       │
│              │   sciontool     │       │
│              │   forwarder     │       │
│              └────────┬────────┘       │
│                       │                │
│                       │ OTLP (Cloud)   │
└───────────────────────┼────────────────┘
                        │
                        ▼
              ┌─────────────────┐
              │  Cloud Backend  │
              │ (Logging/Trace) │
              └─────────────────┘
                        ▲
                        │ OTLP (Cloud)
              ┌─────────┴─────────┐
              │    System Logs    │
              │  (Hub & Broker)   │
              └───────────────────┘
```

## Administrator Setup: Cloud Logging

To centralize logs and traces from all Scion components in Google Cloud, you must configure the OTLP endpoints and project identifiers.

### Connecting Hub and Broker Logs

The Scion Hub and Runtime Broker use structured logging (`slog`) with an OpenTelemetry bridge. To enable log forwarding to Google Cloud:

1.  **Configure Environment Variables**: Set the following on your Hub and Broker server processes:

    ```bash
    # Enable OTel log forwarding
    export SCION_OTEL_LOG_ENABLED=true

    # Set the GCP OTLP endpoint (standard for Cloud Trace/Logging)
    export SCION_OTEL_ENDPOINT="monitoring.googleapis.com:443"

    # Specify your GCP Project ID
    export SCION_GCP_PROJECT_ID="your-project-id"
    ```

2.  **Authentication**: Ensure the service account running the Hub/Broker has the following IAM roles:
    - `roles/logging.logWriter`
    - `roles/cloudtrace.agent`
    - `roles/monitoring.metricWriter`

### Direct Cloud Logging (Alternative)

As an alternative to the OTel pipeline, the Hub and Broker can send logs directly to Google Cloud Logging using the `cloud.google.com/go/logging` client library. This is simpler to set up when you only need log forwarding without traces or metrics:

```bash
# Enable direct Cloud Logging
export SCION_CLOUD_LOGGING=true
export SCION_GCP_PROJECT_ID="your-project-id"

# Optional: customize the log name (default: "scion")
export SCION_CLOUD_LOGGING_LOG_ID="scion-hub"

scion server start --enable-hub
```

Both approaches can be used simultaneously — OTel for the full telemetry pipeline and Cloud Logging for direct log delivery.

### Configuring Agent Telemetry

Agents use `sciontool` as their init process, which includes an embedded OTLP forwarder. This forwarder must be configured to point to your cloud backend.

#### Via Settings File (Recommended)

The preferred approach is to configure telemetry in `settings.yaml`. Settings at the global level apply to all agents; project-level settings apply to a specific project. Templates and individual agents can further override these via their `scion-agent.yaml`.

```yaml
# In ~/.scion/settings.yaml (global) or .scion/settings.yaml (project)
telemetry:
  enabled: true
  cloud:
    enabled: true
    endpoint: "monitoring.googleapis.com:443"
    protocol: grpc
  filter:
    events:
      exclude:
        - "agent.user.prompt"
```

See the [Orchestrator Settings Reference](/scion/reference/orchestrator-settings/#telemetry-configuration-telemetry) for the full field reference, and [Metrics & OpenTelemetry](/scion/hub-admin/metrics/#configuration-hierarchy) for how settings merge across scopes.

#### Via Hub Environment Variables

For hosted deployments, environment variables can be set at the Project or Broker level on the Hub. These are automatically injected into every agent container.

```bash
SCION_OTEL_ENDPOINT="monitoring.googleapis.com:443"
SCION_GCP_PROJECT_ID="your-project-id"
SCION_TELEMETRY_ENABLED="true"
```

#### Harness-Specific Configuration

If you are using agents that natively support OpenTelemetry (like `opencode`), you may need to explicitly tell the agent where to find the `sciontool` forwarder (which is `localhost` from the agent's perspective):

- **gRPC (Default)**: `OTEL_EXPORTER_OTLP_ENDPOINT="localhost:4317"`
- **HTTP**: `OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4318"`

These harness-specific env vars are injected at agent start time via the harness config's `env` map and are separate from the Scion telemetry settings.

## Agent Logs

Agent logs are written to `/home/scion/agent.log` inside the container. The sciontool logging system writes to both stderr and this file.

### Cloud Log Viewer & Hub API

Scion provides a built-in Cloud Log Viewer in the Web UI to stream agent logs in real-time. This is backed by the Hub API, which retrieves logs directly from the active runtime broker or from the persisted `agent.log` file, ensuring comprehensive visibility into agent execution regardless of its current state.

### Log Ownership and Permissions

The `sciontool` utility ensures that `agent.log` is owned by the `scion` user during initialization, even if `sciontool` is initially run as root. The log file is created with permissive `0666` permissions to ensure multiple processes can contribute to the log stream.

### Log Levels

- **INFO**: Normal operational events
- **ERROR**: Critical failures
- **DEBUG**: Detailed information (enabled with `SCION_DEBUG=true` or `SCION_LOG_LEVEL=debug`)

## Telemetry Collection

The telemetry pipeline in sciontool collects and forwards OpenTelemetry (OTLP) data from agents. See the [Metrics & OpenTelemetry guide](/scion/hub-admin/metrics/) for deep configuration details.

### What's Collected

| Data Type | Source | Description |
|-----------|--------|-------------|
| Traces | Agent OTLP | Span data for tool calls, API requests |
| Metrics | sciontool | Counters and histograms for tokens, tools, and latency |
| Correlated Logs | sciontool | Log records linked to traces for every hook event |
| Hook Events | Harness hooks | Tool calls, prompts, model invocations converted to spans |
| Session Metrics | Gemini session files | Token counts, turn counts, tool statistics |

### Privacy Controls

By default, user prompts (`agent.user.prompt`) are excluded from telemetry to protect privacy. Additionally, sensitive attributes are automatically redacted or hashed.

- **Redacted**: `prompt`, `user.email`, `tool_output`, `tool_input`
- **Hashed**: `session_id`

## HTTP Request Logs

HTTP requests to Hub, Broker, and Web servers are logged as a dedicated structured stream, separate from application logs. Request logs use the `google.logging.type.HttpRequest` format and include project/agent IDs, a generated request ID, and trace context from incoming headers.

### Enabling Request Log Output

| Method | Configuration |
|--------|--------------|
| **File** | Set `SCION_SERVER_REQUEST_LOG_PATH=/path/to/requests.log` |
| **Cloud Logging** | Automatic when `SCION_CLOUD_LOGGING=true` — uses log name `scion_request_log` |
| **Stdout** | Default when running in background mode (suppressed in `--foreground` mode) |

### Trace Context Propagation

The middleware generates a UUID `request_id` for every request and captures trace headers (`X-Cloud-Trace-Context`, `traceparent`, `X-Trace-ID`). These IDs are automatically attached to all application logs emitted during the request via `logging.Logger(ctx)`, enabling end-to-end correlation between the request log entry and any downstream application log entries.

### Cloud Logging Queries

Request logs appear under a separate log name from application logs:

```
-- All HTTP request logs
logName="projects/YOUR_PROJECT/logs/scion_request_log"

-- Slow requests (latency > 1s)
logName="projects/YOUR_PROJECT/logs/scion_request_log"
httpRequest.latency > "1s"

-- Failed requests for a specific project
logName="projects/YOUR_PROJECT/logs/scion_request_log"
httpRequest.status >= 400
labels.project_id = "my-project"

-- Correlate a request with its application logs
logName="projects/YOUR_PROJECT/logs/scion" OR logName="projects/YOUR_PROJECT/logs/scion_request_log"
jsonPayload.request_id = "YOUR_REQUEST_ID"
```

See the [Local Development Logging guide](/scion/development/logging/#http-request-logging) for the full field reference and file output format.

## Querying Logs by Subsystem

Hub and Broker logs include a `subsystem` attribute that identifies the internal subsystem that produced each log entry. This is separate from the top-level `component` field (which reflects the server mode: `scion-hub`, `scion-broker`, or `scion-server`) and provides finer-grained filtering.

### Available Subsystems

| Subsystem | Description |
|-----------|-------------|
| `hub.agent-lifecycle` | Agent create, start, stop, delete, and state transitions |
| `hub.auth` | Authentication and authorization decisions |
| `hub.control-channel` | WebSocket lifecycle for broker connections |
| `hub.messages` | Message routing from `scion message` to brokers |
| `hub.notifications` | Event-driven notification dispatch and subscription matching |
| `hub.scheduler` | Background recurring and one-shot scheduled tasks |
| `hub.env-secrets` | Environment variable and secret management |
| `hub.templates` | Template CRUD, hydration, and bootstrap |
| `hub.workspace` | Git worktree sync operations |
| `hub.dispatcher` | HTTP agent dispatch to brokers |
| `broker.agent-lifecycle` | Container provisioning, environment resolution, template hydration |
| `broker.control-channel` | Broker-side WebSocket connection to the hub |
| `broker.messages` | Message injection into agent tmux sessions |
| `broker.heartbeat` | Periodic broker status reports to hub |
| `broker.env-secrets` | Broker-side environment gathering and finalization |

In combo server mode (`scion-server`), both `hub.*` and `broker.*` subsystem logs appear in the same stream. The dotted prefix distinguishes them without requiring separate processes.

### Cloud Logging Query Examples

All examples assume your logs are in the `scion` log name. Adjust the `logName` filter to match your configuration.

#### Filter by Server Component

```
-- All hub logs (hub-only or combo mode)
logName="projects/YOUR_PROJECT/logs/scion"
labels.component="scion-hub"

-- All logs from combo server mode
logName="projects/YOUR_PROJECT/logs/scion"
labels.component="scion-server"
```

#### Filter by Subsystem

```
-- All hub subsystem logs
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "^hub\."

-- All broker subsystem logs
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "^broker\."

-- A specific subsystem
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem = "hub.notifications"
```

#### Agent Lifecycle Debugging

```
-- All agent lifecycle events across hub and broker
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.agent-lifecycle$"

-- Agent lifecycle for a specific agent
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.agent-lifecycle$"
jsonPayload.agent_id = "my-agent-id"

-- Only errors in agent lifecycle
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.agent-lifecycle$"
severity >= ERROR
```

#### Message Tracing

```
-- All message-related logs (hub routing + broker injection)
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.messages$"

-- Messages from a specific sender
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.messages$"
jsonPayload.sender = "agent-slug"

-- Messages to a specific recipient in a project
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.messages$"
jsonPayload.recipient = "target-agent"
jsonPayload.project_id = "my-project-id"
```

#### Auth and Security Auditing

```
-- All authentication and authorization events
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem = "hub.auth"

-- Auth failures only
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem = "hub.auth"
severity >= WARNING
```

#### Control Channel Monitoring

```
-- All control channel activity (hub + broker sides)
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.control-channel$"

-- Control channel errors (connectivity issues)
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "\.control-channel$"
severity >= ERROR
```

#### Operational Noise Reduction

```
-- All hub logs EXCEPT heartbeat noise
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "^hub\."
jsonPayload.subsystem != "broker.heartbeat"

-- Only high-priority subsystems (notifications, auth, agent lifecycle)
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem = "hub.notifications" OR
jsonPayload.subsystem = "hub.auth" OR
jsonPayload.subsystem =~ "\.agent-lifecycle$"
```

#### Combining with Time and Severity

```
-- Errors across all subsystems in the last hour
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem != ""
severity >= ERROR
timestamp >= "2026-03-03T00:00:00Z"

-- Debug-level broker logs for troubleshooting
logName="projects/YOUR_PROJECT/logs/scion"
jsonPayload.subsystem =~ "^broker\."
severity = DEBUG
```

:::tip
Create **saved queries** in the Cloud Logging console for subsystem filters you use frequently. For alerting, use log-based metrics with a filter like `jsonPayload.subsystem = "hub.notifications" AND severity >= ERROR` to trigger alerts on notification dispatch failures.
:::

## Structured Messaging Pipeline

Scion includes a comprehensive structured messaging pipeline that provides reliable delivery of messages to and from agents. This pipeline is fully observable:

- **Hub API Integration**: Messages can be sent and retrieved via the new Hub API, allowing external systems to programmatically interact with agents.
- **Web UI "Messages" Tab**: An interactive interface in the dashboard allows administrators and users to trace message flows in real-time.
- **Multi-Stage Broker Adapter**: Ensures robust delivery of messages to the agent containers, including external notifications. You can monitor message flow health in logs using the `hub.messages` and `broker.messages` subsystems.

## Stalled Agent Detection

The Hub includes an automated monitoring system to detect "zombie" or stalled agents. This system tracks the heartbeat signals emitted by runtime brokers.

- **Heartbeat Timeout**: If an agent stops responding and fails to emit a heartbeat within the configured `StalledThreshold`, it is automatically transitioned to an `offline` activity status.
- **Common Causes**: Currently, this may be due to an agent being unable to refresh its auth token, which disconnects it from sending its heartbeat and other updates. These agents can be stopped and restarted to be provisioned with a new auth token. They should be able to refresh this token as long as they can maintain a connection to the Hub.
- **Notifications**: Stalled events can trigger automated browser push notifications (by default, `stalled` and `error` states are included in the default notification triggers), proactively alerting administrators to health issues.
- **Visibility**: The Web UI clearly flags offline agents with specialized status badges, ensuring they are not lost among active workloads.

## Server Maintenance Logs

Server maintenance operations (like `rebuild-server`, `rebuild-web`, and `pull-images`) emit structured execution logs that are captured in real-time. These logs are accessible via the Web Dashboard's Maintenance Panel, providing immediate visibility into the progress of updates and synchronization tasks. In addition to real-time streaming, historical logs for completed maintenance tasks are archived and associated with their respective run records, allowing administrators to review past operations for duration, completion status, and potential failure points.

## Troubleshooting for Admins

### Logs Not Appearing in GCP

1.  **Verify Endpoints**: Ensure `SCION_OTEL_ENDPOINT` is set to `monitoring.googleapis.com:443`.
2.  **Check Credentials**: Outside of GKE/Cloud Run (where ADC is automatic), agents need a GCP service account key file. Verify the `scion-telemetry-gcp-credentials` secret is registered with target `~/.scion/telemetry-gcp-credentials.json`. Inside the agent, check `echo $SCION_OTEL_GCP_CREDENTIALS` — it should point to the file. See [GCP Credentials for Agent Containers](/scion/hub-admin/metrics/#4-gcp-credentials-for-agent-containers-non-adc-environments) for setup.
3.  **Check Permissions**: Verify the Workload Identity or Service Account has `roles/logging.logWriter`.
4.  **Inspect Agent Init**: View the agent container logs (stderr) to see if `sciontool` reported a telemetry startup failure:
    ```
    [sciontool] ERROR: Failed to start telemetry: connection refused
    ```
5.  **Network Policy**: If running in Kubernetes, ensure Egress is allowed to GCP APIs.

### Missing Trace Correlation

If you see logs but they aren't linked to traces in the Cloud Trace waterfall:
1.  Ensure the agent is using the `sciontool` gRPC port (4317).
2.  Verify `SCION_OTEL_LOG_ENABLED=true` is set on the system components.

## Related Guides

- [Metrics & OpenTelemetry](/scion/hub-admin/metrics/) - Detailed telemetry configuration
- [Hub Server](/scion/hub-admin/hub-server/) - Hub integration for hosted mode
- [Runtime Broker](/scion/hub-user/runtime-broker/) - Broker setup and configuration
