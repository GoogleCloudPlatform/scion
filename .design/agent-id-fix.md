# Design: Hub-wide authorization bypass for non-user callers (issue #591)

**Author:** aid-arch
**Date:** 2026-07-28
**Repo state:** `ptone/scion` @ base commit `8dbf167`
**Status:** FOR REVIEW — Q1–Q4 answered by ptone 2026-07-28 and incorporated. See §11 for the
resolutions and §12 for the follow-on track ptone raised.
**Supersedes/extends:** `projects/svc-accnt/SECURITY-authz-bypass.md` (sa-arch), `projects/svc-accnt/findings-sa-inv.md` §5.1/§5.2/Q10/Q12

> **Handling — RESOLVED 2026-07-28.** The brief asked that specifics not circulate publicly
> pending a disclosure path. That was already moot: `ptone/scion` is a public repo and
> `ptone/scion#591` was opened on it at 14:36Z with a 9,299-char body carrying the root-cause
> idiom, the `identity.go` explanation, and a `file:line` impact table. Escalated to ptone, who
> ruled at 15:57Z: *"We are working on fixing these in short order today - so post followups on
> the fork repo publicly to make sure they are tracked."*
>
> All five follow-ups are filed publicly and cross-referenced from #591:
> **#595** (`matchesResource` engine defect), **#596** (GCP gate alignment), **#598** (route
> authz manifest), **#599** (ServeMux migration), **#600** (dead-code cleanup).
>
> For the record: `#591` on **upstream** `GoogleCloudPlatform/scion` is an unrelated merged PR
> (Slack broker plugin). The security issue exists only on the fork.

---

## 1. Summary

An agent-authenticated caller — and, on some paths, an unauthenticated one — can perform
privileged operations hub-wide and cross-project, because ~24 handler sites write their
authorization guard in a form that is silently skipped for any identity that is not a
`UserIdentity`.

The authorization *engine* (`AuthzService.CheckAccess`, `authz.go:94`) is correct: it
dispatches on `identity.Type()`, handles agents, and fails closed on unknown types. Only the
call sites are wrong.

**The fix is two parts that must ship atomically:**

- **Part 1** — convert the call sites to a fail-closed shared helper.
- **Part 2** — give agents a defined authorization baseline, because after Part 1
  `checkAccessForAgent` is default-deny with nothing granting agents anything.

This document specifies both, plus three corrections to the source doc's site inventory that
expand and re-rank the scope.

### What this design changes relative to the brief

Three findings altered the shape of the answer:

1. **There is a second syntactic form of the same bug** that the source doc classified as
   "verified fail-closed." Four additional unguarded cross-project reads. (§3.2)
2. **`createProjectAgent` is Group A, not Group B** — it has no isolation check at all, not
   "isolation implied by the URL project." Cross-project agent creation. (§3.3)
3. **The repo already has a working agent-authorization model** — JWT token scope + project
   isolation — implemented correctly in three places and simply absent at the broken sites.
   Part 2 therefore does not need to invent a policy baseline; it codifies what exists. This
   makes Part 2 substantially smaller and lower-risk than the brief anticipated. (§5)

---

## 2. Root cause

Defer to `SECURITY-authz-bypass.md` for the full narrative. In brief:

`agentIdentityWrapper` (`identity.go:120-145`) implements `AgentIdentity` but not
`UserIdentity` — no `Email()`, `DisplayName()`, `Role()`. `brokerIdentityImpl`
(`brokeridentity.go:29-44`) implements neither. So both fail the `UserIdentity` type
assertion in `GetUserIdentityFromContext` (`identity.go:167-176`), which returns `nil`, and
the entire guard block is skipped. No error, no log, no audit record.

### 2.1 The structural cause, which matters more than the idiom

Three facts together explain why this bug class exists and why it will recur without a
structural fix:

1. **There is no authorization middleware.** The chain (`server.go:2863-2900`) is
   CORS → `UnifiedAuthMiddleware` → `adminModeMiddleware` → `userActivityMiddleware` →
   `BrokerAuthMiddleware` → logging → recovery. Every authorization gate is per-handler.
2. **`RequireAuth`, `RequireUserAuth`, and `RequireRole` (`auth.go:457-500`) are never
   called from anywhere.** The source doc lists `auth.go:472,490` under "verified
   fail-closed" — accurate, but only because it is dead code. It should not be read as
   evidence that a middleware layer is enforcing anything.
3. **There is no shared authorization helper.** Roughly 110 `CheckAccess` call sites across
   `pkg/hub` each hand-write the same fetch → nil-check → check → `writeError` sequence. The
   only reusable guards are three narrow ones: `requireAdmin`
   (`skill_registry_handlers.go:99`, used only within that file), `checkBrokerDispatchAccess`,
   and `authorizeProjectImport`.

An omission in a hand-written five-line idiom repeated 110 times is invisible to the
compiler, invisible to `go vet`, and nearly invisible to review. The bug is a predictable
output of that structure. **The single highest-value deliverable in this change is the
shared helper**, not any individual site conversion — the conversions are what make the
helper load-bearing.

---

## 3. Corrected scope inventory

The source doc's site table tracks one idiom. There are three.

### 3.1 Idiom 1 — `GetUserIdentityFromContext` guard (the known one)

```go
if u := GetUserIdentityFromContext(ctx); u != nil {
    decision := s.authzService.CheckAccess(ctx, u, res, action)
    if !decision.Allowed { /* 403 */ }
}
// no else
```

Skipped for agents and brokers. This is the source doc's Group A + Group B.

### 3.2 Idiom 2 — `identity.(UserIdentity)` type-assertion guard (NEW; Q1: in scope)

```go
identity := GetIdentityFromContext(ctx)
if identity == nil { Unauthorized(w); return }
...
if userIdent, ok := identity.(UserIdentity); ok {
    decision := s.authzService.CheckAccess(ctx, userIdent, res, action)
    if !decision.Allowed { Forbidden(w); return }
}
// no else
```

Functionally identical. Agents pass the nil check and skip the block; **so do HMAC-valid
brokers**, since `BrokerAuthMiddleware` writes `brokerIdentityImpl` into the generic identity
key (`brokerauth.go:825-826`) and it implements neither interface.

I audited all 30 occurrences of this form in `pkg/hub`. Twenty-two are genuinely compensated
(they have an `else { Forbidden(w); return }`, or a dedicated
`GetAgentIdentityFromContext` branch with project isolation) and four are attribution-only.
**Four are unguarded bypasses**, all cross-project reads, and all four sit in a function
whose *sibling write path* does have the missing `else`:

| Site | Function | Route | Leaks |
|---|---|---|---|
| `project_settings_handlers.go:74` | `handleProjectSettings` | `GET /api/v1/projects/{id}/settings` | project settings incl. `scion.io/default-gcp-identity-service-account-id` |
| `handlers_shared_dirs.go:47` | `handleProjectSharedDirs` | `GET /api/v1/projects/{id}/shared-dirs` | host mount paths |
| `project_pre_start_hook_handlers.go:77` | `handleProjectPreStartHooks` | `GET /api/v1/projects/{id}/pre-start-hooks` | hook `Script` bodies |
| `project_pre_start_hook_handlers.go:245` | `handleProjectPreStartHookByID` | `GET /api/v1/projects/{id}/pre-start-hooks/{hid}` | hook `Script` body |

The source doc places "workspace handlers" and "skills" in Group D. That is correct for
`handlers_skills_injection.go` and `handlers_resource_import.go` (both have the `else`), but
not for these four. **Recommend folding in — Q1.**

### 3.3 Correction — `createProjectAgent` is Group A

The source doc lists `handlers_projects_core.go:1758` under Group B, "isolation only implied
by the URL project." There is **no isolation.** `createProjectAgent`
(`handlers_projects_core.go:1720-1767`) resolves the caller only for attribution and calls
`createAgentInProject` directly. It is missing all three checks its twin `createAgent`
performs at `handlers_agents_core.go:323-357`:

| Check | `createAgent` (`POST /api/v1/agents`) | `createProjectAgent` (`POST /api/v1/projects/{id}/agents`) |
|---|---|---|
| `HasScope(ScopeAgentCreate)` | ✅ `:325` | ❌ |
| `req.ProjectID == agentIdent.ProjectID()` | ✅ `:330` | ❌ |
| `CheckAccess(agent, ActionCreate)` for users | ✅ `:352` | ❌ |
| Field-level GCP identity validation | ✅ `:294-311` | ❌ |

Any agent token creates an agent in **any** project. This belongs in the severity table
alongside cross-project PTY. It also means the missing-`else` fix (sa-inv Q12) and the
`createProjectAgent` fix are the same fix — see §4.3.

> **Severity upgrade — this is the route real traffic uses.** I traced the clients while
> checking a separate claim, and `POST /api/v1/projects/{projectId}/agents` is not a
> secondary path: it is what the **CLI** calls for `scion create`, `scion start`, and
> `scion sync`. `hubclient.agentsPath()` (`pkg/hubclient/agents.go:120-124`) switches to the
> project-scoped URL whenever `ProjectAgents(projectID)` is used, and
> `createAgentWithBrokerResolution` (`cmd/common.go:1136-1148`) — the single funnel for both
> CLI create paths — always uses it. `scion-chat-app` uses it too. The web UI and the A2A
> bridge use the unscoped `POST /api/v1/agents`, which is the one that *is* gated.
>
> So the ungated create route is the dominant one by real-world volume, and the gated route
> is the minority path. That inverts the usual "obscure secondary endpoint" reading of this
> class of finding and should be reflected in how #591 is triaged.

**The fourth row is a separate gap, contributed by sa-arch and verified independently.**
`createProjectAgent` validates name, `cleanupMode`, and labels, then calls
`createAgentInProject` — which does not re-validate. So on the project route:

- `metadata_mode: block|passthrough` **with** a `service_account_id` is accepted (rejected on
  `POST /api/v1/agents` at `:298`).
- An **unrecognised** `metadata_mode` is accepted entirely. `createAgentInProject` branches
  only on `== passthrough` (`:408`) and `== assign` (`:425`); anything else falls through with
  no SA resolution and no passthrough ownership check. The `default:` arm that rejects this
  exists only in `createAgent` (`:307`).
- **A live consequence, found while resolving sa-arch's empty-mode question.** The
  config-building switch at `:568-586` is the only GCP switch in the file with **no
  `default:` arm**, and its `else` branch is what applies the project's configured default
  mode. So `POST /api/v1/projects/{id}/agents` with body `{"name":"x","gcp_identity":{}}`
  matches no case, assigns nothing, and **bypasses the `else` that would have applied the
  project default** — leaving `AppliedConfig.GCPIdentity` nil. If the project is configured
  with `default-gcp-identity-mode: assign`, that default is silently dropped and the agent
  starts with no identity.

  This is fail-*safe* (nil → broker `block`), so it is a correctness bug, not a hole.

  **Reachability, stated precisely** — I checked every client after this was described to me
  as affecting "essentially every CLI-created agent," and that is not right. No in-repo
  client can produce it: both request structs use `*GCPIdentityAssignment` with `omitempty`
  (`handlers_agents_core.go:100-102`, `:1483`), so a zero value serializes as *absence*, not
  `{}`. The CLI's `hubclient.CreateAgentRequest` (`pkg/hubclient/agents.go:159-189`) has **no
  GCP field at all**, so CLI-created agents always take the `else` branch and *do* get the
  project default. The web UI always emits an explicit mode and posts to the gated route
  anyway. The case is reachable only from a hand-rolled HTTP caller.

  Keep the fix regardless — it is three lines, and "only reachable by a non-standard client"
  describes most of the rest of this document. But it should be recorded as
  defence-in-depth-plus-correctness, not as a live governance hole. The *governance* concern
  sa-arch is pointing at is real and is captured above: the CLI does use this route, and on
  this route the project default is applied by a branch that no validation protects.

This is an authorization-adjacent input-validation gap on a route this track already has
open. See §8.4 for what an unrecognised value does downstream — the answer changed twice
during review and the final characterisation matters.

### 3.4 Idiom 3 — attribution (do NOT convert)

~25 sites use `GetUserIdentityFromContext` to set `CreatedBy`/`UpdatedBy`, name a message
sender, or filter a list. These are correct. Full list in the source doc §C.

⚠️ **`handlers_github_app.go` needs two different things done a few lines apart:**

- **`:113`** — the `func handleUpdateGitHubApp` declaration. There is **no authorization of
  any kind** in this handler. Any authenticated principal overwrites the hub GitHub App
  private key, webhook secret, app ID, and API base URL. **ADD** an admin-only gate.
- **`:121`** — `user := GetUserIdentityFromContext(ctx); if user != nil { userID = user.ID() }`,
  threaded into `setGitHubAppSecret` as the secret's author. This is attribution and it is
  correct. **DO NOT CONVERT.**

Note the asymmetry, because an earlier draft of this section got it wrong and the error
propagated into §7.1 and two developer briefs: these are **not** a matched pair of guards with
opposite verdicts. `:113` has no guard at all — the finding is an *absence*. The file contains
exactly one `GetUserIdentityFromContext` occurrence, at `:121`.

Two consequences worth carrying forward:

1. The lint rule must still key on *guard shape* rather than the getter name, so that `:121`
   is not flagged. That requirement is real; only the illustration was wrong. See §7.1 for the
   test case that actually demonstrates it.
2. A lint rule keyed on guard shape **cannot see `:113` at all**, and never will. Absence of a
   check has no syntax. See §7.3.

### 3.5 Additional finding, not in any prior inventory

`getGroupMember` (`handlers_groups.go:685-695`) has no authorization at all — any
authenticated caller reads any group membership by ID. Low severity (membership metadata),
same class. Recommend folding in since we are already in the file.

---

## 4. Part 1 — call-site conversion

### 4.1 The `authorize` helper

New file `pkg/hub/authorize.go`:

```go
// authorize performs a fail-closed authorization check for any identity kind.
// It writes a 403 (or 401 for an unauthenticated caller) and returns false when
// access is denied, so callers can write:
//
//	if !s.authorize(w, r, agentResource(agent), ActionDelete) { return }
//
// Unlike the pre-#591 idiom it MUST NOT be wrapped in an identity-kind guard:
// nil and non-user identities are denied here, not skipped.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, resource Resource, action Action) bool {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	decision := s.authzService.CheckAccess(ctx, identity, resource, action)
	if !decision.Allowed {
		slog.Warn("authorization denied",
			"principal_type", identity.Type(),
			"principal_id", identity.ID(),
			"resource_type", resource.Type,
			"resource_id", resource.ID,
			"action", action,
			"reason", decision.Reason,
			"path", r.URL.Path)
		Forbidden(w)
		return false
	}
	return true
}

// authorizeMsg is authorize with a caller-supplied denial message, for the
// handful of sites whose existing 403 text is user-facing guidance.
func (s *Server) authorizeMsg(w http.ResponseWriter, r *http.Request, resource Resource, action Action, msg string) bool
```

Design notes:

- **The `slog.Warn` on denial is not incidental.** The defining property of this bug was
  that the skip was silent. Every denial now produces a structured log line naming the
  principal type — which is also the signal an operator needs to detect a Part 2 baseline
  that is too tight.
- Takes `r`, not `ctx`, so it can log the path and so a future audit hook has the request.
- Returns `bool` rather than an `error` to match the existing `checkBrokerDispatchAccess`
  and `authorizeProjectImport` shape.
- `Forbidden(w)` (`errors.go:190`, "Insufficient permissions") is the default. Several
  existing messages are actively misleading — `performAgentDelete` says *"Only the agent's
  creator can delete it"* when admins, ancestors, and project owners also pass — so the
  generic message is an improvement, not a regression. `authorizeMsg` covers the few cases
  where the text carries real guidance (e.g. the GCP SA assignment message).

**Non-goal:** do not convert the ~110 already-correct `CheckAccess` sites in this change.
Scope is the broken ones. The helper becomes the standard for new code via the lint rule.

### 4.2 Preserving 404-not-403 on isolation checks

Several handlers deliberately return `NotFound` rather than `Forbidden` for cross-project
access, to avoid disclosing resource existence (`getAgent:1438`, `handlers_logs.go:56`).

**Conversion must keep those checks and run them before `authorize`.** In `handlers_logs.go`
the current order is inverted — the authz block is at `:47`, the isolation check at `:54` —
so a naive in-place replacement would 403 an agent before the isolation check can 404 it,
leaking existence. **Every conversion in `handlers_logs.go` must also reorder: isolation
first, then `authorize`.** This is the one place where the mechanical conversion is not
mechanical, and it needs to be called out to the implementer.

### 4.3 Shared agent-caller gates

Three sites need more than a resource/action check because the legitimate agent flow through
them is scope-gated, not policy-gated (§5). Each gets a named helper rather than a
hand-written branch, modelled on the existing, correct `authorizeProjectImport`
(`handlers_resource_import.go:476`):

```go
// authorizeAgentCreate gates agent creation for every caller kind. Exhaustive
// and fail-closed. Replaces the caller-kind branch in createAgent, which had no
// else clause (sa-inv Q12), and supplies the gate createProjectAgent never had.
func (s *Server) authorizeAgentCreate(w http.ResponseWriter, r *http.Request, projectID string) bool
//   agent  -> HasScope(ScopeAgentCreate) && agentIdent.ProjectID() == projectID
//   user   -> CheckAccess(Resource{Type:"agent", ParentType:"project", ParentID:projectID}, ActionCreate)
//   other  -> deny

// authorizeAgentLifecycle gates start/stop/suspend/restart/message/exec.
func (s *Server) authorizeAgentLifecycle(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool
//   agent  -> HasScope(ScopeAgentLifecycle) && agentIdent.ProjectID() == agent.ProjectID
//   user   -> CheckAccess(agentResource(agent), ActionAttach)
//   other  -> deny
```

`authorizeAgentCreate` is called from **both** `createAgent` and `createProjectAgent`,
which simultaneously closes the missing-`else` and the cross-project hole of §3.3 — one fix,
not two.

### 4.4 Conversion table

**Group A1 — idiom 1, unguarded.**

| Site | Function | Conversion |
|---|---|---|
| `handlers_agents_core.go:1655` | `performAgentDelete` | `authorize(agentResource, ActionDelete)` |
| `handlers_agents_core.go:1468-1611` | `updateAgent` | **add** `authorize(agentResource, ActionUpdate)` — no check exists today; **plus the two SA checks, see §4.5** |
| `handlers_agents_core.go:409` | GCP passthrough | restructure — see note below |
| `handlers_agents_core.go:446` | SA assign | `authorizeMsg(gcpServiceAccountResource, ActionRead, …)` |
| `handlers_projects_core.go:2098` | `updateProject` | `authorize(projectResource, ActionUpdate)` |
| `handlers_projects_core.go:2271` | `deleteProject` | `authorize(projectResource, ActionDelete)` |
| `handlers_projects_core.go:1977` | lifecycle actions | `authorizeAgentLifecycle` (§4.3) |
| `handlers_projects_core.go:1720` | `createProjectAgent` | `authorizeAgentCreate` (§4.3) |
| `handlers_groups.go:321` | `updateGroup` | `authorize(groupResource, ActionUpdate)` |
| `handlers_groups.go:378` | `deleteGroup` | `authorize(groupResource, ActionDelete)` |
| `handlers_groups.go:482` | `addGroupMember` | `authorize(groupResource, ActionAddMember)` |
| `handlers_groups.go:518` | role-hierarchy guard | deny non-user callers outright |
| `handlers_groups.go:701` | `removeGroupMember` | `authorize(groupResource, ActionRemoveMember)` |
| `handlers_groups.go:685` | `getGroupMember` | **add** `authorize(groupResource, ActionRead)` (§3.5) |
| `handlers_runtime_brokers.go:342` | `updateRuntimeBroker` | `authorize(brokerResource, ActionUpdate)` |
| `handlers_runtime_brokers.go:387` | `deleteRuntimeBroker` | `authorize(brokerResource, ActionDelete)`; also move `actorID` assignment out of the guard so audit records a real actor |
| `handlers_runtime_brokers.go:434` | `checkBrokerDispatchAccess` | remove `userIdent == nil → return true` — **superseded, see the correction below** |
| `handlers_agent_create_helpers.go:882` | `canDispatchToBroker` | remove `userIdent == nil → return true` — **superseded, see the correction below** |
| `pty_handlers.go:93` | `handleAgentPTY` | `authorize(agentResource, ActionAttach)` |
| `handlers_logs.go:193` | cloud-logs stream | `authorize(agentResource, ActionRead)` |
| `handlers_logs.go:364` | message-logs stream | `authorize(agentResource, ActionRead)` |
| `handlers_logs.go:522` | project message-logs stream | `authorize(projectResource, ActionRead)` |
| `handlers_github_app.go:113` | `handleUpdateGitHubApp` | `s.requireAdmin(w, r)` — the helper already exists at `skill_registry_handlers.go:99`; promote it out of that file |

*Note on `:409` (GCP passthrough):* this is not a `CheckAccess` call. It is a hand-rolled
`userIdent.Role() != "admin" && broker.CreatedBy != userIdent.ID()` comparison, so no policy
can grant passthrough and the project-owner bypass does not apply. Converting it to
`authorize(brokerResource(broker), ActionDispatch)` is the semantically right move but is a
*behaviour change for users*, not just for agents. **Recommend the minimal fix here** — keep
the hand-rolled comparison, add an explicit non-user deny — and file the
policy-engine alignment separately, to keep this PR's user-visible surface at zero.

*Correction — the two broker-dispatch rows above are wrong as written.* They say to remove the
`userIdent == nil → return true` branch. Taken literally that **panics**:
`GetUserIdentityFromContext` returns a literal `nil` on the miss path, and `CheckAccess` opens
with `switch identity.Type()`, so the nil branch is load-bearing against a 500 — it is not
merely a fail-open. Removing it without also widening the getter turns a silent bypass into a
crash. This note is left in place of a rewrite so the record shows the instruction was
corrected rather than quietly replaced.

The ruling (recorded in `em-notes/broker-dispatch-ruling.md`, which supersedes these two rows
and these two rows only) is to convert both functions to the *unnarrowed* identity instead:
take `GetIdentityFromContext`, deny on nil, keep the existing `AutoProvide` short-circuit where
it is, then switch exhaustively on `identity.Type()` — `"user"`/`"dev"` keeps today's
`CheckAccess(..., ActionDispatch)` unchanged, `"agent"` is scope- and project-checked, and
`default:` denies. Both functions must end up structurally identical apart from their
signatures; they are one decision written twice, and if they drift, one of them becomes the
hole.

Note the blast radius is wider than "agents": a `"broker"`-typed caller is neither a
`UserIdentity` nor an `AgentIdentity`, so it lands in `default:` and is **denied**. The
existing comment claims the nil branch exists for broker-to-broker dispatch. Denying that
caller is a deliberate, chosen behaviour change — see the ruling for why it is safe (any broker
caller reaching the real `CheckAccess` was already denied by its `default:` arm) and for the
test it requires.

**Group A2 — idiom 2, unguarded (§3.2).** Four sites; each gets the
`else { Forbidden(w); return }` its sibling write path already has. Preferred form is to
convert the whole function to `authorize`, which removes the asymmetry as a class.

**Group B — idiom 1, compensated.** Convert to `authorize`, keeping the isolation check
first (§4.2). These are the sites that depend on Part 2; see §6.

| Site | Function | Resource / Action |
|---|---|---|
| `handlers_agents_core.go:1442` | `getAgent` | `agentResource` / `ActionRead` |
| `handlers_logs.go:47` | `handleAgentLogs` | `agentResource` / `ActionRead` |
| `handlers_logs.go:109` | `handleAgentCloudLogs` | `agentResource` / `ActionRead` |
| `handlers_logs.go:287` | `handleAgentMessageLogs` | `agentResource` / `ActionRead` |
| `handlers_logs.go:447` | `handleProjectMessageLogs` | `projectResource` / `ActionRead` |

---

### 4.5 The PATCH SA checks — ours, not svc-accnt's

Settled with sa-arch 2026-07-28 after two rounds. The `updateAgent` work divides **three**
ways, not two:

| | Piece | Owner | Depends on |
|---|---|---|---|
| (a) | `authorize(agentResource, ActionUpdate)` | this track | nothing |
| (b) | `sa.ScopeID != projectID` and `!sa.Verified` on the PATCH path | **this track** | nothing |
| (c) | `CanActAs` / `actAs` IAM check | svc-accnt | Q14 — **still unruled** |

**Why (b) is not svc-accnt's.** The natural-looking seam is authz-vs-IAM, which would put
anything SA-shaped on svc-accnt's side. That is a *conceptual* boundary, not a *dependency*
one. The decisive test is "does it need anything svc-accnt has to build first," and (b) needs
nothing: no GCP API call, no toggle, no Q14. It reads two local store fields.

**Why it cannot wait.** `updateAgent` today calls `GetGCPServiceAccount` at `:1585` and assigns
straight through at `:1590` — no scope check, no `Verified` check. Create has both, at `:435`
and `:439`. So (a) alone leaves PATCH accepting an SA from another project or an unverified one;
it merely requires update rights on the agent, **which the agent's creator has by definition**.
"Create with no SA, then PATCH the SA in" survives (a) and dies to (a)+(b). Deferring (b) leaves
the create-side hardening defeatable for the entire window — and that window is now downstream
of Q14/Q15/Q16, which are still open with ptone.

This is not the §596 regression-window hazard; nothing is being removed. It is the different
and quieter cost of shipping a gate whose bypass is left open beside it.

#### Implementation constraint from svc-accnt — mirror `:435` literally

sa-arch's request, and it should be honoured to the letter:

> Whoever implements (b), mirror `:435` **literally** — same shape, same error string — so the
> later conversion is a two-site grep rather than a hunt.

The reason is specific. Q5 locked as option A (real hub-scoped SAs, pickable in any project), so
svc-accnt Goal 2 must turn `sa.ScopeID != projectID` from an equality test into a scope-aware
check that unions in hub-scoped SAs. After (b) there will be **two** sites carrying that
requirement instead of one. If P4 converts `:435` and misses the PATCH twin, hub-scoped SAs will
work at create and be rejected at PATCH — silently, and presenting as a Goal 2 bug rather than a
missed conversion.

So: **do not improve the wording, do not refactor the two checks into a helper, do not adjust
the error string.** A near-duplicate that greps identically is worth more here than a tidier
version that does not. sa-arch is adding the PATCH site to SC-2's table so P4 inherits both.

`!sa.Verified` is unaffected by Goal 2 and survives unchanged.

#### Why this section is spelled out rather than assumed

Both tracks had independently written down that **the other one** was doing (b). This document's
§10 handoff table said svc-accnt would add the scope-match and `Verified` validation; svc-accnt's
P3 surface table said *"Track S fixes the authz; svc-accnt adds the IAM gate and validation
parity."* Two documents, mirrored stale claims, pointing opposite ways.

Neither was wrong when written, and neither would have been caught by reading either document
carefully — each was internally consistent. Only the cross-check found it. Had both sides stayed
internally consistent and not compared, the gap would have shipped, with each track assuming the
other held it.

That is the argument for stating ownership of (b) explicitly here and in svc-accnt's plan, rather
than letting it follow from the authz/IAM seam. The general form is banked as svc-accnt's SC-3:
**split on dependency boundaries, not conceptual ones.**


## 5. Part 2 — the agent authorization baseline

### 5.1 The state after Part 1

`checkAccessForAgent` (`authz.go:181-217`) allows via exactly three routes:

1. **Ancestor bypass** — `canAccessAsAncestor(agent.ID(), resource.Ancestry)`. Only
   `agentResource()` populates `Ancestry`, so this covers an agent acting on its
   **descendants**. Note it does **not** cover self-access: an agent's own ancestry is
   `[root_user, …, parent]` and does not contain itself.
2. **Policy match** on principals `{agent:<id>}` ∪ effective groups.
3. **Delegation fallback**.

Effective groups for an agent (`group_store.go:819-885`) are: explicit memberships, plus the
implicit `project:<slug>:agents` group resolved at query time from the agent's project, plus
transitive parents. The implicit group needs no membership plumbing — **it is already
correct and already wired**.

But **nothing is bound to it.** The only two automatic policy bindings are `seed.go:98`
(`hub-member-create-projects` → hub-members, users) and `handlers_projects_core.go:639`
(`project:<slug>:member-create-agents` → project members group, users). So after Part 1,
agents are denied nearly everywhere.

### 5.2 The reframing: the agent model already exists

The brief's three options all assume the answer must be expressed in the policy engine. It
does not have to be, because **Scion already has a coherent agent-authorization model that
is not the policy engine**: JWT token scope + project isolation.

It is implemented correctly, exhaustively, and fail-closed in three places:

| Site | Agent branch |
|---|---|
| `createAgent` (`handlers_agents_core.go:323-343`) | `HasScope(ScopeAgentCreate)` + `req.ProjectID == agentIdent.ProjectID()` |
| `authorizeProjectImport` (`handlers_resource_import.go:477-487`) | `HasScope(ScopeAgentCreate)` + project match |
| `handleProjectBroadcast` (`handlers_agent_messaging.go:395-406`) | `HasScope(ScopeAgentLifecycle)` + project match |

The scopes are declared and documented for precisely this purpose (`agenttoken.go:44-58`):

```go
ScopeAgentCreate    = "project:agent:create"     // create sub-agents within the same project
ScopeAgentLifecycle = "project:agent:lifecycle"  // start/stop/restart agents within the same project
ScopeAgentNotify    = "project:agent:notify"
ScopeProjectSecretRead = "project:secret:read"
```

They are granted deliberately, per-agent, from the agent's template
(`hub_access.scopes` → `AppliedConfig.HubAccessScopes`,
`handlers_agent_create_helpers.go:175-177`) — i.e. they are already an *administered*
capability grant, not an ambient one.

**So the bug is not only "wrong getter." It is that ~24 sites have no agent model at all** —
a user model plus an implicit fall-through. Part 2's job is to apply the model that exists
uniformly, not to design a new one.

### 5.3 Evaluation of the brief's options

#### Narrated comparison (requested by ptone)

**Setup.** Project `acme-web` (P1), owned by alice, containing agents `builder` and `tester`
(both created by alice) and `builder-kid` (created by `builder`). Separate project
`acme-api` (P2) contains `api-worker`.

**Actor throughout: `builder`**, presenting its agent JWT.
ProjectID = P1, Ancestry = `[alice]`, Scopes = `[agent:status:update, agent:log:append,
project:agent:create]`. Note it does **not** hold `project:agent:lifecycle` — that lets the
table show what the scope gate actually gates.

| # | `builder` requests | Today | A | B (rec.) | C |
|---|---|---|---|---|---|
| 1 | `GET /agents/builder` — read self | ✅ | ✅ | ✅ | ✅ |
| 2 | `GET /agents/tester` — read sibling | ✅ | ✅ | ✅ | ✅ |
| 3 | `GET /agents/tester/logs` | ✅ | ✅ | ✅ | ✅ |
| 4 | `GET /agents/builder-kid` — read own child | ✅ | ✅ | ✅ | ✅ |
| 5 | `GET /projects/P1/message-logs` | ✅ | ⚠️ see below | ✅ | ✅ |
| 6 | `DELETE /agents/builder-kid` — delete own child | ✅ | ✅ | ✅ | ✅ |
| 7 | `DELETE /agents/tester` — delete sibling | ✅ | ❌ | ❌ | **✅** |
| 8 | `POST /agents/tester/pty` — shell into sibling | ✅ | ❌ | ❌ | **✅** |
| 9 | `POST /agents/tester/restart` — no lifecycle scope | ✅ | ❌ | ❌ | **✅** |
| 10 | `POST /projects/P1/agents` — create in own project | ✅ | ✅ | ✅ | ✅ |
| 11 | `POST /projects/P2/agents` — create cross-project | **✅** | ❌ | ❌ | ❌ |
| 12 | `GET /agents/api-worker` — read cross-project | 404 | 404 | 404 | 404 |
| 13 | `PATCH /projects/P1` — rename own project | ✅ | ❌ | ❌ | **✅** |
| 14 | `DELETE /projects/P1` — delete own project | ✅ | ❌ | ❌ | **✅** |
| 15 | `PUT /github-app` — overwrite hub private key | **✅** | ❌ | ❌ | ❌ |

Rows 1–4 and 6 are the legitimate traffic we must not break. Rows 11 and 15 are the
headline bugs. Rows 7–9 and 13–14 are where the options actually differ.

**The decisive observation: A and B produce identical outcomes on every row except 5 — and
on row 5, A is worse.**

Row 5 in detail. `handleProjectMessageLogs` checks `projectResource(project)` /
`ActionRead`. To let an agent read its own project's logs:

- **Under B**, the engine computes `projectIDForResource(resource)` → `P1` and compares it
  to `agent.ProjectID()` → `P1`. The comparison is made from the agent's own token claim, so
  it cannot be misconfigured.
- **Under A**, you seed a policy with `ResourceType: "project"`, `ScopeType: "project"`,
  `ScopeID: P1`, bound to `project:acme-web:agents`. But `matchesResource`
  (`authz.go:369-374`) only enforces project scoping when `resource.ParentType == "project"`,
  and `projectResource()` (`capabilities.go:70-77`) sets no `ParentType`. So the scope check
  is skipped and **the policy matches project P2 as well**. `builder` is granted
  project-read on every project in the hub.

  Today two isolation checks happen to mask this. The trap is that Option A's whole selling
  point is that policy now expresses the isolation — which invites removing those redundant
  checks, at which point the grant becomes live. The defect is quiet exactly until someone
  acts on the design's own logic.

So Option A costs a backfill migration over every existing project, plus an engine fix to
`matchesResource` that changes evaluation for user-authored policies too — in order to arrive
where Option B arrives with neither.

**What A genuinely buys** and B does not: per-project tunability. An admin could widen or
narrow one project's agent baseline by editing that project's policy row. B is uniform
hub-wide, adjustable only by binding an explicit deny. Given ptone's Q3 framing — *"establish
the clear control points, we can adjust policy detail later"* — that flexibility is the thing
we are deliberately deferring, and §5.4 keeps the door open at zero migration cost.

**Option C** differs on rows 7, 8, 9, 13 and 14. Note what row 14 means concretely: an agent
could delete the project it lives in. And row 8 is the cross-project PTY vector, merely
narrowed to same-project rather than closed. C re-grants most of the severity table.

---

**Option A — seed a policy bound to `project:<slug>:agents`.** Rejected for now, on three
counts:

1. **It needs a backfill migration.** Seeding at project creation
   (`handlers_projects_core.go:441`) only covers new projects. Every existing project needs
   the policy created retroactively, inside the same PR as a 24-site security conversion.
2. **It steps directly into a latent policy-engine defect.** `matchesResource`
   (`authz.go:369-374`) enforces project scoping only when `resource.ParentType == "project"`:
   ```go
   case "project":
       if policy.ScopeID != "" && resource.ParentType == "project" && resource.ParentID != policy.ScopeID {
           return false
       }
   ```
   `projectResource()` (`capabilities.go:70-77`) sets **no** `ParentType`. So a
   project-scoped policy with `ResourceType: "project"` matches **every** project. A seeded
   policy granting agents `project`/`read` in project P would grant it in every project —
   reintroducing a cross-project read while fixing one. The defect is latent today only
   because nothing seeds such a policy. Option A requires fixing `matchesResource` first,
   which changes evaluation for existing user-authored policies too.
3. **Once corrected for the Group B sites, it is functionally identical to Option B** — the
   same grant, expressed with a migration, N per-project policy rows, and an engine change.

**Option B — project isolation sufficient for read-class actions.** Recommended, with the
refinement below.

**Option C — project-scoped bypass mirroring the user path.** Rejected. It would grant an
agent project-owner-equivalent authority: delete its own project, mutate any sibling agent,
add itself as an owner-role group member. That is most of the severity table, re-granted.

### 5.4 Recommendation

**Two mechanisms, no seeded policy, no migration.**

**(1) A read-class project bypass in `checkAccessForAgent`,** inserted *after* policy
evaluation:

```go
func (a *AuthzService) checkAccessForAgent(...) Decision {
    // 0. ancestor bypass                                    [unchanged]
    // 1. build principals + effective groups                [unchanged]
    // 2. decision := a.evaluatePolicies(...)                [unchanged]
    //    if decision.PolicyID != "" { return decision }     [unchanged]

    // 3. NEW — project-scoped read baseline.
    //    An agent may perform read-class actions on resources in its own project.
    //    This codifies the project-isolation gate that these paths already relied
    //    on before #591; it grants nothing that was not already reachable.
    if isReadClassAction(action) {
        if pid := projectIDForResource(resource); pid != "" && pid == agent.ProjectID() {
            return Decision{Allowed: true, Reason: "agent project read baseline", Scope: "project"}
        }
    }

    // 4. delegation fallback                                [unchanged]
}

func isReadClassAction(a Action) bool { return a == ActionRead || a == ActionList }
```

Four properties that make this the right shape:

- **Position after step 2 makes it revocable.** `evaluatePolicies` returns any matched
  policy — allow *or* deny — and step 2 returns it before the baseline is reached. So an
  admin can bind an explicit `deny` policy to `project:<slug>:agents` and it will win. This
  is strictly better than the user path, whose bypasses precede policy evaluation and
  therefore cannot be overridden at all.
- **`pid != ""` is load-bearing.** Resources with no project — `brokerResource`,
  `templateResource`, the GitHub App config, hub-scoped resources — yield `""` from
  `projectIDForResource` and get no bypass. Omitting that guard would make empty match empty
  and allow everything. This must be an explicit test.
- **Read-class is `ActionRead` and `ActionList` only.** Deliberately **not** `ActionAttach`:
  PTY, exec, and message mutate a running agent and include the cross-project PTY vector.
  Not `ActionCreate`: handled by scope (§4.3). Not any mutation.
- **It grants nothing new.** Every Group B path already permitted exactly this via its
  project-isolation check. This is a consolidation of six copy-pasted checks into one
  enforced location, not a new grant.

**(2) Scope-gated helpers for the two legitimate mutating agent flows** — `ScopeAgentCreate`
for creation, `ScopeAgentLifecycle` for lifecycle actions — per §4.3.

**Everything else stays default-deny.** No policy is seeded. `project:<slug>:agents` remains
the admin extension hook: an operator wanting to grant agents more binds a policy to it
through the existing policy API, and Option A remains available later with no migration and
no engine change. We are choosing *not to spend the option now*, not closing it.

### 5.5 What agents lose

Intentional denials after this change — all of them the point of the fix:

- Delete, update, or PTY into any agent outside their own project (and, within their own
  project, any of these on a non-descendant without an explicit policy).
- Update or delete any project, group, or runtime broker.
- Add group members, including owner-role members.
- Overwrite the hub GitHub App private key and webhook secret.
- Assign a GCP service account they have no read access to.
- Create agents in a project other than their own.

Retained: read/list within their own project; full access to their own descendants via the
ancestry bypass; create and lifecycle within their own project *when the corresponding token
scope was granted by their template*.

The one genuinely debatable retention is lifecycle-on-peers — **Q3**.

---

## 6. Group B site-by-site: how each passes after conversion

| Site | Resource / Action | Passes via |
|---|---|---|
| `handlers_agents_core.go:1442` `getAgent` | `agentResource` / `ActionRead` | Read baseline — `projectIDForResource` → `a.ProjectID`, matches. Covers self-read (**not** covered by the ancestry bypass, since an agent is not in its own ancestry) and sibling-read. Cross-project still 404s at the isolation check, which runs first. |
| `handlers_logs.go:47` `handleAgentLogs` | `agentResource` / `ActionRead` | Read baseline. **Requires the reorder of §4.2.** |
| `handlers_logs.go:109` `handleAgentCloudLogs` | `agentResource` / `ActionRead` | Read baseline. Requires reorder. |
| `handlers_logs.go:287` `handleAgentMessageLogs` | `agentResource` / `ActionRead` | Read baseline. Requires reorder. |
| `handlers_logs.go:447` `handleProjectMessageLogs` | `projectResource` / `ActionRead` | Read baseline — `projectIDForResource` returns `r.ID` for `Type=="project"`, matches the agent's project. |
| `handlers_projects_core.go:1720` `createProjectAgent` | n/a | **Not the read baseline.** `authorizeAgentCreate` — `ScopeAgentCreate` + project match. This is a *tightening*: the site has no gate today. Agents whose template does not grant `project:agent:create` and which were relying on this route will now be denied — correctly, since the same caller is already denied on `POST /api/v1/agents`. |

The three Group A log-stream sites (`:193`, `:364`, `:522`) are the streaming mirrors of
`:109`, `:287`, `:447` and pass the same way — with the difference that they gain the
isolation check they never had.

---

## 7. Lint rule

### 7.1 What it must detect

Not "uses `GetUserIdentityFromContext`" — that fires on ~25 legitimate attribution sites
(§3.4) and will be suppressed within a week. The rule must detect **an authorization check
inside an identity-kind guard that has no `else`**, in three syntactic forms:

```
if <ident> := GetUserIdentityFromContext(ctx); <ident> != nil { … CheckAccess … }   // no else
if <ident>, ok := <x>.(UserIdentity); ok           { … CheckAccess … }              // no else
<ident> := GetUserIdentityFromContext(ctx); if <ident> == nil { return true }       // fail-open
```

The discriminator for the first two is the presence of `authzService.CheckAccess` (or
`requireAdmin`, or a hand-rolled `Role() != "admin"` comparison) **inside** the guarded block.

The third form was added during implementation and is not in the original inventory's shape
list. It is the same bug written inside-out — an early return of `true` on the non-user
caller — and it has no benign reading. Without it, the two §4.4 sites
`checkBrokerDispatchAccess` (`handlers_runtime_brokers.go:435`) and `canDispatchToBroker`
(`handlers_agent_create_helpers.go:883`) get no regression coverage at all.

**Test case.** An earlier draft named `handlers_github_app.go:113` vs `:121` as the canonical
example — "same file, adjacent lines, opposite verdicts." That is factually wrong; see the
correction in §3.4. `:113` is a function declaration with no guard, so there is no pair.

The rule is instead verified by `./hack/check-authz-guards.sh --self-test`, which runs the
classifier over an embedded fixture covering each shape positively and negatively, with the
`:121` attribution shape as the explicit negative twin of the gate shape. Testing the
classifier directly is strictly better than pinning it to one file's current contents: the
fixture cannot silently stop being a test case when someone edits a handler.

### 7.2 Mechanism

**Follow the repo's existing precedent: a ripgrep + allowlist script in `hack/`,** modelled
on `hack/check-project-compat-literals.sh`, wired into `make ci`.

Rationale — I checked the alternatives:

- **There is no `.golangci.yml` in the repo.** golangci-lint runs with built-in defaults
  only, so `forbidigo`/`depguard` would mean introducing a config file, which would also
  surface a wave of pre-existing default-linter findings.
- **Decisively: the golangci-lint CI job uses `only-new-issues: true`
  (`.github/workflows/ci.yml:98`) and the Makefile target uses `--new-from-rev=main`.** A
  golangci-lint-based rule would not flag a single existing violation. For a regression
  guard on a security fix, retroactive coverage is the whole point.
- A true `go/analysis` analyzer is the highest-fidelity option but is entirely greenfield
  here: no `tools/`, no `x/tools` in `go.mod`, no analyzer plumbing, plus a custom-plugin or
  `unitchecker` build. Not justified for a pattern that is reliably detectable lexically.

Concretely: `hack/check-authz-guards.sh` — multiline `rg` for the two guard shapes with
`CheckAccess` in the body, an anchored `allowed_paths` allowlist carrying an intent comment
per entry, `exit 0` with a warning if `rg` is absent (CI installs it at
`.github/workflows/ci.yml:67-71`), non-zero exit on unlisted hits. Add a `.PHONY` target
beside `compat-literals` (`Makefile:70-72`) and register it in the `ci` and `ci-full` lists.

**The allowlist should be empty on merge.** That is the acceptance criterion for Part 1.

### 7.3 What it cannot detect, and why that must be said out loud

The rule keys on the **shape of a guard**. It therefore cannot detect **the absence of any
check at all** — absence has no syntax to match. This is not a gap to be closed by a better
regex; it is the boundary of the technique.

Four rows in the §4.4 conversion table are exactly that class. They are "add a check that does
not exist today," they have no guard to key on, and **they will never appear in this script's
output**:

| Site | Function | What is missing |
|---|---|---|
| `handlers_agents_core.go:1468-1611` | `updateAgent` | no authorization |
| `handlers_projects_core.go:1720` | `createProjectAgent` | no gate at all |
| `handlers_groups.go:685` | `getGroupMember` | no authorization (§3.5) |
| `handlers_github_app.go:113` | `handleUpdateGitHubApp` | no authorization (§3.4) |

**If an implementer skips one of these four, the build stays green.** They must be confirmed by
a reviewer against this list, not by CI. Note that the last one is the single most sensitive
endpoint in the change — it writes the hub's GitHub App private key.

Two things follow, and both belong in the PR description rather than in tribal memory:

- The guard is a **regression barrier against reintroducing the idiom**, not a completeness
  check on authorization coverage. Describing it as the latter would be overselling it, and
  would invite exactly the false confidence that let #591 persist.
- Coverage of the absent-check class needs a route manifest that asserts every registered
  route reaches an authorization decision — filed as **#598**, out of scope here.

---

## 8. Secondary findings

### 8.1 Broker-header short-circuit — IN SCOPE (Q4 resolved: fail closed)

`UnifiedAuthMiddleware` (`auth.go:143-152`) short-circuits on the mere presence of
`X-Scion-Broker-ID`, setting only an auth-type *label* — never an identity — and calling
`next.ServeHTTP`. If `brokerAuthService` is nil, `BrokerAuthMiddleware` is never installed
(`server.go:2875`), and a bare `curl -H "X-Scion-Broker-ID: anything"` reaches handlers
fully unauthenticated.

**Exploitability, verified:** `Enabled: true` is hardcoded at both construction sites
(`server.go:251`, `cmd/server_foreground.go:1292`) and there is **no settings key, env var,
or CLI flag that can set it false.** So it is unreachable in any shipped build. It is
reachable only via a zero-valued `ServerConfig` — tests, embedded/library use, or a future
`ServerConfig` literal that bypasses `DefaultServerConfig()`.

That makes it not currently exploitable, but fail-open *by convention rather than by
construction* — the same property that produced #591. **Recommend fixing in this PR**: never
call `next.ServeHTTP` from that branch without a validated identity; if broker auth is
unavailable, reject the request rather than passing it through. Small, thematically
identical, and cheap to test.

### 8.2 `matchesResource` project scoping — class-level engine defect (filed as #595)

**Status:** filed publicly as **ptone/scion#595** on ptone's instruction. sa-arch found this
independently from the `gcp_service_account` and `template` sides during P0.2; I found it from
the `projectResource` side via §5.3. Same defect. **This track owns `authz.go`, so if it lands
in code it lands here.** Specified below so it is ready either way.

**The defect.** `matchesResource` (`authz.go:369-374`):

```go
case "project":
    // Policy scoped to a project — resource must be in that project
    if policy.ScopeID != "" && resource.ParentType == "project" && resource.ParentID != policy.ScopeID {
        return false
    }
```

The scope check is written as a **deny-list**: it rejects only resources that *declare* a
project parent and disagree. A resource with no `ParentType` satisfies none of the
conditions, falls through, and **matches**. So a project-scoped policy silently applies to
every parentless resource in the hub.

**It is broader than three resource types.** Surveying `capabilities.go`, the builders that
set no project parent are:

| Builder | `ParentType` | Affected |
|---|---|---|
| `agentResource` `:57` | always `"project"` | no |
| `gcpServiceAccountResource` `:157` | always `"project"` | no |
| `projectResource` `:70` | **none** | yes |
| `templateResource` `:80` | **none** | yes |
| `brokerResource` `:148` | **none** | yes |
| `userResource` `:125` | **none** | yes |
| `groupResource` `:108` | conditional (`:118`) | yes, when hub-scoped |
| `policyResource` `:133` | conditional (`:141`) | yes, when hub-scoped |
| `harnessConfigResource` `:89` | conditional (`:101`) | yes, when global- or user-scoped |

**`harness_config` is the reachability case, and I initially missed it.** sa-arch caught it.
`HarnessConfigScope` is `global` / `project` / `user` (`models.go:637-639`) and the parent is
set only for `project`. Global harness configs are **created by default bootstrap** —
`resource_bootstrap.go:135` and `system_handlers.go:558` both write
`Scope: store.HarnessConfigScopeGlobal`. So parentless resources of this type already exist on
essentially every hub with no operator action, which makes the defect a statement about live
data rather than about a hypothetical configuration. A harness config determines the harness an
agent runs, so blast radius is comparable to `template`.

Framing for the writeup: **lead with `policy`** for the self-referential argument (a
project-scoped grant reaching the hub's own authorization configuration), **cite
`harness_config`** for reachability. `gcp_service_account` stays forward-looking — it sets
`ParentType` unconditionally today and becomes affected only once svc-accnt P4 makes it
hub/user-scoped.

**`enforceUATConstraints` has the same class defect but a different shape.** `authz.go:413-419`:

```go
if resource.Type == "project" {
    if resource.ID != projectID { /* deny */ }
} else if resource.ParentType == "project" && resource.ParentID != projectID {
    /* deny */
}
```

It carries the explicit `Type == "project"` arm that `matchesResource` lacks entirely — that
difference *is* the `project` half of the bug. So on the UAT path `project` is **not** affected;
`template`, `broker`, `user`, `harness_config`, and hub-scoped `group`/`policy` are. sa-arch
originally described the two functions as identical in shape and has since corrected it.

A mitigating factor on the UAT path only: after the project check, `authz.go:423` requires
`resource.Type + ":" + action` to be in the token's scopes. A UAT scoped to project X reaching a
parentless template still needs `template:read` on the token. That is a scope-gated reach rather
than an open one — it lowers severity, not the need to fix.

**The proposed point-fix does not fix the class.** sa-arch's suggested change — *"compare
`resource.ID` when `resource.Type == "project"`"* — repairs only `projectResource`. Templates,
brokers, users, and hub-scoped groups and policies would still fall through. It is a
point-fix for one of seven affected types, and adopting it under the "class-level" label would
leave the defect live while closing the ticket.

**Recommended class-level fix** — invert to an allow-list and reuse the helper that already
encodes this exact question:

```go
case "project":
    // A project-scoped policy applies only to resources that resolve to that
    // project. Parentless / hub-scoped resources resolve to "" and must NOT
    // match — fail closed rather than falling through. No outer ScopeID guard:
    // a project-scoped policy with an empty ScopeID matches nothing.
    if pid := projectIDForResource(resource); pid == "" || pid != policy.ScopeID {
        return false
    }
```

**The outer `if policy.ScopeID != ""` guard is deliberately dropped** — sa-arch caught that
keeping it reproduces the same overload one level up: a project-scoped policy with an *empty*
`ScopeID` would skip the check entirely and match everything. Not reachable through the API
(`createPolicy:191` requires `scopeId` for project scope) and no seeded row produces it, so
this is hardening rather than a defect fix. Dropping the guard costs nothing, because
`pid == ""` is already rejected and a non-empty `pid` can never equal an empty `ScopeID`.

`projectIDForResource` already exists at `authz.go:446-454` and handles both arms
(`Type == "project"` → `r.ID`; `ParentType == "project"` → `r.ParentID`; else `""`). Three
properties:

- **One change covers all seven affected types, and every future resource type**, because new
  builders default to `""` and are therefore excluded rather than included.
- **It is the same helper the read-class baseline uses** (§5.4). After this, the engine has a
  single definition of "which project is this resource in," used for both policy scoping and
  the agent baseline. Today those would be two different notions in the same file.
- Fail-closed by construction, which is the theme of the whole change.

**Reachability: operator-reachable, not firing by default.** All three auto-created policies
miss. `seed.go:51` (`hub-member-read-all`) and `seed.go:62` (`hub-member-create-projects`) are
`ScopeType: "hub"`, so the `case "project"` arm never runs.
`handlers_projects_core.go:588` (`project:<slug>:member-create-agents`) is project-scoped but
`ResourceType: "agent"`, which fails the type check at `authz.go:359` before scope matching
even runs — and `agentResource` always carries a project parent regardless.

Two activation paths. The first is **misconfiguration**: an operator writes a project-scoped
policy with `resourceType: "template"`, `"harness_config"`, or `"*"`. `handlers_policies.go:178-200`
validates `name`, `scopeType`, `scopeId`, `actions`, and `effect` — but **never validates
`resourceType`** against a known set, and nothing warns. That is a real footgun but it requires
a mistake.

The second is sharper, and sa-arch's point: **the defect activates when someone *correctly*
implements the recommended refactor.** The hand-rolled project-isolation checks scattered
through the handlers are exactly what masks this today, and Option A's own logic calls for
removing them once policy evaluation is trusted to express isolation. So it needs no
misconfiguration and no neglect. This contributed to recommending B (§5.4) — but the defect
stays armed either way for whoever revisits A later.

**Blast radius.** The only configurations that change
behaviour are **user-authored project-scoped policies targeting a parentless resource type** —
which are precisely the ones silently over-matching today. Low real-world risk, but it is a
behaviour change for existing policies and must be called out in the PR description, not
buried.

**On fix shape.** Worth stating the principle explicitly, because it generalises past this
patch: absence of a parent is currently **overloaded**. It means both *"this resource is not in
a project"* and *"no project restriction applies."* Those must come apart. A hub-scoped resource
should be **outside** a project-scoped policy's reach, not **unconstrained** by it. The
`pid == ""` clause above does that minimally; the durable version is to make "not in a project"
a positively represented state rather than something inferred from a missing field.

Adding `resourceType` validation in `handlers_policies.go` is worthwhile independently, but it
is defence in depth — it does not fix the engine. **If added, it must cover the update path as
well:** `updatePolicy:333-335` assigns `req.ResourceType` with no allow-list (it validates
`effect` at `:343` and nothing else), so a validator added only to `createPolicy` is bypassable
with a follow-up PATCH. Scope itself is not at risk on that path — `UpdatePolicyRequest`
(`:55-67`) carries no `ScopeType`/`ScopeID` field, so scope is immutable after creation. Update
widens the resource-type footgun, not the scope one.

**Recommendation on scope.** If ptone wants it in this PR, it belongs in **Phase 1**
(engine changes, before any call-site conversion), with tests asserting: project-scoped
policy + parentless resource → no match; + same-project child → match; + other-project child
→ no match; + the project itself → match; and both seeded policies unchanged. It is ~6 lines
and shares Phase 1's existing test scaffolding.

Two arguments against folding it in, for ptone to weigh: it is the only part of this change
that alters behaviour for **user-authored policies** rather than for agents, and it is not
required by anything else here. It **is** a hard prerequisite if Option A is ever adopted
(§5.3), so deferring it means Option A carries it as a dependency.

### 8.3 Dead auth helpers — tracking issue (Q4 follow-up)

`RequireAuth`, `RequireUserAuth`, `RequireRole` (`auth.go:457-500`) are unreferenced. Either
adopt them as the middleware layer or delete them; leaving them invites exactly the
misreading the source doc made — the source doc cited `auth.go:472,490` as evidence of
fail-closed enforcement, when in fact nothing calls them.

**ptone has asked for a tracking issue for dead-code / tech-debt cleanup** (Q4 follow-up).
This family is the first entry. Related candidates found while surveying: the
`checkBrokerDispatchAccess` / `canDispatchToBroker` duplication (two near-identical
fail-open helpers, one HTTP and one silent), and `requireAdmin` being file-private in
`skill_registry_handlers.go` while ~30 sites hand-roll `user.Role() != "admin"` instead.
Whether the `RequireAuth` family is deleted or adopted should be decided together with §12,
since adopting it is the seed of the middleware layer.

### 8.4 `metadata_mode` — two of three layers fail open on an unrecognised value

Raised by sa-arch off the §3.3 finding. I traced it independently; the conclusion below is
the corrected one, and it is **not** the benign "sidecar fails closed" reading that was
briefly circulated mid-review.

The mode is consumed by **string equality only, at every layer**. There is no normalising
resolver — note the asymmetry with `store.ResolveWorkspaceSharingMode`
(`pkg/store/models.go:235`), which exists precisely so that *"unrecognized or future wire
labels safely fall back."* There is no `ResolveGCPMetadataMode`.

On an unrecognised value:

| Layer | Site | Behaviour | |
|---|---|---|---|
| Hub — build config | `handlers_agents_core.go:569-585` | `switch` with **no default** → `AppliedConfig.GCPIdentity` left nil → env never set | **closed** |
| Broker | `runtimebroker/start_context.go:353-372` | `if mode == "assign" \|\| mode == "block"` with **no else** → `GCE_METADATA_HOST` / `GCE_METADATA_ROOT` never set → SDKs reach the real `169.254.169.254` | **OPEN** |
| Sidecar | `sciontool/metadata/server.go:631, :679` | deny-listed on `block`, not allow-listed on known modes → serves `email`/`scopes`/`token`/`identity` instead of 403 | **OPEN** |
| Sidecar | `sciontool/metadata/server.go:332` | `if s.config.Mode != modeBlock { return }` → skips the defence-in-depth `iptables REJECT` on the metadata IP | **OPEN** |

Two further details I found that are not yet in sa-arch's write-up and belong in the P3
brief:

- **`agent/run.go:1364` matches by prefix, not equality:**
  `strings.HasPrefix(e, "SCION_METADATA_MODE=block")`. So `SCION_METADATA_MODE=blocked`
  satisfies the `block` test and is granted `NET_ADMIN`, while failing the broker's equality
  test at `start_context.go:361`. The two layers disagree about what "block" means.
- **`GCPIdentity != nil` with `MetadataMode == ""` — UNREACHABLE, defence-in-depth only.**
  I audited every construction site of `store.GCPIdentityConfig` in the repo: there are
  eleven, all in `handlers_agents_core.go` (`:571, :578, :582, :591, :598, :606, :611, :617,
  :1573, :1577, :1590`), and **every one assigns one of the three exported constants**.
  There is no literal, no variable copy, and no path that deserializes user JSON straight
  into the store type. Specifically: the PATCH path has a terminating `default:` (`:1596`)
  that rejects `""`; the project-scoped PATCH does not accept `gcp_identity` at all;
  templates cannot carry a GCP block (no `gcp` field exists in `api.ScionConfig`, asserted by
  `TestApplyProjectDefaults_GCPIdentityNotApplied`); and there is no agent import/export or
  clone endpoint (`handlers_resource_import.go:306` restricts `kind` to `template` and
  `harness-config`).

  This corrects my own earlier framing of this item, which implied the combination was
  produced on the create path. It is not.

  It remains worth defending because the store layer does not validate on read
  (`agent_store.go:136-141`), so a hand-edited row, a restored backup, or a future migration
  would land fail-open at `start_context.go:353-355` — the non-nil-but-empty config clobbers
  the broker's `"block"` default and then matches neither arm of the
  `== "assign" || == "block"` test. The defence belongs in the broker/sidecar allow-listing,
  not at the Hub edge.

- **The same read path also swallows its unmarshal error** (contributed by sa-arch, verified):

  ```go
  if a.AppliedConfig != "" {
      var cfg store.AgentAppliedConfig
      if err := json.Unmarshal([]byte(a.AppliedConfig), &cfg); err == nil {
          sa.AppliedConfig = &cfg
      }
  }
  ```
  `agent_store.go:136-141`. On corrupt JSON the error is discarded and `AppliedConfig` is
  left nil — indistinguishable from an agent that legitimately has no applied config. A
  corrupted record therefore presents as an unconfigured one, silently, and every downstream
  nil-check reads it as "no GCP identity configured." Second symptom of the same
  no-validation-on-read weakness as the bullet above.

**Correct characterisation of the risk.** Not currently exploitable: no path gets a raw
unknown mode past the Hub, and the Hub token broker (`handlers_gcp_identity.go:792, :874`)
is strictly `== assign`, so no *assigned* SA token can be minted. The realistic impact if it
were reached is loss of confinement of the **underlying node/broker compute identity** —
which is exactly what `block` mode exists to prevent.

But safety rests on a thin and largely accidental margin, and one correction to how that
margin has been described:

> sa-arch's corrected note says safety rests on the missing default arm at `:569`. That is
> the *backstop*. The **primary** barrier is the deliberate validating `default:` at
> `handlers_agents_core.go:308` (create) and `:1597` (PATCH), which return an explicit
> `ValidationError`. Those are real, documented invariants.
>
> **The reason this matters to this track is that `createProjectAgent` bypasses the primary
> barrier entirely.** On `POST /api/v1/projects/{id}/agents` the deliberate check is absent
> and safety really does rest on the accidental `:569` behaviour alone. That is the thin
> margin, and it is thin on exactly the route §3.3 already identifies as ungated.

So this track does not own the layered fix, but it does own restoring the primary barrier on
the route that lost it. See §9 Phase 2 for how, and note the ownership refinement there — I
recommend a different placement than the one sa-arch proposed, to avoid a rebase collision.

**Why the Hub-edge validation is the load-bearing part** (trace contributed by sa-arch;
recorded as their finding, I have not re-verified the sidecar-start divergence end-to-end).
`httpdispatcher.go:1341` injects `resolvedEnv["SCION_METADATA_MODE"] = gcpID.MetadataMode`
verbatim from `AppliedConfig` on every start and restart — no validation, no allow-list — and
`start_context.go:360` only ever *adds* env keys for `assign|block`, never removes them. So
any stored mode string reaches the agent container unfiltered, and create and restart can
diverge, with restart on the fail-open side.

The point for this design: `validateGCPIdentityRequest()` at the write chokepoint is not
merely input hygiene. It is the **sole guarantee** for a propagation path several layers
downstream that nobody would discover by reading the create handler. That is the argument for
putting it in `createAgentInProject` rather than duplicating it per-route — one chokepoint,
one guarantee. Given the validation lands, `:1341` needs no change; P7 fixes the sidecar end.

**Owned by svc-accnt P7** (moved from P3 — separate package, unblocked today, no reason to
gate it behind Q2): allow-listing rather than deny-listing in the broker and sidecar, a
`store.ResolveGCPMetadataMode` mirroring the workspace-sharing resolver, the `HasPrefix`
mismatch at `agent/run.go:1364`, and both no-validation-on-read symptoms at
`agent_store.go:136-141`. Not this track.

---

## 9. Phased implementation plan

**Part 1 and Part 2 must land together.** The phases below are commits in one stack, merged
as a unit. Phase 2 must not merge without Phase 1: converting call sites before the baseline
exists denies agents on every Group B path.

### Phase 0 — Foundations (no behaviour change)

**Files:** `pkg/hub/authorize.go` (new), `pkg/hub/errors.go`, `pkg/hub/skill_registry_handlers.go`

- `authorize`, `authorizeMsg` (§4.1).
- `authorizeAgentCreate`, `authorizeAgentLifecycle` (§4.3).
- Promote `requireAdmin` out of `skill_registry_handlers.go` into `authorize.go`.

**Tests:** table-driven over identity kinds — nil → 401; user allowed → true; user denied →
403; agent allowed → true; agent denied → 403; broker → 403. Assert the denial log line is
emitted with `principal_type`.

**Reviewable in isolation. Nothing calls these yet.**

### Phase 1 — Part 2 baseline

**Files:** `pkg/hub/authz.go`

- `isReadClassAction`; the read-class project bypass in `checkAccessForAgent` (§5.4).

**Tests:**
- Agent + `ActionRead` + same-project agent resource → allow.
- Agent + `ActionRead` + other-project → deny.
- Agent + `ActionRead` + `projectResource` of own project → allow.
- Agent + `ActionUpdate`/`ActionDelete`/`ActionAttach` + same project → **deny** (the
  read-class boundary).
- Agent + `ActionRead` + resource with no project (`brokerResource`) → **deny** (the
  `pid != ""` guard).
- Agent + explicit `deny` policy on `project:<slug>:agents` + `ActionRead` + same project →
  **deny** (revocability).
- Agent + descendant resource + `ActionDelete` → allow (ancestor bypass unchanged).
- User path unchanged — regression suite must be untouched.

### Phase 2 — Call-site conversion

**Files:** `handlers_agents_core.go`, `handlers_projects_core.go`, `handlers_groups.go`,
`handlers_runtime_brokers.go`, `handlers_agent_create_helpers.go`, `handlers_logs.go`,
`pty_handlers.go`, `handlers_github_app.go`, `project_settings_handlers.go`,
`handlers_shared_dirs.go`, `project_pre_start_hook_handlers.go`

Order within the phase, so the risky work lands on a clean base:

1. Group B + the `handlers_logs.go` reorder (§4.2) — the sites with regression risk.
2. `authorizeAgentCreate` into `createAgent` + `createProjectAgent` (closes Q12 and §3.3).
3. **Hoist the field-level GCP identity validation into `createAgentInProject`** (§3.3 row 4).

   Extract `handlers_agents_core.go:294-311` into
   `validateGCPIdentityRequest(w http.ResponseWriter, cfg *store.GCPIdentityConfig) bool`,
   call it at the **top of `createAgentInProject`**, and delete the copy from `createAgent`.

   sa-arch proposed adding the block to `createProjectAgent`. I recommend the hoist instead,
   for three reasons:
   - `createAgentInProject` is the single choke point both routes already pass through.
     Duplicating the block into `createProjectAgent` leaves two copies of a validation that
     has already drifted once — it reproduces the failure mode rather than removing it.
   - **This track owns `createAgentInProject`.** sa-arch's note says svc-accnt P3 will
     "hoist the field-level validation into `createAgentInProject`" — that is the same edit
     to the same function, from a track that rebases onto this one. Doing it here means P3
     rebases onto a done deal instead of colliding with us in the repo's most
     security-sensitive function, which is the exact scenario the serialisation was
     designed to avoid.
   - It closes any *future* third caller of `createAgentInProject` by construction.

   Behaviour for `POST /api/v1/agents` is unchanged (same check, one frame later, still
   before any persistence or SA resolution).

   **Two constraints on the implementation:**

   - **Strict-reject, never normalise** (constraint from sa-arch, and I agree). An
     unrecognised or empty `metadata_mode` must return `ValidationError`. Do **not** coerce
     it to `block`. Silently rewriting input is precisely how the layer disagreements in
     §8.4 stayed invisible — a rejected request is a signal, a coerced one is not. Note this
     differs from the *downstream* fix P3 will make, where falling back to `block` is
     correct: at the edge we know the caller's intent is malformed and can say so; in the
     broker we only know the value is unusable and must fail safe.
   - **Also add a `default:` arm to the config-building switch at `:568-586`**, returning a
     500-class internal error. After the hoist it is unreachable by construction — that is
     the point. §8.4 is a case study in an invariant that held by accident and was described
     as a guarantee; making this one explicit costs three lines and converts the accident
     into a documented assertion. It also fixes the silent project-default drop in §3.3.
4. Group A1 mechanical conversions.
5. Group A2 (idiom 2, §3.2).
6. `handlers_github_app.go:113` — and **leave `:121` alone**; add a comment saying why.

**Tests: one regression test per site.** Each asserts an agent token receives 403 (or 404
where isolation runs first) on the path that previously succeeded. This is the test class
whose absence allowed the original bug — it is the deliverable, not an afterthought.

Additionally:
- Cross-project PTY denied.
- Cross-project agent creation via `POST /api/v1/projects/{other}/agents` denied.
- **Parity suite across both create routes** (§3.3): the same request body must produce the
  same verdict on `POST /api/v1/agents` and `POST /api/v1/projects/{id}/agents`. Cover
  `metadata_mode: block` + `service_account_id` → 400 on both; unrecognised `metadata_mode`
  → 400 on both; `assign` with no `service_account_id` → 400 on both; **`gcp_identity: {}`
  (empty mode) → 400 on both**, which is the §3.3 project-default-drop case. This suite is
  the regression guard for the drift class itself, not just for today's four instances.
- **Project-default application on the project route**: a project configured with
  `default-gcp-identity-mode: assign` must produce an agent with that mode via *both* create
  routes. Today the project route silently yields nil.
- Broker-authenticated request denied at each converted site.
- GitHub App update denied for non-admin *and* for agent.
- Positive tests that the legitimate agent flows still work: self-read, sibling log read,
  descendant delete, scoped create, scoped lifecycle.

### Phase 3 — Regression guard and hardening

**Files:** `hack/check-authz-guards.sh` (new), `Makefile`, `.github/workflows/ci.yml`,
`pkg/hub/auth.go`

- The lint script (§7), allowlist empty.
- Broker-header fail-closed fix (§8.1) + test that `X-Scion-Broker-ID` with no valid
  signature is rejected when `brokerAuthService` is nil.

### Not in this change

- `matchesResource` project scoping (§8.2) — separate issue.
- `handlers_agents_core.go:409` policy-engine alignment (§4.4 note) — separate issue.
- Dead `RequireAuth` family (§8.3) — separate issue.
- The downstream `metadata_mode` layering (§8.4) — owned by svc-accnt P3. This track fixes
  the Hub-edge validation only. **No verification task for the EM here:** the sidecar
  behaviour is traced and documented in §8.4, not open.
- Any seeded agent policy (Option A) — deferred, hook preserved.

---

## 10. svc-accnt coordination

Per the agreed sequencing, **this track owns `createAgentInProject` and `updateAgent` and
lands first.** svc-accnt P3 rebases onto the result.

What svc-accnt receives from this track:

| Item | Shape |
|---|---|
| `s.authorize(w, r, resource, action) bool` | `pkg/hub/authorize.go` — Goal 1's IAM gate layers on top of this, not beside it |
| `updateAgent` | Now carries `authorize(agentResource, ActionUpdate)` **and** the scope-match + `Verified` checks (§4.5). svc-accnt adds **only** the `CanActAs` IAM gate on top |
| `createAgentInProject` | The `:446` SA-assign check is converted to `authorizeMsg`; svc-accnt replaces `ActionRead` with `ActionAssign` + `CanActAs` |
| `createAgentInProject` | **Now opens with `validateGCPIdentityRequest`** (Phase 2 step 3). This is the hoist sa-arch scheduled for P3 — it is done, do not redo it. Both create routes are covered. |
| `authorizeAgentCreate` | Exhaustive caller-kind gate — svc-accnt's agent-caller reasoning can assume a validated caller kind |

**Owned by svc-accnt P7** (moved from P3): the layered `metadata_mode` fix per §8.4 —
allow-listing in `runtimebroker/start_context.go:353` and `sciontool/metadata/server.go:631,
:679, :332`, a `store.ResolveGCPMetadataMode`, the `HasPrefix` mismatch at
`agent/run.go:1364`, non-nil-config-with-empty-mode, and the swallowed unmarshal error at
`agent_store.go:138`. This track fixes only the Hub-edge validation, not the downstream
layers. P7 does not depend on this track and need not wait for it.

Note the `:409` recommendation in §4.4 (minimal fix, defer the policy alignment) — svc-accnt
should be aware that the passthrough check remains a hand-rolled comparison after this track
lands.

I have **not** assumed any concurrent edit to those two functions.

---

## 11. Open questions

**All four answered by ptone on 2026-07-28. No open questions remain.**

| # | Question | Resolution |
|---|---|---|
| **Q1** | Fold in the four idiom-2 sites (§3.2)? | **Yes.** In scope. |
| **Q2** | Read-class bypass (§5.4) rather than a seeded policy (Option A)? | **Yes**, after the narrated comparison in §5.3. |
| **Q3** | Agent lifecycle on **peers** via `ScopeAgentLifecycle`, or **descendants only**? | **Peers, via `ScopeAgentLifecycle`** — *"we want to establish the clear control points, I assume we can adjust policy detail later."* |
| **Q4** | Broker-header fail-closed fix in this PR, or separate issue? | **Fail closed, in this PR.** Plus a tracking issue for dead-code cleanup (§8.3). |

**On Q3's rationale, and why the design supports it.** ptone's framing — establish the
control points now, tune the policy later — is exactly what §5.4 is built for. Both
mechanisms are control points with a named, greppable enforcement site
(`authorizeAgentLifecycle`, `isReadClassAction`) rather than a distributed condition, so
tightening later means editing one function, not re-auditing 24 handlers. And because the
read baseline sits *after* policy evaluation, an admin can already narrow it per-project
today by binding a deny policy to `project:<slug>:agents`, without waiting for Option A.

The one thing to be explicit about: choosing peers-via-scope means an agent holding
`project:agent:lifecycle` can restart or exec into any sibling in its project. That is a real
grant, and it is broader than descendants-only. It is defensible because the scope is
template-administered rather than ambient, and because `handleProjectBroadcast`
(`handlers_agent_messaging.go:395`) already treats that scope as conferring exactly this
authority — so we are making an existing, deliberate grant uniform rather than inventing one.
If it later proves too broad, the narrowing is a two-line change in one helper.

## 12. Follow-on track: consistent authorization enforcement

ptone, on the §2.1 finding: *"it sounds like we want to establish more consistent use of auth
middleware? nearly all requests have Authentication check — most should have authorization
check as well."*

That is the right diagnosis, and it is the durable fix for this bug class. This section sizes
it honestly, because the obvious version does not work here.

**Why a straightforward authorization middleware cannot do the job.** Authorization needs the
*resource* — owner, project, labels, ancestry — which requires the DB fetch the handler is
already doing. A middleware running before the handler has only the URL. So middleware can
enforce *that* a check happens; it cannot perform the check for most routes.

**The real obstacle is that the route set does not exist as data.** I checked:

- Routing is `http.ServeMux` with **Go 1.0-style patterns** — no method, no `{id}` wildcards
  — despite `go.mod` declaring `go 1.26.1` (`server.go:738`, `setupRoutes` `:2682-2861`).
- **114 registered patterns**, of which 28 are prefix subtrees that fan out. They expand to
  roughly **300–400 distinct (method, path, action) operations**.
- Fan-out is up to **five layers** of hand-rolled `strings.TrimPrefix` / `SplitN` /
  `if HasPrefix(subPath, …)` ladders — `extractAction` (`server.go:3107`),
  `handleProjectRoutes` (~25 branches, `handlers_projects_core.go:1306-1561`),
  `handleAgentByID` (11 branches, `handlers_agents_core.go:1324`), plus 15 more dispatchers.
- **No OpenAPI spec, no generated client, no route-enumerating test exists.**
- Middleware is applied to the whole mux uniformly (`applyMiddleware`, `:2864-2899`). The
  sole per-route precedent is `requireWorkstation` (`server.go:2939`) on 13 endpoints.

So "a test that walks the route table" has nothing to walk. The table would have to be
hand-transcribed from ~30 dispatch ladders, and a new `if HasPrefix(subPath, "new-thing")`
branch would silently under-cover rather than fail. **That is the project hiding behind the
question, and it is weeks, not days.**

**Recommended staging.** Three steps, decreasing certainty and increasing cost:

1. **This PR** — `s.authorize()` (§4.1) plus the `hack/` lint rule (§7). This does not make
   enforcement uniform, but it makes it *uniform in shape and greppable*, which is the
   precondition for everything below. Denials also become observable via the structured log.
2. **Small, high-value, independent of this PR** — a manifest covering only the **86
   exact-path routes**, plus a test asserting the 14-entry public allowlist
   (`isUnauthenticatedEndpoint`, `auth.go:344-386`) matches a declared `public` class. A day
   or two. It has immediate value: it would already have caught two live inconsistencies I
   found — `/api/v1/settings/public` (`server.go:2818`) is *not* in the allowlist despite the
   name, and `/metrics` (`server.go:2685`) is not allowlisted while `/healthz` and `/readyz`
   are. Neither is a vulnerability; both are exactly the drift a manifest exists to catch.
3. **The actual fix, as its own track** — migrate to Go 1.22+ `ServeMux` patterns
   (`"POST /api/v1/agents/{id}/{action}"`) so `r.PathValue` replaces `extractAction`. This is
   mechanical but wide (~18 dispatcher files). It is worth doing on its own merits, and it is
   what makes the manifest enforceable rather than aspirational: once routes are enumerable,
   a default-deny registry where every route must declare its authz class becomes a small
   addition, and undeclared routes can fail closed.

**Recommendation: do not attempt 2 or 3 inside this PR.** Mixing a wide routing refactor into
a security fix is how the fix gets delayed and how review quality drops on the part that
matters. Step 2 is cheap enough to run in parallel with implementation by someone else. Step
3 should be a tracked follow-up with its own design.

One caveat worth recording for whoever picks up step 3: the second obstacle is not routing
but vocabulary. Authorization today is 93 inline `CheckAccess` calls with the `Action` chosen
per site, ~30 ad-hoc `Role() != "admin"` comparisons, one file-private `requireAdmin`, and
several bespoke in-dispatcher gates. Several decisions are genuinely conditional
(self-access, agent-vs-user, project isolation) and will not reduce to a single class per
route. The manifest will need a small set of classes plus an explicit
`handler-enforces` escape hatch — and that escape hatch must be *declared*, so it is
countable, rather than being the silent default it is today.

One further item needs a product answer before the PR is final, and I have not assumed one:

- **Are there deployed agents relying on `POST /api/v1/projects/{id}/agents` whose templates
  do not grant `project:agent:create`?** Those callers work today (the route has no gate) and
  will be denied after this change. The same caller is already denied on `POST /api/v1/agents`,
  so this is a consistency fix rather than a new restriction — but it is the most likely
  source of a production break, and it is worth a look at real template configs before
  shipping. Note that in dev-auth mode the scope is auto-granted (`server.go:1943-1946`), so
  local development is unaffected.
