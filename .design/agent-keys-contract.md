# Agent keys contract — frozen wire, authorization and cutover semantics

Status: **FROZEN** for task 0.1 (ptone/scion#2191, master design ptone/scion#2184, phase 2185).
Every later agent-keys task (1.1, 1.2, 2.1, 2.2, 2.3, 3.1, 3.2, 4.1, 4.2, 4.3; phase 5 deferred)
implements against this document for the wire/type-level detail. Per #2184's own rule ("An
implementation owner records any changed decision here before changing dependent contracts"), the
master issue body remains the canonical record of decisions and deviations: §11 below carries the
exact text delivered to the master body for the lead to apply, and this file updates in lockstep
with it rather than in its place.

This task makes **no production behaviour change**. It freezes the contract, adds
compile-only Go types (`pkg/agentkeys`, `api.AgentActionKeys`, an explicit
`agentActionPermission` mapping), and updates design docs. Nothing here is reachable at
runtime yet — see "Go contract types: placement" below for exactly what was and was not wired.

Tested revision: this branch, based on `origin/main` at `6585a8b` ("fix(hub): honor top-level
raw and plain flags on POST /api/v1/agents/{id}/message (#2053)").

## 1. Decision record

The five decisions #2184 recommended for review are **ADOPTED** as written. Restated here for
task-local reference, with the code anchors that ground each one on the current `main`:

1. **Dedicated broker keys route (Option B).** A new broker route (`POST
   /api/v1/agents/{id}/keys`, its own handler) replaces the plan of routing keys indefinitely
   through the message transport. It does not reuse `sendMessage`
   (`pkg/runtimebroker/handlers.go:2115`) or its `mgr.MessageRaw` call
   (`pkg/runtimebroker/handlers.go:2157`), and it does not go through
   `RuntimeBrokerClient.MessageAgent` / `AgentDispatcher.DispatchAgentMessage`
   (`pkg/hub/server.go`). This removes the message logger from the keys path entirely, rather
   than trying to keep redacting it forever.
2. **`api.AgentActionKeys` + one `authorizeAgentKeys` entry point, mapped to the existing attach
   permission.** The route action is not a new independently granted authorization permission.
   `api.AgentActionKeys = "keys"` is added to `pkg/api/agent_actions.go`; `agentActionPermission`
   in `pkg/hub/authorize.go` gets an **explicit** case mapping it to `ActionAttach`, so the
   mapping is a visible decision rather than an accident of that function's `default: return
   ActionAttach` branch. `authorizeAgentKeys` itself (task 2.1) does not exist yet.
3. **Content-free audit, no conversation persistence.** As soon as the Hub bridge ships (task
   2.3), no accepted raw/keys request is written to `store.Message` /
   `pkg/hub/agent_dm_operation.go`'s persistence step. Existing historical rows are not rewritten.
4. **Short-lived message-API bridge + hidden `message --raw` alias, then removal.** Both are
   temporary (Phase 2/3 through Phase 4), owned from day one, removed on evidence — not on a
   calendar.
5. **`/keys` is the documented REST API; Plain is untouched.** No new permission family, no quick
   keys UI, no new SSE event, no key-sequence language, no scheduled keys, no managed-runtime
   keys, in this design.

Publishing this contract is not prior user approval and does not authorize deployment; per
#2191's scope guard, no code beyond the compile-only types below, no CI beyond the focused
commands in "Verification", and no agent dispatch is authorized by this task alone.

## 2. Public request/result/error contract

### 2.1 Route shapes

Both accept the identical body and produce the identical result/error shape:

- `POST /api/v1/agents/{id}/keys` — `{id}` is the Hub's canonical agent ID, resolved the same
  way the existing top-level `/api/v1/agents/{id}/message` route resolves it.
- `POST /api/v1/projects/{project}/agents/{id-or-slug}/keys` — `{project}` accepts exactly what
  the route's existing project resolver accepts today: a canonical project UUID, or the hosted
  `{uuid}__{slug}` form (`resolveProjectID`, `pkg/hub/handlers_projects_core.go:2523`-2529, which
  extracts the UUID from that form and otherwise passes the input through unchanged; `GetProject`'s
  own lookup, `pkg/store/entadapter/project_store.go:197`-199 via `parseGetID`, treats anything that
  isn't a valid UUID as not-found). **There is no bare-slug form for `{project}`** — a project slug
  alone always 404s here, the same as any other unresolvable value (§3.1, AK-21e). `{id-or-slug}`
  for the agent uses the route's existing agent resolver and verifies target membership in
  `{project}`.

Both routes today share one handler, `(*Server).handleAgentMessage`
(`pkg/hub/handlers_agent_messaging.go:1425`), reached via `handlers_agents_core.go:3046` and
`handlers_projects_core.go:2488`. Keys must **not** be a branch inside that handler or inside
`MessageRequest` (`handlers_agent_messaging.go:1388`): it is a new, dedicated handler reached by a
new action dispatched before any message-specific code runs (see decision 1). Authentication
supplies the actor; request JSON may never override actor, project authority, target, session or
tmux options — the same rule `handleAgentMessage` already applies to `Sender`/`SenderID`
(`handlers_agent_messaging.go:1449`-1476, the "B5 SECURITY FIX" block) extends to keys with no
exception.

### 2.2 Request

```json
{ "keys": "C-c" }
```

Frozen as `agentkeys.Request` (`pkg/agentkeys/types.go`). Validation (`agentkeys.ValidateBody`,
`pkg/agentkeys/validate.go`):

| Rule | Behaviour |
| --- | --- |
| Unknown top-level field | Rejected (400 `invalid_request`) |
| Duplicate `"keys"` field | Rejected (400) — `encoding/json` would otherwise silently keep the last occurrence; validation reads the token stream so it does not inherit that leniency |
| `keys` is an array, object, number, bool, or null | Rejected (400) |
| `keys` missing | Rejected (400) |
| `keys` is `""` | Rejected (400) — **empty is invalid input, not a synonym for Enter** |
| `keys` contains a NUL byte | Rejected (400) |
| `keys` exceeds `agentkeys.MaxBytes` (4096) UTF-8 bytes | Rejected (413 `payload_too_large`) |
| `keys` is whitespace-only (e.g. `"   "`) | **Valid.** Preserved verbatim |
| `keys` contains leading/trailing/internal spaces | Preserved verbatim — never trimmed, split, or tokenized |
| `keys` is UTF-8 text (accents, CJK, emoji) | Preserved verbatim; the byte ceiling is on UTF-8 bytes, not runes |
| `keys` is literal text containing `@name` | Text, not mention syntax — never parsed for recipients |

The HTTP body itself is also bounded (`agentkeys.MaxHTTPBodyBytes`, 32 KiB — sized so no
*compactly encoded* (no insignificant whitespace) ≤4096-byte request is ever rejected by the
body-size check even under worst-case JSON `\u00XX` escaping, while bounding allocation for a
clearly-oversized/malicious body before it is parsed).

### 2.3 Tmux semantics — exact, not "atomic sequences"

This is the single most important thing every dependent task must implement identically, because
`cmd/keys.go`'s own help text (`Long`, `keysCmd`, lines 34-48) currently misleads about it:

> **One request string becomes exactly one `tmux send-keys` argument.** The dedicated broker
> handler (task 1.1) delivers `keys` to tmux as a single `send-keys -t <target> -- <keys>`
> command — see "Correction recorded during task 1.1 implementation" below for the exact delivery
> mechanism. tmux recognizes a small set of named keys (`Enter`, `Escape`, `C-c`, arrow names, etc.)
> **only when the entire argument matches one name exactly**; any other string — including one
> containing spaces — is typed as literal text, character by character, including the spaces.
>
> **Correction recorded during task 1.1 implementation:** `keys` is not delivered as a process
> argument. The broker handler (`agent.Manager.SendKeys`, superseding `MessageRaw` per decision
> below) supplies tmux with a single generated `send-keys -t <target> -- "<keys>"` command over
> stdin, via `Runtime.ExecWithStdin` with the fixed argv `tmux source-file -`, with every byte of
> `keys` encoded as a three-digit octal escape (`\ddd`) inside the double-quoted argument. tmux's
> own command-argument parser reverses this encoding back to the exact original bytes before
> evaluating the argument, so the semantic invariant stated above — one string becomes one
> argument, a named key is recognized only on an exact whole-argument match, no shell evaluation,
> no whitespace tokenization, no arbitrary tmux flags — is unchanged from a direct invocation.
>
> **Transport requirements**, binding on this route and on any local-mode equivalent:
>
> - No payload, and no reversible encoding of it, in a host or guest process's arguments,
>   environment variables, temporary files, or a command-bearing URL.
> - No load-buffer/paste-buffer substitution.
> - No helper that reads the stdin payload and then rebuilds a payload-bearing command line.
> - stderr and tracing of the delivery call suppressed at the source.
> - tmux at or above version 3.1.
> - A backend unable to meet these requirements returns `keys_unsupported` before any injection.
>
> **Deviation from #2184 recorded here:** `cmd/keys.go`'s help text says `scion keys my-agent
> "Up Up Enter"` and calls it usable for "interactive TUI applications", implying three key
> presses. On current `main` this is **not true**: `cmd/keys.go:60` joins CLI args with `" "`
> into one string, which becomes one argv element, which tmux does not recognize as a named key,
> so it is typed as the 11 literal characters `U`,`p`,` `,`U`,`p`,` `,`E`,`n`,`t`,`e`,`r`. This
> matches #2184's own finding ("do not promise that the current example is a sequence") but goes
> one step further: the current help text is actively wrong about what the example does, not
> merely silent about sequencing. Task 3.2 (docs/CLI help) must correct this text as part of its
> scope; task 3.1 must not build client-side "convenience" splitting on spaces to make the
> example true, because that would silently reinterpret literal text as tokens for every other
> caller. **Multiple keys require separate `/keys` calls.** No shell evaluation, no variable
> expansion, no whitespace tokenization, no arbitrary tmux flags, and no atomic multi-key sequence
> is promised by this contract. `Enter` explicitly submits (it is a recognized tmux key name); an
> empty string is rejected before dispatch and never silently becomes `Enter` (§2.2).

### 2.4 Result

```json
{ "status": "dispatched", "operation_id": "<uuid>", "agent_id": "<uuid>" }
```

Frozen as `agentkeys.Response` / `agentkeys.StatusDispatched` (`pkg/agentkeys/types.go`). HTTP 200
means the broker acknowledged successful terminal injection, **not** that the harness consumed or
obeyed it. `operation_id` correlates audit and error records; it is **not** an idempotency key and
not a durable status resource. There is no 202/queued state.

### 2.4a Error response

Errors reuse the existing, already-universal pkg/hub envelope as-is — no new Go type is defined for
it — `pkg/hub.ErrorResponse{Error: pkg/hub.APIError{Code, Message, Details, RequestID}}`
(`pkg/hub/errors.go:31`-41), written by the existing `writeError` helper
(`pkg/hub/errors.go:217`) and already parsed client-side by
`pkg/apiclient.ParseErrorResponse`/`APIError` (`pkg/apiclient/errors.go:26`-135). This is the exact
shape every other Hub route already produces, including the 401 the shared auth middleware writes
before a keys handler ever runs (§2.5's OperationID policy):

```json
{ "error": { "code": "keys_denied", "message": "<sanitized>", "details": { "operation_id": "<uuid>" } } }
```

`code` is the `Outcome` string. `details` is the existing `map[string]interface{}`
(`pkg/hub.APIError.Details`); handlers set `details["operation_id"]` only when an operation ID
exists for this outcome (§2.5's OperationID policy) and omit the key entirely otherwise — never a
null or empty-string value. 429 responses additionally set the standard `Retry-After` HTTP header
(RFC 9110 §10.2.3, seconds); that is a transport-level header, unrelated to `details`.

Handlers must not reuse a *different* existing 404 shape for keys' own `not_found`: the
project-scoped route's agent resolver already writes its own 404 today with `code:
"agent_not_found"` and `details: {"agent_slug":..., "project_id":...}`
(`handlers_projects_core.go:2422`-2443) for its own (non-keys) callers. A keys handler sitting
behind that resolver must answer with `code: "not_found"` (matching `OutcomeNotFound`) and, once
authenticated and past validation, an `operation_id` — not silently inherit the resolver's
different code and details shape just because it happens to be convenient to call into.

**`message` is human-readable and non-normative.** Nothing in this contract freezes the exact text of
`error.message` — only the HTTP status, `code` (the `Outcome` string), and `details.operation_id`
presence rule above are frozen. `message` may differ between routes and between implementations of
a related check: `agentkeys.ValidateBody` validates a full `{"keys":...}` envelope and so has
envelope-specific messages (e.g. "unexpected trailing content after request body") with no
equivalent in `agentkeys.ValidateKeysJSON`, which validates one already-located JSON value instead
and falls back to a generic "invalid JSON" for the same class of malformed bytes (§7). Every T/P/B
parity row in §6 (AK-7..17, AK-38..41, AK-60) is stated in terms of outcome code and HTTP status,
never message text, for exactly this reason: clients and parity tests must branch only on status,
`code`, and `details.operation_id` presence, never on `message`.

### 2.5 HTTP / machine-outcome table

Frozen as `agentkeys.Outcome` and `agentkeys.HTTPStatus` (`pkg/agentkeys/types.go`):

| HTTP | Outcome constant | Meaning |
| --- | --- | --- |
| 200 | `OutcomeDispatched` (`"dispatched"`) | Broker acknowledged terminal injection |
| 400 | `OutcomeInvalidRequest` (`"invalid_request"`) | Invalid shape or size; no terminal effect |
| 413 | `OutcomePayloadTooLarge` (`"payload_too_large"`) | `keys` exceeds 4096 bytes; no terminal effect |
| 401 | `OutcomeUnauthorized` (`"unauthorized"`) | Missing authentication; no terminal effect |
| 403 | `OutcomeKeysDenied` (`"keys_denied"`) | Authenticated but insufficient live authority; no terminal effect |
| 404 | `OutcomeNotFound` (`"not_found"`) | Missing/out-of-scope target under existing disclosure policy |
| 409 | `OutcomeAgentNotRunning` (`"agent_not_running"`) | Target not running; no wake/start |
| 409 | `OutcomeTerminalNotReady` (`"terminal_not_ready"`) | Target running, session not ready |
| 422 | `OutcomeCrossProjectKeysUnsupported` (`"cross_project_keys_unsupported"`) | Authenticated **agent** crosses its own project; see §3.1 — no disclosure at all on the project-scoped route, and the top-level route shares today's existing lifecycle-action disclosure (422 vs. 404 reveals foreign-vs-missing, exactly as 403 vs. 404 already does) |
| 422 | `OutcomeKeysUnsupported` (`"keys_unsupported"`) | Managed backend, unsupported runtime, or broker lacking the route; never downgraded to messaging |
| 422 | `OutcomeRawInputRemoved` (`"raw_input_removed"`) | Post-cutover: old raw message input rejected with `/keys` guidance |
| 422 | `OutcomeRawCombinationUnsupported` (`"raw_combination_unsupported"`) | Bridge-only (task 2.3): a legacy field implies routing/fan-out/conversation/attachment/lifecycle semantics keys does not support (§6.1) |
| 429 | `OutcomeKeysRateLimited` (`"keys_rate_limited"`) | Independent budget exceeded; `Retry-After` header set; describes admission, not permission to replay |
| 503 | `OutcomeKeysUnavailable` (`"keys_unavailable"`) | Dispatch definitively did not start (offline broker, no immediate route, expired admission) |
| 502/504 | `OutcomeKeysOutcomeUnknown` (`"keys_outcome_unknown"`) | Dispatch may have run/partially run; never auto-retried, never reported as delivered |

**OperationID policy (recorded deviation from #2184).** Authentication always runs first, entirely
outside and before the keys handler, exactly as #2184's own execution order requires
("authenticate and bounded decode → resolve → authorize → …") — `pkg/hub`'s shared
`UnifiedAuthMiddleware` (`pkg/hub/auth.go:150` onward) answers an unauthenticated request with 401,
in the existing envelope above, before any handler runs at all. **`OutcomeUnauthorized` therefore
carries no operation ID: the request never reaches the code that would mint one.** Once inside the
(now-authenticated) handler, an operation ID is minted as soon as request validation succeeds
(`ValidateBody`/`ValidateKeys` returning `nil`), before authorization (`authorizeAgentKeys`) runs,
and from that point appears in every subsequent response and audit record for the request:
`OutcomeKeysDenied`, `OutcomeNotFound`, `OutcomeAgentNotRunning`, `OutcomeTerminalNotReady`,
`OutcomeCrossProjectKeysUnsupported`, `OutcomeKeysUnsupported`, `OutcomeRawInputRemoved`,
`OutcomeRawCombinationUnsupported`, `OutcomeKeysRateLimited`, `OutcomeKeysUnavailable`,
`OutcomeKeysOutcomeUnknown`, and `OutcomeDispatched`. Only three rows carry no operation ID:
`OutcomeUnauthorized` (never reaches a handler), `OutcomeInvalidRequest` and
`OutcomePayloadTooLarge` (failures during validation itself, inside the handler but before an
operation is recognized to exist). #2184 only requires "post-admission" results (after budget
charge and phase/route checks, per its own execution order) to carry one; this contract extends
that floor to also cover the in-handler denials #2184's execution order places before admission —
`OutcomeKeysDenied`, `OutcomeNotFound`, and `OutcomeCrossProjectKeysUnsupported` — because giving
every audit-logged decision a stable opaque correlation key costs nothing once the handler is
already running, and is simpler to implement correctly than tracking the admission-phase boundary
per outcome. It does not, and cannot, extend the floor to `OutcomeUnauthorized`, which is decided
before any handler-level state exists. This is `agentkeys.Outcome`'s doc comment verbatim
(`pkg/agentkeys/types.go`); do not restate a different rule anywhere else in this document or its
implementations.

A local client/network error must also represent uncertainty honestly: an implementation may only
return `agent_not_running`/`terminal_not_ready`-style "definitely no effect" outcomes when it can
prove the call failed *before* runtime execution began; otherwise it must return
`keys_outcome_unknown`. `agentkeys.ClassifyDispatchError` (`pkg/agentkeys/broker.go`) is the single
function every later task must use to make that determination — see §4. Errors must be sanitized:
runtime stderr/argv can contain the injected input (see §5, audit).

`OutcomeCrossProjectKeysUnsupported` is a **new** code, distinct from the pre-existing
`MessageDenialCrossProjectRawUnsupported` (`"cross_project_raw_unsupported"`,
`pkg/hub/authorize_message.go:46`) that already gates agent-to-agent DM raw delivery today
(`pkg/hub/agent_dm_operation.go:357`-367, "4b. Foreign raw keystroke-injection rejection").
**Decided:** from task 2.3 onward, the bridge returns `cross_project_keys_unsupported` for that same
agent-to-agent DM case, because the bridge's decision must match a direct `/keys` call's decision
(AK-24/25 auth parity) and this is what `/keys` returns. Before 2.3 ships, the pre-existing
message-path code keeps governing that DM path unchanged — 0.1 does not touch
`agent_dm_operation.go`. 2.3 removes that branch (and the old code along with it) when it routes
agent-to-agent raw DMs through `ExecuteAgentKeys`. See §6.1's field table for the same decision
recorded against the specific legacy fields it governs, and §8 for the GCP#2053-adjacent context.

## 3. Authorization

`ExecuteAgentKeys` (task 2.2) is the sole authoritative operation. Both public routes and the
temporary raw adapter invoke it through `authorizeAgentKeys` (task 2.1); no path may reach the
broker without going through it. Broker/system credentials cannot call the public operation by
impersonating a user.

| Caller | Decision |
| --- | --- |
| Human session | `ActionAttach` on the target agent; retains owner/privacy/cross-member restrictions (same as today's `agentActionPermission` default branch, `pkg/hub/authorize.go:392`-411, function `agentActionPermission`) |
| User access token | Same human authority, intersected with credential scope and project/hub boundary; a token name or automation label is not an agent identity |
| Agent credential | Valid current credential, lifecycle scope (`ScopeAgentLifecycle`), and sender's current project **equal to** target project; no self/parent/ancestor shortcut |
| Broker credential | Only authenticated Hub→broker execution under the internal contract (§4); never direct public `/keys` authority |

Key points, restated because they are easy to get backwards:

- **Human cross-project use is allowed** when the human's live permissions and credential
  boundary allow selecting another project — this is not a blanket ban. **Agent cross-project use
  is refused** (422 `cross_project_keys_unsupported`) — an authenticated agent identity may never
  cross its own project boundary for keys, mirroring the existing raw-message rule
  (`agent_dm_operation.go:357`) and the general project-isolation rule agent identities already
  live under (`authorizeAgentLifecycle`, `pkg/hub/authorize.go:325`). These are deliberately
  different rules for different caller kinds — do not collapse them into one.
- Message modes (open/closed/etc.) and message budgets **do not** grant or deny keys. Attach
  authority must not be acquired through `agent:message` or an ordinary project-manage alias:
  verified against the registry, not just asserted — `agent.attach`
  (`pkg/hub/permissions/registry.go:129`) has `ExcludeFromManageAlias: true` and is a distinct
  registry entry from `agent.message` (`registry.go:133`, `CapabilityKind: CapabilityScope`, no
  `AgentScopes`); granting one never grants the other. `agent.attach`'s `Enforcement` list
  currently reads `["pkg/hub/authorize.go:authorizeAgentLifecycle", "pkg/hub/pty_handlers.go"]`;
  task 2.1 must append `"pkg/hub/authorize.go:authorizeAgentKeys"` to that list when it lands, so
  the registry stays an accurate index of what enforces each permission.
- CLI-side project resolution must honor the selected project: resolve a unique target within it,
  never pass an empty scope that could select a same-named agent from a different project.
  Projects without a Hub ID use the existing local project identity/filter; ambiguous resolution
  fails rather than guessing.
- `api.AgentActionKeys` is registered with an **explicit** mapping in `agentActionPermission`
  (§ "Go contract types") rather than left to that function's default branch — this is what
  decision 2 means by "distinguish the route action from an independently granted authz
  permission": the mapping exists so it is visible and auditable, not so that "keys" becomes a
  new grantable permission in its own right.
- **Required order of operations for task 2.1 — stated as invariants 2.1 must satisfy, not as
  instructions for which function the new code lives in.** Exact placement is 2.1's call; what
  follows is the observable behavior any placement must produce, checked against `main` as of this
  contract so the invariants are grounded rather than asserted:
  1. Authentication precedes everything, on both routes (existing shared middleware); 401 carries
     no operation ID (§2.5). On the project-scoped route, the shared project-resolution 404
     (`handleProjectAgents`, §3.1/AK-21e/AK-21f) also precedes everything below and, like 401,
     carries no operation ID — it answers from a gate every action on that route shares, before any
     keys-specific code, including invariant 2's validation, ever runs.
  2. Body validation (`ValidateBody`) precedes both **agent-target** resolution and authorization;
     400/413 carry no operation ID.
  3. An operation ID is minted as soon as validation succeeds, before authorization and before any
     **agent-target** resolution outcome is reported. Every outcome from that point on — `not_found`,
     `keys_denied`, both 422 codes, both 409s, 429, 503, 502/504, and 200 — carries it (§2.5); none
     of them may reuse a different, older resource-not-found shape that predates this contract and
     has neither the right code nor an operation ID (e.g. the project-scoped route's existing agent
     resolver writes `agent_not_found`/`details.agent_slug,project_id` for its own non-keys callers
     — a keys `not_found` must not inherit that shape). This is deliberately narrower than invariant
     1's project-resolution 404: that one is decided earlier, by different (non-keys) code, and is
     exempted from the operation-ID floor for that reason, not because the floor has an unstated
     exception.
  4. The agent-cross-project refusal (§3.1) is decided using only the caller's own project and
     already-available routing information (the resolved `{id}` on the top-level route, or
     `{project}`'s own resolved project ID on the project-scoped route) — **on the project-scoped
     route specifically, no target-agent lookup of any kind, successful or not, may precede or be
     required by this comparison** (AK-21c).
  5. `authorizeAgentKeys` runs only after 1-4 pass.

  **Top-level route (T), verified consistent with these invariants today:**
  `handlers_agents_core.go`'s `set_message_mode`/`reincarnate`/`message` special cases (`:2936`,
  `:2945`, `:2953`) already run before the generic `if !selfAccess` block (`:2998`) and before any
  `GetAgent` lookup keys would need — adding `api.AgentActionKeys` to that same early list, calling
  `authorizeAgentKeys` directly, satisfies invariants 1-5 without disturbing this order. Skipping
  that early branch would instead fall into the generic block's own project check (`:3017`-3019,
  403) or its `CheckAccess` call (`:3029`, "Only the agent's creator can interact with it") — both
  wrong codes and bodies for this route.

  **Project-scoped route (P): the equivalent early-branch list does *not* satisfy invariant 4 as
  written, which is why this is stated as an invariant rather than a placement instruction.**
  `handleProjectAgentAction`'s early special-case list (`set_message_mode`/`reincarnate`/`message`,
  from `:2446`) sits *after* an agent-resolution block (`:2422`-2443: `GetAgentBySlug` then a
  `GetAgent` fallback, writing 404 `agent_not_found`/`details.agent_slug,project_id` on a miss) that
  already runs once the POST-method gate (`:2415`-2418) passes. Placing keys' branch in that same
  list, by analogy with T, would query (and on a hit, disclose) target-agent existence before the
  cross-project comparison, violating invariant 4 and AK-21c. 2.1 must satisfy invariant 4 by
  whatever restructuring it chooses — for example, moving the comparison ahead of the existing
  resolution block, or gating that block's execution on the comparison having already passed for an
  agent caller — and must ensure a resolution miss reached *after* the comparison passes is reported
  per invariant 3 (`not_found`, keys envelope, operation ID), not the resolver's existing shape.

### 3.1 Agent cross-project refusal: order and disclosure

#2184 requires the 422 to be "evaluated without exposing an otherwise inaccessible target"; that
phrase alone is not an algorithm, and today's nearest precedent (the generic lifecycle path's own
cross-project check) already makes a disclosure trade-off worth naming explicitly rather than
silently inheriting.

- **Project-scoped route** (`POST /api/v1/projects/{project}/agents/{id-or-slug}/keys`): once
  `{project}` is known to resolve to a real project, compare it against the calling agent
  credential's own project **before** resolving the *agent* target at all, and return 422 on
  mismatch. This requires no agent lookup and leaks nothing about agent existence — the
  project-scoped resolver never has to determine whether a same-slug agent exists in the URL's
  project, because the credential's project alone already decides the outcome.
  **Canonicalization:** `{project}` accepts a canonical UUID or the hosted `{uuid}__{slug}` form —
  see §2.1 — never a bare slug, so the comparison is between two values already known to be UUIDs:
  `project.ID` (the field `GetProject` returns once `{project}` resolves — see below) and the agent
  credential's own project ID. Compare those two directly; there is no separate slug-to-ID
  translation step for the comparison itself to get wrong.
  **An unresolvable `{project}` is 404 `not_found`, decided *before* this comparison and before
  keys-specific code runs at all.** The route's existing entry point
  (`handleProjectAgents`, `pkg/hub/handlers_projects_core.go:2030`-2041) calls
  `s.store.GetProject(ctx, projectID)` and writes `NotFound(w, "Project")` immediately on a miss,
  for every action on the route, before dispatching to any agent- or action-specific code
  (including wherever keys' own early-branch case lives, per the bullet above). No keys-specific
  code can intervene before that point without moving the check itself, which this task does not
  ask 2.1 to do: the project resolver is shared infrastructure serving every other project-scoped
  action identically, and carving out a keys-only exception to run before it would be a change to
  that shared entry point, not to keys' own authorization path. This is not a new or inconsistent
  disclosure: every other project-scoped action already reveals project non-existence via this same
  404 today (`GET`/`PATCH`/`DELETE` on the project itself, listing its agents, and so on), so keys
  matching that existing behavior discloses nothing beyond what the route already discloses.
  **Both human and agent callers get the identical response**: `handleProjectAgents`'s project
  resolution runs before any caller-type-specific logic, so this 404 is caller-agnostic. The
  response uses the existing, already-universal `pkg/hub.ErrorResponse`/`APIError` envelope
  (`NotFound`'s own writer, `pkg/hub/errors.go:344`-347) with `code: "not_found"` — the same string
  as `OutcomeNotFound`, though this particular 404 is not itself a keys `Outcome` value, since it
  is produced by shared code the keys handler never reaches — and, like `OutcomeUnauthorized`,
  carries no `operation_id`: it happens before request validation, on a shared gate every action
  passes through, not inside the keys handler's own logic.
- **Top-level route** (`POST /api/v1/agents/{id}/keys`): **Option 1, chosen.** Resolve `{id}`
  first — keys' own lookup, the same way every other action on this route resolves its own target
  (§3's T paragraph: keys' early branch runs before any generic `GetAgent`, so this is not a
  pre-existing shared lookup keys merely reads the result of) — then compare the resolved
  agent's project to the calling agent credential's project: mismatch → 422
  `cross_project_keys_unsupported`; no such agent anywhere → 404 `not_found`. This is the same
  disclosure trade-off `authorizeAgentLifecycle`'s existing lifecycle-action path already makes
  today (`handlers_agents_core.go:2998`-3021: `GetAgent(id)` first, and a project mismatch on an
  existent agent returns a *different* outcome — 403 — than a nonexistent one): keys does not
  introduce a *new* disclosure that lifecycle actions do not already have, it only changes the
  status code from 403 to 422 for this specific action. (Option 2 — return an identical response
  for "missing" and "foreign-project" — was considered and rejected: it would make keys the one
  action on this route with a different, stricter disclosure policy than every other agent action,
  for no disclosure actually prevented, since the identical top-level route already leaks the same
  information for start/stop/message/exec today.)
- Acceptance coverage: AK-21 (agent, cross-project, existing target → 422) now has explicit
  siblings — AK-21b (agent, cross-project, non-existent target ID → 404), AK-21c (project-scoped
  route, `{project}` resolves but differs from the credential's project by canonical ID, target
  existence never queried → 422), AK-21e (project-scoped route, `{project}` does not resolve
  to any project at all → 404 `not_found`, identical for human and agent callers, no
  `operation_id`, decided before any keys-specific code runs), and AK-21f (project-scoped route,
  `{project}` is a bare project slug rather than a UUID or `{uuid}__{slug}` — this is a special
  case of AK-21e, not a separate resolvable form: a bare slug never resolves, so it 404s
  identically) — see §6.

## 4. Internal Hub → broker keys contract

Frozen in `pkg/agentkeys/broker.go` and `pkg/agentkeys/dispatcher.go`.

### 4.1 Wire format and addressing

**Path, query, body — each stated explicitly, none left for 1.1/1.2 to guess:**

- Path: `BrokerRoutePath`'s `{id}` segment is the agent's **slug**, matching every existing broker
  route (`brokerHTTPTransport.MessageAgent`, `pkg/hub/broker_http_transport.go:328`-333, calls its
  own parameter "agentID" but passes `agent.Slug`). It is not the Hub UUID.
- Query: `agentkeys.BrokerProjectIDQueryParam` (`"projectId"`) carries the canonical project ID,
  matching every existing broker route's `?projectId=` convention.
- Body (`agentkeys.BrokerRequest`, JSON-tagged): `project_id` (duplicated from
  the query, matching `MessageAgent`'s existing `reqBody["project_id"]` pattern), `agent_id`,
  `operation_id`, `execute_before` (RFC 3339 with nanoseconds, UTC — callers must call `.UTC()`
  before marshaling a `BrokerRequest`, since `time.Time` JSON-encodes whatever zone offset the
  value carries and the broker compares this deadline against its own UTC clock), `keys`.

**Identity binding to the resolved container is a hard requirement for task 1.1.** The broker
resolves the target the same way every existing route does — `matchesAgent` (slug/name/container-ID
match against the path segment, then project-label/field match against the query parameter,
`pkg/runtimebroker/handlers.go:56`-78) — and then MUST additionally require the resolved
container's existing `"agent_id"` label to equal `BrokerRequest.AgentID`, returning `not_found` on
any mismatch. That label already exists on every container started through the Hub's own dispatch
path: `pkg/agent/run.go:1382` sets it from `agentID`, which `run.go:99`-103 takes from
`opts.Env["SCION_AGENT_ID"]` when present, and the runtime broker's own start path populates that
env var from the Hub-dispatched canonical agent ID
(`pkg/runtimebroker/start_context.go:403`-405, `in.AgentID`). Nothing new needs to be added to
provisioning. Binding to this label — not just the slug — is what prevents a same-slug agent
recreated (deleted and rebuilt, or restarted with a new identity) inside the execute-before window
from receiving input meant for the agent that was originally resolved and authorized: slug-only
resolution cannot tell the two apart, and #2184 requires the broker to "verify the agent belongs to
the addressed runtime/project" and to cover "target deletion/restart race" without rerouting.

**This must be atomic, not check-then-use.** The dedicated keys handler does not resolve the
container and check the label itself and then call a separately-resolving primitive: that would
leave a race window between the check and the use, in which exactly the recreate this binding
exists to prevent could occur, reopening the gap. Instead the check is frozen into the manager
primitive itself — see §4.3's frozen `SendKeys` signature, which takes the expected agent ID as a
parameter and resolves, checks, and executes within that one call, on the one container it resolved.
Fail closed on a missing or empty `"agent_id"` label (e.g. a container started outside the Hub's
dispatch path, or started before `SCION_AGENT_ID` injection existed): `SendKeys` treats it as
`ErrTargetNotFound`, translated to `not_found`, the same as a mismatch — never a slug-only match.

**Rollout-visible consequence, stated here because it is a real cutover risk, not an implementation
detail:** any already-running container whose `agent_id` label does not equal the Hub's canonical
ID for it will answer every keys request with `not_found` until it is restarted under the current
dispatch path. That is the intended fail-closed behavior — not a bug to route around with a
slug-only fallback — but it is a change operators will observe during rollout and should be told
about, not discovered. See AK-45/AK-46 in §6 for the acceptance rows this adds.
`agentkeys.BrokerRequest.AgentID`'s doc comment states this identically.

**Outcome channel:** success is HTTP 200 with body `Outcome == OutcomeDispatched` — the only success
shape; there is no other combination that means success. Every other case is a failure the broker
reports via a well-formed `BrokerResult` body carrying an outcome from a fixed allowlist
(`OutcomeNotFound` — including the identity-binding mismatch above —, `OutcomeAgentNotRunning`,
`OutcomeTerminalNotReady`, `OutcomeKeysUnsupported`, `OutcomeKeysUnavailable` for an
already-expired deadline found at broker admission), at the matching HTTP status
(`HTTPStatus(outcome)`). See §4.3 for exactly how an adapter turns that body into
`agentkeys.BrokerOutcomeError`, the one case where a **plain** 404 (no parseable `BrokerResult`)
means something different (§4.3), and the fail-safe default for anything else.

**Typed request** (`agentkeys.BrokerRequest`): `ProjectID`, `AgentID`, `OperationID`,
`ExecuteBefore time.Time`, `Keys`. It carries strictly less than the public request — no caller
identity, no session/tmux options — so the broker cannot reinterpret scope from anything the
client sent.

**Typed result** (`agentkeys.BrokerResult`): `OperationID`, `Outcome`, and a `Message` that must
never contain key content, argv, or runtime stdout/stderr.

**Broker route**: `agentkeys.BrokerRoutePath` = `POST /api/v1/agents/{id}/keys`
(`agentkeys.BrokerRouteMethod`). This reuses the existing generic
`/api/v1/agents/{id}/{action}` shape (`pkg/runtimebroker/handlers.go:1341`,
`extractAction`/`handleAgentAction`) with `action = "keys"` — the broker's mux needs no new
pattern — but the handler is dedicated and must not call `sendMessage`'s code (decision 1).

**Control-channel "method name"**: there is no separate RPC method registry in this codebase.
`ControlChannelBrokerClient` (`pkg/hub/controlchannel_client.go:261`) tunnels the same HTTP method
and path used over plain HTTP inside a `wsprotocol.RequestEnvelope`. The "control-channel method
name" §2184 asks this task to freeze is therefore the same constant: `POST`, tunneled to
`agentkeys.BrokerRoutePath`.

### 4.2 Execute-before deadline

`agentkeys.CapExecuteBefore(now, requestDeadline, window)` computes `min(requestDeadline,
now+window)`, where `window` should normally be `agentkeys.DefaultAdmissionWindow` (30s). It
**fails closed**: a zero `requestDeadline` (missing/invalid internal deadline) returns
`agentkeys.ErrMissingDeadline` rather than silently falling back to `now+window`. A non-positive
`window` returns `agentkeys.ErrInvalidWindow` rather than silently producing an already-expired
deadline. The broker cannot extend the resulting deadline; it must enforce
expiration at broker admission, after any control-channel semaphore/target-lock wait, and
immediately before runtime execution. Hub/broker clocks are assumed reasonably synchronized.

### 4.3 Error classification

`BrokerClient.ExecuteKeys` and `Dispatcher.DispatchAgentKeys` both return `(BrokerResult, error)`.
What a non-nil `error` means is the entire honest-outcome and no-replay property — without one
frozen rule, each 1.2 adapter would classify differently. Frozen now, in `pkg/agentkeys/broker.go`:

- **Sentinel errors, split by which layer proves the condition.** One is Hub-side-only:
  `ErrNotDispatched` (the call is proven to have failed before it reached, or before any broker
  handler began runtime execution on, the target — network refused, no synchronous route to the
  broker at all, or the Hub's own pre-send check found `ExecuteBefore` already past; maps to
  `OutcomeKeysUnavailable`, 503). Task 1.2's `Dispatcher`/`BrokerClient` implementations return this
  one directly, with no broker response to translate. The remaining manager-level return classes —
  `ErrTargetNotFound`/`ErrAgentNotRunning`/`ErrTerminalNotReady`, `ErrKeysUnsupported`, and an error
  wrapping `ErrKeysNotStarted` (task 1.1's implementation; see the full return-class table in §4.3's
  "Frozen manager signature" bullet below for exactly which of `SendKeys`'s return values these are
  and how each is translated) — are returned by `SendKeys` (broker-side, task 1.1) and translated by
  the broker's own dedicated keys handler into the matching `BrokerResult`/`BrokerOutcomeError`
  before any HTTP response leaves the broker process; task 1.2's Hub-side code never sees or wraps
  these directly. These do **not** cover "the broker responded" in any wire-visible form on the Hub
  side — see `BrokerOutcomeError` next, which is what a 1.2 adapter actually constructs after
  decoding the broker's already-translated response. This split is about which Go process proves and
  returns each error, not about a separate Hub-transport sentinel set: every manager-level class
  below is broker-process-internal and reaches the Hub only as an already-translated HTTP response.
- **One error type for what the broker decides about itself and reports back**:
  `agentkeys.BrokerOutcomeError{Outcome, Message}`, constructed by an adapter only when the broker's
  response is a well-formed, allow-listed decision — see `ValidBrokerOutcome` and
  `BrokerOutcomeError`'s doc comment in `pkg/agentkeys/broker.go` for the exact allowlist (the five
  outcomes in §4.1's "Outcome channel" paragraph) and construction rule (HTTP status matches
  `HTTPStatus(outcome)`, body decodes as a `BrokerResult`, outcome is allow-listed).
- **The one specified exception, closing the "old broker vs. missing agent" ambiguity**: both an
  old broker without the keys route at all (its generic unrecognized-action handler,
  `pkg/runtimebroker/handlers.go:1537`-1540, answers 404 via `NotFound(w, "Action")`, producing
  `{"error":{"code":"not_found",...}}` — no top-level `outcome` field) and the dedicated handler's
  own "agent doesn't exist" (`BrokerResult{Outcome: "not_found"}` at 404) are plain HTTP 404. An
  adapter distinguishes them by shape, not by guessing: **HTTP 404 whose body does not decode as a
  `BrokerResult` with a `ValidBrokerOutcome` value constructs
  `BrokerOutcomeError{Outcome: OutcomeKeysUnsupported}`** (this is how "broker lacks the route"
  becomes 422 `keys_unsupported`, matching #2184's "old brokers return keys_unsupported" and
  AK-28 — not `keys_outcome_unknown`, and not `ErrNotDispatched`, since neither correctly conveys
  "this is a capability gap, safe to treat like any other unsupported-backend case"). **Any other
  malformed or disagreeing response** — an unparseable body on a non-404 status, a status/body
  outcome mismatch, or a 2xx whose `Outcome` is not `OutcomeDispatched` — classifies as
  `OutcomeKeysOutcomeUnknown` instead, because unlike a plain 404 those shapes cannot rule out that
  a real handler began executing before producing a malformed response.
- `agentkeys.ClassifyDispatchError(err) Outcome` is the **only** code allowed to decide what an
  error means, and it recognizes exactly two things: a `*BrokerOutcomeError` (re-validating its
  `Outcome` against `ValidBrokerOutcome` itself, trusting no adapter blindly) and `ErrNotDispatched`
  (`errors.Is`, mapping to `OutcomeKeysUnavailable`). Everything else defaults to
  `OutcomeKeysOutcomeUnknown` — `nil` (a caller bug — success has no error to classify), a
  `BrokerOutcomeError` asserting an outcome outside the allowlist (a transport bug, not a decision
  to trust), an ordinary unrecognized error, **and, deliberately, any manager-level return value
  from `SendKeys`'s own return-class table** (§4.3's "Frozen manager signature" bullet below —
  `ErrTargetNotFound`/`ErrAgentNotRunning`/`ErrTerminalNotReady`, `ErrKeysUnsupported`, an error
  wrapping `ErrKeysNotStarted`, or a `*agentkeys.ValidationError`) if one ever reached this function
  unwrapped. `ClassifyDispatchError` does not special-case any of those: a correct task 1.1/1.2
  implementation always translates each into a `BrokerOutcomeError` (or, for `ValidationError`, into
  the handler's own pre-existing plain-envelope rejection — see the table below) before this
  function ever sees them (per the bullet above), so the fail-safe for a bug that forwards one
  verbatim is the same as for any other unrecognized error — never a guess that the forwarding was
  reliable.
- **Concrete trap named and closed:** `HybridBrokerClient.MessageAgent` returns
  `ErrMessageDeferred` for its routeForward/undeliverable case
  (`pkg/hub/controlchannel_client.go:745`-753), meaning "queue it, a durable send will happen
  later." Keys has no durable queue, so task 1.2's hybrid keys adapter must return
  `ErrNotDispatched` for the equivalent case, never `ErrMessageDeferred` or any other
  deferred/queued error.
- Every adapter — HTTP, control-channel, hybrid, and the authenticated wrapper
  (`AuthenticatedBrokerClient` over `brokerHTTPTransport`, `pkg/hub/brokerclient.go:27`-38) — must
  be single-attempt: no SDK `WithRetry`, no HTTP redirect following, no tunnel-reconnect resend, no
  routing fallback after an uncertain send. An adapter that retries internally has already violated
  no-replay before `ClassifyDispatchError` is ever reached.
- **Frozen manager signature, with identity binding carried atomically** (task 1.1,
  `pkg/agent.Manager`/`AgentManager`):
  `SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error`,
  replacing today's `MessageRaw(ctx, agentID, projectID, keys string) error`
  (`pkg/agent/manager.go:76`,`377`). The identity check must not be split across two steps — a
  broker handler that resolves and checks the label itself, then calls a manager primitive that
  re-resolves by slug alone (the way `MessageRaw` does today) and executes, reopens the
  recreate-inside-the-window race the binding exists to close, because nothing guarantees the second
  resolution finds the same container the first one checked. `SendKeys` closes that gap by doing
  resolution, the `"agent_id"`-label check against `expectedAgentID`, and delivery, in one call, on
  one resolved container: the handler passes `BrokerRequest.AgentID` straight through as
  `expectedAgentID` and does no resolution or label check of its own (see §4.1). The Go signature
  above and the public wire contract are unchanged by everything below; this table documents task
  1.1's implementation of what this signature always promised — an honest return value — not a
  new interface.

  **Return classes.** The signature was originally frozen naming three sentinels and omitting a
  fourth (`ErrKeysUnsupported`) that this contract already requires elsewhere (§2.5/AK-28); this
  table is the corrected, complete, and authoritative enumeration. Every row below is a
  manager/broker-process return value — internal to this one process boundary, translated to an
  HTTP response before it ever leaves the broker — and is a **different, broker-internal taxonomy**
  from §4.3's own `BrokerOutcomeError`/`ErrNotDispatched` taxonomy above, which is what task 1.2's
  Hub-side adapters actually see on the wire and which this table does not change:

  | `SendKeys` return value | Proven before delivery began? | Broker handler's translation |
  | --- | --- | --- |
  | `nil` | — (success) | `BrokerResult{Outcome: OutcomeDispatched}`, HTTP 200 |
  | `agentkeys.ErrTargetNotFound` | Yes | `BrokerResult{Outcome: OutcomeNotFound}`, HTTP 404 |
  | `agentkeys.ErrAgentNotRunning` | Yes | `BrokerResult{Outcome: OutcomeAgentNotRunning}`, HTTP 409 |
  | `agentkeys.ErrTerminalNotReady` | Yes | `BrokerResult{Outcome: OutcomeTerminalNotReady}`, HTTP 409 |
  | `agent.ErrKeysUnsupported` | Yes | `BrokerResult{Outcome: OutcomeKeysUnsupported}`, HTTP 422 |
  | error wrapping `agent.ErrKeysNotStarted` (checked via `errors.Is` on that sentinel only) | Yes | `BrokerResult{Outcome: OutcomeKeysUnavailable}`, HTTP 503 |
  | `*agentkeys.ValidationError` | Yes — checked before any resolution | Not a `BrokerOutcomeError` case — see below |
  | any other error | No | the ordinary non-`BrokerResult` error envelope; a Hub-side adapter classifies this `OutcomeKeysOutcomeUnknown` |

  "Proven before delivery began" means proven before the tmux delivery call itself began — **not**
  "before any `Exec`": `SendKeys`'s own terminal-readiness probe is itself an `Exec` call and may
  already have run (and succeeded) before one of the first six rows is returned. Once delivery may
  have begun, ctx cancellation or an expired deadline alone never proves non-execution on their own
  — that ambiguity is exactly the "any other error" row, never one of the proven rows above.

  `agent.ErrKeysNotStarted` is matched by `errors.Is` on that sentinel's own identity, never by
  inspecting a wrapped context error's identity or type: the actual delivery call's own failure path
  deliberately does not join its error with `%w`, so a backend error that happens to wrap a context
  error *after* delivery began can never be mistaken for this row by an implementation that
  (incorrectly) checked for a context error directly instead of this sentinel's identity.

  `*agentkeys.ValidationError` is **not** added to `ValidBrokerOutcome`'s allowlist, and
  `ClassifyDispatchError` is **not** extended to recognize it — it does not become an additional
  broker-assertable outcome alongside the five already listed in §4.1's "Outcome channel"
  paragraph. In the ordinary flow the runtime broker's dedicated keys handler
  validates keys shape itself, before ever calling `SendKeys`, and responds directly with the
  existing plain (non-`BrokerResult`) 400/413 envelope already described in §2.4a/§2.5 — a separate,
  pre-existing rejection path, not a translation of a value `SendKeys` returned. `SendKeys` performs
  the identical check defensively, for a caller (e.g. local mode) that invokes it directly without
  going through that handler; a correct implementation never lets this value reach the handler's
  `BrokerResult` translation switch at all.

  Any other, untranslated manager return value that somehow reaches the Hub side (a task 1.1/1.2
  implementation bug) falls through `ClassifyDispatchError`'s existing default case to
  `OutcomeKeysOutcomeUnknown`, the same fail-safe as any other unrecognized error — never a guess
  that some proven-safe classification applies.

### 4.4 Dispatcher/broker-client interfaces

```go
// Target is a Hub-internal call parameter, never serialized.
type Target struct {
    RuntimeBrokerID string
    AgentID         string // canonical Hub UUID
    AgentSlug       string // broker-resolution identifier (path "{id}")
    ProjectID       string // canonical Hub project ID
}

type BrokerClient interface {
    ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req BrokerRequest) (BrokerResult, error)
}

type Dispatcher interface {
    DispatchAgentKeys(ctx context.Context, target Target, operationID string, executeBefore time.Time, keys string) (BrokerResult, error)
}
```

`Dispatcher` deliberately does not take a bare `(agentID, projectID string, req BrokerRequest)`
shape, where `req.AgentID`/`req.ProjectID` would carry the same two values a second time with no
rule for what happens if they disagree. `Target` is instead the **only** identity input to
`Dispatcher`, carrying every field `pkg/hub.AgentDispatcher`'s existing methods already take from
an already-resolved `*store.Agent` (compare `HTTPAgentDispatcher.DispatchAgentMessage`,
`pkg/hub/httpdispatcher.go:2726`, which reads exactly `RuntimeBrokerID`/`agent.ID`/`agent.Slug`/
`agent.ProjectID` off `*store.Agent`); `BrokerRequest` is built **inside** the `Dispatcher`
implementation from `Target` plus this method's own `operationID`/`executeBefore`/`keys`
arguments, so there is nothing left that can disagree with itself. `Dispatcher` does not
re-resolve or re-authorize `target` — 2.2 passes the same `*store.Agent`-derived facts it already
authorized and phase-checked; a second store read inside the dispatcher could observe a target
that changed since then.

These are **new, standalone interfaces**, not new methods on the existing
`pkg/hub.AgentDispatcher` / `pkg/hub.RuntimeBrokerClient` (`pkg/hub/server.go:394`,`538`). Adding
a method to either of those would force every existing implementation and test double across the
repo to grow a keys method before any route exists — real behaviour-adjacent wiring, which this
task must not do. Task 1.2 implements `BrokerClient` once per transport (HTTP, control-channel,
hybrid, and the authenticated wrapper — §4.3) and implements `Dispatcher` the way
`HTTPAgentDispatcher.DispatchAgentMessage` wraps `RuntimeBrokerClient.MessageAgent` today. Task
2.2's `ExecuteAgentKeys` calls `Dispatcher`; it does not itself pick a transport.

Serialization/locking: per-target critical sections (serializing keys injections against message
paste/interrupt) live in the owning broker manager (task 1.1's territory,
`pkg/agent/manager.go`), not in `pkg/agentkeys` — that package has no I/O.

No conversation resolver, message store, observer, mention parser, notification subscription,
message-status callback, or managed `CreateInteraction` call is involved anywhere in this path.

## 5. Concrete defaults

All frozen as named constants so later tasks share one source of truth instead of re-deriving
magic numbers:

| Default | Value | Constant |
| --- | --- | --- |
| `keys` field size ceiling | 4096 UTF-8 bytes | `agentkeys.MaxBytes` |
| HTTP request body ceiling | 32 KiB | `agentkeys.MaxHTTPBodyBytes` |
| Per-principal+project rate limit | 5 req/s, burst 10 | `agentkeys.PrincipalProjectRateLimit`, `agentkeys.PrincipalProjectBurst` |
| Per-target rate limit | 10 req/s, burst 20 | `agentkeys.TargetRateLimit`, `agentkeys.TargetBurst` |
| Admission window (execute-before cap) | 30s | `agentkeys.DefaultAdmissionWindow` |

**Decoding gaps.** (a) A request body larger than
`MaxHTTPBodyBytes` returns 413 `payload_too_large` directly from the transport-level read (e.g.
`http.MaxBytesReader`'s error), without ever reaching `ValidateBody`. (b) `agentkeys.ValidateBody` is the
**only** decoder any `/keys` handler may use for the request body. Decoding into `Request` with
plain `encoding/json.Unmarshal` instead would accept case-variant field names (`"KEYS"`) and a
duplicate `"keys"` key (silently keeping the last one) that `ValidateBody` correctly rejects —
using `Request` as a struct tag reference is fine, unmarshaling directly into it for request
parsing is not.

Rate limits are **per-Hub-instance** token buckets (no distributed quota service in this design);
both the `/keys` routes and the temporary raw bridge share the same two buckets, separate from the
aggregate DM message allowance. Local mode has no Hub quota. Limit-map entries must expire when
inactive (task 2.2's concern; not implemented here).

**Audit event field list** (content-free; admission and outcome events both use it): timestamp,
operation ID, authenticated actor kind/ID, source project (when applicable), target agent/project,
credential ID/kind (if already available), route (`"keys"` or `"raw"` for the transitional
bridge), input byte length, decision code (an `agentkeys.Outcome` value), dispatch outcome,
duration. **Never** logged in a keys path, including debug logs and errors: the input itself, any
preview or hash of it, named-key summaries, terminal contents, bearer values, request JSON, or
runtime command arguments.

This has a real, already-identified source-level leak to close before 2.2/1.1 ship: `pkg/runtime/
common.go`'s `runSimpleCommand`/`runSimpleCommandWithStdin` (lines ~620-655) log
`strings.TrimSpace(string(out))` — the combined stdout/stderr of a failed command — on failure.
`agent.Manager.SendKeys`'s tmux delivery call (see §2.3's "Correction recorded during task 1.1
implementation" for the exact mechanism) goes through `Runtime.Exec` (the readiness probe) and
`Runtime.ExecWithStdin` (the keys delivery itself), and on some runtime backends that path can
reach these helpers (or their per-backend equivalents); Kubernetes errors in particular can embed
stderr. Broker-level redaction is not sufficient — task 1.1 must suppress this at the source for
the keys call path specifically (it must not blanket-disable failure logging for every other
caller of these helpers, which rely on it for real diagnostics).

## 6. Acceptance matrix

Later tasks cite these IDs in their own test names/comments. "Route" column: **T** = top-level
`/api/v1/agents/{id}/keys`, **P** = project-scoped route, **B** = temporary raw bridge
(`message --raw` / top-level `raw`/`nested raw`), **All** = all three.

AK-3..AK-6 are marked **T/P**, not **All**: a legacy `MessageRequest` has no single `"keys"` JSON
field to be missing, duplicated, wrongly-typed, or unknown-adjacent to — §6.1 governs which legacy
fields are accepted/ignored/rejected instead. AK-7..AK-17 stay **All**: they describe properties of the
*string content* itself (empty, whitespace-only, NUL, size, tmux semantics), and the bridge
validates that same string — sourced from `message`/`structured_message.msg` instead of `keys`,
via `agentkeys.ValidateKeysJSON` (§6.1's "Keys-content parity" paragraph and §7) rather than
`ValidateKeys` — before calling `ExecuteAgentKeys`, so those properties hold for the bridge exactly
as stated **provided the bridge extracts that string with `ValidateKeysJSON` from the shadow-decoded
raw bytes, not from the lenient `encoding/json` string already produced for raw-selection** — a
naive extraction via that lenient decode would silently accept invalid UTF-8/lone-surrogate content
U+FFFD-corrupted instead of rejecting it, breaking AK-16's parity for exactly that input class. See
the B-specific rows (AK-38 onward) added after AK-37 for the bridge's own envelope-shape and
field-rejection cases, which are not the same shape as AK-3..AK-6.

| ID | Route | Input / scenario | Expected outcome |
| --- | --- | --- | --- |
| AK-1 | T | `{"keys":"C-c"}`, valid session, attach authority | 200 `dispatched` |
| AK-2 | P | Same as AK-1, project-scoped | 200 `dispatched`, identical body/outcome shape to AK-1 |
| AK-3 | T/P | Unknown top-level field | 400 `invalid_request` |
| AK-4 | T/P | Duplicate `"keys"` field | 400 `invalid_request` |
| AK-5 | T/P | Missing `keys` field | 400 `invalid_request` |
| AK-6 | T/P | `keys` is a number/bool/null/array/object | 400 `invalid_request` |
| AK-7 | All | `keys` is `""` | 400 `invalid_request` (never becomes Enter) |
| AK-8 | All | `keys` is whitespace-only (`"   "`) | 200 `dispatched`; the exact whitespace reaches argv |
| AK-9 | All | `keys` contains a NUL byte | 400 `invalid_request` |
| AK-10 | All | `keys` is exactly 4096 bytes | 200 `dispatched` |
| AK-11 | All | `keys` is 4097 bytes | 413 `payload_too_large` |
| AK-12 | All | `keys` = `"Enter"` | Recognized tmux key name; single `send-keys` argv element `"Enter"` |
| AK-13 | All | `keys` = `"Escape"`, `"C-c"` | Recognized tmux key names; verbatim single argv element |
| AK-14 | All | `keys` contains spaces, e.g. `"Up Up Enter"` | **Typed as 11 literal characters, not three key presses** — see §2.3 |
| AK-15 | All | `keys` = `"@builder do the thing"` | Literal text; never parsed for mentions/recipients |
| AK-16 | All | `keys` contains Unicode (accents, CJK, emoji) | Preserved byte-for-byte; ceiling checked in bytes, not runes |
| AK-17 | All | No shell metacharacters interpreted (`$(...)`, `` ` ``, `;`, `\|`) | Typed literally; no shell evaluation |
| — | All | Two logical key presses in one call | **Not supported** — no atomic sequence API; caller must issue two calls |
| AK-18 | T/P | Human session, no `ActionAttach` on target | 403 `keys_denied` |
| AK-19 | T/P | No authentication | 401 `unauthorized` |
| AK-20 | T/P | Agent credential, same project, lifecycle scope | 200 `dispatched` |
| AK-21 | T | Agent credential, cross-project target that exists | 422 `cross_project_keys_unsupported` (§3.1 Option 1: resolve first, then compare) |
| AK-21b | T | Agent credential, target ID does not exist in any project | 404 `not_found` — distinguishes "foreign" from "nonexistent" per §3.1 |
| AK-21c | P | Agent credential, URL `{project}` (UUID or hosted `{uuid}__{slug}` form) resolves to a project != credential's project | 422 `cross_project_keys_unsupported`, decided before any target-agent lookup — target existence is never queried (§3.1) |
| AK-21d | B (post-2.3) | Agent-to-agent raw DM across projects, routed through `ExecuteAgentKeys` | 422 `cross_project_keys_unsupported` — produced by the bridge order's own step 6 (§6.1), same code and decision as AK-21 (§2.5, §8) |
| — | B (pre-2.3) | Agent-to-agent raw DM across projects, current `agent_dm_operation.go` branch | 422 `cross_project_raw_unsupported` (unchanged pre-2.3 behavior; pinned here so 2.3's cutover of this exact case is a visible, intentional change, not a silent one) |
| AK-21e | P | Any caller (human or agent), URL `{project}` does not resolve to any project | 404 `not_found` via `handleProjectAgents`'s existing project resolver, before any keys-specific code runs; identical for human and agent callers; no `operation_id` (§3.1) |
| AK-21f | P | Any caller, URL `{project}` is a bare project slug (not a UUID, not `{uuid}__{slug}`) | 404 `not_found`, identical to AK-21e — a bare slug is simply one more unresolvable value, not a supported addressing form (§2.1, §3.1) |
| AK-22 | T/P | Human session, cross-project target, live attach authority present | 200 `dispatched` — **not** a blanket cross-project ban |
| AK-23 | T/P | Closed message mode, attach authority present | Same allow decision as an open-mode caller — message mode does not gate keys |
| AK-24 | B | Attach authority, closed message mode, `message --raw` | Same decision as AK-23's `/keys` call (auth parity) |
| AK-25 | B | Message-only authority (no attach), `message --raw` | Denied — same as a `/keys` call from the same caller |
| AK-26 | All | Target agent not running | 409 `agent_not_running`; no wake/start |
| AK-27 | All | Target running, terminal/session not ready | 409 `terminal_not_ready` |
| AK-28 | All | Managed-runtime backend or broker without the keys route | 422 `keys_unsupported`; never downgraded to a message send |
| AK-29 | All | Broker unreachable / no immediate route (incl. cross-Hub forward with no synchronous transport) | 503 `keys_unavailable`; before execution, no queued retry |
| AK-30 | All | Ambiguous local/network failure after dispatch attempted | 502/504 `keys_outcome_unknown`; response never says "delivered"; caller must not auto-retry |
| AK-45 | All | Slug/name resolves to a container, but its `agent_id` label differs from the requested `AgentID` (e.g. a same-slug agent recreated inside the execute-before window) | 404 `not_found`; never dispatched to the mismatched container (§4.1 identity binding) |
| AK-46 | All | Slug/name resolves to a container with a missing or empty `agent_id` label (started outside the Hub's dispatch path, or before `SCION_AGENT_ID` injection existed) | 404 `not_found`; fail closed, never a slug-only match (§4.1). **Rollout-visible**: any container in this state stays `not_found` for keys until restarted under the current dispatch path — not a bug, but an operator-visible behavior change worth flagging during cutover, not something to silently work around with a fallback |
| AK-47 | All | Old broker without the keys route responds 404 with its generic unrecognized-action body (no parseable `BrokerResult`) | 422 `keys_unsupported`, not `keys_outcome_unknown` — the one specified 404 exception (§4.3) |
| AK-31 | All | Rate limit exceeded (principal+project or target bucket) | 429 `keys_rate_limited`; `Retry-After` present |
| AK-32 | All | Any allow/deny/unknown outcome | Content-free audit record exists per §5's field list; captured logs contain no key content when searched for a distinctive test secret |
| AK-33 | All | Successful dispatch | Before/after counts show zero new `store.Message` rows, zero SSE `message` events, zero observer/mention/notification fan-out |
| AK-34 | All | Any outcome | No automatic replay: SDK `WithRetry`, HTTP redirect replay, message-retry helpers, tunnel reconnect replay, and routing fallback are all disabled on this path |
| AK-35 | B (post-cutover) | Old raw message input after Phase 4 removal | 422 `raw_input_removed`, names `/keys` as the replacement |
| AK-36 | B | `raw:false` after Phase 4 tombstone | Rejected at decode time, does not silently become a normal message |
| AK-37 | All | `Plain` requests (no `Raw`) | Unaffected; ordinary message semantics; no redirect to keys |
| AK-38 | B | `raw:true` with an empty `msg`/`structured_message.msg` | 400 `invalid_request` (same rule as AK-7, sourced from the legacy body field) |
| AK-39 | B | `raw:true` with `msg` containing an escaped NUL (`\u0000`) | 400 `invalid_request` (same rule as AK-9) |
| AK-40 | B | `raw:true` with `msg`/`structured_message.msg` at 4096 / 4097 bytes | 200 `dispatched` / 413 `payload_too_large` (same boundary as AK-10/11) |
| AK-41 | B | Nonempty legacy `message` and nonempty nested `structured_message.msg` disagree | 400 `invalid_request` (§6.1 `msg`/`message` row) |
| AK-42 | B | `raw` and `plain` both true (either field, either location) | 400 `invalid_request` (§6.1 `plain` row) |
| AK-43 | B | `raw:true` plus any one of: nonempty `recipients`, `mentions`, `attachments`, `metadata`; `interrupt`, `notify`, `wake`, `broadcasted`, `observer_only` true; nonempty `surface`, `external_ref`, `parent_ref`, `channel`, `thread_id`, `conversation_id`, `delivery_text`; a `recipient`/`recipient_id` that fails every comparison in §6.1's recipient rule | 422 `raw_combination_unsupported` (§6.1's "Rejected" rows); affects zero targets, no message side effect |
| AK-44 | B | `raw:true` with only `keys`-equivalent content and CLI-incidental `version`/`timestamp`/`type`/matching `recipient` | 200 `dispatched`; ignored fields do not block the call |
| AK-48 | B | `recipient` addresses the target by raw UUID (`recipient_id`-style value in the `recipient` field) rather than slug | 200 `dispatched` — matches the resolved agent's UUID (§6.1 recipient comparison) |
| AK-49 | B | `recipient` addresses the target by a non-canonical/non-slugified display name the client typed verbatim | 200 `dispatched` — matches via `api.Slugify(value)` against the resolved slug (§6.1) |
| AK-50 | B | Legacy `Plain` (non-raw) message whose body exceeds `agentkeys.MaxHTTPBodyBytes` (32 KiB) — e.g. a `msg` of 16000 non-ASCII runes, `\uXXXX`-escaped to ~96 KB of body — but is well within the new 2 MiB read bound and valid today | Unaffected — the bridge's pre-authorization raw-selection read must not apply the keys-specific 32 KiB ceiling; `authorizeAgentMessage` then `handleAgentMessage` proceed exactly as they do today, no new 413 (§6.1) |
| AK-51 | B | Cross-project agent sender with `raw:true` and a nonempty unsupported field (e.g. `wake:true`) in the same request | 422 `cross_project_keys_unsupported`, never `raw_combination_unsupported` — step 6 precedes step 7 (§6.1's precedence rule) |
| AK-52 | B | Any request (raw or not) whose body exceeds the new 2 MiB pre-authorization read bound | 413, generic (non-keys) Hub envelope, before authorization — a new, disclosed behaviour change (§6.4); a body built only from fields that have a limit today cannot come within roughly 1 MiB of this bound |
| AK-53 | B | Raw selected via a case-variant spelling (`"RAW":true`, or `"structured_message":{"Raw":true}`) | Classified identically to the canonical spelling by `encoding/json`'s case-insensitive field matching — reaches the bridge (steps 2-9), never legacy message-path raw delivery (§6.1 classification parity) |
| AK-54 | B | A well-formed raw JSON object immediately followed by trailing bytes in the same body | Leading object raw → bridge (steps 2-9), trailing bytes ignored, the same as `readJSON` ignores them. Leading object not raw → unchanged message path, trailing bytes handled exactly as `readJSON` handles them today. Never legacy raw delivery either way (§6.1 classification parity, dispatch-layer fail-closed invariant) |
| AK-55 | All (dispatch layer) | A `structuredMsg` with `Raw == true` reaches an `AgentDispatcher.DispatchAgentMessage` implementation after 2.3 ships — a classification-parity defect scenario; must not occur in a correct implementation | Refused at the dispatch layer: zero broker calls, never delivered via `mgr.MessageRaw`. Any already-persisted row is marked failed through the caller's existing failure path; a synchronous caller returns a generic non-keys-specific failure, an asynchronous caller logs a content-free defect signal only — this layer cannot make an already-persisted row or already-published SSE/observer event disappear (§6.1 dispatch-layer backstop, part (a)) |
| AK-56 | B (pre-2.3, 0.2) | Legacy `structured_message.raw` on `POST /api/v1/projects/{projectId}/broadcast` | Will be rejected unconditionally by task 0.2 (ptone/scion#2218, open as of this writing) before 2.3 ships; cited here as an ingress-inventory prerequisite of 2.3, not a behaviour this task or 2.3 itself introduces (§6.1) |
| AK-57 | B (pre-2.3, 0.2) | Legacy `message.raw` via either broker/plugin-inbound route (`/api/v1/broker/inbound`, `/api/v1/broker/inbound/routed`) | Will be rejected unconditionally by task 0.2 before sender resolution; cited here as an ingress-inventory prerequisite of 2.3 (§6.1) |
| AK-58 | B (pre-2.3, 0.2) | A `"raw"` key, any value including `false`, in a scheduled-event or recurring-schedule advanced Payload JSON | Will be tombstoned with 422 by task 0.2; cited here as an ingress-inventory prerequisite of 2.3 (§6.1) |
| AK-59 | B | `Decoder.Decode` returns a non-nil error after already populating `Raw == true` (e.g. `{"raw":true,"message":"hi","interrupt":"yes"}`, where `interrupt` is given a string instead of a bool) | Classifies as not-raw regardless of the already-populated `Raw` field; falls through to the unchanged message path; `readJSON`'s own identical decode hits the same error and returns today's existing post-authorization 400; no bridge, no dispatch (§6.1 classification parity) |
| AK-60 | B | `{"raw":true,"message":"\ud800","structured_message":{"msg":"<literal U+FFFD>"}}` — the lenient classification decode reduces both fields to the same "�" content, but `message` and `structured_message.msg` are two different fields | 400 `invalid_request` deterministically: the shadow-decode strictly decodes `message`'s raw bytes independently of `structured_message.msg`'s, and `message`'s lone surrogate escape fails strict decoding regardless of `structured_message.msg`'s value or which candidate an implementation happens to check first (§6.1 Keys-content parity) |

### 6.1 Legacy envelope field table (2.3 bridge)

Base: current `main` `MessageRequest` (`pkg/hub/handlers_agent_messaging.go:1388`) and
`messages.StructuredMessage` (`pkg/messages/types.go:122`), including GCP#2053's top-level
`raw`/`plain`. "Bridge" = the temporary normalization from a legacy raw request into an
`agentkeys.Request`/`ExecuteAgentKeys` call (task 2.3).

**For task 2.3: where the bridge must branch — an invariant, not a placement instruction, because
the obvious placement breaks auth parity.** On both routes, `authorizeAgentMessage` runs and can
deny the request *before* `handleAgentMessage` is ever reached: verified at
`handlers_agents_core.go:2953`-2996 (top-level `AgentActionMessage` branch: identity check,
`GetAgent`, `authorizeAgentMessage`, and only on success a `goto actionDispatch` that eventually
calls `handleAgentMessage`) and `handlers_projects_core.go:2460`-2488 (the project-scoped
`AgentActionMessage` branch, same shape: `authorizeAgentMessage` then `s.handleAgentMessage`).
Placing the bridge's raw-detection and dispatch inside `handleAgentMessage` itself — the natural
reading of "both routes delegate to the same handler" below — would therefore only be reached after
message-mode authorization already passed, breaking auth parity: AK-24 requires attach authority
with a closed/`none` message mode to get the same decision as `/keys`, but `authorizeAgentMessage`
would deny it first; AK-25's message-only caller would get `authorizeAgentMessage`'s own denial
code instead of the `/keys`-equivalent one; and decision 5's "message modes do not grant or deny
keys" would be violated for the bridge specifically.

**Required invariant.** Neither `AgentActionMessage` branch reads the request body today:
verified at the same two anchors above, and confirmed as the *only* body read on either route being
`readJSON(r, &req)` inside `handleAgentMessage` itself (`handlers_agent_messaging.go:1430`, which
has no size limit of its own — `readJSON`, `server.go:5312`, is a bare `json.NewDecoder(r.Body).
Decode(v)`). Checked further: no generic body-size middleware wraps this route either — neither
`http.Server` (`server.go:4459`-4464, which configures only timeouts and the handler chain) nor
`applyMiddleware` (`server.go:5067`) applies a `MaxBytesReader`; every `http.MaxBytesReader` call in
`pkg/hub` is a specific handler's own opt-in (e.g. `handlers_agent_message_mode.go:90`,
`handlers_env_secrets.go:703`), and `handleAgentMessage` is not one of them. **The message path has
no request-body size limit at all today.** Both
`messages.MaxMsgSize`/`MaxMessageLength` bound only the *decoded* `msg` field after JSON parsing
(`pkg/messages/types.go:174`-179), not the HTTP body — a `msg` at that limit, JSON-escaped, can be
far larger on the wire (16000 non-ASCII runes can encode to well over 96 KB of body), so neither
constant is a valid body cap.

**Task 2.3 therefore adds a new body read ahead of `authorizeAgentMessage`, for every message
request regardless of whether it turns out to be raw** — raw-vs-not is unknown until that body is
decoded. This is new work the bridge introduces, not existing capability it reuses, and it must
satisfy five requirements so it cannot regress Plain (AK-37) or ordinary messages, and cannot let a
raw request reach legacy delivery unauthorized:

- **Bound — a new, disclosed limit, not a reused one, sized from an honest worst case.** Since no
  limit exists today, 2.3 introduces one for this read specifically: **2 MiB (2,097,152 bytes)**.
  Redone field by field, with every *bounded* field escaped to its worst case:
  `structured_message.metadata` (`MaxMetadataEntries` × (`MaxMetadataKeySize` +
  `MaxMetadataValueSize`), each byte control-escaped to `\u00XX`, 6 bytes per source byte) =
  32 × (256×6 + 4096×6) = 835,584 bytes; `msg`/`message` at `MaxMessageLength` (16000 runes), all
  non-BMP and surrogate-pair-escaped (12 bytes/rune) = 192,000 bytes (64,000 UTF-8 bytes, within
  `MaxMsgSize`). Combined: **1,027,584 bytes** — about 21 KB under a 1 MiB bound, which is too thin
  a margin to be a serious bound; 2 MiB leaves roughly 1 MiB of headroom over this bounded-field
  worst case instead, and matches the generic body-size convention already used elsewhere in this
  codebase for JSON request bodies of unbounded shape (`handlers_chat_v2.go:439`,
  `handlers_chat_prefs.go:96`, both `http.MaxBytesReader(w, r.Body, 1048576)`, doubled for margin),
  plus this codebase's existing large-body precedent for a single request
  (`handlers_chat_v2.go:4639`'s attachment-upload `MaxBytesReader`, sized in the tens of megabytes).

  **This bound cannot be, and is not claimed to be, a provable ceiling over every body `main`
  accepts and delivers today.** `messages.StructuredMessage.Validate()` (`pkg/messages/types.go:166`
  -215) checks only `msg` (length), `type` (enum), sender/recipient *non-emptiness*, attachment
  *count*, `metadata` (entry count plus per-entry key/value size), and `channel` (a short,
  alphanumeric-plus-hyphen field). Every other field `readJSON` decodes has no length or count limit
  of its own, independent of whatever cap 2.3 picks: `structured_message`'s
  `sender`/`sender_id`/`recipient`/`recipient_id`/`recipients`/`thread_id`/`conversation_id`/
  `delivery_text`/`status`/`timestamp`; the top-level `surface`/`external_ref`/`parent_ref`; the
  top-level `message` field when `structured_message` is also present (`handleAgentMessage` never
  reads or validates it in that case, `handlers_agent_messaging.go:1440`-1442 — but `readJSON` still
  decodes it, so its bytes count against body size regardless of being unused); `structured_message.
  attachments` element strings (the *count* is capped at `MaxAttachments` = 10,
  `messages/types.go:189`, but no per-string length limit exists); top-level `mentions` (count and
  element length unbounded on the wire — truncated to `MaxMentionRecipients` = 10 only after decode,
  `handlers_agent_messaging.go:1541`-1542, so this reads as capped but is not, on the wire, where body
  size is measured); duplicate `metadata` members (`encoding/json` collapses repeated keys into one
  map entry, so a body can carry any number of duplicate `metadata` members while the decoded map
  still passes the 32-entry check); any unknown JSON field (`readJSON`'s `json.NewDecoder` has no
  `DisallowUnknownFields`, `server.go:5312`); and insignificant whitespace. A body that pads any of
  these can exceed 2 MiB, or any other finite cap, while still nominally satisfying every check
  `messages.StructuredMessage.Validate()` enforces — padding an unbounded field is by definition a
  way to construct one. **2 MiB is sized to comfortably clear the
  worst case of every field that has a limit; it is not a bound no legitimate body can ever reach,**
  and this contract does not claim otherwise. Applying this bound is a **new, intentionally-
  introduced behaviour change**, not a preservation of an existing one; it is recorded as such in
  §6.4 below, next to §6.2/§6.3, with its own AK row (AK-52), and the unbounded fields above are
  named so 3.2's migration note can list them. It is **not** `agentkeys.MaxHTTPBodyBytes` (32 KiB) —
  that ceiling is keys' own, far tighter, and applying it here would 413 legitimate large Plain
  messages purely because the bridge had to look at the body first. `MaxHTTPBodyBytes` applies only
  once raw has actually been selected (bridge step 4 below), never to this earlier read.
- **Over-cap outcome.** A body exceeding the 2 MiB bound is rejected at this read, before
  authorization, with a generic (non-keys) 413 in the existing Hub error envelope. A body built only
  from fields that have a limit today cannot come within roughly 1 MiB of this bound (see above), so
  the 413 is unambiguous for realistic traffic; a body that deliberately pads one of the unbounded
  fields listed above past 2 MiB is rejected too — that is this bound's intended, disclosed effect
  on such a body, not an edge case it fails to handle.
- **Re-supply.** A request that turns out not to be raw must reach `handleAgentMessage` with a
  byte-identical body — the new read must not consume `r.Body` destructively without restoring it
  (e.g. read into a buffer, then reset `r.Body` to a fresh reader over that buffer before falling
  through), so `readJSON` inside `handleAgentMessage` sees exactly what it would have seen today.
- **Classification parity: raw selection must be computed by decoding the identical bytes with the
  identical mechanism `handleAgentMessage` uses, not a custom or partial parser.** Concretely: the
  new read decodes the buffered body with `encoding/json`'s `Decoder.Decode` into the same
  `MessageRequest` type `readJSON` decodes into (not a hand-written scanner, not a
  streaming/bounded partial read), and derives raw selection with the same
  OR-of-top-level-and-nested-`raw` rule already established above. Because it is the same decoder,
  the same type, and the same bytes, case-variant field names (`encoding/json` matches
  case-insensitively), duplicate scalar keys (last one wins), duplicate object-valued keys (fields
  are merged into the earlier occurrence, not replaced outright — verified:
  `{"structured_message":{"raw":true},"structured_message":{"msg":"x"}}` decodes to
  `StructuredMessage{Msg:"x", Raw:true}`, keeping both fields, not just the second occurrence's),
  and trailing bytes (`Decoder.Decode` stops after the first JSON value and ignores what follows)
  all classify **identically** to whatever `readJSON` would later compute for that same body — by
  construction, not by a separate claim that needs re-verifying every time either decoder changes.
  **`Decode`'s error return governs classification exactly as it governs `readJSON`'s: any non-nil
  error means not-raw, full stop, regardless of which fields the decoder already populated before
  reaching it.** `Decoder.Decode` continues past a field-level `UnmarshalTypeError` and populates
  every field it reaches before the error — verified: `{"raw":true,"message":"hi","interrupt":"yes"}`
  (`interrupt` expects a bool) returns a non-nil error with `Raw == true` and `Message == "hi"`
  already set. Such a body must not be read as "raw selected" merely because `Raw` ended up `true`
  in the partially-populated struct: it is exactly the class of body `readJSON` also fails to parse
  later (same bytes, same type, same error), so it must fall through and produce that same eventual
  400 in the same position (post-authorization) as it does today, not a new pre-authorization one
  (AK-59) — see the fail-closed invariant below for why this is safe even in principle, not only in
  the common case.
- **Fail closed, from 2.3 onward — two guarantees from two different layers, because neither layer
  alone can deliver both.** Classification parity makes disagreement between the two decodes
  impossible by construction, but 2.3 must not rely on that alone, and must not stop at guarding only
  the one handler discussed most in this section.

  **(a) Dispatch-layer backstop: no delivery, ever — but not zero side effects, because side effects
  already happened by the time this layer runs.** The enforcement point is every
  `AgentDispatcher.DispatchAgentMessage` implementation — today exactly one,
  `HTTPAgentDispatcher.DispatchAgentMessage` (`httpdispatcher.go:2726`) — not
  `dispatchWithBrokerRetry`: verified on `main`, `dispatchWithBrokerRetry` (`broker_routing.go:148`)
  is the caller most call sites use, but four production call sites invoke
  `AgentDispatcher.DispatchAgentMessage` directly and never pass through it —
  `messagebroker.go:1046`'s `publishDeliveryFailed`, `messagebroker.go:1118`'s
  `publishDeliveryDeferred`, `handlers_agent_messaging.go:3225`'s `publishBroadcastDeliveryFailed`,
  and `reconcile.go:346`'s `deliverMessage` (all four build a Hub-constructed message with `Raw`
  false, or pass `nil`, so nothing is reachable through them today — but a guarantee stated at
  `dispatchWithBrokerRetry` would not cover them if that ever changed). The interface method
  itself, not any one caller of it, is what every path — direct
  dispatch (`handleAgentMessage`, `ExecuteAgentDM`, `handleGroupMessage`, `broadcastDirect`,
  `processMentions`, native web chat, notification/subscriber delivery, scheduled-message delivery)
  and every broker-proxy-published message alike (`deliverToAgent`, `messagebroker.go:692`, reached
  by `PublishMessage`/`PublishBroadcast`/`PublishToGroup`/`fanOutToProject`/`fanOutGlobal`) — shares.
  From 2.3 onward, every implementation of this method must refuse to dispatch — not merely go
  unreached by an accurate bridge — any call whose `structuredMsg` carries `Raw == true`: **zero
  broker calls, never `mgr.MessageRaw`**, regardless of which of the call sites above reached it or
  whether a new one is added later, since the guarantee lives in the implementation every one of them
  already calls through the same interface. What this layer cannot do is undo work its caller already
  did before calling it: on every path above, persistence (`store.CreateMessage`) and any SSE/observer
  publish (`PublishUserMessage` or equivalent) already happened before dispatch runs (verified, e.g.
  `handlers_agent_messaging.go:2254`/`:2266` before `:2351`; `agent_dm_operation.go:483`/`:515` before
  `:553`; `messagebroker.go`'s persistence before `:924`) — a function called after those side effects
  cannot make them zero. This layer therefore requires, instead: where a `store.Message` row was
  already persisted, mark it failed through the same existing failure path every other dispatch error
  already uses at that call site (e.g. `MarkMessageFailed`) — no new code path, since a rejection here
  is just an ordinary dispatch error to that caller. Where the caller is synchronous and holds an HTTP
  response (`handleAgentMessage`, `ExecuteAgentDM`'s originating request), it returns a generic,
  non-keys-specific failure the same way it already handles any other dispatch error. Where the
  caller is asynchronous and has no HTTP response to write (broker-proxy subscription delivery,
  `notifications.go:407`, the notification sweep `notification_sweep.go:233`, scheduled delivery
  `server.go:3585`), it logs a content-free, distinct defect signal only — there is no envelope to
  answer. Log the attempt (content-free, per §5's audit rules) distinctly from an ordinary validation
  or authorization denial in all cases, since it signals an implementation defect in the
  classification-parity requirement above, not a normal caller error: a correct bridge never lets
  `Raw == true` reach this layer at all (AK-55).

  **(b) Zero side effects: only ingress-level rejection, before persistence, can give this.** The
  single-agent route's own pre-authorization branch (this section) is one source; the other sources
  are ptone/scion#2218's (task 0.2's) ingress guards, described next. §10 already sequences 2.3 after
  0.2; this is why: 0.2 merging is a **normative prerequisite of 2.3**, not optional
  defence-in-depth layered on top of (a) — (a) alone cannot give zero side effects for any ingress
  other than this section's, so 2.3 depends on 0.2 having already closed the rest.

  This closes the gap even against a future implementation defect in the classification-parity
  requirement above: no body can reach legacy raw delivery without first passing
  `authorizeAgentKeys`, full stop, regardless of how it got past the bridge's own detection or which
  ingress it entered through — (a) guarantees that much unconditionally; (b) is what additionally
  guarantees no trace of the attempt (a persisted row, an SSE event) either. See §8's correction
  below, which this same requirement fixes a self-contradiction in.

  **Ingress inventory (the sources for (b), beyond this section's own route).** ptone/scion#2218
  (task 0.2, open as of this writing — its merge is what makes (b) hold for these ingresses, per the
  prerequisite above) closes every ingress other than the still-supported single-agent path: project
  broadcast (`handleProjectBroadcast`) will reject `structured_message.raw` unconditionally
  immediately after decode (AK-56); both broker/plugin-inbound routes
  (`handlers_broker_inbound.go`, `handlers_broker_inbound_routed.go`) will reject `message.raw`
  unconditionally before sender resolution (AK-57); one-shot scheduled events and recurring
  schedules will tombstone a `"raw"` key, any value including `false`, in the advanced Payload JSON
  with 422 (AK-58); and raw against a managed-runtime target will be rejected at both the HTTP layer
  and `ExecuteAgentDM`. The only ingress 0.2 leaves open is exactly the one this section already
  governs — `handleAgentMessage`'s single-recipient path and the agent-DM operation it forwards to
  — because that is the one legacy raw shape decision 4 keeps alive until 2.3's cutover. 2.3's job is
  to close that last ingress at the point this section specifies, the same way 0.2 closes the others,
  not to re-guard each ingress separately a second time — and 2.3 must not ship ahead of 0.2 merging,
  per the prerequisite above.

Once raw selection is determined this way: if raw is selected, the request never reaches
`authorizeAgentMessage` or `handleAgentMessage` at all — it proceeds through the bridge's own steps
2-9 below, ending in `authorizeAgentKeys`/`ExecuteAgentKeys` exclusively. If raw is not selected (an
ordinary or `Plain` request, or a body the new read could not parse), the existing path —
`authorizeAgentMessage` then `handleAgentMessage`, body re-supplied unchanged — continues completely
unaffected (AK-37, AK-50, AK-52), and can never subsequently deliver as raw (AK-53/AK-54/AK-59).
2.1/2.3 choose how the branch itself is written; this invariant is what any implementation must
produce.

**Keys-content parity, closing a second, narrower gap in accepted-content fidelity, not
authorization.** Classification parity above governs the raw *selection* boolean only. The
`msg`/`message` *content* itself must not be taken from the lenient `encoding/json` decode used for
that classification: `encoding/json` silently replaces invalid UTF-8 and lone `\uD800`-`\uDFFF`
surrogate escapes with U+FFFD (`pkg/agentkeys/validate.go`'s own doc comment explains why this
matters), so a naive implementation that reused the classification decode's already-decoded string
would accept `{"raw":true,"message":"\ud800"}` as U+FFFD on the bridge while the identical content
is a 400 on T/P (`{"keys":"\ud800"}`).

Extraction must not be a hand-written byte "re-location" against the buffered body — that has no
single correct answer once field names can be case-variant or duplicated. `encoding/json` matches
field names case-insensitively, lets a later duplicate scalar overwrite an earlier one, and
**merges** duplicate object values rather than replacing them (verified:
`{"structured_message":{"raw":true},"structured_message":{"msg":"x"}}` decodes to
`StructuredMessage{Msg:"x", Raw:true}`) — a hand-written locator applying any different rule picks a
different string than the classification decode did. Instead: **run a second `Decoder.Decode` of
the identical buffered bytes into a shadow struct whose JSON tags match
`MessageRequest`/`StructuredMessage` exactly** (a `json.RawMessage` field tagged `message`, and a
nested struct with a `json.RawMessage` field tagged `msg` under a `structured_message`-tagged
field), so field selection — case-insensitive matching, scalar overwrite, object merge — is
identical to the classification decode by construction, the same guarantee classification parity
already gives raw selection itself.

Once raw is selected (step 1), each of the two resulting `json.RawMessage` values is one of: absent
(the key never appeared in the body — no candidate from that source); a JSON `null` literal (treated
identically to absent); a non-string value (400 `invalid_request`); or a JSON string, decoded with
`agentkeys.ValidateKeysJSON` (added to the frozen package by this task — §7 — applying the same
`decodeJSONString`/`decodeUnicodeEscape` pair `ValidateBody` uses, plus the same field-level rules
`ValidateKeys` applies, in one call). **Both present-and-non-null candidates must be decoded
unconditionally, in either order:** a decode failure on either one is 400 `invalid_request`
regardless of whether the other candidate would have resolved cleanly, so the same input never
produces two different codes depending on implementation-specific evaluation order (AK-60). If both
decode successfully and their string values differ, that is step 3's conflict (AK-41), 400
`invalid_request`; if they agree, or only one candidate is present, that value is
`agentkeys.Request.Keys` — no further `ValidateKeys` call is needed, since `ValidateKeysJSON` already
applied those rules. This is how B rejects exactly the same invalid-UTF-8/lone-surrogate content T/P
do, and how a case-variant or duplicated `structured_message` key never produces a different keys
value than the classification decode used to select raw in the first place.

**Bridge check order, fixed so no input can produce two different codes depending on
implementation-specific ordering:**

1. Determine raw selection via the shadow-decode above (OR of top-level/nested `raw`, any non-nil
   `Decode` error meaning not-raw regardless of already-populated fields); if not selected (including
   an unparseable body), stop here and fall through to the unchanged message path with the body
   re-supplied unchanged.
2. Reject a `raw`+`plain` conflict — 400 `invalid_request`, no operation ID.
3. Strictly decode both the `message` and `structured_message.msg` candidates (via
   `agentkeys.ValidateKeysJSON`, treating an absent or JSON-`null` candidate as no candidate from
   that source) and reject a conflict between two present, non-null, differing values — 400
   `invalid_request`, no operation ID. A decode failure on either present candidate is 400
   `invalid_request` here too, regardless of the other candidate (Keys-content parity above).
4. The resulting value (whichever candidate was present, or either if they agreed) is
   `agentkeys.Request.Keys` directly — `ValidateKeysJSON` already applied `ValidateKeys`'s
   field-level rules, so a size violation surfaces here as 413 `payload_too_large`, no operation ID.
   (`agentkeys.MaxBytes`/`MaxHTTPBodyBytes` apply from here on, not to step 1's read.)
5. **Mint the operation ID** — immediately once (1)-(4) pass, matching §2.5's general policy
   exactly ("minted as soon as validation succeeds, before authorization and before any resolution
   outcome is reported"). Every step from here on carries it.
6. **Agent cross-project refusal.** If the sender is an authenticated agent identity and its
   project differs from the already-resolved target's project (see the no-separate-resolution note
   below), reject with 422 `cross_project_keys_unsupported`, carrying the operation ID. This is the
   bridge's source for AK-21d, and it mirrors §3 invariant 4 exactly: a distinct check, decided
   before any later authorization-adjacent step, not folded into or superseded by one.
7. Evaluate every other "Rejected" row below (including the `recipient`/`recipient_id` rule) — 422
   `raw_combination_unsupported` on the first one that fails, carrying the operation ID.
8. `authorizeAgentKeys` — never `authorizeAgentMessage`, per the branch-point invariant above;
   `keys_denied` on failure, carrying the operation ID.
9. `ExecuteAgentKeys` / dispatch.

**Unambiguous precedence:** step 6 (cross-project) is strictly before step 7
(unsupported-combination). An input that would trigger both — e.g. a cross-project agent sender
with `raw:true` and `wake:true` — always yields 422 `cross_project_keys_unsupported`, never
`raw_combination_unsupported`. This is the same precedence §3 already gives the direct `/keys`
routes (invariant 4 before invariant 5's `authorizeAgentKeys`), stated here so the bridge order's
own heading ("no input can produce two different codes") is actually true.

**No separate bridge resolution step exists, and there is no bridge-specific `not_found` case.**
The `AgentActionMessage` branch this bridge lives inside (per the branch-point invariant above) is
only reached with an *already-resolved* target agent: on T, `handlers_agents_core.go:2960`
calls `GetAgent(id)` inside that same branch, before `authorizeAgentMessage`; on P, the target is
resolved even earlier, by the shared agent-resolution block (`handlers_projects_core.go:2422`-2443)
that runs for every action on the route before any action-specific code, including this branch. A
resolution failure on either route is answered by that pre-existing, non-keys code — today's
ordinary (non-`agent_not_found`-shaped on T; `agent_not_found`-shaped on P) response — before the
bridge, the new pre-authorization read, or raw-detection ever run, exactly as it already is for
every ordinary and `Plain` message today. This is not a new gap keys introduces: the bridge reuses
whichever agent value the surrounding message-dispatch code already resolved for step 6's
cross-project comparison and step 7's recipient-rule comparison, and never needs, and never
produces, its own `not_found` outcome. (Contrast the direct `/keys` routes, §3, where 2.1 *is*
introducing new code ahead of any existing resolution and so must own that outcome in the keys
envelope — the bridge has no equivalent gap to close because it doesn't introduce a new resolution
path at all.) On the project-scoped route specifically, this also means a bridged raw request *does*
have target existence queried (by the pre-existing shared resolution block) before the cross-project
decision — unlike AK-21c, which is scoped to the direct `/keys` route only. That is today's existing
message-path disclosure, not a new one, and this contract does not ask 2.3 to meet AK-21c on the
bridge.

Malformed-input checks (steps 2-4, all 400/413, pre-operation-ID) always precede both the
operation-ID mint (step 5) and step 6's cross-project check and step 7's
semantic-unsupported-combination check, and everything after them (steps 6-9, all
operation-ID-bearing) — the same "structural validation before authorization-adjacent concerns"
ordering the direct `/keys` routes already use (`ValidateBody` before anything else). Concretely: an
empty `msg` combined with a nonempty unsupported field (e.g. `wake:true`) is 400 (step 4, no
operation ID), not 422 — the malformed body is caught before the bridge ever looks at `wake` or
mints an ID.

Source column, checked against `main`: `MessageRequest` has
exactly 11 JSON fields — `message`, `structured_message`, `raw`, `plain`, `interrupt`, `notify`,
`wake`, `mentions`, `surface`, `external_ref`, `parent_ref` — all top-level ("request" below).
Every other field in this table (`sender`, `sender_id`, `recipient`, `recipient_id`, `recipients`,
`version`, `timestamp`, `type`, `urgent`, `broadcasted`, `observer_only`, `attachments`,
`metadata`, `channel`, `thread_id`, `conversation_id`, `delivery_text`, `status`) exists **only**
nested inside `structured_message`, never as a top-level `MessageRequest` field. In particular
`urgent` has no top-level counterpart at
all (`MessageRequest` has no `Urgent` field), and the top-level `delivery_text` that does exist in
this codebase (`broker_http_transport.go:345`) is a Hub→broker wire field on a completely different
hop (§4), not something an inbound `/message` client request can set at all.

| Field | Source | Bridge treatment | Outcome / HTTP | Notes |
| --- | --- | --- | --- | --- |
| `raw` | request | **Accepted** — selects the bridge | — | OR'd with nested `raw` (§8); `true`+`false` still means raw |
| `structured_message.raw` | nested | **Accepted** — selects the bridge | — | Same OR rule |
| `message` | request | **Accepted** — becomes `agentkeys.Request.Keys` | 400 `invalid_request` if it conflicts | Conflicting nonempty legacy `message` and nested `msg` (AK-41): **rejected**, not merged |
| `structured_message.msg` | nested | **Accepted** — becomes `agentkeys.Request.Keys` | 400 `invalid_request` if it conflicts | Same conflict rule as `message` |
| `plain` / `structured_message.plain` | request / nested | **Rejected if true together with raw** | 400 `invalid_request` (AK-42) | `plain` alone (no raw) is ordinary messaging, untouched (AK-37) — this row only fires when `raw` is also true |
| `structured_message.sender`, `.sender_id` | nested | **Ignored** | — | Never authoritative; actor always comes from auth context |
| `structured_message.recipient`, `.recipient_id` | nested | **Ignored unless non-empty and not a match** (exact comparison rule below) | 422 `raw_combination_unsupported` on a non-match | Never authoritative — actor/target always come from auth context + URL — but a non-matching value is a caller error to reject, not silently discard, tightening today's silent-ignore behavior for this one path |
| `structured_message.recipients` | nested | **Rejected if non-empty** | 422 `raw_combination_unsupported` (AK-43) | Implies fan-out; keys is a single-target operation |
| `structured_message.version`, `.timestamp`, `.type` | nested | **Ignored** | — | CLI-generated incidental fields must not block the keys caller (AK-44) |
| `interrupt` | request | **Rejected if true** | 422 `raw_combination_unsupported` (AK-43) | Interrupt is a harness-level messaging action with its own authorized path (`message --interrupt`); combining it with raw is out of scope here. Absent/false: ignored |
| `notify` | request | **Rejected if true** | 422 `raw_combination_unsupported` (AK-43) | No notification subscription on the keys path |
| `wake` | request | **Rejected if true** | 422 `raw_combination_unsupported` (AK-43) | No wake/start on the keys path |
| `mentions` | request | **Rejected if non-empty** | 422 `raw_combination_unsupported` (AK-43) | No mention fan-out |
| `surface`, `external_ref`, `parent_ref` | request | **Rejected if set** | 422 `raw_combination_unsupported` (AK-43) | Implies conversation resolution, which keys skips entirely |
| `structured_message.urgent` | nested | **Ignored** | — | No priority-queue concept for synchronous injection; no top-level equivalent exists |
| `structured_message.broadcasted`, `.observer_only` | nested | **Rejected if true** | 422 `raw_combination_unsupported` (AK-43) | Implies fan-out/observer delivery semantics |
| `structured_message.attachments` | nested | **Rejected if non-empty** | 422 `raw_combination_unsupported` (AK-43) | No attachment ingestion on the keys path (stronger than the existing cross-project-only attachment rule) |
| `structured_message.metadata` | nested | **Rejected if non-empty** | 422 `raw_combination_unsupported` (AK-43) | No arbitrary metadata may override the keys body |
| `structured_message.channel`, `.thread_id`, `.conversation_id` | nested | **Rejected if set** | 422 `raw_combination_unsupported` (AK-43) | Implies conversation/channel addressing |
| `structured_message.delivery_text` | nested | **Rejected if set** | 422 `raw_combination_unsupported` (AK-43) | Hub-internal derived field; already documented as never accepted from client JSON on the non-raw path — the bridge must enforce that for the raw path too, not assume it is unreachable there |
| `structured_message.status` | nested | **Ignored** | — | Not meaningful on an inbound request |

Every 422 `raw_combination_unsupported` rejection above affects zero targets and happens before any
message side effect, per #2184's "unsupported delivery semantics" text; every 400
`invalid_request` rejection is a malformed/conflicting-input case per #2184's "raw+plain" text —
see `OutcomeRawCombinationUnsupported`'s doc comment (`pkg/agentkeys/types.go`) for why the split
is exactly there and not elsewhere.

**`recipient`/`recipient_id` comparison, specified exactly** — a bare "not naming the URL's target"
rule is not precise enough to implement without risk of rejecting legitimate legacy callers:
`cmd/keys.go:117` sends `"agent:"+agentName` using the name exactly as the user typed it, not
necessarily its canonical slug; `cmd/message.go:723,837` send `"agent:"+ref.Value` the same way; and
the project-scoped route resolves by slug and falls back to the Hub UUID,
`handlers_projects_core.go:2423`-2427 — so a UUID-addressed or non-canonical-name-addressed legacy
caller must not be rejected:

- Strip an optional leading `agent:` prefix from `recipient` before comparing.
- `recipient` matches if the resulting value equals any of: the raw URL path segment (whatever the
  client sent as `{id}`/`{id-or-slug}`, unmodified), the resolved agent's slug, the resolved
  agent's UUID, or `api.Slugify(value)` compared against the resolved slug (covers a legacy caller
  that sent a display name rather than the canonical slug).
- `recipient_id` matches only if it equals the resolved agent's UUID exactly — it has no slug or
  display-name form to be lenient about.
- Any `user:` recipient, a group/broadcast form (`messages.IsGroupRecipient`), or any value that
  fails every comparison above → 422 `raw_combination_unsupported`, the same as any other
  unsupported-routing field.

Acceptance coverage: AK-48 (bridge, `recipient` addresses the target by its raw UUID rather than
slug → 200, not rejected) and AK-49 (bridge, `recipient` addresses the target by a
non-canonical/non-slugified name the client typed, matching via `api.Slugify` → 200) join AK-43's
existing "non-matching recipient → 422" case — see §6.

Retired-field rejection (AK-35/AK-36) is a **Phase 4** behaviour (task 4.2); this table freezes
what 2.3 must implement during the bridge and what 4.2 must convert into hard tombstones.

### 6.2 Bridge size behaviour change (recorded for 2.3/3.2)

Today, legacy raw messages are bounded by `messages.MaxMessageLength` (16000 characters) and
`messages.MaxMsgSize` (64 KiB) — `pkg/messages/types.go`'s `Validate()`. Once the bridge routes
through `ExecuteAgentKeys`, the same content is bounded by `agentkeys.MaxBytes` (4096 bytes)
instead: a legacy raw caller sending, say, 8000 bytes of literal keystrokes succeeds today and gets
413 `payload_too_large` after 2.3 ships. This is an intentional consequence of decision 1 (keys
gets its own, tighter, terminal-input-appropriate ceiling — 4096 bytes is already generous for
keystroke injection, unlike a message body), not an oversight, but it is a real behaviour change
for any existing caller sending long raw payloads. Task 2.3 must surface this in its own PR
(expect it, do not treat a new wave of 413s as a regression), and task 3.2 must document it in the
migration note alongside the raw/plain spelling guidance.

### 6.3 Bridge authorization tightening (recorded for 2.3/3.2)

Once the bridge routes every raw request through `authorizeAgentKeys` (§6.1 step 8) instead of
`authorizeAgentMessage`, the authority required to send `message --raw` changes for callers who
today rely on message-mode authorization rather than attach authority — this follows directly from
adopted decision 2, but is a real, rollout-visible behaviour change worth recording alongside §6.2's
size reduction, not just a logical consequence to leave implicit.

- **Agents.** Today, a same-project agent-to-agent raw DM is authorized by `authorizeAgentMessage`
  (message modes), not by any lifecycle scope. After 2.3, it requires `ScopeAgentLifecycle` (§3's
  agent-credential row). `ScopeAgentLifecycle` is granted only to the `full` agent role
  (`pkg/hub/agentrole.go:56`-66); the more restricted roles do not have it. Concretely: a
  non-`full`-role agent that can successfully send `message --raw` to a same-project agent today
  will get 403 `keys_denied` from the identical call once 2.3 ships, even though nothing about its
  role or the target changed.
- **Humans.** A human with message authority but not `ActionAttach` on the target sees the same
  shift: allowed today via `authorizeAgentMessage`, denied after 2.3 via `authorizeAgentKeys`.

This tightening is intended — decision 2 and §3's authorization table were never about preserving
message-mode-based raw access — but task 2.3 must surface it the same way §6.2 asks for the 413s
(expect it, do not treat the new 403s as a regression), and task 3.2 must document the authority
change in the migration note alongside the raw/plain spelling and size guidance.

### 6.4 New pre-authorization body-size cap (recorded for 2.3/3.2)

§6.1's new pre-authorization read (added ahead of `authorizeAgentMessage` on both routes to
determine raw selection) introduces a 2 MiB body-size bound where **no bound of any kind exists
today** — verified: `readJSON` (`server.go:5312`) is unbounded, and neither `http.Server`
(`server.go:4459`-4464, which configures only timeouts and the handler chain) nor `applyMiddleware`
(`server.go:5067`) applies a body limit to this route. This is therefore a new, disclosed behaviour
change, not a preserved limit, exactly like §6.2's size reduction and §6.3's authorization
tightening — with the caveat §6.1 states plainly: it bounds `msg`/`message` and `metadata` (the two
fields with a meaningful size limit) with roughly 1 MiB of margin over their combined worst case
(see §6.1's arithmetic), but it is not a bound that no legitimate
body can reach in principle, because every other field `readJSON` decodes — enumerated in full in
§6.1 (`structured_message`'s addressing/routing/status fields, the top-level conversation-resolution
fields, the unused top-level `message` field when `structured_message` is present, `attachments`
element length, `mentions` count and element length, duplicate `metadata` members, unknown JSON
fields, and whitespace) — has no limit of its own today and can be padded past any finite cap. Task
2.3 must surface this the same way §6.2/§6.3 ask for their own behaviour changes, and task 3.2 must
document it — including §6.1's unbounded-field list — in the migration note. See AK-52.

## 7. Go contract types: placement

New, cycle-free leaf package **`pkg/agentkeys`** (imports only the stdlib):

- `types.go` — `Request`, `Response`, `StatusDispatched`, the `Outcome` type and its 15 constants
  (including `OutcomeRawCombinationUnsupported`), `HTTPStatus`. Error responses reuse pkg/hub's
  existing envelope rather than a new type here — see §2.4a. Unit-tested in `types_test.go` (a
  table test plus a completeness check over every `Outcome` constant).
- `validate.go` — `ValidateBody` (raw-JSON, strict decode via token-stream structure plus
  `json.RawMessage` values so it can apply its own escape decoding: unknown fields, duplicate keys,
  non-string, missing, empty, NUL, size, invalid UTF-8, unpaired UTF-16 surrogate escapes),
  `ValidateKeys` (same field-level rules on an already-decoded string, for a caller that only has a
  lenient-decoded string left), `ValidateKeysJSON` (the bridge's sanctioned re-extraction entry
  point — §6.1 "Keys-content parity": checks UTF-8 validity and `json.Valid` on a `json.RawMessage`
  captured by a second `Decoder.Decode` of an already-buffered body — since `decodeJSONString` alone
  checks only surrounding quotes and recognized escapes, not full JSON validity — then strict-decodes
  it and applies `ValidateKeys`'s field-level rules, in one call, so it has neither ValidateKeys's
  lenient-decode gap nor a hand-written locator's field-selection ambiguity), `decodeJSONString`/
  `decodeUnicodeEscape` (the strict string/escape
  decoder all three functions share), `ValidationError`/`AsValidationError`. Error messages are
  fixed strings, never interpolated with client-supplied data. Unit-tested in `validate_test.go`:
  every simple escape, a real (not literal-UTF-8) surrogate-pair escape in both hex cases, the
  escaped-`\u0000` and unpaired-surrogate cases, a case-variant field name, an invalid escape,
  trailing non-JSON bytes, the multibyte size boundary, a differential test against `encoding/json`
  over randomly generated valid strings, and — for all three of `ValidateBody`, `ValidateKeys` and
  `ValidateKeysJSON` — a table asserting the exact `Outcome` and fixed message a distinctive secret
  produces on every failure path, never echoed back.
- `broker.go` — `BrokerRequest`/`BrokerResult` (JSON-tagged), `Target`, `BrokerRoutePath`,
  `BrokerRouteMethod`, `BrokerProjectIDQueryParam`, `DefaultAdmissionWindow`, `CapExecuteBefore`,
  `ErrMissingDeadline`, `ErrInvalidWindow`, the four sentinel errors split by layer
  (`ErrTargetNotFound`/`ErrAgentNotRunning`/`ErrTerminalNotReady` — manager-level, translated by the
  broker's own handler; `ErrNotDispatched` — Hub-side only), `BrokerOutcomeError` and
  `ValidBrokerOutcome` (the broker-decided-outcome channel — §4.3), and
  `ClassifyDispatchError`. Unit-tested in `broker_test.go`, including `BrokerOutcomeError` for
  every allow-listed outcome, a rejected (non-allow-listed) one, and the defensive fallback for all
  three manager-level sentinels (`ErrTargetNotFound`/`ErrAgentNotRunning`/`ErrTerminalNotReady`) if
  any ever reached `ClassifyDispatchError` directly — none should, in a correct implementation, and
  all three classify identically (`OutcomeKeysOutcomeUnknown`) if one does.
- `dispatcher.go` — `BrokerClient`, `Dispatcher` interfaces (declarations only; no
  implementation — that is 1.2's job), built around `Target` so no duplicate identity parameters
  exist between the two.
- `limits.go` — the four rate-limit constants (§5).

Also: `pkg/hub/authorize_agentkeys_test.go`, a test pinning the mapped **value**:
`agentActionPermission(api.AgentActionKeys) == ActionAttach`. It does not detect deletion of the
explicit case itself — today's case returns exactly what the default branch also returns, so
removing the case and letting `AgentActionKeys` fall through to default leaves this test green
(confirmed by a mutation check). What it does catch is the outcome that actually matters: either
branch's return value changing so `AgentActionKeys` stops mapping to `ActionAttach`.

**Why a new package instead of `pkg/api` or `pkg/hub`:** `pkg/api` already holds
`AgentActionKeys` (it must — every existing agent-action constant lives there and
`agentActionPermission` keys off that package), but is a much broader grab-bag; the DTOs, outcome
codes, broker contract and new interfaces are a self-contained unit that 1.1, 1.2, 2.1, 2.2, 2.3,
and eventually 3.1 (hubclient) all need, without needing anything else `pkg/api` carries. `pkg/hub`
and `pkg/runtimebroker` do not import each other today (verified: no non-test import either
direction) and must not start to; both already import `pkg/api` freely, so a second small leaf
package imports exactly as cleanly and keeps the keys contract from being buried inside either
side's package. The name `agentkeys` (not `keys`) is deliberate: `keys` is heavily overloaded
already in this codebase (`pkg/projectkeys`, `pkg/hub/oidckeys.go`, DM/conversation "keys" in
`pkg/messages/dm_key.go`) and a bare `pkg/keys` would be actively confusing next to those.

**What changed in existing packages (compile-only, no dispatch reachable):**

- `pkg/api/agent_actions.go`: added `AgentActionKeys = "keys"`. **Not** added to
  `RuntimeBrokerAgentActionMethod`'s switch, so `handleAgentAction`
  (`pkg/runtimebroker/handlers.go:1536`) still returns 404 for a `keys` action — there is no
  route yet.
- `pkg/hub/authorize.go`: `agentActionPermission` gained an explicit `case
  api.AgentActionKeys: return ActionAttach` (decision 2). This branch is not literally unreachable —
  a real `POST /api/v1/agents/{id}/keys` already reaches `agentActionPermission("keys")` through the
  generic `if !selfAccess` authz block (`handlers_agents_core.go:3023`-3029), because that block
  runs for *any* action string before the dispatch `switch`. What is unchanged is the **outcome** of
  that request: no `case api.AgentActionKeys` exists in either action-dispatch `switch`
  (`handlers_agents_core.go`/`handlers_projects_core.go`), so it still falls through to that
  `switch`'s default (no case matches) and the route responds 404, exactly as before this task. The
  accurate description is "reachable only through the generic authz block; dispatch still 404s".

**Deliberately not touched by this task:** `pkg/hub.AgentDispatcher`, `pkg/hub.RuntimeBrokerClient`
and their implementations (`HTTPAgentDispatcher`, `HTTPRuntimeBrokerClient`,
`ControlChannelBrokerClient`, `HybridBrokerClient`); `pkg/agent.Manager`/`AgentManager` (the
`MessageRaw`→`SendKeys` rename is task 1.1's, not this task's, to avoid touching a
widely-implemented interface without also shipping its real implementation); any HTTP mux
registration in `pkg/hub` or `pkg/runtimebroker`; `cmd/keys.go` (still builds a raw
`StructuredMessage` today — task 3.1's migration).

## 8. Reconciliation with GCP#2053

GCP#2053 ("honor top-level raw and plain flags on POST /api/v1/agents/{id}/message") is already
in the branch this contract is built on (`origin/main` @ `6585a8b`). Current behaviour, verified
by reading the code directly (not the PR title):

- `MessageRequest.Raw`/`MessageRequest.Plain` (top-level JSON fields,
  `handlers_agent_messaging.go:1395`-1401) are merged **onto** the nested structured message with
  OR semantics: `if req.Raw { structuredMsg.Raw = true }` / same for `Plain`
  (`handlers_agent_messaging.go:1443`-1448). When there is no nested structured message (the
  legacy plain-`message` path), `structuredMsg.Raw = req.Raw` directly
  (`handlers_agent_messaging.go:1515`-1516) — top-level is the only source in that path.
  This is exactly the "OR semantics... true+false still means raw" rule §2184 requires, already
  true on `main` today for the fields that exist; task 2.3 must preserve it, not reinvent it.
- Both public route shapes (`handlers_agents_core.go:3046`, `handlers_projects_core.go:2488`)
  delegate to the **same** `handleAgentMessage`, so top-level raw/plain *field* handling is already
  unified across routes — task 2.3 inherits that, it does not need to re-unify it. This is a fact
  about where `Raw`/`Plain` are merged onto the struct today, not about where 2.3's own bridge
  dispatch must live: `handleAgentMessage` is reached only after `authorizeAgentMessage` already
  ran and passed, which is too late for the bridge — see §6.1's branch-point invariant.
  `cmd/keys.go` itself still goes through this exact path today (`sendKeysViaHub`,
  `cmd/keys.go:96`-128): it builds a `Raw=true` structured message via `buildStructuredMessage`
  and calls `SendStructuredMessage`, confirming #2184's finding #1 is accurate on current `main`.
- Agent-to-agent DM raw delivery already has a cross-project refusal today, independent of this
  contract: `agent_dm_operation.go:357`-367 rejects `input.Raw &&
  input.SenderAgent.ProjectID != input.TargetAgent.ProjectID` with
  `MessageDenialCrossProjectRawUnsupported` (`"cross_project_raw_unsupported"`, HTTP 422). This
  predates and is **not** the same code as this contract's `cross_project_keys_unsupported`
  (§2.5). **Decided:** task 2.3 collapses these into one
  code, `cross_project_keys_unsupported`, at the moment it routes agent-to-agent raw DMs through
  `ExecuteAgentKeys` and removes this `agent_dm_operation.go` branch. Before 2.3 ships, the old
  code keeps governing unchanged — 0.1 does not touch `agent_dm_operation.go`, so there is no
  window where both codes are simultaneously live for the *same* code path; they are simply
  sequential (old code, then new code, at the 2.3 cutover), not two live codes to be reconciled
  later.
- GCP#2053 does not touch `cmd/keys.go`'s own hub path, PTY, local-mode dispatch, or
  `agent.Manager.MessageRaw`. Nothing about this contract's manager-primitive rename (1.1's job)
  or dedicated broker route (1.1/1.2's job) is affected by #2053 one way or the other — they are
  disjoint code paths today (the manager method is currently reached only from
  `pkg/runtimebroker/handlers.go:2157`'s `sendMessage`, which #2053 did not change) and remain
  disjoint under this contract through the 2.3 cutover: 2.3 stops the dispatch layer from
  *delivering* any request carrying `Raw == true` (§6.1's fail-closed requirement), but the message
  path's `Raw` branch code, the `Raw` struct fields, and the `message --raw` alias itself are not
  deleted until Phase 4 (task 4.2) has evidence
  to retire them — 2.3 disables the behaviour immediately; Phase 4 removes the vestigial code later.
- No deviation from #2184's substance is required by #2053 — the merged behaviour matches what
  #2184 assumed implementation would rebase onto. The one thing worth recording precisely (done
  above) is exactly which struct fields and line ranges implement it, so 0.2/2.3 do not have to
  re-derive it from the PR title.

## 9. `.design/messaging-conversation-model.md` updates

Sections 2.8, 2.9, and the Appendix A "What changed and why" table described `scion keys` as
"local/terminal only" (`§2.9`'s table row, `"local-only"` in Appendix A) — written before this
design existed. Updated in this same change to point at this contract instead of asserting
local-only-ness; AC-15a (which already governs every deprecation warning's replacement, including
the eventual `--raw` deprecation warning this design's Phase 2/4 will add) gets a pointer note
rather than new text, since its existing verification rule already covers the future `message
--raw` warning without modification.

## 10. Ownership and sequencing (for coordination, not new scope)

Per #2184's suggested lanes: runtime/broker owner → Phase 1 (1.1 broker-side primitive + safe
logging fix in `pkg/runtime/common.go`; 1.2 dispatcher + all transport adapters, against
`agentkeys.Dispatcher`/`BrokerClient` frozen here); Hub authorization owner → 2.1
(`authorizeAgentKeys`, wiring the `AgentActionKeys` case into the dispatch switches this task
deliberately left absent) then 2.2 (`ExecuteAgentKeys`, limits, audit, consuming `agentkeys.
Dispatcher`); 2.3 follows 2.2 and 0.2 (owns the message-handler cutover using the field table in
§6.1); client owner → 3.1 (hubclient + CLI `keys`/alias, including the `cmd/keys.go` help-text fix
from §2.3); docs/inventory owner → 3.2. Phase 5 (relationship-authorization integration) stays
explicitly deferred pending #2119/#2120.

## 11. Retirement gate and master-body decision record (AC4)

#2191 AC4 requires "replacement reachability and deployed-client inventory, not elapsed time
alone" to govern retirement, with risks and decisions recorded in the master issue body (#2184).
This task does not edit #2184 (that is the lead's gate to apply on approval); the exact text below
is also delivered verbatim (byte-identical fenced block, checked by diff) to
`/scion-volumes/scratchpad/projects/agent-keys/reviews/0.1-master-body-update.md` for the lead to
paste in. The two copies must never disagree; if this section changes, that file changes with it
in the same commit. That file also carries a small "Part 1" fix not duplicated here, since it is
not part of the frozen Decisions text: #2184's "Status and outcome" intro paragraph currently calls
the decisions below "proposals for review, not previously approved decisions," which becomes stale
once they are marked Adopted — the file gives the lead the one-sentence replacement for that line
too.

```markdown
## Decisions

Adopted on approval of ptone/scion#2215. Publishing this plan was not prior user approval;
adoption follows PR approval, not the existence of a draft.

1. Adopt Option B, with a dedicated broker keys route in the core rather than an indefinite Raw StructuredMessage transport adapter. This adds transport work but makes the end state real and eliminates the message logger from the keys path.
2. Add `api.AgentActionKeys` and one `authorizeAgentKeys` entry point. Initially map that operation explicitly to the existing attach permission and its credential ceilings: user ActionAttach; authenticated agents require lifecycle scope and the same project. A route action name is not a new independently granted authorization permission. Do not introduce a second permission/scope family just for the initial migration. Phase 5 integrates the shared relationship evaluator without silently broadening access.
3. Stop conversation persistence for all accepted raw requests as soon as the Hub bridge ships. Audit attempts and outcomes without input content. Existing historical rows remain; no ambiguous historical data rewrite.
4. Keep a short-lived message API bridge and hidden `message --raw` alias during the coordinated rollout, then remove both once replacement reachability and client/image migration are evidenced. No permanent alias, mandatory multi-release waiting period, or success-producing legacy fallback.
5. Document `/keys` as the supported REST API. Document both raw spellings only in the migration note. Plain is outside this removal. Optional quick-key UI, new SSE events, scheduled keys, key sequences and managed-runtime keys are deferred.

These choices were reviewed together in task 0.1; they did not require five separate conversational
approvals. An implementation owner records any changed decision here before changing dependent
contracts.

**Removal gate, restated verbatim here and matching "Cutover and deployment" below — both must
always say the same thing:**

> Removal gate: exercise the replacement for user and agent callers in local/Hub modes; record the
> versions deployed; migrate known scripts/docs/images; demonstrate no known callers still depend
> on raw via content-free counters plus explicit owner confirmation. Inventory whether old callers
> enable message retries, and upgrade them or disable those retries before bridge use. Low observed
> usage alone is not proof. Do not dual-send or shadow-execute keys. The temporary bridge has an
> owner and removal task from day one. If rollout fails, pause removal or roll back the coordinated
> component set; never restore message-based raw fallback inside new clients. No database rollback
> is required because there is no schema change.

Additionally, per #2191 AC4 (a requirement on top of, not part of, the gate text above): replacement
reachability and deployed-client inventory govern retirement — not elapsed time alone.

**Deviations and risks recorded in task 0.1 (ptone/scion#2215, `.design/agent-keys-contract.md`),
each specified in full there:**

- **Error responses reuse the existing Hub error envelope.** Keys does not introduce a new error
  shape: `{"error":{"code","message","details"}}`, the same envelope every other Hub route
  (including 401 from the shared auth middleware) already produces. `code` is the machine outcome
  string (e.g. `keys_denied`); `details.operation_id` is present only where noted next. See
  contract §2.4a.
- **OperationID policy.** An operation ID is minted inside the keys handler, immediately after
  request validation succeeds and before authorization runs — never before authentication, which
  always happens first via shared middleware outside any handler. A 401 (unauthenticated) therefore
  never reaches a handler and carries no operation ID. Every outcome from validation onward carries
  one: `keys_denied`, `not_found` (the keys-handler-produced one; see the project-resolution
  exception next), `agent_not_running`, `terminal_not_ready`, `cross_project_keys_unsupported`,
  `keys_unsupported`, `raw_input_removed`, `raw_combination_unsupported`, `keys_rate_limited`,
  `keys_unavailable`, `keys_outcome_unknown`, and `dispatched`. Only three carry none:
  `unauthorized` (never reaches a handler), `invalid_request`, and `payload_too_large` (both
  failures during validation itself). This extends #2184's "post-admission" floor to cover
  in-handler denials that happen before admission, because doing so costs nothing once the handler
  is running. See contract §2.5.
- **An unresolvable project on the project-scoped route is 404, not a keys-specific code, for
  either caller kind.** `POST /api/v1/projects/{project}/agents/{id}/keys` with a `{project}` that
  does not resolve to any real project returns the existing shared project resolver's ordinary 404
  `not_found` (`handleProjectAgents`, before any keys-specific code runs), identically for human and
  agent callers, carrying no operation ID — the same disclosure every other project-scoped action
  already has today. This is a plain 404, not the 422 `cross_project_keys_unsupported` that governs
  an agent crossing into a project that *does* exist. See contract §3.1.
- **Broker-side identity binding on the existing `agent_id` container label, enforced atomically in
  one call.** Every runtime-broker container already carries this label (`pkg/agent/run.go`,
  populated from the Hub dispatcher's `SCION_AGENT_ID`); nothing new needs to be provisioned. The
  frozen manager primitive takes the expected agent ID as a parameter and, within one call, resolves
  the target container, checks its `agent_id` label against that expected ID, and executes on that
  same resolved container — no separate check followed by a second, independent resolution, which
  would reopen the exact recreate-inside-the-window gap this binding exists to close. A resolution
  miss or identity mismatch is `not_found`; both fail closed rather than falling back to a
  slug-only match. Operators should expect any container whose `agent_id` label does not match the
  Hub's canonical ID for it — including containers started before `SCION_AGENT_ID` injection existed,
  or outside the Hub's own dispatch path — to receive `not_found` for keys until restarted; this is
  the intended fail-closed behavior, surfaced here because it is a rollout-visible behavior change.
  See contract §4.1 and §4.3.
- **Broker-decided outcomes have one channel back to the Hub.** A broker's own definitive decision
  (`not_found`, `agent_not_running`, `terminal_not_ready`, `keys_unsupported`, or an
  already-expired admission deadline it detects itself) travels as a single typed error the
  dispatch layer recognizes, restricted to that fixed allowlist. An old broker without the keys
  route at all is distinguished from a real "agent not found" by response shape (both currently
  answer HTTP 404) and is classified `keys_unsupported`, matching "old brokers return
  keys_unsupported" above. Any other malformed or disagreeing response classifies as an honest
  "outcome unknown" rather than being guessed at. See contract §4.3.
- **Cross-project refusal order and disclosure (agent-crossing-into-an-existing-foreign-project
  case).** The project-scoped route compares the URL project against the caller's own project
  before any agent-target lookup, so it never has to determine whether a same-slug agent exists
  elsewhere. The top-level route resolves the agent target first, the same as every other action on
  that route, then compares projects — matching the disclosure this codebase's existing
  lifecycle-action cross-project check already has today (a foreign existing agent and a
  nonexistent one already produce different outcomes there); keys only changes the status code used
  for the foreign case, not the disclosure. See contract §3.1.
- **Cross-project code unification.** The new `cross_project_keys_unsupported` code and the
  pre-existing message-path `cross_project_raw_unsupported` code are sequential, not simultaneous:
  the bridge task adopts the new code for agent-to-agent raw DMs at the same moment it removes the
  old code path. See contract §2.5 and §8.
- **New outcome code for the bridge's unsupported-combination rejections.** `raw_combination_unsupported`
  (422) covers a legacy envelope field that implies routing, fan-out, conversation, attachment, or
  lifecycle semantics the keys path does not support — this issue's "422 for unsupported delivery
  semantics" text. A raw+plain conflict, or a conflicting legacy `message` and nested `msg` body,
  is 400 `invalid_request` instead, matching this issue's "400 for raw+plain" text. See contract
  §6.1.
- **Bridge recipient-field tightening.** The bridge does not blindly ignore a legacy envelope's
  `recipient`/`recipient_id` field: a non-empty value that does not name the request's own target
  (matched against the raw URL segment, the resolved slug, the resolved UUID, or a slugified
  display name — not merely an `agent:<slug>` string match, so legacy callers addressing by UUID or
  by an unslugified name are not incorrectly rejected) is rejected rather than silently discarded.
  See contract §6.1.
- **Bridge size ceiling reduction.** Legacy raw messages are bounded today by 16000 characters /
  64 KiB; once the bridge routes through the new operation, the same content is bounded by 4096
  bytes instead. A legacy caller sending a longer raw payload succeeds today and will receive
  `payload_too_large` after the bridge ships — an intentional, disclosed behaviour change, not a
  regression to silently absorb. See contract §6.2.
- **Bridge authorization tightening.** Once the bridge routes raw requests through
  `authorizeAgentKeys` instead of `authorizeAgentMessage`, callers who relied on message-mode
  authorization rather than attach authority lose access: a non-`full`-role agent (only the `full`
  role has `ScopeAgentLifecycle`) that can send a same-project `message --raw` today gets 403
  `keys_denied` after 2.3 ships, and the same happens to a human with message authority but not
  `ActionAttach`. This follows from adopted decision 2 and is intentional, but is a real,
  rollout-visible authority change alongside the size reduction above. See contract §6.3.
- **Bridge body-size cap (new, disclosed).** The bridge's pre-authorization read (needed to
  determine raw selection before `authorizeAgentMessage` runs) introduces a 2 MiB body-size bound
  where no bound of any kind exists on the message path today. A body built only from `msg`/`message`
  and `metadata` at their worst case (the two fields with a meaningful size limit) cannot come within
  roughly 1 MiB of this bound. Every other field `readJSON` decodes — `structured_message`'s
  `sender`/`sender_id`/`recipient`/`recipient_id`/`recipients`/`thread_id`/`conversation_id`/
  `delivery_text`/`status`/`timestamp`, the top-level `surface`/`external_ref`/`parent_ref`, the
  unused top-level `message` field when `structured_message` is present, `structured_message.
  attachments` element length (count capped at 10, strings unbounded), `mentions` (count and
  element length unbounded on the wire, though truncated to 10 after decode), duplicate `metadata`
  members (collapsed into one map entry after decode, so any number can appear on the wire), unknown
  JSON fields, and whitespace — has no length or count limit of its own and can in principle be
  padded past the cap, so this is not a claim that no legitimate body can ever reach it — it is a new
  rejection (413) sized with a clear margin over every bounded field's worst case, where no rejection
  existed at all before, alongside the size reduction and authorization tightening above. See
  contract §6.4.
- **Bridge classification and keys-content parity.** Raw selection is computed by decoding the
  request body with the exact same mechanism (`encoding/json.Decoder.Decode` into the same
  `MessageRequest` type) the existing message handler uses, so case-variant field names, duplicate
  keys (including object-valued keys, which `encoding/json` merges rather than replaces), and
  trailing bytes classify identically on both sides by construction — not merely in the cases this
  contract anticipated — and a decode error means not-raw regardless of which fields were already
  populated when it occurred. A second, narrower requirement (keys-content parity) extends this from
  the raw-selection boolean to the extracted keys content itself: the bridge re-decodes the
  `message`/`structured_message.msg` candidate from the same buffered bytes with a new exported
  strict decoder (`agentkeys.ValidateKeysJSON`, added to the frozen package by this task), not with
  the lenient string already produced for classification, so invalid UTF-8 and lone surrogate escapes
  are rejected identically on both sides too.
- **Fail-closed delivery, split by layer.** From 2.3 onward, every `AgentDispatcher.DispatchAgentMessage`
  implementation (today exactly one) refuses to dispatch — never delivering via `mgr.MessageRaw` — any
  call whose message carries `Raw == true`, regardless of which Hub code path reached it; an
  already-persisted row is marked failed the same way any other dispatch error already is, and a
  synchronous caller's HTTP response reflects a generic failure. This layer alone cannot make an
  already-persisted row or SSE/observer event disappear, since those already happened before dispatch
  runs on every path — that guarantee (zero side effects) instead comes from rejecting at ingress,
  before persistence: this section's own bridge for the single-agent route, and ptone/scion#2218's
  (task 0.2's) guards for every other ingress, whose merge is accordingly a normative prerequisite of
  2.3, not optional defence in depth. Together, the dispatch-layer backstop and the ingress-level
  rejections ensure no request can reach legacy raw delivery without first passing the same
  authorization keys requires, regardless of which Hub ingress it arrives through. See contract §6.1.
- **`cmd/keys.go`'s current CLI help text is inaccurate about key-sequence behaviour** (it presents
  `scion keys my-agent "Up Up Enter"` as three key presses; on current `main` it types 11 literal
  characters instead, because the CLI joins its arguments into a single string before sending it).
  The client task must correct this text; it is not a new limitation introduced by this work. See
  contract §2.3.
```

## 12. Checks run for this task

- `gofmt -l` on every changed Go file: clean.
- `go build -p 2 ./pkg/agentkeys/... ./pkg/api/... ./pkg/hub/...`: clean.
- `go vet -p 2 ./pkg/agentkeys/... ./pkg/api/... ./pkg/hub/...`: clean (this also compiles every
  `_test.go` file in all three packages, including `pkg/hub`'s, so it is a real compile check on
  the full package even where the test *binary* is not fully run below).
- `go test -p 2 -count=1 ./pkg/agentkeys/...`: ok — covers `HTTPStatus` (table plus a
  completeness check over every `Outcome` constant), `CapExecuteBefore`,
  `ClassifyDispatchError` (including every `BrokerOutcomeError` case and the defensive fallback for
  all three manager-level sentinels — `ErrTargetNotFound`/`ErrAgentNotRunning`/`ErrTerminalNotReady`
  — if one ever reached it unwrapped), `ValidBrokerOutcome`, and the string/escape decoder (every
  simple escape, real surrogate-pair escapes in both hex cases, unpaired-surrogate rejection,
  invalid UTF-8, the NUL rule via both raw-byte and escaped paths, a case-variant field name,
  trailing non-JSON bytes, the multibyte size boundary, a 5000-iteration differential test against
  `encoding/json`, the worst-case body-ceiling arithmetic (`6*MaxBytes+len(minimal envelope) <=
  MaxHTTPBodyBytes`), a `ValidateKeysJSON` accept-path test (plain ASCII, a real *escaped* surrogate
  pair — `"\uD83D\uDE00"`, not literal multi-byte UTF-8 — and literal multi-byte UTF-8, each asserted
  equal to `ValidateBody`'s own output for the equivalent `{"keys":...}` body, not just to a
  hand-picked expected string), and three table tests — one each for `ValidateBody`, `ValidateKeys`
  and `ValidateKeysJSON` — that push a distinctive secret through every failure path (unknown field,
  non-string value, invalid escape, trailing garbage, invalid UTF-8, NUL, over-size, unpaired
  surrogate for `ValidateBody`; empty, NUL, over-size, invalid UTF-8 for `ValidateKeys`; non-string
  value, invalid escape, NUL, unpaired surrogate, over-size, invalid UTF-8, an unescaped control
  byte, and an unescaped interior quote for `ValidateKeysJSON` — the last two confirming empirically
  that `json.Valid` catches input that is not valid JSON at all before `decodeJSONString`, which
  checks only quotes and recognized escapes, would otherwise silently accept it) and assert both the
  exact `Outcome` and the exact fixed message per case, not just that an error occurred).
- `go test -p 2 -count=1 ./pkg/api/...`: ok.
- `go test -p 2 -count=1 -run TestAgentActionPermission_KeysMapsToAttach ./pkg/hub/`: ok — pins
  `agentActionPermission(api.AgentActionKeys) == ActionAttach`, the one production function this
  task changes.
- `go test -p 2 -count=1 ./pkg/hub/ -run 'TestAuthorize|TestAgentAction|TestAgentActionPermission'`:
  ok (104.5s) — the broader authorize/action-dispatch surface around that function.
- Full `go test -p 2 -count=1 ./pkg/hub/...`, run twice as the only heavy command at each time
  (`-timeout 10m` default, then explicit `-timeout 20m`): **both runs timed out** (at 602.989s and
  1208.785s respectively) via Go's own `panic: test timed out after N`, having gotten through the
  large majority of the package's tests serially in both cases without reaching the end. This
  reflects the aggregate size of `pkg/hub`'s test suite exceeding a 20-minute serial budget in this
  container, not a hang localized to one test: the second run's timeout fired while
  `TestHandleAgentMessage_BrokerTimeout504` had been running for only 28s, with hundreds of other
  tests already completed beforehand. **Two actual test failures** surfaced identically in both
  runs (not timeout-related): `TestBackupBinary` and `TestBackupAndRestoreRoundtrip`
  (`binary_update_executor_test.go`), both failing with "backup copy failed: exit status 1" —
  traced to `backupBinary` (`pkg/hub/maintenance_executors.go:1737`-1738), which shells out to
  `sudo cp`; `sudo` is not available/configured in this container. Both are pre-existing and
  unrelated to this diff: they exercise a binary self-update/backup feature nowhere near
  `pkg/api/agent_actions.go`, `pkg/hub/authorize.go`, or `pkg/agentkeys`, and fail on an external
  precondition (`sudo`) this task's changes do not touch. No other failures were observed in either
  run. This is disclosed as an environment limitation rather than treated as a passing gate or
  silently omitted.
- `make ci` was not run (reserved for staggered runs, and, given the above, likely to hit the same
  timeout characteristic).
