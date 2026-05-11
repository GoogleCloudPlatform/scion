---
title: API Reference
description: Hub and Runtime Broker REST/WebSocket specifications.
---

The Scion ecosystem exposes several APIs for coordination, management, and observability. This reference provides an overview of the primary resource types and communication patterns.

## Hub API

The Scion Hub provides a RESTful API (mostly JSON) for managing the state of the system.

### Authentication
Most endpoints require a `Bearer` token in the `Authorization` header.
- **User Tokens**: Obtained via OAuth or Dev Auth.
- **Agent Tokens**: Issued to agents at startup for state reporting.
- **Broker Tokens**: Used for broker-to-hub communication, often combined with HMAC request signing.

### Core Resources

#### Agents (`/api/v1/agents`)
- `GET /`: List agents (filterable by project, user, phase).
- `POST /`: Dispatch a new agent.
- `GET /:id`: Get detailed agent state (phase, activity, detail).
- `DELETE /:id`: Stop and remove an agent.
- `GET /:id/logs`: Stream agent logs (WebSocket).

Agent state uses a layered model:
- **Phase**: Lifecycle stage (`created`, `provisioning`, `cloning`, `running`, `stopped`, `error`).
- **Activity**: Runtime activity within the `running` phase (`idle`, `thinking`, `executing`, `waiting_for_input`, `completed`, `limits_exceeded`, `offline`). Note: `offline` occurs when an agent heartbeat has not been heard for some time, often due to an expired auth token that the agent failed to refresh.
- **Detail**: Freeform context (tool name, message, task summary).

#### Projects (`/api/v1/projects`)
- `GET /`: List projects you have access to.
- `POST /register`: Register or link a project repository.
- `GET /:id`: Get project metadata and statistics.
- `GET /:id/secrets`: Manage environment secrets for the project.

#### Runtime Brokers (`/api/v1/brokers`)
- `GET /`: List registered runtime brokers.
- `POST /register`: Register a new compute node.
- `POST /join`: Complete the two-phase broker registration.
- `GET /:id`: Get broker status and capacity.

#### Templates (`/api/v1/templates`)
- `GET /`: List available agent templates.
- `POST /`: Upload a new template or version.

## Runtime Broker API

The Runtime Broker exposes a local API (usually on port 9800) for agent execution and management.

### Control Channel (WebSocket)
Brokers maintain a persistent outbound WebSocket connection to the Hub. The Hub uses this tunnel to send commands (e.g., `CreateAgent`) to brokers that might be behind NAT.

### Local Endpoints
- `GET /healthz`: Basic liveness and readiness check.
- `POST /api/v1/agents`: (Internal) The Hub dispatches agents to this endpoint.
- `GET /api/v1/agents/:id/attach`: (WebSocket) Provides a terminal stream for interactive sessions.

## Communication Patterns

### State Reporting
Agents use the `sciontool` utility to report their state back to the Hub via the `POST /api/v1/agents/:id/status` endpoint. State updates include the agent's current phase, activity, and contextual detail (e.g., which tool is executing). This happens at high frequency during task execution.

### Log Streaming
Logs are collected by the Runtime Broker and can be streamed in two ways:
1. **Real-time**: Streamed via WebSocket from the Broker to the Hub, then to the Dashboard/CLI.
2. **Persistent**: Batched and uploaded to a storage backend (like GCS) after agent completion.