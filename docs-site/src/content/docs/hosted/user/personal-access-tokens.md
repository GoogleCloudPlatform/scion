---
title: User Access Tokens
description: Managing and using user access tokens (UATs) in Scion.
---

Scion supports **user access tokens (UATs)** for programmatic access to the Hub API and for
authenticating CLI operations when browser-based OAuth is not feasible — for example in CI/CD
pipelines or automation scripts.

:::note[Naming]
The canonical term is **user access token (UAT)**, per the root
[`GLOSSARY.md`](https://github.com/GoogleCloudPlatform/scion/blob/main/GLOSSARY.md). You may
still see the older name *personal access token (PAT)* in some places, and the on-the-wire token
prefix is `scion_pat_` — a legacy artifact of that earlier name. The two terms refer to the same
credential.
:::

## Overview

A user access token is a scoped, revocable bearer token linked to your user account, used for
non-interactive authentication. Unlike a full OAuth session, a UAT is **scoped to a single
project** (or, for a [hub-bound token](#hub-bound-tokens), to the projects you can
reach) and carries a specific set of action permissions, so a token minted for CI can do only
what CI needs.

**Note on legacy keys:** the legacy `sk_live_*` API keys have been completely removed. All users
must migrate to `scion_pat_*` tokens.

## Scoping and permissions

Every token has a boundary (a single project, or the hub for a
[hub-bound token](#hub-bound-tokens)) and an explicit list of **scopes** (action
permissions). Available scopes:

| Scope | Grants |
|-------|--------|
| `project:read` | Read project metadata |
| `project:list` | List projects. A project token lists only its own project; on a hub token this scope can be selected only by holders of a hub-wide role that grants it |
| `project:update` | Update project settings, configurations, and annotations |
| `project:manage` | Manage project membership through your own project role, under the same governance rules as a signed-in session; hub role-binding authority is not used through a token. A role is granted only when the token also carries a scope for every permission of that role |
| `agent:create` | Create agents |
| `agent:read` | Read agent status/metadata |
| `agent:list` | List agents |
| `agent:lifecycle` | Start, stop, suspend, restart, restore, and reincarnate agents |
| `agent:delete` | Delete agents |
| `agent:message` | Send messages to agents |
| `agent:attach` | Attach to agent sessions (terminal, exec, env, reset-auth) — your own agents and their descendants |
| `agent:port_access` | Access agent forwarded ports — your own agents and their descendants, plus agents in projects where your role grants `agent.port_access` (project owners and admins). Registering, deleting or clearing an agent's exposed ports also needs `agent.port_access` at hub level for the token's holder |
| `agent:update` | Update an agent's metadata and configuration, report its status, and sync its workspace |
| `agent:stop_all` | Stop all agents of a project (any boundary that contains it), or of the whole hub (hub-bound tokens only) |
| `agent:set_message_mode` | Change an agent's message mode, under the same owner rules as a signed-in session. Granting `hub` mode (by setting it, by a cascade, or when creating an agent) needs a hub-bound token |
| `agent:manage` | All agent scopes except `agent:attach`, `agent:port_access`, `agent:update`, `agent:stop_all` and `agent:set_message_mode` (convenience alias) |

In addition to project and agent scopes, Scion supports UAT scopes for other resource types, among them `skill`, `template`, `harness_config`, `group`, `user`, `broker`, and `gcp_service_account`. Each resource type provides a `*:manage` convenience alias (e.g., `skill:manage`, `template:manage`) that grants all available actions for that resource.

To change your own injected skills (`scion user skills add` or `scion user skills remove`), a token needs the `user_skill_injection:update` scope on a Hub boundary. Reserved permissions (for example, `hub.federation.read`, `hub.federation.update`, `hub.teams_manifest.update`, and `agent.log_append`) are not offered as token scopes and are not part of any built-in role.

You can dynamically discover all available scopes and their descriptions by querying the API:
```bash
curl -H "Authorization: Bearer $SCION_HUB_TOKEN" \
     https://scion.example.com/api/v1/auth/scopes
```

When a token is created, the Hub resolves its scopes into a fixed, versioned set of
permissions (the token's permission ceiling), checks that you hold every one of them in the target
project, and stores that ceiling with the token. Every later request made with the token is
limited to that ceiling. A token whose stored ceiling is missing or has an unrecognized version is
denied. Existing tokens are converted to versioned ceilings automatically on upgrade.

### Hub-boundary tokens for broker registration

Most tokens are bound to one project. A token can instead be bound to the **hub boundary**
(`"boundary": {"kind": "hub"}` in `POST /api/v1/auth/tokens`), which is used for hub-level
resources such as Runtime Brokers. The `broker:create` scope is available only with a hub
boundary:

| Scope | Boundary | Grants |
|-------|----------|--------|
| `broker:create` | Hub only | Register a Runtime Broker, or re-register (issue a new join token for) one your user created, including with `scion hub brokers join-token create`. A token does not let a super-admin re-register another user's broker. Requires that you currently hold `broker.create`, which hub members do. |
| `broker:read` | Hub only | Read Runtime Broker records and the projects a broker serves. `scion runtime-broker register` uses it to check an existing registration. It also reads the environment variables (sensitive values masked) and secret metadata of a Runtime Broker your user created. |
| `federation_token:mint` | Hub only | Obtain a short-lived user access credential for yourself to present to a peer hub (`scion hub auth federation-token`). Experimental: available only while the `federation.project_visitors` experiment is on and the audience is listed in `server.oidc.user_token_audiences`. |

`broker:create` covers registration only. It never associates a broker with a project, never
turns on auto-provide, and never rotates a broker's secret. Associating a broker with a project
needs a sign-in by the broker's owner; see
[Sharing a broker with a project](/scion/hosted/ha/runtime-broker/#sharing-a-broker-with-a-project).
For the full steps, see
[Headless registration with a hub token](/scion/hosted/ha/runtime-broker/#headless-registration-with-a-hub-token).

### Hub-boundary tokens for groups

The group scopes (`group:create`, `group:read`, `group:list`, `group:update`, `group:delete`,
`group:addMember`, `group:removeMember`) are available only with a hub boundary. A group with no
project is a hub-level resource; a project's own groups belong to that project.

- Creating a group needs `group:create`. Creating it under a parent group is also a member add on
  the parent (see below).
- The group list shows only the groups your token may read (`group:read`).
- Adding a member, or creating a group under a parent, never gives the new member more authority
  than the token itself holds. If the group, or any group that contains it, has a hub-wide
  (system-scoped) role binding, a token cannot add members to it. If it has project role
  bindings, the token must hold every permission of those roles in each of those projects: you
  hold them through your own roles, and the token's scopes cover them.
- Changing a group's owner (`ownerId`) with a token needs `group:update` and your current update
  access on the group. For the new owner, the token must also pass the delegation check of a
  member add against the group's role bindings (the previous item); `group:addMember` and the
  group's role hierarchy do not apply. A refused change always returns the same 403, including
  for a group that does not exist.
- A token is held to the group's own role hierarchy: only the group's owner, or an owner or admin
  member of the group, adds members, even when you are a super-admin.
- The members of a project's agents group are the project's agents; no one adds members to it.

### Hub-boundary tokens for user administration

A hub token can change another user's account through `PATCH /api/v1/users/{id}`. Each
field is checked on the hub-level user record, and your user must also hold the matching
permission:

| Scope | Boundary | Grants |
|-------|----------|--------|
| `user:update` | Hub only | Change another user's display name and preferences. A token PATCH of another user that names no `status` field is checked as a profile update, so it needs this scope. |
| `user:suspend` | Hub only | Suspend or reactivate another user (the `status` field). |

- A request that names several fields is written only if every field passes its check;
  otherwise nothing is written.
- A token that cannot use either scope on that user gets the same `403` for every user ID,
  including one that does not exist.
- Changing a user's role, and changing your own profile, still need a sign-in. No credential can
  change your own status or activate an invited user.

### Scopes are restrictions, not grants

Selecting a scope only **limits** what a token may ever be used for — it never by itself grants
access to anything. Whether a request actually succeeds is still checked against your current
authority on the specific target, every time the token is used.

This matters most for `agent:attach` and `agent:port_access`: you may select either scope for a
project before you have created a single agent in it. The token gains no access from selection
alone — each later attach or port request is independently checked against the specific agent.
An attach request succeeds only for your own agents and their descendants. A port request also
succeeds for any agent in a project where your role grants `agent.port_access`, which the
built-in `project-owner` and `project-admin` roles do. Losing project access (for example,
being removed from the project) makes every request against that project fail immediately, even
though the token itself is still otherwise valid.

Concretely, every request made with a token passes all of these checks, in order, and any error
along the way denies the request:

1. The token's boundary is valid.
2. The request's target resolves to a scope (a project or the hub) that the boundary allows.
3. The requested permission is inside the token's stored permission ceiling.
4. For a project target, you still have active access to that project.
5. Your live authority on the target (role bindings, groups, relationship grants, and access
   boundaries) allows the action.

### Checking what you can select

Before minting a token, you can ask which scopes you are currently eligible to select for a given
project, and why a particular scope is not available to you:

```bash
scion hub token scopes --project my-project
```

Or via the API:
```bash
curl -H "Authorization: Bearer $SCION_HUB_TOKEN" \
     "https://scion.example.com/api/v1/auth/scopes?projectId=<project-id>"
```

Each scope in the response reports `eligible` and, when it is not, a machine-readable
`eligibilityReason` (for an alias such as `agent:manage`, also which member scopes are
ineligible). The **Create Token** form in the web UI uses the same information: once you pick a
project, scopes you cannot select are shown with the reason.

This answers only "may I select this restriction" — it never lists which agents or other targets
the resulting token could reach. If you request eligibility for a project you cannot access, or
one that does not exist, the request is denied identically in both cases, so the response cannot
be used to discover whether a given project ID exists.

## Creating a token

Generate a new token with the Scion CLI:

```bash
scion hub token create \
  --project my-project \
  --name "github-actions" \
  --scopes project:read,agent:create,agent:read,agent:attach \
  --expires 90d
```

- `--project` (required unless `--boundary hub`) — the project name or ID the token is scoped to.
- `--boundary` — `project` (the default) or `hub`. See [Hub-bound tokens](#hub-bound-tokens).
- `--name` (required) — a human-readable label.
- `--scopes` (required) — a comma-separated list of the scopes above.
- `--expires` — a duration (`90m`, `2h`, `30d`, `1y`) or an RFC 3339 date
  (`2026-12-31T00:00:00Z`). Defaults to 90 days; maximum 1 year. `m` means minutes; there is
  no month unit (use `30d` or `1y` for longer). `1y` is always 365 days.
- `--purpose` — an optional description of what the token is for (up to 128 bytes).
- `--label` — an optional `key=value` label; repeat the flag for more (up to 8). Keys are
  lowercase, start with a letter and may contain digits, `_`, `.` and `-` (up to 32 bytes).
  Values are up to 64 bytes. Keys that could be confused with identity or authorization
  fields (for example `user`, `agent`, `owner`, `role` or `scope`) are rejected.

The purpose and labels are descriptive only: they grant no permissions and cannot be changed
after the token is created. The Hub records them, along with the token's identity, in request
logs, authorization decisions and audit records, so you can tell which automation made a call.

The command prints the token value **once**. Store it securely — it cannot be retrieved later.
Each requested scope is checked against your live authority in the project before the token is
written. If a requested scope is denied, the Hub returns `403` with error code
`scope_violation`, with `details.selector` and `details.reason` naming the scope and the reason;
nothing is created. Run `scion hub token scopes --project <project>` to see the full picture
before retrying.

### Hub-bound tokens

A token can instead carry a **hub boundary**, which lets one token work across every project you
can reach, and is the only boundary that can select hub-level scopes such as `hub_health:read`
and `broker:read`. Mint one with the CLI by passing `--boundary hub` and no `--project`:

```bash
scion hub token create --boundary hub \
  --name health-check \
  --scopes hub_health:read,broker:read,agent:list \
  --expires 30d
```

The web UI's **Create Token** form always mints project tokens. Through the API, send
`"boundary": {"kind": "hub"}` instead of `projectId` to `POST /api/v1/auth/tokens`. A missing
boundary never means hub: a request that names neither `projectId` nor `boundary` is rejected
with `400` (`details.reason` `boundary_required`). A hub boundary that also names a project, or a
`boundary` that disagrees with `projectId`, is rejected with `400` (`boundary_invalid`). Token
management requires a signed-in session credential; a UAT cannot mint another token. Every
authenticated caller that is not a user with a session or dev credential (a UAT, an agent, a
Runtime Broker or any other credential) gets the same `403` from `POST /api/v1/auth/tokens`,
decided before the request body is read. With a session credential:

```bash
curl -X POST -H "Authorization: Bearer $SESSION_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"name":"hub-automation","boundary":{"kind":"hub"},"scopes":["template:manage"]}' \
     https://scion.example.com/api/v1/auth/tokens
```

Token responses include a `boundary` object (`{"kind":"project","projectId":"…"}` or
`{"kind":"hub"}`); `projectId` is omitted for hub tokens, so read `boundary.kind` to tell them
apart.

The same per-request checks apply, with these limits:

- **Lists.** Every list needs the exact list permission in the token, on a hub or a project
  token: `project:list` to list projects, `agent:list` to list agents, `skill:list` to list
  skills. Without it the list is empty. A hub token lists only projects (and their resources)
  you currently have access to. A project token lists only its own project. A skill list also
  needs `skill:read`, and its total counts only the skills the token may read. Template and
  harness configuration lists filter each row by `template:read` or `harness_config:read`; with
  `template:list` or `harness_config:list` on a hub token, a holder of hub-wide list access gets
  the unfiltered list. Query filters such as `projectId` only narrow a token's results. A list
  cursor issued to one token does not work for another token, or for a browser session.
- **Delegation.** Grants a token creates must fall inside its boundary. A token can never create
  a system-scoped grant. Under a hub boundary, you must also currently have the matching access in
  the grant's project.
- **Messages.** A message sent to an agent with a token passes the same boundary, ceiling, and
  live project-access checks before any other rule can allow it.
- **Inbox and conversations.** Chat writes made with a token (sending, changing or removing your
  own inbox items, notifications, conversations and direct messages) need `inbox:write`. Reading
  your own inbox, notifications, direct messages, direct-message read state and chat preferences
  needs `inbox:read`. Both scopes act only on your own records. Reading a project chat thread (the
  thread, its messages and your read state on it) or the members of its chat space needs
  `project:read` on its project (agent rows in the member list also need `agent:list`), and
  posting to a thread needs `inbox:write` as well. A project token reaches only records of its
  project, and only while you are a member; a record with no project, such as a direct message
  between users, needs a hub token.
- **Limits and quotas.** Reading hub-wide limit definitions, entitlements and usage needs
  `quota:read`; creating, updating or deleting them needs `quota:read` together with `quota:create`,
  `quota:update` or `quota:delete`.
  These scopes can be selected only on a hub token, and your user must also hold the matching
  hub permission.
- **Project configuration.** Setting the project messaging policy, and template and project
  configuration operations, each require their own permission; project owners hold
  `project.set_messaging_policy`.
- **Hub configuration.** Hub configuration operations admit only a hub token carrying the
  matching selector, for example `hub_lifecycle_hooks:update` to change hub pre-start hooks.
  Hub pre-start hook scripts are redacted in read responses for every credential other than an
  interactive sign-in, so a token sees hub hook metadata but not the script.
- **Integrations, GitHub App, and metrics.** Chat integration, GitHub App, metrics, and
  diagnostics operations admit only a hub token carrying the matching selector (for example
  `hub_integrations:update` or `hub_metrics:read`). Some of these operations need an interactive
  sign-in, and every token is refused for them: writing integration secrets or integration
  settings that configure credentials, authentication, endpoints or host paths; installing or
  updating an integration; and changing the GitHub App configuration.
- **Invitations and the allow list.** Inviting users (single or bulk) and managing invitation
  codes admit only a hub token carrying `user:invite`. Reading the allow list and its domains
  needs `hub_allow_list:read`; adding, importing or removing allow-list entries needs
  `hub_allow_list:read` and `hub_allow_list:update`. Your user must also hold the matching hub
  permission. An invited user gets no role binding or group membership from the invitation.
  Removing an allow-list entry deletes the invited user together with that user's role bindings
  and group memberships, and is refused with `409` while the invited user is a project's last
  owner.
- **Roles, role bindings and access constraints.** These admit only a hub token. Creating,
  importing or duplicating a custom role needs `role:create`, updating one `role:update`, and
  deleting one `role:delete`, each together with `role:read`. Creating or updating a role needs
  your user to hold every permission of the role at hub level, and the token must carry a scope
  for each of them; a
  project-scoped custom role can be bound in every project, so a project role does not count.
  A token cannot create a system-scoped custom role. Binding a custom project role needs
  `role_binding:create`, current project management access to that project, and every
  permission of the role in that project within the token's scopes. Deleting a system-scoped
  binding needs `role_binding:delete`. A token can never create a system-scoped binding or
  delete a super-admin binding. Constraint previews need `access_constraint:admin`; creating a
  constraint, or an update that tightens it (removes permissions from its maximum set, or
  widens or removes its time window), also needs `access_constraint:read`. Deleting a
  constraint, or an update that relaxes it (adds permissions, changes its subject or scope, or
  narrows or adds its time window), needs an interactive sign-in.
- **Your own user scope.** Templates, skills and harness configurations in your own user scope
  admit only a hub token carrying the selector of the action. Your templates need
  `template:read` to list, read or download, `template:create` to create or to clone into your
  scope, `template:update` to update, upload, finalize or reimport, and `template:delete` to delete. A
  user-scope skill needs `skill:create`. Your harness configurations need `harness_config:create`
  to clone into your scope, `harness_config:update` to reimport, and `harness_config:delete` to
  delete, together with `harness_config:read`. Importing or discovering templates or harness
  configurations for your user scope needs `template:create` or `harness_config:create`. A
  project token is refused for all of these, because these records belong to no project. Any
  token can list your injected skills.
- **Global catalog and skill registries.** Creating a global skill needs `skill:create_global`,
  and managing skill registries needs `skill:register`; both can be selected only on a hub
  token. Importing or discovering templates or harness configurations for the global catalog
  needs `template:create` or `harness_config:create` on a hub token. Discovering skills for a
  project needs `skill:create` on a token whose boundary contains the project; discovering
  them with no project needs `skill:create_global` on a hub token. Resolving `gh://` skill
  references with no project needs `skill:read` on a hub token.
- **Schedules.** Creating a scheduled event or schedule needs `scheduled_event:create`; updating,
  re-targeting, pausing or resuming a schedule needs `scheduled_event:update`; cancelling or
  deleting needs `scheduled_event:delete`. The request also passes the event type's own target
  checks, and each fire stays within the recording token's ceiling and boundary. A fire fails once
  that token is revoked, deleted or has expired, so a recurring schedule authored with a token
  stops firing at the token's expiry; pause and resume it with a sign-in or a newer token to record new
  authority. See [Scheduling](/scion/hosted/user/scheduling/#security--authorization).
- **Identity governance.** Reading role definitions, role bindings and access constraints needs
  `role:read`, `role_binding:read` or `access_constraint:read`; reading a principal's
  effective access (the admin effective-access view), explaining another principal's
  authorization, or reading an access constraint's audit history needs `hub_audit:read`; among the
  built-in roles, only super-admin holds the `hub.audit.read` permission. These scopes can be
  selected only on a hub token (for writes to roles, role bindings and access constraints, see
  **Roles, role bindings and access constraints** above). Listing users
  needs `user:list` together with `user:read`, and the list holds only users the token may read;
  among the built-in roles, only super-admin holds the `user.list` permission. Redeeming an invitation code and linking a chat account
  always need a sign-in.
- **Runtime Broker registration.** The `broker:create` scope can be selected only on a hub token.
  A project token cannot create or re-register a Runtime Broker (see
  [Hub-boundary tokens for broker registration](#hub-boundary-tokens-for-broker-registration)).

## Using a token

Authenticate by setting the token in the `SCION_HUB_TOKEN` environment variable:

```bash
export SCION_HUB_TOKEN="scion_pat_..."
scion list --project my-project
```

When no stored interactive login exists, the CLI uses the token for all communication with the
Hub.

:::caution[A stored login takes precedence]
A stored interactive login (from `scion hub auth login`) takes precedence over
`SCION_HUB_TOKEN`. To run the CLI under a scoped token, use an environment with no stored login:
a dedicated OS user, an isolated `HOME`, or log out first (`scion hub auth logout`).
:::

### Scopes for CLI use

Most CLI commands that run in a project look the project up on the Hub first, which needs
`project:read`, and `project:list` when the CLI finds the project by its git remote, slug or
name. Include `project:read` and `project:list` in every token you use with the CLI. Scopes
common CLI flows need:

| Flow | Scopes |
|------|--------|
| Any command run in a project | `project:read`, `project:list` |
| `scion list` | `project:read`, `agent:list` |
| `scion look`, `scion logs` | `project:read`, `agent:read` |
| `scion start` / `scion create` | `project:read`, `agent:create`, `agent:read` |
| `scion message` | `project:read`, `agent:message` |
| `scion attach` | `project:read`, `agent:attach` |
| `scion stop`, `scion suspend`, `scion resume`, `scion restore` | `project:read`, `agent:lifecycle` |
| `scion delete` | `project:read`, `agent:delete` |
| `scion project service-accounts list`, `scion service-accounts list` | `project:read` (a project's accounts) |

A token without `project:read` gets `404 Not Found` on the project lookup. The CLI reports this
as a likely missing `project:read` scope and stops. A user access token cannot register a new
project, so the CLI does not try to link or register the project under one. Link the project
once with an interactive login (`scion hub link`), then use the token.

Token scopes limit what the CLI can do on the Hub. Local actions, such as `scion clean` and
`scion delete --cleanup-only --force`, act on the local machine with the user's file permissions,
and token scopes don't limit them.

To use the CLI from a coding agent running on your machine, see
[Using the scion CLI from a coding agent](/scion/hosted/user/coding-agent-cli/).

### What scoped tokens cannot do

Because a UAT is scoped, some operations that must be re-checked later, or that rely on
owner or administrator shortcuts, require an unscoped sign-in (CLI or Web UI login) instead:

- **Chat scheduled messages**: creating, listing, sending now, dismissing or cancelling a message
  scheduled in a chat conversation.
- **Runtime Broker registration**: creating a Runtime Broker (`POST /api/v1/brokers` and the
  embedded Runtime Broker path of project registration) admits a UAT only when it is a
  hub-boundary token carrying `broker:create` and your user holds `broker.create`; any other UAT
  is denied with `403`. Re-registering an existing Runtime Broker (issuing a new join token) with
  such a token works only for a Runtime Broker your user created: a UAT does not satisfy the
  super-admin shortcut. See
  [Runtime Broker](/scion/hosted/ha/runtime-broker/#broker-registration-permission).
- **Broker secret rotation and association**: no UAT can rotate a Runtime Broker's HMAC
  secret or carry the broker owner's consent to associate a broker with a project. See
  [Runtime Broker](/scion/hosted/ha/runtime-broker/#broker-ownership).
- **Runtime Broker changes**: updating or deregistering a Runtime Broker, writing its
  environment variables or secrets, and sending a heartbeat on its behalf need a sign-in; a UAT
  is denied with `403` and `details.reason` naming why.
- **Image operations on a Runtime Broker**: pulling or removing a harness-config image on a
  named Runtime Broker (`broker_id`) needs dispatch authority on that broker, which no UAT
  scope carries, so a UAT is denied even on an auto-provide broker.

## Trust level separation

It is crucial to distinguish how **users** authenticate with the Hub from how **agents**
authenticate with the Hub. Scion uses two separate environment variables to enforce strict
privilege boundaries:

### `SCION_HUB_TOKEN` (user level)
- **Purpose**: Authenticates a human user or a CI/CD pipeline.
- **Scope**: Grants access based on the user's permissions and the specific scopes assigned to
  the token.
- **Usage**: Used by the Scion CLI or external scripts calling the Hub API.

### `SCION_AUTH_TOKEN` (agent level)
- **Purpose**: Authenticates an agent running within a container.
- **Scope**: Carries a Hub-issued JWT scoped specifically to that agent. It is short-lived,
  auto-injected by the Runtime Broker, and grants only the specific permissions that agent needs
  to function (e.g., reporting status, reading its own secrets).
- **Usage**: Automatically used by the `sciontool` binary running inside the agent.

:::danger[Privilege escalation risk]
**Never inject a `SCION_HUB_TOKEN` (or a user-level UAT) into an agent container as the
`SCION_AUTH_TOKEN`.**

Injecting a user token into an agent means the agent will operate with your full user
permissions, rather than its intended, restricted scope. This allows the agent to create other
agents, access other projects, or read secrets it shouldn't have access to. The Scion runtime
automatically handles agent authentication; you do not need to manually configure agent tokens.
:::

## Managing tokens

Tokens can be managed either via the CLI or the Web UI.

### Using the Web UI
The easiest way to administer your tokens is through the **Web UI management interface**
available in your user profile. It lets you create, view, and revoke tokens visually, and
configure action permissions and project-level scopes.

### Using the CLI

List your tokens (name, ID, visible prefix, status, expiry, and scopes):

```bash
scion hub token list
```

Revoke a token — it stops working for authentication but remains visible in listings as
revoked. Log and message streams and terminal sessions opened with the token end within
about 15 seconds of the token being revoked or expiring, of its user being suspended, or of
the user losing the permission the stream needs:

```bash
scion hub token revoke <token-id>
```

Delete a token entirely (rather than leaving it in listings as revoked):

```bash
scion hub token delete <token-id>
```
