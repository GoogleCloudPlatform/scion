# slow-list P1a — state.ts SSE coalescing + completeness flag (#2385)

Branch `perf/2385-sse-coalesce`, commit `00697d661d3c0bec112c10cadd012735303cea0f`,
rebased on `origin/main` (169540e). Touches `web/src/client/state.ts` plus three new
test files; no consumer/page files changed.

## What

Implemented the state.ts-only slice from lists-graph.md §7 / §11 P1a:

- `handleAgentEvent` now skips mutation and notification entirely when the merged
  agent is shallow-equal to what state already holds, with `detail` compared
  field-by-field rather than by reference (`agentsShallowEqual`/`agentDetailEqual`).
- `dirty.upserted` / `dirty.deleted` / `dirty.unknown` accumulate since the last
  flush. `dirty.unknown` is a `Map<id, {phase?, activity?, lastActivityEvent?}>`,
  last-value-wins per field, for deltas an ID not yet in `state.agents`.
- `pendingAgentDeltas` entries (buffered deltas for an unknown ID) now expire 30s
  after they were last touched (`pendingAgentDeltaTimers`); there was no expiry
  before this change.
- One coalesced flush per `requestAnimationFrame`, or a 100ms fallback when no
  frame arrives (hidden tab) — whichever fires first wins and cancels the other.
  A flush emits, in order: `agent-created` per created ID, `agents-changed`
  `{upserted, deleted, unknown, generation}`, then the legacy `agents-updated`
  once per flush instead of once per event.
- `setScope` discards the dirty set outright (no trailing flush), bumps a new
  `generation` counter, invalidates open seed-epoch tokens, clears the
  completeness flag, and rejects any pending `sseConnected` waiters — only on an
  actual scope change (the existing early-return for an unchanged scope is
  unaffected).
- `agents-resync` fires on an SSE `connected` that follows a `disconnected`
  within the same scope generation; one per outage even if `connected` fires
  twice. The first connect after `setScope` is never a resync.
- `sseConnected(generation): Promise<void>` resolves at once if already
  connected in that generation, otherwise on the next `connected` of that
  generation; rejects immediately if already stale, and rejects if the
  generation changes while waiting.
- Seed epochs: `beginSeedEpoch()` / `seedAgents(list, {token, partial})` /
  `endSeedEpoch(token)`. An open epoch records merged deltas per ID as they're
  applied; `seedAgents` re-applies them over the REST snapshot so an SSE update
  during the fetch is never clobbered. Every seed (epoch or plain) now skips
  tombstoned IDs (`deletedAgentIds`). `partial: true` merges into the existing
  object instead of replacing it. A token invalidated by a scope change (or
  never opened, or already ended) makes `seedAgents` a complete no-op.
- Completeness flag: `markAgentSetComplete('full'|'compact')` upgrades but never
  downgrades; `isAgentSetComplete(need)` — `compact` is satisfied by either
  value, `full` only by `full`. Cleared only by an actual `setScope` change; not
  by resync, label-shaped reseeds, partial seeds or seed epochs. No callers yet
  (lands with the API only, per phase plan). Doc comment carries the R10 note
  that a consumer reading full fields must check `isAgentSetComplete('full')`
  specifically.
- The `ports` SSE branch was folded into the same shallow-equal/dirty/flush path
  (previously it mutated and notified unconditionally, even when nothing
  existed to update); everything else about its behavior is unchanged.

`debug-log.ts:124` needs no code change: it already re-subscribes to
`agents-updated` and will now log once per flush instead of once per event,
since state.ts coalesces that event (T5, noted in the PR body).

## Why

Design doc `lists-graph.md` r8 (md5 `2a21517bf376552064b727012ea67935`) §7 and
§11 P1a. This is the first vertical slice (state.ts only) so P4's consumers get
coalesced updates and off-page live stats from day one, ahead of the P1c web
commit. The completeness flag lands with no callers so home/agents.ts/graph
(P5/P6) can consume it later without a state.ts change.

## Tests

New files (existing web test runner, `npx vitest run`):

- `state-coalescing.test.ts` (W2): a 10k-event deterministic fuzz (seeded
  PRNG) asserting the final `state.agents` content matches an independently
  written reference reducer (no equality-skip, no coalescing) — this is what
  would catch a bug in the new shallow-equal check silently dropping a real
  change. Also: at most one `agents-updated`/`agents-changed` per flush; every
  changed ID appears in `upserted`/`deleted`/`unknown`; unchanged agents stay
  `===`; a changed `detail` is not mistaken for a no-op; unknown-buffer 30s
  expiry (including TTL refresh on a later delta); all four resync edge cases
  pinned against sse-client.ts (double `connected` after one drop, a
  `handshake-failed` retry that never opened, a server `reconnect`-style
  disconnect/connect, the no-resync connect after `setScope`); `sseConnected`
  resolves at once / waits / rejects-stale / rejects-on-generation-change.
- `state-seed-epoch.test.ts` (W3): an SSE delta during a drain survives the
  seed; a compact (partial) seed keeps full fields; a non-partial seed
  replaces outright; a tombstoned agent is never resurrected (plain seed, and
  within an epoch, including alongside another ID's surviving delta); a scope
  change invalidates the token (full no-op); `endSeedEpoch` is idempotent; an
  unrelated open epoch doesn't affect a plain seed.
- `state-completeness-flag.test.ts`: set, upgrade (compact→full), no downgrade
  (full stays full after `markAgentSetComplete('compact')`), and "cleared only
  by an actual scope change" — explicitly not cleared by a no-op `setScope`
  call to the same scope, a plain reseed, a partial seed, a seed epoch, an SSE
  resync, or an SSE delta merge.

Existing `state.test.ts` and every other test touching `StateManager`
(`scope-capabilities.test.ts`, `sse-client.test.ts`, `home.test.ts`,
`chat-member-flicker.test.ts`, `chat-palette-groups.test.ts`, etc.) pass
unchanged.

## Commands and results

- `npm run typecheck` (`tsc --noEmit`): pass, no output.
- `npm run lint` (`eslint src --ext .ts,.tsx`): pass, exit 0.
- `npx prettier --check` on all touched files: pass.
- `npx vitest run --no-file-parallelism` (full suite): 103 files, 2830 tests,
  all passing. (`--no-file-parallelism` was needed because this sandbox's
  forked-worker pool times out under default parallelism — a resource limit of
  this environment, not a test issue; confirmed by re-running individually
  and by the flag alone fixing it.)

## Round 1 review fixes (slow-list-lists-rev-p1a-1)

Commit `6552bb88c1d18e43f11f75844abee64e871bb79a`, rebased on `origin/main`
(f06ccbc). Full disposition table is in the updated gs report
(`lists-p1a-dev.md`); summary:

- **B1** (`sseConnected` resolved at once right after `setScope`, before the
  new generation actually connected): fixed with a `connectedGeneration`
  tracker, reset in `setScope`/`disconnected`/`disconnect()`.
- **B2** (a seed epoch lost SSE deltas for IDs not yet in `state.agents` —
  the normal first-drain case): fixed. The unknown-ID branch now also
  records into open seed epochs; `seedAgents` applies a recorded delta
  through a new shared `mergeAgentDelta` helper (extracted from
  `handleAgentEvent`) and clears the consumed `pendingAgentDeltas` entry.
- **B3** (the W2 fuzz flushed once for all 10k events, making its per-flush
  claims vacuous): rewritten to interleave rAF/100ms flush points with
  per-flush assertions, fuzzed `_capabilities`, and a new
  setScope-discards-dirty test.
- **N1-N5, nits**: all fixed (tombstoned IDs dropped outright; epoch
  finally-rule documented and `seedAgents` self-ends its epoch;
  `disconnect()` rejects waiters; `exposedPorts` compared by value; the
  report's lint claim corrected to match reality). See the gs report's
  disposition table for exact file:function locations.
- **FYI** (created-after-delete in one flush): fixed — a re-upsert now
  removes the ID from `dirty.deleted`.

## Deviations from the brief

- `seedAgents`'s existing (pre-P1a) callers use the single-argument form with
  no token/partial. Tombstone-skipping (`deletedAgentIds`) was made
  unconditional — it applies to that legacy call shape too, not only under a
  seed-epoch token — since resurrecting a definitively-deleted agent via any
  seed path is a bug regardless of caller. No existing test relied on the old
  (non-skipping) behavior; checked all real (non-mocked) call sites.
- The `ports` branch's `!existing` case used to unconditionally call
  `notify('agents-updated')` even though nothing changed; it is now a true
  no-op (consistent with the new equality-skip principle). No buffering was
  added for ports on an unknown ID — that stayed exactly as before, to keep
  the diff to the documented merge semantics.
