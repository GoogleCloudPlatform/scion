# Project visitors list and read agents, and read their own grants and DMs (phase 1, item 5, part B)

**Date:** 2026-10-13
**Stacked on:** project visitors message agents and read their own DM (part A)
**Tracked in:** ptone/scion#4603

## What changed

- **Agent list and read for visitors.** `visitorAdmittedSubRoutes` gains
  two `GET` entries under `/api/v1/projects/`:
  `{projectId}/agents` and `{projectId}/agents/{agentId}` (agent ID or
  slug; project as `{uuid}` or `{uuid}__{slug}`). `projects.byId` now lists
  `GET` and `POST` in `VisitorMethods`. Each sub-route entry carries an
  explicit `Access` check (`visitorAccessMessage`,
  `visitorAccessAgentList`, `visitorAccessAgentRead`). The zero value
  refuses, so an entry added without a check admits no one.
  - List: the project exists, the visitor holds a project-visitor grant on
    it that is active now, and `CheckAccess` allows `agent.list` on the
    project's agents.
  - Read: the agent exists in the named project, the visitor holds an
    active grant on that project, and `CheckAccess` allows `agent.read` on
    the agent. The agent's message mode does not matter.
  - Both run in the route guard on every request. Nothing is cached.
- **Projection.** `VisitorAgentView` has exactly `name`, `slug` and
  `status`. `status` is `state.AgentState.DisplayStatus()`: the activity
  while running, otherwise the phase. `visitorAgentView` builds it field by
  field, so a field added to `store.Agent` later never reaches a visitor.
  `listProjectAgents` and `getProjectAgent` branch to the visitor handlers
  first. The list returns every agent of the project that is not deleted,
  newest first, and ignores query parameters. An agent in message mode
  `none` is listed and readable: the design limits only messaging by mode.
- **Own grants.** `GET /api/v1/users/me/visitor-grants` returns the
  caller's project-visitor bindings: project ID, slug and name, window, and
  status (`active`, `scheduled`, `expired`). A revoked grant no longer
  exists and is not listed.
- **Own DMs.** `GET /api/v1/users/me/visitor-dms` lists the caller's DMs
  with agents. Each row has the conversation ID, DM key, project slug,
  agent slug and last activity. A conversation is listed only when:
  - the visitor participates in it;
  - its key is the canonical `dm:agent:<agentId>:user:<visitorId>`;
  - the visitor may message the agent now, the same check that admits the
    DM read routes.
- **Who the new routes answer.** Both new routes answer only a
  `VisitorIdentity` (404 for any other identity), are admitted on `GET`
  only, and are registered in the route metadata, the authz manifest, the
  classification test, the catalog exemptions and the pending bearer
  dispositions.
- **Send response.** For a visitor sender, the message send response adds
  `conversation_id` and `conversation_key` (`visitorDeliveryConversation`).
  Every other sender's response is unchanged (the fields are `omitempty`
  and empty).
- **Uniform refusal.** Every case answers with part A's refusal, byte for
  byte: unknown project or agent, a project without a grant, a grant not
  started, expired or revoked, other methods and sub-routes, and the issuer
  flag off. An unknown project that reaches `handleProjectAgents` also
  answers a visitor with it.

## Decisions

- The by-ID agent read (`GET /api/v1/agents/{id}`) stays closed to
  visitors. The design names the project form only.
- Resolving by project slug plus agent slug: the own-grants route gives the
  project ID for a slug, and the read route accepts the agent slug.
  `GetProject` resolves IDs only.
- DM discovery follows the design ruling: the agent projection stays at
  three fields, the send response names the DM, and a visitor-only DM list
  route exists. The fallback (adding the agent ID to the projection) was not
  needed.
- Two independent checks protect the granted-project rule: an active
  project-visitor binding on the project, and `CheckAccess`. A hub-wide
  rule that might someday admit a user does not open another project's
  agents to a visitor.

## Follow-ups

- The bridge's agent cards should list only agents the caller may message.
  The visitor view has no messageability field, so the bridge has to try
  the send or the hub has to add a filter. A design decision is needed.
- The own-DM list reads every conversation the visitor participates in,
  with no paging. That is fine for visitors with few DMs; add paging if it
  grows.
