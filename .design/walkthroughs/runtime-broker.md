# Runtime Broker API Testing Walkthrough

This guide provides step-by-step instructions for testing the Runtime Broker API alongside the Hub API on a Mac with apple-container runtime.

## Prerequisites

- macOS with `container` CLI installed (Apple Virtualization Framework)
- Go 1.21+ installed
- `curl` or similar HTTP client
- `jq` for JSON formatting (optional but recommended)

## 1. Build and Start Both Servers

### Build the Binary

```bash
# From the project root
go build -buildvcs=false -o scion ./cmd/scion
```

### Start Both Hub and Runtime Broker APIs

```bash
# Start both servers together
./scion server start --enable-hub --enable-runtime-broker
```

You should see output like:
```
2025/01/25 10:00:00 Starting Hub API server on 0.0.0.0:9810
2025/01/25 10:00:00 Database: sqlite (/Users/you/.scion/hub.db)
2025/01/25 10:00:00 Hub API server starting on 0.0.0.0:9810
2025/01/25 10:00:00 Starting Runtime Broker API server on 0.0.0.0:9800
2025/01/25 10:00:00 Runtime Broker API server starting on 0.0.0.0:9800
2025/01/25 10:00:00 Agent dispatcher configured for co-located runtime broker
2025/01/25 10:00:00 Registered global grove with runtime broker your-hostname
```

When both servers start together, they automatically:
1. Create a "global" grove as the default grove for the system
2. Register the runtime broker as a provider to the global grove
3. Set up an agent dispatcher for zero-friction agent handoff

### Alternative: Start Just Runtime Broker

If you only want to test the Runtime Broker API without the Hub:

```bash
./scion server start --enable-runtime-broker
```

### Custom Configuration

```bash
# Custom ports
./scion server start --enable-hub --port 8810 \
  --enable-runtime-broker --runtime-broker-port 8800
```

## 2. Test Runtime Broker Health Endpoints

### Health Check

```bash
curl -s http://localhost:9800/healthz | jq
```

Expected response:
```json
{
  "status": "healthy",
  "version": "0.1.0",
  "uptime": "5s",
  "checks": {
    "container": "available"
  }
}
```

The `checks` field shows available runtimes. On Mac with apple-container, you'll see `"container": "available"`.

### Readiness Check

```bash
curl -s http://localhost:9800/readyz | jq
```

Expected response:
```json
{
  "status": "ready"
}
```

### Broker Info

```bash
curl -s http://localhost:9800/api/v1/info | jq
```

Expected response:
```json
{
  "brokerId": "abc123-...",
  "name": "",
  "version": "0.1.0",
  "type": "container",
  "capabilities": {
    "webPty": false,
    "sync": true,
    "attach": true,
    "exec": true
  },
  "supportedHarnesses": ["claude", "gemini", "opencode", "generic"],
  "resources": {
    "agentsRunning": 0
  }
}
```

## 3. Agent Management via Runtime Broker API

### List Agents

```bash
curl -s http://localhost:9800/api/v1/agents | jq
```

Expected response (empty initially):
```json
{
  "agents": [],
  "totalCount": 0
}
```

### Create an Agent

Agents can be created directly via the Runtime Broker API, or through the Hub API with grove-scoped endpoints.

**Via Runtime Broker API (direct):**

```bash
curl -s -X POST http://localhost:9800/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{
    "name": "test-agent",
    "config": {
      "template": "claude",
      "task": "Hello, this is a test agent"
    }
  }' | jq
```

**Via Hub API (requires grove and runtime broker):**

When both Hub and Runtime Broker servers are co-located, agents can be created via the Hub API and will automatically be dispatched to the runtime broker.

```bash
# Get the global grove ID
GROVE_ID=$(curl -s http://localhost:9810/api/v1/groves | jq -r '.groves[] | select(.slug == "global") | .id')

# Create an agent via Hub API (uses global grove's default runtime broker)
curl -s -X POST http://localhost:9810/api/v1/agents \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"test-agent\",
    \"groveId\": \"$GROVE_ID\",
    \"template\": \"claude\",
    \"task\": \"Hello, this is a test agent\"
  }" | jq
```

When creating agents via the Hub API, you must either:
1. Specify a `runtimeBrokerId` explicitly, OR
2. Use a grove that has a default runtime broker configured (set automatically when co-located servers start)

If neither is available, you'll receive a `no_runtime_broker` error with a list of available alternatives.

Expected response:
```json
{
  "agent": {
    "agentId": "",
    "name": "test-agent",
    "status": "running",
    "containerStatus": "",
    "config": {
      "template": "claude"
    },
    "runtime": {
      "containerId": "container-id-..."
    }
  },
  "created": true
}
```

### Get Agent by ID

```bash
curl -s http://localhost:9800/api/v1/agents/test-agent | jq
```

### Stop an Agent

```bash
curl -s -X POST http://localhost:9800/api/v1/agents/test-agent/stop | jq
```

Expected response:
```json
{
  "status": "accepted",
  "message": "Stop operation accepted"
}
```

### Delete an Agent

```bash
curl -s -X DELETE "http://localhost:9800/api/v1/agents/test-agent?deleteFiles=true"
# Returns 204 No Content on success
```

## 4. Agent Interaction

### Send a Message

Send a message to a running agent's harness (via tmux):

```bash
curl -s -X POST http://localhost:9800/api/v1/agents/test-agent/message \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Please list the files in the current directory",
    "interrupt": false
  }'
```

### Execute a Command

Run a one-off command inside the agent container:

```bash
curl -s -X POST http://localhost:9800/api/v1/agents/test-agent/exec \
  -H "Content-Type: application/json" \
  -d '{
    "command": ["ls", "-la"],
    "timeout": 30
  }' | jq
```

Expected response:
```json
{
  "output": "total 24\ndrwxr-xr-x ...",
  "exitCode": 0
}
```

### Get Logs

```bash
curl -s -X POST http://localhost:9800/api/v1/agents/test-agent/logs
```

Returns plain text logs.

### Get Stats

```bash
curl -s -X POST http://localhost:9800/api/v1/agents/test-agent/stats | jq
```

## 5. Combined Hub + Runtime Broker Workflow

This workflow demonstrates how the Hub and Runtime Broker APIs work together with automatic handoff.

### Step 1: Check Both Servers and Global Grove

When both servers start together, a "global" grove is automatically created and the runtime broker is registered.

```bash
echo "=== Hub Health ==="
curl -s http://localhost:9810/healthz | jq

echo -e "\n=== Runtime Broker Health ==="
curl -s http://localhost:9800/healthz | jq

echo -e "\n=== Global Grove ==="
curl -s http://localhost:9810/api/v1/groves | jq '.groves[] | select(.slug == "global")'
```

### Step 2: Create Agent via Hub (Automatic Handoff)

When you create an agent via the Hub API while a co-located runtime broker is running, the agent is automatically dispatched to the runtime broker and started.

**Runtime Broker Resolution:**

Agents created via the Hub API require a runtime broker. The Hub resolves the runtime broker in this order:
1. Use the explicitly specified `runtimeBrokerId` if provided
2. Fall back to the grove's `defaultRuntimeBrokerId` (set when the first broker registers)
3. Return an error with available alternatives if neither is available


```bash
# Get the global grove ID
GROVE_ID=$(curl -s http://localhost:9810/api/v1/groves | jq -r '.groves[] | select(.slug == "global") | .id')

# Create an agent in the global grove (uses default runtime broker)
curl -s -X POST http://localhost:9810/api/v1/agents \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"feature-agent\",
    \"groveId\": \"$GROVE_ID\",
    \"template\": \"claude\",
    \"task\": \"Hello, please list the files in the current directory\"
  }" | jq
```

**Specifying a runtime broker explicitly:**

```bash
# Get available runtime brokers for the grove
BROKER_ID=$(curl -s "http://localhost:9810/api/v1/runtime-brokers?groveId=$GROVE_ID" | jq -r '.brokers[0].id')

# Create an agent with explicit runtime broker
curl -s -X POST http://localhost:9810/api/v1/agents \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"feature-agent-2\",
    \"groveId\": \"$GROVE_ID\",
    \"runtimeBrokerId\": \"$BROKER_ID\",
    \"template\": \"claude\",
    \"task\": \"Hello, please describe the project structure\"
  }" | jq
```

The agent will be created and automatically started on the co-located runtime broker. The response includes the agent with status "provisioning" or "running" and the assigned `runtimeBrokerId`.

### Step 2b: Create Agent via Grove-Scoped Endpoint (Alternative)

You can also use the RESTful grove-scoped endpoint. This supports both UUID and `{uuid}__{slug}` format for grove IDs:

```bash
# Using grove ID with slug format for readability
curl -s -X POST "http://localhost:9810/api/v1/groves/${GROVE_ID}__global/agents" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "another-agent",
    "template": "claude",
    "task": "Hello, please analyze the codebase"
  }' | jq
```

### Step 3: List Agents on Runtime Broker

With automatic handoff, agents created via the Hub now appear in the runtime broker's agent list:

```bash
curl -s http://localhost:9800/api/v1/agents | jq
```

Expected output shows the agent is running:
```json
{
  "agents": [
    {
      "agentId": "feature-agent",
      "name": "feature-agent",
      "status": "running",
      ...
    }
  ],
  "totalCount": 1
}
```

### Step 4: Verify Agent in Hub

```bash
curl -s "http://localhost:9810/api/v1/agents?groveId=$GROVE_ID" | jq
```

### Step 5: Register Additional Groves (Optional)

For project-specific groves, you can manually register them with a local path. The `path` field specifies where the grove is located on this broker:

```bash
curl -s -X POST http://localhost:9810/api/v1/groves/register \
  -H "Content-Type: application/json" \
  -d '{
    "gitRemote": "https://github.com/myorg/myproject.git",
    "name": "My Project",
    "path": "/path/to/myproject/.scion",
    "broker": {
      "name": "My MacBook",
      "version": "0.1.0",
      "runtimes": [
        {"type": "container", "available": true}
      ],
      "capabilities": {
        "webPty": false,
        "sync": true,
        "attach": true
      },
      "supportedHarnesses": ["claude", "gemini", "opencode", "generic"]
    }
  }' | jq
```

### Step 6: List Agents by Grove

Use the grove-scoped endpoint to list agents for a specific grove:

```bash
curl -s "http://localhost:9810/api/v1/groves/${GROVE_ID}/agents" | jq
```

## 5b. Project-Specific Grove Testing

This section demonstrates registering a local project as a grove and creating agents within it. This is the typical workflow for project-specific agent management.

### Test Setup

For this test, we'll use an existing local project at `/Users/user/src/cli-projects/qa-scion`.

### Step 1: Verify the Project Has a .scion Directory

```bash
ls -la /Users/user/src/cli-projects/qa-scion/.scion
```

If it doesn't exist, you can initialize it:

```bash
cd /Users/user/src/cli-projects/qa-scion && scion init
```

### Step 2: Register the Project Grove with Local Path

The key difference from the global grove is that we provide the `path` field to specify where the grove is located on this broker:

```bash
BROKER_ID=$(curl -s http://localhost:9800/api/v1/info | jq -r '.brokerId')

PROJECT_RESPONSE=$(curl -s -X POST http://localhost:9810/api/v1/groves/register \
  -H "Content-Type: application/json" \
  -d '{
    "name": "QA Scion",
    "gitRemote": "https://github.com/example/qa-scion",
    "path": "/Users/user/src/cli-projects/qa-scion/.scion",
    "broker": {
      "id": "'$BROKER_ID'",
      "name": "Local Mac",
      "version": "0.1.0",
      "runtimes": [{"type": "container", "available": true}],
      "capabilities": {"sync": true, "attach": true}
    }
  }')

echo $PROJECT_RESPONSE | jq
PROJECT_GROVE_ID=$(echo $PROJECT_RESPONSE | jq -r '.grove.id')
echo "Project Grove ID: $PROJECT_GROVE_ID"
```

### Step 3: Verify the Grove Provider Has the Local Path

```bash
# The provider record should now include the local path
curl -s "http://localhost:9810/api/v1/runtime-brokers?groveId=$PROJECT_GROVE_ID" | jq
```

Expected response (note the `localPath` field included for each broker):
```json
{
  "brokers": [
    {
      "id": "7d2bdf70-a975-4e7d-930c-2b67448ed8f6",
      "name": "Local Mac",
      "slug": "local-mac",
      "type": "container",
      "version": "0.1.0",
      "status": "online",
      "connectionState": "connected",
      "lastHeartbeat": "0001-01-01T00:00:00Z",
      "capabilities": {
        "webPty": false,
        "sync": true,
        "attach": true
      },
      "runtimes": [
        {
          "type": "container",
          "available": true
        }
      ],
      "localPath": "/Users/user/src/cli-projects/qa-scion/.scion",
      "created": "2026-01-25T11:22:02.903695-08:00",
      "updated": "2026-01-25T11:29:46.819009-08:00"
    }
  ],
  "totalCount": 1
}
```

The `localPath` field is included when querying runtime brokers filtered by `groveId`, providing the grove-specific filesystem path for each broker provider.

### Step 4: Create an Agent in the Project Grove

```bash
curl -s -X POST "http://localhost:9810/api/v1/groves/${PROJECT_GROVE_ID}/agents" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "project-agent",
    "template": "claude",
    "task": "Hello! Please describe the project structure in /workspace"
  }' | jq
```

### Step 5: Verify Agent is Using the Project Path

The agent should now be running with the project's workspace mounted. Check the agent on the runtime broker:

```bash
curl -s http://localhost:9800/api/v1/agents/project-agent | jq
```

The agent's container will have `/Users/user/src/cli-projects/qa-scion` as its workspace source, properly mounted at `/workspace` inside the container.

### Step 6: Clean Up

```bash
curl -s -X DELETE "http://localhost:9800/api/v1/agents/project-agent?deleteFiles=true"
# Returns 204 No Content on success
```

### Key Differences: Global vs Project Groves

| Aspect | Global Grove | Project Grove |
|--------|--------------|---------------|
| Path | `~/.scion` (automatic) | Explicit `path` in registration |
| Created | Auto-created on server start | Manual registration required |
| Workspace | Current directory or empty | Project directory mounted |
| Git Worktrees | Not applicable | Used for isolated agent branches |

## 6. Error Handling

### No Runtime Broker Available

When creating an agent via the Hub API without a runtime broker configured:

```bash
# Try to create agent for a grove with no registered brokers
curl -s -X POST http://localhost:9810/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{"name": "orphan-agent", "groveId": "grove-with-no-brokers"}' | jq
```

Expected response (422):
```json
{
  "error": {
    "code": "no_runtime_broker",
    "message": "No runtime brokers available for this grove; register a runtime broker first",
    "details": {
      "availableBrokers": []
    }
  }
}
```

### Runtime Broker Unavailable

When specifying a runtime broker that does not exist at all (by ID, name or
slug), the Hub returns `404 runtime_broker_not_found`. The message names the
requested broker and lists the brokers the caller can use for the project
(ptone/scion#2715):

```json
{
  "error": {
    "code": "runtime_broker_not_found",
    "message": "Runtime broker \"no-such-broker\" not found. Brokers you can use for this project: \"My Mac\"",
    "details": {
      "requestedBrokerId": "no-such-broker",
      "availableBrokers": [
        {"id": "broker_abc", "name": "My Mac", "status": "online"}
      ]
    }
  }
}
```

An existing broker that is not yet a provider is auto-linked, if the caller
may update the project. A named broker that exists but is offline (not
connected over the control channel and not `online` in the store) gets
`503 runtime_broker_unavailable` at resolution, before any agent row is
created or any provider link is written. The same 503 is returned when
auto-linking fails:

```bash
curl -s -X POST http://localhost:9810/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{"name": "test", "groveId": "grove123", "runtimeBrokerId": "offline-broker"}' | jq
```

Expected response (503):
```json
{
  "error": {
    "code": "runtime_broker_unavailable",
    "message": "Specified runtime broker is unavailable",
    "details": {
      "requestedBrokerId": "offline-broker",
      "availableBrokers": [
        {"id": "broker_abc", "name": "My Mac", "type": "container", "status": "online"}
      ]
    }
  }
}
```

### Agent Not Found

```bash
curl -s http://localhost:9800/api/v1/agents/nonexistent | jq
```

Expected response (404):
```json
{
  "error": {
    "code": "agent_not_found",
    "message": "Agent not found"
  }
}
```

### Validation Error

```bash
curl -s -X POST http://localhost:9800/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{}' | jq
```

Expected response (400):
```json
{
  "error": {
    "code": "validation_error",
    "message": "name is required"
  }
}
```

### Invalid JSON

```bash
curl -s -X POST http://localhost:9800/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{invalid}' | jq
```

Expected response (400):
```json
{
  "error": {
    "code": "invalid_request",
    "message": "Invalid request body: ..."
  }
}
```

## 7. Full Workflow Script

Save this as `test-runtime-broker.sh`:

```bash
#!/bin/bash
set -e

HUB_URL="http://localhost:9810"
BROKER_URL="http://localhost:9800"

echo "=== 1. Health Checks ==="
echo "Hub:"
curl -s $HUB_URL/healthz | jq '.status'
echo "Runtime Broker:"
curl -s $BROKER_URL/healthz | jq '{status, checks}'

echo -e "\n=== 2. Runtime Broker Info ==="
curl -s $BROKER_URL/api/v1/info | jq '{type, capabilities}'

echo -e "\n=== 3. Register Grove with Broker ==="
GROVE_RESPONSE=$(curl -s -X POST $HUB_URL/api/v1/groves/register \
  -H "Content-Type: application/json" \
  -d '{
    "gitRemote": "https://github.com/test/demo-project",
    "name": "Demo Project",
    "broker": {
      "name": "Test Mac",
      "version": "0.1.0",
      "runtimes": [{"type": "container", "available": true}],
      "capabilities": {"sync": true, "attach": true}
    }
  }')
echo $GROVE_RESPONSE | jq
GROVE_ID=$(echo $GROVE_RESPONSE | jq -r '.grove.id')
echo "Grove ID: $GROVE_ID"

echo -e "\n=== 4. List Agents (should be empty) ==="
curl -s $BROKER_URL/api/v1/agents | jq

echo -e "\n=== 5. Create Agent via Hub ==="
AGENT_RESPONSE=$(curl -s -X POST $HUB_URL/api/v1/agents \
  -H "Content-Type: application/json" \
  -d "{
    \"name\": \"demo-agent\",
    \"groveId\": \"$GROVE_ID\",
    \"template\": \"claude\",
    \"task\": \"Hello, please describe the current environment\"
  }")
echo $AGENT_RESPONSE | jq
AGENT_ID=$(echo $AGENT_RESPONSE | jq -r '.agent.id')
echo "Agent ID: $AGENT_ID"

echo -e "\n=== 6. List Agents in Hub ==="
curl -s "$HUB_URL/api/v1/agents?groveId=$GROVE_ID" | jq '.agents[] | {name, status}'

echo -e "\n=== 7. List Runtime Brokers ==="
curl -s $HUB_URL/api/v1/runtime-brokers | jq '.brokers[] | {name, type, status}'

echo -e "\n=== 8. Final Health Stats ==="
curl -s $HUB_URL/healthz | jq '.stats'

echo -e "\n=== Done! ==="
```

Run it:
```bash
chmod +x test-runtime-broker.sh
./test-runtime-broker.sh
```

## 8. Cleanup

### Stop the Server

Press `Ctrl+C` to gracefully shutdown both servers.

### Reset Database

```bash
rm ~/.scion/hub.db
```

### Clean Up Test Agents

If you created real agents with containers:

```bash
# List scion containers
container list | grep scion

# Stop and remove
container stop <container-name>
container rm <container-name>
```

## Troubleshooting

### Port Already in Use

```bash
# Find process using port 9800
lsof -i :9800

# Use different ports
./scion server start --enable-runtime-broker --runtime-broker-port 9801
```

### Container Runtime Not Found

If you see `"runtime": "unavailable"` in health checks:

```bash
# Verify container CLI is installed
which container

# Check container runtime status
container version
```

### No Agents Listed

The Runtime Broker API lists agents that are:
1. Actually running as containers with `scion.agent=true` label
2. Have agent directories in known grove paths

When both Hub and Runtime Broker are running together (co-located), agents created via the Hub are automatically dispatched to the Runtime Broker and will appear in both agent lists.

### Permission Issues

```bash
# Ensure scion directory exists and is writable
mkdir -p ~/.scion
chmod 755 ~/.scion
```

## API Reference Summary

### Runtime Broker API (Port 9800)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Liveness check |
| `/readyz` | GET | Readiness check |
| `/api/v1/info` | GET | Broker information |
| `/api/v1/agents` | GET | List agents |
| `/api/v1/agents` | POST | Create agent |
| `/api/v1/agents/{id}` | GET | Get agent details |
| `/api/v1/agents/{id}` | DELETE | Delete agent |
| `/api/v1/agents/{id}/start` | POST | Start agent |
| `/api/v1/agents/{id}/stop` | POST | Stop agent |
| `/api/v1/agents/{id}/restart` | POST | Restart agent |
| `/api/v1/agents/{id}/message` | POST | Send message |
| `/api/v1/agents/{id}/exec` | POST | Execute command |
| `/api/v1/agents/{id}/logs` | POST | Get logs |
| `/api/v1/agents/{id}/stats` | POST | Get stats |

### Hub API (Port 9810)

**Standard Endpoints:**

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Liveness check |
| `/api/v1/groves` | GET | List groves |
| `/api/v1/groves/register` | POST | Register grove with broker |
| `/api/v1/groves/{id}` | GET/PATCH/DELETE | Grove operations |
| `/api/v1/agents` | GET/POST | List/create agents |
| `/api/v1/agents/{id}` | GET/PATCH/DELETE | Agent operations |

**Grove-Scoped Endpoints (RESTful):**

These endpoints scope agent operations to a specific grove. The `{groveId}` supports both UUID format and `{uuid}__{slug}` format for readability.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/groves/{groveId}/agents` | GET | List agents in grove |
| `/api/v1/groves/{groveId}/agents` | POST | Create agent in grove |
| `/api/v1/groves/{groveId}/agents/{agentId}` | GET | Get agent in grove |
| `/api/v1/groves/{groveId}/agents/{agentId}` | PATCH | Update agent in grove |
| `/api/v1/groves/{groveId}/agents/{agentId}` | DELETE | Delete agent in grove |
| `/api/v1/groves/{groveId}/agents/{agentId}/status` | POST | Update agent status |

See `hub-api.md` (in this directory) for full Hub API documentation.
