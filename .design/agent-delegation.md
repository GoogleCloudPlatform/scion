# Agent-Bound Delegation and Credential Exchange

**Status:** G.1 design for review ([ptone/scion#2130](https://github.com/ptone/scion/issues/2130)).
Product decisions were made by ptone on 2026-09-28 and 2026-09-30 (§21). The decision-audit
placement follows the 2026-10-09 removal of routine decision-audit persistence (§14.4).
Implementation is G.2 ([ptone/scion#2131](https://github.com/ptone/scion/issues/2131)) and G.3
([ptone/scion#2132](https://github.com/ptone/scion/issues/2132)).
**Tracker:** G, [ptone/scion#2115](https://github.com/ptone/scion/issues/2115). This is an optional
extension that ships after ordinary bearer user access tokens (UATs). It does not gate them.
**Date:** 2026-10-10
**Anchored at:** GoogleCloudPlatform/scion `main` @ `abc63c5`
**Related contracts:** A.1 [ptone/scion#2117](https://github.com/ptone/scion/issues/2117), A.2
[ptone/scion#2118](https://github.com/ptone/scion/issues/2118), B.2
[ptone/scion#2120](https://github.com/ptone/scion/issues/2120), B.3
[ptone/scion#2121](https://github.com/ptone/scion/issues/2121), D.1
[ptone/scion#2123](https://github.com/ptone/scion/issues/2123), D.2
[ptone/scion#2124](https://github.com/ptone/scion/issues/2124), E.1
[ptone/scion#2126](https://github.com/ptone/scion/issues/2126), E.2
[ptone/scion#2127](https://github.com/ptone/scion/issues/2127), decision audit
[ptone/scion#2379](https://github.com/ptone/scion/issues/2379)
**Vocabulary:** `authorization-operation-contract.md`, `authorization-operation-catalog.md`,
`user-access-tokens.md`

All line numbers refer to `abc63c5`. Recheck them before G.2 starts. Function and type names that
already exist on `main` are cited as they are. Names introduced by this design (for example
`DelegatedAgentIdentity` and `decideAgentDelegation`) are working names: the G.2 PR may rename
them, but not change the behaviour this document fixes.

---

## 1. Problem and goals

An agent sometimes needs to act across projects, or on hub-level targets, on behalf of the person
who controls it. Today there are two ways to do that, and both are wrong:

- The agent's own JWT is project-scoped (`AgentTokenClaims`, `pkg/hub/agenttoken.go:141`). Its
  scopes are all project-relative (`ScopesForRole`, `pkg/hub/agentrole.go:44`), and the agent
  delegation ceiling is evaluated at project scope only (`checkDelegationCeiling`,
  `pkg/hub/authz_delegation_ceiling.go:157`, scope fixed at `:179`). It cannot carry hub authority,
  and it should not.
- The person can hand the agent a UAT. The hub then sees the **person** as the principal. The
  token's purpose and labels (E.1) are issuer-supplied text, not a verified actor, so audit cannot
  tell the agent's actions apart from the person's.

G adds an **agent delegation grant**. An interactive user creates a Hub record that binds one chosen
agent to a boundary, an exact frozen permission ceiling, an expiry and a no-subdelegation policy.
The agent authenticates with **its own agent credential** to **exchange** the grant for a
short-lived, audience-bound, opaque **delegated credential**. On every request made with that
credential, the hub:

1. treats the **agent as the verified actor**, with the issuing user as the **authorizing user**;
2. takes authority only from the grant. The agent's project JWT scopes and the agent's own
   relationships add nothing;
3. allows a request only if every one of these allows it: the credential's ceiling, the hub's
   agent-delegation policy, the issuer's **live** authority on the resolved target (evaluated the
   way a D.1 bearer of that boundary is evaluated), every access constraint, and the whole
   revocation chain (credential → grant → issuer → agent → the agent credential used at exchange);
4. admits a delegated credential only on a reviewed, code-owned allow-list of routes;
5. records both the actor agent and the authorizing user, the grant ID and the credential ID in
   audit. Creation ancestry is not rewritten.

Goals:

1. Specify issuance, exchange, revocation, use-time evaluation, audit and lifecycle precisely
   enough that G.2 and G.3 can be implemented without reopening core UAT boundaries.
2. **Ordinary bearer automation keeps working without this feature.** G adds a new credential kind
   behind a default-off experiment. It changes no UAT, agent JWT, session or broker behaviour, and
   it adds no hub authority to the project agent JWT (§17, acceptance criterion 1).
3. Distinguish a verified actor binding from proof of possession and from labels (§15).
4. State plainly what the delegated ceiling bounds (authorization decisions) and what it does not
   bound (computation that follows an allowed decision), especially for `agent.attach` (§11.9).

## 2. Non-goals

- Proof of possession (DPoP, mTLS, key-bound tokens). A delegated credential is a bearer once
  issued (§15).
- Subdelegation. The schema leaves room for it (§18.1); v1 rejects it.
- Replacing ordinary bearer UATs, or changing their human-principal semantics.
- Hub scopes on the project agent JWT. No system-scoped rows in `delegation_edges`.
- Grants sourced from an existing UAT. Issuance is session-only (§6).
- Revoking plaintext already delivered to a running process.
- Explaining delegated decisions through `POST /api/v1/authz/explain`. `resolveExplainPrincipal`
  (`pkg/hub/audit_authz.go:646`) builds only user and agent identities, and G does not extend it in
  v1.
- Confining computation that results from an allowed decision (§11.9).
- Ending an established attach session on revocation or expiry (§11.9, §16.6).
- Keeping a delegated credential valid across agent JWT rotation. The agent re-exchanges (§9.3).
- A web UI for grant management. It is deferred to
  [ptone/scion#2438](https://github.com/ptone/scion/issues/2438) (§18.9).

## 3. Current state (factual, at `abc63c5`)

### 3.1 Agent credentials

- **Mint.** `(*Server).AuthorizeAgentToken` (`pkg/hub/agent_token_mint.go:114`) computes an
  `AgentTokenGrant` from the stored agent row. Ancestry comes from `agent.Ancestry`. Scopes are
  filtered by the delegation-chain ceiling, and the agent must pass `agentStanding`
  (`pkg/hub/agent_standing.go:143`: not deleted, not held, chain live, root user active and
  admitted). `SignAgentToken` (`agent_token_mint.go:144`) signs it, and `recordAgentCredential`
  (`:197`) stores the credential row. No token is returned unless the row is stored.
- **Validation at the edge.** The agent arm of `UnifiedAuthMiddleware` (`pkg/hub/auth.go:270-377`)
  looks up the credential by JTI hash (`evaluateAgentCredentialStatus`, `agenttoken.go:216`). A
  revoked row → 401. A store error → 503. A token with **no** row still authenticates as a legacy
  token. Step 1b refuses a held agent, and step 1c applies the run-scope check.
- **Refresh.** `handleAgentTokenRefresh` (`pkg/hub/handlers_agents_core.go:5188`) re-mints through
  `AuthorizeAgentToken` and then revokes the presented credential **best-effort**
  (`:5320-5325`).
- **Cascades.** Delete revokes agent credentials fail-stop inside the deletion engine
  (`agent_delete_engine.go:995`). Suspend revokes them best-effort
  (`handlers_agent_lifecycle.go:486-497`).

### 3.2 Bearer evaluation, boundaries and ceilings (A.1, A.2, D.1)

- `TokenBoundary{Kind, ProjectID}` (`pkg/hub/authz_boundary.go:49`), `ResolveTargetScope`
  (`:151`), `BoundaryAllows` (`:386`).
- `ProjectTargetAdmission(ctx, principal, projectID, permissionID, target, memo
  *ProjectAdmissionCache) (ProjectAdmissionResult, error)` (`authz_boundary.go:1504`): current
  membership (source `membership` or `group`) or a system grant for exactly that permission on that
  target (`system_role`). Never calls `Decide`. It accepts local, dev and federated users.
- `CanMintSelector` (`authz_boundary.go:1658`): batched, target-free mint eligibility for local
  users, with stable `MintDenialReason` codes (`:1581-1590`). `BuildCeilingFromSelectors`
  (`pkg/hub/permissions/ceiling.go:90`) builds a V1 `FrozenPermissionCeiling` (`ceiling.go:47`);
  `Allows` (`:61`) denies an empty list and an unknown version.
- `EvaluateBearerCeiling(ctx, user, boundary, ceiling, permissionID, target, opts BearerOptions)
  BearerEvaluation` (`pkg/hub/authz_bearer.go:352`). It runs the audit-free `decide` body, never
  reads the identity from `ctx`, and evaluates `permissionID` exactly as given
  (`TestEvaluateBearerCeiling_EvaluatesExactPermissionID`, `authz_bearer_test.go:257`;
  `TestEvaluateBearerCeiling_IgnoresContextIdentity`, `:275`). `user.Identity` must be an
  `*AuthenticatedUser`. For hub targets it does not check account status; the caller must.
  `BearerOptions.Memo` is the A.1 `*ProjectAdmissionCache`.
- `CredentialContext` (`pkg/hub/authz.go:189`) carries `Boundary *TokenBoundary` and `Ceiling`.
  `CredentialKind` aliases the closed set in `pkg/credentialmeta/metadata.go:40-55`.

### 3.3 Classification, routing and the operation catalog

- Identity classification is fail-closed. `principalContextForIdentity` (`authz.go:2247`) and
  `credentialContextForIdentity` (`:2290`) switch on concrete types, and an unknown type gets an
  empty kind, which `decide` denies (`:640-697`). `AncestryIsHubAttested` (`identity.go:368`)
  requires a local-provenance marker. `TestIdentityClassification_EveryTypeHasExplicitOutcome`
  (`identity_classification_test.go:286`) fails until every new identity type has a row.
- Agent sub-routes are resolved once in `routeGuard` (`route_metadata.go:1264`, resolver hook
  `:1287-1295`) into an immutable `AgentSubRoute{RouteID, OperationID, Method, AgentID,
  ProjectID, Suffix}` (`pkg/hub/agent_routes.go:111`), from `agentSubRouteTable` (`:195`) by
  `ResolveAgentSubRoute(method, escapedPath)` (`:352`). Dispatch switches on `RouteID`. A handler
  reached without a resolved route answers 500.
- Each catalog operation declares a `Bearer BearerDisposition` (`authzop/operation.go:109-113`,
  `authzop/bearer.go`): `admit`, `admit_self`, `session_only` (with a reason), `non_user` or
  `out_of_scope`. The only exemption is the `PendingBearerOperations` set
  (`authzop/pending.go:282`), whose members still await a disposition. It includes `agent.list`,
  `agent.message.send`, `skill.read` and `template.read`, which G admits later. `TestBearerDisposition_EveryOperationDeclaresOne`
  (`authzop/bearer_test.go:92`) and `TestBearerDispositionMatrix_CatalogEntryPoints`
  (`pkg/hub/bearer_disposition_matrix_test.go:452`) enforce it. G admitting a pending operation
  for delegated use does not depend on its UAT disposition, but the operation must have left the
  pending set (through D's bearer work) before G adds its admission, so the two changes never
  edit the same entry at once.
- `TestCatalogHTTPEntryPoints_LiveMethodCheck`
  (`pkg/hub/authzop_catalog_method_inventory_test.go:1179`) probes every catalogued HTTP entry
  point against the real mux. New routes need fixtures or a code-cited exclusion (§18.4).

### 3.4 Lifecycle seams (B.3)

`pkg/hub/agent_lifecycle_tx.go` defines `AgentTxHook func(ctx, tx store.Store, agent
*store.Agent, actor AuditActor) error` (`:80`) and four registries on `*Server`:
`RegisterSoftDeleteHook` (`:119`), `RegisterHardDeleteHook` (`:125`), `RegisterRestoreHook`
(`:131`) and `RegisterReincarnateClaimHook` (`:137`). Hooks run inside the lifecycle transaction,
after the agent-row write and the delegation-edge (de)activation and before the audit record, on a
transaction-scoped store. A non-nil error rolls back the whole operation. Soft-delete hooks do not
run on hard delete. Order and rollback are pinned by
`Test{SoftDelete,HardDelete,Restore,ReincarnateClaim}TxHookOrder` and `Test*HookErrorRollsBack`
(`agent_lifecycle_tx_test.go`). The reincarnation claim (`reincarnateClaimTx`, `:451`) is one
transaction: the `state_version` compare-and-swap, the reincarnation record, edge replacement,
hooks, then audit.

### 3.5 Audit

- `Decide` (`authz.go:612`) wraps the audit-free `decide` (`:628`) and emits exactly one decision
  record. `AuthzRequest.AlwaysAudit` (`:252`) and `Decision.AlwaysAudit` (`:375`) exempt a
  decision from allow sampling. `BuildDecisionAuditRecord` (`audit_authz.go:78`) builds a record
  for paths outside `Decide`.
- Routine decision records are no longer persisted in the database. GoogleCloudPlatform/scion#2986 removed the
  `decision_audits` table. The production emitter is inert (`server.go:2317`), and decision records
  reach the typed `pkg/hub/auditevent` sink only when the default-off experiment
  `hub.authorization_decision_audit_v2` is admitted, and then only for decisions inside the sink's
  recorded domain.
- Mutation audit (`store.MutationAuditRecord`) is persisted and can be written in the same
  transaction as the change it records.
- E.1 reserves the verified-actor label keys `actor_agent_id`, `authorizing_user_id`,
  `source_grant_id`, `delegation_edge_id`, `parent_grant_id`, `exchange_agent_credential_id` and
  `actor_kind` (`credentialmeta/metadata.go:288-302`). `pkg/hub/e2a_no_g_column_test.go:43`
  reserves the matching Go field names for G's audit block.

## 4. Design overview

```
issue (session) ──► grant[active] ──exchange(agent JWT)──► credential[active, ≤ TTL]
     │                  │  ▲                                    │
     │                  │  └──────── re-exchange = refresh ─────┘
     │   revoke / expire / issuer suspended or not admitted / agent deleted, held or suspended
     │   / reincarnation claimed / exchange agent credential revoked or rotated / policy narrows to ∅
     │                                                    ──► uses and exchanges deny
     │   a third active credential on one grant ──► the oldest is revoked ("superseded")
     ▼
 mutation audit rows (grant create, credential issue, grant revoke, credential revoke),
 each written in the same transaction as the change
```

- A grant confers nothing until it is exchanged. What it confers is always limited by live issuer
  authority.
- A delegated credential is an opaque bearer bound to one grant. It never refreshes itself.
- Every request with a delegated credential goes through one dedicated decision procedure
  (`decideAgentDelegation`, §11). None of the ordinary pipeline's agent or UAT steps run for it.
- G adds new files, and only small explicit arms in shared files (§12.3, §18.10).

## 5. Terms

- **Grant** (`AgentDelegationGrant`): a durable Hub record created by an issuer. It binds one
  agent to a boundary, a frozen ceiling and an expiry.
- **Delegated credential** (`AgentDelegatedCredential`): a short-lived opaque bearer produced by
  exchange. It references exactly one grant.
- **Actor:** the agent whose own agent credential was verified at exchange.
- **Authorizing user (issuer):** the user who created the grant.
- **Controller:** the agent's current owner (`store.Agent.OwnerID`) or a user in the agent's
  Hub-recorded ancestry (`store.Agent.Ancestry`), read from the store row.
- **Exchange agent credential:** the `AgentCredential` row verified at exchange.
- **Hub agent-delegation policy:** a reviewed, code-owned list of permissions that may ever be
  delegated to agents, per boundary kind, which an optional hub setting can narrow but never widen
  (§13).

## 6. Grant issuance

Route: `POST /api/v1/agents/{agentId}/delegations`. Operation `agent.delegation.create`.

Requests carry **published selectors** (`agent:read`, `project:read`, `agent:message`,
`agent:attach`; `pkg/hub/permissions/registry.go:181-194`). Stored ceilings, the policy table and
evaluation use **canonical permission IDs** (`agent.read`, `project.read`, `agent.message`,
`agent.attach`).

```json
{
  "boundary": {"kind": "hub"},
  "permissions": ["agent:read", "project:read", "agent:message"],
  "expiresAt": "2026-10-17T00:00:00Z",
  "name": "nightly-report",
  "purpose": "optional, E.1-bounded",
  "labels": {"team": "reports"},
  "maxCredentialTtlSeconds": 900
}
```

`boundary` is `{"kind": "hub"}` or `{"kind": "project", "projectId": "<id>"}`. `name`, `purpose`
and `labels` are validated exactly as for a UAT (E.1, `ValidateCredentialMetadata`): the same
length and count limits, the same character rules, and the same reserved label keys, so a label
can never name an actor. A validation failure answers 400 `validation_error` and never echoes the
value. These fields are descriptive only and never affect a decision.

Rules, applied in order. Each one fails closed. §8.4 collects every error code.

1. **Credential admission.** Only an interactive session of a local, non-federated user with a
   store user row is admitted: `requireSessionCredentialFor(w, ctx,
   authzop.ReasonCredentialManagement)` (`pkg/hub/session_only_gate.go:93`), plus an explicit check
   that the identity is an `*AuthenticatedUser` (a dev-user session is refused with 403
   `credential_not_admitted`). UATs, agent JWTs, delegated credentials, federated, broker and dev
   credentials are refused. The issuer's email must not be a reserved platform identity
   (`isReservedPlatformIdentity`, `auth.go:890`; 403 `reserved_identity`).
2. **Agent binding.** The agent exists, is not deleted, passes `agentStanding`, is not suspended,
   and has Hub-attested ancestry. The issuer is the agent's **controller** and holds
   `agent.delegation.create` on that agent through the relationship pipeline. Binding to another
   member's agent is not possible in v1 (decided by ptone, §21). The controller relationship is
   checked again at every exchange (§8 step 6). A missing or deleted agent answers 404
   `agent_not_found`. An agent that fails standing, is suspended or lacks attested ancestry answers
   409 `agent_not_eligible`. An issuer who is not the controller, or lacks the permission, answers
   403 `issuer_not_controller`.
3. **Access to the agent's project.** For every boundary kind,
   `ProjectTargetAdmission(ctx, issuerPC, agent.ProjectID, "agent.delegation.create",
   agentResource, nil)` returns `Admitted=true` with no error. Retained ancestry alone is not
   enough, and a system role that holds an unrelated permission does not qualify. The same
   check repeats at every exchange (403 `issuer_project_access`) and every use. At issuance an
   issuer who is not admitted to the agent's project is already refused at rule 2 with 403
   `issuer_not_controller`, because the owner and ancestor relationships that grant
   `agent.delegation.create` themselves require project access; this rule stays as a second
   check.
4. **Boundary.** A valid `TokenBoundary` (`Valid()`, `authz_boundary.go:58`). Boundary admission is
   mint eligibility, which stays target-free: `CanMintSelector(ctx, issuerPC, boundary,
   selectors)` must return `OK` for every selector. For a hub boundary this applies D.1's hub
   eligibility (system authority, or the permission held through an active project-scoped binding,
   or relationship eligibility). A malformed boundary (unknown kind, or `projectId` present or
   missing contrary to the kind) answers 400 `validation_error` with `details.field = "boundary"`
   and `details.reason` = `boundary_invalid` or `boundary_required`, exactly as UAT mint does
   (`writeTokenBoundaryError`, `handlers_auth.go:1036`). A well-formed project boundary naming a
   project that does not exist, or that the issuer cannot access, fails mint eligibility and
   answers the uniform 403 `forbidden` with no reason, as UAT mint does for
   `ErrUATProjectForbidden` (`handlers_auth.go:905-906`), so the response does not confirm whether
   the project exists.
5. **Ceiling.** Resolve and freeze the selectors.
   1. Every selector resolves through `ResolveSelector` (`permissions/registry.go:659`), its
      allowed boundaries include the boundary kind, and `CanMintSelector` returns `OK`. A non-OK
      result answers 403 `scope_violation` with `details.selector` and `details.reason` (the
      `MintDenialReason`: `unknown_selector`, `boundary_not_allowed`, `flat_role_insufficient` or
      `no_relationship_candidacy`), exactly as UAT mint does (`handlers_auth.go:895-904`). A
      `project_access_required` refusal never appears in a response; it collapses to the uniform
      403 `forbidden` of rule 4. This is the same eligibility function as UAT mint, so an owner can delegate `agent.attach` on their own agents through relationship
      eligibility.
   2. Every resulting permission ID is in the hub agent-delegation policy for that boundary kind,
      and is not narrowed away by the hub setting (§13). Otherwise 403
      `permission_not_delegable`.
   3. The set is not empty. Otherwise 400 `validation_error`.
   4. The ceiling is built with `BuildCeilingFromSelectors` and stored as `CeilingVersionV1`.
      Version 0 (`CeilingVersionUnspecified`) is never written for a grant, and no legacy
      implication table ever applies to a grant or delegated-credential ceiling. Manage aliases
      expand once at issuance, and `agent:attach` is never included by a manage alias
      (`ExcludeFromManageAlias`, `registry.go:185`).
   5. Non-amplification uses the same normalized frozen-ceiling rule as UAT mint (the issuer's
      eligibility for each canonical permission), not identity-type caveat intersection.
6. **Lifetime.** `expiresAt` is optional. When it is omitted, the grant expires 7 days after
   issuance. When it is given, it must be in the future and at most 30 days ahead.
   `maxCredentialTtlSeconds` is at most 3600; the default is 900 (decided by ptone, §21). A past or
   too-distant expiry, or a TTL above the maximum, answers 400 `validation_error` naming the field.
7. **No subdelegation.** `allowSubdelegation` is stored as `false`. A request that sets it, or sets
   `parentGrantId`, answers 400 `subdelegation_not_supported`.
8. **Caps.** At most 10 active grants per agent and 50 per issuer, enforced inside the
   transaction under a lock, like `LockUserForTokens`. A request past either cap answers 409
   `grant_limit_reached`.
9. **Agent row lock.** The issuance transaction calls `tx.LockAgentRows([]string{agentID})`
   (`pkg/store/store.go:355-364`; `SELECT … FOR UPDATE` on Postgres, the serialized write
   transaction on SQLite) and re-reads the agent **before** it inserts the grant. It answers 409
   `agent_reincarnating` if a reincarnation is in flight (`reincarnationInFlight`,
   `reincarnate_worker.go:49`) or if `state_version` or `Generation` differs from the values read
   at the start of the request. The grant snapshots `agent_project_id`, `agent_generation` and
   `agent_state_version`. Issuance does not bump `state_version`.
10. **Atomicity.** The grant row and its mutation audit (`agent_delegation_grant_create`) are
    written in one `WithTx`. If the audit write fails, the grant is rolled back.
11. **Output.** Grant metadata only. Issuance returns no secret. The only secret-bearing response in
    G is the exchange response.

With rule 9, the reincarnation claim and issuance can race in either order without leaving a usable
grant. If the claim commits first, issuance fails with 409. If issuance commits first, the claim's
`UPDATE` waits for the row lock, and the claim's G hook (§16.2) then revokes the new grant inside
the claim transaction.

## 7. Grant revoke, list and read

| Operation | Route | Admitted credentials | Who may act |
| --- | --- | --- | --- |
| `agent.delegation.revoke` | `DELETE /api/v1/agents/{agentId}/delegations/{grantId}` | session; the bound agent's own agent JWT | the issuer; the bound agent (relinquish its own grant); the agent's current owner; hub admins (any grant); project admins of P, for project(P)-bounded grants only |
| `agent.delegation.credential.revoke` | `DELETE /api/v1/agents/{agentId}/delegations/{grantId}/credentials/{credentialId}` | as above | as above |
| `agent.delegation.revoke_all` | `POST /api/v1/users/me/delegations/revoke-all` (the caller's own grants); `POST /api/v1/agents/{agentId}/delegations/revoke-all` | session | the issuer, for their own grants; per agent, the same rule per grant. Grants the caller may not revoke are skipped and reported, not revoked |
| `agent.delegation.list` | `GET /api/v1/agents/{agentId}/delegations`; `GET /api/v1/users/me/delegations` | session; the bound agent's own agent JWT (its own grants, metadata only) | as revoke, per grant. Grants outside the caller's authority are left out, not counted |
| `agent.delegation.read` | `GET /api/v1/agents/{agentId}/delegations/{grantId}` | as list | as list, per grant. A grant outside the caller's authority returns 404 `grant_not_found` |

- **Scope-aware control authority.** Each owner and admin path is evaluated against the grant's
  **boundary**. A project admin gains no authority over a hub-bounded grant because the agent lives
  in their project. No path is admitted merely because revocation narrows access (decided by
  ptone, §21).
- **Dev-user sessions** cannot issue (§6 rule 1). On the management routes they are admitted only
  as hub-admin control authority.
- **Bound-agent JWT paths.** When the bound agent acts with its agent JWT, the handler itself
  applies exchange steps 1-3 (§8): the credential kind is a local agent JWT, the agent credential
  row is re-loaded by JTI hash and must exist, be unrevoked and be unexpired, and the path
  `agentId` equals the subject. As at exchange, the middleware refuses a revoked row with 401
  `unauthorized` and a status lookup fault with 503 `unavailable`; in the handler, a missing row, an
  expired row or a lookup error denies with 401 `agent_credential_invalid`. G does not rely on the
  middleware alone, because the middleware still admits a JWT with no credential row (§3.1).
- **Effects.** Grant revoke marks the grant and all its credentials revoked, and writes the mutation
  audit (`agent_delegation_grant_revoke`) whose ID is stored as `revocation_audit_id`, all in one
  `WithTx`. Revoking an already revoked grant returns 204 with no new audit row. Credential revoke
  writes `agent_delegation_credential_revoke`. Revoke publishes the same authorization-change
  notification as UAT revoke (`publishConduitAuthzChanged`).
- **Never returned:** credential plaintext, `key_hash`, or any credential prefix beyond the fixed
  `scion_adt_` marker. Returned: grant ID, agent, issuer, boundary, canonical permission IDs,
  ceiling version, name, purpose and labels (all labelled as user-supplied), expiry, maximum credential TTL,
  created, last exchanged, status, revocation fields, and the count and expiry of active
  credentials.
- **Refused:** a delegated credential on any of these operations, and a UAT on any of them (grant
  management stays session-bound).
- **Denial codes:** `credential_not_admitted`, `agent_credential_invalid`, `grant_not_found`
  (missing, or not visible to the caller), `forbidden`, `audit_failed` (§8.4). These routes stay
  available while the experiment is off (§10).

## 8. Exchange

### 8.1 Request and response

Route: `POST /api/v1/agents/{agentId}/delegations/{grantId}/exchange`. Operation
`agent.delegation.exchange`.

```json
{ "audience": "scion-hub:<hub ID>", "permissions": ["agent:read"], "ttlSeconds": 600 }
```

`permissions` (selectors) and `ttlSeconds` are optional. The default is the whole grant ceiling and
the grant's maximum TTL.

Response, exactly once, with `Cache-Control: no-store`:

```json
{ "token": "scion_adt_…", "expiresAt": "…", "grantId": "…", "credentialId": "…",
  "audience": "…", "permissions": ["agent.read"] }
```

### 8.2 Checks

Authentication is the agent JWT only (`X-Scion-Agent-Token` or `Authorization: Bearer`). The
checks run in this order, and each one fails closed. External responses collapse to a small set of
codes. The precise reason goes only to the decision record.

1. The credential kind is `agent_jwt` and the principal is a local agent (`AncestryIsHubAttested`).
   Otherwise 403 `credential_not_admitted`.
2. **Re-load the agent credential row** by JTI hash. Two outcomes are decided earlier, by the
   agent-token middleware (`auth.go:270-377`), before the handler runs: a **revoked** row is refused
   with 401 `unauthorized`, and a credential-status lookup fault with 503 `unavailable`. In the
   handler, a row that is not found (a legacy token with no row), an expired row, or a handler-level
   lookup error → 401 `agent_credential_invalid`. sciontool treats **both** 401 responses
   (`unauthorized` and `agent_credential_invalid`) as "re-authenticate": the current JWT is not
   retried; it backs off (exponential, capped) and retries only after its next agent JWT refresh.
   A 503 is retried with backoff using the same JWT.
3. The path `agentId` equals the JWT subject. Otherwise 403 `credential_not_admitted`.
4. **Load the grant filtered by `agent_id = subject`.** A grant that does not exist, or is bound to
   another agent, returns the same 404 `grant_not_found`. The decision record says which applied.
   Other agents cannot observe that a grant exists.
5. **Actor state**, from the store row: the agent passes `agentStanding` (not deleted, not held,
   chain live, root user active and admitted), its phase is not suspended,
   `agent.ProjectID == grant.agent_project_id`, `agent.Generation == grant.agent_generation`, and
   no reincarnation is in flight. A mismatch or an in-flight reincarnation → 403
   `grant_agent_changed`.
6. **Controller re-check:** the issuer is still the store row's `OwnerID` or a user in its
   `Ancestry`. G never reads ancestry from the agent JWT claims. Otherwise 403
   `issuer_not_controller`.
7. **Issuer project admission:** `ProjectTargetAdmission(ctx, issuerPC, agent.ProjectID,
   "agent.delegation.create", agentResource, nil)` admits with no error. Otherwise 403
   `issuer_project_access`. An issuer who has left the agent's project cannot keep a hub grant
   alive by re-exchanging.
8. **Grant state:** not revoked, not expired, `ceiling_version` is V1 (version 0 or an unknown
   version denies), and `parent_grant_id` is null. Otherwise 403 `grant_inactive`.
9. **Issuer state:** the user row exists, is active, is a local (non-federated) user, and
   `isReservedPlatformIdentity` is false for the issuer's email. A lookup error denies. External
   code: 403 `issuer_invalid`; the decision record carries `issuer_missing`, `issuer_suspended`,
   `issuer_federated` or `reserved_identity`. A suspended local issuer does not reach this step: project admission
   (step 7) refuses an account that is not active, with 403 `issuer_project_access`. So at
   exchange, `issuer_invalid` covers a missing issuer row, a federated issuer and a reserved
   platform identity. A reserved-identity hit **denies and revokes** the grant and its
   credentials (`revoke_reason = "reserved_identity"`, mutation audit). A failed revocation write
   still denies.
10. **Policy:** every ceiling permission is still in the hub agent-delegation policy and the hub
    narrowing setting. If the policy was narrowed after issuance, the credential gets the
    intersection. An empty intersection answers 403 `permission_not_delegable`.
11. **Audience:** `audience` equals this hub's delegated-credential audience, which is
    `scion-hub:<hub ID>`, or `scion-hub` when the hub has no ID. Otherwise 400 `invalid_audience`.
12. **Subset:** the requested permissions, resolved through `ResolveSelector`, are a subset of the
    grant ceiling (after step 10). An unknown selector answers 400 `validation_error`; a permission
    outside the ceiling answers 403 `outside_ceiling`.
13. **Per-grant cap:** inside the transaction, under the grant lock, at most **2** unexpired,
    unrevoked credentials remain for the grant after the insert. The oldest beyond that are revoked
    with `revoke_reason = "superseded"`.
14. **TTL** = min(requested, `grant.max_credential_ttl`, 60 minutes, `grant.expires_at − now`,
    `ac.ExpiresAt − now`), where `ac` is the agent credential row from step 2. A delegated
    credential never outlives the agent credential it was exchanged with.
15. **Write** the credential row (hash only), the supersede revocations, `grant.last_exchanged_at`
    and the mutation audit (`agent_delegation_credential_issue`) in one `WithTx`. Roll back if the
    audit write fails. Never log the token or its hash.

### 8.3 Refresh is re-exchange

A delegated credential cannot refresh or extend itself. The agent calls exchange again with its
current agent JWT, so every refresh re-verifies the agent, the controller relationship, the issuer,
the grant and the policy. No refresh path is detached from its parents.

Issuer live authority for the delegated permissions is **not** checked at exchange, because it is
target-relative. It is checked on every use (§11.2 step 11).

### 8.4 Error codes

Every G denial uses the hub's structured error body. The table is the external contract for
issuance (§6), management (§7) and exchange (§8). The precise reason, where it is finer than the
external code, goes to the decision record (§14.2).

| HTTP | Code | Where |
| --- | --- | --- |
| 400 | `validation_error` | malformed body; malformed boundary (`details.field = boundary`, `details.reason` = `boundary_invalid` or `boundary_required`); empty ceiling; unknown selector at exchange; expiry in the past or beyond 30 days; TTL above 60 minutes; invalid name, purpose or labels (the value is never echoed) |
| 400 | `subdelegation_not_supported` | issuance sets `allowSubdelegation` or `parentGrantId` |
| 400 | `invalid_audience` | exchange audience is not this hub's |
| 401 | `unauthorized` | exchange or bound-agent management: the agent credential row is revoked (refused by the agent-token middleware before the handler) |
| 401 | `agent_credential_invalid` | exchange or bound-agent management: no agent credential row (legacy token), an expired row, or a handler-level lookup error |
| 503 | `unavailable` | exchange or bound-agent management: the middleware's credential-status lookup failed (retry with the same JWT) |
| 403 | `credential_not_admitted` | a dev session at issuance; anything but a local agent JWT at exchange (including a session super-admin); path agent differs from the JWT subject; a delegated credential on a management route |
| 403 | `forbidden` with `details.reason = CREDENTIAL_MANAGEMENT`, `details.credential = session_required` | a UAT on a session-only G route (`requireSessionCredentialFor`, `session_only_gate.go:93`) |
| 403 | `forbidden` (no reason) | a non-user identity (an agent JWT other than the bound agent's own management paths) on a session-only G route |
| 403 | `reserved_identity` | issuance by a reserved platform identity |
| 403 | `issuer_not_controller` | issuer is not the agent's owner or recorded ancestor, or lacks `agent.delegation.create` (at issuance this includes an issuer not admitted to the agent's project) |
| 403 | `issuer_project_access` | exchange: issuer not admitted to the agent's project, including a suspended local issuer (step 7 refuses an account that is not active). At issuance the same condition answers `issuer_not_controller` (§6 rules 2-3) |
| 403 | `issuer_invalid` | exchange: issuer deleted, federated or a reserved platform identity |
| 403 | `scope_violation` | issuance: `CanMintSelector` refused a selector; `details.selector` names it and `details.reason` carries the `MintDenialReason` (`unknown_selector`, `boundary_not_allowed`, `flat_role_insufficient`, `no_relationship_candidacy`) |
| 403 | `forbidden` (no reason) | issuance: a project boundary naming a project that does not exist or that the issuer cannot access (the uniform response UAT mint gives, so existence is not confirmed) |
| 403 | `permission_not_delegable` | a permission outside the hub agent-delegation policy, or an empty intersection after narrowing |
| 403 | `outside_ceiling` | exchange: requested permissions exceed the grant ceiling |
| 403 | `grant_inactive` | exchange: grant revoked, expired, or with an unsupported ceiling version |
| 403 | `grant_agent_changed` | exchange: agent project or generation changed, or a reincarnation is in flight |
| 404 | `agent_not_found` | issuance: agent missing or deleted |
| 404 | `grant_not_found` | grant missing, bound to another agent, or not visible to the caller |
| 404 | `not_found` | issuance or exchange while the experiment is off (`requireExperiment`) |
| 409 | `agent_not_eligible` | issuance: agent fails standing, is suspended or lacks attested ancestry |
| 409 | `agent_reincarnating` | issuance during a reincarnation, or after the agent row changed under the lock |
| 409 | `grant_limit_reached` | issuance past the per-agent or per-issuer cap |
| 500 | `audit_failed` | the mutation audit write failed; the change was rolled back |

At use, a delegated request is refused with the admitted handler's existing 403 `forbidden` or
404, and with 403 `credential_not_admitted` on a route outside the admission table. While the
experiment is off, the middleware refuses a `scion_adt_` credential with the same 401 as any
unusable credential.

## 9. Delegated credential format

### 9.1 Encoding and storage

- An opaque random bearer with the prefix `scion_adt_` followed by at least 256 bits from
  `crypto/rand`, base64url-encoded. Only its SHA-256 is stored. Every use needs store reads for the
  revocation chain anyway, so a self-contained JWT would add a signing-key and claim-trust surface
  for no benefit.
- `last_seen_at` is updated best-effort and asynchronously, coalesced and never in the request's
  transaction. A failure to update it never affects the decision.
- The audience is this hub's delegated-credential audience: `scion-hub:<hub ID>`, or `scion-hub`
  when the hub has no ID. It is stored on the credential row and compared with the hub's audience
  on every use, so a row copied between hub instances that share a database but have different
  audiences is refused.

### 9.2 Presentation and detection

- Presented only as `Authorization: Bearer`. `detectTokenType` (`auth.go:765`) gains a
  `scion_adt_` arm **before** `looksLikeJWT` and before the external-bearer fallback, so a delegated
  token is never offered to external-bearer validation.
- Sent in `X-Scion-Agent-Token`, it fails JWT validation and is refused as an invalid agent token
  (existing behaviour).
- **Both headers present.** The middleware evaluates `X-Scion-Agent-Token` first
  (`extractAgentToken`, `agenttoken.go:431`):
  - a valid agent JWT header plus `Bearer scion_adt_…` authenticates as the plain agent. The
    delegated token is ignored: no delegated authority and no grant in the decision record;
  - an invalid agent JWT header plus `Bearer scion_adt_…` → 401;
  - neither case produces a delegated identity. A later refactor must not merge the two.

### 9.3 Validity across agent JWT rotation

At use, a delegated credential is refused if its exchange agent credential is revoked **for any
reason**, including routine refresh rotation. Refresh revokes the presented agent credential, so
after every refresh the agent re-exchanges with its new agent JWT (§8.3). There is no rotation
exception. Because refresh revokes best-effort, a failed revoke leaves credentials exchanged with
the old agent credential valid until their own expiry (at most 60 minutes) and no longer.

## 10. Feature gate and policy setting

- **Experiment.** G registers the server-layer experiment `hub.agent_delegation` (default off,
  stage alpha) in `pkg/experiments/registry.go`. The gate covers exactly three things:
  - the issuance route and the exchange route, wrapped with `requireExperiment`
    (`pkg/hub/experiments.go:91`), which answer 404 while it is off;
  - the middleware credential arm, which checks `experimentEnabled` (`experiments.go:50`) on every
    request and refuses a `scion_adt_` credential while it is off;
  - `decideAgentDelegation`, which checks it again (defence in depth).
- **Management stays available.** Grant revoke, credential revoke, revoke-all, list and read are
  **not** gated. They answer while the experiment is off, so an operator can review and end grants
  during the disabled period.
- **Disabling suspends; it does not revoke.** Stored grants and unexpired credentials work again
  when the experiment is turned back on. Revoke and revoke-all, which work while it is off, are the
  way to end grants.
- **Narrowing setting.** An optional operational settings section `agent_delegation_policy`
  (`pkg/config/opsettings/registry.go`, database-only like `artifacts`) can remove permissions from
  the policy per boundary kind. It can never add any. Changes apply at the next exchange and on
  every use.
- Names: G's strings must not collide with existing uses of `agent_delegation`, which is already
  the agent-create mutation audit type (`agent_create_tx.go:36`) and a `CanDelegate` grant type
  (`authz_candelegate.go:44`). G's audit types use the `agent_delegation_grant_*` and
  `agent_delegation_credential_*` prefixes, its credential kind is `delegated_agent` (§12.1), and its
  experiment and setting names are as above.

## 11. Use-time evaluation

### 11.1 Authentication and routing

- A new middleware arm validates the credential and builds a `DelegatedAgentIdentity` (§12). It
  loads the credential, grant, issuer, agent and exchange agent credential rows once, builds the
  issuer principal from the issuer row (§11.5), and stores both in one request-scoped
  `delegatedRequestState` under G's private context key. It sets the identity before the credential
  context, with the same pointer (`contextWithIdentity`, `identity.go:534`;
  `contextWithCredentialContext`, `:555`), so the identity-binding check holds.
- In the audit-free `decide` body, immediately after the principal and credential derivation and
  the classification block (`authz.go:637-697`), and **before** the unsupported-principal switch and
  the bearer gate, one branch:
  - denies a mismatched pair (the delegated principal kind without the delegated credential kind,
    or the reverse);
  - sends every matched pair to `decideAgentDelegation`. No other step of the ordinary pipeline
    runs for a delegated request. That includes the agent synthetic bindings, the agent scope
    restriction, the relationship fallback and the agent delegation-ceiling step (step 10), which
    only matches an `AgentIdentity`.
- **Route admission (code-owned, deny by default).** `AuthzRequest` carries no operation
  (`authz.go:213-258`), and several handlers only require authentication. G therefore owns an
  admission table, `agentDelegationAdmittedRoutes` in a new `pkg/hub/agent_delegation_routes.go`,
  keyed by `{Method, RouteID}` and matched against the server's own routing result:
  - `routeGuard` looks the request up right after the agent sub-route resolver hook. An unlisted
    route answers 403 `credential_not_admitted` for a delegated credential.
  - The table supplies its own operation ID. An `AgentSubRoute.OperationID` that is empty (several
    project-form rows have none, `agent_routes.go:254-276`) never admits.
  - An entry names an exact route resolution, never a coarse prefix. `projects.byId` covers every
    non-agent path under `/api/v1/projects/` and the resolver does not classify those paths, so
    admitting `GET /api/v1/projects/{id}` (`project.read`, G.3-a) first needs an exact resolution
    for that one path. A coarse pattern without a resolution denies.
  - Every alias, every upgrade or stream entry point and every route classified
    `RouteAuthenticated` is either listed or denied.
  - `/events`, `/auth/*` and `/healthz` are on the web mux, outside `routeGuard` (`web.go:993-1012`).
    `/events` builds its identity only from a web session (`authorizeSSESubjects`, `web.go:2021`),
    so a `scion_adt_` bearer never becomes an event-stream identity.
  - On a match, `routeGuard` puts the table's operation in the request context. The middleware
    never sets it. `decideAgentDelegation` denies when the operation is missing or unknown,
    including on internal service paths that reach `Decide` outside an HTTP route.
- **Secondary checks.** A handler may call `Decide` again for another permission. For a delegated
  credential, that call is **evaluated** only if `(operation, permission)` is on the route's
  `Secondary` list, and the check still needs its own exact permission, ceiling and live issuer
  proof. Listing confers nothing. `agent.attach` is never a secondary entry on a non-PTY route.
- **Drift.** A two-way test pins the admission table against the catalog: every table operation
  admits the delegated credential kind in the catalog, and every catalog operation that admits it
  has a table entry. The joint real-mux suite (`TestAgentSubRoute_MuxDispatch`,
  `agent_routes_test.go:391`) gains G's cases.

### 11.2 Decision procedure

```text
decideAgentDelegation(req):                       // rows loaded once by the middleware
  c  := credential row;  g := grant(c.grant_id);  ac := agentCredential(c.exchange_agent_credential_id)
  0  op := operation from routeGuard; deny if missing or unknown; deny if experiment off
  1  c active, unexpired, c.audience == hub audience
  2  g active, unexpired, ceiling_version == V1
  3  parent chain active (v1: no parent)
  4  issuer exists, is active, is a local non-federated user, and is not a reserved platform
     identity. The revocation of the grant and its credentials on a reserved hit is written by
     the authentication middleware on the same request, before the decision; the decision
     itself only denies and stays write-free
  5  agent: agentStanding passes; phase not suspended; project == g.agent_project_id;
     Generation == g.agent_generation; no reincarnation in flight
  5a issuer ∈ {agent.OwnerID} ∪ users(agent.Ancestry)            (store row, never the JWT claim)
  5b ProjectTargetAdmission(issuerPC, agent.ProjectID, "agent.delegation.create",
                            agentResource, memo).Admitted         (any error denies)
  5c ac row loaded, ac not revoked (any reason), now < ac.ExpiresAt
  6  op admits (delegated principal, delegated credential) in the catalog  (defence in depth)
  7  p := req.Permission, or the B.2 resource-permission resolver; deny if not exactly one ID
  7a p == primary permission of op, or (op, p) is on the route's Secondary list
  8  ceiling(c).Allows(p)                                         (c.permissions ⊆ g.ceiling)
  9  p ∈ policy[g.boundary.kind] ∩ hub narrowing
  10 evidence := canonical A.1 TargetScopeEvidence for (op, target); never "missing ⇒ hub"
  11 r := EvaluateBearerCeiling(ctx, issuerPC, g.boundary, ceiling(c), p, target,
                                BearerOptions{Evidence: evidence, Memo: memo})
     deny unless r.Decision allows
  allow: MatchedGrant = "agent_delegation:" + g.id; provenance records r.AccessSource and
         r.TargetScope
  every decision: AlwaysAudit = true; DeniedBy = "agent_delegation" on deny (empty on allow);
                  the G code and attribution go in the G block on Decision (§14)
  any store or lookup error in steps 0-11 → deny (no read-only allowance)
```

`memo` is the request's `*ProjectAdmissionCache`. It is used both for G's own admission calls and
as `BearerOptions.Memo`, so there is one memo per request.

`EvaluateBearerCeiling` stage to G code (external responses collapse to 403 `forbidden`, or 404
where the handler already answers 404; the precise code goes to the decision record):

| `BearerEvaluation.Stage` | G code |
| --- | --- |
| `boundary_invalid` | `grant_boundary_invalid` |
| `target_unknown` | `target_unknown` |
| `outside_boundary` | `outside_boundary` |
| `ceiling` | `outside_ceiling` |
| `boundary_eligibility` | `outside_boundary_eligibility` |
| `project_access` | `issuer_project_access` |
| `authority` | `issuer_authority` |
| `error` | `evaluation_error` |

Steps 0-9 run before the evaluator, so its `boundary_invalid` and `ceiling` stages are defence in
depth (the boundary is validated at issuance, and the ceiling is checked in step 8). Every v1
delegable permission has a UAT selector and therefore a `PermissionAllowedBoundaries` row, so
`boundary_eligibility` is reachable only through a later registry change.

### 11.3 Permission resolution

1. `p` is `AuthzRequest.Permission` when set. Otherwise it is the B.2 table-driven resolver
   (`resolveResourcePermission`, `pkg/hub/authz_permission_resolver.go:98`), which fails closed on
   ambiguous or unknown pairs. If neither yields exactly one canonical ID, deny
   `permission_unresolved`.
2. `p` must be the operation's primary permission or on the route's `Secondary` list. Otherwise
   deny `credential_not_admitted`.
3. The same `p` and the **actual resolved target** go to `EvaluateBearerCeiling` positionally. It
   evaluates `p` as given and never re-derives it.

### 11.4 Response convention

G keeps the UAT convention. Handlers such as the agent GET load the row before authorization, so an
existing agent outside the boundary answers 403 and a missing one answers 404. `Decision.Reason` is
never written to a delegated caller's response. The admission-table drift test checks that each
admitted route's handler does not write it.

### 11.5 Issuer principal

`EvaluateBearerCeiling` requires `user.Identity` to be an `*AuthenticatedUser`
(`authz_bearer.go:361-368`). G builds it with `NewAuthenticatedUser` (`identity.go:71`) from the
**stored** issuer row, using the stored role. The `PrincipalContext` is only a function argument.
It is held under G's private context key and is never placed in the identity slot, so no handler's
`GetUserIdentityFromContext` sees the issuer as the caller. Because the evaluator does not check
account status for hub targets, step 4 checks that the issuer is active on every request.
`ProjectTargetAdmission` accepts federated users, so step 4 also refuses a federated issuer
explicitly.

### 11.6 Caching

v1 has **no cross-request cache**. The rows are loaded once per request and shared through the
request state. Revocation takes effect at the next use, and that is a tested rule. Any future
cross-request cache needs its own staleness bound and review.

### 11.7 Capabilities and environment redaction

- **Capabilities.** `ComputeCapabilities`, `ComputeCapabilitiesBatch`
  (`capabilities.go:176/263`) and `ComputeScopeCapabilities` (`:212`) gain an explicit delegated arm
  next to their early scoped-user branch. On a non-list route, capabilities contain only the route's
  primary action, for the route's resolved target, when the primary decision allowed it. Scope
  capabilities are empty. `ComputeMessageability` reports "not messageable", and the agent-create
  hint (`addAgentCreateIfAnyProjectAllows`, `handlers_agents_core.go:591`) adds nothing. None of
  these calls `Decide` or writes a record for a delegated caller. `agent.attach` is never computed
  as a capability.
- **Environment.** A delegated request never receives an unredacted agent environment, whatever its
  grant holds. Both helpers, `envViewAllowed` (`agent_env_redaction.go:96`) and `canViewAgentEnv`
  (`:68`), return false for a delegated identity. The one site that tests `ActionAttach` directly
  (`agent_sorted_project_list.go:553`) is routed through `envViewAllowed`. A static test asserts that
  no other site decides env visibility.

### 11.8 Delegated lists (G.3-L)

The list slice (`agent.list` and listing of skills and templates) lands last, in G.3-L, from D.2's
merged list shape. Until then, no list operation is in the admission table and no list permission
is in the policy, so delegated list requests are refused at the route gate. The rules G.3-L keeps:

- **Scope.** A delegated caller's list scope comes only from G's list arm: the issuer's own list
  scopes for the operation's primary permission, intersected with the grant boundary, returned as a
  tagged scope (none, an explicit project set, or all projects for a hub boundary). The agent's own
  principals contribute nothing. `ResolveListScopes` returns an error, never a scope, for a delegated
  identity, so `applyCredentialCaveats` (`authz_list.go:154`), whose default arm leaves scopes
  unreduced, is never reached for it.
- **Item filter.** Each delegable list operation declares exactly one per-item filter permission
  (`agent.list → agent.read` and the matching skill and template reads). Every item is decided
  through the same delegated procedure, including per-item `ProjectTargetAdmission` on the item's
  own project. The UAT list-row widening (`listRowReadCeiling`, `authz.go:267`) does not apply to a
  delegated ceiling: a grant without the filter permission returns no items.
- **Paging.** Totals count only returned items, and denied item IDs never appear in a response or a
  cursor. Delegated cursors are sealed (`listCursorSealer`, `authorized_list.go:531`). On the
  endpoints that do not yet seal for other callers (the hub agent list, the project agent list and
  the skill list, `authorized_list.go:469-479`), the delegated branch opens the cursor once and
  seals once, following `listGroups` (`handlers_groups.go:102-190`). An invalid cursor answers 400
  `invalid_cursor`, which clients treat as "restart from the first page". Cursors are bound by a
  delegated arm in `scopedCursorBinding` (`authorized_list.go:404`) that includes agent, grant,
  credential and boundary.
- **Audit.** One primary decision record and one aggregated item-filter record per list request.
- G.3-L confirms these against D.2's merged shape before coding. The planned store additions (a
  template project-ID filter, an all-projects skill scope, and `SkipTotalCount` in the skill store)
  are not on `main` yet.

### 11.9 What a delegated decision bounds, and what it does not

- **Authority comes only from the grant.** Each decision allows only what the issuer may do now on
  that target, reduced by the grant ceiling, the boundary, the policy and the access constraints.
- **Relationship interpretation (decided by ptone, 2026-09-30).** The agent's own relationships add
  no authority. The issuer's owner or ancestor relationship counts only inside step 11, as proof of
  the issuer's authority for a permission the issuer explicitly delegated. The actor stays the
  agent, and relationship rules are never evaluated with the agent standing in for the user.
- **The ceiling bounds decisions, not the computation that follows them.**
  - `agent.message` instructs an independently authorized recipient. The recipient acts with its own
    authority, and its downstream effects are not bounded by the messaging grant.
  - `agent.attach` gives shell access in the target agent's container. That includes the target's
    credentials and material: its agent JWT (`~/.scion/scion-token`, 10 hours, refreshable), its
    environment and files, and its own sciontool delegation socket together with any delegated
    credentials that sciontool holds. The attaching party can then act with the target's downstream
    authority, which the grant does not bound. An established session can outlive revocation and
    the credential's expiry, and revocation cannot retract material already copied. Attach is
    authorized at the WebSocket handshake and at reconnect only
    ([ptone/scion#1811](https://github.com/ptone/scion/issues/1811)). ptone included attach in v1
    on 2026-09-28 with this documentation obligation; the operator docs, the CLI help and the future
    web grant form repeat it.
- **No read-only allowance on errors.** Unlike the project delegation ceiling's read allowance
  (`ceilingReadAllowance`, `authz_delegation_ceiling.go:105`), a delegated credential denies on any
  lookup error, including for reads.
- **Durable effects.** No durable-effect permission is delegable in v1 (§13). If a later version
  makes one delegable, the recorded effect ceiling must be the grant ceiling intersected with issuer
  authority, the child records `CreatedBy` = the actor agent with its real ancestry, and provenance
  carries the authorizing user and the grant ID.

## 12. Principal model

### 12.1 Identity type and kinds

```go
// Illustrative.
type DelegatedAgentIdentity struct {
    agentID, agentProjectID string
    authorizingUserID       string
    grantID, credentialID   string
    exchangeAgentCredID     string
    boundary                TokenBoundary
    ceiling                 permissions.FrozenPermissionCeiling // the credential's subset
}
func (d *DelegatedAgentIdentity) ID() string   { return d.agentID }
func (d *DelegatedAgentIdentity) Type() string { return "agent_delegated" }
// It deliberately implements neither UserIdentity nor AgentIdentity.
```

- `PrincipalKindAgentDelegated = "agent_delegated"` in `pkg/hub`, and `PrincipalAgentDelegated` in
  `authzop`'s closed principal set.
- Credential kind `delegated_agent`: `KindDelegatedAgent` joins the closed set in
  `pkg/credentialmeta` (so audit references can carry it), `CredentialKindDelegatedAgent` aliases it
  in `pkg/hub`, and `CredentialDelegatedAgent` joins `authzop`'s closed credential set.
  `TestCatalogValidation` gains a rule: an operation that admits the delegated principal admits the
  delegated credential, and the reverse.
- `AuthTypeAgentDelegation` joins the auth-type constants (`identity.go:670-686`).

### 12.2 Explicit classification arms

| Classifier | Arm for `*DelegatedAgentIdentity` |
| --- | --- |
| `principalContextForIdentity` | `PrincipalKindAgentDelegated`, `ID = agentID`, by concrete type. A `Type()` string match alone is not accepted |
| `credentialContextForIdentity` | `CredentialKindDelegatedAgent`, `ID = credentialID`, `Boundary`, `Ceiling` (the `Boundary` doc comment stops saying "UAT only"). Never interactive, UAT or agent JWT |
| `isRecognizedPrincipalKind`, `isRecognizedCredentialKind`, `suppliedCredentialCompatible` (`authz.go:2120/2134/2172`) | recognize only the matched pair |
| `AncestryIsHubAttested` | **false**. The identity has no local-provenance marker. Its actor attestation is the exchange record, carried as provenance, not as ancestry. `identityInventoryExpectation` (`identity_classification_test.go:118`) gets a row with `attested = false` |
| Session-only gates (`requireSessionCredentialFor`, `enforceSessionCredential`) | deny |
| `GetUserIdentityFromContext`, `GetAgentIdentityFromContext` | no match |

### 12.3 Other places that need an explicit arm

Several shared helpers treat an unrecognized identity permissively, so G adds an explicit arm in
each, with a test:

- `CanDelegate` (`authz_candelegate.go:81`) and `intersectCredentialCaveats` (`:538`): a delegated
  identity is refused, like the hub delivery credential at `:91`. No v1 operation delegates
  authority onward.
- `catalogListReadBatch` (`authorized_list.go:127`): deny-all, ahead of the default arm.
- `scopedCursorBinding` (`authorized_list.go:404`): a delegated arm (§11.8).
- `authorizeAgentMessage` (`authorize_message.go:232`): a delegated arm that runs G's decision for
  `agent.message` before any ancestor or owner allow, and grants no piercing of message modes.
- `authorizeAgentTargetAction` (`authorize.go:429`): deny, because no lifecycle operation is
  delegable in v1.
- The reincarnation self-exemption (`handlers_agent_reincarnate.go:188-201`) keys on an
  `AgentIdentity` with a matching ID. A delegated identity never matches it; a test pins this.
- `adminModeMiddleware` (`admin_mode.go:100-135`): a delegated credential gets 503 during
  maintenance. It matches no admitted arm today; a test pins this.
- `IsUnscopedLocalPlatformAdmin` (`identity.go:282`): false.

## 13. Hub agent-delegation policy

- **The only source of delegability** is a reviewed per-permission list in a G-owned file,
  `pkg/hub/permissions/agent_delegation.go`: `AgentDelegableRegistry map[string][]BoundaryKind`.
  Absence means not delegable, and registering a new permission never makes it delegable.
- **Guard tests** (not policy):
  - every key exists in the permission registry;
  - no key is the base permission of a catalog operation whose effects include grant authority,
    change authority, change ownership, mint credential, issue credential, assign credential or
    create resource;
  - every key has at least one catalog operation that admits the delegated credential, and every
    such operation's permission is a key.
- **v1 set (decided by ptone, 2026-09-30).** Exact reviewed permission IDs only, with no wildcard
  or read-family expansion:

  | Permission | Boundaries | Admitted route in v1 | Lands in |
  | --- | --- | --- | --- |
  | `agent.read` | project, hub | `GET` agent (`agents.byId`, operation `agent.read`) | G.2-a |
  | `project.read` | project, hub | `GET` project (operation `project.read`) | G.3-a |
  | `agent.message` | project, hub | the HTTP agent message route, once it is catalogued (today `agent.message.send` has only a broker-call entry point and the HTTP route is pending a catalog entry) | G.3-a |
  | `agent.attach` | project, hub | the PTY WebSocket (`agents.pty`, operation `agent.attach`) only. `agent.lifecycle.exec`, `.env` and `.resetauth` share the permission but are **not** admitted | G.3-a |
  | `agent.list`; skill and template read and list | project, hub | per D.2's merged list operations (at `abc63c5`, skill and template listing is served by the `skill.read` and `template.read` operations) | G.3-L |

  A key enters `AgentDelegableRegistry` in the same change that admits its first route, so the
  guard tests always hold.
- **Hub-level targets.** A.1 now resolves hub-level targets (hub resources, global and core skills
  and templates) to the hub target scope through explicit evidence (`authz_hub_target.go`). A
  delegated request on a hub-level target is evaluated like any other: only on an admitted route,
  with canonical evidence, under a hub-bounded grant, and with the issuer's live authority. None of
  the G.2-a or G.3-a routes has a hub-level target. G.3-L adds tests for hub-catalog items.
- **Excluded from v1** (never keys):
  - `agent.create` (operation `agent.lifecycle.create` declares create-resource and grant-authority
    effects), `project.create`, agent delete.
  - `agent.lifecycle` (operation `agent.lifecycle.control` covers start, stop, suspend and restart
    together; there is no separate single-agent stop permission). Start and restart mint a fresh
    agent JWT and gather material for the target agent.
  - `schedule.event.*`: scheduled work outlives the request.
  - every `credential.*`, `user.*`, `group.*`, role, binding and policy permission; token
    management; hub settings writes; secret value reads; port access.
- Follow-up issues after G ships (decided by ptone): delegating `agent.create` and
  `project.create`, start and restart, and single-agent stop if a distinct operation is introduced.

## 14. Dual actor and user audit

### 14.1 Fields

| Field | Source | Trust |
| --- | --- | --- |
| actor kind and ID | `agent_delegated` and the agent ID from the grant, bound at exchange by a verified agent JWT | verified at exchange; the presenter of a given request is not proven |
| authorizing user ID | `grant.issuer_user_id` | server record |
| credential ID and kind | the delegated credential row ID, `delegated_agent` | server record |
| grant ID, parent grant ID | server record | server record |
| exchange agent credential ID | `AgentCredential.ID` verified at exchange | verified at exchange |
| boundary, permission | grant and credential | server record |
| grant name and purpose | issuer-supplied, E.1-bounded | **user-supplied label; never an identity** |
| correlation ID | request | — |

The G block uses the Go field names reserved in `e2a_no_g_column_test.go:43` (`ActorAgentID`,
`AuthorizingUserID`, `SourceGrantID`, `DelegationEdgeID`, `ParentGrantID`,
`ExchangeAgentCredentialID`, `ActorKind`, `AgentDelegationCode`), with label keys matching the
canonical set in `credentialmeta`. In v1 it lives in two places: on `Decision`, next to
`AlwaysAudit` and `DeniedBy`, and as persisted columns on mutation records (§18.1). G does not add
these fields to `store.DecisionAuditRecord`, `BuildDecisionAuditRecord` or the typed `auditevent`
decision schema now; that is the follow-up described in §14.4. G's block never overwrites E's
credential or principal fields.
Rendering: `actor=agent:<id> actor_binding=exchange_verified via=delegation:<grantId>`. It never
renders `verified=true`, because the binding was checked at exchange, not per request.

### 14.2 Decision records

- `decideAgentDelegation` runs inside `decide` and emits nothing itself. The single-exit `Decide`
  wrapper emits exactly one record per evaluated check, primary or secondary, allow or deny.
- Every delegated decision sets `Decision.AlwaysAudit`, so a delegated allow is never sampled away.
  Whether a record is retained still depends on the sink (§14.4).
- Every delegated deny sets `Decision.DeniedBy = "agent_delegation"` (a new `DeniedBy` constant next
  to `DeniedByDelegationCeiling`, `authz.go:418`). The fine-grained G code goes in the G block on
  `Decision` as `AgentDelegationCode` and in `Reason`, which the existing record builder already
  maps; until the follow-up, a retained decision record carries `denied_by` and the reason, not the
  G block.
- A deny from G's own issuance (§6) or exchange (§8) checks writes one decision record with the same
  `denied_by` and the code in `Reason`, built with `BuildDecisionAuditRecord`. An ordinary pipeline deny of those
  operations, before G's checks run, keeps the pipeline's own `denied_by`.
- The list-scope arm and the item filter (G.3-L) emit through `BuildDecisionAuditRecord` with the
  same flag. The item filter writes one aggregated record per list request, as a distinct record
  kind, built from a decision with `DeniedBy` unset and carrying allowed, denied and
  dropped-by-admission counts and up to 50 denied item IDs.

### 14.3 Mutation records

Mutation audit is persisted and transactional. It is G's durable attribution:

- grant create, each credential exchange (one row per issued credential), grant revoke, credential
  revoke and every supersede or cascade revocation, each in the same transaction as the change;
- every mutation made with a delegated credential, through the normal mutation-audit path, with the
  G block filled in.

Each row names the agent, the issuing user and the grant. Because credentials live at most 60
minutes and each issuance is recorded, every delegated request falls inside a recorded,
attributable credential lifetime.

### 14.4 Where decision records go

Per-decision records for delegated requests, including delegated reads, follow the platform
decision-audit sink exactly as for every other caller: its experiment flag
(`hub.authorization_decision_audit_v2`) and its domain and validation exclusions. `AlwaysAudit`
only exempts a decision from allow sampling; it does not bring a decision into the sink's recorded
domain. Many delegated decisions, for example `agent.read` on an agent inside a project, therefore
produce no decision record even when the sink is on. G does not add its own per-request database
writes.

G does not rely on decision records for attribution. The mutation records (§14.3), which are
written unconditionally, are the attribution source. Adding G actor fields to the typed
`auditevent` decision schema is a separate follow-up, coordinated with the audit workstream
([ptone/scion#2379](https://github.com/ptone/scion/issues/2379)) and approved by the maintainers,
and outside G.2-a. This design
does not change that schema.

### 14.5 Messages

A message sent with a delegated credential is stored with `Sender = "agent:<A>"` and `SenderID = A`,
plus two new nullable columns, `AuthorizingUserID` and `DelegationGrantID`. Recipients, the web UI
and the CLI render it as "from agent A, authorized by <issuer> via delegation <grantId>". A
recipient never sees it as sent by the issuer.

### 14.6 Labels

Labels cannot imitate the verified-actor block: E.1 reserves `actor`, `actor_binding`, `verified`
and the seven canonical keys. G.2-a also reserves `verified_actor`, in coordination with E.

## 15. Verified binding vs proof of possession vs labels

| Property | UAT + automation label (E) | Delegated credential (G) | Proof of possession |
| --- | --- | --- | --- |
| Principal | the human user | the agent (actor) plus the authorizing user | not provided |
| Actor identity | a label: user-supplied, unverified | verified: an agent JWT was validated at exchange, and the grant names that agent | not provided |
| Use by another holder of the credential | usable until expiry or revocation | usable until its short expiry or revocation; attribution still names the agent | would require key binding (DPoP, mTLS): out of scope |
| Revocation | token revoke; user suspend | credential, grant, issuer, agent, controller relationship, issuer project admission, exchange agent credential, policy. It stops future decisions only: an established attach session continues, and material already copied cannot be retracted | — |
| Scope of effect | the user's own actions | the decisions G evaluates. Not bounded: work a message recipient does with its own authority, and anything done inside an attach session | — |
| Authority | user live authority ∩ token ceiling | issuer live authority ∩ grant ceiling ∩ agent policy | — |

The CLI help and the docs state that a delegated credential proves which agent it was **issued
to**, not which process presents it. Any process in the agent's container that can reach the
sciontool delegation helper can use it.

## 16. Lifecycle

### 16.1 Transactional revocation through B.3 hooks

G registers one `AgentTxHook` in each of the four registries (§3.4). Each hook revokes the agent's
active grants and their credentials, with `revoke_reason` and a mutation audit, on the
transaction-scoped store. A hook error rolls back the lifecycle operation.

| Registry | Revoke reason | Why it must be transactional |
| --- | --- | --- |
| `RegisterSoftDeleteHook` (`softDeleteAgentTx`) | `agent_deleted` | restore keeps the agent ID, project and phase, so use-time checks alone would pass again |
| `RegisterHardDeleteHook` (`hardDeleteAgentTx`) | `agent_deleted` | soft-delete hooks do not run on hard delete. The hook gets the pre-delete row, so grant rows must not have a cascading foreign key that removes them first |
| `RegisterRestoreHook` (`restoreAgentTx`) | `agent_restored` | revokes any grant still active (fail closed). B.3 reactivates only its own delegation edges; G grants are never revived. The issuer issues a new grant |
| `RegisterReincarnateClaimHook` (`reincarnateClaimTx`) | `agent_reincarnated` | no grant may be usable once a reincarnation is claimed (decided by ptone, 2026-09-30) |

### 16.2 Reincarnation

- The claim is one transaction (§3.4). G's hook runs after the claim's `UPDATE`, so the row lock is
  held. If the hook fails, the claim fails and no reincarnation starts. If the reincarnation record
  insert fails, the whole claim rolls back, including G's revocation, which is correct because
  nothing was claimed.
- A role or service-account change applies after the claim: the worker writes the new applied
  configuration during provisioning and increments `Generation` only at completion
  (`reincarnate_worker.go:870`). G revokes at the claim, before any authority change.
- A worker failure after the claim does not revive grants. After a failed reincarnation, the issuer
  issues a new grant.
- While a reincarnation is in flight, exchange and use deny `grant_agent_changed`, and issuance
  answers 409 `agent_reincarnating`. The generation snapshot is defence in depth.

### 16.3 Paths without hooks

These cascades are best-effort, because use-time checks are authoritative:

- **Agent suspend** (`suspendAgent`, auto-suspend): use denies on the suspended phase. G.3 revokes
  grants best-effort.
- **Agent holds** (membership loss): `agentStanding` refuses a held agent at exchange and use. If an
  admin lifts the hold, unrevoked grants work again, as after an issuer is unsuspended.
- **Issuer suspend or delete** (`updateUser`, `deleteUser`): use and exchange deny on the issuer
  check and on project admission. G.3 revokes the issuer's grants best-effort. A user who still owns
  agents cannot be deleted (`checkUserOwnsNoAgentsTx`).
- **Issuer suspension resumes on unsuspend.** A non-revoked, unexpired grant works again, as a UAT
  does. Revoke-all is the way to end grants.
- **Project delete** (`ProjectDeletionService`): it runs no agent lifecycle hooks. G adds an
  explicit step that revokes grants and credentials of the project's agents to the project deletion
  cascade.
- **Purge of soft-deleted agents:** no hooks. Grants were already revoked at soft delete.
- **Failed-create cleanup** (`compensateAgentCreate`, `agent_create_tx.go`): no hooks. If a grant
  was issued for an agent whose create is later rolled back, the agent row is removed, so exchange
  and use deny on the missing agent (§8 step 5, §11.2 step 5). Agent IDs are never reused, so such a
  grant can never become usable again; it stays listed until it expires or is revoked.
- Grant rows tolerate the agent row disappearing in every case (no cascading foreign key, §18.1).

### 16.4 Agent credential gaps

- A JWT with no agent credential row cannot exchange (§8 step 2). Mint now records the row before
  returning a token, so only legacy tokens are affected, until the next refresh writes a row.
- If refresh fails to revoke the old agent credential, credentials exchanged with it stay valid
  until their own expiry (at most 60 minutes).

### 16.5 Multiple grants and depth

- An agent may hold several active grants, up to the cap. A credential binds to exactly one grant.
  There is no union across grants at request time, and the decision record names the grant.
- At most 2 active credentials per grant. Re-exchange supersedes the oldest beyond that.
- There is no subdelegation, so the depth is always 0. The schema carries `parent_grant_id` (null)
  and `depth` (0) so a future rule can be added without a migration. A future rule needs separate
  approval: a child grant would be strictly narrower in permissions and boundary, expire no later
  than its parent, have depth at most 3 and be cycle-free, and every parent would be checked on
  every use.

### 16.6 Attach sessions

Revoking a grant or credential denies the next attach handshake or reconnect. An established
session can outlive both the credential's expiry and revocation, so the 60-minute credential
lifetime is not a maximum session duration. A future PTY ticket mint must deny for delegated
credentials, or re-run `decideAgentDelegation` when the ticket is redeemed.

## 17. Concrete flows

### 17.1 Positive

1. **Hub read across projects (G.3-L).** Alice is a member of P1 and P2 and owns agent A in P1. She
   issues a hub grant for `project:read`, `agent:read` and `agent:list` for 7 days. A exchanges it
   with its JWT, gets a 15-minute credential, and lists agents in P2. Allowed: Alice is admitted to
   P2 and holds `agent.list` and `agent.read` there. The mutation record of the exchange names
   agent A, Alice and the grant. When `hub.authorization_decision_audit_v2` is admitted (or under
   the test sink) and the decision is inside the sink's recorded domain, the decision record also
   shows actor `agent:A`, authorizing user Alice and the
   grant.
2. **Read an agent in another project (G.2-a).** With a hub grant for `agent:read`, A reads agent B
   in P2. Allowed while Alice can read B. The environment in the response is redacted.
3. **Attach to the issuer's own descendant.** Alice issues a project(P1) grant for `agent:attach`. A
   attaches to Alice's agent C in P1, where Alice is C's ancestor. Allowed through Alice's
   relationship to C, although A has no relationship to C of its own. What A can see inside C's
   container is documented, not prevented (§11.9).
4. **Refresh.** 14 minutes later A re-exchanges and gets a new credential. The old one stays valid
   until it expires (at most 2 active). A third exchange supersedes the oldest. After a refresh of
   A's agent JWT, credentials exchanged with the old agent credential deny, and A re-exchanges.
5. **Super-admin across projects.** Super-admin S issues a hub grant for `agent:read` and
   `agent:message` to S's agent. It can read and message agents in any project while S remains
   super-admin. Messages show "from agent, authorized by S".
6. **Ordinary automation is unaffected.** With the experiment off or on, Alice's hub UAT works
   exactly as D specifies, and agent A keeps using its project JWT for its own scopes.

### 17.2 Negative (each is a test)

1. **Wrong agent:** agent B presents its own JWT to exchange A's grant → 404 `grant_not_found`,
   identical to a missing grant. The decision record (under the test sink) says "bound to
   another agent".
2. **Wrong credential at exchange:** a UAT or a user session → refused.
3. **Legacy, revoked or expired agent JWT**: a revoked row → 401 `unauthorized` from the
   middleware; no row (legacy) or an expired row → 401 `agent_credential_invalid`; a middleware
   status lookup fault → 503 `unavailable`. No credential is issued in any case.
4. **Agent suspended, held or deleted after exchange** → the next use and the next exchange deny.
5. **Issuer suspended, deleted or federated** → use and exchange deny. Issuer demoted from
   super-admin → per-target denial at the next use.
6. **Issuer loses access to P2** → use on a P2 target denies, although the grant is hub-bounded.
7. **Issuer leaves the bound agent's project but keeps ancestry** → issuance, exchange and use all
   deny, including use of a hub grant on a target in another project the issuer can still access.
8. **Issuer holds only catalog-level hub-member grants, or an unrelated system permission** →
   issuance denies, and use for a project target denies.
9. **Grant revoked or expired** → use and exchange deny. Credential TTL is clamped to the grant
   expiry and to the exchange agent credential's expiry.
10. **Permission outside the ceiling** (for example agent delete with a read grant) → deny.
11. **Permission removed from the policy or narrowed by the hub setting** → deny at the next use and
    the next exchange.
12. **Project-bounded grant used on a P2 or hub target** → deny. Unknown target scope → deny.
13. **Issuance via a UAT, a delegated credential or a dev session** → deny. Issuer not the
    controller → deny. Permission not issuer-eligible or not delegable → deny. Expiry beyond 30 days
    → deny. `allowSubdelegation` or `parentGrantId` set → deny.
14. **Delegated credential on issuance, exchange, grant management, UAT management or agent token
    refresh** → deny. A project admin of P revoking, listing or reading a hub-bounded grant → deny
    (left out of lists, 404 on read). A bound agent revoking another agent's grant → 404.
15. **Unknown audience at exchange, or a credential row whose audience differs from the hub
    audience** → deny.
16. **Ordinary agent JWT on a hub operation** → still denied. No system-scoped delegation edge exists
    after issuance.
17. **A label such as `actor=agent:A` on a UAT** → refused as a reserved key. Identity, ancestry,
    capabilities and delegation are unchanged.
18. **Store or audit failure during issuance, exchange or revoke** → the change rolls back and no
    credential is returned.
19. **Lookup error at use, including for a read** → deny.
20. **Delegated request on a route not in the admission table** → 403 `credential_not_admitted`,
    including authentication-only routes, aliases, stream entry points, path and method variants,
    `agent.lifecycle.exec`/`.env`/`.resetauth` with an attach grant, and a project-form route whose
    `OperationID` is empty. A listed secondary check whose permission is outside the ceiling or
    lacks issuer authority → deny.
21. **A durable effect** such as `agent.create` → deny: it is not a policy key, and the effect guard
    fails if it is ever added.
22. **A grant with only `agent.attach`** → stop, start and restart deny (no implication table). A
    grant row with ceiling version 0 or an unknown version → exchange and use deny.
23. **Both headers:** a valid agent JWT header plus `Bearer scion_adt_…` → the plain agent, with no
    delegated authority. An invalid agent JWT header plus `Bearer scion_adt_…` → 401.
24. **Exchange agent credential revoked** (operator revocation or refresh rotation) → the next use of
    credentials from that exchange denies. A re-exchange with the refreshed JWT succeeds.
25. **Classification:** a delegated identity fails every session gate, both interface lookups and
    `AncestryIsHubAttested`, and reaches `decideAgentDelegation` or a deny from every `Decide`
    entry. A fake type that returns `Type() == "agent_delegated"` without being the concrete type is
    refused. `CanDelegate`, `catalogListReadBatch`, the agent-create hint, the reincarnation
    self-exemption and `IsUnscopedLocalPlatformAdmin` refuse or ignore it.
26. **Ambiguous permission:** a resource and action pair that the resolver cannot map to one ID, with
    no explicit `Permission` → deny `permission_unresolved`.
27. **Issuer no longer controller at exchange** → 403 `issuer_not_controller`.
28. **Third exchange for one grant** → the oldest active credential is revoked (`superseded`) and
    denies at its next use.
29. **Environment:** with a grant that includes `agent.attach` and an issuer who could see the
    unredacted environment, every agent response is redacted. Capabilities never include attach.
30. **Message attribution:** a message sent with a delegated credential shows the agent as sender
    and the issuer as authorizer. The delegated gate runs before the ancestor and owner allows.
31. **sciontool:** after exchange, the agent's token file, the child process environment and the
    sciontool logs contain no `scion_adt_` string. The helper socket is mode 0600 and owned by the
    agent user.
32. **Soft delete whose G hook fails** → the delete rolls back. **Restore of an agent with an active
    grant** → the restore hook revokes it.
33. **Reincarnation:** use while a reincarnation is in flight denies; a completion whose generation
    write fails still denies (the grant was revoked at the claim); a claim whose G hook fails rolls
    back and does not start; issuance during an in-flight reincarnation answers 409; issuance and
    claim racing in either order leave no usable grant (Postgres lock test included).
34. **Exchange close to agent credential expiry** → the credential's `expiresAt` is no later than
    the agent credential's. Use after the agent credential's expiry denies.
35. **Reserved platform identity as issuer** → issuance, exchange and use deny. A hit at exchange or
    use also revokes; a failed revocation write still denies.
36. **One decision record per delegated check**, asserted against the test sink only: with
    `AlwaysAudit` and the G block set on the `Decision`, `denied_by` and the reason in the record,
    and E's credential and principal fields unchanged, a delegated allow is emitted even with the
    allow-sampling rate at 0. In production,
    retention also requires the sink to be admitted and the decision to be inside its recorded
    domain (§14.4).
37. **Dev session** → may revoke, list and read grants only as hub-admin control authority.
38. **Maintenance mode on** → 503 on every admitted route.
39. **Experiment off** → `scion_adt_` credentials refused, and issuance and exchange answer 404,
    while revoke, revoke-all, list and read still answer. Back on → an unexpired, unrevoked grant
    and credential work again. Revoke or revoke-all while off ends them for good.
40. **Seeded roles:** no built-in role other than super-admin and hub-admin holds
    `agent.delegation.revoke_all`; hub-member, hub-viewer, project-member and every agent role hold
    no `agent.delegation.*` permission; no role other than super-admin holds
    `agent.delegation.create` or `agent.delegation.exchange`; a session super-admin calling exchange
    → 403 `credential_not_admitted`.
41. **Project delete** → the project's agents' grants and credentials are revoked.

## 18. Data, API and operation changes

### 18.1 Schema (both stores, through ent)

```text
agent_delegation_grants
  id uuid pk
  agent_id uuid (index)   agent_project_id uuid   agent_generation int   agent_state_version int
  issuer_user_id uuid (index)
  boundary_kind enum(project, hub)   boundary_project_id uuid NULL   -- hub ⇒ NULL, project ⇒ NOT NULL
  ceiling_version int NOT NULL (>= 1)   ceiling_permission_ids text (JSON array)
  name string   purpose string NULL   labels json NULL                -- E.1 bounds
  allow_subdelegation bool default false   parent_grant_id uuid NULL   depth int default 0
  max_credential_ttl_seconds int
  expires_at time NOT NULL   created time   last_exchanged_at time NULL
  revoked_at time NULL   revoked_by string NULL   revoke_reason string NULL
  issuance_audit_id uuid NOT NULL   revocation_audit_id uuid NULL
  index(agent_id, revoked_at), index(issuer_user_id, revoked_at)

agent_delegated_credentials
  id uuid pk   grant_id uuid (index)   agent_id uuid (index)
  key_hash string unique (sensitive)   prefix string
  audience string   ceiling_permission_ids text (⊆ grant)
  exchange_agent_credential_id uuid
  issued_at   expires_at   revoked_at NULL   revoke_reason NULL   last_seen_at NULL
  index(grant_id, revoked_at, expires_at)
```

- The grant is its own table, not a system-scoped `delegation_edges` row. The one-active-edge
  partial unique index on `(delegate_type, delegate_id, scope_type, scope_id)`
  (`pkg/ent/schema/delegationedge.go:103-105`) would allow only one grant per agent and scope, the
  project-scoped ceiling walk must never see grants, and grants carry fields edges lack. The
  boundary and ceiling columns may reuse B.3's `EffectCeilingMixin`
  (`pkg/ent/schema/mixin_effect_ceiling.go:32`) if its semantics fit.
- No foreign key from grants to `agents` cascades on delete (§16.1).
- **Mutation audit G block.** `mutation_audits` (`pkg/ent/schema/mutationaudit.go`) gains nullable
  columns, all default empty, written only by G's code paths:

  ```text
  actor_agent_id                 string NULL   -- the verified actor agent
  authorizing_user_id            string NULL   -- the grant issuer
  source_grant_id                string NULL   -- the grant
  parent_grant_id                string NULL   -- always empty in v1
  delegation_edge_id             string NULL   -- reserved; empty in v1 (no edge is involved)
  exchange_agent_credential_id   string NULL   -- the agent credential verified at exchange
  actor_kind                     string NULL   -- "agent_delegated"
  agent_delegation_code          string NULL   -- the G code on a denial-related record
  ```

  The remaining §14.1 fields reuse existing columns, filled by `ApplyActor` from the delegated
  request's credential context, as E.2a does for a UAT:
  - credential ID and kind → `ActorCredentialID`, `ActorCredentialType`;
  - grant boundary → `CredentialBoundaryKind`, `CredentialBoundaryProjectID`;
  - grant name and labels → `CredentialName`, `CredentialLabels` (bounded snapshot, still
    user-supplied);
  - correlation ID → `CorrelationID`.

  Mutation records have no permission column, and G adds none. The permission is recorded in the
  decision record (`permission_id`, when retained, §14.4). The mutation record of an exchange
  carries the issued credential's canonical permission IDs in its `AfterSummary`, and the grant
  create record carries the frozen ceiling there, so the durable trail always names the delegated
  permissions. `store.MutationAuditRecord` (`pkg/store/models.go:3817`) gets the
  matching Go fields with the names reserved in `e2a_no_g_column_test.go:43` (`ActorAgentID`,
  `AuthorizingUserID`, `SourceGrantID`, `ParentGrantID`, `DelegationEdgeID`,
  `ExchangeAgentCredentialID`, `ActorKind`, `AgentDelegationCode`), and
  `entadapter/mutation_audit_store.go` persists and reads them. `AuditActor` and `ApplyActor`
  (`pkg/hub/audit_actor.go`) carry the G block from the delegated request state into every mutation
  record, never overwriting E's fields.
- **Decision side.** G adds its block to `Decision` (`authz.go`, next to `AlwaysAudit` and
  `DeniedBy`) only. `store.DecisionAuditRecord` (`models.go:3759`), `BuildDecisionAuditRecord`
  (`audit_authz.go:78`) and the typed `auditevent` decision schema are unchanged; adding G fields
  to them is the separate, maintainer-approved follow-up of §14.4.
- `e2a_no_g_column_test.go` is updated in the same change for the mutation-audit fields only: on
  `MutationAuditRecord` the reserved Go names become real fields, and its E-writer tests keep
  asserting that E's paths leave them empty. The decision-record names stay reserved for the
  follow-up.
- Migration: purely additive tables and nullable columns through `AutoMigrate`. No backfill. The
  message model (`store.Message`, `models.go:2396`; `pkg/ent/schema/message.go`) gains two
  nullable columns (§14.5).
- Purge: expired delegated credentials are purged by a new periodic job. `PurgeExpiredAgentCredentials`
  has no production caller to extend. Grants are kept for audit.

### 18.2 Store

A new `AgentDelegationStore` interface in `pkg/store/store.go`, embedded in `Store`, implemented in
a new `pkg/store/entadapter/agent_delegation_store.go`: create, get and list grants; lock for caps;
create, get-by-hash and revoke credentials; revoke by grant, agent, issuer and project; update
last-exchanged. The new create and revoke methods are added to `SecurityMutationSymbols`
(`pkg/hub/authzop/catalog.go:70`) with classifications. That scanner checks only listed symbols, so
listing them is a review obligation.

### 18.3 Operations

New catalog specs in a G-owned `pkg/hub/authzop/catalog_agent_delegation.go`:

| Operation | Credentials | `Bearer` | Effects | Notes |
| --- | --- | --- | --- | --- |
| `agent.delegation.create` | `session_jwt` | `SessionOnly(ReasonCredentialManagement)` | issue credential | governance and atomic audit |
| `agent.delegation.exchange` | `agent_jwt` | `NonUser()` with a `Pin` naming G's UAT and session refusal test | mint credential | the bearer matrix skips non-user operations, so G pins its own test |
| `agent.delegation.revoke`, `.credential.revoke`, `.list`, `.read` | `session_jwt`, `agent_jwt` | `SessionOnly(ReasonCredentialManagement)` | revoke authority (revokes); read (list, read) | the handler takes the bound-agent branch first and then requires a session, so a UAT gets 403 with `details.reason = CREDENTIAL_MANAGEMENT` and `details.credential = session_required` |
| `agent.delegation.revoke_all` | `session_jwt` | `SessionOnly(ReasonCredentialManagement)` | revoke authority | |

No G operation lists `scoped_uat`. Operations admitted for delegated use (§13) add
`PrincipalAgentDelegated` and `CredentialDelegatedAgent` as they are admitted.

### 18.4 Routes

Each new route needs:

- the mux registration in `server.go` (agent-scoped routes fall under the existing `/api/v1/agents/`
  prefix; the `/api/v1/users/me/delegations` routes need their own patterns);
- `routeMetadataTable` entries (`route_metadata.go`; a missing pattern fails closed with 500) and
  `route_authz_manifest.go` entries;
- rows in `agentSubRouteTable` (`agent_routes.go:195`) naming the operation, for the routes under
  `/api/v1/agents/{agentId}/delegations`;
- a catalog entry point, so `TestBearerDisposition_EveryRoutePatternCovered` passes;
- `TestCatalogHTTPEntryPoints_LiveMethodCheck` fixtures: a seeded grant and credential in
  `idFixtures`, entries in `patternOverrides` and, for the destructive verbs, `opPatternOverrides`
  with disposable twins, and `bodyOverrides` for create and exchange. The handlers answer 405 for an
  unknown method and 404 for an extra segment, so no exclusion is expected. The live inventory test
  server turns the experiment on.

### 18.5 Permissions and seeded roles

- Registry rows for `agent.delegation.create`, `.read`, `.list`, `.revoke`, `.credential.revoke`,
  `.revoke_all` and `.exchange`, with **no** `UATScope`, so none is token-selectable and the
  selector pin (`ValidateSelectorRegistry`) is unchanged.
- Rows in `ProjectTargetApplicability` (true) and `CollectionTargetClasses` for every new
  permission. **No** `PermissionAllowedBoundaries` rows: that table is only for permissions with a
  UAT selector, and its stale-key test fails on any other row.
- A `RelationshipPolicies` row (`permissions/relationship_policy.go`) for `agent.delegation.create`
  (owner and ancestor, the agent's own agents, not mint-eligible), merged into the existing owner row
  where the two-row rule requires it, with the characterization allowlist updated.
- Seeded roles: super-admin gets every permission automatically. hub-admin gets revoke,
  credential revoke, revoke-all, list and read. project-owner and project-admin get revoke,
  credential revoke, list and read, enforced per grant for project-bounded grants only. No other
  role gets any `agent.delegation.*` permission. Each changed role bumps its `Revision`
  (`reconcileBuiltInRoles`, `seed.go:625`). Exchange authenticates an agent JWT and never consults
  role bindings, so super-admin's automatic copy of `agent.delegation.exchange` grants nothing.

### 18.6 SDK

A new `pkg/hubclient/agent_delegations.go`: create, list, get and revoke grants (user), revoke-all,
and exchange (agent).

### 18.7 Agent side (sciontool)

- `ExchangeDelegation(ctx, grantID, audience)` next to `RefreshToken` (`pkg/sciontool/hub/client.go:744`)
  and a new `pkg/sciontool/delegation/` helper.
- sciontool holds the delegated token in memory only and serves it to processes in the agent
  container through a local unix socket (mode 0600, owned by the agent user). The preferred form is a
  proxy that attaches the header to a hub request, so the token never leaves sciontool. It is never
  written to the token file, a config file or a child process's environment.
- sciontool re-exchanges before expiry and after every agent JWT refresh. It treats both 401
  responses from exchange (`unauthorized` and `agent_credential_invalid`) as "re-authenticate" and
  a 503 as retryable, as in §8 step 2. Any same-UID process in the container that can open the
  socket can use the delegated authority; this is not proof of possession.

### 18.8 CLI

`scion hub delegation create | list | get | revoke` in a new `cmd/hub_delegation.go`. The command is
**user mode only** (decided by ptone, 2026-09-30): add `"hub.delegation": true` to `assistantDenied`
in `cmd/cli_mode.go`, and do not add it to `agentAllowed`. The `scion` CLI has no v1 consumption of
delegated credentials; agent-side exchange lives in sciontool only. `create --help` repeats the
attach consequences when `agent:attach` is selected.

### 18.9 Web

Deferred to [ptone/scion#2438](https://github.com/ptone/scion/issues/2438). When it is built, the
grant form repeats the attach consequences, and the copy distinguishes "Agent delegation (verified
agent)" from "Access token (acts as you; label is descriptive)".

### 18.10 Shared files

G adds new files and keeps hooks in shared files small:

| File | G's change | Driven by |
| --- | --- | --- |
| `pkg/hub/authz.go` | kind constants; explicit classification arms; the routing branch in `decide`; the `DeniedBy` constant | §11.1, §12.1, §12.2, §14.2 |
| `pkg/hub/auth.go` | one `detectTokenType` arm and one middleware arm; G's sites in the `isReservedPlatformIdentity` covered-sites comment (`auth.go:854-890`) | §9.2, §11.1 |
| `pkg/hub/identity.go`, `pkg/credentialmeta/metadata.go` (+ test) | the auth type; the credential kind in the closed set; `verified_actor` reserved with E | §12.1, §14.6 |
| `pkg/hub/route_metadata.go`, `agent_routes.go`, `route_authz_manifest.go`, `server.go` | route registrations; the admission lookup after the resolver hook; registration of G's four lifecycle hooks; the credential purge job | §11.1, §16.1, §18.1, §18.4 |
| `pkg/hub/authz_candelegate.go`, `authorized_list.go`, `authz_list.go`, `capabilities.go`, `authorize.go`, `handlers_agents_core.go` (`addAgentCreateIfAnyProjectAllows`) | the explicit arms | §11.7, §12.3 |
| `pkg/hub/authorize_message.go`, `handlers_agent_messaging.go` | the delegated message gate; the sender attribution columns written on send | §12.3, §14.5 |
| `pkg/hub/agent_env_redaction.go`, `agent_sorted_project_list.go` | both env helpers return false; the direct `ActionAttach` site at `:553` routed through `envViewAllowed` | §11.7 |
| `pkg/hub/handlers_agent_lifecycle.go` | best-effort grant revocation on agent suspend | §16.3 |
| `pkg/hub/handlers_users_core.go` | best-effort grant revocation on issuer suspend and delete | §16.3 |
| `pkg/hub/project_deletion_service.go`, `pkg/store/entadapter/project_store.go` | the explicit grant and credential revocation step in the project deletion cascade | §16.3 |
| `pkg/hub/audit_actor.go` | the G block in `AuditActor`/`ApplyActor` for mutation records | §14, §18.1 |
| `pkg/store/models.go`, `pkg/store/store.go` | the two grant and credential models and the `AgentDelegationStore` interface; G fields on `MutationAuditRecord`; the two message columns | §14.5, §18.1, §18.2 |
| `pkg/ent/schema/mutationaudit.go`, `pkg/ent/schema/message.go` (+ regenerated `pkg/ent/**`) | nullable G columns; nullable message columns | §14.5, §18.1 |
| `pkg/store/entadapter/mutation_audit_store.go`, `message` store, `composite.go` | persist and read the new columns; embed the new store | §18.1, §18.2 |
| `pkg/hub/authzop/operation.go`, `catalog.go` | closed-set constants; mutation symbols | §12.1, §18.2 |
| `pkg/hub/permissions/registry.go`, `project_applicability.go`, `collection_target_classes.go`, `relationship_policy.go`; `pkg/hub/seed.go` | permission, applicability and policy rows; seeded roles | §18.5 |
| `pkg/experiments/registry.go`, `pkg/config/opsettings/registry.go` | the experiment and the narrowing section | §10 |
| `pkg/hub/e2a_no_g_column_test.go` | updated for the mutation-audit G fields | §18.1 |
| `pkg/hubclient/` (new `agent_delegations.go`) | SDK methods | §18.6 |
| `pkg/sciontool/hub/client.go`, new `pkg/sciontool/delegation/` | exchange, re-exchange and the socket helper | §18.7 |
| `cmd/hub_delegation.go` (new), `cmd/cli_mode.go`; CLI message rendering | the command and its mode entry; "authorized by" rendering | §14.5, §18.8 |
| `web/src/components/shared/agent-message-viewer.ts`, `web/src/components/shared/chat/chat-message.ts` | "authorized by" rendering of delegated messages (the grant management UI stays deferred) | §14.5 |
| tests: `identity_classification_test.go`, `authzop_catalog_method_inventory_test.go`, `agent_routes_test.go`, `bearer_disposition_*_test.go`, `cmd/cli_mode_test.go` | new rows and fixtures | §12.2, §18.3, §18.4, §18.8 |

New G files: `agent_delegation.go` (service), `handlers_agent_delegation.go`,
`identity_delegated.go`, `authz_agent_delegation.go` (`decideAgentDelegation`),
`agent_delegation_routes.go`, `permissions/agent_delegation.go`,
`authzop/catalog_agent_delegation.go`, the two ent schemas and the store adapter.

## 19. Phases

G.1 (this document) needs nothing further. G.2 and the non-list G.3 slices build on what is merged
at `abc63c5`: A.1, A.2, B.2, the B.3 lifecycle hooks, D.1 and E.2a. G.3-L also needs D.2's merged
list operations.

1. **G.2-a: vertical slice (one PR).** Schema and store (both dialects), issuance, exchange, the
   middleware arm, the classification and permissive-helper arms (§12), and a decision procedure
   that admits **one** route, the agent `GET` (`agent.read`), behind the experiment. It includes the
   admission table with one entry, env redaction at every site and primary-only capabilities.
   **Stop for review.** Fan-out depends on this slice passing.
2. **G.2-b.** Revoke, list and read operations (§7), the caps, the credential purge job, and catalog,
   scanner and applicability completeness.
3. **G.3-a.** The full decision procedure and admission for `project.read`, `agent.message` (once
   the HTTP message route is catalogued) and `agent.attach` (PTY handshake and reconnect), with
   drift tests.
4. **G.3-b.** The lifecycle hooks and cascades (§16), re-exchange after refresh, the sciontool
   exchange and socket helper, message sender attribution.
5. **G.3-c.** SDK, CLI, revoke-all, operator docs, end-to-end scenarios.
6. **G.3-L.** The delegated list slice (§11.8), starting with `agent.list` on the hub agent list as
   one slice, then the project agent list, skills and templates.

Later work, outside these phases: G actor fields in the typed decision-audit schema, a separate
follow-up, coordinated with the audit workstream and approved by the maintainers (§14.4).

## 20. Acceptance criteria

1. **Ordinary bearer automation is unchanged.** With the experiment off, no issuance or exchange
   route answers and every `scion_adt_` credential is refused; only the grant management routes
   answer, so existing grants can be reviewed and revoked. With it on, the existing UAT, agent JWT, `CanDelegate`,
   cross-member attach and delegation-ceiling suites pass unchanged, and an ordinary agent JWT gains
   no hub authority (golden test).
2. Every flow in §17 has a behaviour-level test through real handlers or the real middleware.
3. Only a `scion_adt_` credential produces a delegated identity. No label or header sets a verified
   actor.
4. The plaintext credential appears once, is hash-only at rest, and never appears in logs.
5. A store or audit failure rolls back issuance, exchange and revoke on both stores.
6. Every issuance, exchange, revocation and delegated mutation has a mutation record carrying the
   actor agent, the authorizing user, the grant and the credential (unconditional). When
   `hub.authorization_decision_audit_v2` is admitted, or under the test sink, and the decision is
   inside the sink's recorded domain, decision records agree with those mutation records on actor, authorizing user, grant, credential, boundary and
   correlation ID.
7. A delegated identity is never classified as an interactive, UAT or agent JWT credential, never
   counts as hub-attested ancestry, and is refused or ignored by every helper in §12.3.
8. Every delegable permission is a key of the reviewed policy and has an admitting catalog
   operation. No v1 key has a durable or credential effect.
9. A delegated credential is admitted only on routes in the admission table, matched on the single
   server-owned route resolution that also drives dispatch, tested through the real mux, guard and
   dispatcher.
10. A delegated request never receives an unredacted agent environment.
11. Messages sent with a delegated credential show the agent as sender and the issuer as authorizer.
12. The operator docs state that the ceiling bounds decisions, not resulting computation, and list
    the attach consequences of §11.9.
13. Registry, catalog, applicability tables, routes, bearer dispositions and client surfaces agree
    (drift and coverage tests). The CLI command has its recorded mode decision.

## 21. Decision record

### 21.1 Decisions by ptone

| Date | Decision |
| --- | --- |
| 2026-09-28 | `agent.attach` is in the v1 delegable set, with documentation of its consequences (§11.9) |
| 2026-09-30 | The agent's own relationships add no authority; the issuer's relationship is only the ceiling proof for an explicitly delegated permission (§11.9) |
| 2026-09-30 | The issuer is the agent's active owner or Hub-recorded ancestor user, checked again at every exchange (§6, §8) |
| 2026-09-30 | Grant lifetime default 7 days, maximum 30 days; credential lifetime default 15 minutes, maximum 60 minutes; at most 2 active credentials per grant (§6, §8) |
| 2026-09-30 | v1 delegable set: exact read and list permissions, `agent.message`, `agent.attach`. `agent.create`, `project.create`, start and restart, and single-agent stop are follow-up issues (§13) |
| 2026-09-30 | Revoke: the issuer and the bound agent may relinquish; the current owner and hub admins may revoke any grant; project admins of P only project(P)-bounded grants (§7) |
| 2026-09-30 | Grants are revoked in the reincarnation claim transaction (§16.2) |
| 2026-09-30 | `scion hub delegation` is user mode only. A web UI is wanted later (§18.8, §18.9) |

### 21.2 Design rulings (2026-09-28)

- A separate grant table, not system-scoped delegation edges (§18.1).
- A code-owned, deny-by-default route admission table, bound to one canonical sub-route resolution
  (§11.1).
- One declared per-item filter permission per delegable list operation (§11.8).
- The exchange agent credential is part of the revocation chain, with no rotation exception
  (§9.3).
- Attach follows the handshake and reconnect contract of ptone/scion#1811 (§16.6).

### 21.3 Adapted to merged code (2026-10-10)

- Decision records follow the platform decision-audit path, including its flag and its domain and
  validation exclusions (§14.4). Mutation audit is the attribution source. A later option, not in v1, would let delegation be enabled only while the decision-audit
  sink is admitted and healthy.
- The feature gate is a registered experiment, not a hub setting (§10).
- The credential kind is `delegated_agent`, as `user-access-tokens.md` already names it, and joins
  the closed set in `pkg/credentialmeta` (§12.1).
- Issuer evaluation builds an `*AuthenticatedUser` from the stored row and checks issuer status and
  federation explicitly (§11.5). One admission memo serves both G and the bearer evaluator.
- The evaluator's `boundary_eligibility` stage has its own G code (§11.2).
- Actor checks use `agentStanding`, which includes agent holds (§8, §11.2).
- At exchange and on the bound-agent management paths, a revoked agent credential is refused by
  the agent-token middleware with 401 `unauthorized`, and a status lookup fault with 503
  `unavailable`. 401 `agent_credential_invalid` is kept for a legacy token with no row, an expired
  row and a handler-level lookup error. Clients treat both 401 codes as "re-authenticate" (§8.2
  step 2, §8.4).
- At use, the reserved-identity revocation write happens in the authentication middleware, before
  the decision; `decideAgentDelegation` still denies a reserved issuer and writes nothing (§11.2
  step 4).
- The reincarnation claim has no separate revert path any more; a failed record insert rolls back the
  whole claim (§16.2). Project delete needs an explicit G revocation step (§16.3).
- Each G operation declares a bearer disposition (§18.3). G permissions get no
  `PermissionAllowedBoundaries` rows and do get `CollectionTargetClasses` rows (§18.5).
- Several shared helpers need explicit arms for a new identity type (§12.3).
- Single-agent stop exists only inside `agent.lifecycle.control`, so it stays excluded (§13).

## 22. Alternatives considered

- **Reuse the `agent` principal kind, with the credential kind as discriminator — rejected.** The
  identity would satisfy agent shortcuts such as the lifecycle helper's agent branch and the
  reincarnation self-exemption.
- **A signed JWT with grant and actor claims — rejected.** Every use reads the store for the
  revocation chain anyway, and a JWT adds a signing-key and claim-trust surface.
- **Exchange at `/api/v1/auth/agent-delegation/exchange` — rejected.** The agent-scoped route fits
  the agent sub-route table and its self-access checks.
- **Issuance from an existing UAT — rejected.** It would put a human bearer in the agent path and
  couple UAT and grant revocation.
- **Auto-revoke grants on issuer suspension — not adopted.** Suspension blocks use and unsuspension
  resumes it, as for UATs. Revoke-all ends grants.
- **sciontool writing the token to a file or a child environment — rejected.** Both put the bearer
  where many processes can read it.
- **Pre-narrowing all-projects delegated lists to the issuer's member projects — deferred** until
  truncation at the candidate cap is observed. Per-item admission stays the authority either way.
