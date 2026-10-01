# members-multirole P1 — atomic set/remove-all (ptone/scion#2529)

**Branch**: `scion/members-multirole-p1`
**Base**: `GoogleCloudPlatform/scion` main @ `e761178`
**Scope**: design.md §12 P1 only (backend vertical slice), as amended by two
mid-flight rulings from ptone: D1 blocks custom roles on agent principals
(400 `principal_ineligible`), and D3 = authority model (c) from
`design-d3-addendum.md` (owner-or-hub-override today, factored behind one
seam for a later permission-based model).

## What was built

`PUT`/`DELETE /api/v1/projects/{id}/members/principals/{principalType}/{principalId}`
— a declarative "set this principal's whole project role set" endpoint that
replaces add/edit/partial-removal with one atomic transaction, additive to
the existing per-binding `POST`/`PATCH`/`DELETE /members[/{bindingID}]` API
(unchanged, still used by `rs1_*`/`rs2_*`/`rs3_*`/`d002_*`/`pm1_*`).

- **`pkg/hub/project_membership_set.go`** (new): the engine.
  - `planRoleSet(current, currentDefs, desired) rolePlan` — pure diff
    (Keep/Remove/Create/BuiltInChange) between current bindings and the
    desired role-definition set. Unit-tested in isolation.
  - `SetMemberRoles` — pre-transaction governance, CanDelegate (once per
    created binding, not once per request), and precondition checks, then a
    single `WithTx`: re-evaluate authority under lock, re-read and compare
    against `expectedRoleDefinitionIds` (409 `membership_changed` on
    mismatch), delete-then-create per the D4 ordering, last-owner guard on
    the full post-state, one audit row per binding change sharing a
    `CorrelationID`.
  - `customRoleAuthorityFromStore(ctx, s, actorID, projectID, perm)` — the
    **one** function that decides custom-role grant/revoke authority (direct
    owner OR system-scope `role_binding.create`/`.delete`, the latter gated
    on the actor holding no project role of their own — review r1 F2),
    callable pre-transaction with the outer store and in-transaction with
    `tx`. No other call site makes this decision (design-d3-addendum.md A1).
    Built-in governance (`checkBuiltInChangeGovernance`, `reevaluateActorTx`)
    stays on the existing system-scope-only hub override, deliberately kept
    separate so a principal holding only a custom role — even one carrying
    `role_binding.create` — cannot bypass the built-in matrix (A2).
  - `checkNoRoleBindingPermissionInCreatedCustomRoles` (review r1 F1) — a
    structural refusal, independent of the authority function above and of
    `CanDelegate`: any newly created custom role whose permissions include
    `role_binding.*` is refused for every actor, including the hub
    `role_binding.*` override, which `CanDelegate` cannot refuse on its own
    (that actor's own ceiling already includes it). Checked pre-transaction
    and re-checked in-transaction from the same resolved role definitions.
  - `applyRolePlanTx` (in `project_membership_service.go`): the one
    purpose-named step that applies a `rolePlan`'s delete-then-create, so the
    new engine never calls `tx.CreateRoleBinding`/`tx.DeleteRoleBinding`
    directly, keeping every such call enumerable per the existing RS1 O-3 AST
    guard (unchanged — `applyRolePlanTx` already lives in
    `project_membership_service.go`, the file the guard already allowlists)
    and classified in the `authzop` mutation catalog (extended additively —
    see Deviations; review r2 R2-5 correction: only the catalog was
    extended, not the AST guard itself). Replaced an earlier pair of generic
    forwarders (`txCreateRoleBinding`/`txDeleteRoleBinding`) after review r1
    F3 flagged them as reusable primitives that left the governed call site
    invisible to the catalog.
- **`pkg/hub/handlers_project_members.go`**: `resolveMemberPrincipal`
  (extracted from `addProjectMember`, reused by both); `projectMemberGroup`
  response type; `roleKind` added to `projectMemberInfo`; the PUT/DELETE
  handlers; `buildProjectMemberGroup`.
- **`pkg/hub/handlers_projects_core.go`**: routes `members/principals/...`
  above the existing binding-ID dispatch (no collision — binding IDs are
  UUIDs).
- **`pkg/hub/errors.go`**: `invalid_role_set`, `empty_role_set`,
  `membership_changed`.
- **`pkg/hub/project_membership_service.go`**: additive only —
  `MembershipDecision.Details` (nil by default, used for
  `requiredPermission`/`roleDefinitionId`/`currentRoleDefinitionIds`);
  `principalEligibleForRole` gates the custom (non-built-in) role-name branch
  behind an explicit `!IsBuiltInProjectMembershipRole` guard (not the
  switch's `default`), returning true for user/group and false for agent on
  a custom role name; the switch's own `default` fails closed (`false`) for
  an unmatched name the guard already said was built-in — the one
  eligibility switch for D1 (review r2 R2-5 correction: this was reworded
  from an earlier, stale description of the default branch granting
  eligibility; review r1 L4 made it an explicit guard with a fail-closed
  default instead). `AddMember`/`UpdateMemberRole`/`RemoveMember` are
  untouched.

## Decisions folded in mid-flight

ptone sent three rulings while this was in progress; all three are reflected
in the code and tests, not just noted for later:

1. **D1 = block.** Creating a custom binding for `principalType=agent` is
   400 `principal_ineligible`, for every actor including hub override.
   Keeping a custom role an agent already holds (seeded directly) is
   unaffected — eligibility only gates `plan.Create`, never `plan.Keep`.
2. **D3 = authority model (c)** (`design-d3-addendum.md`): one
   store-parameterised `customRoleAuthorityFromStore`, asked for `create`
   and `delete` separately even though both resolve to the same check today
   (owner-or-hub-override) — a later permission-based model (seeding
   `role_binding.*` to project-owner) changes this function's body only, not
   its signature or any call site. Denials carry
   `details.requiredPermission`. Custom-row audit summaries carry
   `authority: "project_owner"|"hub_role_binding"`.
3. **Escalation tests** (ptone's explicit list) are all named
   `TestSetMemberRoles_Escalation_*`; see Tests below.

## Tests

`pkg/hub/project_membership_plan_test.go` (11 tests): `planRoleSet`
keep/add/remove/built-in-change/built-in↔none/duplicates/custom-only, plus
`.changes()` and `.hasCustomCreate/Remove()`.

`pkg/hub/project_membership_set_test.go` (47 tests, SQLite; 58 total with the
11 plan tests): atomicity (one denial leaves every binding and the audit log
untouched), admin tier, last owner (incl. an expired-owner removal while one
active owner remains), eligibility, validation, the credential gate (UAT and
agent token), idempotency (`changed:false`, no new audit rows),
preconditions, hub override (via direct service calls — see Deviations),
principal addressing (email/slug, percent-encoding, a rejected embedded
slash, 404 on an unbound principal), a concurrent-PUT race, audit-contract
assertions (CorrelationID, `CanDelegateResult`, authority, roleKind on
create/remove/DELETE-all rows), and the 9 ported-and-re-targeted
miller79/scion PR #127 scenarios (`TestSetMemberRoles_Ported_*`).

Escalation tests (ptone's list, verbatim names):
- `TestSetMemberRoles_Escalation_BeyondCeilingLeavesOtherBindingsUnapplied` (i)
- `TestSetMemberRoles_Escalation_RoleBindingPermissionRefusedForOwner` /
  `_RefusedForHubOverride` (ii, for every actor — review r1 F1)
- `TestSetMemberRoles_Escalation_CanDelegatePerBindingNotPerRequest` (iii)
- `TestSetMemberRoles_Escalation_AdminCannotGrantAnyCustomRoleEvenWithinCeiling` /
  `_AdminWithHubRoleBindingStillRefused` (iv, incl. review r1 F2)
- `TestSetMemberRoles_Escalation_CustomOnlyHolderCannotChangeBuiltIn` (v / A2,
  now also covering the DELETE-all/remove half per review r1 L1)

Attribution: `TestSetMemberRoles_Ported_*` and the fixture pattern they share
port miller79/scion PR #127's `handlers_roles_owner_custom_test.go`,
re-targeted to the new PUT endpoint per design.md §7. Commits carrying that
ported code carry the trailer
`Co-authored-by: Anthony Lofton <6901313+miller79@users.noreply.github.com>`.

`go test ./pkg/hub/ -run 'RS|D002|PM1|ProjectMember'` is green, unmodified.

## Deviations from the design, with reasons

- **Named `builtInRoleChange` type instead of an anonymous struct** for
  `rolePlan.BuiltInChange` (design.md shows `*struct{ Old ...; New ... }`
  inline). Same shape, easier to construct/assert in tests.
- **`MembershipDecision` gained a `Details map[string]interface{}` field.**
  Needed to carry `roleDefinitionId`/`requiredPermission`/
  `currentRoleDefinitionIds` out of the service; nil by default, so every
  existing call site (`AddMember` etc.) is unaffected.
- **Two registry-style checks needed additive entries, not code changes to
  avoid them:**
  - `rs1_extended_test.go`'s `TestRS1_AST_BypassPathsDocumented` enumerates
    files allowed to call `CreateRoleBinding`/`DeleteRoleBinding` directly.
    Rather than add `project_membership_set.go` to that allowlist (which
    would mean editing an `rs1_*` file against the brief's "stay green,
    unmodified" instruction), the new engine calls a purpose-named
    `applyRolePlanTx` added to `project_membership_service.go` instead, so
    the direct store calls stay inside the file that guard already exempts.
    `rs1_extended_test.go` was not touched. (Review r1 F3 replaced an earlier
    pair of generic forwarders with this single-purpose step, after the
    generic version was flagged as making the governed call site invisible
    to the `authzop` catalog below — see "What was built".)
  - `pkg/hub/authzop/catalog.go`'s `MutationClassifications` table requires
    every discovered `CreateRoleBinding`/`DeleteRoleBinding` call site to be
    classified. `applyRolePlanTx` needed two new `ExemptionInternalOnly`
    entries (mirroring the existing `replaceBindingTx` entries), since a
    proper `OperationID` would need a `route_metadata.go` entry, which is
    off-limits for this slice. Flagging this for whoever eventually wires a
    real `project.membership.set` operation ID.
- **Hub-override tests call `ProjectMembershipService.SetMemberRoles`
  directly** instead of through HTTP PUT. The PUT/DELETE entry gate is
  `project.manage` (per design.md §3.1), and hub-admin does not hold
  `project.manage` (`seed.go` `hubAdminPermissionIDs`) — exactly like the
  existing `AddMember`/`RemoveMember` hub override, whose own tests
  (`rs5_global_admin_governance_test.go`, `rs5_r2_hardening_test.go`) also
  call the service directly rather than through the equally-gated `/members`
  HTTP endpoints. In production, `/api/v1/admin/role-bindings` (unchanged in
  P1; design.md §7 explicitly did not port miller79/scion PR #127's
  relaxation of it) has no `project.manage` gate, so it is one way to reach
  the hub override. It is not the only way: review r1 raised, and r2
  confirmed, that this PUT/DELETE endpoint's own `project.manage` gate can
  also be passed by an actor with no built-in project role of their own who
  holds a custom role carrying `project.manage` — such an actor, if they
  also hold system `role_binding.*`, reaches the hub override over this
  endpoint too.

## Validation gate transcript

Manually exercised against a live dev hub: atomic add of `{admin, customA}`
to a new user, edit member→admin keeping a custom role, a custom grant, a
refused beyond-ceiling grant leaving state unchanged, and remove-all via
DELETE, each followed by the bindings and mutation-audit rows read back via
the API/sqlite3. The transcript is held with the review artifacts, not in
this upstream-bound file.

## Gates run

- `go build -buildvcs=false ./...` — pass.
- `go vet -buildvcs=false ./pkg/hub/...` — pass.
- `gofmt -l` on every changed/added file — clean.
- `go test ./pkg/hub/ -run 'RS|D002|PM1|ProjectMember|TestSetMemberRoles|TestPlanRoleSet'` — pass (regression + new, after clearing leaked `SCION_*`/`CLAUDE_CODE_ENABLE_TELEMETRY` env vars that otherwise fail unrelated tests in this container — see below).
- `GOGC=40 golangci-lint run --new-from-rev=upstream-main --concurrency=1 ./pkg/hub/... ./pkg/hub/authzop/...` — pass.
- `make ci` — pass, after unsetting this agent-container's own leaked
  `SCION_*`/`CLAUDE_CODE_ENABLE_TELEMETRY` environment variables (this is a
  live Scion agent session; its own `SCION_PROJECT_ID`, `SCION_HUB_URL`,
  etc. otherwise leak into unrelated `cmd`/`pkg/config`/`pkg/harness` tests
  that assert on hub reachability or env adoption, per AGENTS.md's
  documented "leaked SCION_* env vars" gotcha). Confirmed each affected test
  passes in isolation with the var unset, and confirmed `pkg/hub` itself
  never referenced any of the leaked variables.

## Adjacent cleanup noticed, not implemented

Per design.md §11: the `/admin/role-bindings` custom-project-role path still
has no `LockProjectForMembership`, no mutation audit, and no credential gate
(`handlers_roles.go`). `SetMemberRoles` would be the natural destination for
it, but design.md explicitly reserves that reroute as optional P4/P1b
cleanup, out of scope here.

## Review round 1 fixes

Closed every finding from the first review round (0 Critical, 1 High, 3
Medium, 6 Low, 5 Nit) — F1-F4, L1-L6, N1-N5, no declines:

- **F1** (High): a structural, actor-independent refusal
  (`checkNoRoleBindingPermissionInCreatedCustomRoles`) now blocks any
  *created* custom role carrying a `role_binding.*` permission for every
  actor, including the hub override, pre-transaction and re-checked
  in-transaction. The owner-only escalation test (ii) was re-scoped to prove
  the guarantee for every actor, plus a new hub-override test asserting
  refusal and zero writes.
- **F2** (Medium): `customRoleAuthorityFromStore`'s hub `role_binding.*`
  fallback is now gated on the actor holding no project role of their own,
  matching the built-in governance override's own condition and the
  function's own doc comment.
- **F3** (Medium): the generic `txCreateRoleBinding`/`txDeleteRoleBinding`
  forwarders were replaced with one purpose-named `applyRolePlanTx`; the two
  `authzop/catalog.go` exemptions now describe what that step actually
  governs. No `rs1_*`/`rs2_*`/... test file was touched.
- **F4** (Medium): the audit-contract tests were vacuous (no request logger
  in the test harness meant `CorrelationID` was always empty, so the
  shared-ID assertion passed trivially). Added a helper that installs a real
  request logger, and assertions on `CanDelegateResult`, the authority (Via)
  field, and `roleKind` across create, remove, and DELETE-all rows.
- **L1-L6, N1-N5**: all fixed — stronger escalation-test (v) assertions and
  a remove variant; a hub-override case for the D1 agent-ineligibility test;
  the credential gate now rejects a non-user (e.g. agent) identity with the
  same `credential_insufficient` code a UAT gets, checked before resource
  authorization; `principalEligibleForRole`'s custom-role branch is reached
  only by an explicit "not built-in" guard rather than being the switch's
  catch-all default; `roleKind` lost its `omitempty` and is now set on the
  PATCH response too; a `projectRoleKind` helper replaces four separate
  inlined copies; the role-change audit row now carries `roleKind` and
  `principalType`; bare issue references were qualified
  (`miller79/scion PR #127`, `ptone/scion#2529`) in both code comments and
  commit history; this file was renamed to the dated convention and had its
  internal-path references and stale test count removed; the concurrency
  test no longer calls `require`-based helpers from a non-test goroutine.

A finding-by-finding closure table (ID → commit/file:line → how closed) was
produced for this round and shared with the reviewing agents; it is not
duplicated here. `go test ./pkg/hub/ -run 'SetMemberRoles|RoleSet|Escalation|
OwnerCustom|RS|D002|PM1|ProjectMember|Catalog|Classif|AST'`, `go test
./pkg/hub/authzop/...`, `gofmt -l`, `go vet`, and a scoped
`golangci-lint run --new-from-rev=upstream-main` all pass on the fixed head.
The branch was rebased onto a fresh `upstream-main` afterward. Per the P1
broker throttle, the full `make test-hub-sqlite`/`make ci` were not run
locally for this round; they run in the PR's GitHub CI.

## Review round 2 fixes

Closed every open finding from the second review round (0 Critical, 0 High,
0 Medium, 2 Low, 6 Nit — every round-1 finding was independently re-verified
closed, and no new High/Medium/Critical surfaced). No declines:

- **R2-1** (Low): the `applyRolePlanTx` doc comment and its two
  `authzop/catalog.go` exemption reasons said the last-owner check runs
  *before* the call. It actually runs *after*, inside the same transaction,
  as a post-state count whose violation rolls back every mutation the call
  made. Reworded both to describe that ordering.
- **R2-2** (Low): a narrow TOCTOU hardening inside the accepted FYI-2
  residual. Phase P's `CanDelegate` call is evaluated once, pre-transaction,
  against the actor's authority source at that moment; it is not re-run
  in-tx. If that source itself changes between phases — e.g. a direct owner
  who also holds hub `role_binding.*` is demoted from ownership by a
  concurrent request before the lock lands — the grant could otherwise
  commit under a ceiling `CanDelegate` never evaluated. Added an in-tx check
  that refuses with 409 `membership_changed` if the actor's role,
  hub-override status, or any asked custom-authority `Via` differs from its
  pre-transaction value, plus a service-level test using a store wrapper to
  simulate the concurrent demotion landing exactly between phases.
- **R2-3, R2-7** (Nit): two test comments overstated what their assertions
  proved — the escalation test (v)'s reason-text check does not prove the
  request reached `reevaluateActorTx` (it reaches the pre-tx branch first),
  and the audit-remove test's comment described the *removed* role's
  original grant authority when the code (and the test) can only observe
  the *remover's* authority. Both comments were corrected to describe only
  what the assertions actually show.
- **R2-4** (Nit): a bare `#127` issue reference, introduced ironically in
  the commit that fixed the previous round's bare-reference finding (N2).
  Reworded on this round's final history rewrite.
- **R2-5** (Nit): four stale or inaccurate statements in this file (the
  base SHA, the RS1 AST guard being "extended" when only the catalog was,
  `principalEligibleForRole`'s default-branch description after review r1's
  L4 made it an explicit guard, and an incomplete description of how the
  hub override is reached in production) were corrected, along with the
  matching test comment at `project_membership_set_test.go`'s
  `mmrServiceCtx`.
- **R2-6** (Nit): `roleKindBuiltIn`/`roleKindCustom` constants were added
  next to `projectRoleKind` and used at every remaining
  `"builtin"`/`"custom"` comparison and default-value site (5 places across
  `project_membership_set.go` and `handlers_project_members.go`), closing
  the gap N1 (review r1) left when it centralised the derivation but not
  every call site's literal.
- **R2-8** (Nit): the concurrent-PUT test accepted any combination of `{200,
  409}` for its two racing requests, including both returning 409 — which
  would mean the lock rejected both instead of serializing them. Added an
  assertion that at least one request succeeds, per §12 P1.

FYI-A and FYI-B required no action (consistent with Part 0 by design); FYI-C
and FYI-D (round 1's accepted residuals) still apply unchanged.

A finding-by-finding closure table (ID → commit/file:line → how closed) was
produced for this round and shared with the reviewing agents; it is not
duplicated here. The same throttled gate set as round 1 (`go test
./pkg/hub/ -run 'SetMemberRoles|RoleSet|Escalation|OwnerCustom|RS|D002|
PM1|ProjectMember|Catalog|Classif|AST'`, `go test ./pkg/hub/authzop/...`,
`gofmt -l`, `go vet`, and a scoped `golangci-lint run
--new-from-rev=upstream-main`) all pass on the fixed head, which was
rebased onto a fresh `upstream-main` afterward. Per the P1 broker throttle,
the full `make test-hub-sqlite`/`make ci` were not run locally for this
round either; they run in the PR's GitHub CI.
