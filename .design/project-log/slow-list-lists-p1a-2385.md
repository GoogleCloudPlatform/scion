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

## Round 2 review fixes (slow-list-lists-rev-p1a-2)

Commit `d5ab6b820f66265fc4f3019dd8c059488a6d6e4f`, rebased on `origin/main`
(f06ccbc, unchanged since round 1's fix). All 14 round-1 findings verified
closed by the round-2 reviewer. Round 2 found:

- **B1** (new, introduced by round 1's own B1 fix): resetting
  `state.connected = false` in `setScope` silently changed
  `chat-thread.ts:1536`'s reconnect-catch-up seeding
  (`stateManager.isConnected`), risking silently dropped chat messages on a
  warm navigation into chat. Fixed by removing that line — `connectedGeneration`
  alone is what `sseConnected` needs, and it never reads `state.connected`.
  Added a test pinning that `isConnected` is unchanged by `setScope`.
- **N1**: the W2 fuzz's property (b) was one-directional and never examined
  buffered/unknown IDs, so dropping one from `dirty.unknown` or
  over-reporting an unchanged ID in `upserted` would still pass. Fixed:
  the fuzz now computes the expected unknown set per flush window from the
  batch's own events and asserts `upserted`/`unknown` exactly, `deleted` as
  a superset of IDs actually removed.
- **N2**: added the untested `{token, partial: true}` combination (the
  compact drain's exact call shape) and a
  `connected`→`disconnected`→`sseConnected`-stays-pending test (the other
  half of the `connectedGeneration` contract within one generation).
- **nit-1/nit-2**: fixed (array-hole comparison intent in `exposedPortsEqual`;
  `recordSeedEpochDelta`'s JSDoc now documents both call sites).
- **FYIs** (`setCurrentUserId` generation gap; ports-for-unknown-ID still
  droppable by a first-drain seed): no code change, noted in the gs report
  for P1c.

## Round 3 review (APPROVE, with findings to close)

All round-1 and round-2 findings verified closed by the round-3 reviewer
(including mutation-testing the round-2 N1 fuzz fix). One residual
non-blocking finding and one nit remained:

- **N1** (round 2's `unknown`-set fuzz check only exercised the opening
  ~5% of the 10k-event run, since all 24 fixed IDs get permanently
  tombstoned early): fixed by alternating `setScope` every ~500 events in
  the fuzz (resetting the test's own shadow bookkeeping the same way
  production resets `state.agents`/`deletedAgentIds`, and asserting no
  stale flush fires across the switch), plus a floor assertion
  (`flushesWithNonEmptyUnknown >= 200`; measured 698/2080 with the fix).
- **nit-1**: trimmed the `setScope` comment to its two durable sentences,
  and removed every "(round N review X)" attribution tag from `state.ts`
  (code comments only — test names that reference a round/finding ID are
  left as useful traceability, not cleaned up, since the instruction was
  scoped to `state.ts`).
- **FYI** (a redundant `deleted` for an already-gone/never-known ID still
  reports it in `agents-changed.deleted`): noted in the gs report for P1c
  — consumers should treat `deleted` as idempotent/safe-as-superset, not
  assume every entry corresponds to a real transition.

## Round 4 review (confirmation; APPROVE, two small items to close)

Round 4 verified round 3's N1 and nit-1 closed with mutation evidence (the
round-3-surviving single-ID mutant for `recordUnknownDirty` now fails the
fuzz) and confirmed the round's `state.ts` change was comment-only. It
raised two small new items:

- **N1**: the in-fuzz "no stale flush across setScope" assertion added in
  round 3 could never fail, since the scope-reset block runs only after
  `verifyFlush` already drained the batch's flush — so there is never a
  pending flush left to discard at that point. Took option (b) as directed:
  deleted the vacuous assertion and the "extends ... in-flight dirty sets"
  claim from the comment (the dedicated, already-existing B3
  setScope-discard test is what actually covers that behavior).
- **nit-1**: dropped the remaining "(round N review X)" tags from four
  inline comments in `state-coalescing.test.ts` that were added by the
  round-3 fix itself (test *names* keep their tags, as agreed in round 3).

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

## Upstream Gemini review, GoogleCloudPlatform/scion#2189 (3 comments)

Fixed all three. Full disposition table and commands/results:
`gs://scion-xproject-exchange/slow-list/reports/lists-p1a-gemini-2189.md`.

- **High** (`shallowObjectEqual`): matching key *counts* isn't matching key
  *sets* — `{message: undefined}` and `{currentTurns: undefined}` both have
  one key, and indexing a missing property reads `undefined` on either side,
  so the old count-only check called them equal. Added the same
  `hasOwnProperty` guard `agentsShallowEqual` already uses one level up.
  Test: `state-coalescing.test.ts` → "Gemini #4151811120: ...".
- **Medium** (`bufferAgentDelta`) and **Medium** (`recordSeedEpochDelta`):
  Gemini's suggested fix (deep-merge `detail` itself) was wrong — `detail` is
  *always* replaced wholesale by whichever delta sets it last, even for a
  real base going through `mergeAgentDelta` one delta at a time (checked
  `pkg/hub/events.go:PublishAgentStatus`: every SSE status event carries a
  full current-state `AgentDetail`, not a partial one, mod `omitempty`
  dropping zero-value fields — there is nothing to deep-merge within
  `detail` across events). The *actual* gap: `mergeAgentDelta` promotes
  `detail` fields (`message`, `currentTurns`, `currentModelCalls`,
  `startedAt`) onto the agent's own top level, and that promotion runs once
  per real delta application, so an earlier delta's promoted field survives
  a later delta that doesn't repeat it (top-level spread leaves an absent
  key alone). `bufferAgentDelta`/`recordSeedEpochDelta` only ran promotion
  once, on the final accumulated delta — so a buffered/recorded delta's
  promoted field could be silently lost if a later delta's own `detail`
  didn't carry it, which immediate sequential application never does.
  Fixed by extracting the promotion block from `mergeAgentDelta` into a
  shared `promoteDetailFields` helper, and running every delta through it
  *before* folding it into the buffer/epoch accumulator (re-running it at
  final-merge time is idempotent). Tests: `state-coalescing.test.ts` →
  "Gemini #4151811134: ..."; `state-seed-epoch.test.ts` → "Gemini
  #4151811140: ...". Both tests build an independent "immediate sequential
  application" StateManager (agent created/seeded first, same two deltas
  applied one at a time) and assert the buffered/epoch path produces an
  identical agent object; both fail against the pre-fix code (verified by
  stashing the production fix and re-running them) and pass after it.
  Extended the W2 10k-event fuzz's `genEvents` to generate partial,
  differing `detail` shapes (previously every detail-bearing status event
  set both `message` and `currentTurns` together, so the fuzz could never
  have caught this) and updated its independent reference reducer
  (`referenceApply`) to mirror the same promote-before-accumulate fix via a
  duplicated `promoteDetailFieldsRef` helper (the production helper isn't
  exported).

## Round 6 review (APPROVE; N1, N2, nit1, nit2 all closed)

Full review: `gs://scion-xproject-exchange/slow-list/reviews/lists-p1a-rev-6.md`.
New head, addendum and full disposition:
`gs://scion-xproject-exchange/slow-list/reports/lists-p1a-gemini-2189.md`.

- **N1** (the changed fuzz still couldn't catch the bug it was changed for —
  one final-only comparison let a transient divergence get overwritten by a
  later event long before the run ended): added a second fuzz test that
  checks every 25 events (the interval the reviewer used to independently
  confirm the gap) against an incremental reference model, for both existing
  seeds.
- **N2** (the reference's buffered-path accumulator copied production's own
  promote-then-spread design, so it wasn't independent for that path):
  replaced it with a `ReferenceModel` that stores buffered raw deltas in
  arrival order and replays them one at a time through the known-agent merge
  path once `created` supplies a base — the actual definition of sequential
  application, not an accumulator shortcut.
- Implementing N2 exactly as directed exposed a **real, separate,
  pre-existing bug** the N1 checkpoint test then caught: sticky-activity
  preservation was never extended to buffered/recorded deltas. Two SSE
  status deltas can race the `created` event for the same unknown ID
  (documented as reachable, state.ts's own comment on the unknown-ID
  branch); if the first sets a sticky activity (e.g. `completed`) and the
  second tries to reset it to `working`/`''`, `bufferAgentDelta` (and
  `recordSeedEpochDelta`) flattened both into one delta via a plain
  top-level spread *before* any sticky check ran, so the first delta's
  sticky activity was silently lost — something sequential application
  (applying each delta immediately, one at a time, to a real base) never
  does. The design doc (§7) lists sticky activity alongside detail promotion
  as one of the "documented merge semantics" that must apply when "buffering
  early deltas"; this was a gap in that, not a design choice, and leaving it
  in would have meant either leaving the new, more-rigorous N2 oracle
  permanently red or weakening it back into another production-shaped copy.
  Fixed it the same way as the Gemini fixes: extracted a shared
  `applyDeltaStep` helper (sticky-activity suppression, then
  `promoteDetailFields`) used by `mergeAgentDelta` (real base) and by
  `bufferAgentDelta`/`recordSeedEpochDelta` (the accumulator built so far, as
  a pseudo-base). Verified against all three historical versions of
  `state.ts` (pre-fix `78c7c9ef`, round-1 fix `043425ef`, and this round) —
  see the gs report for the exact per-version pass/fail matrix.
- **nit1**: fixed the misattribution — the comment now credits "the
  promoteDetailFields fix for Gemini #4151811134/#4151811140", not Gemini
  directly (Gemini proposed the deep-merge that was declined).
- **nit2**: trimmed `bufferAgentDelta`'s and `recordSeedEpochDelta`'s doc
  comments to one line each pointing at `promoteDetailFields`/
  `applyDeltaStep`; the reasoning lives once, on the shared helpers.
