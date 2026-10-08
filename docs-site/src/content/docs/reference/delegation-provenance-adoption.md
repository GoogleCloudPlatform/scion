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

The migration stops at the first hop whose write fails rather than skipping
it and continuing. Hops run top-down and a descendant needs its parent
recorded, so continuing would mostly record skips for the descendants. The
trade-off: a write error that repeats on every start leaves every later
pending hop pending, and they are not adopted automatically. The start-up
summary and the status view report them, and an admin commit can adopt them.

If planning or the snapshot write fails on the first start, the Hub serves
requests with no snapshot (the status view reports `snapshotTaken: false`),
and the next start takes the snapshot, including edges written in between.
Every path rule applies to those edges.

Adoption records are retained indefinitely as evidence. They have no foreign
keys, so they outlive the edges and agents they name.

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

On a service-account assignment, the message asks for an authorized user to
recreate the agent directly, which clears the denial whether the unrecorded
hop is the agent's own edge or an ancestor's. The details name the admin
alternative: adopting the chain through the route above.

No edge or ancestor ID is returned to the caller. The Hub's server log, at
debug level, names the delegate of the unrecorded hop. A hop denied only
because its provenance version is not supported keeps the same denial
without these keys, because adoption does not apply to it. So does a denial
of a permission that no adopted ceiling carries, such as the artifact
permissions, which unrecorded chains never held.

## Admin recovery API

All routes require a Hub system admin on an interactive or dev credential: an
interactive session, or the local development credential on a Hub that
accepts it. The credential kind is read from the request's credential, and a
request with any other credential is refused with 403, including agent
tokens, user access tokens, federated identities and broker requests made on
behalf of a user. No permission is registered for these routes, so they
cannot be delegated.

### `GET /api/v1/admin/delegation-adoption`

Returns `snapshotTaken`, the marker, the cohort header, counts by status and
by reason, a page of records, and the adoptable unrecorded hops that no record
covers. `snapshotTaken` is `false`, and the cohort header `null`, until the
boot cohort snapshot exists.

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

A revert writes the reverting admin to the record's `revertedByKind`,
`revertedById` and `revertedAt` and the restored edge's summary to
`revertSummary`; the adopter (`actorKind`, `actorId`) and `afterSummary` are
kept.

Each written hop gets a `delegation_adoptions` record (origin `admin_commit`)
and a mutation audit record (`delegation_provenance_adoption` or
`delegation_provenance_adoption_revert`) with before and after summaries.
One `delegation_provenance_adoption_commit` record summarizes the commit:
`hops` counts the edges written and, on a revert, `covered_records` counts
the additional records marked `reverted` with those edges.
Adopted edges from a commit record the admin as initiator.

## Reverting

A revert deactivates the adopted edge (cause `adoption_reverted`) and
reactivates the original unrecorded row, so the original denials return.
Adopting again needs a new preview and commit.

An edge is reverted once. When several records point at the same adopted
edge (for example an admin commit and a later boot record that recognized
it), the revert of that edge covers all of them: the preview lists the others
under `coveredRecordIds`, and the commit marks them all `reverted`. A
covered record that names a different original edge than the hop refuses
the hop (reason `covered_original_differs`).

Revoking access does not need a revert: deactivating an adopted ancestor's
edge, deleting the ancestor, or suspending the root user denies descendants
at once through the live delegation walk, and restoring the user restores
access.
