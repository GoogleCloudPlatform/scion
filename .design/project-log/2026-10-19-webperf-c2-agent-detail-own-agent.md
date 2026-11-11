# webperf C2: agent detail reacts only to its own agent

**Date:** 2026-10-19
**Branch:** scion/webperf-c2
**Issue:** ptone/scion#5305 (epic #5301)

## Problem

`scion-page-agent-detail` listened to the legacy `agents-updated` event,
which fires once per coalesced state flush for *any* agent in the
`project.X.>` scope. On every flush it did
`this.agent = { ...this.agent, ...updatedAgent }`, a new identity even when
this agent had not changed, so the whole ~3.4k-line page re-rendered on every
sibling agent's status tick. `projects-updated` did the same with
`this.project`.

## Change

- Listen to `agents-changed` instead (its payload is
  `{upserted, deleted, unknown, generation}`) and return early unless
  `this.agentId` is in one of the three. The existing merge and tombstone
  logic are unchanged.
- `projects-updated` carries no ID, so the page remembers the stateManager
  project object it last merged (or seeded: `loadData` seeds `this.project`
  itself). It skips the flush when the object is that same reference.
  stateManager replaces a project's object on each of its events and never
  mutates one in place, so reference equality is a reliable "nothing
  changed" signal; an extra `updatedAt` comparison would be redundant.
- Event payloads are read defensively (optional chaining on `upserted`,
  `deleted` and `unknown`), so a partial `agents-changed` payload is
  ignored instead of throwing.
- On `agents-resync` (an SSE reconnect) the page refetches its agent
  through `backgroundRefresh()`. Changes during the outage arrive in no
  `agents-changed` flush, and the old "pick it up at any flush" behaviour no
  longer covers them (review finding M1). This adds a reaction listener only.
- No change to subscriptions, SSE or the 15 s relative-time tick (C5a).

## Emitter audit

`agents-updated` is dispatched in one place only: `state.ts` `flush()`,
right after `agents-changed` in the same flush. Every `state.agents` mutation
that schedules a flush (`handleAgentEvent`: delete, upsert, created, and
unknown-ID deltas; `applyDeleteAccepted` goes through `handleAgentEvent`)
records the ID in `dirty.upserted`, `dirty.deleted` or `dirty.unknown`. The
silent mutations (`seedAgents`, `removeAgent`, `setScope`'s clear) never
dispatched `agents-updated` either. So no path emits only the legacy event,
and no fallback listener is needed. One deliberate narrowing: before, a REST
seed of this agent by another component was picked up at the next unrelated
flush; now it is picked up at the next flush that names this agent. This
page seeds its own data and assigns it directly, so nothing it relies on
changes.

## Tests

`agent-detail.test.ts`: the `FakeStateManager` now tracks dirty IDs and emits
`agents-changed` before `agents-updated`, the same way the real flush does.
New tests use a render-count spy (an instance override of `render()`):
- `agent-detail ignores agents-changed for sibling agents`: 0 renders.
- `agent-detail re-renders for its own agent`: 1 render.
- `agent-detail project update with same reference ignored`: 0 renders, then
  1 render for a new object with a new `updatedAt`.
- `agent-detail tolerates a partial agents-changed payload`: no throw and no
  render for payloads missing fields; a partial payload naming this agent
  still applies.
- `agent-detail refetches its agent on agents-resync`: GETs the agent and shows
  the new phase.

Run against the old source, the sibling and project tests fail. The
page-load tombstone-guard test now sends its flush as an `unknown` entry for
this agent, because an unrelated flush is ignored outright and would no
longer exercise the guard. The removeAgent-prune test likewise sends an
explicit `unknown` entry instead of relying on an earlier dirty ID.

## Learnings

- Lit calls `this.render()`, so overriding `render` on the instance is
  a simple render counter that needs no `vi.spyOn` on a protected method.
- `web/node_modules` is not present in a fresh clone; run `npm ci` through
  heavy-build first.
