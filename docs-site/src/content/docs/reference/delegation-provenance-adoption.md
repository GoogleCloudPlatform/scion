---
title: Delegation Provenance Adoption
description: Upgrade migration and admin recovery API that give pre-provenance delegation edges a bounded, recorded ceiling.
---

This reference describes how the Hub treats delegation edges written before
authority provenance was recorded, the upgrade migration that adopts them,
and the admin recovery API for status, preview, commit and revert.

---

## Background

Every agent has one active **delegation edge** in its project: the user or
agent that delegated authority to it, the role, and (for edges written by
current Hubs) the recorded **provenance** and frozen **effect ceiling** of
the write.

An edge written before these fields existed reads back with provenance
version 0 and an *unrecorded* ceiling. The delegation walk denies every
permission that requires recorded provenance on such a hop:

- `gcp_service_account.use`, `gcp_service_account.assign`
- `project.secret_read`, `secret.use`
- `secret.deliver`, `env_var.deliver`, `skill_injection.deliver`
- `agent.identity_token`

On a Hub with a project-default service account in `assign` mode, an agent
whose chain includes such a hop cannot create a child: the child takes the
default service account, and the Scion `ActionAssign` check denies it.
Refreshing the agent's token does not change the edge.

**Adoption** replaces a validated unrecorded edge with a recorded one. The
new edge keeps the real typed delegator, delegate, project scope and role,
records source credential kind `system_migration`, and carries a bounded
ceiling taken from a frozen compatibility policy. No session, token or JTI
evidence is recorded for the historic write. Both service-account checks
(Scion `ActionAssign` and GCP `actAs`) apply unchanged.

## Compatibility policy V1

The policy is a literal table per role. It never follows later changes to
the permission registry or role scopes.

| Role | Ceiling |
|------|---------|
| `readonly` | `harness_config.list` `harness_config.read` `project.read` `skill.list` `skill.read` `template.list` `template.read` |
| `baseline` | readonly + `agent.notify` `agent.port_forward` `agent.status_update` `agent.token_refresh` |
| `full` | baseline + `agent.attach` `agent.create` `agent.delete` `agent.lifecycle` `agent.set_message_mode` `gcp_service_account.assign` `project.secret_read` `secret.use` `template.create` `template.update` `gcp_service_account.use` `env_var.deliver` `secret.deliver` `skill_injection.deliver` |

- An agent whose applied configuration already carries an `assign`-mode
  service account also gets `gcp_service_account.assign` and
  `gcp_service_account.use`, so it keeps the token scope for its own account.
- The role used is the lower of the edge role and the agent's applied role.
  The edge's recorded role is unchanged.
- For an edge delegated by another agent, the ceiling is intersected with
  that agent's (adopted or recorded) ceiling, the same rule new children
  follow.
- `agent.identity_token` is in no row.

New children of an adopted agent are created through the normal create path,
so their ceilings stay within the adopted chain.

## Which edges are adopted

The planner examines the ancestor closure of every live agent (a stopped
agent is live). Each hop gets exactly one outcome:

| Outcome | Meaning |
|---------|---------|
| `adopt` | An unrecorded hop on a fully valid path. |
| `recognized` | An edge already carrying a `system_migration` bounded V1 ceiling bound to its own project (from an earlier run or an operational repair). Left as is. |
| `recognized_above_policy` | Recognized, and its IDs are not a subset of the policy for its role. Reported only; never narrowed. |
| `recorded` | A recorded edge on a valid path. Never rewritten. |
| `excluded` | Never adopted automatically. The first failing rule is recorded as the reason. |

Exclusion reasons:

| Reason | Rule |
|--------|------|
| `missing_edge`, `duplicate_active_edges` | The agent has no, or more than one, active edge in its project. |
| `scope_mismatch` | The agent's only edges are in another project, or an agent delegator is in another project. |
| `unsupported_delegator_type` | The delegator is not a user or an agent. |
| `no_principal_root` | The delegator is the backfill sentinel `user:system/migration`. |
| `root_missing`, `root_inactive` | The user delegator does not exist or is not active. |
| `parent_missing`, `parent_deleted` | The agent delegator does not exist or is deleted. |
| `ancestor_excluded`, `cycle`, `too_deep` | A hop above is excluded, the chain repeats, or it is deeper than the walk examines. |
| `delegator_ancestry_mismatch` | The delegator disagrees with the agent's recorded creator. |
| `unknown_provenance_version`, `malformed_provenance`, `malformed_ceiling` | The row is not a clean unrecorded row or a recognized recorded row. |
| `role_none`, `unknown_role`, `unreadable_agent_config` | The edge or applied role is `none` or unknown, or the agent's configuration cannot be read. Scheduled-dispatch children always have role `none`. |

No timestamp selects or excludes an edge.

## Upgrade migration

The migration runs during schema migration at Hub start (under the schema
migration lock on Postgres), after the delegation-edge backfill. Its marker is
the hub setting `migration_delegation_provenance_adoption_v1`. The older
backfill marker `migration_delegation_edge_backfill_v1` does not affect it.

1. On its first run it takes a **cohort snapshot**: one record per examined
   edge in the `delegation_adoptions` table, plus the header setting
   `delegation_provenance_adoption_cohort`, in one transaction.
2. It adopts each `pending` record of that snapshot, top-down, one
   transaction per hop. Each hop deactivates the original row (cause
   `provenance_adopted`) only if it is unchanged, then inserts the adopted
   row. A hop whose state changed is recorded as `skipped_changed`.
3. When no pending record remains it writes the marker.

Later starts never take a new snapshot, so edges written after the snapshot
are not adopted automatically; the status view lists them as
`notInCohort` and an admin commit can adopt them. A write failure does not
stop the Hub from starting: the remaining hops keep their current denial and
the next start resumes the same snapshot.

At start the Hub logs a summary of record counts, and a warning of the form:

```text
delegation provenance adoption: N hops on live agent chains remain unrecorded; review GET /api/v1/admin/delegation-adoption
```

## Denial details

A request denied because a hop is unrecorded keeps its existing message and
adds these keys to the error `details`:

```json
{
  "deny_cause": "ceiling_unrecorded",
  "remediation": "delegation_provenance_adoption",
  "remediation_path": "/api/v1/admin/delegation-adoption"
}
```

No edge or ancestor ID is returned to the caller. The Hub's server log names
the delegate of the unrecorded hop.

## Admin recovery API

All routes require a Hub system admin authenticated by an interactive session
or by the local development credential. Agent tokens and user access tokens
are refused with 403. No permission is registered for these routes, so they
cannot be delegated.

### `GET /api/v1/admin/delegation-adoption`

Returns the marker, the cohort header, counts by status and by reason, a page
of records, and the adoptable unrecorded hops that no record covers.

| Query | Meaning |
|-------|---------|
| `status` | Filter records by status. |
| `reason` | Filter records by reason. |
| `projectId` | Filter by project. |
| `limit`, `offset` | Paging (default 100, at most 500). |

### `POST /api/v1/admin/delegation-adoption/previews`

Plans over current state. Writes nothing.

```json
{
  "operation": "adopt",
  "scope": { "projectId": "<project>", "agentIds": ["<agent>"] }
}
```

For `adopt`, the scope selects live agents whose ancestor closure is planned
(an empty scope plans every live agent). For `revert`, pass `recordIds`, and
optionally `confirmOriginalEdgeIds` (record ID to edge ID) for a recognized
record whose original row is ambiguous.

The response lists each hop with its outcome, reason, delegator, role, and
before and after ceilings, plus `planId` and `planFingerprint`.

### `POST /api/v1/admin/delegation-adoption/commits`

Repeat the preview request and add `planFingerprint` (and optionally
`planId`). The Hub recomputes the plan inside one transaction and:

- returns 409 `stale_authorization_preview` if the fingerprint differs;
- returns 403 `mutation_permission_lost` if the caller has stopped being an
  active system admin;
- returns 422 if the plan writes nothing, writes more than 500 hops, or
  contains a record that cannot be reverted;
- otherwise writes every hop, or nothing.

Each written hop gets a `delegation_adoptions` record (origin `admin_commit`)
and a mutation audit record (`delegation_provenance_adoption` or
`delegation_provenance_adoption_revert`) with before and after summaries.
One `delegation_provenance_adoption_commit` record summarizes the commit.
Adopted edges from a commit record the admin as initiator.

## Reverting

A revert deactivates the adopted edge (cause `adoption_reverted`) and
reactivates the original unrecorded row, so the original denials return.
Adopting again needs a new preview and commit.

Revoking access does not need a revert: deactivating an adopted ancestor's
edge, deleting the ancestor, or suspending the root user denies descendants
at once through the live delegation walk, and restoring the user restores
access.
