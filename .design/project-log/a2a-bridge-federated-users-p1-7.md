# A2A bridge serves federated users (phase 1, item 7)

**Date:** 2026-10-13
**Stacked on:** project visitors list and read agents, and read their own grants and DMs (item 5, part B)
**Tracked in:** ptone/scion#4603

## What changed (extras/scion-a2a-bridge only)

- **Credential kinds.** `decodeFederationToken` reads `email` and
  `scion_token_kind`. `classifyFederationClaims` decides the caller kind:
  - `agent`: an agent caller, built exactly as before.
  - `user`: a user caller. It is refused when it also carries an agent
    claim, or has no email.
  - No kind: an agent when the credential carries an agent claim
    (`project_id`, `agent_name`, `ancestry`, `root_user`). A user when it
    carries an email and no agent claim. Otherwise an agent, as before.
  - Any other kind, or a kind that is not a string: refused.
- **User caller path.** `CallerIdentity` gains `Subject` and
  `IsFederatedUser()`, which is nil-safe. The federation scheme sends a
  user caller to `GET /api/v1/auth/me` with the caller's credential in
  `X-Scion-Federation-Token`, plus the bridge's minted bearer, the same
  pair the federation client already sends.
  - When the hub names a project visitor account (a user, not an agent,
    whose ID is a canonical UUID), `UserID` is that account ID. The email
    comes from the credential.
  - When the hub answers but names no such account (its project visitor
    experiment is off, or an identity issuer's user is not linked), the
    bridge uses `legacyFederationCaller`: the agent caller it built for the
    credential before credential kinds existed. Nothing changes for those
    callers (design §11, "feature off: nothing changes").
  - A hub 401 is answered with 401, a 403 or 404 with the uniform refusal
    (403, the hub's refusal text), and a hub that cannot be asked with 502.
  - Sender label: `user:<account ID>`.
  - `CallerKey`: `fed:<account ID>`. The hub maps (issuer, subject) to one
    account, so the key is unique per hub. Design §4.6 keyed tasks on the
    issuer host and subject; two issuers on one host (realms, or a port in
    the issuer or the subject) could share that key, so the account ID
    replaces it.
  - The SDK task store records `caller.UserID` (the account ID), so a reply
    on the user's own topic passes `validateTopicUser`.
- **Executor.** For a federated user:
  - The message is text only (no `a2aTaskId` metadata). The hub refuses
    anything else from a visitor. The reply is correlated through the
    user's own topic: the bridge subscribes to that account's user topic
    alone (`subscribeUserTopic`), not to the project's user topics, so it
    takes in no other user's DM traffic for a federated user.
  - An unknown agent, an agent or project the bridge does not expose (the
    server lets a federated user's request for one reach the executor), and
    a hub refusal of the send all end the task with the uniform refusal, as
    a failed task rather than a JSON-RPC error. A hub or store failure while
    resolving the agent ends it with "Failed to route message to agent".
  - Reply correlation: a federated user's task takes a reply only when the
    reply arrives on its own account's user topic. In plugin mode (no SDK
    task store) the local active-task cache records the account
    (`registerActiveTaskFor`), and `pickPluginModeTask` picks a federated
    user's task only for its own topic; every other reply goes, as before,
    to the latest task not owned by a federated user. In standalone mode
    `validateTopicUser` matches a user topic against the stored account ID,
    and `requireTopicUserForFederatedTask` refuses a topic with no user for
    a federated user's task (`PostgresTaskStore.IsFederatedUserTask`).
  - The reverse holds too: a reply on a user's topic reaches only that
    user's task. An agent caller's task (its replies arrive on the bridge
    admin's topic, `subscribeAdminUserTopics`) takes a reply without
    `a2aTaskId` only on the bridge admin's topic or a topic with no user
    (`agentCallerReplyTopic`), never on a member's or a federated user's
    topic. Before this item, under the federation scheme only agent
    callers existed and the bridge took in only the admin topic; a
    federated user's subscription makes user topics arrive, so the rule is
    needed now. Plugin mode: the cache records agent callers
    (`registerActiveTaskFor` takes the caller, also on the legacy send and
    stream paths), and the legacy task store fallback checks the stored
    caller key (`legacyTaskTakesReply`). Standalone mode:
    `requireAdminTopicForAgentTask` (`PostgresTaskStore.IsAgentCallerTask`),
    with and without `a2aTaskId`. Tasks with no caller identity and local
    user callers keep their rules (neither exists under the federation
    scheme).
  - Before the reply is forwarded, `recheckFederatedUserAccess` reads the
    DM (`GET /api/v1/chat/conversations/dm:agent:<agentId>:user:<accountId>/messages?limit=1`)
    with the caller's credential. A refusal ends the task with the uniform
    refusal. A hub error also stops the reply ("Unable to verify access to
    agent").
  - Cancel sends no interrupt for a federated user (the hub refuses
    interrupts from visitors).
- **Durable resubscribe.** `DurableRequestHandler.SetBridge` (wired in
  `main.go`). Before it streams a response event to a federated user, the
  handler runs the same re-check (`yieldResubscribeEvents`, the per-batch
  step of `SubscribeToTask`). Without a bridge, route, exposed agent or
  resolvable agent, it fails closed.
- **No invented access.**
  - `resolveContext` never auto-provisions an agent for a federated user.
  - The legacy `Bridge.SendMessage` and `SendStreamingMessage` entry points
    refuse federated users. The streaming one sends as the bridge admin,
    and neither has a production caller.
  - Legacy `CancelTask` sends no interrupt for them.
- **Agent cards.** New authenticated `GET /projects/{projectSlug}/agents`,
  for federated users only (404 for others):
  - The project ID comes from the caller's own `GET /api/v1/users/me/visitor-grants`
    (active grant with that slug).
  - Then `GET /api/v1/projects/{id}/agents?messageable=true`, narrowed to
    the project's `exposed_agents`.
  - Cards are built from the hub's projection only (name and slug, generic
    description) through the new `buildAgentCard`, which `GenerateAgentCard`
    now also uses (same output). The exposed-agent check is `Bridge.AuthorizeExposed`, the
    same check as the JSON-RPC route.
  - Every refusal is the uniform refusal. Responses are `private, no-store`.
- **Docs.** `docs-site/.../hosted/user/a2a-bridge.md` describes users from
  another hub, and the two requirements: a bridge signing key, and native
  chat on the hub (the access check reads the conversation through it).
- The bridge credential sent next to a federated user's is minted by one
  `MintingAuth` per bridge and reused until it nears expiry.

## Decisions and notes

- Uniform refusal text = the hub's `visitorRouteRefusalMessage`, so bridge
  and hub answers cannot be told apart.
- The DM key is computed locally with `messages.DMConversationKey`, the
  same call the hub makes. `hubclient.MessageResponse` has no
  `conversation_key` field, and adding one would change the root module.
- The visitor calls (`auth/me`, `visitor-grants`, `messageable` list, DM
  read) are raw HTTP in the bridge. `hubclient` has no `messageable`
  option and no chat routes. No root module file changed.
- Bridge and hub differ on credentials without the claim. Bridge: email
  and no agent claim means user. Hub (item 6B): no claim always means
  agent. Every credential a hub mints carries the claim, so they agree in
  practice. For a disagreeing credential the hub's `auth/me` answer names
  an agent, and the bridge serves it as the agent caller it always was.
- Identity issuers (`issuer_type: user`) issue credentials with an email
  and no agent claim. The bridge asks the hub's `auth/me` about them. They
  become user callers only when the hub names a project visitor account;
  otherwise they keep the agent caller path they had before. Two
  differences remain: an extra `auth/me` call per request, and an earlier
  answer when the hub refuses the credential (401 or 403 at the bridge
  instead of a failed send) or cannot be reached (502).

## Follow-ups

- `auth/me` runs on every request for a user caller (no cache). A
  short cache keyed by credential hash would cut hub calls during polling.
- Standalone reply correlation without metadata stays ambiguity-closed per
  agent: two callers with active tasks on the same agent drop the reply.
  This was already true for every caller; narrowing by the topic's user
  would help.
- Plugin mode, auth schemes with local user callers (not the federation
  scheme): a reply on one user's topic can still reach another local
  user's task, as before this item, because local user callers subscribe
  to the project's user topics. Correlating every user caller's task by
  its topic user would change that for all callers.
- A federated user's own-topic subscription is kept for the life of the
  process, like every other subscription (one per account that called).
- Push notifications (legacy `PushDispatcher`) have no re-check. They are
  off in production wiring (`PushNotifications: false`).
- The docs page names the scheme `oidcFederation`; the config value is
  `federation`.

## Merge forward (2026-10-18)

- Merged the visitor agent read branch (with the visitor messaging, visitor route allow-list and federation credential check work beneath it) and then upstream main into this branch. Both merges were clean, and this item's own diff is unchanged.

## Docs and comment correction (2026-10-18)

- The docs page and two bridge comments said that a user access credential from another hub is served as before while the project visitor experiment is off. The Hub now refuses that credential with 401 and the bridge passes the 401 on, so the text says that now. The docs also say that a Hub 400 (a request the Hub does not accept from a user caller) ends the task with "Failed to route message to agent". Text only; no code changes.
- Docs: the reply access check names its own failure text ("Unable to verify access to agent"); other routing and send answers still end with "Failed to route message to agent".

## Merge forward and unlinked users refused (2026-10-19)

- Merged the visitor agent read branch forward again (it now carries the visitor route allow-list change that follows GoogleCloudPlatform/scion#3577). The merge was clean, and this item's own diff is unchanged.
- With that change the Hub answers `GET /api/v1/auth/me` with 403 for a user from another issuer that has no project visitor account (for example, an identity issuer user not linked to an account). The bridge passes that refusal on as the uniform refusal (403, "You don't have access to this resource") before any task starts.
- `legacyFederationCaller` was no longer reachable and is removed; the earlier note above that such a credential is served as an agent caller no longer holds. A Hub answer that names no project visitor account (an agent, or an ID that is not an account ID) is refused the same way, so the caller is never admitted under another identity.
- The `decodeFederationToken` and auth middleware comments and the docs page now say this. The fake-hub tests that modelled the old 200 answer now model the 403 and assert the uniform refusal; a JSON-RPC send from such a user is refused with nothing sent, no topic subscribed and no access re-check.
