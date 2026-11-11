---
title: Cleaning Up After Test Identities
description: Operator runbook for removing what expired or deleted test identities created on a test hub, checking a run by absence, and handling resources the hub cannot prove were test-created.
---

This runbook is for operators and CI owners who issue **test identities** on a test hub (see `scion hub test-identity` in the [CLI reference](/scion/reference/cli/)). It covers:

- `scion hub test-identity cleanup`: remove what your expired or deleted test identities created.
- `scion hub test-identity cleanup --expect-absent <file>`: check that a run left nothing behind.
- `scion hub test-identity provenance <type>:<id>`: ask whether the hub recorded one resource as created by a test identity.
- What stays a human decision: resources the hub cannot prove were created by a test identity.

The routes behind the commands (`POST /api/v1/test-identities/cleanup`, `GET /api/v1/test-identities/provenance`) are described under "Test identities" in the [API reference](/scion/reference/api/).

## How the hub knows what a test identity created

When an agent, project, schedule or group is created, the hub records which test identity is at the root of the creating chain (directly, or through that identity's agents and schedules) and who issued that identity. The record is written in the same transaction as the resource, is never taken from the request, and cannot be changed afterwards. It stays on the resource after the identity itself is deleted.

Cleanup selects **only** by that record. It never selects by name, label, owner, creator or email domain. A resource without the record is never removed by cleanup, the purge or the expiry reaper.

## Who can clean up what

Cleanup needs the `test_identity.issue` permission, like issuing. What a run reaches depends on the caller:

| Caller | Reaches |
|---|---|
| An issuer (its own sign-in, or an access token with the `test_identity:issue` scope) | The identities it issued |
| A workload issuer delegation (a target-hub profile, `--hub-profile`) | Only the identities issued through that delegation; never another delegation's, nor the grantor's own |
| An admin sign-in | Every issuer's identities |
| A test identity of any kind, including test identities with admin roles | Nothing: refused with `403` |

In every case a run reaches only identities that have **expired** or been **deleted**. A live identity, and everything it created, is never touched, so two jobs that share an issuer cannot remove each other's live work. Within a reached identity, the run removes its agents (with their files on the broker), schedules, groups and projects, and finally the identity itself if its row still exists. A project that another user or group owns, or that holds resources someone else created, is kept, and the identity's own access to it is removed.

## Run a cleanup

1. **Dry run first.** It lists the identities the run would reach and every target, and changes nothing:

   ```bash
   scion hub test-identity cleanup --dry-run
   ```

2. **Run it:**

   ```bash
   scion hub test-identity cleanup
   ```

   The output lists each identity (`expired` or `absent`) and each target with its outcome: `deleted`, `kept` (a project kept as described above) or `not_done` with the reason. A run that cannot finish, for example because a broker is offline, lists what is left and exits non-zero. Run it again to finish; it is safe to repeat.

3. **With a target-hub profile** (an agent runner working against another test hub), the same command acts as the profile's issuer delegation:

   ```bash
   scion --hub-profile t1 hub test-identity cleanup --dry-run
   scion --hub-profile t1 hub test-identity cleanup
   ```

   In agent mode, `cleanup` and `provenance` run only with a profile for another hub, never against the agent's own hub.

## Check a run by absence

A test run should verify its end state from what it knows it created, not from a count. Record the IDs of the resources the run created, one `<type>:<id>` per line:

```text
# created by run 42
agent:6f1c...
project:0b2d...
schedule:9e44...
```

Then, after the run's identities have expired (or after you deleted or purged them). It does not matter whether the hub's expiry reaper, which purges an identity five minutes after it expires, got there first:

```bash
scion hub test-identity cleanup --expect-absent ./created-ids.txt
```

Before the cleanup, the command asks the hub for each ID's provenance. After it, each ID is reported as one of:

- `cleaned`: proven, and removed by this run.
- `absent (proven by deletion record <id>)`: already gone, removed by the reaper, a purge or an earlier cleanup; the hub proves it by the delete audit record (its `provenance_basis`), and `<id>` is that record's correlation ID.
- `absent`: proven before the run and gone after it.
- `NOT cleaned`: proven but still present, with the reason (for example its identity is still live, or a project was kept).
- `proven:false ... real-admin action required`: the hub has no record, in your issuer scope, that a test identity created or deleted it. Cleanup never removes it and never reports it as cleaned.

The command exits non-zero when any ID is `NOT cleaned` or unproven. Treat an unproven ID as a finding for a human, not as something to retry.

## Explain one resource

```bash
scion hub test-identity provenance agent:6f1c...
```

The answer covers your issuer scope: the identities you issued (with a profile, those of its delegation), or every issuer's for an admin sign-in. A proven resource shows the identity it is recorded under, the issuer and the identity's state (`live`, `expired` or `absent`). A resource that is already gone is shown as deleted, with the correlation ID of its deletion record. An unproven resource shows only `proven:false` and the reason `no fixture stamp`. The hub never returns the owner or creator of an unproven resource, and a resource that does not exist, or another issuer's, gets exactly the same answer.

## Audit

Every removal made by a cleanup, a purge or the expiry reaper writes its usual audit record (`agent_hard_delete`, `schedule_delete`, `group_delete`, `project_delete`, `test_identity_purge` and so on) with a `provenance_basis` field: the identity, its issuer, the record column the removal relied on, the run ID and the trigger (`test_identity_cleanup`, `test_identity_purge` or `test_identity_expire`). The record is written in the same transaction as the removal; if it cannot be written, the removal is not made. A cleanup run also writes one `test_identity_cleanup` record with its counts and issuer scope. Its correlation ID is the run ID printed by the command, and every removal record of the run carries the same correlation ID. If that run record cannot be written after the removals, the command fails (the hub answers `500` with the report), but the removal records exist with the run's correlation ID, so the run's audit trail can be recovered from them.

## Resources the hub cannot prove: a real-admin action

Some resources on a test hub have no provenance record, and cleanup will never remove them:

- **Legacy orphans**: resources created before the hub recorded provenance, including ones whose owner is the development sign-in pseudo-user or a user that no longer exists.
- **Resources of real users**, including a real user's agent in a project a test identity created.
- **Resources outside the hub**: service accounts, cluster role bindings, bucket prefixes, token files. The hub records nothing about them; they stay with the infrastructure owner and that tool's own creation records.

These are removed only by a human administrator acting with their own interactive sign-in, never with a CI credential and never by a pattern sweep. A one-time procedure:

1. **Inventory (read only).** With your own super-admin sign-in, list the candidate agents and projects: for example those whose owner or creator is the development pseudo-user, or whose owner has no user row. For each, record the ID, slug, project, creation time, labels, whether it is held, and whether it is running. Check each with `scion hub test-identity provenance <type>:<id>`; a proven one belongs to the cleanup above, not here.
2. **Decide per item.** For each unproven item, decide to keep, transfer or delete it, and write the decision and the reason down (for example "no test provenance; creator absent"). The hub owner makes this decision.
3. **Delete agents** you decided to delete with `scion delete` under your own sign-in, removing their broker files. A governance hold does not block a delete.
4. **Delete projects** you decided to delete. Project deletion needs a super-admin interactive sign-in.
5. **Check by absence.** Each deleted ID now answers not found, and its container is gone from the broker host.
6. **Keep a receipt**: what was deleted, the basis ("human decision; no automated provenance"), the time and the actor. The hub's `agent_hard_delete` and `project_delete` audit records carry your sign-in as the actor.

Automation must never do these steps for an unproven resource.
