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
  The message handler already takes the sender from the request identity,
  so a visitor sends as `user:<issuer-verified email>` with its local
  account ID as `SenderID`; the DM is `dm:agent:<agentId>:user:<account ID>`.
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
- **Request shape.** A visitor sends a text message only, in its envelope,
  to the one agent: `plain`, `interrupt`, `wake`, `notify`, `mentions`,
  attachments, client metadata (artifact references included), a
  conversation reference, group or multi-recipient addressing, a channel,
  prerendered delivery text, a message type other than instruction, chat
  or reply, and a thread other than the visitor's own DM answer `400` with
  `details.field`.
- **Envelope.** The hub sets `sender_visitor` (`"true"`) and
  `sender_issuer` (the issuer's host) on a visitor's message after the
  reserved-key strip. Both keys are hub-reserved
  (`messaging.StripReservedMetadata`), so a client value never reaches an
  agent on any path, and both are on the legacy renderer's allowlist.
- **DM read.** The visitor's history page lists every message of its DM
  (no `web` channel filter): its own messages arrive through the hub API,
  not the web chat. Agent replies addressed by the visitor's account ID
  land in the same DM and name the visitor by its issuer-verified email
  (`userAddressName`), never the synthetic address.
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
  the envelope uses the issuer URL's host.
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
  routes refused; request fields refused; sender metadata not settable by
  a local user; the agent's reply lands in the visitor's DM; an unlinked
  federated user gets the same response with the experiment on and off.
- `pkg/messaging`, `pkg/messages`: the reserved-key strip, the new
  envelope and the legacy renderer for the two keys.

## Follow-ups (part B and later)

- Agent list and read for visitors, projected to name, slug and status, and
  `GET` of the visitor's own grants.
- An agent replying to `user:<email>` resolves the email against local
  accounts only (federated rows never match email lookups), so it cannot
  reach a visitor and may reach a local account with the same email; the
  reply must use the conversation or `user:<account ID>`. Worth a hint in
  the envelope or a reply-address rule for visitor DMs.
- Managed-runtime agents receive the message body only, with no envelope,
  so they do not see the visitor metadata (true for every sender today).
