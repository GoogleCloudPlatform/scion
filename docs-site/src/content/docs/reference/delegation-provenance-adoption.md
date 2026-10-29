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

### Legacy exception: the owner's own user secrets for adopted baseline and readonly agents

The V1 table gives `secret.use` to `full` only, so the user secret parent
ceiling refuses an adopted `baseline` or `readonly` agent its owner's
progeny-shared user secrets. Before that ceiling applied to agent start and
restart, these agents received them there. As a **legacy compatibility
exception**, the Hub keeps that behaviour at start and restart for exactly
this cohort:

- the agent's own edge (no agent above it), delegated by the agent's owner;
- written by adoption (source credential `system_migration`) and bounded;
- whose ceiling is exactly the `baseline` or the `readonly` V1 row (with or
  without the assigned service account permissions), and that row's role
  does not rank above the edge's recorded role. Adoption ceilings an edge at
  the lower of its role and the agent's applied role but keeps the edge's
  role, so an edge recorded as `full` for an agent whose applied role is
  `baseline` carries the `baseline` row and is in the cohort, reported as
  `adopted_baseline`;
- with an adoption record under policy V1 (`adopted` or `recognized`) for
  that edge.

This exception covers the owner's own user secrets, through `secret.use`. A
separate fallback covers the Project, Hub and Broker variables and secrets of
legacy agents, through the delivery permissions: see
[Launch material for legacy agents](#launch-material-for-legacy-agents). Its
`legacy_adopted_*` groups are keyed on the adopted edge's ceiling at any depth,
so an agent can fall in that group and not in this one, or in both.

For such an agent the ceiling lets the missing `secret.use` pass, and every
other check still applies: a held or deleted agent, an agent-delegated chain,
an edge recorded any other way, a terminal user who is inactive or not
admitted to the project, and the secret source check (the secret must be the
owner's own, shared with progeny and authored by the owner) all still deny.

The runtime read (`scion secret get` inside the agent, the agent secret
fetch) and the agent secret list are **not** restored for this cohort. Since
ptone/scion#2072 every runtime read and list has required the agent token
scope `project:secret:read`, and `baseline` and `readonly` tokens never
carry it, so these agents are refused there (`capability_required`), as they
were before the ceiling. To give such an agent its owner's secrets at
runtime, reincarnate it as its owner with role `full`.

An edge written later for such an agent, for example by a scope re-issue,
keeps the `system_migration` source but has no adoption record of its own,
so the agent leaves the cohort and its owner's shared secrets are withheld at
start again (fail closed). Reincarnating the agent as its owner with role
`full` restores them.

Each use at start or restart is counted
(`scion.hub.user_secret.legacy_adopted_fallback`, attributes `path` and
`role`) and logged at warning level as `user secret allowed under the legacy
adopted-agent fallback`, with the agent, the cohort (`adopted_baseline` or
`adopted_readonly`), the edge and the secret name, never the value. The
warning is logged at most once every 10 minutes per agent and secret. **The
exception ends with the planned policy V2 backfill**, which is to re-ceiling
these edges; the counter shows when it is no longer used.

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
   transaction per hop. Each hop re-plans the delegate against current
   state, checks the record's original edge and before-fingerprint, then
   deactivates the original row (cause `provenance_adopted`) only if it is
   still active with unrecorded provenance, and inserts the adopted row. A
   hop whose state changed is recorded as `skipped_changed`.
3. When no pending record remains it runs the **retry pass** (below), then
   writes the marker with the current `retry_version`.

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

### Retry of skipped records

The retry pass re-applies the snapshot's `skipped_changed` records whose
reason is `edge_changed` or `ancestor_not_adopted`, top-down, through the
same per-hop path as a pending record. Every path rule still applies: the
hop must still be adoptable, name the record's original edge and match its
before-fingerprint, and an agent delegator's own hop must already be
recorded. A retried parent that is adopted unblocks its child in the same
pass. A hop that really changed is skipped again, with its current reason.
Records skipped for any other reason (for example `fingerprint_changed`)
are not retried.

The pass exists because earlier builds compared the edge's `updated` time
as stored text when deactivating the original row. On SQLite, a row whose
timestamp text was not in canonical form (RFC 3339 with `Z`, a non-UTC
offset, or a monotonic clock suffix) failed that check although it was
unchanged, so the hop was recorded `skipped_changed`/`edge_changed`, its
descendants `skipped_changed`/`ancestor_not_adopted`, and the marker was
still written. The check no longer compares the timestamp.

A marker whose `retry_version` is lower than the Hub's (including a marker
with no `retry_version`, written by an earlier build) makes the next start
run the retry pass once over the marker's cohort, then rewrite the marker
with the new `retry_version` and counts. No new snapshot is taken and no
admin step or database edit is needed. If a hop write fails during the pass,
the marker is left as it was and the next start runs the pass again.

## Service-account assignment backfill

The GCP token mint and the GCS link proxy also require the agent's active
**service-account assignment**: the row that records who authorized the
agent's `assign`-mode service account, with that principal's frozen ceiling.
Agent create, a service-account PATCH and reincarnation write it. An agent
created before assignment rows were recorded has none, and its token
requests are refused with `ceiling_provenance_missing`.

A one-shot data migration at Hub start (marker
`sa_assignment_legacy_backfill`, after schema migration and so after
adoption) writes the missing row for every live `assign`-mode agent that has
no active row. It copies the agent's single active delegation edge in its
project, because agent create records the same provenance and ceiling on
the edge and on the assignment:

- An edge recorded from a session, access token, agent, local development
  or schedule credential is copied as is.
- An edge recorded by adoption (`system_migration`) is not an accepted
  assignment source. The row records the edge's delegator as the source,
  with credential kind `agent` for an agent and `session` for a user, and
  the edge's ceiling.

Rows carry origin `legacy_backfill`. The migration never replaces an existing
row. It skips an agent, logging a warning with the reason, when the agent
has no active edge or more than one (`no_active_edge`, `ambiguous_edges`),
the edge is the migration sentinel or unrecorded
(`migration_sentinel_edge`, `unrecorded_edge`), the edge's ceiling does not
allow `gcp_service_account.assign` (`ceiling_lacks_assign`), or the delegator
is deleted or not an active user (`delegator_missing`,
`delegator_inactive`). A skipped agent keeps its denial. The usual
remediation is to reincarnate it with the same role, as a user who holds
`gcp_service_account.assign`. Skips do not block the marker, and a store
error leaves the marker unwritten so the next start retries.

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
reincarnate the agent, or to recreate it directly. Either clears the denial
whether the unrecorded hop is the agent's own edge or an ancestor's. A user's
reincarnation that keeps the role re-records the agent's edge with the user as
delegator, as a user's create does, when the agent's own edge is unrecorded or
an edge above it is unrecorded and every edge between them is accepted on this
Hub. The denial is only reported in that case, because the check walks up from
the agent and stops at the first edge that fails, so the reincarnation clears
it even when an edge further up (for example one recorded with local
development credentials on a Hub without dev auth) is not accepted. A
reincarnation by the agent itself or by another agent keeps the edge and does
not clear it. The details name the admin alternative: adopting the chain
through the route above.

No edge or ancestor ID is returned to the caller. The Hub's server log, at
debug level, names the delegate of the unrecorded hop. A hop denied only
because its provenance version is not supported keeps the same denial
without these keys, because adoption does not apply to it. So does a denial
of a permission that no adopted ceiling carries, such as the artifact
permissions, which unrecorded chains never held.

## Launch material for legacy agents

When the Hub starts or restarts an agent, it authorizes each Project, Hub and Broker variable and secret, and each Project secret it sends for the broker to fetch a `gh://` skill, with the delivery permissions above (`secret.deliver`, `env_var.deliver`, `skill_injection.deliver`), walking the agent's delegation chain. Agents whose chain fails that walk only because of legacy data keep receiving that material, as they did before the check existed. A hop is legacy when it is one of:

| Group (`cohort` in the log) | The hop's agent has |
| :--- | :--- |
| `legacy_no_edge` | no active edge in the project |
| `legacy_sentinel_edge` | only the backfill's migration edge (`user:system/migration`) |
| `legacy_unadopted_edge` | only an edge with unrecorded provenance, including scheduled children with role `none`, which adoption skips |
| `legacy_adopted_readonly`, `legacy_adopted_baseline`, `legacy_adopted_capped` | only an adopted edge (source `system_migration`) whose ceiling lacks the delivery permissions only because policy V1 capped it: the ceiling lies within the V1 row of the edge's own role. The group is named after the lowest V1 row the ceiling fits in (`readonly`, then `baseline`; `capped` if neither), whatever the edge role. Adoption keeps the edge role but caps the ceiling at the lower of the edge and applied roles, and intersects a child's ceiling with its adopted parent's. So an edge of role `full` can carry the `baseline` or `readonly` row, and it is served here. An edge re-issued by an agent delegator keeps the `system_migration` source and is judged the same way. |

This fallback covers Project, Hub and Broker items only. The owner's own user secrets of adopted `baseline` and `readonly` agents follow their own exception, under different rules (an exact V1 row, an adoption record and the owner as delegator): see [Legacy exception: the owner's own user secrets for adopted baseline and readonly agents](#legacy-exception-the-owners-own-user-secrets-for-adopted-baseline-and-readonly-agents). One start can use both, each for its own items.

Each item delivered this way is logged at `Warn` at most once an hour per agent and item, as `launch material delivered through the legacy fallback`. The line has the agent ID, the group, `exception` (`legacy`, or `product_gap` for the group below), and the item's name and scope. The Hub remembers the 16384 most recently logged items; beyond that it forgets the oldest first.

### Product-gap exception: agents created from a user access token

One more group is served the same way, although it is not legacy data:

| Group (`cohort` in the log) | The hop's agent has |
| :--- | :--- |
| `uat_created_no_deliver_mapping` | only an edge recorded from a user access token, whose ceiling cannot hold the delivery permissions because no token scope grants them; or it is a descendant of such an agent whose own edge, recorded by its parent agent, lacks the delivery permissions only because the parent could not pass them down (every agent between the two must be of the same kind) |

This covers agents created from a user access token today and in future, so it is a gap in the current product, not a migration leftover. It does not end with a later backfill. It ends only when a user access token scope grants the delivery permissions: from then on the Hub stops treating such edges as an exception by itself. Its removal is tracked as a follow-up issue. The `Warn` line marks it `exception=product_gap`, and the census counts it separately from the legacy groups.

The fallback only ever admits the three delivery permissions. It grants nothing else, whatever group an agent falls in.

The fallback never applies when:

- any agent on the chain is held;
- a delegator or ancestor agent is deleted, missing or held, or the root user is not active;
- the agent has more than one active edge in the project. Such agents are withheld for now: the `Warn` line names the cause `ceiling_provenance_ambiguous`, and the census counts them as `duplicate_edges`. This will be revisited once census numbers from real Hubs are available;
- a hop has local-development provenance and the Hub does not run dev auth;
- a recorded hop that is not one of the groups above has a ceiling without the permission. Examples: an agent whose parent's ceiling does hold the permission, or an adopted edge whose ceiling is not within the V1 row of its role;
- an earlier check (the item's association with the agent, an access constraint, the test-admin policy) refuses the item;
- the chain is fully recorded, which leaves the root user's project admission as the deciding check.

When the walk cannot follow edges past a hop (`legacy_no_edge`, `legacy_sentinel_edge`), the agent's stored ancestry stands in for the rest of the chain: its root user must be active and every ancestor agent must exist, not be deleted and not be held. An agent with no stored ancestry is refused. The root user's project membership is not checked for these agents, which is the behaviour before the check existed.

The fallback applies to start and restart selection, to the skill-fetch credentials sent with them, and to the create-time check for whether a requested key exists. Other users of the delivery permissions get the strict walk. To find the affected agents before or after an upgrade, run the read-only census against a copy of the Hub database or a read replica: `go run ./hack/delivery-census -driver sqlite -db <hub.db>` (add `-dev-auth` for a workstation Hub), or `-driver postgres -db <url>`. It opens the database read-only and reports, per agent, the group it falls in and what a start delivers, with counts by group and by deny cause.

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
