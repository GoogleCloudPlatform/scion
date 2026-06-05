# Project Visibility — Implementation Plan

Companion to `project-visibility-membership.md` (approved design). This plan
sequences the work into work-packages (WPs) with concrete file anchors, names the
one real architectural decision the design left implicit, and orders the WPs by
dependency so they can be parallelized across developer agents safely.

Branch: `design/project-visibility-membership` (all work; PR at end, no merge).

---

## 0. The one architectural decision to confirm: how "role-aware" is enforced

The design (§3.3, OQ2→B) wants: **member = read-only, admin = create/manage,
owner = full**, all on the *single* members group. But the policy engine today
**cannot condition a policy binding on a member's role** — `PolicyBinding` is just
`(PrincipalType, PrincipalID)` (`pkg/store/models.go:1232`), and
`GetEffectiveGroups` returns group IDs only, dropping role
(`pkg/store/entadapter/group_store.go:735`).

There are two ways to get role-aware behavior:

- **(Recommended) Reuse the existing role bypass — purely subtractive.** Read is
  granted by a project-scoped read policy bound to the members group → *everyone
  in the group (any role) can read*. Create/stop/manage is **already** gated by
  `isProjectOwnerOrAdmin` (`pkg/hub/authz.go:461`), which explicitly checks
  `membership.Role == owner || admin`. So we **remove `create, stop_all` from the
  members-group policy** and let the existing admin/owner bypass grant them. Net
  effect: member→read-only, admin/owner→create/manage, with **no policy-engine
  changes**. This is why the migration "bump members→admin" is needed: today every
  member gets create via the group policy; after this change only admin/owner do.
  - **Known limitation (flag, don't fix now):** `isProjectOwnerOrAdmin` checks the
    user's *direct* membership row on the project members group. A user who is
    admin only via a **nested** team group would get read (nesting resolves) but
    not create. Acceptable for v1; note as follow-up.

- **(Deferred alternative) Extend the policy engine** with a `role` on
  `PolicyBinding`, carry role through `GetEffectiveGroups`, and filter on it in
  `evaluatePolicies`. More expressive (role survives nesting) but invasive and not
  required to meet the approved spec. Out of scope for this pass.

**This plan assumes the recommended approach.** If ptone prefers the engine
extension, WP-B grows substantially — calling it out before we build.

---

## 1. Work-package sequence & dependencies

```
WP-0 (schema/codegen, SOLO first)
        │
        ├──► WP-B (authz/policy/enforcement/migration)  ← behavior-changing core
        ├──► WP-A (agent-list filter + caching)
        ├──► WP-C (broker read scoping)
        └──► WP-D (UI wiring)            ← independent, can start anytime
```

WP-0 lands first and alone because it regenerates `pkg/ent/**` (codegen); doing it
once avoids merge conflicts in generated files. WP-A/B/C/D then run in parallel.
**WP-B is the only behavior-changing package** — its sub-steps (narrow read-all +
add project read + enforcement gaps) must land *together* and be tested as a unit,
or members lose visibility to their own projects mid-rollout.

---

## WP-0 — Schema & codegen foundation (solo, lands first)

1. **Add `project_id` index on agents** — `pkg/ent/schema/agent.go:176` `Indexes()`
   currently has only `index.Fields("slug","project_id").Unique()`. Add
   `index.Fields("project_id")`. Makes the `project_id IN (...)` filter (§3.5)
   efficient.
2. **Drop the inert per-agent `visibility` field** — `pkg/ent/schema/agent.go:63`
   (`field.String("visibility").Default("private")`). Remove the field and all code
   references. NOTE: ent auto-migration does **not** drop the DB column by default,
   so the column is left orphaned/harmless; a `WithDropColumn` cleanup can follow
   later. Verify no code reads agent visibility for decisions (it's hardcoded
   "private" today, so safe).
3. **Regenerate:** `go generate ./pkg/ent/...` (runs the `ent generate` in
   `pkg/ent/generate.go:17`). Commit generated changes.
4. Leave the **project** `visibility` field in place (`project.go:74`) — the design
   says it may be retired or repurposed; retiring the column is not required and we
   stop authoring it from the UI in WP-D. Document it as internal/legacy.

Verify: `make test-fast build`.

---

## WP-B — Authz / policy / enforcement core (the behavior change)

All in `pkg/hub`. Land these together.

### B1 — Narrow the global read-all grant (§3.1)
`pkg/hub/seed.go:50` seeds `hub-member-read-all` with `ResourceType:"*"`,
`actions:[read,list]`. Replace the single `"*"` policy with **explicit per-type
allow policies** bound to `hub-members`:
- **KEEP** (hub-readable directory/catalog): `user`, `group`, `template`,
  `harness_config` → seed read/list allows for these.
- **GATE** (remove from global read): `project`, `agent`, `broker` → no hub-wide
  allow; visibility comes from membership.
- **SENSITIVE** (tighten): `policy`, `gcp_service_account`,
  `secret`/`environment`/`variable` → no hub-wide read. Project-scoped instances
  derive from associated-project membership (same gating as agents via the
  project read policy in B2 where they share scope); hub-level ones stay
  admin/owner-only (admin bypass already covers admins).
- Seeding is idempotent by policy name; ensure re-seed replaces the old wildcard
  (delete-by-name or upsert) so existing hubs migrate.

### B2 — Add project-scoped member read + role-aware create (§3.2, §3.3)
`createProjectMembersGroupAndPolicy` (`pkg/hub/handlers.go:3633`):
- **Add** a project-scoped read policy bound to the members group:
  `scope=project, scopeID=<project.ID>, resourceType=project` and one for
  `agent`, `actions:[read,list], effect=allow`. (Agents derive from project, §3.7
  — granting agent read at project scope to the members group is the mechanism.)
- **Remove `create, stop_all`** from the existing
  `project:<slug>:member-create-agents` policy (handlers.go:3730). Create/stop now
  flow from the admin/owner role bypass (`isProjectOwnerOrAdmin`). Rename the
  policy to `project:<slug>:member-read` to match its new role.
- Idempotency: on re-run, replace the old create-policy with the read-policy
  (handle existing hubs).

### B3 — Migration backfill: bump existing members → admin (§3.3, §6)
`createProjectMembersGroupAndPolicy` already runs at startup for every project
(`pkg/hub/server.go:777` seedDefaultPoliciesAndGroups loop) and has a backfill
block (handlers.go:~3706 promotes sole member→owner). Add a **one-time, idempotent
bump**: any group member currently at `role=member` who predates this change →
`role=admin`, to preserve their create-agent ability. Guard so it runs once (e.g.
skip if any admin/owner already present beyond the creator, or gate on a stored
"migrated" marker / version). New members added post-migration default to `member`
(read-only) per `addGroupMember` default (`handlers_groups.go:509`) — unchanged.
- **Care:** do not bump the `hub-members` group's members (that would make everyone
  admin everywhere). Only bump per-project `project:<slug>:members` groups.

### B4 — Close enforcement gaps (§3.4)
- **getProject** (`pkg/hub/handlers.go:5104`) does not gate read. Add
  `CheckAccess(ctx, identity, Resource{Type:"project", ID, OwnerID}, ActionRead)`;
  403/404 on deny. Same for **getProjectAgent** (`handlers.go:4827`) — add a read
  check (derives from project per §3.7).
- **Fail closed on nil identity:** `listProjects` (handlers.go:3289),
  `listAgents` (handlers.go:276), `getProject`, single-agent GET currently treat
  `identity == nil` as "skip filter / return everything." Change to: nil identity →
  empty result or 401 (no anonymous read). `CheckAccess` already denies unknown
  identity (`authz.go:95` default deny) — lean on it for the GETs; for the LISTs,
  short-circuit to empty/401 when identity is nil.

Verify: targeted tests for member-can-read-own-project, non-member-cannot,
hub-members-added→all-can-read, nil-identity→empty.

---

## WP-A — Cross-project agent list filter + per-request caching (§3.5)

`pkg/hub/handlers.go` `listAgents` (276):
- Apply `filter.MemberProjectIDs = resolveUserProjectIDs(...)` for the **default**
  scope, not just `scope=shared` (currently only shared/mine set it). Default scope
  = "agents in my projects." The SQL IN predicate already exists
  (`agent_store.go:450` → `agent.ProjectIDIn`).
- **Cache `resolveUserProjectIDs`** (handlers.go:5960) once per request in
  `r.Context()` (it does a BFS via `GetEffectiveGroups` + `GetGroupsByIDs`). Add a
  context key + memoizing wrapper; reuse across listAgents/listProjects/capability
  batches in the same request.
- Pagination stays SQL-honest (no post-hoc drop). `totalCount`/cursors remain
  correct.

(Depends on WP-0's `project_id` index for efficiency; logic itself is independent
of WP-B but should be tested after B so the filter semantics match.)

---

## WP-C — Broker read scoping (§4)

`pkg/hub` broker handlers + `handlers_broker_projects.go`:
- **Broker read** = owner OR hub admin OR member of ANY project the broker
  contributes to. Resolve via `ProjectContributor` (project_id ↔ broker_id;
  `pkg/ent/schema/projectcontributor.go`). Add a `CheckAccess`/derive helper that
  loads contributing project IDs for a broker and tests intersection with the
  caller's project set (`resolveUserProjectIDs`).
- **Broker list** scoped the same way: brokers you own + brokers contributing to a
  project you're a member of. Add a filter keyed off `ProjectContributor`.
  `broker_id` index already exists on `ProjectContributor` (no new index needed —
  confirm during impl).
- Freshly-registered broker not yet linked → visible to owner/admins only.
- `handleBrokerProjects` (`handlers_broker_projects.go:30`) is broker-HMAC-authed
  (broker enumerating its own projects) — leave as-is; this WP is about *user*
  read of brokers.

---

## WP-D — UI wiring (§3.6)

- **Remove the visibility selector** from `web/src/components/pages/project-create.ts`
  (markup at `:609-622`, the `visibility` `@state` at `:64`, and drop `visibility`
  from the POST body at `:371`). New projects default to creator-only.
- **Members-card hint:** in the project-settings Members card
  (`web/src/components/shared/group-member-editor.ts`, used for
  `project:<slug>:members`), add a small hint: *"To make this project visible to
  all hub users, add the hub-members group."* Scope the hint to the project members
  context (don't show it on unrelated group editors).
- Verify: `cd web && npm run typecheck && npm run lint`.

---

## 2. Deferred follow-ups (track, do not implement here)

- **OQ9 templates & harness configs** — keep globally listable now (WP-B KEEPs
  `template`/`harness_config`); their own visibility + grove-attachment design is a
  separate pass.
- **Grove→project terminology cleanup** — DELIVERED as standalone branch
  `scion/grove-cleanup` (commit 230ca7f, by visibility-explorer): adds
  `api.NormalizeVisibility()` (legacy `grove`/`project` → `team`) applied at
  write entry points for Templates, HarnessConfigs, and Projects; fixes stale
  comments; leaves wire-compat shims (`groveId/grove*` JSON, NATS subjects,
  `SCION_GROVE_ID`, container labels) intentionally untouched. **Direction (ptone,
  2026-06-05): pull it into THIS stream, not a standalone PR.** It lands as a
  discrete cherry-picked commit (`230ca7f`) on the branch, applied AFTER WP-0 and
  BEFORE wave 2 so the backend agent builds on top of it (no `handlers.go`
  conflict). Logical separation is preserved by keeping it as its own commit. The
  project-visibility normalization it adds is **interim/superseded** (project
  visibility becomes membership-derived and is no longer UI-authored after WP-D),
  so that portion goes inert; the Template/HarnessConfig normalization stands.
  Backend agent is told NormalizeVisibility is already present — do not re-add it.
- **Normalize-on-read / one-time migration for historical `"project"` rows**
  (ptone deciding) — recommendation: **write-path only for now**. Templates/harness
  are deferred (their migration rides with the OQ9 follow-up), and the *project*
  visibility column is being retired from authoring, so a read-normalization or
  backfill for project rows is wasted effort. Revisit only if the column is
  repurposed as a derived cache.
- **Finer-grained permissions / role-in-policy engine** — the deferred alternative
  in §0; revisit if nested-group admins or per-action grants are needed.
- **Drop the orphaned `agent.visibility` (and possibly `project.visibility`) DB
  columns** via `WithDropColumn` once the field removal has soaked.

---

## 3. Test & verification gates

- Go: `make test-fast` then `make build` per WP; add unit tests for the new
  enforcement paths in WP-B (the critical, behavior-changing package).
- Web: `npm run typecheck && npm run lint` for WP-D.
- Integration smoke (manual / verify skill): private project invisible to
  non-member; add user→visible; add `hub-members`→visible to all; non-member can
  still open an "everyone" project's agents on demand; top-level agent firehose
  shows only your projects.
- Final: `make ci-full` before opening the PR.

---

## 4. Rollout ordering (single branch)

1. WP-0 (schema/codegen) → commit.
2. WP-B (all sub-steps together) + WP-A + WP-C + WP-D in parallel.
3. Integration smoke + `make ci-full`.
4. Open PR on `design/project-visibility-membership`. **Do not merge.**
