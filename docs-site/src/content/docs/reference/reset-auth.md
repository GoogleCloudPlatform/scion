---
title: Reset Auth and Scope Re-issue
description: Deliver a fresh Hub token to an agent, or re-issue an agent's role scopes from its delegator's current authority.
---

`scion reset-auth` has two jobs:

- **Reset auth** (default): give a running agent a fresh Hub token without restarting it.
- **Scope re-issue** (`--reissue-scopes`): recompute an agent's role scopes from its delegator's
  current authority, record the result, revoke the agent's current credentials and deliver a new
  token. Hub super-admins only, and always audited.

Both require a Hub connection.

## Reset auth

```bash
scion reset-auth <agent-name>
```

The Hub issues a new token for the agent's current run and pushes it into the running container,
which restarts its token refresh loop. Use it when an agent's token expired and cannot refresh
itself, for example after a Hub signing-key rotation. A stopped agent gets a fresh token at its next
start. The token carries the same scopes a refresh would.

## Scope re-issue

A scope added to an agent role later does not reach some existing agents. Artifact access
(`project:artifact:read` and `project:artifact:write`) is the common case. A scope re-issue
recomputes the agent's role scopes from its delegator's current authority, without recreating the
agent.

```bash
scion reset-auth <agent-name> --reissue-scopes --dry-run   # show the change
scion reset-auth <agent-name> --reissue-scopes             # apply it
```

### What it computes

The re-issued scopes equal what the agent would be issued if its delegator created it today, at
its current role or lower:

- **Role**: the lowest of the agent's stored role, its delegator's role (for an agent delegator),
  and the project's maximum agent role. A re-issue never raises the role.
- **Delegator**: the delegator recorded on the agent's delegation record, evaluated as it is now.
  - **Agent delegator**: the parent agent's current token scopes and delegation record.
  - **User delegator, session**: the user's current authority.
  - **User delegator, user access token**: that token's own permission ceiling, which is never
    widened. An agent created from a token issued before a scope existed does not gain it; create
    the agent again from a current credential.
  - **User delegator, local development**: only while local development authority is enabled on
    the Hub.
- The operator's own authority is not an input. A super-admin running the re-issue gets the same
  result as anyone else would.

Scopes that the delegator's current authority no longer supports are **removed**, so a re-issue
also works as a clawback. If the role goes down, the agent's stored role goes down with it.

### Refusals

The re-issue changes nothing, records a denial in the audit log, and answers `403` (denied by the
delegation ceiling) when the agent:

- has no delegation record, more than one, or one with no recorded provenance (for example one
  written by the delegation-edge backfill). Recreate the agent.
- names a delegator that no longer exists, has been deleted, or is not active.
- names a user access token that is revoked, expired or missing.
- names a delegator that cannot delegate the agent's role.

A lookup error while reading the delegation chain answers `503`; retry later. A lookup error while
checking a single permission withholds only the scopes covering it, and the response lists them
with the cause `lookup`.

### What it changes

When the result differs from the current state, one transaction:

1. replaces the agent's delegation record (the old record is kept, inactive, with the cause
   `scope_reissue_replaced`);
2. lowers the stored role if the role went down;
3. revokes every active credential of the agent;
4. writes an `agent_scopes_reissued` audit record with the scopes added, removed and withheld, the
   roles before and after, the records replaced and created, and where the authority came from.

A running agent then receives a new token through the reset-auth push, and an
`agent_scopes_reissue_dispatch` record notes whether it arrived. If the push fails, the agent has no
valid token until you run `scion reset-auth <agent-name>`, which issues one from the new record. A
stopped agent receives the new scopes at its next start.

When nothing would change, nothing is written, revoked or pushed. A dry run computes exactly the
same result, writes an `agent_scopes_reissued` record marked `dry_run`, and changes nothing else.

Token refresh, start, restart and Hub startup never re-issue scopes on their own.

### Child agents

A re-issue does not change an agent's children. Re-issue parents before their children, one by
one, or use the bulk mode below, which does this for you.

### Bulk mode

```bash
scion reset-auth --all --reissue-scopes            # dry run: review the summary
scion reset-auth --all --reissue-scopes --apply    # apply
```

Re-issues every agent on the Hub. It is a dry run unless `--apply` is given.

- Every non-deleted agent is processed. If the agent list cannot be read completely, the run stops
  with an error before changing anything.
- Within each project, parents run before their children, so a child is computed against its
  parent's newly recorded authority and additions reach the whole tree in one run.
- Each agent is computed, recorded, audited, revoked and pushed on its own. A refusal or a failed
  push affects only that agent; the summary lists agents changed, unchanged, refused and not
  delivered.
- Running it again is a no-op for agents already up to date.
- An `agent_scopes_reissue_batch` audit record holds the operator, whether it was a dry run, and the
  counts.

### API

| Request | Body |
| :--- | :--- |
| `POST /api/v1/projects/{projectId}/agents/{id}/reset-auth` (or `/api/v1/agents/{id}/reset-auth`) | `{"reissue_scopes": true, "dry_run": false}` |
| `POST /api/v1/admin/agents/reset-auth-all` | `{"reissue_scopes": true, "dry_run": false}` (omit `dry_run` for a dry run) |

Without `reissue_scopes`, both routes keep their reset-auth behaviour.

The single-agent response holds `added`, `removed`, `kept`, `withheld` (scope and cause),
`role_before`, `role_after`, `ceiling_source`, `edge_replaced`, `edge_new`, `credentials_revoked`,
`dispatched`, `noop` and `dry_run`. The bulk response holds `succeeded`, `noop`, `refused` and
`push_failed`, plus one entry per agent with its outcome and diff.

Both require a super-admin on an interactive session. Agent tokens, user access tokens and Hub
admins without super-admin are refused.
