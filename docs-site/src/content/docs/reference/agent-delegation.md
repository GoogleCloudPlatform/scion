---
title: Agent Delegation
description: Let an agent act with a bounded, time-limited subset of a user's Hub access, and review and end those grants.
---

Agent delegation lets a user give an agent they own, or created, a bounded and time-limited subset
of their own Hub access. For example, an agent might read agents in other projects the user can
see, message them, or attach to them. The user issues a **grant**. The agent exchanges the grant,
with its own agent token, for a short-lived **delegated credential** and uses that credential on
the Hub.

Every use is checked again against the issuer's live access at that moment. If the issuer loses
access, is suspended or deleted, or stops controlling the agent, or the grant is revoked or
expires, the agent's delegated access stops.

:::caution[Experimental]
Issuance and exchange are behind the `hub.agent_delegation` experiment (default off). See
[Experiments](/scion/reference/experiments/).
:::

## What a delegated decision bounds, and what it does not

**The grant bounds each authorization decision, not the computation that follows it.** Each
delegated request is allowed only for what the issuer may do right now on that target, reduced
by:

- the grant's frozen permission ceiling;
- its boundary (one project, or the Hub);
- the Hub delegation policy;
- access constraints.

What happens after an allowed decision is not bounded by the grant:

- **`agent:message`** instructs an independently authorized recipient. The recipient acts with its
  own authority, and what it does next is not bounded by the messaging grant. Messages show the
  agent as the sender and the issuer as the authorizer.
- **`agent:attach`** gives shell access in the target agent's container. Before you delegate it, know
  what that includes:
  - **The target's credentials and material:**
    - its agent token (`~/.scion/scion-token`, valid for 10 hours and refreshable);
    - its environment and files;
    - its own sciontool delegation socket, together with any delegated credentials that sciontool
      holds.
  - **The target's downstream authority:** the attaching party can act with it, and the grant does
    not bound it.
  - **Sessions:** an established session can outlive revocation and the credential's expiry.
    Revoking cannot retract material already copied.
  - **When it is checked:** attach is authorized only at the WebSocket handshake and at reconnect.
  - **Broker path only:** a delegated attach uses only the Runtime Broker's terminal path. An agent
    whose attach would take the conduit path (for example, on a managed runtime) is refused with
    `503 runtime_attach_unsupported`.

`scion hub delegation create --help` repeats the attach consequences, and the command prints them
whenever `agent:attach` is selected.

## The experiment flag

`hub.agent_delegation` gates exactly three things:

- the issuance and exchange routes, which answer `404` while it is off;
- the authentication arm for delegated credentials (`scion_adt_…`), which refuses every
  delegated credential while it is off;
- the delegated decision procedure, which checks it again.

**Grant management stays available while the experiment is off.** Grant list, read, revoke,
credential revoke and revoke-all always answer, so you can review and end grants during a
disabled period.

**Turning the experiment off suspends grants; it does not revoke them.** Stored grants and
unexpired credentials work again when it is turned back on. To end grants for good, revoke them,
or use revoke-all.

An optional operational setting, `agent_delegation_policy`, can narrow the delegable permissions
per boundary kind. It can never widen them. See [Server configuration](/scion/reference/server-config/).

## Issuance

```sh
scion hub delegation create <agent> --permission agent:read --permission agent:list --boundary hub --name nightly-triage --expires 7d
```

- **Who may issue:** the agent's owner, or a user in its recorded ancestry, who is admitted to the
  agent's project. Issuance needs an interactive session; an access token, an agent token, a
  delegated credential or a dev session is refused.
- **What may be delegated:** exact read and list permissions, `agent:message` and `agent:attach`,
  each only where the issuer could select it for the chosen boundary. Creating agents, starting,
  stopping and deleting them, and every credential operation are not delegable.
- **Lifetimes:** a grant lasts 7 days by default, at most 30. Each delegated credential lasts
  15 minutes by default, at most 60, and never outlives the grant or the agent token used for the
  exchange.
- **Limits:** at most 2 active credentials per grant, 10 active grants per agent and 50 per issuer.
  There is no subdelegation.
- **Name, purpose and labels** are descriptive, user-supplied text. They never confer identity.

## Exchange

The agent exchanges a grant through sciontool, with its own agent token:
`POST /api/v1/agents/{agentId}/delegations/{grantId}/exchange`. sciontool keeps the delegated
credential in memory only, re-exchanges before it expires and after every agent token refresh, and
serves it to processes in the agent container through a local socket (mode `0600`). It never writes
the credential to a file or to a child process's environment. The scion CLI never consumes a
delegated credential.

A refresh of the agent's own token ends the credentials exchanged with the old token at their next
use. sciontool then exchanges again with the new token.

### Transport authentication in front of the Hub

sciontool sends delegated requests with `Authorization: Bearer scion_adt_…`. In the default
transport header mode, `SCION_TRANSPORT_MODE` unset (`HeaderAuthorization`), the transport OIDC token
is added only when a request has no `Authorization` header. **A delegated request from sciontool
therefore goes out without the transport OIDC token.**

What to configure:

- **The Hub sits behind Google IAP or Cloud Run invoker IAM:** set the agents' `SCION_TRANSPORT_MODE`
  to `iap` or `cloudrun_invoker`. The transport token then travels in `Proxy-Authorization` or
  `X-Serverless-Authorization`, beside the delegated credential, and delegated requests pass the
  proxy.
- **No platform proxy requires a transport credential:** nothing is needed.

## Revoke, revoke-all and lists

| Command | Route | Who may act |
| --- | --- | --- |
| `scion hub delegation list [<agent>]` | `GET /api/v1/agents/{agentId}/delegations`, `GET /api/v1/users/me/delegations` | per grant, as for revoke; grants outside your authority are left out |
| `scion hub delegation get <agent> <grant>` | `GET /api/v1/agents/{agentId}/delegations/{grantId}` | as for list; a grant outside your authority answers `404 grant_not_found` |
| `scion hub delegation revoke <agent> <grant>` | `DELETE /api/v1/agents/{agentId}/delegations/{grantId}` | the issuer; the bound agent (it relinquishes its own grant); the agent's current owner; hub admins, for any grant; project owners and admins of P, for grants bounded to project P only |
| `scion hub delegation revoke <agent> <grant> --credential <id>` | `DELETE …/delegations/{grantId}/credentials/{credentialId}` | as for revoke |
| `scion hub delegation revoke-all --mine` | `POST /api/v1/users/me/delegations/revoke-all` | your own issued grants |
| `scion hub delegation revoke-all <agent>` | `POST /api/v1/agents/{agentId}/delegations/revoke-all` | per grant: the issuer and the agent's current owner, and holders of `agent.delegation.revoke_all` (hub admins, for any grant) |

- **Names:** the revoke-all permission is `agent.delegation.revoke_all`. Its catalog operation is
  `agent.delegation.revokeall`, because operation IDs take no underscore.
- **A hub-bounded grant is out of reach for project admins,** whichever project the agent lives in.
- **Revoke** ends the grant and every credential exchanged from it, in one transaction with its
  audit record. Revoking an already revoked grant answers `204` and records nothing new.
- **Revoke-all** revokes every unrevoked grant it covers, expired ones included, in one transaction.
  - A failure revokes nothing.
  - Grants you may see but not revoke this way are reported as `skipped`, with the reason
    `forbidden`. Revoke those one by one. For example, a project admin's per-agent revoke-all skips
    project-bounded grants, because project roles hold `agent.delegation.revoke` but not
    `agent.delegation.revoke_all`.
  - Revoke-all needs a session; the bound agent relinquishes its grants one by one.
  - The CLI asks for confirmation on a terminal (default No). `--yes` (`-y`) or `--non-interactive`
    confirms; without a terminal and without either flag it refuses.
- **Management needs a session** (or, on the per-agent routes other than revoke-all, the bound
  agent's own agent token). Delegated credentials and access tokens are refused.
- **Responses never carry** a credential, its hash or any prefix beyond `scion_adt_`.
- **Lists are not paginated.**
- **Revoked and expired grants stay listed.** They are never purged. Credential rows are deleted
  24 hours after they expire.
- **Lifecycle changes revoke grants automatically:**
  - **Transactional:** deleting, restoring or reincarnating an agent, and deleting its project.
  - **Best effort:** suspending the agent, and suspending or deleting the issuer.
- **Unsuspending an issuer** makes their unrevoked, unexpired grants work again, as for access
  tokens. To end grants, use revoke-all.

## Dual audit

- **Mutation records (always written).** Every issuance, exchange, revocation (single, credential,
  revoke-all, and lifecycle) and every mutation made with a delegated credential writes a mutation
  audit record. Each record carries:
  - the actor agent;
  - the authorizing user (the issuer);
  - the grant;
  - the exchange agent credential, where one applies.

  Each revoke-all revocation writes its own `agent_delegation_grant_revoke` record.
- **Decision records.** When `hub.authorization_decision_audit_v2` is admitted and the decision
  falls inside the sink's recorded domain, each delegated check also writes a decision record. It
  shows the agent as the actor, the issuer as the authorizing user, the grant, the boundary and the
  reason a denial was made. A delegated allow is always recorded, whatever the sampling rate.
  Mutation records remain the source of attribution.

Responses to a denied delegated request carry no decision reason. Read the reason in the decision
record.
