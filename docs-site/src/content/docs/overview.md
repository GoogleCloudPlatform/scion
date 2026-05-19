---
title: Scion Overview
---

Scion is an experimental multi-agent orchestration testbed designed to manage concurrent LLM-based agents running in containers across your local machine and remote clusters. It enables developers to run groups of specialized agents with isolated identities, credentials, and workspaces, allowing for a dynamic and evolving graph of parallel execution of tasks such as research, coding, auditing, and testing.

## Configuration

Scion uses a flexible configuration system based on **Profiles**, **Runtimes**, and **Harnesses**. This allows you to define different environments (e.g., local Docker vs. remote Kubernetes) and switch between them easily.

- **Global Settings**: `~/.scion/settings.yaml` (User-wide defaults)
- **Project Settings**: `.scion/settings.yaml` (Project overrides)

For detailed information on configuring Scion, see the [Orchestrator Settings Reference](/scion/reference/orchestrator-settings/) and [Agent Configuration Reference](/scion/reference/agent-config/).
To learn about the different agent tools supported by Scion, see [Supported Harnesses](/scion/supported-harnesses/).

## Getting Started

Scion is designed to be easy to start with.

1.  **Install**: Follow the [Installation Guide](/scion/getting-started/install/) to get Scion on your machine.
2.  **Initialize**: Run `scion init` in your project root to create a `.scion` directory.
3.  **Start an Agent**: Use `scion start <agent-name> "<task>"` to launch an agent.
4.  **Interact**: Use `scion attach <agent-name>` to interact with the agent's session, or `scion logs <agent-name>` to view its output.
5.  **Resume**: Use `scion resume <agent-name>` to restart a stopped agent, preserving its state.

## Architecture

Scion follows a Manager-Worker architecture:

```d2
User -> Scion CLI: Start Agent
Scion CLI -> Runtime Broker: Provision Container
Runtime Broker -> Agent Container: Execute Task
Agent Container -> Scion CLI: Progress Updates
```

- **scion**: A host-side CLI that orchestrates the lifecycle of agents. It manages the "Project" (the project workspace) and provides tools for template management (`scion templates`).
- **Agents**: Isolated runtime containers (e.g., Docker) running the agent software (like Gemini CLI, Claude Code, or OpenAI Codex).
