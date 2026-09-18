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
five modes:

| Mode | Default | Description |
|------|---------|-------------|
| `project` | Yes | Bidirectional with all agents and users in the project. |
| `hub` | | Project cell + cross-project DM delivery to eligible recipients on the same Hub. |
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

## Cross-Project Agent Messaging

Cross-project agent messaging allows agents in different projects on the
same Hub to exchange direct messages. The feature is **off by default** and
governed by three independent controls, all of which must permit a message
for delivery to succeed.

### Three controls

| Control | Field | Default | Who changes it |
|---------|-------|---------|----------------|
| Hub availability | `cross_project_messaging_enabled` (boolean) in Hub messaging settings | `false` | Local, unscoped Hub administrator |
| Agent outbound reach | Agent `messageMode` set to `hub` | `project` | Existing managers, subject to the grant guard below |
| Receiving project | `crossProjectInbound` on the destination project | `none` | Active direct project owner or local, unscoped Hub administrator |

The **receiving project's inbound policy** is directional:

| Policy | Meaning |
|--------|---------|
| `none` | Accept no agent messages from other projects. |
| `members` | Accept an external agent only when its Hub-attested originating human is currently an active member of this receiving project. |
| `any` | Accept an eligible agent from any project on this Hub. |

All policies require the external **sender** to use `hub` mode. The
recipient may use `project` or `hub` mode. A send can succeed while the
reverse reply is denied. Receiving a message never grants authority to reply.

A `project`-mode recipient can receive and read an authorized cross-project
DM but cannot reply across projects until an authorized actor grants it
`hub` mode.

### What cross-project messaging does not grant

- No general membership in the other project.
- No access to the other project's agents, files, settings, or resources.
- No cross-Hub communication (that remains on A2A/OIDC).
- No project group, broadcast, or plugin channel access across projects.
- The `any` policy does not open `none`, `lineage`, or `branch` recipients.

### Scope

The first release supports cross-project agent DMs, explicitly addressed
DM fan-out, and scheduled direct messages. Project-owned group
conversations, native topics, project broadcasts, and plugin channels
retain their current project boundaries. Foreign project rooms are
unsupported in the first release; group support is an intended future
extension.

---

## Mode Decision Table

### User to Agent

| Target mode | Condition | Result |
|-------------|-----------|--------|
| `project` | User holds `agent.message` on the project | ALLOW |
| `project` | User lacks `agent.message` | DENY |
| `hub` | User holds `agent.message` on the project | ALLOW |
| `hub` | User lacks `agent.message` | DENY |
| `branch` | User is in the agent's ancestry chain | ALLOW |
| `branch` | User is a project owner | ALLOW |
| `branch` | Otherwise | DENY |
| `lineage` | User is in the agent's ancestry chain | ALLOW |
| `lineage` | User is a project owner | ALLOW |
| `lineage` | Otherwise | DENY |
| `none` | User is super-admin | ALLOW |
| `none` | Otherwise (including project owner) | DENY |

For human-to-agent delivery, `hub` behaves identically to `project`. The
`hub` mode grants no new right to message unrelated humans.

### Agent to Agent (same project)

| Sender mode | Target mode | Condition | Result |
|-------------|-------------|-----------|--------|
| `project` | `project` | Same project | ALLOW |
| `project` | `hub` | Same project | ALLOW |
| `hub` | `project` | Same project | ALLOW |
| `hub` | `hub` | Same project | ALLOW |
| `branch` | `branch` | Direct parent/child relationship | ALLOW |
| `branch` | `branch` | Not direct parent/child | DENY |
| `lineage` | any | _(lineage agents have no agent-to-agent edges)_ | DENY |
| any | `lineage` | _(lineage agents have no agent-to-agent edges)_ | DENY |
| `none` | any | Sealed | DENY |
| any | `none` | Sealed | DENY |
| Mixed (`project`/`branch`) | | Mode mismatch | DENY |

Within the same project, `hub` behaves identically to `project` — it joins
the same communication cell. The `hub` mode only gains additional
cross-project reach described below.

### Agent to Agent (cross-project)

For agents in **different** projects, only `hub`-mode senders may attempt
delivery. All gates must pass:

| Gate | Check | Denial code |
|------|-------|-------------|
| Hub enabled | `cross_project_messaging_enabled` is `true` | `cross_project_disabled` |
| Sender mode | Sender must be `hub` | `cross_project_sender_mode` |
| Target mode | Target must be `project` or `hub` | `cross_project_target_mode` |
| Inbound policy | Destination project's `crossProjectInbound` permits sender | `cross_project_inbound_none` |
| Origin trust | Sender's Hub-attested ancestry is valid | `cross_project_untrusted_origin` |
| Membership (if `members`) | Origin human is an active member of destination project | `cross_project_origin_not_member` |

A `project`-mode sender cannot send cross-project messages, even to reply
in an existing cross-project DM.

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

| Principal | Pierces `project` | Pierces `hub` | Pierces `branch` | Pierces `lineage` | Pierces `none` |
|-----------|:-:|:-:|:-:|:-:|:-:|
| Super-admin | Yes | Yes | Yes | Yes | Yes |
| Project owner | Yes | Yes | Yes | Yes | No |
| Ancestry user | Yes | Yes | Yes | Yes | No |
| Project member (non-owner) | Yes | Yes | No | No | No |

`hub` mode follows the same piercing rules as `project` mode.

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

## Hub Mode Grant Guard

Granting `hub` mode is subject to a non-escalation rule that prevents
unauthorized agents from widening their own or others' messaging reach.

### Agent callers

An agent may grant `hub` mode to another agent only when **all** of these
conditions are met:

1. The calling agent is **full-role** (checked from its stored record).
2. The calling agent is **already in `hub` mode** (checked from its stored record).
3. The calling agent holds the required `project:agent:set_message_mode` scope.
4. The target agent is in the **same project** as the caller.

A full-role `project`-mode agent cannot grant `hub` to itself, a peer, or
a child. A `hub`-mode agent without full role cannot grant it either. This
applies to all paths that change effective mode: explicit mode changes,
template resolution, parent inheritance, default/reset resolution,
cascades, and dry-run previews.

### Human callers

Human callers with existing `set_message_mode` authorization (project
owners, super-admins, lineage owners) can seed `hub` mode on agents. This
human-authorized seed is needed to establish the first `hub`-mode agent in
a project.

### Behavior when Hub is disabled

Mode values may be configured while the Hub feature is off. The API/UI
report that the stored mode is inactive for external messaging. With the
switch off, a `hub` agent retains its same-project `project` behavior.
Enabling the Hub switch activates the stored modes without requiring
reconfiguration.

---

## Cross-Project DM Read Access

Cross-project conversation history is accessible to both endpoints when:

1. Both agents have valid, non-deleted records.
2. The Hub feature is enabled.
3. Both endpoints are in `project` or `hub` mode, with at least one in `hub`.
4. At least one permitted sending direction exists for the pair.

This means a `project`-mode recipient can list and read its incoming
cross-project DM even though it cannot reply. A hub-to-project downgrade
alone does not close reads if the reverse hub-to-project edge still exists.

**Hub disable** closes all cross-project agent history/list/resolve/stream
access. Stored messages and authorized human audit views are retained.

**Project policy changes** govern new incoming content. Previously accepted
history remains readable while the other direction is still allowed.

---

## Cross-Project Denial Codes

When a cross-project message is denied, the server returns a stable,
machine-readable denial code:

| Code | Meaning |
|------|---------|
| `cross_project_disabled` | Cross-project messaging is disabled by the Hub administrator. |
| `cross_project_sender_mode` | The sender must be in `hub` mode to send across projects. |
| `cross_project_target_mode` | The recipient must be in `project` or `hub` mode. |
| `cross_project_inbound_none` | The recipient's project does not accept external agent messages. |
| `cross_project_origin_not_member` | The sender's originating user is not an active member of the recipient's project. |
| `cross_project_untrusted_origin` | The sender's identity origin could not be verified (missing ancestry, non-human root, federated identity, deleted/disabled user). |
| `cross_project_surface_unsupported` | Cross-project messaging is not supported for this conversation type (e.g., group rooms). |

Denial codes are returned in the `MessageDecision.Code` field and in API
error responses. The UI maps these codes to user-visible explanations.

---

## API Reference: Project Messaging Policy

### Endpoints

```
GET  /api/v1/projects/{id}/messaging-policy
PUT  /api/v1/projects/{id}/messaging-policy
```

### GET Response

```json
{
    "crossProjectInbound": "none",
    "revision": 1,
    "effectiveCrossProjectInbound": "none",
    "hubCrossProjectEnabled": false,
    "capabilities": {
        "crossProjectConversationKinds": ["direct"]
    }
}
```

### PUT Request

```json
{
    "crossProjectInbound": "members",
    "expectedRevision": 1
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `crossProjectInbound` | string | Yes | One of: `none`, `members`, `any` |
| `expectedRevision` | int64 | Yes | CAS revision for optimistic concurrency; returns 409 on conflict |

### Authorization

| Caller | Result |
|--------|--------|
| Active direct project owner | ALLOWED |
| Local, unscoped Hub administrator | ALLOWED |
| Project admin (non-owner) | DENIED |
| All other callers | DENIED |

---

## API Reference: Messaging Capabilities

### Endpoint

```
GET /api/v1/messaging/capabilities
```

Returns the Hub's current cross-project messaging capabilities without
exposing settings or topology.

### Response

```json
{
    "hubEnabled": true,
    "supportedModes": ["none", "lineage", "branch", "project", "hub"],
    "crossProjectConversationKinds": ["direct"]
}
```

---

## API Reference: Target Resolution

### Endpoint

```
GET /api/v1/messaging/targets/resolve?project=<id-or-slug>&agent=<id-or-slug>
```

Read-only exact target lookup with privacy-preserving 404 responses.
Returns minimal identity and directional reachability without requiring
broad foreign project read access.

### Response

```json
{
    "agent": {
        "id": "<uuid>",
        "slug": "reviewer",
        "projectId": "<uuid>",
        "projectSlug": "tools"
    },
    "messageability": {
        "canMessage": true,
        "canReachViewer": false,
        "replyReason": "cross_project_inbound_none"
    }
}
```

Undisclosed and nonexistent targets return indistinguishable 404 responses.

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
| `mode` | string | Yes | One of: `none`, `lineage`, `branch`, `project`, `hub` |
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
| Full-role agent (same project) | ALLOWED | D7 amendment: full role includes `set_message_mode` scope |
| Agent callers (non-full role) | DENIED | D7: insufficient role |
| Project admin (non-owner) | DENIED | D7: admin cannot unseal `none` agents |
| UATs (any scope) | DENIED | D7: no UAT scope exists for this action |

**Hub mode grant guard.** When the requested mode is `hub`, agent callers
must additionally be currently full-role **and** already in `hub` mode. A
full/project agent cannot grant `hub`. See [Hub Mode Grant Guard](#hub-mode-grant-guard)
for details.

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
| Agent Scopes | `project:agent:set_message_mode` |
| Default Role | Project owner only (explicitly excluded from project admin) |

This permission is intentionally restricted:

- **Agent scope** (`project:agent:set_message_mode`) is granted to full-role
  agents only. Full-role agents can change message mode for any agent within
  the same project. Agents with lesser roles cannot hold this scope.
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
| D3+D9 | Five-tier mode system: none, lineage, branch, project, hub |
| D4 | Lineage mode: strict user-to-agent only, no agent-to-agent edges |
| D5 | Mode is fully orthogonal to agent role |
| D6 | Piercing rules: super-admin pierces all; owner/ancestry pierce lineage/branch; user-identity-only |
| D7 | Mode changes restricted to human users and full-role agents; no UAT scope |
| D8 | System plane exempt from all mode checks |
| D9 | Branch mode uses 1-degree parent/child edges; relay closure = branch cell |
| D10 | Modes are mutable; mutation is foundational to the design |
| D11 | Hub mode: project cell + cross-project DMs; recipient may be project or hub mode |
| D12 | Hub mode grant: agent caller must be full-role and already hub-mode (non-escalation) |
| D13 | Cross-project DMs only in first release; group expansion must remain possible |
| D14 | Hub disable blocks cross-project sends and agent history access; retains records |

For the full decision record with rationale, see the
design notes (internal: `msg-authz-design-notes.md`).

---

## CLI Reference

### `scion start --message-mode <mode>`

Sets the initial message mode when creating an agent. Overrides template
and parent inheritance. Valid modes: `none`, `lineage`, `branch`, `project`,
`hub`. Hub-only (ignored in local mode).

### `scion create --message-mode <mode>`

Sets the initial message mode for a provisioned agent. Same semantics as
`scion start --message-mode`.

### `scion set-message-mode <agent> <mode>`

Changes a running agent's messaging mode via the Hub API.

Flags:
- `--cascade` — apply the mode change to all descendant agents
- `--dry-run` — preview cascade effects without applying changes

When the requested mode is `hub`, the hub mode grant guard applies for
agent callers (see [Hub Mode Grant Guard](#hub-mode-grant-guard)).
Hub-only. Returns an error when Hub is not available.

### Cross-project messaging commands

```sh
# Send a message to an agent in another project
scion message --project <target-project> @<agent> "message"

# List conversations including cross-project DMs
scion conversation list --project <project>

# Get conversation with a cross-project agent
scion conversation get --project <target-project> @<agent>

# View cross-project conversation messages
scion conversation messages --project <target-project> @<agent> --limit 50

# Reply to a cross-project conversation by ID
scion message conv:<conversation-uuid> "message"

# Qualified fan-out to agents in multiple projects
scion message @<project-slug>/<agent-slug> @<other-project>/<other-agent> "message"
```

### Hub and project messaging administration

```sh
# View Hub messaging settings
scion hub messaging get

# Enable cross-project messaging on the Hub
scion hub messaging set --cross-project enabled

# View a project's inbound policy
scion project messaging get --project <project>

# Set a project's inbound policy (uses CAS)
scion project messaging set --project <project> --inbound members
```

These administration commands are human-only and unavailable in agent mode.

---

## Rollout and Rollback

### Rollout

1. Deploy schema migrations and compatible readers/writers with the Hub
   flag set to `false`. New endpoints may report unavailable until their
   enforcing phase lands.
2. Upgrade all Hub replicas before enabling the flag. Mixed-version
   enabled deployments are unsupported.
3. Use two designated test projects: set `members` on one, opt a
   designated sender into `hub` mode. Demonstrate DM delivery and denied
   reply, then grant the recipient `hub` mode and demonstrate the allowed
   reverse edge.
4. Verify membership removal and Hub disable against a queued message.
5. Keep default settings unchanged for all other projects/agents.

### Rollback

Turn the Hub flag off first. This stops subsequent cross-project decisions
and closes external agent conversation access. Retain message/audit records
and configured project policies for later recovery. Already-delivered
content remains outside recall.

Prefer rolling back application behavior while retaining additive schema.
Before downgrading to a binary predating the `hub` enum, export
configuration, disable the feature, and use an explicit migration to map
`hub` agents to `project` if required by the old reader.

---

## Source Files

| Concept | File |
|---------|------|
| Mode decision logic | `pkg/hub/authorize_message.go` |
| Cross-project evaluator | `pkg/hub/authorize_message.go` (`evaluateCrossProject`, `validateCrossProjectOrigin`, `CheckEffectiveMembership`) |
| Hub mode grant guard | `pkg/hub/authorize_message_mode_grant.go` |
| Project messaging policy | `pkg/hub/project_messaging_policy.go` |
| Hub operational settings | `pkg/hub/operational_settings.go` (`CrossProjectMessagingEnabled`) |
| Hub admin messaging settings | `pkg/hub/admin_messaging.go` |
| Target resolution | `pkg/hub/handlers_messaging_targets.go` |
| Messaging capabilities | `pkg/hub/handlers_messaging_capabilities.go` |
| Conversation resolver | `pkg/hub/handlers_conversation_resolve.go` |
| Conversation send | `pkg/hub/handlers_conversation_send.go` |
| Scheduled message auth | `pkg/hub/authorize_scheduled_message.go` |
| Fan-out and boundaries | `pkg/hub/handlers_agent_messaging.go` |
| set_message_mode handler | `pkg/hub/handlers_agent_message_mode.go` |
| Permission registry | `pkg/hub/permissions/registry.go` |
| Role seeds (admin exclusion) | `pkg/hub/seed.go` |
| MessageMode constants | `pkg/store/models.go` |
| set-message-mode CLI command | `cmd/set_message_mode.go` |
| Hub client SetMessageMode | `pkg/hubclient/agents.go` |
| Hub client messaging SDK | `pkg/hubclient/messaging.go` |
| Conversation CLI commands | `cmd/conversation.go` |
| Hub messaging CLI commands | `cmd/hub_messaging.go` |
| Project messaging CLI commands | `cmd/project_messaging.go` |
| CLI mode allowlist | `cmd/cli_mode.go` |
| Denial code types (frontend) | `web/src/shared/message-mode.ts` |
| Cross-project UI badges | `web/src/components/shared/agent-message-viewer.ts` |
| Hub settings toggle | `web/src/components/pages/admin-server-config.ts` |
| Project inbound policy UI | `web/src/components/pages/project-settings.ts` |
| Agent hub mode UI | `web/src/components/pages/agent-create.ts`, `agent-configure.ts`, `agent-detail.ts` |
