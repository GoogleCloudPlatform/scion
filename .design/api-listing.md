# Design Doc: Raise `scion list` Default Limit to 500

**Author:** api-listing-architect
**Date:** 2026-07-23
**Status:** Final

---

## Problem Statement

`scion list` silently truncates results at 50 agents with no warning, no pagination controls, and no `--limit` or `--count` flag. The truncation occurs at three layers: the agent store defaults to 50 (capped at 200), both HTTP handlers default to 50, and the CLI makes a single API call without setting a limit or reading the cursor from the response. Users with more than 50 agents see a truncated list with no indication that results are missing.

## Chosen Approach

**Raise default limits to 500** across agents and projects, add a `--count` CLI flag, fix the broken agent store cursor implementation, and surface a truncation warning when results are capped. This is a "raise the ceiling" fix, not a pagination UX change. 500 covers real-world use cases; cursor-based pagination infrastructure already exists end-to-end and will be fixed for correctness but not exposed to end users in this PR.

### Rationale
- Real-world agent counts rarely exceed a few hundred; 500 is effectively unlimited for current usage.
- Full cursor-based CLI pagination (looping, `--page-size`, etc.) adds UX complexity agents don't need today.
- Fixing the cursor bug is cheap (~20 lines) and ensures correctness if pagination is ever needed.
- Numeric `--offset` is impractical given the store's keyset pagination model.

---

## Exact Changes Per Layer

### Layer 1: Store Constants — `pkg/store/entadapter/agent_store.go`

**Lines 40-41:** Raise both constants:
```go
// Before:
const (
    defaultAgentListLimit = 50
    maxAgentListLimit     = 200
)

// After:
const (
    defaultAgentListLimit = 500
    maxAgentListLimit     = 500
)
```

**Why same value for both?** The user wants 500 to be the effective cap. If `default == max`, any request without an explicit limit gets 500, and no request can exceed 500. This is intentional — the max protects against pathological API calls, and 500 is the chosen ceiling.

### Layer 2: Agent Store Cursor Fix — `pkg/store/entadapter/agent_store.go`

**Lines 381-428 (`ListAgents`):** Add cursor consumption, modeled on the existing `projectCursorPredicate` in `project_store.go` (lines 471-486).

Changes:
1. Add a new `agentCursorPredicate` method that looks up the cursor agent's `created` timestamp and ID, then returns a keyset predicate: `created < cursor.created OR (created == cursor.created AND id < cursor.id)`.
2. In `ListAgents()`, after building the filter predicates but before the query, if `opts.Cursor != ""`, call `agentCursorPredicate` and append the predicate to the query.

The pattern is identical to `projectCursorPredicate`:
```go
func (s *AgentStore) agentCursorPredicate(ctx context.Context, cursor string) (predicate.Agent, error) {
    cursorUID, err := parseUUID(cursor)
    if err != nil {
        return nil, fmt.Errorf("invalid cursor: %w", err)
    }
    c, err := s.client.Agent.Get(ctx, cursorUID)
    if err != nil {
        return nil, fmt.Errorf("invalid cursor: %w", mapError(err))
    }
    return agent.Or(
        agent.CreatedLT(c.Created),
        agent.And(agent.CreatedEQ(c.Created), agent.IDLT(cursorUID)),
    ), nil
}
```

Then in `ListAgents`, insert after the filter predicates block:
```go
if opts.Cursor != "" {
    pred, err := s.agentCursorPredicate(ctx, opts.Cursor)
    if err != nil {
        return nil, err
    }
    query.Where(pred)
}
```

### Layer 3: Project Store Default — `pkg/store/entadapter/project_store.go`

**Line 432:** Raise inline default from 50 to 500:
```go
// Before:
if limit <= 0 {
    limit = 50
}

// After:
if limit <= 0 {
    limit = 500
}
```

**Note:** The project store does NOT have a `maxListLimit` constant (unlike the agent store). No max cap is added — this matches the existing pattern. If a caller passes `limit=1000`, the project store already allows it. The change here is only to the *default* when no limit is specified.

### Layer 4: HTTP Handler Defaults

#### `pkg/hub/handlers_agents_core.go` — Line 212
```go
// Before:
limit := 50

// After:
limit := 500
```

#### `pkg/hub/handlers_projects_core.go` — Line 1642 (project-scoped agent list)
```go
// Before:
limit := 50

// After:
limit := 500
```

#### `pkg/hub/handlers_projects_core.go` — Line 175 (project list)
```go
// Before:
limit := 50

// After:
limit := 500
```

**Why these three handlers?** User decision: raise limits for agents and projects in this round. Other entity handlers (users, groups, templates, etc.) remain at 50 and can be raised in a follow-up if needed.

### Layer 5: CLI `--count` Flag — `cmd/list.go`

#### New flag registration (in `init()`, after line 696):
```go
listCmd.Flags().IntVar(&listCount, "count", 0, "Maximum number of agents to return (default: server limit)")
```

Add package-level var:
```go
var listCount int
```

#### Wire into `listAgentsViaHub()` (around line 129):
Set `opts.Page.Limit` from the flag:
```go
opts := &hubclient.ListAgentsOptions{
    IncludeDeleted: listDeleted,
    Phase:          filterPhase,
    Labels:         parsedLabels,
}
if listCount > 0 {
    opts.Page.Limit = listCount
}
```

#### Truncation warning (in `displayAgents()`, before rendering):
After receiving the response, check if results are truncated:
```go
// In listAgentsViaHub, after receiving resp:
if resp.Page.TotalCount > len(resp.Agents) {
    fmt.Fprintf(os.Stderr, "Warning: showing %d of %d agents. Use --count %d to see all.\n",
        len(resp.Agents), resp.Page.TotalCount, resp.Page.TotalCount)
}
```

This requires passing `TotalCount` through to the display path. The `listAgentsViaHub` function currently discards `resp.Page`. Options:
- Emit the warning in `listAgentsViaHub()` immediately after the API call (before converting to `AgentInfo`) — **recommended**, since this is where the page metadata is available.

#### JSON output (`--format json`):
The stderr warning is clean for JSON mode — JSON goes to stdout, warning goes to stderr. Additionally, when `--format json` is active and results are truncated, include truncation metadata in the JSON envelope:
```go
if outputFormat == "json" {
    output := struct {
        Agents    []api.AgentInfo `json:"agents"`
        Truncated bool            `json:"truncated,omitempty"`
        Total     int             `json:"totalCount,omitempty"`
        Shown     int             `json:"shownCount,omitempty"`
    }{
        Agents:    agents,
        Truncated: totalCount > len(agents),
        Total:     totalCount,
        Shown:     len(agents),
    }
    enc := json.NewEncoder(os.Stdout)
    enc.SetIndent("", "  ")
    return enc.Encode(output)
}
```

**Note:** This changes the JSON output shape from a bare array to an object envelope. This is a minor breaking change but aligns with the server's response format (which already returns `{agents: [...], totalCount, nextCursor}`). If backward compatibility with the bare-array format is critical, this can be gated behind truncation (only use envelope when truncated).

### Layer 6: Web UI — `web/src/components/pages/agents.ts`

**Line 438:** Add `limit=500` to the API call:
```typescript
// Before:
const url = qs ? `/api/v1/agents?${qs}` : '/api/v1/agents';

// After:
params.set('limit', '500');
const qs = params.toString();
const url = `/api/v1/agents?${qs}`;
```

This ensures the web UI explicitly requests 500, rather than relying on the server default. Even though we're also raising the server default, explicit is better — it protects against future server-side changes.

---

## Phased Implementation Plan

### Phase 1: Store Layer (independently committable)

**Files:**
- `pkg/store/entadapter/agent_store.go`
  - Raise `defaultAgentListLimit` to 500 (line 40)
  - Raise `maxAgentListLimit` to 500 (line 41)
  - Add `agentCursorPredicate()` method
  - Wire cursor into `ListAgents()` (insert after filter predicates, before query execution)
- `pkg/store/entadapter/project_store.go`
  - Raise inline default from 50 to 500 (line 432)

**Tests (same commit):**
- `pkg/store/entadapter/agent_store_test.go`
  - Add `TestListAgents_CursorPagination` — modeled on `TestListProjects_CursorPagination` (project_store_test.go:628-667). Create 125+ agents, verify: default page caps at 500, cursor walk enumerates all agents with no gaps or duplicates.
  - Add `TestListAgents_DefaultLimit` — verify that `ListAgents` with `Limit=0` returns up to 500 agents (not 50).
  - Add `TestListAgents_MaxLimit` — verify that `ListAgents` with `Limit=1000` caps at 500.

**Why first?** The store is the foundation. All upper layers depend on these limits. The cursor fix is pure correctness and has no behavioral impact on callers that don't pass a cursor (which is all of them today).

### Phase 2: HTTP Handlers (independently committable)

**Files:**
- `pkg/hub/handlers_agents_core.go` — line 212: `limit := 500`
- `pkg/hub/handlers_projects_core.go` — line 1642: `limit := 500`
- `pkg/hub/handlers_projects_core.go` — line 175: `limit := 500`

**Tests (same commit):**
- `pkg/hub/handlers_agent_test.go`
  - Add test verifying default limit is 500 when no `?limit=` is specified.
  - Add test verifying `?limit=100` is honored.
  - Add test verifying response includes `totalCount` and `nextCursor` fields.

**Why second?** Handler changes are trivial one-liners. Testing verifies the handler-to-store integration works with the new defaults.

### Phase 3: CLI `--count` Flag + Truncation Warning (independently committable)

**Files:**
- `cmd/list.go`
  - Add `listCount` var and `--count` flag registration in `init()`
  - Wire `listCount` into `opts.Page.Limit` in `listAgentsViaHub()`
  - Add truncation warning to stderr after API call
  - Update JSON output to include truncation metadata when results are truncated

**Tests (same commit):**
- `cmd/list_test.go`
  - Test that `--count 10` passes `limit=10` to the API (via mock or test server).
  - Test truncation warning output.
- `pkg/hubclient/agents_test.go`
  - Test that `Page.Limit` is correctly serialized as `?limit=N` in the request URL.

**Why third?** The CLI depends on both the store and handler changes being in place. The `--count` flag is the user-facing feature.

### Phase 4: Web UI (independently committable)

**Files:**
- `web/src/components/pages/agents.ts` — add `limit=500` to the fetch URL.

**Tests:**
- Manual verification: load the agents page and confirm the network request includes `?limit=500`.
- If the project has web UI tests, add a test verifying the limit parameter is set.

**Why last?** Web UI is the simplest change and has the least risk. It's also independently deployable — the server-side changes (phases 1-2) are backward-compatible with the old web UI.

---

## Test Plan

### New tests by location:

| Test File | Test Name | What It Verifies |
|-----------|-----------|-----------------|
| `pkg/store/entadapter/agent_store_test.go` | `TestListAgents_CursorPagination` | Cursor walk enumerates all agents, no gaps/dupes |
| `pkg/store/entadapter/agent_store_test.go` | `TestListAgents_DefaultLimit` | Default (Limit=0) returns up to 500 |
| `pkg/store/entadapter/agent_store_test.go` | `TestListAgents_MaxLimit` | Limit>500 is capped at 500 |
| `pkg/hub/handlers_agent_test.go` | `TestListAgents_DefaultLimit500` | Handler returns up to 500 with no ?limit |
| `pkg/hub/handlers_agent_test.go` | `TestListAgents_ExplicitLimit` | ?limit=N is honored |
| `pkg/hub/handlers_agent_test.go` | `TestListAgents_ResponseMetadata` | Response includes totalCount, nextCursor |
| `cmd/list_test.go` | `TestListCountFlag` | --count N passes limit=N to API |
| `cmd/list_test.go` | `TestListTruncationWarning` | Stderr warning when totalCount > returned |
| `pkg/hubclient/agents_test.go` | `TestListAgentsPageLimit` | Page.Limit serialized as ?limit=N |

### Existing test models:
- `TestListProjects_CursorPagination` in `project_store_test.go:628-667` — exact pattern for cursor pagination test
- `TestListGroupsWithLimit` in `group_store_test.go` — pattern for limit tests
- `TestListSchedulesFilterAndPagination` in `schedule_store_test.go` — pattern for filter+pagination combo

---

## Out of Scope

1. **`--offset` flag** — The store uses keyset (cursor-based) pagination. Numeric offset would require either inefficient SQL `OFFSET` or a skip-N loop. With 500 as the default, offset is unnecessary.

2. **Full cursor-based pagination in the CLI** — No `--page-size`, `--next-page`, or automatic looping. The 500 limit covers real-world use. If a user has >500 agents, `--count N` lets them raise it explicitly.

3. **Web UI pagination controls** — No "Next"/"Previous" buttons or infinite scroll for the agents page. The limit is raised to 500; the admin-users page already has pagination as a reference if it's needed later.

4. **Other entity listing endpoints** — Only agents and projects are raised to 500 in this PR. The remaining 15+ handlers (users, groups, templates, policies, skills, schedules, harness configs, allowlist entries) stay at `limit := 50`. They can be raised in a follow-up.

5. **Runtime broker handler refactor** — `pkg/runtimebroker/handlers.go:348` uses in-memory array slicing instead of DB-level limits. Out of scope — the in-memory slice limit stays at 50. Refactoring to use proper DB pagination is separate work. (Note: the runtime broker exists as a separate service that may list agents from its own perspective; investigating its purpose is not in scope here.)

6. **JSON output envelope change** — If backward compatibility of the `scion list --format json` bare-array output is a concern, the envelope change (wrapping in `{agents: [...], truncated, totalCount}`) can be deferred or gated. The design proposes it; the developer should confirm with the user if the shape change is acceptable.

---

## Decisions Made

| # | Question | Decision | Rationale |
|---|----------|----------|-----------|
| 1 | Skip `--offset` flag? | **Yes, skip** | Keyset pagination makes numeric offset impractical. 500 covers real-world use. |
| 2 | Raise all endpoints or just agents? | **Agents and projects only** | Targets the reported problem. Other entities can be raised in a follow-up. |
| 3 | Fix agent store cursor bug? | **Yes, fix it** | ~20 line fix for correctness. Pattern exists in project store. Ensures cursor pagination works if ever needed. |
| 4 | `--count` truncation behavior? | **Warn on stderr** | Loud, prominent warning. Least disruptive — user still sees results. For JSON output, stderr warning doesn't pollute stdout; additionally include `truncated`/`totalCount` in JSON envelope. |
| 5 | Runtime broker in scope? | **No, out of scope** | Different code path, different risk. Don't change its current limit or pagination model. |
