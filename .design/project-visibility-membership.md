# Project Visibility via Membership (Subtractive Model)

## Status
**Approved (design)** — all 9 open questions resolved with ptone@google.com on
2026-06-05; ready for an implementation-planning pass. Supersedes the core
approach of `access-visibility.md` (visibility-as-stored-enum). Preserves that
doc's terminology feedback and agent-inheritance notes; reframes the mechanism
around group/role membership.

### Resolved decisions (summary)
- **OQ2** read-only tier → role-aware single members group: member=read-only,
  admin=create/manage, owner=full; migrate existing members → admin.
- **OQ3** agents are team members → agent read derives from project; drop per-agent
  visibility.
- **OQ4** "everyone" = `hub-members` added as a real member row; "public" term and
  the visibility dropdown removed; Members-card hint added.
- **OQ5** top-level agent list = caller's projects only (`IN` filter + new
  `project_id` index); no denormalization.
- **OQ1** narrow read-all via explicit per-type allows; GATE {project, agent,
  broker}; KEEP {user, group, template, harness_config}; tighten sensitive
  {policy, gcp_service_account, secret/env/variable} (project-scoped derive from
  associated project; hub-level admin/owner-only).
- **OQ6** broker read = owner + admins + members of any contributing project.
- **OQ7** terminology dissolved (no user-facing visibility labels remain).
- **OQ9** templates/harness configs deferred to a follow-up; grove-cleanup agent
  spawned for legacy terminology.
- **OQ8** fail closed — no anonymous access; auth always required.

---

## 1. Problem & Key Finding

The Hub create-project dialog offers private/team/public, but visibility is
**stored and never enforced** — and, more importantly, *everyone can already see
everyone's projects*. The reason is not "unimplemented default-open"; it is an
explicit global grant.

**Root cause (the load-bearing fact):** `pkg/hub/seed.go` seeds a hub-wide
policy `hub-member-read-all`:

```
scope=hub, resourceType="*", actions=[read, list], effect=allow
```

bound to the `hub-members` group. Every user is auto-enrolled into `hub-members`
on login (`ensureHubMembership`). So one policy grants every user read+list on
**every resource type**, projects included. This is why visibility is currently
moot.

**Consequence:** Implementing visibility is primarily a **subtractive** change —
*stop globally granting read*, then let membership decide who sees what.

---

## 2. Core Principle: Visibility is Emergent from Membership

Visibility is **not** a fixed attribute chosen at project creation. It is a live
reflection of the project's current membership/role state, which can change over
time. We remove the creation-time selector entirely.

The existing membership plumbing already supports this with **one source of
truth**:

- The project "Members" panel in the web UI *is* the auto-created
  `project:<slug>:members` group (`<scion-group-member-editor>` bound to that
  group's ID). List/add/remove all go through `/api/v1/groups/{id}/members` →
  `GroupMembership`. No second collection to sync.
- Groups can contain **other groups** (memberType="group", cycle-checked via
  `WouldCreateCycle`), and `GetEffectiveGroups` resolves nesting transitively
  (BFS up `parent_groups`). So a reusable cross-project team group can be dropped
  into many projects.
- The "all hub users" group already exists: **`hub-members`** (seeded, every user
  auto-joined). It is GroupType `explicit` with login-time auto-enrollment
  (materialized rows), not a virtual group — but functionally it is "everyone on
  the hub," and `GetEffectiveGroups` + the policy engine honor it natively.

### 2.1 The three levels expressed as membership

| Level | Meaning | Mechanism |
|-------|---------|-----------|
| **private** | Owner (+ explicitly added members) only | Default. Members group has only the owner. |
| **team** | The project's collaborators | Members group has users and/or nested groups. |
| **everyone** (public) | All hub users (read-only) | Add the **`hub-members`** group to the project, at a read-only role. |

"public" deliberately means **everyone on the hub**, never outside it
(per terminology feedback in `access-visibility.md`: prefer "everyone" over
"public"; "grove-team"/"project-team" over "team"). Visibility is read-only;
mutations remain guarded by role/policy.

No creation-time choice is required for any of these. private↔team is just "who's
in the members group"; everyone is "is `hub-members` one of the members."

---

## 3. Required Changes

### 3.1 Narrow the global read-all grant (the core subtractive step)

`hub-member-read-all` must stop granting read on membership-gated resource types.
Per ptone: at minimum **projects, agents, brokers**.

**Decision (ptone, 2026-06-05) — RESOLVED:**
- **Mechanism (a):** replace the `"*"` allow with **explicit per-type read/list
  allow policies**, only for the types that stay hub-readable. (Rejected the
  keep-wildcard-with-denies approach.)
- **GATE (membership-derived, removed from global read):** project, agent, broker.
- **KEEP globally member-readable (directory/catalog):** user, group, template,
  harness_config. (template/harness_config later honor their own visibility +
  grove attachment — OQ9.)
- **SENSITIVE — tighten now:** policy, gcp_service_account, secret / environment /
  variable. These were world-readable via the wildcard and should not be. Per
  ptone, the project-scoped sensitive resources should derive access from the
  project(s) they're associated with (same membership-gating as agents), and many
  have no UI outside project settings anyway — so project-membership-scoped read
  is the right model for them; hub-level ones (e.g. global policies) go
  admin/owner-only.

### 3.2 Add project-scoped read for members (required, not optional)

The per-project members policy currently grants only `agent: create, stop_all`
(`project:<slug>:member-create-agents`) — **not read**. Members can read today
*only* because of the global grant. After 3.1, we must add a project-scoped read
grant so members keep visibility into their own projects:

```
scope=project, scopeID=<project>, resourceType=project|agent, actions=[read,list],
effect=allow  → bound to project:<slug>:members group
```

When `hub-members` is added to the project (everyone/public), it inherits this
same read grant → all hub users can read that project. This is exactly the "add
the all-users group to a role" model.

### 3.3 Read-only role tier (the "everyone is safe" prerequisite) — RESOLVED → B

**Decision (ptone, 2026-06-05): Option B — role-aware policies on the single
members group**, keeping one group / one Members panel / one source of truth.
Three roles to start (explicitly noted as a starting point; fine-grained
permissions may be refined later):

| Role | Grants |
|------|--------|
| **member** | read-only (read/list project + its agents) |
| **admin** | member + create/manage agents (today's "member-create-agents") |
| **owner** | admin + manage membership and visibility |

**Enforcement decision (ptone, 2026-06-05): subtractive-only for now.** The policy
engine cannot condition a `PolicyBinding` on a member's role today
(`PolicyBinding` is `(PrincipalType, PrincipalID)` only; `GetEffectiveGroups`
returns group IDs, dropping role). Rather than extend the engine, we reuse the
existing `isProjectOwnerOrAdmin` role bypass (`pkg/hub/authz.go`, already checks
`role==admin||owner`): read is granted by a project-scoped policy bound to the
members group (all roles read), and `create/stop_all` is **removed** from that
policy so create/manage flows from the admin/owner bypass. No policy-engine change.
This is why existing members are bumped to `admin` on migration.

> **Future improvement (deferred):** make policies genuinely role-aware by adding a
> `role` field to `PolicyBinding`, carrying role through `GetEffectiveGroups`, and
> filtering on it during policy evaluation. This would let role survive group
> nesting (the subtractive approach's one gap: a user who is admin only via a
> *nested* team group gets read but not create) and enable finer per-action grants.
> Not required to meet this design; revisit when nested-admin or per-action
> permissions are needed.

Implications:
- "everyone/public" = add `hub-members` at role=**member** (read-only) — safe.
- Read-only sharing of an individual/group = add at role=member; collaborators who
  can act = admin.
- **Migration cost:** existing members currently get create-agent via the members
  policy; to preserve that, bump existing `member` rows to `admin` during rollout
  (one-time backfill). New default for added members is read-only.
- The per-project policy set becomes role-conditioned (read bound to all roles;
  create/stop bound to admin+owner; member/visibility management to owner).

### 3.4 Close the two enforcement gaps

- `getProject` (single GET by id) does **not** enforce read today — add a
  `CheckAccess(ActionRead)` gate. Same for single-agent GET if similarly open.
- `listProjects`/`listAgents` return everything when identity is nil. **RESOLVED
  (OQ8): fail closed** — a nil/unauthenticated identity sees nothing (empty/401);
  authentication is always required. No anonymous read surface.

### 3.5 Cross-project agent list filtering + performance

The top-level agent list must return only agents in projects the user can see
(member/owner) **plus** agents in "everyone" projects. Findings:

- `AgentFilter.MemberProjectIDs` already pushes a `project_id IN (...)` predicate
  into SQL — good. The handler already computes `resolveUserProjectIDs` and
  applies it for `scope=shared`; we extend it to the default scope.
- **Missing index:** the agents table only has a composite unique
  `(slug, project_id)` index; there is no standalone `project_id` index. Add
  `index.Fields("project_id")` — cheap, makes the IN filter efficient.
- `resolveUserProjectIDs` cost is the BFS in `GetEffectiveGroups` (≈2–15 queries
  depending on group nesting) + `GetGroupsByIDs`. Compute once per request and
  cache in request context.
- **Agent-list scope (RESOLVED → A, ptone 2026-06-05):** the top-level
  cross-project agent list shows only agents in projects the caller is a
  member/owner of — `WHERE project_id IN (your project set)`. **No denormalization
  needed.** Agents in "everyone"/hub-members projects the caller hasn't joined are
  still fully readable on demand via the per-project agent list and single-agent
  GET (both use derived project read); they simply don't appear in the personal
  cross-project firehose.
- Concrete plan: (1) always apply `MemberProjectIDs` for the default scope (not
  just `scope=shared`); (2) add `index.Fields("project_id")` to the agent schema;
  (3) cache `resolveUserProjectIDs` once per request in context.
- Pagination stays honest because filtering is in SQL (no post-hoc drop), so
  `totalCount` and cursors remain accurate.

### 3.6 Retire the creation-time visibility input (RESOLVED)

**Decision (ptone, 2026-06-05):** Remove the visibility selector from the
create-project dialog entirely, and retire the user-facing term "public." New
projects default to creator-only (private by emergence). "Everyone" visibility is
achieved post-creation by adding the `hub-members` group as a member (OQ4,
option 1). The project-settings **Members card** gets a small hint, e.g. "To make
this project visible to all hub users, add the hub-members group." The DB
`visibility` column is no longer authored by the user; it may be retired or
repurposed only as a derived cache feeding §3.5's denormalized agent flag (see
OQ5).

---

### 3.7 Agents derive from project (RESOLVED)

**Decision (ptone, 2026-06-05): agents are team members.** Agent read access is
derived entirely from the parent project — if you can read the project, you see
all its agents; otherwise none. The per-agent `visibility` field (currently inert,
hardcoded "private") is dropped. Owner bypass still lets a creator see their own
agent; admin/owner roles still gate create/stop/manage. This removes the
project-vs-agent visibility-ceiling rules from the old design. It also simplifies
the cross-project agent list (§3.5): an agent's readability == its project's
readability, which is what makes the denormalized `project_public` flag on agents
(OQ5) a complete predicate.

## 4. Brokers (RESOLVED → a)

**Decision (ptone, 2026-06-05):** Brokers are hub-level and linked to projects
many-to-many via `ProjectContributor`. Broker **read = owner + hub admins +
members of ANY project the broker contributes to** (resolve via
`ProjectContributor`). Read-only for plain members; create/attach/manage stays
admin/owner-gated.
- Cross-broker list scopes the same way (brokers you own + brokers contributing to
  a project you're a member of) — analogous to the agent-list filter but keyed off
  the broker↔project link table; add a filter and likely an index on
  `ProjectContributor`.
- A freshly-registered broker not yet linked to any project is visible to its
  owner/registrant and admins only until attached.

---

## 5. Open Questions (to resolve one-by-one)

- **OQ1 — Scope of read-all narrowing.** RESOLVED → explicit per-type allows;
  GATE {project, agent, broker}; KEEP {user, group, template, harness_config};
  tighten sensitive {policy, gcp_service_account, secret/env/variable} now —
  project-scoped ones derive from associated-project membership, hub-level ones
  admin/owner-only. See §3.1.
- **OQ2 — Read-only tier shape.** RESOLVED → B (role-aware single members group;
  member=read-only, admin=create/manage, owner=full). Migrate existing members →
  admin. See §3.3.
- **OQ3 — Agent access derivation.** RESOLVED → agents are team members. Agent read
  derives purely from project membership (read the project ⇒ see all its agents);
  drop the inert per-agent visibility field. Creator/owner bypass still applies;
  admin/owner roles still gate create/stop/manage. See §3.7.
- **OQ4 — Everyone/public maintenance.** RESOLVED → option 1: "everyone" is the
  `hub-members` group added as a real member row (role=member). The word "public"
  is retired and the visibility dropdown is removed from project creation. The
  Members card gets a hint: "to make this visible to all hub users, add the
  hub-members group." See §3.6.
- **OQ5 — Denormalization for the agent list.** RESOLVED → A: top-level list =
  caller's projects only (`project_id IN (mine)`); no denormalization. Add the
  `project_id` index; cache `resolveUserProjectIDs` per request. "Everyone"
  projects' agents readable on demand via project/single-agent views. See §3.5.
- **OQ6 — Broker visibility model.** RESOLVED → a: broker read = owner + admins +
  members of any contributing project; list scoped the same way. See §4.
- **OQ7 — Terminology.** RESOLVED (dissolved by OQ4/OQ6): the visibility dropdown
  is removed and "public" is retired, so there are no user-facing visibility-level
  labels left to rename. User-facing vocabulary is now just project roles
  (member/admin/owner) and the informal "all hub users" in the Members-card hint.
  The legacy `visibility` constants/column become internal-only/retired.
- **OQ8 — Unauthenticated access.** RESOLVED → fail closed: nil/unauthenticated
  identity sees nothing (empty/401); auth always required; no anonymous read. See
  §3.4.
- **OQ9 — Other visibility-bearing resources.** RESOLVED → DEFER. Templates &
  harness configs (visibility + grove-attachment semantics) are out of scope for
  this pass and get their own follow-up design. They stay globally listable for
  now (KEEP list, §3.1) so nothing breaks. Separately, ptone asked to spin up a
  dedicated agent to clean up legacy "grove" terminology in this area (see note
  below).

> **Follow-up agent (grove→project cleanup):** spawned to assess/clean legacy
> "grove" references — primarily the Template/HarnessConfig visibility middle-tier
> value `"grove"` (→ `team`/`project`), and to evaluate the broader
> `groveId/groveName/grove/grovePath` JSON aliases in `pkg/store/models.go`,
> `pkg/api/types.go`, `pkg/hub/template_handlers.go`. Caution: many of those JSON
> tags are intentional wire backward-compat — rename cosmetic/internal uses, but
> flag (don't silently remove) anything that changes the API wire format.

---

## 6. Migration Notes

- Removing `hub-member-read-all` (for the 3 types) is the only behavior-changing
  step; everything else is additive (new project-scoped read grants, index,
  resolver caching).
- Existing projects: backfill the project-scoped member read policy (idempotent,
  alongside the existing `createProjectMembersGroupAndPolicy`).
- No user-facing visibility data migration needed if the column is retired; if
  repurposed as a cache, derive it from membership on first access.

---

## 7. References

- `access-visibility.md` — prior (stored-enum) design + inline ptone feedback.
- `pkg/hub/seed.go` — `hub-member-read-all`, `hub-members`, `ensureHubMembership`.
- `pkg/hub/authz.go` — CheckAccess flow, effective-group resolution.
- `pkg/hub/handlers.go` — listProjects/listAgents, getProject, resolveUserProjectIDs,
  createProjectMembersGroupAndPolicy.
- `pkg/store/entadapter/{group_store,agent_store,project_store}.go` — filters,
  GetEffectiveGroups, indexes.
- `pkg/ent/schema/{group,agent,project}.go` — schemas/indexes.
