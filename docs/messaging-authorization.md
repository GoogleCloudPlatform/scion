# Messaging Authorization

This document describes the message mode system that controls who can deliver
messages to an agent. It covers mode definitions, the decision table, piercing
rules, the API for changing modes, and the permissions that underpin the system.

For the full design rationale and decision record, see the
design notes (internal: `msg-authz-design-notes.md`).

---

## Overview

Every agent has a **message mode** that governs its conversational reach.
The mode governs an agent's conversational reach — both who can deliver
messages to the agent and who the agent can deliver messages to. There are
four modes:

| Mode | Default | Description |
|------|---------|-------------|
| `project` | Yes | Bidirectional with all agents and users in the project. |
| `branch` | | Ancestry users + direct parent/child agents (both must be `branch` mode). |
| `lineage` | | Ancestry users only. Zero agent-to-agent edges. |
| `none` | | Sealed. No message-plane delivery except from super-admin. |

The default mode is **`project`**, which preserves pre-mode behavior: any
project member with the `agent.message` permission can message any agent.
No tightening occurs until someone explicitly sets a non-default mode.

### Key principles

1. **Not creator-private.** Lineage and branch modes are "private to lineage
   users + project owners," not creator-private. The counterparty set grows
   silently when a user is promoted to project owner. True creator-privacy
   exists only in sole-owner projects.

2. **Owner role stays rare.** Basic project usage never requires the owner or
   admin role. The default `project` mode allows any project member with
   `agent.message` to send messages. The owner role is needed only for
   accessing lineage- or branch-restricted agents.

3. **Mode is orthogonal to agent role.** Role governs sender-side capabilities
   over Hub resources; mode governs conversational reach including inbound
   delivery. All role x mode combinations are coherent.

---

## Mode Decision Table

### User to Agent

| Target mode | Condition | Result |
|-------------|-----------|--------|
| `project` | User holds `agent.message` on the project | ALLOW |
| `project` | User lacks `agent.message` | DENY |
| `branch` | User is in the agent's ancestry chain | ALLOW |
| `branch` | User is a project owner | ALLOW |
| `branch` | Otherwise | DENY |
| `lineage` | User is in the agent's ancestry chain | ALLOW |
| `lineage` | User is a project owner | ALLOW |
| `lineage` | Otherwise | DENY |
| `none` | User is super-admin | ALLOW |
| `none` | Otherwise (including project owner) | DENY |

### Agent to Agent

| Sender mode | Target mode | Condition | Result |
|-------------|-------------|-----------|--------|
| `project` | `project` | Same project | ALLOW |
| `branch` | `branch` | Direct parent/child relationship | ALLOW |
| `branch` | `branch` | Not direct parent/child | DENY |
| `lineage` | any | _(lineage agents have no agent-to-agent edges)_ | DENY |
| any | `lineage` | _(lineage agents have no agent-to-agent edges)_ | DENY |
| `none` | any | Sealed | DENY |
| any | `none` | Sealed | DENY |
| Mixed (`project`/`branch`) | | Mode mismatch | DENY |

### System to Agent

| Source | Target mode | Result |
|--------|-------------|--------|
| System plane | Any (including `none`) | ALLOW |

---

## System-Plane vs. Message-Plane Dividing Line

All messaging falls into one of two planes:

- **Message plane**: Anything relaying another principal's free text. This
  includes user messages, agent-to-agent messages, mentions, and broadcasts.
  Message-plane delivery obeys all mode checks.

- **System plane**: Hub-generated operational notices with fixed templates.
  This includes delivery failure notices, lifecycle notifications (child
  completion, state changes), and scheduled event fires. System-plane
  messages bypass all mode checks (D8).

The system-plane flag is set exclusively by hub-internal code paths. It is
**never** derived from external request data — including JWT claims — and
is **never** settable from any external ingress. Without this exemption,
`none`/`lineage`/`branch` agents would break scheduling and sub-agent
workflows.

---

## Piercing Rules

Piercing allows certain users to reach agents in restricted modes. Piercing
is evaluated on the **human principal at delivery time**.

| Principal | Pierces `project` | Pierces `branch` | Pierces `lineage` | Pierces `none` |
|-----------|:-:|:-:|:-:|:-:|
| Super-admin | Yes | Yes | Yes | Yes |
| Project owner | Yes | Yes | Yes | No |
| Ancestry user | Yes | Yes | Yes | No |
| Project member (non-owner) | Yes | No | No | No |

### Critical constraints

- **User-identity-only.** Piercing is never inherited by an owner's agents.
  If a project owner has a `project`-mode agent, that agent cannot deliver
  messages to a `lineage` or `branch` agent. This prevents relay exploits
  (U -> owner's agent -> restricted agent). Evaluated on the human principal,
  never on on-behalf-of markers.

- **UAT caveat.** For User Access Tokens, piercing applies only when the
  token also carries the `agent:message` scope. A narrow-scoped token held
  by a project owner does not pierce.

- **`none` is sealed.** Only super-admin can reach `none`-mode agents on the
  message plane. Project owners and lineage users retain `attach`/PTY access
  (mode governs only the message plane), but cannot deliver messages.

---

## Mixed Modes in a Branch

Mixed message modes within a branch are allowed. The per-edge rule (both
endpoints must be `branch` mode AND have a direct parent/child relationship)
is the sole enforcement mechanism. Key behaviors:

- **Mode mixtures only ever remove edges, never add them.** Mixing is
  fail-safe: a comprehension issue, not a security one.

- A `branch`-mode child under a `project`-mode parent **cannot** message its
  parent in either direction. Its reachable set may be surprisingly small.

- A `project`-mode agent inside a branch is denied in both directions with
  every `branch`-mode relative (bridge test). It communicates normally with
  the project cell only.

---

## Quarantine (mode=none)

Setting an agent to `none` mode immediately blocks all message-plane delivery.
This is a quarantine kill-switch independent of the agent's role.

### Quarantine behavior

- The mode change takes effect on the **next message delivery**. There is no
  grandfathering of open conversations.
- Delivery to a newly-quarantined agent fails closed. The sender receives a
  system-plane notice about the delivery failure.
- **Super-admin** can still reach quarantined agents.
- **Attach/PTY** remains available to holders of `agent.attach`. Mode governs
  only the message plane.
- **System-plane** messages (scheduled events, lifecycle notifications)
  continue to be delivered.

### Quarantining an entire branch

Use the cascade option to quarantine all descendants at once:

```json
{
    "mode": "none",
    "cascade": true
}
```

### Unquarantining

Set the mode back to `project` (or any other mode). All transitions are legal
with no preconditions:

```json
{
    "mode": "project"
}
```

---

## API Reference: set_message_mode

Changes the message mode of an agent and optionally cascades to all
descendants.

### Endpoints

```
POST /api/v1/agents/{id}/set_message_mode
POST /api/v1/projects/{pid}/agents/{aid}/action/set_message_mode
```

### Request

```json
{
    "mode": "none|lineage|branch|project",
    "cascade": false
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `mode` | string | Yes | One of: `none`, `lineage`, `branch`, `project` |
| `cascade` | bool | No | If true, apply the mode to all descendants of this agent |

### Response

```json
{
    "agent_id": "abc123",
    "mode": "none",
    "previous_mode": "project",
    "cascade": {
        "count": 3,
        "agent_ids": ["def456", "ghi789", "jkl012"]
    }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `agent_id` | string | The target agent's ID |
| `mode` | string | The new message mode |
| `previous_mode` | string | The mode before this change |
| `cascade` | object | Present only when `cascade: true` was requested |
| `cascade.count` | int | Number of descendants whose mode was updated |
| `cascade.agent_ids` | string[] | IDs of the updated descendants |

### Authorization

| Caller | Result | Rationale |
|--------|--------|-----------|
| Super-admin | ALLOWED | |
| Project owner | ALLOWED | |
| Lineage owner (user in agent's ancestry) | ALLOWED | |
| Project admin (non-owner) | DENIED | D7: admin cannot unseal `none` agents |
| Agent callers | DENIED | D7: human-only operation |
| UATs (any scope) | DENIED | D7: no UAT scope exists for this action |

### Semantics

- **Live effect.** Mode is read from the agent record at delivery time. A
  change applies to the next message; there is no grandfathering.
- **Every transition is legal.** No preconditions, no cascade requirement.
  Mixed modes are allowed everywhere.
- **Cascade is best-effort.** Each descendant is updated independently; a
  failure to update one descendant does not stop the rest. One audit event
  is emitted per affected agent.
- **Spawn defaults.** When a child agent is created, its mode defaults to the
  parent's mode. Templates may override to any mode.
- **Audit.** Every mode change emits an audit record: actor, agent,
  from-mode, to-mode, timestamp.

---

## Permission Reference

### agent.message

| Field | Value |
|-------|-------|
| Permission ID | `agent.message` |
| Resource | `agent` |
| Action | `message` |
| Capability Kind | Scope (project-wide) |
| Description | Send messages to agents |
| UAT Scope | `agent:message` |
| Default Role | Project member |

This is the permission that gates user-to-agent messaging for `project`-mode
agents. It is project-scoped (not per-agent) because the relay rule makes
per-agent granularity dishonest: any agent could be asked to relay a message
across a per-agent boundary.

### agent.set_message_mode

| Field | Value |
|-------|-------|
| Permission ID | `agent.set_message_mode` |
| Resource | `agent` |
| Action | `set_message_mode` |
| Capability Kind | Resource (per-agent) |
| Description | Change agent message mode |
| UAT Scope | _(none)_ |
| Agent Scopes | _(none)_ |
| Default Role | Project owner only (explicitly excluded from project admin) |

This permission is intentionally restricted:

- **No agent scope** because mode changes extend authority and must fail
  closed. Agents cannot hold this permission.
- **No UAT scope** because bearer tokens cannot unseal agents.
- **Excluded from project admin** because folding mode changes into the admin
  role would let admins unseal `none`-mode agents, breaking the quarantine
  boundary.
- **Distinct from `agent.update`** because project admins hold `agent.update`;
  if mode changes were folded into update, admins could unseal agents.

---

## Design Decisions Reference

The messaging authorization system is governed by ten design decisions
(D1-D10) ratified by the project sponsor. Key decisions:

| ID | Summary |
|----|---------|
| D1 | `message` is a first-class axis, split from lifecycle/attach |
| D2 | User-side messaging grant is project-coarse (relay rule) |
| D3+D9 | Four-tier mode system: none, lineage, branch, project |
| D4 | Lineage mode: strict user-to-agent only, no agent-to-agent edges |
| D5 | Mode is fully orthogonal to agent role |
| D6 | Piercing rules: super-admin pierces all; owner/ancestry pierce lineage/branch; user-identity-only |
| D7 | Mode changes are human-only, no agent scope, no UAT scope |
| D8 | System plane exempt from all mode checks |
| D9 | Branch mode uses 1-degree parent/child edges; relay closure = branch cell |
| D10 | Modes are mutable; mutation is foundational to the design |

For the full decision record with rationale, see the
design notes (internal: `msg-authz-design-notes.md`).

---

## Source Files

| Concept | File |
|---------|------|
| Mode decision logic | `pkg/hub/authorize_message.go` |
| set_message_mode handler | `pkg/hub/handlers_agent_message_mode.go` |
| Permission registry | `pkg/hub/permissions/registry.go` |
| Role seeds (admin exclusion) | `pkg/hub/seed.go` |
| MessageMode constants | `pkg/store/models.go` |
