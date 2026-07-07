---
title: Local Development Logging
description: Setting up structured logging for Hub and Broker development.
---

When developing the Hub or Runtime Broker locally, you often need to test the logging pipeline and verify log output before deploying to GCP. This guide covers how to configure structured logging for local development, including GCP Cloud Logging format testing.

## Quick Start

### Enable GCP-Format Logging Locally

Set the `SCION_LOG_GCP` environment variable to output logs in GCP Cloud Logging format:

```bash
# Run hub with GCP-formatted logs
SCION_LOG_GCP=true scion server start --enable-hub

# Run broker with GCP-formatted logs
SCION_LOG_GCP=true scion server start --enable-runtime-broker

# Run both with debug logging enabled
SCION_LOG_GCP=true SCION_LOG_LEVEL=debug scion server start --enable-hub --enable-runtime-broker
```

### Standard JSON Output

Without `SCION_LOG_GCP`, logs use standard JSON format:

```json
{
  "time": "2025-02-09T12:34:56Z",
  "level": "INFO",
  "msg": "Server started",
  "component": "scion-hub",
  "port": 9810
}
```

### GCP Cloud Logging Output

With `SCION_LOG_GCP=true`, logs use GCP's expected format:

```json
{
  "severity": "INFO",
  "message": "Server started",
  "timestamp": "2025-02-09T12:34:56Z",
  "logging.googleapis.com/labels": {
    "component": "scion-hub",
    "hostname": "dev-machine"
  },
  "logging.googleapis.com/sourceLocation": {
    "file": "/path/to/server.go",
    "line": "172",
    "function": "cmd.runServerStart"
  },
  "port": 9810
}
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_LOG_GCP` | Enable GCP Cloud Logging JSON format on stdout | `false` |
| `SCION_LOG_LEVEL` | Log level (`debug`, `info`, `warn`, `error`) | `info` |
| `K_SERVICE` | Auto-enables GCP logging format (set by Cloud Run) | - |
| `SCION_CLOUD_LOGGING` | Send logs directly to Cloud Logging via client library | `false` |
| `SCION_CLOUD_LOGGING_LOG_ID` | Log name in Cloud Logging | `scion` |
| `SCION_GCP_PROJECT_ID` | GCP project ID (priority 1 for Cloud Logging) | auto-detect |
| `GOOGLE_CLOUD_PROJECT` | GCP project ID (priority 2 for Cloud Logging) | - |
| `SCION_SERVER_REQUEST_LOG_PATH` | Write HTTP request logs to a file at this path | (disabled) |

## Sending Logs to GCP from Local Machine

For development, you can pipe logs directly to GCP Cloud Logging using the `gcloud` CLI.

### Prerequisites

1. Install [Google Cloud SDK](https://cloud.google.com/sdk/docs/install)
2. Authenticate: `gcloud auth login`
3. Set your project: `gcloud config set project YOUR_PROJECT_ID`

### Pipe Logs to Cloud Logging

```bash
# Create a custom log stream for development
SCION_LOG_GCP=true scion server start --enable-hub 2>&1 | \
  while read line; do
    echo "$line" | gcloud logging write scion-dev-hub --payload-type=json
  done
```

### Using a Named Pipe (Background Server)

For long-running development sessions:

```bash
# Create a named pipe
mkfifo /tmp/scion-logs

# Start log forwarder in background
cat /tmp/scion-logs | while read line; do
  echo "$line" | gcloud logging write scion-dev-hub --payload-type=json
done &

# Run server with logs to pipe
SCION_LOG_GCP=true scion server start --enable-hub > /tmp/scion-logs 2>&1
```

### View Logs in GCP Console

Navigate to **Logging > Logs Explorer** in the GCP Console and filter by:

```
logName="projects/YOUR_PROJECT/logs/scion-dev-hub"
```

## Direct Cloud Logging

Instead of piping stdout to `gcloud` or running a full OTel pipeline, you can send logs directly to Google Cloud Logging using the built-in client library. This works for both Hub and Runtime Broker servers.

### Setup

1. Authenticate with Application Default Credentials:

    ```bash
    gcloud auth application-default login
    ```

2. Set the required environment variables and start the server:

    ```bash
    export SCION_CLOUD_LOGGING=true
    export SCION_GCP_PROJECT_ID="your-project-id"

    # Start Hub with direct Cloud Logging
    scion server start --enable-hub

    # Or start Broker
    scion server start --enable-runtime-broker

    # Or both
    scion server start --enable-hub --enable-runtime-broker
    ```

3. Optionally customize the log name (defaults to `scion`):

    ```bash
    export SCION_CLOUD_LOGGING_LOG_ID="scion-dev"
    ```

### How It Works

When `SCION_CLOUD_LOGGING=true`, the server creates a `cloud.google.com/go/logging` client that sends structured log entries to Cloud Logging as a background handler. Logs continue to appear on stdout as normal — Cloud Logging is additive, not a replacement.

The project ID is resolved in this order:
1. `SCION_GCP_PROJECT_ID` environment variable
2. `GOOGLE_CLOUD_PROJECT` environment variable
3. Auto-detected from the environment (e.g., metadata server on GCE/Cloud Run)

### Viewing Logs

Logs appear in **Logging > Logs Explorer** in the GCP Console. Filter by log name:

```
logName="projects/YOUR_PROJECT/logs/scion-server"
```

Or filter by component label:

```
logName="projects/YOUR_PROJECT/logs/scion-server"
labels.component="scion-hub"
```

### Combining with Other Log Backends

Direct Cloud Logging can be used alongside other logging backends:

```bash
# Cloud Logging + GCP-formatted stdout + debug level
SCION_CLOUD_LOGGING=true \
SCION_GCP_PROJECT_ID="your-project-id" \
SCION_LOG_GCP=true \
SCION_LOG_LEVEL=debug \
scion server start --enable-hub
```

All three backends (stdout, OTel, Cloud Logging) operate independently and can be enabled simultaneously.

## OpenTelemetry Export

For more advanced setups, you can export logs via OpenTelemetry to GCP:

```bash
# Enable OTel log bridging
export SCION_OTEL_LOG_ENABLED=true
export SCION_OTEL_ENDPOINT="monitoring.googleapis.com:443"
export GOOGLE_APPLICATION_CREDENTIALS="/path/to/service-account.json"

scion server start --enable-hub
```

See the [Observability guide](/scion/hub-admin/observability/) for full OTel configuration.

## Log Levels

Use `--debug` flag or `SCION_LOG_LEVEL=debug` for verbose output during development:

```bash
# Via flag
scion server start --enable-hub --debug

# Via environment
SCION_LOG_LEVEL=debug scion server start --enable-hub
```

Debug logging includes:
- Request/response details
- Internal state transitions
- Detailed error context

## Component Names

The log `component` field reflects the server mode:

| Mode | Component |
|------|-----------|
| Hub only | `scion-hub` |
| Broker only | `scion-broker` |
| Both | `scion-server` |

### Subsystem Attribute

In addition to `component`, each log entry includes a `subsystem` field that identifies the internal subsystem (e.g., `hub.notifications`, `broker.agent-lifecycle`). Use this for fine-grained filtering when debugging a specific area:

```bash
# Filter local GCP-formatted logs by subsystem with jq
SCION_LOG_GCP=true scion server start --enable-hub 2>&1 | \
  jq 'select(.subsystem == "hub.auth")'

# Show all subsystem values in the log stream
SCION_LOG_GCP=true scion server start --enable-hub --enable-runtime-broker 2>&1 | \
  jq -r '.subsystem // empty' | sort -u
```

See the [Observability guide](/scion/hub-admin/observability/#querying-logs-by-subsystem) for the full list of subsystems and Cloud Logging query examples.

## HTTP Request Logging

HTTP requests to the Hub, Runtime Broker, and Web server are logged as a **dedicated structured stream** using the [`google.logging.type.HttpRequest`](https://cloud.google.com/logging/docs/reference/v2/rest/v2/LogEntry#HttpRequest) format. This stream is separate from application logs, making it easy to filter, query, and alert on request traffic.

### Request Log Destinations

Request logs are routed based on the server's configuration:

| Condition | Destination |
|-----------|-------------|
| `SCION_SERVER_REQUEST_LOG_PATH` is set | JSON lines written to the specified file |
| `SCION_CLOUD_LOGGING=true` | Sent to Cloud Logging under log name `scion_request_log` (separate from application logs in `scion-server`) |
| Background / piped mode (no file, no cloud) | Written to stdout as JSON |
| `--foreground` mode (no file, no cloud) | **Suppressed** — request logs do not appear on stdout in foreground mode to reduce noise |

You can combine file and Cloud Logging output. When `--foreground` is set, file and Cloud Logging targets are still active — only stdout output is suppressed.

### File Output

To write request logs to a file:

```bash
SCION_SERVER_REQUEST_LOG_PATH=/var/log/scion/requests.log scion server start
```

Each line is a JSON object:

```json
{
  "time": "2026-03-07T12:00:00.000Z",
  "level": "INFO",
  "msg": "Request completed",
  "httpRequest": {
    "requestMethod": "GET",
    "requestUrl": "/api/v1/projects/my-project/agents",
    "status": 200,
    "responseSize": 1234,
    "userAgent": "scion-cli/0.1.0",
    "remoteIp": "192.168.1.1:54321",
    "latency": "0.045s",
    "protocol": "HTTP/1.1"
  },
  "component": "hub",
  "project_id": "my-project",
  "agent_id": "",
  "request_id": "550e8400-e29b-41d4-a716-446655440000"
}
```

### Request Log Fields

| Field | Description |
|-------|-------------|
| `httpRequest.requestMethod` | HTTP method (GET, POST, etc.) |
| `httpRequest.requestUrl` | Full request URL |
| `httpRequest.status` | HTTP status code |
| `httpRequest.responseSize` | Response body size in bytes |
| `httpRequest.userAgent` | Client User-Agent header |
| `httpRequest.remoteIp` | Client IP address and port |
| `httpRequest.latency` | Request duration as `"Xs"` (e.g. `"0.045s"`) |
| `httpRequest.protocol` | HTTP protocol version |
| `component` | Server component: `hub`, `broker`, or `web` |
| `project_id` | Project ID extracted from the URL path (if applicable) |
| `agent_id` | Agent ID extracted from the URL path (if applicable) |
| `request_id` | Generated UUID for correlating logs within a request |
| `trace_id` | Trace header value from `X-Cloud-Trace-Context`, `traceparent`, or `X-Trace-ID` (if present) |

### Trace Context Propagation

The request logging middleware generates a unique `request_id` (UUID) for every request. If the client sends a trace header (`X-Cloud-Trace-Context`, `traceparent`, or `X-Trace-ID`), it is also captured as `trace_id`.

Both `request_id` and `trace_id` are automatically attached to all application logs emitted during the request when using `logging.Logger(ctx)`:

```go
// In any handler, Logger(ctx) automatically includes request_id, trace_id, project_id, agent_id
log := logging.Logger(r.Context())
log.Info("Processing agent", "name", agentName)
// Output includes: request_id=..., trace_id=..., project_id=..., agent_id=...
```

### Cloud Logging Queries

When Cloud Logging is enabled, request logs appear under a separate log name (`scion_request_log`) from application logs (`scion-server`). This allows independent filtering:

```
-- All HTTP request logs
logName="projects/YOUR_PROJECT/logs/scion_request_log"

-- Slow requests (latency > 1s)
logName="projects/YOUR_PROJECT/logs/scion_request_log"
httpRequest.latency > "1s"

-- Failed requests to a specific project
logName="projects/YOUR_PROJECT/logs/scion_request_log"
httpRequest.status >= 400
labels.project_id = "my-project"

-- Correlate a request with its application logs
logName="projects/YOUR_PROJECT/logs/scion-server" OR logName="projects/YOUR_PROJECT/logs/scion_request_log"
jsonPayload.request_id = "550e8400-e29b-41d4-a716-446655440000"
```

### Analyzing Request Logs with jq

```bash
# Pretty-print request logs from file
cat /var/log/scion/requests.log | jq .

# Show only failed requests
cat /var/log/scion/requests.log | jq 'select(.httpRequest.status >= 400)'

# Top endpoints by request count
cat /var/log/scion/requests.log | jq -r '.httpRequest.requestUrl' | sort | uniq -c | sort -rn | head

# Average latency per endpoint
cat /var/log/scion/requests.log | jq -r '[.httpRequest.requestUrl, .httpRequest.latency] | @tsv'
```

## Testing Log Output

To verify your logging configuration without sending to GCP:

```bash
# Pretty-print GCP-formatted logs with jq
SCION_LOG_GCP=true scion server start --enable-hub 2>&1 | jq .

# Filter for specific severity
SCION_LOG_GCP=true scion server start --enable-hub 2>&1 | \
  jq 'select(.severity == "ERROR")'

# Extract just messages
SCION_LOG_GCP=true scion server start --enable-hub 2>&1 | \
  jq -r '.message'
```

## PTY Escape Sequence Debugging

When troubleshooting complex terminal interactions (such as shifted keys or mouse reporting in the web UI), Scion includes built-in PTY logging capabilities.

To capture raw escape sequences sent and received by the agent's PTY pipeline:

```bash
# Start the broker with debug logging enabled
SCION_LOG_LEVEL=debug scion server start --enable-runtime-broker
```

In debug mode, the broker logs detailed hexadecimal representations of all input and output sequences. This is essential for:
- Verifying `CSI u` sequences for extended keys (e.g., `Shift+Enter`).
- Debugging tmux mouse selection payloads (`SGR` format coordinates).
- Analyzing window resize events and terminal redrawing.

## Related Guides

- [Observability](/scion/hub-admin/observability/) - Full telemetry pipeline setup
- [Metrics](/scion/hub-admin/metrics/) - OpenTelemetry metrics configuration
- [Hub Server](/scion/hub-admin/hub-server/) - Hub deployment and configuration
- [Runtime Broker](/scion/hub-user/runtime-broker/) - Broker setup
