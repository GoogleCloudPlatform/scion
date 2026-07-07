---
title: Managed Agents
description: Running cloud-managed agents that bypass the Runtime Broker and container layer entirely.
---

**What you will learn**: What a managed agent is, when to use one instead of a containerized
agent, how to enable the backend on your Hub, and how the day-to-day CLI differs.

:::note[Applies to any hosted tier]
Managed agents are a Hub feature and work on **any** hosted deployment — Workstation, Single-node,
or HA. They are documented here in the Single-node section because they need the least
infrastructure of all, but the HA Admin guide links to this same page.
:::

## What is a managed agent?

A **managed agent** is an agent whose lifecycle is managed directly by the Hub through a cloud
provider's agent API, **bypassing the Runtime Broker and container layer entirely**. Instead of
Scion provisioning a container, mounting a workspace, and running a harness (Claude Code, Gemini
CLI, …) inside it, the model, tools, sandbox, and orchestration loop all run server-side in the
cloud service.

The first supported backend is **Google Managed Agents** (the Gemini API at
`generativelanguage.googleapis.com`), with the Antigravity agent as the base agent.

Managed agents implement the same `Manager` interface and agent-label system as containerized
agents, so `scion start`, `scion message`, `scion stop`, `scion list`, and friends work the same
way. What changes is that a managed agent has:

- **No container** — nothing is provisioned on a runtime broker.
- **No workspace mount** — v1 targets repo-less use cases (research, exploration, standalone
  tasks). Repo-aware workflows (workspace sync, worktree branching) are a future addition.
- **No broker involvement** — API calls go directly from the Hub to the cloud provider.

See the [Glossary](/scion/glossary/) for the canonical definition and how it relates to the
[Runtime Broker](/scion/hosted/ha/runtime-broker/) taxonomy.

## When to use a managed agent

Choose a **managed agent** when:

- You want a repo-less agent for research, exploration, or a standalone task.
- You do not want to run or pay for a Runtime Broker and container infrastructure.
- You want the cloud provider to own the sandbox and orchestration loop.

Choose a **containerized (brokered) agent** when:

- The agent needs a workspace — a git checkout, worktree, or shared directory.
- You need a specific harness (Claude Code, Gemini CLI, Codex, OpenCode).
- You need interactive terminal attach, suspend/resume, or other container-only operations
  (see [Limitations](#limitations) below).

The choice between a managed agent and a brokered agent is a **deployment-time decision
controlled by a broker profile**, not a property of the agent template. The same template can run
on a container runtime or on a managed service depending on the profile you select at
`scion start` time.

## Enabling the backend (admin)

Managed agent backend configuration — provider, base agent, and API key — lives in **Scion
settings on the Hub**, not in templates. Add a `managed_agents` section:

```yaml
# In the Hub's settings (e.g. ~/.scion/settings.yaml), not template YAML
managed_agents:
  google:
    api_key: "<key>"                         # Or resolved from Hub secrets
    base_agent: "antigravity-preview-05-2026"
    model: "<optional-model-override>"       # Optional
```

| Field | Meaning |
|-------|---------|
| `api_key` | API key for the Google Managed Agents (Gemini) API. Required. Can also be resolved from Hub secrets rather than written in plaintext. |
| `base_agent` | The base agent identifier (e.g. `antigravity-preview-05-2026`). |
| `model` | Optional model override. |

Keeping the credential in settings (or Hub secrets) rather than in templates keeps templates
portable and credential-free.

## Selecting a managed agent (user)

The execution mode is chosen with the `--profile` flag at agent creation. Use the
`managed-agents` profile to route the agent to the Google Managed Agents backend:

```bash
scion start my-researcher --profile managed-agents "Summarize the latest RFCs on X"
```

When the profile is `managed-agents`, the Hub bypasses broker dispatch entirely and manages the
agent directly through the cloud API. The template is the same regardless of profile — swapping
the profile is all it takes to move a task between a container and a managed service.

The following commands work with managed agents:

| Command | Behavior |
|---------|----------|
| `scion start` / `scion create` | Create (and start) a managed agent. |
| `scion message <name> "…"` | Send a message — begins a new interaction with the cloud agent. |
| `scion look <name>` | View the agent's status and latest output (step types and token usage). |
| `scion stop <name>` | Cancel the running interaction. |
| `scion delete <name>` | Delete the agent and its cloud-side resources. |
| `scion list` | List agents; managed agents show a `managed:google` runtime. |
| `scion logs <name>` | Read logs from the cloud provider (GCP Cloud Logging), not local files. |

## Limitations

Because there is no container, some container-oriented operations are not available for managed
agents and return a clear error:

- **`scion attach`** — not supported (there is no tmux session). Use `scion message` and
  `scion look` instead.
- **`scion suspend`** — not supported; use `scion stop` instead.
- **`scion message --raw`** — not supported (raw tmux key delivery has no meaning without a
  container).
- **Workspace mounting** — no local workspace or file sync in v1; managed agents target
  repo-less tasks.

## How it works

Managed agents are a peer concept to the Runtime + Harness stack, not a replacement. The
`Manager` interface is the branching point: a `ManagedAgentManager` implements the same interface
as the container-based `AgentManager` but delegates to a cloud API client. As a result, the CLI
and Hub talk to one interface and do not need to know whether an agent is containerized or
managed.

State for a managed agent (cloud-side identifiers and the interaction chain) is persisted locally
so the CLI can reconnect across restarts, but no container or broker is involved at any point.

## See also

- [Runtime Brokers & Profiles](/scion/hosted/ha/runtime-broker/) — the broker layer that managed
  agents bypass, and how profiles are defined.
- [Glossary: Managed Agent](/scion/glossary/) — the canonical definition.
- [Choosing a Mode](/scion/choosing-a-mode/) — where hosted deployments fit.
