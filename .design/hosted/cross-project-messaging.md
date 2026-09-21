# Design: controlled agent messaging across projects on one Hub

Status: **final** — reflects the architecture delivered by the CPM cleanup
(Phases 1–4, issues #1685–#1696). Implementation is complete on the
`cpm-cleanup-integration` branch. The original investigation
([cross-project-messaging-investigation.md](cross-project-messaging-investigation.md))
is preserved as historical context; its findings informed this design but some
proposed paths were superseded by the cleanup work described here and in the
[decision log](cross-project-messaging-decisions.md).

See [delivery plan](cross-project-messaging-delivery-plan.md) for the phase
ledger and PR links, and [decisions](cross-project-messaging-decisions.md) for
the ratified choices and their implementation consequences.

## 1. Outcome and boundaries

An agent in project A can address an agent in project B on the same Hub, receive
an authorized reply, and find/read their conversation through the CLI. Humans
can configure the feature in the UI and inspect exchanges through existing
authorized agent-management views. The Hub's existing identity, persistence,
Event Bus, Message Broker, and Runtime Broker infrastructure carry the traffic.

The feature is off by default. It does not grant general membership in the
other project. Cross-Hub communication remains on A2A/OIDC. A message reference
never silently selects a different Hub, credentials, or federation transport.

This design supports cross-project agent DMs, explicitly addressed independent
DM fan-out, and scheduled direct messages. Project-owned group conversations,
native topics, project broadcasts, and plugin channels retain their current
project boundaries. A group of recipients in a CLI `set` is a batch of DMs, not
a shared transcript. Foreign project rooms are unsupported in the first release;
they are an intended possible extension, not a permanent prohibition. Section
7 defines the seams needed to add group access without replacing DMs or policy.

## 2. Three controls and their defaults

| Control | Proposed field/value | Default | Who changes it |
|---|---|---|---|
| Hub availability | `cross_project_messaging_enabled: boolean` in the existing Hub messaging settings section | `false` | Local, unscoped Hub administrator |
| Agent outbound reach | Existing `messageMode` gains `hub`; recipients may remain `project` | Remains `project` | Existing managers, with the new agent grant ceiling below |
| Receiving project | `crossProjectInbound: none \| members \| any` | `none` | Active direct project owner or local, unscoped Hub administrator |

The receive choices mean:

- `none`: accept no agent messages originating in another project.
- `members`: accept an external agent only when its hub-attested originating
  human is currently an eligible member of this receiving project.
- `any`: accept an eligible local agent from any project on this Hub.

All choices require the external **sender** to use `hub`; the recipient may
use `project` or `hub`. This recipient behavior is confirmed by the user.
`any` does not open `none`, `lineage`, or `branch` recipients. There is no
explicit list of source projects and no second membership-threshold setting.

The receiver's policy is directional. A -> B checks B's policy; B -> A checks
A's policy independently. A's receive setting does not limit its outgoing
messages. A send can succeed while a reply is denied. Expose that distinction
in reachability responses and UI rather than introducing an implicit reply
exception. Receiving a message never grants authority to reply.

A project-mode recipient can receive and read the authorized DM but cannot
reply across projects until an authorized actor grants it hub mode. Reply
authorization also checks the other project's receive policy. Explain this
asymmetry in the CLI/UI; never upgrade a recipient automatically.

These checks authorize the immediate authenticated sender, not the original
author of every sentence in its message. A local project-mode agent can ask a
hub-mode peer to relay content, and a member's agent can relay material it
obtained elsewhere. The policy cannot prevent such forwarding or act as a data
loss prevention boundary. Existing branch/lineage isolation remains meaningful;
the new project/hub cell deliberately admits this relay behavior. If owners
need an outbound project ceiling, design that separately rather than treating
the inbound `none` setting as one.

## 3. Exact mode compatibility

Existing same-project rules stay intact. `hub` joins the existing `project`
communication cell within its own project; it does not open branch/lineage
boundaries.

| Sender / receiver | `none` | `lineage` | `branch` | `project` | `hub` |
|---|---|---|---|---|---|
| `none` | deny | deny | deny | deny | deny |
| `lineage` | deny | deny | deny | deny | deny |
| `branch` | deny | deny | direct parent/child only | deny | deny |
| `project` | deny | deny | deny | allow | allow |
| `hub` | deny | deny | deny | allow | allow |

This table is for distinct agent endpoints in the **same** project. Existing
self-message and true system-plane exceptions remain separate. For different
projects, only `hub` -> `project` and `hub` -> `hub` may pass the remaining
gates. A `project` sender cannot reply externally, including to an existing DM.
Unknown modes fail closed. Explicitly normalized legacy empty values
may retain the existing `project` default; do not treat an unknown new value as
`project` or `hub`.

For human -> agent delivery, `hub` behaves like `project` under existing user
authorization, UAT caveats, and human piercing rules. It grants no new right to
message unrelated humans. The originating user's membership test is evidence
for one external agent edge; it never converts that agent into a user identity.

### Granting hub mode: confirmed non-escalation rule

An agent may grant `hub` only when it is **currently full-role and already
hub-mode**, with the required authenticated action scopes and same-project
target. Read current role and mode from its stored record, intersect with the
credential's scopes, and fail closed on lookup failure. A full-role project-mode
agent cannot promote itself, a peer, or a child to hub mode. A hub-mode agent
without full role cannot grant it either. Human callers retain existing
action-specific authorization; agent ancestry/on-behalf-of markers cannot take
the human path. A human-authorized seed or an already-authorized full/hub agent
is needed to establish further hub-mode agents.

Use one `AuthorizeMessageModeGrant` helper after resolving the **effective**
mode, covering explicit create, template resolution, parent inheritance,
default/reset resolution, existing-agent mode changes, and cascades. Check any
configure/start/update path that can actually change the stored mode. A template
can describe `hub`, but cannot authorize its instantiation by a non-hub agent.
The existing role-creation ceiling still independently limits the child's role;
an authorized full/hub creator can create a lower-role hub-mode child, which
can message but cannot grant hub mode onward.

Enforce the caller's current authority at the mutation transaction, not just
when a template was saved or a cascade preview was generated. Dry-run uses the
same guard; each actual cascade grant is subject to it, and revocation stops
remaining unauthorized grants. Do not silently clamp an unauthorized hub
request to project mode. Unrelated updates or restart of an unchanged,
already-authorized hub-mode agent are not new grants. Saving a dormant template
does not itself grant an agent a mode.

The requested check does **not** exist for messaging modes in the inspected
code; only the agent role has a creation ceiling. Scope this new guard to the
hub privilege. The older four modes form communication cells, not a reliable
numeric authority ladder; a general ranking change is a separate decision.
Existing authorized transitions among those modes remain as today.

Mode values may be configured while the Hub feature is off. The API/UI report
that they are inactive for external messaging. With the switch off, a `hub`
agent retains its same-project `project` behavior. Agent defaults are unchanged;
children resolve modes through the existing request/template/parent precedence,
then pass the new grant check. A global disable changes effective messaging,
not the stored mode; an authorized full/hub agent can stage hub-mode grants
while disabled, and cannot thereby enable the Hub switch.

## 4. Identity and the `members` decision

Use the local authenticated agent ID to load the current sender. Load the
current receiver and both projects by immutable ID. Verify the caller is a
locally authenticated agent, the token's project agrees with the sender record,
and neither endpoint/project is deleted or otherwise invalid. Ordinary stopped
state is a delivery/lifecycle matter, not a substitute identity.

For `members`:

1. Require `AncestryIsHubAttested(identity)` and reject every federated identity
   from this local exception, even if it names a local user or project UUID.
2. Use the persisted sender ancestry built by the Hub. Resolve its root human
   principal, consistent with `OriginUserID`; reject missing/corrupt ancestry,
   a non-human root, and an absent/disabled/deleted user.
3. Resolve that user's **current active membership in the destination project**.
   Count built-in member/admin/owner bindings, including active effective-group
   membership for member/admin. An owner counts as a member; ownership does
   not permit the agent to pierce a target's mode.
4. Ignore expired, not-yet-active, revoked, unrelated, and custom additive role
   bindings. Public project visibility, a generic read grant, a shared project
   conversation, or Hub-admin status alone is not membership.
5. Distinguish a negative membership result from a store error. Both refuse
   delivery; infrastructure failure returns a retryable service error and an
   operator diagnostic rather than misreporting a membership denial.

No ancestry traversal through live parent-agent records is necessary: the
Hub-attested chain survives deletion of an intermediate parent. Do not scan
arbitrary ancestry IDs for any favorable principal; today's chain has one root
human followed by agents. If future delegation can change originating humans,
version the attestation model before extending this interpretation.

Membership revocation or group/binding expiry must affect the next decision;
membership is not copied into a long-lived token or conversation ACL. Cache
only within one evaluation/request until reliable revision-aware invalidation
is demonstrated. Existing explicit user/agent suspensions, credential revocation,
and deny constraints remain applicable; no permission from the originating
human is otherwise inherited.

## 5. Authorization architecture

Refactor `authorizeAgentMessage` into a typed policy service while keeping its
existing callers as adapters during migration. Suggested interface:

```go
type MessageDecision struct {
    Allowed bool
    Code MessageDenialCode
    CrossProject bool
    HubPolicyRevision int64
    ProjectPolicyRevision int64
}

EvaluateAgentMessage(ctx, authenticatedSender, targetID, deliveryContext)
EvaluateConversationRead(ctx, authenticatedReader, conversationID)
ResolveMessageTarget(ctx, authenticatedSender, targetProjectRef, agentRef)
AuthorizeMessageModeGrant(ctx, authenticatedActor, target, resolvedMode)
```

`deliveryContext` is typed internal data. It must not accept a client boolean
for system-plane status. The external decision is conceptually:

```text
authenticated local agent + current valid sender/receiver records
    -> same project? existing mode matrix (with hub/project compatibility)
    -> otherwise: authoritative Hub switch enabled
    -> sender.mode == hub AND receiver.mode IN {project, hub}
    -> destination policy allows this sender's origin
    -> applicable credential/principal restrictions satisfied
    -> authorize exact destination ID, persist, then dispatch/recheck as needed
```

Use stable codes such as `cross_project_disabled`,
`cross_project_sender_mode`, `cross_project_target_mode`,
`cross_project_inbound_none`, `cross_project_origin_not_member`,
`cross_project_untrusted_origin`, and `cross_project_surface_unsupported`.
Replace string parsing in `mapReasonToCode` with the typed result. Missing state
never becomes `any`.

Do not modify generic agent token project matching, read/lifecycle scopes,
existing role-delegation ceilings, authorized-list semantics, or generic agent
GET to enable this feature. Register the narrow messaging operation and its governance in
`permissions/registry.go`, `authzop`, route metadata, and their guards. The
operation must document which existing credential ceilings it intersects and
the precise messaging-only exception to project confinement. It must not add a
general cross-project wildcard or impersonate the origin user.

### Decisions at asynchronous boundaries

Authorization precedes conversation creation, participant writes, message
history publication, Runtime Broker wake/dispatch, and attachment sharing.
Recheck on delayed delivery, scheduled fire, and each actual retry. Do not let
queued or retried messages retain permission after the policy is disabled.

Carry a server-created delivery context containing canonical sender kind/ID,
source project ID, destination agent/project IDs, credential reference/caveats,
message ID, and policy revisions. The revisions explain a decision; they are
not a reusable authorization grant. Reconstruct the current principal from
trusted stored data and the originating credential's restrictions. Do not
reconstruct authentication from a slug, `From` string, caller metadata, or
system-plane flag. Message Broker plugin ingress remains its own authenticated
user/integration path.

The Event Bus still routes to the **destination** project's agent topic.
Runtime Broker selection comes from the target record. Different projects may
be on the same Runtime Broker or different Runtime Brokers on this Hub.

### Kill switch, consistency, and in-flight limits

Read security policy authoritatively from the shared store at cross-project
authorization/dispatch boundaries. The existing operational-settings cache and
events may drive UI refresh, but a delayed invalidation event must not preserve
an old allow decision. Begin without a cross-request positive cache.

Define the decision point precisely: a check that starts after a committed
disable must observe it; a deferred delivery checks again immediately before
dispatch. Work already handed to a Runtime Broker cannot be recalled. If the
product requires no in-flight dispatch after the disable response, that is a
stronger drain/barrier protocol, not something event invalidation guarantees.
Store outages fail closed for new external access while existing local
messaging continues under its normal policy.

## 6. Storage and settings governance

### Hub

Extend the existing `messaging` Hub settings document with
`cross_project_messaging_enabled`, default false. Extend its hand-written
schema, typed settings, getters, capabilities, audit, and admin response.
Keep it DB-owned; do not add a competing per-agent environment variable or
project override.

First change the existing PUT implementation to field-preserving partial
updates. Omitted means unchanged; null resets **only that field** to its
compiled default. Resetting `conversation_envelope_switch` must not delete the
cross-project flag. Preserve unknown/newer fields during compatible partial
updates. Require a revision/ETag for changing the new security flag; return 409
on conflict. Older envelope-only updates may retain their request format but
must internally merge and CAS/retry without losing concurrent policy changes.

Reuse the existing operational-settings service and `HubSettingStore` on both
SQLite and PostgreSQL. Startup is already driver-agnostic; some nil-service
comments/error text still say SQLite is unsupported and should be corrected.
Preserve existing settings precedence and initialization failure behavior;
do not introduce a parallel configuration store or an environment override for
the new security policy. Exercise real startup wiring on both backends.

### Projects

Use a typed project field/column `cross_project_inbound`, with values
`none/members/any` and default `none`, plus a policy revision for optimistic
concurrency. A dedicated mutation service enforces active direct owner or local
unscoped Hub admin and writes the audit record transactionally.

Expose the policy through its dedicated API and a read-only resolved-settings
summary. Generic project update, annotations, ordinary settings PUT, create
payloads, imports, and agent-owned configuration must not bypass this service.
Reject attempted writes of protected fields rather than silently accepting
them. Creating/cloning a project initializes `none`; copying a project's
non-security defaults does not opt a new project into external messaging.

A typed column avoids interpreting freely writable annotations as authority.
Do not add this field to the existing annotation-copy registry as if it were an
ordinary launch default. Project templates cannot set it. Project deletion
immediately makes its policy unavailable; retained DM records are not an
alternate authorization source.

### Agents, conversations, and messages

- Add `hub` to the store constants/validator, Ent enum and generated code,
  configuration/API/SDK types, template validation, spawn/start/configure paths,
  and tests. Apply supported SQLite/PostgreSQL schema migrations before storing
  the new value.
- Keep one global DM and its existing canonical key. Do not change its nullable
  project into a sender or recipient project and do not mint per-project copies.
- Preserve `Message.ProjectID` as the routing/destination project for an
  agent-recipient delivery. Add server-derived `senderProjectId` and
  `recipientProjectId` provenance (nullable for humans/unknown legacy records),
  persisted with the message. Names are display metadata; IDs are authority.
- Reuse existing message IDs and dispatch states for retries/idempotency. A
  queued message awaiting its final authorization, or denied at that check,
  must not expose its body to the recipient through history/SSE even with a
  "failed" label. It is visible only to its author and authorized audit views.
  Introduce an explicit recipient-visible state if existing dispatch states
  cannot represent this distinction. Publishing into recipient-visible history
  itself counts as delivery and therefore requires the live authorization check.
- Backfill old provenance only when IDs establish it unambiguously. Never join
  agents across projects by sender slug. Preserve historical project IDs if a
  peer is renamed or deleted; reuse of a slug never inherits an old DM.

## 7. Conversation authorization and history

### Direct conversations

Unify direct-conversation authorization around the canonical key's two
`(principal kind, immutable ID)` pairs. Participant rows track sidebar/listing
preferences only. A missing row may hide a conversation from a list; it must
not add/remove a third principal's authority. Leaving a DM hides it, and a
subsequent read-only lookup must not rejoin it.

For a direct conversation:

- Reject participant addition, participant substitution, and default-agent
  reassignment. Those operations belong to group conversations.
- Stop accepting arbitrary `kind: direct` through generic conversation create;
  only the authorized principal-pair send path can mint one.
- Read-only target/reference resolution never creates a conversation,
  participant, message, or visibility grant. A valid peer with no history
  returns `exists: false`/an empty history, not a write.
- `conv:<id>` derives its peer from the key. Client-supplied project, sender,
  recipient, thread, or metadata fields must agree or the request is rejected.
- Do not use the permission to send as permission to read any other conversation
  involving that peer.

Recommended cross-project agent history rule: require canonical participation,
the Hub feature enabled, valid endpoint/project records, and at least one
currently permitted direction for that pair. Evaluate the directional rule
above, including its modes, rather than requiring both endpoints to use `hub`.
Thus a project-mode recipient can list/read its incoming DM while being unable
to send a reply. Both endpoints must remain in `project`/`hub`, with at least
one in `hub` and a permitted outgoing edge; if neither direction is permitted,
agent-facing history access closes. A hub-to-project downgrade alone need not
close reads if the reverse hub-to-project edge still exists.

Project receive-policy changes govern **new incoming content**. Previously
accepted history remains readable while the other direction is still allowed;
the UI must not call this history erasure. Disabling the Hub feature closes
cross-project agent history/list/resolve/stream access altogether. Retain stored
messages and authorized human audit views. Nothing can retract content already
delivered into a harness. Blocking both sends and agent reads on Hub disable is
confirmed by the user. The finer rule for access after individual mode/policy
changes remains a design recommendation, with explicit acceptance tests.

For human observation, preserve the distinction in current message viewers:
ordinary users see their own conversations; a user who can manage one endpoint
may inspect that endpoint's cross-project exchanges through a management view.
That does not grant access to the peer's other messages or general agent record.
For new cross-project rows, require this management check in interagent views;
being a participant in a separate human-agent DM is insufficient. Do not add
humans as canonical participants in the two-agent DM merely to enable a UI.

### Lists, filters, and pagination

`GET /conversations?project_id=P` continues to list only conversations accessible
to the caller. For groups it matches the owning project. For DMs it matches a
canonical agent endpoint in P; for a local/foreign pair, selecting either
participating project finds that single DM. A projectless human-human DM does
not match a project filter. A filter is never an access grant.

Return minimal peer identity and endpoint-project summaries so the CLI/UI need
not call the forbidden foreign agent GET. Apply authorization and filtering
before limits/counts; implement stable store pagination instead of loading every
conversation and returning a first page without a useful cursor. Hide stale or
forged listing rows that fail canonical authorization.

In the first release, project groups retain existing membership/access semantics
and same-project agent restrictions. No external agent can join, set a default,
read, or send to a foreign room by setting `--project`, a conversation ID, or the
new mode. Return `cross_project_groups_unsupported` for an already-visible room
and preserve privacy for undisclosed rooms. This is a versioned capability
boundary, not a permanent data-model invariant.

### Preserve a path to cross-project groups

The user explicitly requires that DM-first delivery leave group expansion
possible. Implement these seams now, without enabling group access:

1. Keep `Conversation.kind` and the existing arbitrary-principal participant
   table. Enforce the immutable two-principal key only for `kind: direct`;
   never impose a two-party constraint on every conversation or participant row.
2. Keep the directional sender -> recipient evaluator independent of the
   conversation kind. Put current group rejection in conversation admission/
   surface policy. A future group evaluator can call the same Hub gate,
   destination-project receive policy, and mode checks for each delivery edge.
3. Expose feature capabilities such as
   `crossProjectConversationKinds: ["direct"]`. The first release reports only
   direct conversations. Clients must branch on capabilities, not assume every
   cross-project conversation will forever have exactly one peer.
4. Treat a group's `projectId` as its owning/governing project, not an immutable
   assertion that every participant is located there. Keep the current
   same-project admission check in application policy; do not add a database
   constraint preventing foreign participants in future group conversations.
5. Keep message addresses and recipient project provenance separate from the
   owning conversation. Retain per-recipient delivery/audience records and a
   general conversation-ID event/read boundary. Do not repurpose a DM key or
   `Message.ProjectID` as the universal group audience ACL.
6. Future group support must define owner-controlled invitations/admission,
   remote-project receipt consent, per-member delivery, join-time history
   visibility, revocation, and observer/event audiences. Passing one DM edge
   must never grant the complete group transcript. The existing `none/members/any`
   policy can govern agent receipt, supplemented by room admission controls.
7. Any DM-to-group promotion creates a new group ID and explicit membership;
   preserve the original DM and its private history. Link them only after
   authorization; do not mutate a two-party DM into a group or retroactively
   expose it. An optional group policy/participant-project association can be
   introduced additively without rekeying old DMs or replacing the Hub policy.

The next group phase can add those admission/history semantics, advertise
`group` capability, and extend existing conversation APIs/CLI/UI. No new group
tables or public group endpoints are required solely to reserve that future.

## 8. API and SDK contract

Names below are proposed unless marked existing. Use `/api/v1` throughout.

| Surface | Contract/change |
|---|---|
| Existing `GET/PUT /admin/messaging` | Add boolean and revision; preserve envelope field on updates; administrator-only mutations. |
| New `GET/PUT /projects/{id}/messaging-policy` | Read under existing project-read rules; owner/admin governance for PUT. Body `{ "crossProjectInbound": "members", "expectedRevision": 3 }`; response includes revision, configured/effective state and capabilities. |
| New `GET /messaging/capabilities` | Authenticated, minimal supported modes, Hub enabled state, and `crossProjectConversationKinds: ["direct"]`; no settings document, project catalog, or topology disclosure. |
| New `GET /messaging/targets/resolve?project=<id-or-slug>&agent=<id-or-slug>` | Read-only exact target lookup; messaging authorization; minimal identity and directional reachability. Does not require broad foreign project read. |
| Existing agent `.../message` actions | Same wire routes; use central evaluator and authoritative sender/target IDs; common denials and delivery behavior for standalone and project-scoped routes. |
| New `GET /conversations/resolve?reference=...&project_id=...` | Read-only canonical resolution shared by CLI; `exists:false` for an authorized peer with no conversation; reject ambiguity; no minting. |
| Existing `GET /conversations`, `/{id}`, `/{id}/messages` | Unified read authorization, endpoint-aware project filtering, peer/project summaries, stable cursor; caps checked on all reads. |
| Existing conversation create/participants/default/leave | Preserve group functions; constrain direct functions to immutable pairs and listing preferences. |
| Existing agent `set_message_mode` actions | Accept `hub`; apply full-role + current hub-mode grant guard for agent callers, including cascade/dry-run; return effective external status and grant capability. |
| Existing scheduled-event APIs | Add canonical target project/agent fields separate from the event's owning project; validate and reauthorize at fire/retry. |
| Existing message/history/search/SSE/attachment routes | Apply the same conversation and observer access rule whenever returning cross-project content. |

Example target result (when the caller may discover this target):

```json
{
  "agent": { "id": "<uuid>", "slug": "reviewer", "projectId": "<uuid>", "projectSlug": "tools" },
  "messageability": { "canMessage": true, "canReachViewer": false, "replyReason": "cross_project_inbound_none" }
}
```

A caller cannot select a different sender through this API. No unbounded
Hub-wide agent/project directory is introduced. Exact lookup deliberately
discloses the minimal address of a permitted peer; deny/nonexistent responses
are indistinguishable for callers without normal target visibility. Expose
detailed policy reasons only for already known/visible targets or authorized
administrators. A disabled-feature error can be returned before target lookup.
Bound/rate-limit lookup attempts; avoid hidden target counts and autocomplete
enumeration.

For an existing DM, exact lookup may return its minimal peer summary when the
caller passes the conversation history rule even if the caller cannot send in
that direction. Resolving an unused peer requires a permitted outgoing send;
an incoming-only possibility is not a general peer-directory grant. The
conversation resolver distinguishes these cases without creating rows.

Use 401 for absent/invalid authentication, privacy-preserving 404 for undisclosed
targets/conversations, 403 with a stable code for a known denied operation, 400
for invalid enums/references, 409 for revision/ambiguous-reference conflicts, and
503 for failure to evaluate live policy. Error bodies must not include hidden
origin users, membership lists, target modes, or project names.

Add typed `hubclient` services and options, not ad hoc HTTP in CLI/UI. Register
new route metadata and authorization-operation coverage. Human callers and
scoped UATs continue through their current authorization and credential
restrictions; the local-agent exception cannot be obtained by spoofing headers.

## 9. CLI contract

Use the existing root `--project/-g` selector consistently. Remove the local
conversation flags bound to `convProject`; use one resolver with an explicit
selected **addressing** project. Resolve paths/Git URLs through existing local
project-marker behavior and slugs/UUIDs through the Hub. Cross-project exact
messaging lookup uses the narrow resolver rather than generic foreign project
or agent GET. Never rewrite `SCION_PROJECT`, the sender token's project, or the
sender endpoint based on the target selection.

Examples:

```sh
scion message --project tools @reviewer "Please review this change"
scion conversation list --project tools
scion conversation get --project tools @reviewer
scion conversation messages --project tools @reviewer --limit 50
scion conversation catch-up --project tools @reviewer --since 30m
scion message conv:<conversation-uuid> "Follow-up"
scion set-message-mode my-agent hub
```

Rules:

- Unqualified agent references use the explicit project or existing current
  workspace project. No implicit search over every project and no first match.
- Every conversation subcommand accepts the inherited project flag. For `list`,
  an explicit flag filters results; no flag retains today's all-participating-
  conversations behavior. For create and name resolution, use the current
  project when no explicit selection exists.
- `conv:<uuid>` is authoritative across projects; with an explicit project,
  verify the conversation matches the endpoint-aware filter or reject the
  mismatch. Without an explicit flag it does not require a local workspace.
- `@agent` read references resolve the agent UUID first and then the pair's DM,
  never the conversation display name. Read resolution does not create a DM.
- `#thread` resolves within the selected project with normal room authorization.
  If an agent selects a foreign project, return an explicit unsupported/denied
  result, not a cross-project room join. Quote `'#thread'` in shell examples.
- Bare UUID target sends bind to exactly that agent and validate any selected
  project. IDs remain the fallback for ambiguous project slugs.
- Duplicate permitted project/thread names return an ambiguity error with only
  authorized choices. Project/agent renames do not change existing DM identity.
- Replies by `conv:` route through the sender's outbound endpoint with a
  `conversation_ref` field. The outbound handler resolves the conversation
  reference, derives the peer, and dispatches through `ExecuteAgentDM`. There
  is no separate conversation send endpoint.
- Human CLI callers retain human authority; selecting a project does not send
  as one of its agents. Do not add a `--from-agent` impersonation shortcut.

For explicit fan-out, add qualified `@<project-slug>/<agent-slug>` targets (or
structured `{projectId,agentId}` SDK targets). A qualified target overrides the
default addressing project for that recipient only. Do not confuse these with
`@email`, canonical `conv:`, or forbidden `#space/thread` grammar. Implement
qualified mentions only with the same parser and tests; slugs inside plain
message prose are not authenticated identities.

Add human administration commands using the same SDK services:

```sh
scion hub messaging get
scion hub messaging set --cross-project enabled
scion project messaging get --project tools
scion project messaging set --project tools --inbound members
```

`set` fetches a revision and performs CAS; it reports a concurrent modification
instead of overwriting it. These new command names are proposed. Put read/send
conversation features in agent and assistant allowlists as appropriate;
Hub/project policy setters are human administration, unavailable in agent mode.
Also add `set-message-mode` to the agent allowlist to expose the confirmed
full-role-agent API authority; granting hub mode still requires the caller's
current stored mode to be hub. Other modes retain existing mutation rules.
Update `cmd/cli_mode.go` and obtain the developer's required confirmation of new
command mode availability when implementing, as required by the repository's
AGENTS.md. That implementation confirmation does not block this design artifact.

## 10. UI behavior

### Hub settings

Add “Allow agent messaging across projects” to Hub settings, off by default.
Explain that the sender needs hub mode and destination projects choose whether
their project/hub-mode agents accept external messages. Show
current revision/effective value, pending save, conflict/error, and unavailable
backend states. Turning it off takes effect for subsequent cross-project checks
and delayed deliveries; do not promise recall of already delivered content.

### Project settings

Add a Messaging section with these exact concepts:

- “No external agents” (`none`).
- “Agents created by this project's members” (`members`), with helper text
  explaining descendant agents in any project and current group membership.
- “Agents from any project on this Hub” (`any`).

Only owners/Hub administrators can edit. Other viewers see the configured
policy and a read-only explanation. When the Hub switch is off, show “Disabled
by Hub administrator”; preserve the project's stored selection. State clearly
that this is inbound policy and replies require permission in the other
direction. Do not add a project allowlist editor.

Make the scope visible: choosing `members` or `any` can admit external messages
to existing project-mode agents; it does not grant those agents external send
authority. Replying requires their own hub mode and the peer project's consent.

### Agents and message composition

Add `hub` to shared types, labels, badges, creation, configuration, detail mode
selection, cascade previews, and mode-transition dialogs. Label it “Hub” and
explain “Project messaging plus permitted agents in other projects on this
Hub.” Show a configured-but-disabled status when appropriate. Keep defaults and
inheritance visible. Update tree-edge rendering for same-project hub/project
compatibility; it cannot infer external permission from modes alone.

Describe `project` as sending within its project and accepting external DMs
when its project permits them. Show grant capability separately from send
capability: a full/project caller cannot select hub for itself or another
agent. Creation/template previews must reflect the same grant guard as save.

Use server decisions for `canMessage` and `canReachViewer`. Avoid claiming a
Hub-wide reachable count when the server only counted local agents; show “in
this project” and compute any external preview only on a bounded request.

In permitted recipient/peer displays show `project / agent`, project IDs in
copyable details, and a cross-project badge. Offer an exact project/agent lookup
where useful without making hidden projects browsable. Human message buttons
send as the logged-in human, under existing authority.

### Conversations and authorized observation

Show project context in conversation lists, headers, interagent markers,
notifications, copied links/references, and management message viewers. Read
peer summaries from the conversation API; do not call foreign agent-detail APIs
just to render a name. Disable reply independently of history visibility and
show a useful reason only when the caller may see it.

Humans inspecting an agent's external exchanges do so in its authorized
management view. Do not enroll those humans into the agent-agent DM or imply
they can reply as the agent. If a link points to an inaccessible peer, render a
label without a link. Clear withdrawn content/subscriptions on permission
updates and let the server enforce revocation for already-open tabs.

Implement in the existing Lit/Shoelace UI. Register new icons in
`web/scripts/copy-shoelace-icons.mjs` and page titles if adding routes. The
feature does not require a new frontend framework or a new chat application.

## 11. Secondary delivery and content surfaces

### Fan-out and mentions

Treat each explicitly named foreign agent as a separate authorized DM edge.
Resolve once to immutable IDs, deduplicate by ID, enforce recipient limits,
and return per-recipient outcomes. Partial delivery follows existing set
semantics; never report the batch as wholly delivered when some targets fail.
Do not return information about targets that the caller could not discover.
Authorization for the primary target cannot authorize its mentions.

Retain the one-project validator for actual project groups. Replace its use as
blanket validation for explicit DM fan-out with contextual per-target
authorization. Do not simply delete `ValidateCrossProjectAddressees` everywhere.
Mentions in a private/project room cannot export that transcript to foreign
agents; require an explicit independent DM action instead.

### Scheduler

An event remains owned/administered in the **sender's** project; its target
agent/project are separate immutable fields. Validate both at creation and
fire. Preserve the author kind/ID and credential restrictions, and reject
unsupported scoped credentials as existing code does until their caveats can
be faithfully re-evaluated. Store target IDs, not a slug that might be reused.
Missing external targets are not provisionally authorized at authoring time.

At fire/retry, re-read agent modes, Hub setting, target project policy, origin
membership, user/principal status, and event authorization. A denied event is
recorded with a stable reason and does not deliver its authored body as a
system notice. The author may receive a fixed-format operational failure notice.

### Attachments and links

First enable text DMs and explicitly reject cross-project attachment payloads
until the content path is implemented. The completed feature uses Hub-managed
attachment IDs with a message/conversation association and checks the same
conversation/observer authorization on upload, link, and download. Support
agent authentication for this narrow content endpoint. Sharing authority applies
to the attached object, not its source directory or project.

Never mount the source project's files in the recipient container or grant a
foreign project read permission to make attachments work. Arbitrary path links
remain text with no promised remote accessibility. Unsupported plugin/file
transport returns a clear capability error; no silent path forwarding or
automatic external-channel bridge.

### Events, notifications, search, and logs

Cross-project DM payloads must not be published on either project's broadly
subscribed message/chat stream. Use principal/conversation-scoped delivery and
authorize both subscription and every emitted cross-project payload. Apply the
same guard to previews, unread counts, catch-up, search, typing, read receipts,
edits/deletes, notification deep links, and attachment fetches. Do not rely on
client filtering or subscription-time authorization alone.

Human observer events require the management-view rule, not blanket project
membership. Existing project topic traffic keeps its project audience. Audit
records carry sender/receiver IDs and projects, decision code, settings
revisions, and correlation ID; normal policy logs omit message bodies and
membership lists. Bound metric labels; do not label metrics with arbitrary
agent/project/user IDs.

## 12. Decision status

The four follow-up decisions below were confirmed in the user's message at
2026-09-17 15:16 UTC. Other rows remain implementation recommendations or
requirements from the original outline; they are not implicitly ratified by
that reply.

| Decision | Choice | Status / consequence |
|---|---|---|
| Agent mode name | `hub` | Proposed wire spelling; denotes same-Hub outbound messaging. |
| Recipient mode | External hub sender may reach project or hub recipient | Confirmed; target project still controls inbound receipt. |
| Hub-mode grant | Agent caller must be full-role and already hub-mode | Confirmed; shared grant guard must cover creation and later mutations. |
| Initial scope | Cross-project DMs; group expansion must remain possible | Confirmed; kind-specific policies/capabilities preserve an additive extension. |
| Hub disable | Block cross-project sends and agent history access | Confirmed; retain records and authorized human audit views. |
| Receive enum | `none`, `members`, `any` | Matches original outline; no source-project allowlist. |
| Membership level | Active member/admin/owner, including valid group-derived member/admin | Proposed detail; custom-only roles and public visibility do not qualify. |
| Origin principal | Hub-attested root human | Proposed detail implementing member-progeny policy. |
| Replies | Independent directional check; sender must have hub mode | Consequence of sender-mode and receiving-project controls; no automatic upgrade. |
| Fine-grained history revocation | Canonical pair + active Hub feature + at least one allowed direction | Proposed detail; permits project-mode recipient reads without enabling replies. |
| Human observation | Existing agent-management authority, scoped to that agent's exchanges | Proposed detail; no peer-history or directory grant. |
| Backend coverage | SQLite and PostgreSQL Hubs | Proposed completeness requirement. |
| Project cloning | Reset receive policy to `none` | Proposed safe default. |

A general ordering/ceiling for the four legacy messaging modes remains outside
this change. Future group admission/history semantics should be a new additive
phase using the extension path in section 7, not a rewrite of DM identity.

## 13. Final architecture (post-cleanup)

The CPM cleanup (Phases 1–4, #1680) consolidated the delivery path into a
single internal operation and removed the duplicate conversation send endpoint.
This section documents the surviving architecture.

### Shared DM operation: `ExecuteAgentDM`

All agent-to-agent DM delivery flows through a single typed internal operation
(`ExecuteAgentDM` in `pkg/hub/agent_dm_operation.go`). Both the structured
inbound handler (agent-to-agent direct route) and the outbound handler
(user/CLI `conv:` and `@agent` paths) construct an `AgentDMInput` and call
this operation. The operation performs all admission checks before any side
effects, then persists, publishes, and dispatches:

1. Rate limiting (aggregate ceiling; type-class relabelling cannot buy extra)
2. Foreign attachment rejection at admission — non-empty attachments on
   cross-project DMs are rejected before ingestion, not silently dropped
3. Authorization — `authorizeAgentMessage` evaluates the mode matrix for
   same-project sends and the full cross-project gate sequence for external
   sends: Hub enabled (authoritative store read), sender mode `hub`, target
   mode `project` or `hub`, destination project inbound policy, and
   origin-human membership where required
4. Wake — for suspended single-agent targets only; the operation resumes the
   agent and waits for readiness before dispatching. Wake runs after all
   admission checks so that denied requests cannot resume an agent. Running
   agents are not restarted; stopped agents are rejected; group and human
   targets do not trigger wake
5. Conversation resolution (resolve-or-create DM)
6. Body-free audit record at admission
7. Message persistence as transient "pending"
8. Observer publication (structured message to conversation subscribers)
9. Dispatch to target agent runtime
10. Post-dispatch state transition: "dispatched" on broker acceptance,
    "failed" on definite rejection, "pending" retained on ambiguous outcome

### Three-outcome delivery model

The operation returns one of three typed outcomes (`AgentDMOutcome`):

- **`accepted`**: message persisted and broker/managed-runtime accepted the
  dispatch. The message row is in "dispatched" state. API wording is
  "dispatched" — does not promise harness consumption.
- **`failed`**: a pre-flight check failed (rate limit, authorization,
  validation, attachment rejection), persistence failed, or dispatch was
  definitively rejected. Pre-flight failures produce no side effects.
- **`ambiguous`**: message was persisted and dispatch may or may not have
  succeeded — e.g. the broker accepted but the MarkMessageDispatched CAS
  failed, or context was cancelled mid-flight. Callers must NOT assume
  delivery and must NOT automatically replay. No blind retry guidance is
  returned.

There is no automatic pending replay and no 501 fallback.

### Single CLI transport: outbound with `conversation_ref`

The CLI sends all agent messages — whether `@agent`, `conv:<uuid>`, or
`#thread` references — through the sender's outbound endpoint
(`POST /api/v1/agents/{id}/outbound/message`). The outbound handler
(`handleAgentOutboundMessage`) calls `resolveOutboundRouting` which proceeds
through six stages:

- **S1**: Recipient resolution (UUID/email lookup)
- **S2**: Channel affinity and validation
- **S3**: ConversationRef resolution — `conv:<uuid>` and `#thread`
  references resolve the conversation and derive the peer
- **S4**: Conversation authorization
- **S5**: Addressee derivation
- **S6**: Group/direct routing fixups

For agent-to-agent DMs, the routing result feeds into `ExecuteAgentDM`.
Authorization (including cross-project gates) runs inside `ExecuteAgentDM`
after routing resolution.

The duplicate `POST /api/v1/conversations/{id}/messages` endpoint was deleted
(#1694). CPM has never been deployed; no backward compatibility shim, 501
fallback, old-client support window, or historical transition was needed.

### Authoritative security settings reads

Cross-project authorization reads the Hub's `cross_project_messaging_enabled`
setting authoritatively from the shared store at decision boundaries (#1686).
The existing operational-settings cache and event propagation drive UI refresh,
but a delayed invalidation event does not preserve an old allow decision. The
very next send after a policy change observes the current setting, regardless
of notification/poll propagation state across replicas.

### Surviving endpoints

| Endpoint | Purpose |
|---|---|
| `POST /api/v1/agents/{id}/outbound/message` | All agent outbound sends (user, agent, conv-ref, thread) |
| `POST /api/v1/agents/{id}/message` | Structured inbound: direct agent-to-agent delivery |
| `POST /api/v1/projects/{id}/agents/{slug}/message` | Project-scoped agent-to-agent delivery |
| `GET /api/v1/conversations`, `/{id}`, `/{id}/messages` | Conversation listing, detail, and message history |
| `GET /api/v1/messaging/capabilities` | Feature flags and supported modes |
| `GET /api/v1/messaging/targets/resolve` | Privacy-preserving exact target lookup |
| `GET /api/v1/conversations/resolve` | Read-only conversation resolution |
| `GET/PUT /api/v1/admin/messaging` | Hub messaging administration |
| `GET/PUT /api/v1/projects/{id}/messaging-policy` | Project inbound policy |

### Intentional scope boundaries

The cleanup deliberately does **not** change:

- Cross-project group conversations, broadcasts, or plugin channels — these
  retain existing project boundaries
- Human-to-agent delivery semantics — `hub` behaves like `project` for human
  senders under existing authorization
- A2A/OIDC federation — cross-Hub communication remains on A2A/OIDC
- The four legacy messaging modes (`none`, `lineage`, `branch`, `project`) —
  their existing compatibility matrix is unchanged
- Generic agent lifecycle, configuration, files, secrets, or credential
  scopes — a messaging peer gains no foreign project access beyond the DM
