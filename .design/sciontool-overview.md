# Design: ScionTool Architecture

## 1. Executive Summary

`sciontool` is a unified Go binary designed to run *inside* Scion agent containers. It serves as the container's specialized init process (PID 1), lifecycle manager, and telemetry forwarder.

Currently, agent containers rely on a mix of shell scripts, Python hooks, and direct entrypoints. `sciontool` consolidates these responsibilities into a single, robust, and testable binary that ensures consistent behavior across different runtimes (Docker, Kubernetes, Apple).

**Key Benefits:**
- Single source of truth for container initialization logic
- Proper zombie process reaping (critical for PID 1)
- Standardized lifecycle hooks across agent types (Gemini, Claude)
- Built-in observability via OTel forwarding
- Testable Go code replacing fragile shell scripts

## 2. Architecture & Components

`sciontool` is built as a modular CLI application using Cobra, with each major capability isolated in its own package.

```mermaid
graph TD
    Entry[Container Entrypoint] --> ScionTool[sciontool init]

    subgraph "ScionTool (PID 1)"
        Reaper[Process Reaper]
        Setup[Setup Logic]
        Supervisor[Process Supervisor]

        Reaper --> Supervisor
        Setup --> Supervisor
    end

    Supervisor --> Agent[Agent Process (Gemini/Claude)]
    Supervisor --> OTel[OTel Collector]
    Supervisor --> TTY[WebTTY Server]

    Agent -->|Hooks| ScionToolHooks[sciontool hook]
    Agent -->|Logs/Traces| OTel

    OTel -->|Forward| External[Hub / Remote OTel]
```

### 2.1. Project Structure

```
cmd/sciontool/
├── main.go           # Entry point
├── root.go           # Root Cobra command
├── version.go        # Version command
├── init.go           # Init/PID 1 command
├── hook.go           # Hook command (harness hooks entry point)
├── daemon.go         # Hub daemon command (hosted mode)
└── otel.go           # OTel subcommands (future)

pkg/sciontool/
├── supervisor/       # Process management & signal handling
├── hooks/            # Hook system
│   ├── lifecycle.go  # Scion lifecycle hooks (pre-start, post-start, session-end)
│   ├── harness.go    # Harness hook dispatcher
│   ├── handlers/     # Shared hook handler implementations
│   └── dialects/     # Harness-specific event parsers
│       ├── claude.go # Claude Code event format
│       └── gemini.go # Gemini CLI event format
├── hub/              # Hub communication (hosted mode)
│   ├── client.go     # Hub API client
│   ├── heartbeat.go  # Liveness reporting loop
│   └── status.go     # Agent status management
├── telemetry/        # OTel collector/forwarder
└── setup/            # Container setup tasks
```

### 2.2. Core Capabilities

#### A. Container Entrypoint (PID 1)
- **Library:** `github.com/ramr/go-reaper`
- **Role:** Acts as the init process. Handles signal propagation (SIGTERM/SIGINT) to child processes and reaps zombie processes to prevent resource leaks.
- **Command:** `sciontool init [--] <command> [args...]`
- **Behavior:**
  1. Initialize the reaper goroutine
  2. Run setup tasks (permissions, mounts)
  3. Spawn the child command (e.g., `gemini`, `tmux`)
  4. Forward signals to child processes
  5. Wait for child exit and propagate exit code

#### B. Setup & Provisioning
- **Role:** Executed before the main agent process starts.
- **Tasks:**
  - Mount FUSE filesystems (gcsfuse for GCS access)
  - Fix permissions on `/workspace` or home directories
  - Inject dynamic configuration files
  - Validate environment requirements

#### C. Hook System

The hook system has two distinct layers:

**C.1. Scion Lifecycle Hooks**
- **Role:** Container-level lifecycle events managed directly by `sciontool init`.
- **Trigger:** Invoked automatically by the supervisor during agent lifecycle transitions.
- **Events:**
  - `pre-start` — Before agent process begins (after setup, before spawn)
  - `post-start` — After agent process is confirmed running
  - `session-end` — On graceful shutdown (before child termination)
- **Use Cases:** Workspace validation, environment checks, cleanup tasks, metrics flush.

**C.2. Harness Hooks**
- **Role:** Replaces existing `scion_hook.py` script. Receives events from agent harnesses (Claude Code, Gemini CLI) during their operation.
- **Command:** `sciontool hook <event> [--dialect=claude|gemini] [--data=<json>]`
- **Trigger:** Called directly by the harness configuration (e.g., Claude Code hooks, Gemini CLI callbacks).
- **Events:**
  - `tool-start` — Before a tool/command execution
  - `tool-end` — After a tool/command execution (includes result status)
  - `prompt-submit` — User prompt submitted to agent
  - `response-complete` — Agent response finished
- **Dialects:** Each harness sends events in its native format. The `--dialect` flag tells sciontool how to parse and normalize the incoming data into Scion-standard events.

```
┌─────────────────────────────────────────────────────────────┐
│                     sciontool init (PID 1)                  │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ Scion Lifecycle Hooks                               │    │
│  │   pre-start → post-start → ... → session-end       │    │
│  └─────────────────────────────────────────────────────┘    │
│                           │                                 │
│                           ▼                                 │
│  ┌─────────────────────────────────────────────────────┐    │
│  │ Agent Process (Claude Code / Gemini CLI)            │    │
│  │                                                     │    │
│  │   On tool use: exec `sciontool hook tool-start`    │    │
│  │   On complete: exec `sciontool hook tool-end`      │    │
│  └─────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────┘
```

#### D. Telemetry & Observability
- **Role:** In-container OTel collector and forwarder.
- **Endpoint:** `localhost:4317` (OTLP gRPC)
- **Function:**
  - Receives OTLP data from the agent process
  - Filters and sanitizes data (removing PII if configured)
  - Batches and forwards metrics/traces to configured backend
- **Backends:** Scion Hub, Google Cloud Trace, or external OTel collector

#### E. Connectivity & Access
- **WebTTY:** Browser-based terminal access via `tsl0922/ttyd` (orchestrated subprocess)
- **Reverse Tunnel:** Secure tunnels for remote management in hosted scenarios

#### F. Hub Daemon (Hosted Mode)
- **Role:** Background daemon process for hub communication in hosted mode.
- **Trigger:** When `SCION_AGENT_MODE=hosted` and `SCION_HUB_ENDPOINT` is set, `sciontool init` spawns itself as a daemon subprocess (`sciontool daemon`).
- **Authentication:** Authenticates with the Hub using a short-lived, Hub-issued JWT provided via the `SCION_HUB_TOKEN` environment variable. See [Agent Authentication](hosted/auth/sciontool-auth.md) for details.
- **Capabilities:**
  - **Heartbeat:** Periodic liveness reporting to the hub (configurable interval, default 30s)
  - **Status Sync:** Reports agent state (idle, busy, error) to the hub
  - **Command Receiver:** Listens for control commands from hub (future: pause, resume, terminate)
- **Command:** `sciontool daemon --hub=<endpoint> --agent-id=<id>`
- **Lifecycle:** Spawned after the agent process starts; terminated during session-end hook.

```
┌─────────────────────────────────────────────────────────────┐
│                   sciontool init (PID 1)                    │
│                                                             │
│   if SCION_AGENT_MODE=hosted && SCION_HUB_ENDPOINT set:    │
│                                                             │
│   ┌──────────────────┐      ┌──────────────────────────┐   │
│   │ Agent Process    │      │ sciontool daemon         │   │
│   │ (Claude/Gemini)  │      │   • heartbeat → hub      │   │
│   │                  │      │   • status sync          │   │
│   └──────────────────┘      └──────────────────────────┘   │
│                                       │                     │
│                                       ▼                     │
│                              ┌─────────────────┐            │
│                              │   Scion Hub     │            │
│                              └─────────────────┘            │
└─────────────────────────────────────────────────────────────┘
```

## 3. Technical Considerations

### 3.1. Build Infrastructure
- **Build Context:** The `cloudbuild.yaml` must use the repo root (`.`) as the build context to access `cmd/sciontool`. The Dockerfile path will be specified explicitly.
- **CGO Requirement:** The `go-reaper` library requires CGO (for `prctl` syscall). The `scion-base` builder image includes `gcc`, so this is supported.
- **Binary Size:** Use `go build -ldflags="-s -w"` to strip debug symbols (~30% size reduction).

### 3.2. Entrypoint & Command Flexibility

**Container Image Defaults:**
- **ENTRYPOINT:** `["sciontool", "init", "--"]` (fixed, ensures sciontool is always PID 1)
- **CMD:** `["gemini"]` (default command, can be overridden)

**Command Override Flow:**
The `scion-agent.json` config or agent template may specify a custom command and arguments. These are passed to the container at runtime and forwarded by sciontool to the child process.

```
Dockerfile:
  ENTRYPOINT ["sciontool", "init", "--"]
  CMD ["gemini"]

Runtime override (from scion-agent.json):
  docker run <image> tmux new-session -A -s main

Effective execution:
  sciontool init -- tmux new-session -A -s main
                    └─────────────────────────┘
                      Passed as child command
```

**Supported Patterns:**
| Source | Command | Result |
|--------|---------|--------|
| Default (no override) | — | `sciontool init -- gemini` |
| Agent config command | `["claude"]` | `sciontool init -- claude` |
| Agent config with args | `["tmux", "new-session", "-A"]` | `sciontool init -- tmux new-session -A` |
| Template with env expansion | `["${AGENT_CMD}"]` | Resolved at container start |

**Implementation Notes:**
- The `--` separator is critical: everything after it is treated as the child command
- `sciontool init` must handle the case where no command is provided (use a sensible default or error)
- Environment variable expansion in command args is handled by the container runtime, not sciontool

### 3.3. Signal Handling
When managing `tmux` or similar session managers:
- SIGTERM → Graceful shutdown (send to child, wait with timeout)
- SIGINT → Immediate forward to child
- SIGHUP → Reload configuration (future)
- Use a configurable grace period (default: 10s) before SIGKILL

## 4. Implementation Phases

### Phase 1: Build Infrastructure & Skeleton
**Goal:** Establish the binary structure and integrate with the build pipeline.

| Action | Files | Details |
|--------|-------|---------|
| Create CLI skeleton | `cmd/sciontool/main.go`, `root.go`, `version.go` | Initialize Cobra CLI with `version` command |
| Update build context | `image-build/cloudbuild.yaml` | Change `dir: .` and specify Dockerfile path |
| Update Dockerfile | `image-build/scion-base/Dockerfile` | Add multi-stage Go build for sciontool |
| Update .dockerignore | `.dockerignore` | Ensure `.git` and large dirs are excluded |

### Phase 2: Init Process (PID 1) & Supervisor
**Goal:** Take over the container entrypoint with proper process management.

| Action | Files | Details |
|--------|-------|---------|
| Implement `init` command | `cmd/sciontool/init.go` | Integrate `go-reaper`, spawn child, handle signals |
| Add supervisor package | `pkg/sciontool/supervisor/` | Process lifecycle management |
| Update entrypoint | `image-build/scion-base/Dockerfile` | Set `ENTRYPOINT ["sciontool", "init", "--"]` |

### Phase 3: Hook System
**Goal:** Implement both Scion lifecycle hooks and harness hook processing.

| Action | Files | Details |
|--------|-------|---------|
| Add lifecycle hooks to init | `cmd/sciontool/init.go` | Call pre-start, post-start, session-end at appropriate points |
| Implement `hook` command | `cmd/sciontool/hook.go` | CLI entry point for harness hooks with dialect parsing |
| Add hook handlers | `pkg/sciontool/hooks/` | Shared handlers for both hook types |
| Add dialect parsers | `pkg/sciontool/hooks/dialects/` | Claude and Gemini event format parsers |
| Replace scion_hook.py | Agent config files | Update harness configs to call `sciontool hook` instead of Python

### Phase 4: Telemetry (OTel)
**Goal:** Enable visibility into agent operations.

| Action | Files | Details |
|--------|-------|---------|
| Add OTel receiver | `pkg/sciontool/telemetry/receiver.go` | OTLP receiver on localhost:4317 |
| Add forwarder | `pkg/sciontool/telemetry/forwarder.go` | Batch and forward to backend |
| Integrate with init | `cmd/sciontool/init.go` | Start OTel as managed subprocess |

### Phase 5: Hub Daemon (Hosted Mode)
**Goal:** Enable hub communication for hosted deployments.

| Action | Files | Details |
|--------|-------|---------|
| Implement `daemon` command | `cmd/sciontool/daemon.go` | Long-running hub communication process |
| Add hub client | `pkg/sciontool/hub/client.go` | HTTP/gRPC client for hub API |
| Implement heartbeat | `pkg/sciontool/hub/heartbeat.go` | Periodic liveness reporting |
| Add status management | `pkg/sciontool/hub/status.go` | Track and report agent state |
| Integrate with init | `cmd/sciontool/init.go` | Spawn daemon when in hosted mode |

### Phase 6: Advanced Connectivity
**Goal:** Enable remote interaction patterns.

| Action | Files | Details |
|--------|-------|---------|
| WebTTY integration | `pkg/sciontool/tty/` | Manage ttyd subprocess |
| Reverse tunnel | `pkg/sciontool/tunnel/` | Secure tunnel to Scion Hub |

## 5. Configuration

`sciontool` is configured via environment variables and an optional JSON config file.

### Environment Variables
| Variable | Description | Default |
|----------|-------------|---------|
| `SCION_AGENT_MODE` | Operating mode: `solo` or `hosted` | `solo` |
| `SCION_HUB_ENDPOINT` | URL for the centralized hub | — |
| `SCION_AGENT_ID` | Unique agent identifier (required for hosted mode) | — |
| `SCION_HEARTBEAT_INTERVAL` | Hub heartbeat interval (hosted mode) | `30s` |
| `SCION_DEBUG` | Any non-empty value enables debug logging (`--log-level debug` does the same). `SCION_LOG_LEVEL` is not read by sciontool. | unset |
| `SCION_OTEL_ENDPOINT` | OTel backend endpoint | — |
| `SCION_GRACE_PERIOD` | Shutdown grace period | `10s` |

### Config File (Optional)
Location: `/etc/scion/config.json` or `$SCION_CONFIG_PATH`

```json
{
  "agent_mode": "hosted",
  "agent_id": "agent-abc123",
  "hub": {
    "endpoint": "https://hub.example.com",
    "heartbeat_interval": "30s"
  },
  "otel": {
    "endpoint": "otel-collector:4317",
    "insecure": false
  },
  "hooks": {
    "pre_start": ["validate-workspace"],
    "post_command": ["log-metrics"]
  }
}
```

## 6. Verification Strategy

### Build Verification
```bash
# From repo root
docker build -f image-build/scion-base/Dockerfile .
```

### PID 1 Verification
```bash
# Start container
docker run -it --rm scion-base:test

# Inside container
ps aux | head -5   # Verify sciontool is PID 1
```

### Signal Handling Test
```bash
# Terminal 1: Start container
docker run --name test-container scion-base:test

# Terminal 2: Send SIGTERM
docker stop test-container

# Verify graceful shutdown in logs
docker logs test-container | grep -i shutdown
```

### Zombie Reaping Test
```bash
# Inside container, create a zombie
( sleep 1 & ) && sleep 2 && ps aux | grep defunct
# Should show no defunct/zombie processes
```

## 7. Risks & Mitigation

| Risk | Impact | Mitigation |
|------|--------|------------|
| **Container bloat** | Increased image size | Multi-stage builds, strip debug symbols (`-ldflags="-s -w"`) |
| **CGO dependency** | Build complexity, cross-compilation issues | Builder image includes gcc; document build requirements |
| **Signal handling with tmux** | Incorrect shutdown behavior | Test signal propagation thoroughly; configurable grace period |
| **Build context size** | Slow builds if large dirs sent to daemon | Robust `.dockerignore` excluding `.git`, `node_modules`, etc. |
| **Subprocess complexity** | Race conditions, resource leaks | Use established patterns (suture library if needed); comprehensive tests |

## 8. Future Considerations

- **Health endpoints:** HTTP health/ready endpoints for Kubernetes probes
- **Metrics exposition:** Prometheus-format metrics at `/metrics`
- **Hot reload:** Reload configuration without restart
- **Plugin system:** Dynamic hook loading for custom integrations

---

## Appendix A: Shutdown Hook Performance Analysis

*Added: 2026-01-30*

### Observed Behavior

Container shutdown logs show delays around these messages:
```
[sciontool] Running pre-stop hooks...
[sciontool] Running session-end hooks...
```

### What the Hooks Actually Do

**Pre-stop hooks** (`pkg/sciontool/hooks/lifecycle.go:62-70`):
1. Looks for executable scripts in `/etc/scion/hooks/pre-stop{,.sh,.d/*}` (none installed by default)
2. Calls `StatusHandler.Handle()` — writes "shutting_down" to `~/agent-info.json`
3. Calls `LoggingHandler.Handle()` — appends to `~/agent.log`

**Session-end hooks** (`lifecycle.go:72-79`):
- Same pattern: script hooks + status update + log append

All registered handlers perform simple local file operations (no network calls).

### Shutdown Sequence

```
SIGTERM/SIGINT received
    │
    ▼
[sciontool] Running pre-stop hooks...
    │  └─ StatusHandler: write agent-info.json
    │  └─ LoggingHandler: append agent.log
    ▼
context.Cancel() called
    │
    ▼
Supervisor sends SIGTERM to child process
    │
    ▼
Child (Claude Code/Gemini) receives SIGTERM
    │  └─ Harness fires SessionEnd/Stop hooks
    │  └─ Each hook spawns: sciontool hook --dialect=claude
    │
    ▼
Child process exits
    │
    ▼
[sciontool] Running session-end hooks...
    │  └─ StatusHandler: write agent-info.json
    │  └─ LoggingHandler: append agent.log
    ▼
sciontool exits with child's exit code
```

### Likely Sources of Delay

1. **Child process shutdown time** — The gap between pre-stop and session-end includes waiting for the child process (Claude Code/Gemini) to exit. The harness may be doing its own cleanup.

2. **Harness hook spawning** — When the child receives SIGTERM, Claude Code fires `SessionEnd` and `Stop` hooks (configured in `pkg/config/embeds/claude/settings.json:21-24`), each spawning a new `sciontool hook` process.

3. **Process spawn overhead** — Each `sciontool hook` invocation starts a new Go process. Multiple hooks during shutdown compound this latency.

4. **Filesystem latency** — On network storage or slow overlay filesystems, the atomic file writes could be slower than expected.

### Potential Optimizations

- **Add timing instrumentation** to identify the exact bottleneck
- **Batch harness hooks** to reduce process spawning during shutdown
- **Async file writes** for non-critical status updates
- **Reduce harness hooks on shutdown** — consider removing `Stop`/`SubagentStop` hooks if `SessionEnd` is sufficient
