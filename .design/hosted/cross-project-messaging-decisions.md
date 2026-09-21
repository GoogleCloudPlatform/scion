# Confirmed decisions and messaging-mode delegation check

Updated 2026-09-21. Originally recorded the user's 2026-09-17 15:16 UTC
follow-up decisions. Now extended with the CPM cleanup implementation decisions
from Phases 1–4 (#1680, issues #1685–#1696). Source inspection was originally
against commit `1b0a25a9774cdfa505c98debf29d7f1812acf6fc`; cleanup
implementation landed on the `cpm-cleanup-integration` branch.

## Confirmed decisions

| Question | Decision | Implementation consequence |
|---|---|---|
| Must a receiving agent also have hub mode? | No; a hub sender can reach a project-mode recipient. | Require sender `hub`, recipient `project` or `hub`, Hub enabled, and eligible destination policy. Receiving does not grant reply authority. |
| May full-role agents select hub mode? | Yes, only if they already have hub mode. | Add a shared effective-mode grant guard to creation and later mode mutation; full role by itself is insufficient. |
| Are cross-project groups part of the first release? | Start with DMs, preserving the option to add groups. | Reuse generic conversation storage, keep direct-pair invariants kind-specific, and version supported kinds. Do not bake permanent project confinement into the participant schema. |
| What does global disable block? | Both cross-project sends and agent history reads. | Recheck pending/retried delivery and all agent read surfaces; retain stored records and authorized human audit access. Already delivered harness content cannot be recalled. |

## Source check: role ceiling exists; messaging-mode ceiling does not

1. **Creation has a role ceiling.**
   `pkg/hub/handlers_agents_core.go:645` reads the creator's stored role and
   message mode. At line 681, explicit higher role requests are rejected using
   `CompareRoles`; line 688 calculates the effective role with `minRole`.
   `pkg/hub/agentrole.go:95` compares agent roles, not messaging modes.
2. **Mode resolution is inheritance, not a ceiling.**
   `pkg/hub/handlers_agents_core.go:1048` chooses explicit request, template,
   parent mode, then project default. Request/template values receive enum
   validation only. Neither path compares the resolved mode with the creator's
   messaging mode. An otherwise-authorized branch-mode creator can select
   project mode through an explicit request or template under this code.
3. **The early creation gate does not supply the missing check.**
   `pkg/hub/authorize.go:136` authorizes agent creation using action scope and
   same-project confinement. It does not receive the resolved messaging mode.
4. **Later mode mutation also lacks a mode ceiling.**
   `pkg/hub/handlers_agent_message_mode.go:127` checks the agent's full-role
   scope and same-project target, without comparing caller and requested modes.
   Line 247 applies the requested mode. `cascadeMessageMode` at line 295 walks
   descendants and line 333 assigns the requested mode to each.
5. **Full role is a separate concept.**
   `pkg/hub/agentrole.go:55` includes both creation and set-message-mode scopes
   in the full-role scope set. Having those scopes is not proof that the agent
   holds hub messaging mode. Existing role tests do not establish a messaging
   delegation invariant.

Conclusion: merely adding `hub` to the enum would expose a grant path to
otherwise-authorized non-hub agents. The implementation must add the guard;
it cannot rely on inheritance or the role ceiling to provide it.

## Required guard contract

Use the same helper from all effective-mode mutation paths:

```text
AuthorizeMessageModeGrant(authenticated actor, target, resolved mode)
  first require existing action-specific authorization
  if this is not a new hub grant: retain existing mode rules
  if human caller: retain existing action-specific human authorization
  if agent caller:
    load current caller record; lookup failure denies the grant
    require caller's stored role == full AND stored message mode == hub
    intersect with authenticated action scopes and other credential limits
    require caller's project == target project
  otherwise deny
```

For creation, any effective `hub` is a new grant, even if it came from a
template, parent inheritance, or a default. For existing agents, a transition
to hub is a grant; restart or unrelated update retaining hub is not. Applying
a cascade to an unchanged hub root still requires checks for descendants that
would newly receive hub mode. No input path may bypass effective-mode
resolution and its guard.

| Agent caller | May grant hub within its own project? |
|---|---|
| Current full role + hub mode + required action scope | Yes, subject to existing target/action authorization. |
| Current full role + any other messaging mode | No, including self-promotion. |
| Current hub mode + any lower role | No. |
| Stale token scope but current role or mode revoked | No. |
| Missing/corrupt caller record or policy-store failure | No; distinguish retryable infrastructure failure from policy denial. |

Read fresh authority at mutation and enforce consistency with concurrent
downgrades; an old preview is not a grant. Dry-run uses the same check. Cascade
execution must stop or deny remaining unauthorized grants after revocation and
report outcomes accurately; it must not silently clamp hub to project mode.

Preserve human authorization separately: permission to create an agent is not
automatically permission to change every existing agent's mode. Agent ancestry
does not let an agent use its originating human's mutation privileges. A
human-authorized seed or an already-authorized full/hub agent bootstraps hub
mode. A full/hub parent can create a lower-role hub child under the independent
role ceiling; that child can send externally but cannot delegate hub onward.

The Hub availability switch does not rewrite stored modes. Authorized full/hub
agents may stage grants while it is disabled, but cannot enable the switch or
send/read externally. This separates stored delegation authority from current
feature availability.

Scope the new rule to the hub privilege. The existing none/lineage/branch/project
modes are communication cells, not a simple total ordering. A general legacy
mode-ranking change would require a separate policy decision; the user-confirmed
hub ceiling does not silently redefine those transitions.

## DM-first without a group dead end

The first release must advertise only direct cross-project conversations and
reject foreign-room operations before effects. Keep the directional message
evaluator independent of conversation kind and retain arbitrary group
participants. A group's project remains its owner, not a database constraint
that every future participant must belong to that project.

Future group support can add explicit admission/invitations, per-recipient
receive consent, history visibility/revocation, and observer rules. A permitted
DM edge must never authorize an entire room transcript. Adding a group uses a
new conversation ID and explicit membership; it does not mutate a DM key or
retroactively share its history. See [design section 7](02-design.md#7-conversation-authorization-and-history).

## Acceptance coverage and remaining proposed details

The [delivery plan](03-delivery-plan.md) includes creation request/template/
inheritance/default paths; self/peer/child mutation; cascade and dry-run;
caller revocation/store failure; human seed; project-recipient read/denied reply;
later authorized promotion and reciprocal reply; and Hub-off sends/history.

The design still labels finer details as proposals: exact eligible built-in
membership roles and group semantics, policy mutation governance, narrow
human observation, and history access after individual mode/policy changes.
The recommended history rule permits a canonical participant to read while at
least one direction remains authorized and the Hub feature is enabled. It
allows project-mode recipient reads without an implicit reply grant. None of
these details weakens the confirmed global-disable behavior.

## CPM cleanup implementation decisions (Phases 1–4)

The following decisions were made during the CPM cleanup implementation
(#1680, September 2026). CPM has never been deployed; these decisions carry
no backward compatibility constraints.

### D-C1: Remove duplicate conversation send endpoint

**Decision**: Delete `POST /api/v1/conversations/{id}/messages` for agent
sends. Route all agent `conv:` references through the outbound endpoint with
a `conversation_ref` field.

**Rationale**: The original design proposed a separate conversation send
endpoint for `conv:<uuid>` replies. During implementation it became clear
this created a duplicate delivery path with independent authorization,
persistence, and dispatch logic — a maintenance and security burden. Since
CPM has never been deployed, there are no existing clients to support.

**Consequence**: One transport for all agent sends. The outbound handler's
`resolveOutboundRouting` derives the peer from the conversation reference
and feeds the result into `ExecuteAgentDM`. The `handleConversationSend` in
`handlers_chat_v2.go` remains for the native web chat user path only.

**Implementation**: #1693 (CLI cutover), #1694 (endpoint deletion).

### D-C2: Shared DM operation vs separate paths

**Decision**: Extract a single typed internal operation (`ExecuteAgentDM`)
that both the structured inbound handler and the outbound handler call for
all agent-to-agent DMs.

**Rationale**: Before the cleanup, the structured inbound path and the
outbound path had separate authorization, persistence, audit, and dispatch
code. This led to inconsistencies in rate limiting, provenance stamping,
and delivery outcome reporting. A shared operation ensures identical
admission checks regardless of entry point.

**Consequence**: `AgentDMInput` / `AgentDMResult` / `AgentDMError` are the
typed contract. Adapters (HTTP handlers) retain routing, conversation
resolution, group/human routing, and HTTP serialization. The core operation
owns rate limiting, authorization, foreign attachment rejection, wake,
conversation resolution, audit, persistence, publication, and dispatch.

**Implementation**: #1688 (extraction), #1689 (truthful outcomes), #1690
(provenance unification).

### D-C3: Wake behavior — resume only for suspended single-agent targets

**Decision**: Wake (resume from suspended state) is attempted only for
single-agent targets that are currently suspended. Running agents are not
restarted. Stopped agents are rejected. Group and human targets do not
trigger wake. Wake runs after all admission checks.

**Rationale**: Wake is a side effect — resuming a suspended agent costs
resources. Denied, oversized, or attachment-rejected requests must not
trigger wake. The post-admission position ensures that only fully
authorized sends can resume an agent.

**Consequence**: `ExecuteAgentDM` checks wake eligibility after
authorization and attachment validation. A denied cross-project send
cannot resume a foreign agent. Managed-runtime agents do not support wake
(explicit unsupported error).

**Implementation**: #1691 (wake honor), #1712 (fork PR).

### D-C4: Next-send authority across replicas (authoritative store reads)

**Decision**: Cross-project authorization reads the Hub's
`cross_project_messaging_enabled` setting directly from the shared store
at every decision boundary, bypassing the replica-local operational
settings cache.

**Rationale**: The existing cache/event propagation model has a
notification delay window. During that window, a recently disabled setting
could still appear enabled on replicas that haven't received the
invalidation event. For a security-critical kill switch, this delay is
unacceptable.

**Consequence**: `ReadAuthoritativeCrossProjectEnabled` performs a direct
store read. The cache continues to drive UI refresh and non-security
reads. The very next send after a disable observes the current setting.
Store outages fail closed for new external access.

**Implementation**: #1686 (#1697 fork PR).

### D-C5: Typed outcomes and ambiguous results

**Decision**: The DM operation returns three typed outcomes: `accepted`,
`failed`, and `ambiguous`. No automatic pending replay is provided.

**Rationale**: Broker acceptance does not guarantee harness consumption.
Post-persistence failures (e.g. broker timeout, CAS failure on
MarkMessageDispatched) leave the message in an indeterminate state.
Reporting this honestly prevents callers from assuming exactly-once
delivery and prevents the system from silently replaying messages.

**Consequence**: API responses distinguish the three states. `ambiguous`
means "persisted but dispatch outcome unknown" — callers must not
automatically retry. The message row may be in "pending" or "dispatched"
state. No blind retry guidance is returned.

**Implementation**: #1689 (#1709 fork PR for provenance).

### D-C6: Foreign attachment capability limit

**Decision**: Cross-project DMs reject non-empty attachment payloads at
admission time, before ingestion. This is a clear capability error, not
a silent drop.

**Rationale**: Cross-project attachment transfer requires Hub-managed
attachment IDs with conversation-scoped authorization on upload, link,
and download. This path is not implemented in the first release. Silently
dropping attachments would be confusing; rejecting at admission makes the
limit explicit and actionable.

**Consequence**: `ExecuteAgentDM` checks for non-empty `Attachments` on
cross-project sends and returns an `AgentDMError` with code
`cross_project_content_unauthorized`. Text-only DMs proceed normally.
The attachment transfer feature is an intended future extension.

**Implementation**: #1687 (#1700 fork PR).

### D-C7: No-deployment / no-transition decision

**Decision**: No backward compatibility shim, 501 fallback, old-client
support window, or historical CPM data migration is implemented.

**Rationale**: CPM has never been deployed. There are no existing clients,
no stored cross-project messages, and no live settings to migrate. Any
compatibility infrastructure would be dead code with ongoing maintenance
cost and no users.

**Consequence**: The cleanup deleted the duplicate endpoint (#1694) and
migrated all test coverage to the surviving outbound path (#1693) without
any transition period. New deployments start with the consolidated
architecture from day one.
