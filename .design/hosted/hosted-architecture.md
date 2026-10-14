# Hosted Scion Architecture Overview

> **Note (local-only mode removed):** this document describes the CLI-only mode with no Hub ("solo" or "local" mode) as current. That mode was removed; every agent now runs through a Hub (the workstation server on a single machine). See `docs-site/src/content/docs/reference/local-mode-removal.md`.

**Status:** Living Document
**Updated:** 2026-02-08

This document provides a high-level overview of the Scion hosted architecture, a distributed platform designed to manage concurrent LLM-based code agents across diverse runtime environments.

## 1. Core Philosophy

The primary goal of the hosted architecture is to separate **State Management** (centralized metadata and coordination) from **Runtime Execution** (isolated container orchestration). This allows Scion to scale from a single developer's laptop to a global fleet of agents running in multiple cloud regions and on-premise clusters.

### Key Architectural Principles

*   **Grove-Centricity**: The **Grove** (Project) is the fundamental unit of registration and identity, not the Runtime Broker. A grove is uniquely identified by its Git remote URL.
*   **Location Transparency**: Users interact with the Hub API; the platform handles routing commands to the appropriate Runtime Broker regardless of where it is running.
*   **NAT Traversal**: Runtime Brokers establish persistent WebSocket connections to the Hub, allowing them to receive commands even when behind firewalls or NAT.
*   **Direct-to-Storage Sync**: Large data transfers (workspaces, templates) bypass the Hub API and occur directly between clients/brokers and cloud storage using signed URLs.
*   **Multi-Context Security**: Distinct authentication mechanisms for Users (OAuth), Infrastructure (HMAC), and Agents (JWT).

---

## 2. High-Level Architecture

```mermaid
graph TD
    User[User (CLI/Web)] -->|HTTPS/WS| Hub[Scion Hub (State Server)]
    
    subgraph "Control Plane"
        Hub -->|DB| DB[(SQLite/Postgres)]
        Hub -->|Events| EventBus((Event Bus))
        %% 2026-02-19: NATS abandoned. Events use in-process channels.
        %% See web-realtime.md for current design.
    end

    subgraph "Cloud Storage"
        Bucket[(GCS/S3 Bucket)]
    end

    subgraph "Runtime Broker A (Docker/Mac)"
        BrokerA[Broker Agent] -->|WS Control Channel| Hub
        BrokerA -->|Agents| AgentA[Agent Container]
    end

    subgraph "Runtime Broker B (Kubernetes)"
        BrokerB[Broker Agent] -->|WS Control Channel| Hub
        BrokerB -->|Agents| PodB[Agent Pod]
    end

    User <-->|Direct Signed URL| Bucket
    BrokerA <-->|Direct Signed URL| Bucket
    BrokerB <-->|Direct Signed URL| Bucket
    
    AgentA -.->|Status Updates (JWT)| Hub
    PodB -.->|Status Updates (JWT)| Hub
```

---

## 3. Core Components

### 3.1 Scion Hub (State Server)
The centralized authority and coordination point.
*   **Registry**: Maintains the canonical registry of Groves, Agents, Templates, and Users.
*   **Router**: Dispatches lifecycle commands to the appropriate Runtime Broker.
*   **Relay**: Proxies interactive PTY sessions and streams events via WebSockets.
*   **Coordinator**: Manages signed URL generation for storage operations.

### 3.2 Grove (The Identity Unit)
A Grove represents a project or workspace, typically backed by a Git repository.
*   **Identity**: Uniquely identified by normalized Git remote URLs.
*   **Multi-Provider**: A single Grove can be served by multiple Runtime Brokers (e.g., a team sharing a project).
*   **Config**: Stores project-specific settings, profiles, and environment variables.

### 3.3 Runtime Broker (Compute Node)
An execution host (Developer laptop, VM, or K8s cluster) that runs agents.
*   **Agent Lifecycle**: Provisions, starts, stops, and deletes agent containers.
*   **Control Channel**: Maintains a persistent WebSocket to the Hub for receiving tunneled HTTP requests.
*   **Template Hydration**: Manages a local cache of agent templates fetched from cloud storage.
*   **Workspace Management**: Handles Git worktrees and snapshotting for agent isolation.

### 3.4 sciontool (Agent Helper)
A binary running inside every agent container.
*   **Heartbeat**: Reports real-time status (THINKING, EXECUTING, etc.) to the Hub using JWT auth.
*   **Observability**: Collects and forwards OTel-compatible traces, logs, and metrics to cloud backends.
*   **Identity**: Injected with unique Agent and Grove identifiers.

---

## 4. Architectural Pillars

### 4.1 Communication & NAT Traversal
Runtime Brokers initiate a persistent **WebSocket Control Channel** to the Hub. This channel tunnels standard HTTP requests from the Hub to the Broker's internal API, enabling the Hub to "dial" brokers that lack public IP addresses.

### 4.2 Storage & Incremental Sync
To handle large agent workspaces efficiently, Scion uses a **Direct-to-Storage** pattern:
1.  **Metadata**: The Hub manages manifests and content hashes.
2.  **Transfer**: The CLI and Runtime Brokers upload/download files directly to Cloud Storage (GCS) using short-lived **Signed URLs**.
3.  **Optimization**: Incremental sync skips unchanged files by comparing content hashes, significantly reducing bandwidth for large repositories.

### 4.3 Unified Authentication
Scion uses a unified middleware stack to handle multiple identity contexts:
*   **User Auth**: OAuth 2.0 (Google/GitHub) with Device Flow for CLI and Session Cookies for Web.
*   **Infrastructure Auth**: HMAC-based request signing for bidirectional trust between Hub and Brokers.
*   **Agent Auth**: Scoped, short-lived JWT tokens issued by the Hub during provisioning.

### 4.4 Environment and Secret Management
The Hub provides centralized management of environment variables and secrets, resolved at agent startup using a hierarchical scope system:
1.  **User Scope** (Lowest priority)
2.  **Grove Scope**
3.  **Runtime Broker Scope**
4.  **Agent Config** (Highest priority overrides)

Secrets are write-only metadata in the Hub API and are decrypted only during the provisioning phase when dispatched to a Broker.

---

## 5. Key Workflows

### 5.1 Grove Registration
When a developer runs `scion hub register` or starts a local broker, the broker registers its local Groves with the Hub. If the Git remote is already known, the broker is added as a "Provider" to the existing Grove.

### 5.2 Agent Provisioning
1.  **Request**: User sends a creation request to the Hub.
2.  **Resolution**: Hub selects an online Broker for the Grove (default or explicit).
3.  **Command**: Hub sends a `CreateAgent` command over the WebSocket Control Channel.
4.  **Execution**: Broker pulls the template, creates a Git worktree, and starts the container.
5.  **Activation**: sciontool inside the container reports a `RUNNING` status to the Hub.

### 5.3 Interactive Attachment (PTY)
Users connect to the Hub via WebSocket. The Hub locates the Broker running the agent and opens a multiplexed stream over the existing Control Channel. The Broker then attaches to the agent's `tmux` session, relaying terminal I/O in real-time.

---

## 6. Observability

Scion leverages OpenTelemetry (OTel) for system-wide observability:
*   **Metrics**: Token usage, tool execution counts, and API latency are reported to the Hub and cloud backends.
*   **Tracing**: Agent task execution is captured as traces, allowing users to visualize the "thought process" and tool usage hierarchy.
*   **Logging**: System logs from Hub and Brokers are bridged to OTLP for centralized analysis.

---

## 7. Operational Modes

### 7.1 Solo Mode (Standalone)
The `scion` CLI acts as its own Hub and Runtime Broker using local SQLite state and Docker. No external connectivity is required.

### 7.2 Hosted/Hybrid Mode
The CLI connects to a remote Scion Hub. Operations are routed to cloud Runtime Brokers or local providers depending on the Grove configuration.

### 7.3 Multi-Broker/Team Mode
A single Hub coordinates multiple developers and cloud clusters. Teams can share Groves, templates, and secrets, providing a unified collaborative environment for AI agents.
