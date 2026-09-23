# Scion Hub API Design

## Status
**Proposed**

## 1. Overview

This document specifies the REST API for the **Scion Hub** (State Server), the centralized service that manages agent state, groves, runtime brokers, templates, and users in the distributed Scion architecture.

The Hub API provides:
- **Grove-Centric Registration**: Groves are the primary unit of registration. Runtime brokers register the groves they serve, not themselves as standalone entities.
- **Git Remote Uniqueness**: Groves associated with git repositories are uniquely identified by their normalized git remote URL.
- **Agent Lifecycle Management**: CRUD operations for agents across distributed runtime brokers
- **Distributed Groves**: A single grove can span multiple runtime brokers (e.g., multiple developers on the same project)
- **Template Registry**: Centralized template storage and distribution
- **Real-time Communication**: WebSocket endpoints for PTY access and status streaming
- **Authentication & Authorization**: User management and access control

### API Conventions

- **Base URL**: `https://{hub-host}/api/v1`
- **Content-Type**: `application/json` for all request/response bodies
- **Authentication**: Bearer token via `Authorization` header
- **Pagination**: Uses `limit` and `cursor` query parameters
- **Versioning**: URL path versioning (`/api/v1`, `/api/v2`)
- **Error Format**: Consistent error response structure (see Section 8)

---

## 2. Data Models

### 2.1 Agent

Represents a running or stopped agent instance.

```json
{
  "id": "string",              // UUID, primary identifier
  "agentId": "string",         // URL-safe slug identifier (e.g., "fix-auth-bug")
  "name": "string",            // Human-friendly display name
  "template": "string",        // Template used to create this agent

  "groveId": "string",         // Grove association (format: <uuid>__<slug>)
  "grove": "string",           // Grove name (for display)

  "labels": {"key": "value"},  // User-defined labels
  "annotations": {"key": "value"}, // System/user annotations

  "status": "string",          // High-level status: provisioning, running, stopped, error
  "connectionState": "string", // Hub connectivity: connected, disconnected, unknown
  "containerStatus": "string", // Container-level status (e.g., "Up 2 hours")
  "sessionStatus": "string",   // Agent session status: started, waiting, completed
  "runtimeState": "string",    // Low-level runtime state

  "image": "string",           // Container image used
  "detached": true,            // Whether running in detached mode
  "runtime": "string",         // Runtime type: docker, kubernetes, apple

  "runtimeBrokerId": "string",   // ID of the Runtime Broker managing this agent
  "runtimeBrokerType": "string", // Type of runtime broker
  "webPtyEnabled": true,       // Whether web terminal access is available
  "taskSummary": "string",     // Current task description

  "appliedConfig": {           // Effective configuration (template + overrides)
    "image": "string",
    "harness": "string",
    "env": {"key": "value"},
    "volumes": [VolumeMount],
    "model": "string"
  },

  "directConnect": {           // Direct connection info (if available, bypassing Hub)
    "enabled": false,
    "sshHost": "string",
    "sshPort": 22,
    "sshUser": "string"
  },

  "kubernetes": {              // K8s-specific metadata (if applicable)
    "cluster": "string",
    "namespace": "string",
    "podName": "string",
    "syncedAt": "string"
  },

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z",
  "lastSeen": "2025-01-24T10:29:00Z",

  "createdBy": "string",       // User ID who created the agent
  "ownerId": "string",         // Current owner user ID
  "visibility": "private",     // Access level: private, team, public

  "stateVersion": 1            // Optimistic locking version
}
```

### 2.2 Grove

Represents a project or logical grouping of agents. **Groves are the primary unit of Hub registration.** When a grove is associated with a git repository, its identity is defined by the normalized git remote URL.

```json
{
  "id": "string",              // UUID
  "name": "string",            // Human-friendly display name
  "slug": "string",            // URL-safe identifier

  "gitRemote": "string",       // Normalized git remote URL (e.g., "github.com/org/repo")
                               // UNIQUE constraint enforced when non-null

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z",

  "createdBy": "string",       // User ID who created/registered the grove
  "ownerId": "string",         // Current owner user ID
  "visibility": "private",     // Access level: private, team, public

  "labels": {"key": "value"},
  "annotations": {"key": "value"},

  "providers": [               // Runtime brokers providing services to this grove
    {
      "brokerId": "string",
      "brokerName": "string",
      "status": "online",      // online, offline
      "profiles": ["docker", "k8s-dev"],  // Profiles this broker can execute
      "lastSeen": "2025-01-24T10:29:00Z"
    }
  ],

  "agentCount": 5,             // Total agents across all providers (computed)
  "activeBrokerCount": 2       // Number of online provider brokers (computed)
}
```

#### Git Remote Normalization

Git remote URLs are normalized before storage to ensure consistent matching:

1. Remove protocol prefix (`https://`, `git@`, `ssh://`)
2. Remove `.git` suffix
3. Convert SSH format to path format (`git@github.com:org/repo` → `github.com/org/repo`)
4. Lowercase the host portion

**Examples:**
- `https://github.com/acme/project.git` → `github.com/acme/project`
- `git@github.com:acme/project.git` → `github.com/acme/project`
- `ssh://git@gitlab.com/team/repo` → `gitlab.com/team/repo`

### 2.3 RuntimeBroker

Represents a compute node that contributes to one or more groves. **Runtime brokers are not the primary registration unit**—they register the groves they serve. The Hub tracks brokers primarily for routing and health monitoring.

```json
{
  "id": "string",              // UUID (generated on first grove registration)
  "name": "string",            // Display name (e.g., "John's MacBook", "prod-k8s-east")
  "slug": "string",            // URL-safe identifier

  "type": "string",            // Primary runtime type: docker, kubernetes, apple
  "version": "string",         // Scion broker agent version (for compatibility)

  "status": "string",          // online, offline, degraded
  "connectionState": "string", // Control channel: connected, disconnected
  "lastHeartbeat": "2025-01-24T10:29:00Z",

  "supportedHarnesses": ["claude", "gemini"], // Available harness types

  "capabilities": {
    "webPty": true,            // Supports web terminal
    "sync": true,              // Supports file sync
    "attach": true             // Supports direct attach
  },

  "runtimes": [                // Available container runtimes on this broker
    {
      "type": "docker",
      "available": true
    },
    {
      "type": "kubernetes",
      "available": true,
      "context": "prod-cluster",
      "namespace": "scion"
    }
  ],

  "resources": {
    "cpuAvailable": "4",       // Available CPU cores
    "memoryAvailable": "8Gi",  // Available memory
    "agentsRunning": 3,        // Current running agents across all groves
    "agentsCapacity": 10       // Maximum agent capacity
  },

  "groves": [                  // Groves this broker contributes to
    {
      "groveId": "string",
      "groveName": "string",
      "profiles": ["docker", "k8s-dev"],  // Profiles this broker can run for this grove
      "agentCount": 2          // Agents running for this grove on this broker
    }
  ],

  "labels": {"key": "value"},
  "annotations": {"key": "value"},

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z"
}
```

### 2.4 Template

Represents an agent template configuration.

```json
{
  "id": "string",              // UUID
  "name": "string",            // Template name (e.g., "claude", "gemini")
  "slug": "string",            // URL-safe identifier

  "harness": "string",         // Harness type: claude, gemini, opencode, codex, generic
  "image": "string",           // Default container image

  "config": {
    "harness": "string",
    "configDir": "string",
    "env": {"key": "value"},
    "volumes": [
      {
        "source": "string",
        "target": "string",
        "readOnly": false,
        "type": "local"
      }
    ],
    "detached": true,
    "commandArgs": ["string"],
    "model": "string",
    "kubernetes": {
      "context": "string",
      "namespace": "string",
      "runtimeClassName": "string",
      "resources": {
        "requests": {"cpu": "1", "memory": "2Gi"},
        "limits": {"cpu": "2", "memory": "4Gi"}
      }
    }
  },

  "scope": "string",           // global, grove, user
  "groveId": "string",         // Grove association (if scope=grove)
  "ownerId": "string",         // Owner user ID
  "visibility": "private",     // Access level

  "storageUri": "string",      // Remote storage URI (e.g., gs://bucket/path)

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z"
}
```

### 2.5 User

Represents a registered user.

```json
{
  "id": "string",              // UUID
  "email": "string",
  "displayName": "string",
  "avatarUrl": "string",

  "role": "string",            // admin, member, viewer
  "status": "string",          // active, suspended

  "preferences": {
    "defaultTemplate": "string",
    "defaultProfile": "string",
    "theme": "dark"
  },

  "created": "2025-01-24T10:00:00Z",
  "lastLogin": "2025-01-24T10:30:00Z"
}
```

### 2.6 StatusEvent

Real-time status update from an agent.

```json
{
  "agentId": "string",
  "status": "string",
  "sessionStatus": "string",
  "message": "string",
  "timestamp": "2025-01-24T10:30:00Z",

  "event": {
    "name": "string",          // Normalized event name (e.g., "session-start")
    "rawName": "string",       // Original event name from harness
    "dialect": "string",       // Source dialect (claude, gemini)
    "data": {
      "prompt": "string",
      "toolName": "string",
      "message": "string",
      "sessionId": "string",
      "success": true,
      "error": "string"
    }
  }
}
```

---

## 3. Agent Endpoints

### 3.1 List Agents

```
GET /api/v1/agents
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `groveId` | string | Filter by grove ID |
| `status` | string | Filter by status (running, stopped, etc.) |
| `runtimeBrokerId` | string | Filter by runtime broker |
| `labels` | string | Label selector (e.g., `env=prod,team=platform`) |
| `limit` | int | Maximum results (default: 50, max: 200) |
| `cursor` | string | Pagination cursor |

**Response:**
```json
{
  "agents": [Agent],
  "nextCursor": "string",
  "totalCount": 100
}
```

### 3.2 Get Agent

```
GET /api/v1/agents/{agentId}
```

**Response:**
```json
Agent
```

### 3.3 Create Agent

```
POST /api/v1/agents
```

**Request Body:**
```json
{
  "name": "string",            // Required: agent name
  "groveId": "string",         // Required: grove to create in
  "template": "string",        // Template name or ID
  "runtimeBrokerId": "string",   // Target runtime broker (optional, Hub selects if omitted)

  "task": "string",            // Initial task/prompt
  "branch": "string",          // Git branch to use
  "workspace": "string",       // Workspace path within grove

  "labels": {"key": "value"},
  "annotations": {"key": "value"},

  "config": {                  // Override template config
    "image": "string",
    "env": {"key": "value"},
    "volumes": [VolumeMount],
    "detached": true,
    "model": "string"
  },

  "resume": false              // Resume from existing agent state
}
```

**Response:**
```json
{
  "agent": Agent,
  "warnings": ["string"]
}
```

### 3.4 Update Agent

```
PATCH /api/v1/agents/{agentId}
```

**Request Body:**
```json
{
  "name": "string",
  "labels": {"key": "value"},
  "annotations": {"key": "value"},
  "taskSummary": "string",
  "stateVersion": 1            // Required for optimistic locking
}
```

### 3.5 Delete Agent

```
DELETE /api/v1/agents/{agentId}
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `deleteFiles` | bool | Also delete agent files (default: false) |
| `removeBranch` | bool | Remove git branch (default: false) |

### 3.6 Agent Actions

#### Start Agent
```
POST /api/v1/agents/{agentId}/start
```

#### Stop Agent
```
POST /api/v1/agents/{agentId}/stop
```

#### Restart Agent
```
POST /api/v1/agents/{agentId}/restart
```

#### Send Message
```
POST /api/v1/agents/{agentId}/message
```

**Request Body:**
```json
{
  "message": "string",
  "interrupt": false           // Send interrupt key first
}
```

#### Get Logs
```
GET /api/v1/agents/{agentId}/logs
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `tail` | int | Number of lines from end |
| `since` | string | RFC3339 timestamp |
| `follow` | bool | Stream logs (upgrades to WebSocket) |

### 3.7 Execute Command

```
POST /api/v1/agents/{agentId}/exec
```

Execute a one-off command inside an agent container (similar to `kubectl exec`).

**Request Body:**
```json
{
  "command": ["string"],       // Command and arguments
  "timeout": 30                // Timeout in seconds (default: 30, max: 300)
}
```

**Response:**
```json
{
  "output": "string",          // Command stdout/stderr
  "exitCode": 0
}
```

### 3.8 Agent Status Reporting (from sciontool)

```
POST /api/v1/agents/{agentId}/status
```

Internal endpoint for agents to report status updates.

**Request Body:**
```json
{
  "status": "string",
  "sessionStatus": "string",
  "message": "string",
  "event": Event
}
```

**Headers:**
- `X-Scion-Agent-Token`: Agent authentication token (scoped to this agent only)

**Token Scope:** Agent tokens are limited to:
- Updating status for the specific agent
- Reporting events/logs for the specific agent
- Cannot access other agents or Hub resources

### 3.9 Sync Agent

```
PUT /api/v1/agents/{agentId}
```

Upsert endpoint for Runtime Brokers to register locally-created agents with the Hub. This is typically used by brokers that are managing agents independently of the Hub's lifecycle control. If the agent doesn't exist, it is created; if it exists, its state is updated.

**Request Body:**
```json
{
  "name": "string",
  "groveId": "string",
  "template": "string",
  "runtimeBrokerId": "string",   // The reporting broker

  "status": "string",
  "containerStatus": "string",
  "sessionStatus": "string",

  "image": "string",
  "labels": {"key": "value"},
  "annotations": {"key": "value"},

  "appliedConfig": {           // The actual running configuration
    "image": "string",
    "harness": "string",
    "env": {"key": "value"}
  },

  "created": "2025-01-24T10:00:00Z"
}
```

**Headers (HMAC Authentication):**
- `X-Scion-Broker-ID`: Runtime broker identifier
- `X-Scion-Timestamp`: Request timestamp (RFC 3339)
- `X-Scion-Nonce`: Random nonce for replay prevention
- `X-Scion-Signature`: HMAC-SHA256 signature

See [Runtime Broker Auth](auth/runtime-broker-auth.md) for the complete authentication specification.

**Response:**
```json
{
  "agent": Agent,
  "created": true              // Whether this was a new registration
}
```

---

## 4. Grove Endpoints

Groves are the primary unit of Hub registration. Runtime brokers register groves, and the Hub enforces git remote uniqueness.

### 4.1 List Groves

```
GET /api/v1/groves
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `visibility` | string | Filter by visibility |
| `gitRemote` | string | Filter by git remote (exact or prefix match) |
| `brokerId` | string | Filter by contributing broker |
| `labels` | string | Label selector |
| `limit` | int | Maximum results |
| `cursor` | string | Pagination cursor |

### 4.2 Get Grove

```
GET /api/v1/groves/{groveId}
```

### 4.3 Register Grove (Primary Registration Endpoint)

```
POST /api/v1/groves/register
```

This is the **primary registration endpoint** for runtime brokers. It performs an upsert based on the git remote URL:
- If a grove with the same git remote exists: adds the broker as a provider
- If no matching grove exists: creates a new grove with this broker as the initial provider

**Request Body:**
```json
{
  "name": "string",            // Grove display name
  "gitRemote": "string",       // Git remote URL (will be normalized)
  "path": "string",            // Local filesystem path on the broker

  "broker": {                  // Broker information
    "id": "string",            // Existing broker ID (optional, for reconnection)
    "name": "string",          // Broker display name
    "version": "string",       // Scion version
    "capabilities": {
      "webPty": true,
      "sync": true,
      "attach": true
    },
    "runtimes": [              // Available runtimes
      {"type": "docker", "available": true},
      {"type": "kubernetes", "available": true, "context": "string", "namespace": "string"}
    ],
    "supportedHarnesses": ["claude", "gemini"]
  },

  "profiles": ["docker", "k8s-dev"],  // Profiles this broker can execute for this grove

  "labels": {"key": "value"},
  "annotations": {"key": "value"}
}
```

**Response:**
```json
{
  "grove": Grove,
  "broker": RuntimeBroker,
  "created": true,             // Whether grove was newly created (vs linked)
  "brokerToken": "string"      // Authentication token for this broker
}
```

**Error Cases:**
- `409 Conflict`: Git remote already registered by a different owner (use link endpoint instead)

### 4.4 Create Grove (Without Broker)

```
POST /api/v1/groves
```

Creates a grove record without an initial contributing broker. Used for pre-provisioning or Hub-managed groves.

**Request Body:**
```json
{
  "name": "string",            // Required
  "gitRemote": "string",       // Optional, but if provided must be unique
  "visibility": "private",
  "labels": {"key": "value"},
  "annotations": {"key": "value"}
}
```

### 4.5 Update Grove

```
PATCH /api/v1/groves/{groveId}
```

### 4.6 Delete Grove

```
DELETE /api/v1/groves/{groveId}
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `deleteAgents` | bool | Also delete all agents (default: false) |

### 4.7 List Grove Agents

```
GET /api/v1/groves/{groveId}/agents
```

Returns agents belonging to a specific grove. Same response format as `GET /agents`.

### 4.8 List Grove Providers

```
GET /api/v1/groves/{groveId}/providers
```

Returns runtime brokers providing services to this grove.

**Response:**
```json
{
  "providers": [
    {
      "brokerId": "string",
      "brokerName": "string",
      "status": "online",
      "profiles": ["docker", "k8s-dev"],
      "agentCount": 2,
      "lastSeen": "2025-01-24T10:29:00Z"
    }
  ]
}
```

### 4.9 Remove Grove Provider

```
DELETE /api/v1/groves/{groveId}/providers/{brokerId}
```

Removes a broker as a provider to this grove. Does not affect agents already running on that broker.

### 4.10 Grove Settings

```
GET /api/v1/groves/{groveId}/settings
PUT /api/v1/groves/{groveId}/settings
```

**Settings Body:**
```json
{
  "activeProfile": "string",
  "defaultTemplate": "string",
  "bucket": {
    "provider": "GCS",
    "name": "string",
    "prefix": "string"
  },
  "runtimes": {
    "docker": RuntimeConfig,
    "kubernetes": RuntimeConfig
  },
  "harnesses": {
    "claude": HarnessConfig,
    "gemini": HarnessConfig
  },
  "profiles": {
    "default": ProfileConfig,
    "k8s": ProfileConfig
  }
}
```

---

## 5. Runtime Broker Endpoints

Runtime brokers are tracked by the Hub for routing and health monitoring, but **grove registration is the primary mechanism** for brokers to connect to the Hub. These endpoints provide administrative views and operations on known brokers.

### 5.1 List Runtime Brokers

```
GET /api/v1/runtime-brokers
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `type` | string | Filter by type (docker, kubernetes, apple) |
| `status` | string | Filter by status (online, offline) |
| `groveId` | string | Filter by grove contribution |

### 5.2 Get Runtime Broker

```
GET /api/v1/runtime-brokers/{brokerId}
```

### 5.3 List Broker Groves

```
GET /api/v1/runtime-brokers/{brokerId}/groves
```

Returns groves this broker contributes to.

**Response:**
```json
{
  "groves": [
    {
      "groveId": "string",
      "groveName": "string",
      "gitRemote": "string",
      "profiles": ["docker", "k8s-dev"],
      "agentCount": 2
    }
  ]
}
```

### 5.4 Update Runtime Broker

```
PATCH /api/v1/runtime-brokers/{brokerId}
```

Updates broker metadata (name, labels, etc.). Broker capabilities and runtimes are updated via grove registration.

### 5.5 Deregister Runtime Broker

```
DELETE /api/v1/runtime-brokers/{brokerId}
```

Removes the broker from all groves and deletes its record. Agents running on this broker are marked as orphaned.

### 5.6 Runtime Broker Heartbeat

```
POST /api/v1/runtime-brokers/{brokerId}/heartbeat
```

Internal endpoint for runtime brokers to report health.

**Request Body:**
```json
{
  "status": "online",
  "resources": {
    "cpuAvailable": "4",
    "memoryAvailable": "8Gi",
    "agentsRunning": 3
  },
  "groves": [                  // Status per grove
    {
      "groveId": "string",
      "agentCount": 2,
      "agents": [
        {
          "agentId": "string",
          "status": "string",
          "containerStatus": "string"
        }
      ]
    }
  ]
}
```

**Headers (HMAC Authentication):**
- `X-Scion-Broker-ID`: Runtime broker identifier
- `X-Scion-Timestamp`: Request timestamp (RFC 3339)
- `X-Scion-Nonce`: Random nonce for replay prevention
- `X-Scion-Signature`: HMAC-SHA256 signature

### 5.7 Runtime Broker Message Delivery Failures

```
POST /api/v1/runtime-brokers/{brokerId}/message-failures
```

Internal endpoint for a runtime broker to report messages it accepted into its
debounce buffer (answering 200, so the hub marked them `dispatched`) but then
failed to deliver to the agent. The hub moves each reported message from
`pending`/`dispatched` to `failed` and sends a `DELIVERY_FAILED` notice to an
agent sender. See ptone/scion#1820.

The hub passes its message ID to the broker as `message_id` on the agent
message request, for agent-to-agent DMs and broker/pub-sub deliveries. Only
those messages can be reported.

Only the broker itself may call this endpoint (HMAC broker identity must match
`{brokerId}`). Reports are ignored for unknown message IDs, for messages whose
recipient agent is not assigned to the reporting broker, and for rows already
in a terminal state. At most 500 failures may be sent per request.

**Request Body:**
```json
{
  "failures": [
    {
      "messageId": "string",   // hub message ID (required)
      "agentId": "string",     // broker-side agent identifier (informational)
      "projectId": "string",   // informational
      "reason": "string"       // stored as the dispatch failure reason
    }
  ]
}
```

**Response:** `200 OK` with `{"marked": 1, "ignored": 0}`.

---

## 6. Template Endpoints

### 6.1 List Templates

```
GET /api/v1/templates
```

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Filter by scope (global, grove, user) |
| `groveId` | string | Filter by grove |
| `harness` | string | Filter by harness type |

### 6.2 Get Template

```
GET /api/v1/templates/{templateId}
```

### 6.3 Create Template

```
POST /api/v1/templates
```

**Request Body:**
```json
{
  "name": "string",
  "harness": "string",
  "scope": "grove",
  "groveId": "string",
  "config": ScionConfig,
  "visibility": "private"
}
```

### 6.4 Update Template

```
PUT /api/v1/templates/{templateId}
```

### 6.5 Delete Template

```
DELETE /api/v1/templates/{templateId}
```

### 6.6 Clone Template

```
POST /api/v1/templates/{templateId}/clone
```

**Request Body:**
```json
{
  "name": "string",
  "scope": "grove",
  "groveId": "string"
}
```

---

## 7. Environment Variables & Secrets Endpoints

The Hub provides endpoints for managing environment variables and secrets scoped to users, groves, or runtime brokers. See `hosted-architecture.md` Section 6 for the full design.

### 7.1 Data Models

#### EnvVar

```json
{
  "id": "string",              // UUID
  "key": "string",             // Variable name (e.g., "LOG_LEVEL")
  "value": "string",           // Variable value

  "scope": "string",           // user, grove, runtime_broker
  "scopeId": "string",         // ID of the scoped entity

  "description": "string",     // Optional description
  "sensitive": false,          // If true, value is masked in responses

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z",
  "createdBy": "string"
}
```

#### Secret (Metadata Only)

```json
{
  "id": "string",              // UUID
  "key": "string",             // Secret name (e.g., "API_KEY")

  "scope": "string",           // user, grove, runtime_broker
  "scopeId": "string",         // ID of the scoped entity

  "description": "string",     // Optional description
  "version": 1,                // Incremented on each update

  "created": "2025-01-24T10:00:00Z",
  "updated": "2025-01-24T10:30:00Z",
  "createdBy": "string",
  "updatedBy": "string"
}
```

**Note:** Secret values are never returned in API responses.

### 7.2 Environment Variable Endpoints

#### List Environment Variables

```
GET /api/v1/env
```

Returns environment variables for the specified scope. Defaults to user scope.

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type: `user`, `grove`, `runtime_broker` (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |
| `key` | string | Filter by specific key (optional) |

**Response:**
```json
{
  "envVars": [EnvVar],
  "scope": "string",
  "scopeId": "string"
}
```

#### Get Environment Variable

```
GET /api/v1/env/{key}
```

Returns a specific environment variable by key.

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |

**Response:**
```json
EnvVar
```

#### Set Environment Variable

```
PUT /api/v1/env/{key}
```

Creates or updates an environment variable. Upsert semantics based on key + scope + scopeId.

**Request Body:**
```json
{
  "value": "string",           // Required: variable value
  "scope": "string",           // Scope type (default: user)
  "scopeId": "string",         // Required for grove/runtime_broker scope
  "description": "string",     // Optional description
  "sensitive": false           // Optional: mask value in responses
}
```

**Response:** `200 OK`
```json
{
  "envVar": EnvVar,
  "created": true              // Whether this was a new variable
}
```

#### Delete Environment Variable

```
DELETE /api/v1/env/{key}
```

Deletes an environment variable.

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |

**Response:** `204 No Content`

### 7.3 Secret Endpoints

#### List Secrets

```
GET /api/v1/secrets
```

Returns secret metadata for the specified scope. **Values are never returned.**

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type: `user`, `grove`, `runtime_broker` (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |

**Response:**
```json
{
  "secrets": [Secret],         // Metadata only, no values
  "scope": "string",
  "scopeId": "string"
}
```

#### Get Secret Metadata

```
GET /api/v1/secrets/{key}
```

Returns metadata for a specific secret. **Value is never returned.**

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |

**Response:**
```json
Secret                         // Metadata only, no value
```

#### Set Secret

```
PUT /api/v1/secrets/{key}
```

Creates or updates a secret. Upsert semantics based on key + scope + scopeId.

**Request Body:**
```json
{
  "value": "string",           // Required: secret value (write-only)
  "scope": "string",           // Scope type (default: user)
  "scopeId": "string",         // Required for grove/runtime_broker scope
  "description": "string"      // Optional description
}
```

**Response:** `200 OK`
```json
{
  "secret": Secret,            // Metadata only, no value
  "created": true              // Whether this was a new secret
}
```

#### Delete Secret

```
DELETE /api/v1/secrets/{key}
```

Deletes a secret.

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `scope` | string | Scope type (default: `user`) |
| `scopeId` | string | ID of the scoped entity (required for grove/runtime_broker) |

**Response:** `204 No Content`

### 7.4 Resolved Environment (Internal)

This endpoint is used internally by the Hub when dispatching agent creation commands. It returns the fully resolved environment for an agent, merging values from all applicable scopes.

```
GET /api/v1/agents/{agentId}/resolved-env
```

**Note:** This is an internal endpoint used by the Hub during agent creation. The resolved environment includes both env vars and decrypted secrets.

**Response:**
```json
{
  "env": {
    "KEY": "value",
    "ANOTHER_KEY": "another_value"
  },
  "sources": {
    "KEY": "user",
    "ANOTHER_KEY": "grove"
  }
}
```

### 7.5 Scope-Specific Convenience Endpoints

For convenience, the API also provides scope-specific endpoints that mirror the generic endpoints:

#### User Scope
```
GET    /api/v1/users/me/env
GET    /api/v1/users/me/env/{key}
PUT    /api/v1/users/me/env/{key}
DELETE /api/v1/users/me/env/{key}
GET    /api/v1/users/me/secrets
GET    /api/v1/users/me/secrets/{key}
PUT    /api/v1/users/me/secrets/{key}
DELETE /api/v1/users/me/secrets/{key}
```

#### Grove Scope
```
GET    /api/v1/groves/{groveId}/env
GET    /api/v1/groves/{groveId}/env/{key}
PUT    /api/v1/groves/{groveId}/env/{key}
DELETE /api/v1/groves/{groveId}/env/{key}
GET    /api/v1/groves/{groveId}/secrets
GET    /api/v1/groves/{groveId}/secrets/{key}
PUT    /api/v1/groves/{groveId}/secrets/{key}
DELETE /api/v1/groves/{groveId}/secrets/{key}
```

#### Runtime Broker Scope
```
GET    /api/v1/runtime-brokers/{brokerId}/env
GET    /api/v1/runtime-brokers/{brokerId}/env/{key}
PUT    /api/v1/runtime-brokers/{brokerId}/env/{key}
DELETE /api/v1/runtime-brokers/{brokerId}/env/{key}
GET    /api/v1/runtime-brokers/{brokerId}/secrets
GET    /api/v1/runtime-brokers/{brokerId}/secrets/{key}
PUT    /api/v1/runtime-brokers/{brokerId}/secrets/{key}
DELETE /api/v1/runtime-brokers/{brokerId}/secrets/{key}
```

---

## 8. WebSocket Endpoints

### 8.1 WebSocket Authentication

Browser WebSocket APIs cannot set custom HTTP headers. WebSocket endpoints support two authentication methods:

1. **Query Parameter Token:**
   ```
   WS /api/v1/agents/{agentId}/pty?token=<bearer-token>
   ```

2. **Ticket-Based Auth (Recommended for browsers):**
   ```
   POST /api/v1/auth/ws-ticket
   ```
   Returns a short-lived (60 second) ticket that can be used once:
   ```json
   {
     "ticket": "string",
     "expiresAt": "2025-01-24T10:01:00Z"
   }
   ```
   Use the ticket in the WebSocket URL:
   ```
   WS /api/v1/agents/{agentId}/pty?ticket=<ticket>
   ```

### 8.2 Agent PTY

```
WS /api/v1/agents/{agentId}/pty
```

Provides web terminal access to an agent. The Hub proxies the connection to the appropriate Runtime Broker via the control channel (see Section 10).

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `token` | string | Bearer token (alternative to header) |
| `ticket` | string | One-time ticket from `/auth/ws-ticket` |

**Initial Handshake:**
```json
{
  "type": "resize",
  "cols": 120,
  "rows": 40
}
```

**Data Messages:**
```json
{
  "type": "data",
  "data": "base64-encoded-bytes"
}
```

**Stream Multiplexing:** When the Hub proxies PTY to a Runtime Broker over the control channel, each stream is assigned a unique `streamId`. The Hub maintains the mapping between client WebSocket connections and control channel streams.

### 8.3 Agent Status Stream

```
WS /api/v1/agents/{agentId}/events
```

Real-time stream of agent status events.

**Event Messages:**
```json
StatusEvent
```

### 8.4 Grove Events

```
WS /api/v1/groves/{groveId}/events
```

Real-time stream of all agent events within a grove.

### 8.5 Global Events

```
WS /api/v1/events
```

Real-time stream of all events (admin only).

**Query Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `groveId` | string | Filter by grove |
| `runtimeBrokerId` | string | Filter by runtime broker |
| `eventTypes` | string | Comma-separated event types |

---

## 9. Error Responses

All error responses follow a consistent format:

```json
{
  "error": {
    "code": "string",          // Machine-readable error code
    "message": "string",       // Human-readable message
    "details": {},             // Additional context (optional)
    "requestId": "string"      // Request tracking ID
  }
}
```

### Error Codes

| HTTP Status | Code | Description |
|-------------|------|-------------|
| 400 | `invalid_request` | Malformed request body |
| 400 | `validation_error` | Request validation failed |
| 401 | `unauthorized` | Missing or invalid authentication |
| 403 | `forbidden` | Insufficient permissions |
| 404 | `not_found` | Resource not found |
| 409 | `conflict` | Resource conflict (e.g., name collision) |
| 409 | `version_conflict` | Optimistic locking failed |
| 422 | `unprocessable` | Valid request but cannot be processed |
| 429 | `rate_limited` | Too many requests |
| 500 | `internal_error` | Server error |
| 502 | `runtime_error` | Runtime broker communication failed |
| 503 | `unavailable` | Service temporarily unavailable |

---

## 10. Authentication & Authorization

### 10.1 Authentication Methods

1. **Bearer Token** (Primary)
   ```
   Authorization: Bearer <token>
   ```

2. **API Key** (Machine-to-machine)
   ```
   X-API-Key: <key>
   ```

3. **Agent Token** (Agent-to-Hub)
   ```
   X-Scion-Agent-Token: <token>
   ```

4. **Broker HMAC Authentication** (Runtime Broker ↔ Hub)
   ```
   X-Scion-Broker-ID: <broker-id>
   X-Scion-Timestamp: <RFC 3339 timestamp>
   X-Scion-Nonce: <base64-encoded nonce>
   X-Scion-Signature: <HMAC-SHA256 signature>
   ```
   See [Runtime Broker Auth](auth/runtime-broker-auth.md) for the complete specification.

### 10.2 Authentication Endpoints

```
POST /api/v1/auth/login
POST /api/v1/auth/logout
POST /api/v1/auth/refresh
GET  /api/v1/auth/me
```

### 10.3 Authorization Model

Resources support three visibility levels:
- **private**: Only the owner can access
- **team**: Team members can access
- **public**: Anyone can read (write requires ownership)

Role-based access control:
- **admin**: Full access to all resources
- **member**: Can create/manage own resources
- **viewer**: Read-only access to visible resources

### 10.4 Token and Secret Lifecycle

#### Broker Shared Secrets (HMAC Authentication)

Runtime Brokers authenticate with the Hub using HMAC-based request signing. See [Runtime Broker Auth](auth/runtime-broker-auth.md) for the complete specification.

1. **Registration:** User creates a broker record, receives a short-lived join token
2. **Join:** Broker exchanges join token for a shared secret (one-time transmission)
3. **Storage:** Broker stores secret securely (`~/.scion/broker-credentials.json` or secret manager)
4. **Authentication:** All subsequent requests are HMAC-signed using the shared secret
5. **Rotation:** Hub initiates secret rotation with grace period for dual-secret validation:
   ```
   POST /api/v1/secrets/rotate (Hub → Broker, over authenticated WebSocket)
   ```
6. **Revocation:** Deleting a broker immediately invalidates the shared secret

#### Agent Tokens

1. **Generation:** When the Hub instructs a broker to create an agent, it generates a short-lived bootstrap token (valid 5 minutes).
2. **Bootstrap Exchange:** The agent's `sciontool` exchanges the bootstrap token for a session token:
   ```
   POST /api/v1/auth/agent-token-exchange
   ```
   **Request:**
   ```json
   {
     "bootstrapToken": "string",
     "agentId": "string"
   }
   ```
   **Response:**
   ```json
   {
     "sessionToken": "string",
     "expiresAt": "2025-01-25T10:00:00Z",
     "refreshBefore": "2025-01-24T22:00:00Z"
   }
   ```
3. **Refresh:** Session tokens are refreshed automatically before expiry.
4. **Scope:** Agent tokens can only update status/events for their specific agent.

#### API Keys

1. **Generation:** Users create API keys via the dashboard or API:
   ```
   POST /api/v1/auth/api-keys
   ```
2. **Scopes:** API keys can be scoped to specific permissions (read-only, specific groves, etc.).
3. **Expiry:** Optional expiry date; default is no expiry.
4. **Revocation:** Keys can be revoked immediately via `DELETE /api/v1/auth/api-keys/{keyId}`.

---

## 11. Broker Control Plane Protocol

Runtime Brokers often run behind NAT/firewalls (developer laptops, on-premise servers). The Hub cannot initiate HTTP connections to these brokers. Instead, **Runtime Brokers establish a persistent WebSocket control channel to the Hub**.

The control channel is established **after grove registration**. The broker must first register at least one grove via the REST API, then connect the control channel for real-time communication.

### 11.1 Control Channel Architecture

```
┌─────────────────┐                    ┌─────────────────┐
│   Scion Hub     │                    │  Runtime Broker   │
│                 │◄───────────────────│  (behind NAT)   │
│                 │   WebSocket        │                 │
│  Control Plane  │   Control Channel  │  Broker Agent   │
│                 │(Broker-initiated)  │                 │
└─────────────────┘                    └─────────────────┘
        │                                      │
        │  Commands (Hub → Broker)             │
        │  ◄────────────────────               │
        │  Events (Broker → Hub)               │
        │  ────────────────────►               │
        │                                      │
        │  Multiplexed Streams                 │
        │  ◄────────────────────►              │
        │  (PTY, File Transfer, Logs)          │
```

### 11.2 Control Channel Connection

```
WS /api/v1/runtime-brokers/connect
```

Runtime Broker initiates a persistent WebSocket connection to the Hub. The broker must have already completed the registration flow and obtained a shared secret.

**Headers (HMAC Authentication):**
- `X-Scion-Broker-ID`: Broker identifier
- `X-Scion-Timestamp`: Request timestamp (RFC 3339)
- `X-Scion-Nonce`: Random nonce for replay prevention
- `X-Scion-Signature`: HMAC-SHA256 signature of the WebSocket upgrade request

Once the WebSocket is established with HMAC authentication, Hub→Broker commands over the connection use session-based trust (no per-message signing required). Broker→Hub requests that require authorization must use standard HMAC-authenticated HTTP requests. See [Runtime Broker Auth](auth/runtime-broker-auth.md) Section 10.5 for details.

**Initial Handshake Message (Broker → Hub):**
```json
{
  "type": "connect",
  "brokerId": "string",
  "version": "1.2.3",
  "groves": [                  // Groves this broker is contributing to
    {
      "groveId": "string",
      "profiles": ["docker", "k8s-dev"]
    }
  ],
  "capabilities": {
    "webPty": true,
    "sync": true,
    "attach": true
  },
  "supportedHarnesses": ["claude", "gemini"],
  "resources": {
    "cpuAvailable": "4",
    "memoryAvailable": "8Gi"
  }
}
```

**Connection Acknowledgment (Hub → Broker):**
```json
{
  "type": "connected",
  "brokerId": "string",
  "hubTime": "2025-01-24T10:00:00Z",
  "groves": [                  // Confirmed grove associations
    {
      "groveId": "string",
      "groveName": "string"
    }
  ]
}
```

### 11.3 Command Messages (Hub → Broker)

The Hub sends commands to Runtime Brokers over the control channel.

**Command Envelope:**
```json
{
  "type": "command",
  "id": "string",              // Unique command ID for correlation
  "command": "string",         // Command type (see below)
  "payload": {}                // Command-specific payload
}
```

**Command Types:**

| Command | Description |
|---------|-------------|
| `create_agent` | Create and start a new agent |
| `start_agent` | Start a stopped agent |
| `stop_agent` | Stop a running agent |
| `delete_agent` | Delete an agent |
| `exec` | Execute a command in an agent |
| `open_stream` | Open a multiplexed stream (PTY, logs) |
| `close_stream` | Close a multiplexed stream |
| `ping` | Keepalive ping |

**CreateAgent Command:**
```json
{
  "type": "command",
  "id": "cmd-123",
  "command": "create_agent",
  "payload": {
    "agentId": "string",
    "name": "string",
    "config": {
      "template": "string",
      "image": "string",
      "homeDir": "string",
      "workspace": "string",
      "repoRoot": "string",
      "env": ["KEY=value"],
      "volumes": [VolumeMount],
      "labels": {"key": "value"},
      "annotations": {"key": "value"},
      "harness": "string",
      "useTmux": true,
      "task": "string",
      "commandArgs": ["string"],
      "resume": false,
      "kubernetes": KubernetesConfig
    },
    "hubEndpoint": "string",
    "agentToken": "string"
  }
}
```

**OpenStream Command (for PTY/logs):**
```json
{
  "type": "command",
  "id": "cmd-456",
  "command": "open_stream",
  "payload": {
    "streamId": "string",      // Hub-assigned stream ID
    "agentId": "string",
    "streamType": "pty",       // pty, logs, sync
    "options": {
      "cols": 120,
      "rows": 40
    }
  }
}
```

### 11.4 Response Messages (Broker → Hub)

**Response Envelope:**
```json
{
  "type": "response",
  "id": "string",              // Correlates to command ID
  "success": true,
  "payload": {},               // Response data
  "error": {                   // Present if success=false
    "code": "string",
    "message": "string"
  }
}
```

### 11.5 Event Messages (Broker → Hub)

Runtime Brokers send unsolicited events to the Hub.

**Event Envelope:**
```json
{
  "type": "event",
  "event": "string",           // Event type
  "payload": {}                // Event-specific data
}
```

**Event Types:**

| Event | Description |
|-------|-------------|
| `agent_status` | Agent status change |
| `agent_created` | Agent creation completed |
| `agent_deleted` | Agent deletion completed |
| `heartbeat` | Periodic health check |
| `resource_update` | Resource availability changed |

**Agent Status Event:**
```json
{
  "type": "event",
  "event": "agent_status",
  "payload": {
    "agentId": "string",
    "status": "running",
    "containerStatus": "Up 2 hours",
    "sessionStatus": "waiting"
  }
}
```

### 11.6 Stream Multiplexing

Byte streams (PTY, logs, file transfer) are multiplexed over the control channel using stream frames.

**Stream Frame:**
```json
{
  "type": "stream",
  "streamId": "string",
  "data": "base64-encoded-bytes"
}
```

**Stream Close:**
```json
{
  "type": "stream_close",
  "streamId": "string",
  "reason": "string"           // Optional close reason
}
```

The Hub maps incoming client WebSocket connections (e.g., `/agents/{id}/pty`) to stream IDs on the appropriate Runtime Broker control channel.

### 11.7 Heartbeat & Reconnection

- **Heartbeat Interval:** Broker sends heartbeat every 30 seconds
- **Timeout:** Hub marks broker as `disconnected` after 90 seconds without heartbeat
- **Reconnection:** Broker should implement exponential backoff (1s, 2s, 4s, ... max 60s)
- **Session Resumption:** On reconnect, Hub sends list of expected agents for reconciliation

**Heartbeat Message:**
```json
{
  "type": "event",
  "event": "heartbeat",
  "payload": {
    "agentsRunning": 3,
    "cpuUsage": "45%",
    "memoryUsage": "60%"
  }
}
```

### 11.8 Independent Broker Management

When a Runtime Broker manages agents independently of Hub lifecycle control (governed by the permissions system):
- It establishes a control channel connection for visibility.
- It sends `agent_status` events for locally-managed agents to the Hub.
- If the Hub lacks the necessary permissions to manage agents on the broker, lifecycle commands (`create_agent`, `stop_agent`, `delete_agent`) will be rejected with an error.
- It can still support `open_stream` for PTY/logs observation if permitted.

The Hub tracks these agents but cannot control their lifecycle without the appropriate permissions.

### 11.9 Transport Selection

The Hub supports two transport modes for communicating with Runtime Brokers:

1. **Control Channel (WebSocket)**: Broker-initiated persistent connection. Used when brokers are behind NAT/firewalls.
2. **Direct HTTP**: Hub calls Runtime Broker API endpoints directly. Used when brokers have stable, reachable endpoints.

**Selection Logic:**

When the Hub needs to send a command to a Runtime Broker:
1. If Broker has an active control channel connection → use WebSocket
2. If Broker has a registered `endpoint` URL and `status == "online"` → attempt direct HTTP
3. If neither available → return `502 runtime_error`

**Endpoint Mapping:**

Control channel commands map to Runtime Broker API endpoints:

| Control Channel Command | Runtime Broker Endpoint |
|-------------------------|----------------------|
| `create_agent` | `POST /api/v1/agents` |
| `start_agent` | `POST /api/v1/agents/{id}/start` |
| `stop_agent` | `POST /api/v1/agents/{id}/stop` |
| `delete_agent` | `DELETE /api/v1/agents/{id}` |
| `exec` | `POST /api/v1/agents/{id}/exec` |
| `open_stream` | `GET /api/v1/agents/{id}/attach` (WebSocket) |

See `runtime-broker-api.md` for the complete Runtime Broker API specification.

### 11.10 Command Timeouts

Commands sent over the control channel have configurable timeouts:

| Command | Default | Max |
|---------|---------|-----|
| `create_agent` | 120s | 300s |
| `start_agent` | 60s | 120s |
| `stop_agent` | 30s | 60s |
| `delete_agent` | 30s | 60s |
| `exec` | 30s | 300s |
| `open_stream` | 10s | 30s |

**Timeout Behavior:**
- If a command times out, the Hub returns `504 Gateway Timeout` to the client
- The Hub does NOT automatically retry failed commands
- For `create_agent`, the Hub marks the agent as `error` with reason `timeout`
- Clients can specify custom timeouts in the command payload (up to max)

**Example with custom timeout:**
```json
{
  "type": "command",
  "id": "cmd-123",
  "command": "create_agent",
  "timeout": 180,
  "payload": { ... }
}
```

---

## 12. Rate Limiting

Rate limits are enforced per-user and per-API-key:

| Endpoint Category | Limit |
|-------------------|-------|
| Read operations | 1000/minute |
| Write operations | 100/minute |
| Agent creation | 20/minute |
| WebSocket connections | 50 concurrent |

Rate limit headers are included in responses:
```
X-RateLimit-Limit: 100
X-RateLimit-Remaining: 95
X-RateLimit-Reset: 1706097600
```

---

## 13. Versioning & Deprecation

- API versions are included in the URL path
- Deprecated endpoints return `Deprecation` header with sunset date
- Breaking changes require a new major version
- Minimum support period: 6 months after deprecation announcement

---

## 14. SDK Considerations

The API is designed to support generated SDKs with:
- Consistent naming conventions
- Predictable URL patterns
- Standard pagination
- Clear type definitions

Recommended SDK languages:
- Go (primary, for CLI and tooling)
- TypeScript (for web dashboard)
- Python (for integrations)

---

## 15. Future Considerations

1. **Batch Operations**: Bulk agent operations for efficiency
2. **Webhooks**: Outbound event notifications
3. **GraphQL**: Alternative query interface for complex clients
4. **Audit Logging**: Comprehensive activity tracking
5. **Multi-tenancy**: Organization/team isolation
6. **Usage Metrics**: Resource consumption tracking and billing

### 15.1 Alternative Transports: gRPC / HTTP/2

The current control channel uses WebSocket with a custom JSON-based protocol. A future iteration could adopt gRPC with HTTP/2 for improved efficiency and stronger contracts.

**Current Approach (WebSocket + JSON):**
- Manual JSON message envelopes with `type`, `id`, `payload`
- Custom stream multiplexing via `streamId` field
- Base64 encoding for binary data (PTY, file transfer)
- Universal browser support via native WebSocket API

**Potential gRPC/HTTP/2 Benefits:**

| Aspect | Current (WS+JSON) | gRPC/HTTP/2 |
|--------|-------------------|-------------|
| Framing | Manual JSON envelopes | Native protobuf framing |
| Multiplexing | Custom `streamId` management | Native HTTP/2 streams |
| Binary data | Base64 overhead (~33%) | Native binary transport |
| Type safety | JSON schema / manual validation | Proto definitions with codegen |
| Bidirectional streaming | Simulated via WebSocket | First-class gRPC streams |

**Migration Path:**
1. Define `.proto` files mirroring current JSON message types
2. Implement gRPC service for control channel (`HostControlService`)
3. Use gRPC bidirectional streaming for command/response flow
4. Use dedicated gRPC streams for PTY (eliminates base64 overhead)
5. For browser clients, deploy gRPC-Web proxy (Envoy) or maintain WebSocket fallback

**Trade-offs:**
- gRPC-Web requires a proxy for browser support; WebSocket is universal
- JSON is human-readable and easier to debug
- Proto file maintenance adds development overhead
- gRPC provides stronger API contracts and better tooling

### 15.2 Message Queue-Based Command Delivery

The current WebSocket control channel requires persistent connections between the Hub and Runtime Brokers. This creates challenges for horizontal scaling and reliability:

- Hub instances must maintain sticky sessions or share connection state
- Network interruptions require reconnection and state reconciliation
- Long-lived connections consume resources even when idle

**Proposed Alternative: Database-Backed Command Queue**

Instead of pushing commands over persistent WebSockets, the Hub writes commands to a database-backed queue, and Runtime Brokers poll for pending commands.

```
┌─────────────────┐                    ┌─────────────────┐
│   Scion Hub     │                    │  Runtime Broker   │
│   (Stateless)   │                    │  (behind NAT)   │
│                 │                    │                 │
│  ┌───────────┐  │    Poll (HTTP)     │                 │
│  │ Command   │◄─┼────────────────────│  Poller Loop    │
│  │ Queue     │  │                    │                 │
│  │ (DB)      │──┼───────────────────►│                 │
│  └───────────┘  │    Commands        │                 │
└─────────────────┘                    └─────────────────┘
```

**Command Queue Schema:**
```json
{
  "id": "cmd-uuid",
  "brokerId": "broker-abc",
  "command": "create_agent",
  "payload": { ... },
  "status": "pending",        // pending, delivered, acked, failed, expired
  "createdAt": "2025-01-24T10:00:00Z",
  "expiresAt": "2025-01-24T10:02:00Z",
  "deliveredAt": null,
  "ackedAt": null,
  "result": null
}
```

**Polling Endpoint:**
```
GET /api/v1/runtime-brokers/{brokerId}/commands?status=pending
```

**Acknowledgment Endpoint:**
```
POST /api/v1/runtime-brokers/{brokerId}/commands/{commandId}/ack
{
  "success": true,
  "result": { ... }
}
```

**Benefits:**
- **Horizontal scaling**: Any Hub instance can write commands; any can serve polls
- **Reliability**: Commands persist through Hub restarts and network interruptions
- **Simplicity**: No connection state management; standard HTTP request/response
- **Observability**: Command history is queryable in the database

**Limitations:**
- **Latency**: Polling interval introduces delay (e.g., 5-10 second lag)
- **Not suitable for interactive streams**: PTY attachment requires real-time bidirectional data

**Hybrid Approach for PTY:**

Interactive streams (PTY, real-time logs) cannot use polling due to latency requirements. A hybrid approach would use:

1. **Command queue for lifecycle operations**: `create_agent`, `stop_agent`, `delete_agent`, etc.
2. **On-demand WebSocket for streams**: When PTY is needed, the queue delivers a `prepare_stream` command containing a unique stream token. The Runtime Broker then initiates a short-lived WebSocket connection for that specific stream.

```
┌────────────────────────────────────────────────────────────────┐
│                      Hybrid Architecture                        │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   CRUD Operations (Polling)          Interactive Streams (WS)  │
│   ─────────────────────────          ────────────────────────  │
│                                                                 │
│   Hub ──► Queue ──► Broker          Browser ◄──► Hub ◄──► Broker│
│       (DB-backed)                         (WebSocket relay)    │
│                                                                 │
│   • create_agent                     • PTY attachment          │
│   • stop_agent                       • Live log streaming      │
│   • delete_agent                     • File sync (future)      │
│   • exec (async)                                               │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

**Stream Initiation Flow:**
1. User requests PTY attachment via Hub API
2. Hub writes `prepare_stream` command to queue with stream token
3. Runtime Broker polls, receives command, stores stream token
4. Runtime Broker connects to Hub WebSocket: `WS /api/v1/streams/{streamToken}`
5. Hub bridges the browser WebSocket to the Broker WebSocket
6. Stream closes when either side disconnects

**Open Questions:**
- Should the Broker maintain a persistent "stream-ready" WebSocket, or connect on-demand per stream?
- How to handle stream token expiration and cleanup?
- Can we use WebRTC for direct browser-to-broker PTY in some scenarios?

This hybrid approach provides the scalability benefits of polling for most operations while preserving real-time capabilities for interactive use cases.
