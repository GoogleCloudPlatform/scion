---
title: Configuration Overview
description: Guide to the Scion configuration file ecosystem.
---

Scion uses a multi-layered configuration system to manage orchestrator behavior, agent execution, and server operations.

## Key Configuration Files

| File | Purpose | Scope | Reference |
| :--- | :--- | :--- | :--- |
| `settings.yaml` | **Orchestrator Settings**. Defines profiles, runtimes, and harness configurations. | Global (`~`) or Project (`.scion`) | [Orchestrator Settings](/scion/reference/orchestrator-settings/) |
| `scion-agent.yaml` | **Agent Blueprint**. Defines the configuration for a specific agent or template. | Template or Agent | [Agent Configuration](/scion/reference/agent-config/) |
| `state.yaml` | **Runtime State**. Tracks system state like sync timestamps. | Project (`.scion`) | N/A (Managed by Scion) |

## Server Configuration

Server configuration (for Hub and Runtime Broker) is now integrated into `settings.yaml` under the `server` key.

- [Server Configuration Reference](/scion/reference/server-config/)

## Telemetry Configuration

Telemetry settings control agent observability — trace collection, cloud forwarding, privacy filtering, and debug output. These are configured via the `telemetry` block in `settings.yaml` and can be overridden per-template or per-agent in `scion-agent.yaml`.

- [Orchestrator Settings — Telemetry](/scion/reference/orchestrator-settings/#telemetry-configuration-telemetry)
- [Metrics & OpenTelemetry Guide](/scion/hub-admin/metrics/)

## Project Settings

In a Hub-managed architecture, Project Settings are maintained by the Hub database and managed via the Web Dashboard or API, rather than a local file. These settings define constraints and capabilities for agents operating within a specific project.

Key configuration areas include:

- **General Settings**: The project's description, default branch, and external Git repository URLs for template synchronization.
- **Agent Limits**: Defines maximum resource constraints for agents in the project, including maximum concurrency, runtime duration limits, and maximum workspace storage. These values pre-populate the agent creation form.
- **Resources & Plugins**: Defines authorized Runtime Brokers and configures Message Broker plugins for the project.

When a project is exported or managed locally in a standalone environment, some of these settings may be serialized into `.scion/state.yaml` or related project files.

## Configuration Hierarchy

Scion resolves settings in the following order (highest priority first):

1.  **CLI Flags**: (e.g., `scion start --profile remote`)
2.  **Environment Variables**: `SCION_*` overrides.
3.  **Project Settings**: `.scion/settings.yaml` (Project level).
4.  **Global Settings**: `~/.scion/settings.yaml` (User level).
5.  **Defaults**: System built-ins.

## Migration

To migrate legacy configuration files to the new schema v1 format:

```bash
# Migrate general settings
scion config migrate

# Migrate server.yaml to settings.yaml
scion config migrate --server
```
