# Incremental delivery plan

This plan implements [the proposed design](02-design.md). Every phase has a
bounded result and an acceptance gate. The feature remains off by default;
adding an enum value or relaxing a single handler is not a usable or complete
release. Updated for the user's 2026-09-17 decisions; see the
[decision and mode-ceiling record](04-decisions-and-mode-ceiling.md).

## Dependency order

```text
0. Pin existing contracts and unify direct-conversation access
   -> 1. Persist and govern policy on both Hub backends
      -> 2. Implement narrow direct delivery and live authorization
         -> 3. Complete conversation APIs, target resolution, SDK, and CLI
            -> 4. Complete settings, agent, and conversation UI
            -> 5. Complete fan-out, scheduling, attachments, and event surfaces
               -> 6. Integration validation, documentation, rollout
```

Phases 4 and 5 can proceed independently after phase 3's API contracts are
stable. Common authorization and storage changes remain sequential. UI mockups
and contract tests can start earlier against fixtures, but must not define an
independent client-side policy implementation. Phases are suggested landable
units; split a phase into multiple PRs when its review size warrants it.

## Phase 0 — Establish invariants and repair the DM access seam

Deliverables:

- Turn the current mode matrix, self/system exceptions, human piercing,
  full-agent mutation authority, and project confinement into characterization
  tests. Record that role creation has a ceiling but messaging-mode creation
  and mutation currently do not. Preserve unrelated legacy mode behavior.
- Inventory all messaging ingresses and read surfaces listed in the
  investigation. Annotate which use authorization before writes and which need
  delivery-time checks. Include non-HTTP callbacks and scheduled execution.
- Extract canonical direct-conversation access using principal kind **and** ID.
  Adopt it in conversation get/messages/list and existing resolver adapters.
- Reject extra participants/default-agent mutation for direct conversations.
  Route direct creation through principal-pair minting; generic create remains
  the group creation surface.
- Separate read resolution from resolve-or-create. Reads/listing must not mint,
  resurrect, or rejoin conversations.
- Keep immutable-pair constraints specific to direct conversations. Preserve
  generic group participants and owning-project semantics; do not add universal
  two-party or same-project participant constraints that block future groups.
- Inventory existing noncanonical direct rows and extraneous participant rows.
  Provide a read-only report and a deterministic migration only where historical
  principal IDs prove the pair. Do not guess from display names or delete
  historical conversations. Unresolvable rows fail closed and have a documented
  operator recovery path.

Primary files: `pkg/hub/handlers_conversations.go`, `pkg/messaging/resolve.go`,
`conversation.go`, `pkg/messages/message_group.go`, and conversation/message
store adapters and tests.

Acceptance gate:

- A third principal cannot read a DM by adding a participant row or knowing its
  UUID. A matching ID with the wrong principal kind is denied.
- A DM with missing listing rows still has the correct canonical ACL; leaving
  it does not change the pair's identity or silently un-leave on GET.
- Malformed keys, direct-create bypasses, same-named peers, and deleted native
  topic/mint guards are covered.
- Existing project groups and supported same-project sends work. All
  cross-project agent sends are still denied.

This phase should be reviewed as a conversation access change, independent of
later feature enablement.

## Phase 1 — Store policy and enforce mutation governance

Deliverables:

- Add `hub` to the agent enum, generated Ent code, validators, API/config/SDK
  types, inheritance, creation, cascade and dry-run paths. Trace configure/start
  and other updates that actually change effective mode; a client field alone
  is not proof of backend support. Keep effective external sending disabled.
- Add the shared hub-mode grant guard after effective-mode resolution. Agent
  callers must be currently full-role and already hub-mode, with valid action
  scopes and a same-project target. Cover explicit requests, templates,
  inheritance, defaults/resets, existing-agent changes, and cascades. Enforce
  current stored authority at mutation, not just preview time. Preserve the
  independent role ceiling and existing action-specific human authorization.
- Add typed project inbound policy and revision with `none` defaults, using
  supported SQLite/PostgreSQL migrations and adapter round-trip coverage.
- Implement the dedicated project messaging-policy service/API with active
  direct-owner/local-unscoped-admin governance and transactional mutation audit.
  Surface capabilities and a read-only settings summary.
- Refactor Hub messaging partial updates: field preservation, field-local null
  resets, unknown-field preservation, conflict handling, and audited revision
  changes. Add the Hub switch, default off.
- Extend the existing driver-agnostic operational-settings service on SQLite
  and PostgreSQL without changing unrelated settings precedence. Correct stale
  backend comments and verify actual startup, not only isolated getters.
- Protect generic project create/update/settings/annotations, imports,
  templates, and clone paths from policy-mutation bypasses. Clones start `none`.
- Register policy operations, permissions, route metadata, and any security
  mutation symbols in the authorization catalog/guards.

Primary files: `pkg/ent/schema/agent.go`, `project.go`, generated Ent files,
`pkg/store/models.go`, `pkg/store/entadapter/*`, `pkg/config/opsettings/*`,
`pkg/hub/admin_messaging.go`, `operational_settings.go`,
`project_settings_handlers.go`, `handlers_agent_message_mode.go`,
`handlers_agents_core.go`, `pkg/hub/permissions/registry.go`,
`pkg/hub/authzop/*`, and relevant `pkg/hubclient` services.

Acceptance gate:

- Fresh install and migration both result in Hub off/project none/agent project.
- Setting one messaging field does not erase another; envelope null reset does
  not reset the cross-project flag. Concurrent updates cannot lose changes.
- Project admin without ownership, scoped UAT, agent, custom additive role,
  and generic project update cannot change receiving policy. Active direct owner
  and local unscoped Hub admin can. Expired ownership does not qualify.
- Modes can be staged while disabled; invalid values are rejected everywhere.
  Existing legacy-mode mutation behavior remains explicitly tested.
- A full/project agent cannot grant hub to itself, a peer, or a new child,
  including via templates/defaults. A non-full/hub agent cannot grant it either.
  An authorized full/hub agent can create a lower-role hub child, which cannot
  delegate hub onward. Authorized humans can seed hub mode.
- Dry-run and real mutation use the same grant guard. Missing caller records,
  stale credentials, concurrent caller downgrade, and cascade revocation fail
  closed; no partial unauthorized grants or silent clamping occur. No-op
  restarts/unrelated updates of existing hub agents are not new grants.
- Hub disable leaves stored modes intact: full/hub agents can stage authorized
  grants but cannot turn on Hub messaging or evade the disabled feature.
- Both database backends round-trip policy and revisions, survive restart, and
  preserve existing settings behavior.

## Phase 2 — Direct agent delivery under one policy evaluator

Deliverables:

- Extract/reuse a typed, error-returning effective-membership reader with direct
  and effective-group bindings and active-time checks.
- Implement the typed message evaluator: unchanged local matrix plus hub/project
  compatibility, external Hub gate, sender hub/recipient project-or-hub modes,
  target inbound policy, root-human attestation/membership, and existing
  applicable restrictions.
- Update user-to-hub-mode delivery without broadening human permissions.
- Use the evaluator in standalone/project-scoped direct routes and immediate
  dispatch. Ensure denial precedes DM minting, history writes, wake/interrupt,
  and other externally visible effects.
- Add immutable sender/recipient project provenance to persisted cross-project
  messages and delivery envelopes. Bind routing to target IDs and target Runtime
  Broker. Sender display names are never authority.
- Inject the same evaluator into Message Broker callbacks/retry paths and carry
  trusted originating identity/caveats. Record denial without publishing denied
  content to recipients. Separate pending/denied storage from recipient-visible
  history; a failure label alone does not make exposing the body safe.
- Read cross-project security state from the authoritative store at decision
  boundaries; event/cache propagation alone is insufficient for the kill switch.
- Make cross-project delivery always require canonical conversation handling,
  even if the legacy conversation-envelope switch is off. No legacy bypass may
  be used for a new external message.

Acceptance gate:

- Table-test all 25 local mode pairs and external pairs, including direct
  parent/child behavior and hub/project compatibility.
- Exercise each combination of Hub flag, endpoint modes, and receive enum.
  Hub senders can reach project/hub recipients with eligible receive policy;
  receiver `any` does not open a `branch`/`lineage`/`none` agent. A project-mode
  recipient can read its incoming DM but cannot reply across projects.
- Test direct member, owner, group member/admin, nested groups, expiry,
  not-before, group removal, custom roles, visibility-only access, suspended
  origin, missing root, and deleted intermediate parent.
- Federated ancestry, forged sender fields, stale/mismatched token project,
  duplicate slugs, and unknown target IDs cannot obtain a local external grant.
- Revoke membership/change policy/disable Hub between enqueue and dispatch or
  retry; delivery is refused and recipient history/previews expose no denied
  body. Store failure does not fail open.
- Verify same-Hub routing across two Runtime Brokers and no federation fallback.
- Prove no new foreign project/agent GET, list, terminal, lifecycle, files,
  secret, or settings authority accompanies a successful send.

At this point an administrator can exercise an off-by-default direct-message
pilot through APIs. It is not yet the complete user-facing capability.

## Phase 3 — Usable conversations, target resolution, SDK, and CLI

Deliverables:

- Add privacy-preserving, exact target resolution and minimal capabilities.
  A valid target can be addressed without generic foreign agent/project read.
- Advertise `crossProjectConversationKinds: ["direct"]`; clients must not
  hard-code that all future cross-project conversations have one peer.
- Add the read-only conversation resolver, canonical two-agent `conv:` send
  path, and unified cross-project history authorization.
- Correct project filtering for global DMs; implement authorization-before-
  pagination and return minimal peer/project summaries and stable cursors.
- Extend hubclient with typed APIs, response/error types, target context,
  provenance, and capability handling.
- Replace conversation-local `convProject` shadow flags with the root project
  selector and consistent per-subcommand semantics. Resolve IDs on the server;
  remove first-display-name matching.
- Keep sender project/credentials separate from selected target project.
  Make `conv:` agent replies work instead of entering the user-only outbound
  addressee path. Preserve human CLI identity behavior.
- Add human policy administration commands, help, completion, JSON output, and
  CAS errors. Update CLI mode allowlists after the repository-required developer
  confirmation of the proposed availability.
- Resolve the existing omission of `set-message-mode` from the agent allowlist
  consistently with full-role-agent API authority and the new hub grant guard.
- Include version/capability errors for older Hubs instead of trying unsupported
  behavior or downgrading to an unauthorized legacy route.

Acceptance gate:

- End-to-end: hub-mode A sends to project-mode B by project+slug; B discovers
  and reads the DM but its `conv:` reply is denied. An authorized actor grants B
  hub mode; with A's receive policy permitting B, the reply succeeds. Repeat
  with equal slugs in both projects and with both agents initially hub-mode.
- One-way policy: send succeeds, history works under the selected rule, reply
  is denied with an appropriate visible reason. No inferred reverse grant.
- Hub disable or loss of both permitted directions closes agent-facing
  external history/list/resolve. A hub-to-project downgrade preserves reads if
  the reverse permitted hub-to-project edge remains. None/branch/lineage modes
  close external reads. Human audit retention remains.
- Test `--project` before/after subcommands, list/create/get/messages/catch-up/
  participants/join/leave/set-default, ID and slug resolution, and explicit
  mismatches. Cross-project groups are reported as unsupported in this version,
  without exposing hidden rooms or permanently constraining their data model.
- List with project A and project B finds the same single DM; no filter retains
  caller participation semantics. Pagination cannot skip authorized rows because
  unauthorized rows consumed the page limit.
- First-use read creates zero rows. Rename preserves the DM; slug reuse points
  to a different agent/DM; invisible and nonexistent targets have matching errors.
- A user who manages one endpoint can observe only that endpoint's exchanges;
  ordinary human-agent DM participation alone cannot expose external exchanges.

## Phase 4 — UI configuration and coherent messaging views

Deliverables:

- Hub switch in settings with effective state, save/CAS errors, and clear
  disable semantics.
- Project inbound selection with owner-only edit capabilities and an effective
  Hub-disabled state. No allowlist control.
- Hub mode in agent create/configure/detail, badges, lists, cascade preview,
  confirmation copy, and tree-edge compatibility.
- Explain that project-mode recipients may receive external DMs but cannot
  reply externally. Display grant capability independently from send and
  receive capability; full-role alone must not imply permission to grant hub.
- Correct forward/reverse messageability using the shared evaluator; label
  local-only reachability counts accurately.
- Project-qualified peer labels in chat, management history, interagent markers,
  notifications, and links. Use minimal peer summaries instead of general
  foreign-agent fetches.
- Safe exact lookup, independent send/reply controls, useful denial copy, and
  permission refresh for open tabs. No agent impersonation from human UI.
- Register icons/page titles and document UI capabilities in shared types.

Primary files: `web/src/shared/types.ts`, `message-mode.ts`,
`web/src/components/pages/settings.ts`, `project-settings.ts`,
`agent-create.ts`, `agent-configure.ts`, `agent-detail.ts`, `agents.ts`,
`shared/message-mode-badge.ts`, `messageability-indicator.ts`,
`quick-message-dialog.ts`, `agent-message-viewer.ts`, `agent-tree-view.ts`,
`components/chat/chat-shell.ts`, `shared/chat/*`, and client state/SSE code.

Acceptance gate:

- Owner/admin/ordinary member/agent-mode capabilities render correctly; hiding
  a control does not replace backend enforcement.
- Hub-off plus project-members plus agent-hub renders configured but inactive,
  and later enablement updates without a restart.
- Duplicate peer names are distinguishable; one-way reply denials and stale
  settings conflicts are understandable and accessible by keyboard.
- Mode/template previews match save authorization: full/project cannot grant
  hub; full/hub can when otherwise authorized. Hub-off state shows the stored
  choice separately from its effective messaging capability.
- No unauthorized foreign detail API calls or hidden project enumeration occur.
- Lit/Vitest component tests and browser flows cover settings, message/history
  views, live revocation, mobile layout, and supported themes.

## Phase 5 — Complete secondary paths and content handling

Deliverables:

- Explicit multi-target DM fan-out with project-qualified agent references,
  canonical ID deduplication, limits, individual policy decisions, and partial
  outcome reporting. Reuse parsing for supported qualified DM mentions.
- Preserve first-release project group/broadcast/plugin boundaries and reject
  unsupported cross-project room forwarding before any effects. Keep the
  per-recipient evaluator separate from group admission/history policy so a
  later group phase can reuse it without weakening today's boundary.
- Extend scheduled-message targets independently of event project ownership.
  Persist author authority/caveats and reauthorize at fire/retry. Keep authored
  scheduled text out of the system-plane exemption.
- Add message-bound, Hub-managed cross-project attachment transfer and narrow
  agent download authorization. Keep path links non-transferring and reject
  unsupported file transports explicitly.
- Apply conversation/observer authorization to stream events, search, previews,
  notification links, unread counts, edits/deletes, receipts, typing, and
  attachment retrieval wherever those surfaces expose cross-project content.
- Make legacy per-agent/interagent views query canonical endpoint/message IDs
  for new rows without broad project-filter removal or sender-slug matching.
- Add policy mutation/delivery audit and bounded denial metrics, with body-free
  normal logs and useful operator diagnostics.

Acceptance gate:

- A mixed authorized/denied fan-out delivers only approved DM copies; a group
  participant cannot use mentions to export a private room transcript.
- Broadcast and foreign room join/default/send remain denied even in hub mode.
- A scheduled message's owning project stays the sender's, and membership
  removal, target deletion/rename, mode change, disable, and retry are checked.
- Attaching a file grants access only to the linked content and eligible
  conversation audience, not the source project's filesystem. Another DM's
  attachment ID and stale download access are denied.
- Nonparticipants and project-only subscribers receive no DM payload, preview,
  attachment metadata, unread-count signal, typing event, or search result.
  Already-open subscriptions are reauthorized after revocation.
- Message Broker/plugin metadata cannot claim a local sender or system-plane
  exemption. Delivery retries do not duplicate persisted messages/notifications.

All optional-looking surfaces in this phase need either implemented support or
the explicit, tested boundary in the design. They must not silently use legacy
behavior. Text-only pilot restrictions are removed only after the attachment
acceptance checks pass.

## Phase 6 — Release verification and documentation

Deliverables:

- Update `docs/messaging-authorization.md`, hosted messaging docs, CLI help,
  settings/API schema examples, template/schema docs, and the project glossary
  if needed. Correct stale scheduled-event/system-plane and mode-governance
  statements. Review `docs-site/AGENTS.md` before changing that site.
- Document directional policy, membership/group semantics, current mode
  authority, human observation, history revocation, project-room boundaries,
  clone defaults, exact addressing, and the cross-Hub federation boundary.
- Review the future-group seams: generic participant model, kind-specific
  capability checks, conversation-independent edge authorization, separate
  message audience/provenance, and new-ID DM-to-group promotion. Group support
  itself is not a first-release deliverable; it must be addable without
  rekeying DMs or retroactively sharing their history.
- Exercise migrations, rollout/rollback, cache/event failure, and backend
  behavior in an isolated test environment. Never use active project agents as
  disposable fixtures.
- Review the complete authorization-operation inventory and route guards; do
  not use a blanket exemption to make new endpoints pass CI.
- Run focused tests during each phase. Before code commits/delivery, run the
  repository-required local CI (`make ci`; `make ci-full` for the full Go/web
  mirror), plus applicable compatibility-literal and generated-code checks.
  Use `go build -buildvcs=false` in worktrees and account for leaked SCION test
  environment variables as documented in AGENTS.md.

### Minimum release matrix

| Axis | Required cases |
|---|---|
| Backend/topology | SQLite single Hub; PostgreSQL single Hub and two Hub replicas; same and distinct Runtime Brokers |
| Identity | Local agent, human session, scoped UAT, local admin, federated agent/user, malformed principal |
| Policy | Hub on/off/error; destination none/members/any; all modes; asymmetric reply policy |
| Membership | Direct, effective group, nested group, owner/admin/member, custom-only, revoked, expired, not-yet-active, suspended origin |
| Addressing | Equal slugs, renamed/recreated peers, UUID, explicit/default project, conv ID, ambiguous thread/project, foreign room |
| Transport | Direct, Message Broker queue, retry, fan-out, scheduled fire, supported attachments, explicitly unsupported plugin/room paths |
| Read surface | Conversation list/get/history, management views, streams, notifications, search, unread/typing/receipt events, downloads |
| Mutation | Each policy/mode API; hub grant guard on explicit/template/inherited/default mode; full/hub vs full/project vs non-full/hub; self/peer/child/cascade/dry-run; actual configure/start changes; stale caller/store failure; generic metadata bypass; clone/import; simultaneous writers |

### Rollout

1. Deploy schema and compatible readers/writers with the Hub flag false. New
   endpoints/options may report unavailable until their enforcing phase lands.
2. Upgrade all Hub replicas and relevant delivery consumers before enabling the
   flag. Old nodes can misinterpret `hub` or overwrite a multi-field settings
   document; a mixed-version enabled deployment is unsupported.
3. Use two designated test projects and eligible origins, set `members`, and
   opt a designated sender into `hub`. First demonstrate DM delivery/read to a
   project-mode recipient and denied reply. Then grant the recipient hub mode
   through an authorized human/full-hub agent and demonstrate the allowed
   reverse edge across Runtime Brokers.
4. Verify membership removal and Hub disable against a second replica and a
   queued message. Observe policy decision revisions and denial metrics.
5. Keep default settings unchanged for every other project/agent. Expand usage
   through explicit owner/admin configuration.

### Rollback

First turn the Hub flag off. This stops subsequent cross-project decisions and
closes external agent conversation access; keep message/audit records and
configured project policies for later recovery. Validate delayed work fails
closed. Already delivered harness content remains outside recall.

Prefer rolling back application behavior while retaining additive schema. A
binary predating the `hub` enum/multi-field messaging document is not a safe
blind downgrade. Before such a downgrade, export configuration, disable the
feature, and use an explicit reviewed migration to map `hub` agents to `project`
if required by the old reader. Do not drop history/provenance columns or erase
conversations as a rollback technique.

## Completion checklist for implementation owners

- Three controls are enforced server-side with safe defaults and live decisions.
- Hub-mode grants obey the full-role/current-hub agent ceiling on every
  effective-mode mutation path; human authorization remains action-specific.
- The user can complete configure -> send -> discover/read -> reply through
  supported CLI/UI/API surfaces without general foreign project access.
- Directional replies and revocation behave as documented, across replicas and
  delayed execution; unavailable stores never preserve permission indefinitely.
- Canonical DM identity, read-only resolution, privacy-preserving lookup, and
  backend migration tests pass.
- All secondary surfaces either honor the policy or return the explicit
  documented boundary; no hidden fallback remains.
- DM-only capability is versioned and group extension remains additive; the
  first release neither enables foreign rooms nor locks the schema to DMs.
- SDK, CLI flags/modes/help, UI states, docs, audit, and rollout/rollback are
  finished, not left as follow-up assumptions.
- Each implementation phase has independent review appropriate to an
  authorization change, and the final integration review covers cross-surface
  privacy and denial-before-effects behavior.

## Adjacent issues to keep visible

The investigation exposed canonical-DM/participant ACL drift, display-name CLI
resolution, Message Broker reauthorization gaps, and single-field settings
reset behavior. Those are prerequisites where this feature depends on them.

Broader legacy attachment access, generic conversation group governance,
historical interagent-view privacy, and unrelated settings/comment drift should
be triaged separately when not needed for cross-project safety. In particular,
do not quietly broaden this work into a new project-room authorization model,
federation rewrite, or general credential-scope refactor.
