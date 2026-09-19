# Investigation: existing messaging and project boundaries

## Evidence and scope

Inspected local source at `1b0a25a9774cdfa505c98debf29d7f1812acf6fc` on
2026-09-17. Paths below are relative to `/workspace`; function names are the
durable lookup anchors. Findings come from source inspection, not a live-Hub
experiment. No messages were sent, settings changed, agents created, or product
code modified for this investigation.

The frontend is a Lit SPA served by Go. The older root overview's React/Koa
description is superseded by `web/AGENTS.md` and the current source tree.

## 1. Message modes are cells, not a simple privilege ladder

`pkg/store/models.go` defines `none`, `lineage`, `branch`, and `project`.
`pkg/ent/schema/agent.go` makes this an Ent enum with default `project`.
`pkg/hub/authorize_message.go:authorizeAgentMessage` is the main delivery policy
entry point. Its agent path reads the sender record and:

- Rejects either endpoint in `none`.
- Rejects different `ProjectID` values unconditionally.
- Allows two `project` agents in the same project.
- Allows two `branch` agents only when they are direct parent and child.
- Rejects `lineage` agent-to-agent edges and mixed modes, including
  `project`/`branch`.

Self-message and Hub-internal system-plane exceptions precede that matrix.
User delivery has distinct ancestry/owner/admin rules; an owner's agents do not
inherit the owner's ability to pierce a restricted agent mode.

Consequently, adding `hub` requires a specified compatibility matrix. Comparing
modes numerically or simply treating `hub` as a wildcard would change existing
branch and lineage protections.

## 2. Message authorization is already distinct from general project authority

`pkg/hub/handlers_agents_core.go:handleAgentAction` and
`pkg/hub/handlers_projects_core.go:handleProjectAgentAction` route message actions
through `authorizeAgentMessage` before generic lifecycle authorization. The
project-scoped action resolves its target slug/UUID within the URL project.

By contrast, `handlers_agents_core.go:getAgent` explicitly denies an agent's
cross-project reads. Project settings, logs, lifecycle, secrets, templates, and
generic authorized-list/credential rules also have project boundaries.

This provides a narrow implementation seam: extend messaging's decision model,
not the meaning of an agent token's project or general `project:read` scope.
There is no reason to give a messaging peer access to the target's configuration,
runtime details, files, credentials, or lifecycle actions.

`pkg/hub/messageability.go` uses the main evaluator in the forward direction,
but `computeCanReachViewer` is an approximation. Its `project` case returns true
without a complete reverse agent decision. Reachability counts are based on a
supplied project's agents. Cross-project policy makes those approximations
materially misleading.

## 3. Identity and membership foundations already exist

`pkg/hub/identity.go:AgentIdentity` exposes the agent ID, project, scopes,
ancestry, token ID, and `OriginUserID()` (the first ancestry entry).
`AncestryIsHubAttested` rejects federated identities for local ancestry trust.

`pkg/hub/handlers_agents_core.go:createAgent` builds ancestry server-side:
human-created agents start with the human ID; agent-created children receive
the creator's stored ancestry followed by the creator's ID. Display names,
`SCION_CREATOR`, message sender strings, and externally supplied ancestry are
not suitable authorization evidence.

Membership is richer than the old direct-membership accessor:

- `pkg/hub/project_membership_service.go` resolves built-in member/admin/owner
  roles from direct and effective-group role bindings.
- Binding activation and expiry matter at decision time.
- Groups may confer member or admin, but not owner.
- Custom additive roles are not automatically project membership.
- A transaction-aware resolver exists; some convenience helpers instead collapse
  lookup failures into an empty result.

The new receive policy should reuse/extract an error-returning effective
membership resolver. Checking only `GetProjectMembership`, `OwnerID`, project
visibility, or a general read permission would implement a different policy.

## 4. Mode mutation and creation need complete plumbing

`pkg/hub/handlers_agent_message_mode.go` serves standalone and project-scoped
mode actions, including cascade and dry-run. Actual code allows full-role agents
with `project:agent:set_message_mode` in their own project, as well as eligible
human owners/ancestors and local platform administrators. UATs cannot change
modes. Some nearby comments still say human-only.

`handlers_agents_core.go:createAgentInProject` resolves mode in this order:
request, template, parent, default `project`. Relevant clients are `cmd/start.go`,
`cmd/create.go`, `cmd/set_message_mode.go`, and `pkg/hubclient/agents.go`.
The configure UI also submits mode values; developers must trace their server
handling rather than assume every configuration/start path already applies them.

Adding one UI option alone would miss enum generation, persistence, templates,
spawn inheritance, configure/start validation, cascade previews, and capability
descriptions.

There is also a CLI/API mismatch: `cmd/cli_mode.go` allows conversation commands
in agent mode but does not currently list `set-message-mode`, even though the
Hub permits full-role agents to perform that action. The implementation should
make the intended CLI availability explicit rather than assume it already works.

### Follow-up: no existing messaging-mode grant ceiling

Rechecked at the user's request on 2026-09-17 after the initial draft:

- `handlers_agents_core.go:createAgentInProject` around lines 642–685 reads the
  parent's stored **role** and rejects an explicitly requested higher role with
  `CompareRoles`; `minRole` applies the parent/project role ceiling.
- Around lines 1048–1068, **message mode** resolution checks enum validity for
  explicit/template values, then assigns them. The parent's mode supplies a
  fallback only. There is no parent-versus-requested message-mode comparison.
- `handlers_agent_message_mode.go:handleSetMessageMode` around lines 127–151
  checks the full-role scope and same-project target, then authorizes the agent
  branch. It does not load/compare the caller's own message mode. The cascade
  helper assigns the requested mode to descendants without such a comparison.
- `authorize.go:authorizeAgentCreate` checks create scope and caller project;
  its signature does not receive the resolved child mode. `agentrole.go` ranks
  roles, not message modes.

Therefore inheritance is not an authorization ceiling today. For example, an
authorized agent creator in `branch` mode can select a valid `project` mode
explicitly or through a template; the observed code contains no mode ceiling
blocking it. This is a source finding, not a live-agent experiment. The new hub
privilege needs a shared non-escalation check; it must not be assumed to exist.
See [the follow-up record](04-decisions-and-mode-ceiling.md) for its contract.

## 5. Conversations already distinguish global DMs and project groups

`pkg/messaging/conversation.go:ResolveOrCreateDMConversation` derives a sorted,
kind-qualified key from two principal IDs. A DM has **nil `ProjectID`**. Agent
UUIDs already disambiguate equal slugs in different projects. A cross-project
DM therefore does not need a new conversation kind or duplicate conversation
rows in each project.

Project group conversations and native topics have project ownership.
`pkg/ent/schema/conversation.go` and `conversation_participant.go` implement the
conversation and listing-participant records.

There is an important access inconsistency to address before expansion:

- `pkg/messaging/resolve.go:checkPostResolutionAuth` uses both kind and ID from
  the canonical DM key as the direct-conversation ACL.
- `pkg/hub/handlers_conversations.go` uses participant rows for get/messages.
  Listing starts with `GetConversationsForPrincipal`.
- `handleAddParticipant` accepts participants after checking the caller's
  participation and an agent's project against the conversation's project.
  A global DM has no project, so that project comparison alone does not protect
  its two-party identity boundary.
- `handleCreateConversation` accepts `kind: direct` without using the canonical
  two-principal DM minting path.

These are observed differences in code paths, not a claim that a live exploit
was reproduced. Direct conversations need a unified canonical ACL, immutable
two-party membership, and read-only resolution before the feature is enabled.

`pkg/messaging/resolve.go` also has a `senderBelongsToProject` heuristic based
on conversation participation. It must not become the membership authority for
the new `members` policy.

## 6. CLI project selection is partially implemented and inconsistent

The actual command is `scion conversation`, alias `conv`; there is currently no
`conversations` alias. `cmd/root.go` already defines a persistent `--project`
flag accepting a path, Hub slug, or Git URL.

`cmd/conversation.go` additionally defines local `--project` flags for the
parent/list and create commands, bound to a separate `convProject` variable.
List passes that string directly to `project_id`; create passes it as
`projectId`. Other subcommands call `resolveConversationRef` without a selected
project. That helper scans conversation display names, returns the first match,
and does not resolve agent identity or reject multiple matches.

The server's current `project_id` list filter compares only
`Conversation.ProjectID`; it therefore excludes global DMs, even when a DM's
peer belongs to the selected project. This must change for useful cross-project
conversation listing.

`cmd/message.go:sendMessageViaConversation` sends `@agent` through the target
project's structured-message service, but sends other references through the
sender's outbound endpoint. The outbound addressee derivation in
`pkg/hub/handlers_agent_messaging.go` explicitly rejects a non-user DM peer.
An agent-to-agent `conv:<id>` reply therefore needs dispatch work; successful
initial `@agent` delivery does not make replies work automatically.

## 7. Delivery is more than one handler

| Surface | Current implementation | Cross-project implication |
|---|---|---|
| Direct delivery | `handlers_agent_messaging.go:handleAgentMessage` | Derive sender and target IDs server-side; authorize before persistence/minting. |
| Set fan-out | `handleGroupMessage` | Current resolution is anchored to one project; independent per-recipient messages can extend it. |
| Mentions | `processMentions`, `pkg/messaging/validate.go:ValidateCrossProjectAddressees` | Slugs resolve within the primary target project; structural validation requires one project. |
| Broadcast | `handleProjectBroadcast` | Agent caller must belong to that project; keep this explicit unless a separate broadcast feature is designed. |
| Message Broker/Event Bus | `pkg/hub/messagebroker.go` | Delivery topics contain destination project and slug; `deliverToAgent` persists and dispatches without calling the central evaluator itself. |
| Broker plugin ingress | `handlers_broker_inbound.go` | Resolves sender as a user and calls message authorization. A plugin's text must not mint a trusted local-agent identity. |
| Scheduled messages | `authorize_scheduled_message.go`, scheduler handlers | Target project must currently match the event project; definitive authorization runs again at fire time. |
| Native chat | `handlers_chat_v2.go` | User-specific handlers, topic project access, mentions, and separate DM-key history logic. |

Message Broker callback authentication must be carried explicitly if a delivery
is checked again after queuing. Reconstructing authority from a message's sender
slug would be unsafe, especially with identical slugs in different projects.

The scheduled-message code explicitly treats user/agent-authored scheduled
content as message-plane, including at fire time. The contrary statement in
`docs/messaging-authorization.md` that scheduled events are system-plane is
stale and should be corrected as part of implementation documentation.

## 8. Hub settings have a good extension point and a reset trap

`pkg/hub/admin_messaging.go` already exposes `GET/PUT /api/v1/admin/messaging`.
`pkg/config/opsettings/sections.go:MessagingSettings` and the hand-written
schema in `registry.go` currently support the conversation-envelope switch.
The section is DB-owned and has no settings.yaml representation.

The PUT handler constructs a one-field replacement document. Resetting its
current field with explicit null deletes the entire section. Adding another
field without refactoring that behavior would drop/reset the new security
setting when an older client updates the envelope setting.

`pkg/hub/operational_settings.go` supplies revisions, cache refresh, and event
propagation. `pkg/store/entadapter/hubsetting_store.go` supports both PostgreSQL
row locks and SQLite's writer model. Crucially, the current
`cmd/server_foreground.go` startup initializes operational settings for **both**
drivers and fails startup after exhausted initialization retries. Comments in
`server.go` and the messaging handler's nil-service 501 error still describe
SQLite/file mode as unsupported; those descriptions lag the startup code.
Reuse the existing driver-agnostic service and verify both backends, rather than
creating a second SQLite settings path from those stale comments.

## 9. Project receive policy is a security setting

`pkg/hub/project_settings_handlers.go` stores existing project settings in
annotations and authorizes ordinary updates with `ActionUpdate`. This is not
the requested owner-only governance boundary. A new receive-policy value needs
an explicit owner-governed mutation path, protection against generic metadata
updates, audit, revision handling, and deliberate project-clone defaults.

## 10. UI, event, history, and attachment surfaces

Mode types/displays live in `web/src/shared/types.ts` and `message-mode.ts`.
Consumers include agent create/configure/detail pages, badges, quick-message
dialogs, lists, cascade UI, and `shared/agent-tree-view.ts:getEdgeStyle`, which
currently recognizes only two-project-mode or two-branch-mode pairs.

Project policy belongs in `pages/project-settings.ts`; the Hub switch belongs
in `pages/settings.ts`. Chat is implemented in `pages/chat.ts`,
`components/chat/chat-shell.ts`, and `shared/chat/*`.

Additional access paths require deliberate treatment:

- `events.go:PublishUserMessage` publishes to agent, user, and sometimes
  project subjects. A cross-project DM must not inherit a project-wide audience.
- `handlers_chat_v2.go:handleConversationInteragent` queries one agent's
  project and has a legacy sender-slug fallback. Simply dropping the project
  filter could leak history and misattribute same-named agents.
- `handlers_messages.go:handleAgentMessages` distinguishes management viewers
  from ordinary read-only viewers. Cross-project observation should preserve
  that distinction and limit results to exchanges involving the managed agent.
- Attachment download currently has a user/project-based path and a
  projectless-DM path. It is not already an agent participant-based attachment
  sharing capability. Remote filesystem paths are also not transferable merely
  because message delivery is allowed.

## Investigation limits

No external protocol research was needed: A2A/OIDC behavior is deliberately
unchanged. No live authorization or delivery probes were made, and test suites
were not run for this document-only task. The delivery plan names tests that
must establish behavior before implementation ships. Recheck source anchors
against the implementation branch, since this is an actively changing tree.
