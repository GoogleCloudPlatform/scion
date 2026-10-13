# Project visitors message agents and read their own DM (phase 1, item 5, part A)

**Date:** 2026-10-13
**Stacked on:** visitor route allow-list in the route guard
**Tracked in:** ptone/scion#4603

## What changed

- **Sender resolution.** `authorizeAgentMessage` treats a `VisitorIdentity`
  (type `visitor`) as a user sender: its project-visitor grant carries
  `agent.message`, checked through `CheckAccess`, and the target agent's
  message mode applies (mode `none` refuses). Ancestry never applies (a
  visitor is a federated identity) and a visitor is never a project owner.
  The message handler takes the sender from the request identity; a
  visitor's sender address is `user:<account ID>` (`userSenderAddress`),
  never its email, with the same ID as `SenderID`; the DM is
  `dm:agent:<agentId>:user:<account ID>`.
- **Sub-route admission.** The send routes and the chat conversation
  routes live under subtree patterns (`/api/v1/projects/`,
  `/api/v1/agents/`, `/api/v1/chat/conversations/`), so a method list
  alone would admit every sub-route. Those patterns list `POST` (and `GET`
  for chat) in `VisitorMethods`, and `routeGuard` then runs
  `admitVisitorSubRoute`: a visitor on a subtree route is admitted only on
  an entry of `visitorAdmittedSubRoutes` for that pattern and method:
  - `POST /api/v1/projects/{projectId}/agents/{agentId}/message`
  - `POST /api/v1/agents/{agentId}/message`
  - `GET /api/v1/chat/conversations/{dmKey}/messages`
  - `GET` and `POST /api/v1/chat/conversations/{dmKey}/read`

  The agent forms are matched with `ResolveAgentSubRoute`, the resolver the
  handlers use; the chat forms with the split
  `handleChatConversationRoutes` uses. The entry must name an existing
  agent (in the named project on the project form), a DM key must be the
  canonical key of that agent and the visitor, and the visitor must be
  allowed to message the agent now. Nothing is cached: revoking a grant
  refuses the next send and the next DM read. `validateVisitorRoutes`
  refuses to start when a subtree pattern lists a method with no entry.
- **Uniform refusal.** Every visitor refusal is the same response: `403`,
  code `forbidden`, message `You don't have access to this resource`, no
  details. That covers a route or method not open to visitors, an unknown
  project or agent, an agent of another project, no current grant (not
  started, expired, revoked), the agent's message mode `none`, another
  principal's DM, and the issuer no longer accepting visitors (the auth
  middleware already answers that one with the same response). The
  message handlers answer a visitor the same way if a check fails there
  after admission.
- **Request shape (allow-list).** A visitor sends a text message only, to
  the one agent. The request may set `message` or `structured_message`;
  the structured message may set `msg`, `type` (instruction, chat or
  reply) and `recipient` (not a group; the hub replaces it), plus `version`, `timestamp`, `sender` and
  `sender_id`, which the hub validates or sets (it clears a visitor's
  timestamp). Every other field with a value, including `urgent`,
  `status` and any field added later, answers `400` with `details.field`
  (`firstFieldOutside` walks the request structs by JSON name).
- **Envelope.** The hub sets `sender_visitor` (`"true"`), `sender_email`
  (the issuer-verified email), `sender_issuer` (the issuer's display
  name, its URL host) and `sender_display` (`alice@corp.example (visitor
  via idp.example)`) on a visitor's message after the reserved-key strip.
  All four keys are hub-reserved (`messaging.StripReservedMetadata`, the
  set that already holds the artifact reference key), so a client value
  never reaches an agent on any path, and all four are on the legacy
  renderer's allowlist.
- **Replies and no unsolicited sends.** An agent's reply to the address
  its envelope showed (`user:<account ID>`) resolves by ID and lands in
  the visitor's DM. A user address of a federated account is its ID
  (`userAddressName`), and email lookups never return a federated account,
  so the visitor's email never addresses the visitor (a local account with
  that email stays a different principal). An agent may send to a visitor
  only when the DM between that agent and the visitor exists
  (`visitorRecipientRefusal`, on the `user:<ID>` and `recipient_id`
  outbound branches); the DM key names the agent and an agent cannot
  create it, so it exists only when the visitor started it. Any other
  send answers exactly like an unknown user.
- **DM read.** The visitor's history page lists every message of its DM
  (no `web` channel filter): its own messages arrive through the hub API,
  not the web chat. Agent replies land in the same DM, addressed
  `user:<account ID>`, never the synthetic address.
- Behind `federation.project_visitors`: a `VisitorIdentity` exists only
  while the experiment is on, and `admitVisitorSubRoute` also checks it.

## Decisions

- **Refusal wording.** The design's text is "You don't have access to that
  agent". The auth middleware already answers the issuer-off case, and the
  route guard every route not open to visitors, with `You don't have
  access to this resource`; to keep all of these identical, the visitor
  message and DM refusals reuse that response.
- **DM read routes.** The read marker exists only on the chat conversation
  routes, keyed by the DM key, which the visitor can build from the agent
  ID and its own ID (`GET /api/v1/auth/me`). The `/api/v1/conversations`
  routes need a conversation ID the visitor cannot discover without the
  conversation list, so they stay closed. The chat routes are registered
  only while native chat is on.
- **Issuer display name.** Trusted issuers have no display name setting;
  the envelope uses the issuer URL's host. A configured display name is a
  follow-up (it is a federation config change).
- **Sender address (design §4.5 as amended 2026-10-13).** The first cut
  showed `user:<email>`, which an agent's reply could not route to the
  visitor and could route to a local account with the same email. The
  address is the account ID; the email moved to hub-set metadata.
- **Agent sends into an asserted conversation.** On the
  `conversation_id`/`conversation_ref` path the conversation itself is
  authorized (the agent must be a participant) and `recipient_id` is not
  resolved; a visitor reads only its own DM with an agent, so a message
  in any other conversation never reaches it.
- `wake` and `interrupt` are refused: a visitor holds no lifecycle
  permission, and an interrupt controls the agent's session.

## Tests

- `TestVisitorSubRoutes_AdmittedSetIsPinned` pins the (pattern, sub-path,
  method) set; `TestRouteGuard_AdmitsVisitorOnlyOnListedMethods` pins the
  (pattern, method) set, now including the three subtree patterns.
- `TestVisitorSubRoutes_MatchExactShapes`, `TestVisitorDMAgentID`,
  `TestValidateVisitorRoutes_SubtreeMethodNeedsSubRoute`,
  `TestRouteGuard_VisitorSubRouteRefusedWhileExperimentOff`.
- End to end through the server handler with signed issuer tokens:
  send admitted with a grant (both forms; envelope, DM key, persisted
  sender); the uniform refusal compared response by response; revocation
  on the next send and DM read; issuer flag off and on again; own DM
  history and read marker, with other DMs, chat, conversation and inbox
  routes refused; every field outside the allow-list refused by name
  (`urgent`, `status` included) and the timestamp hub-set; sender
  metadata (all four keys) not settable by a local user; the agent's reply
  to the envelope address lands in the visitor's DM while a local account
  with the same email receives nothing; the email form is an unknown
  addressee; an agent's send before the visitor wrote, and another agent's
  send, by address and by `recipient_id`, match the unknown-user answer;
  an unlinked federated user gets the same response with the experiment
  on and off.
- `pkg/messaging`, `pkg/messages`: the reserved-key strip, the new
  envelope and the legacy renderer for the four keys.

## Thread ID removed from the visitor allow-list

- The gate run found that a visitor's send with only `thread_id` (its own
  DM key) fails the shared validation: `thread_id` requires `channel`, and
  `channel` is not on the visitor allow-list. So a visitor's `thread_id`
  could never succeed. Visitors send the same request shape as members,
  with no visitor-only defaulting, so `thread_id` is now refused by name
  like `channel`. This narrows the allow-list, consistent with "text
  only". The shared validation is unchanged. The only client that sends
  `thread_id` (the CLI `--thread-id`, deprecated) already requires
  `--channel` with it.

## Follow-ups (part B and later)

- Agent list and read for visitors, projected to name, slug and status, and
  `GET` of the visitor's own grants.
- Web and CLI rendering of a visitor's messages with `sender_display`
  (the persisted sender is `user:<account ID>`; metadata is not
  persisted).
- A display name setting per trusted issuer for `sender_issuer`.
- Agent visitors (home hub, project, root user in the metadata).
- Managed-runtime agents receive the message body only, with no envelope,
  so they do not see the visitor metadata (true for every sender today).
