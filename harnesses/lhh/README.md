# LHH (Long-Horizon Harness) — Scion Harness Bundle

Scion harness bundle for the [Long-Horizon Harness (LHH)](https://github.com/google/adk-samples/tree/main/core/python/long-horizon-harness), a Google ADK-based agent framework with cross-session memory, session persistence, sub-agents, tool guardrails, a self-improvement loop, and context caching/compaction.

## Install

```bash
scion harness-config install harnesses/lhh
```

## Auth Requirements

LHH uses **Vertex AI exclusively** — API key mode is not supported.

### Prerequisites

1. A GCP project with the **Vertex AI API** enabled.
2. **Application Default Credentials (ADC):**
   ```bash
   gcloud auth application-default login
   ```
3. Environment variables (set in your Scion project or shell):
   - `GOOGLE_CLOUD_PROJECT` — your GCP project ID
   - `GOOGLE_CLOUD_REGION` (or `CLOUD_ML_REGION` / `GOOGLE_CLOUD_LOCATION`) — the GCP region (e.g. `us-central1`)

### Create an Agent

```bash
scion create --harness lhh --name my-lhh-agent
```

## Build Instructions

Build the container image locally:

```bash
docker build \
  --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-lhh:latest \
  -f harnesses/lhh/Dockerfile \
  harnesses/lhh/
```

To pin a specific LHH version:

```bash
docker build \
  --build-arg BASE_IMAGE=scion-base:latest \
  --build-arg LHH_VERSION=v1.0.0 \
  -t scion-lhh:latest \
  -f harnesses/lhh/Dockerfile \
  harnesses/lhh/
```

## Architecture

The harness runs two processes inside the container:

| Component | Process | Port |
|---|---|---|
| REPL | Foreground in tmux | — |
| FastAPI + Web UI | Background | 8080 |

The REPL is the primary Scion interface. Scion delivers messages via tmux `send-keys` into an `input()` loop. The web UI is a secondary view, accessible through Scion's `auto_expose_ports` proxy on port 8080.

Both processes share a SQLite session database, so sessions created in the REPL are visible in the web UI.

## Known Limitations

- **Session sharing between REPL and web UI:** Both processes create independent `Runner` instances. SQLite sessions are shared, but in-memory services (memory bank without Agent Engine) are per-process. The REPL is the primary interface; the web UI is a secondary view.
- **No MCP support:** LHH uses its own ADK tool system, not MCP.
- **No telemetry reconciliation:** LHH has its own OTEL setup; Scion telemetry is not wired in this iteration.
- **No API key auth:** LHH requires Vertex AI credentials (ADC or GCP service account).
