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
(f06ccbc). Full disposition table is in the updated dev report
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
  report's lint claim corrected to match reality). See the dev report's
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
  droppable by a first-drain seed): no code change, noted in the dev report
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
  reports it in `agents-changed.deleted`): noted in the dev report for P1c
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

Fixed all three. Full disposition table and commands/results: the dev report.

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

Full review: the round-6 review. New head, addendum and full
disposition: the dev report.

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
  see the dev report for the exact per-version pass/fail matrix.
- **nit1**: fixed the misattribution — the comment now credits "the
  promoteDetailFields fix for Gemini #4151811134/#4151811140", not Gemini
  directly (Gemini proposed the deep-merge that was declined).
- **nit2**: trimmed `bufferAgentDelta`'s and `recordSeedEpochDelta`'s doc
  comments to one line each pointing at `promoteDetailFields`/
  `applyDeltaStep`; the reasoning lives once, on the shared helpers.

**Correction (round 7):** the `applyDeltaStep` pseudo-base design above was
an incomplete fix. It only matches sequential application when the real
base (the REST row a seed-epoch replays against, or `{}` for a fresh
`created`) is *not* itself sticky — see the round-7 entry below. The
`applyDeltaStep` helper described above no longer exists; it was replaced,
not patched.

## Round 7 review (REQUEST CHANGES; B1, N1, nit1, nit2 all closed)

Full review: the round-7 review; probe: the round-7 reviewer's probe.
New head, addendum and full disposition: the dev report.

- **B1 (required):** the round-6 `applyDeltaStep` pseudo-base accumulator
  only remembers the *last* surviving activity, so the final
  `mergeAgentDelta(base, acc)` checks stickiness against the real base
  without knowing whether an intermediate activity in the accumulated
  sequence had already (correctly) replaced that base's sticky value before
  a later `working`/`''` arrived. Concretely: a REST row with
  `activity: 'waiting_for_input'` (sticky), with `thinking` then `working`
  recorded during the epoch, replayed to `waiting_for_input` instead of
  `working` — exactly the stale-REST-over-newer-SSE regression §7/§8 say
  seed epochs exist to prevent. Root-caused and reproduced end-to-end
  through the public API by round 7's reviewer (30 mismatches found by an
  exhaustive probe over `{working, thinking, waiting_for_input, completed,
  none}` × 3 deltas × every base activity, down from 174 before round 6 but
  not zero). Took the reviewer's option (a): `pendingAgentDeltas` and each
  seed epoch's `deltas` now store **raw deltas as an ordered list per ID**
  instead of one accumulated delta, and replay them **one at a time through
  `mergeAgentDelta`** once a base is available — `handleAgentEvent`'s
  created branch (created delta first, then each buffered delta) and
  `seedAgents` (REST row first, then each recorded delta). This is the same
  algorithm the W2 `ReferenceModel`/`applyKnown` (round 6) and the review's
  probe already used as the independent oracle, now adopted in production
  too, so there is no longer a separate "pseudo-base" case to reason about
  — `mergeAgentDelta` always has a real, evolving `Agent` base. Verified
  with the reviewer's own (uncommitted) probe: 30 mismatches → 0, and the
  live sticky-REST-base repro now yields `working`.
- **N1 (non-blocking, closed):** the epoch half of the sticky fix had no
  dedicated test (only the W2 buffer-path fuzz exercised sticky activity at
  all). Added, in `state-seed-epoch.test.ts`: the known-agent
  live-`working`-vs-stale-sticky-REST repro as a committed test, plus an
  exhaustive 5^4 (625-case) enumeration of 3-delta sequences over every base
  activity for the epoch path. A matching exhaustive enumeration for the
  created+buffered path was added to `state-coalescing.test.ts` instead (not
  `state-seed-epoch.test.ts`): it exercises `pendingAgentDeltas`/`created`
  only, no seed-epoch API, which is that file's own documented scope. Both
  new exhaustive tests fail against `7c6140b0` (round 6) and pass after this
  round's fix; both also build their own independent "sequential
  application" oracle (seed/create the base directly, then apply each delta
  immediately, one at a time, through the public SSE path) rather than
  reusing any production or `ReferenceModel` code.
- **nit1 (non-blocking, closed):** `applyDeltaStep`'s doc comment had been
  inserted between `promoteDetailFields`'s own doc comment and
  `promoteDetailFields` itself, leaving two stacked JSDoc blocks attached to
  the wrong function and a stale "shared by buffer/epoch" claim. Resolved
  by removing `applyDeltaStep` entirely (its two call sites besides
  `mergeAgentDelta` are gone under B1's redesign — `mergeAgentDelta` is
  once again its only caller, exactly as before round 6), restoring
  `promoteDetailFields`'s original doc comment above itself, and updating it
  to describe the current (replay-based) design instead of the retired
  accumulator one.
- **nit2 (non-blocking, closed):** dropped the "(round 6 review N1)" /
  "round 6 review N2" tags from `state-coalescing.test.ts`'s comments and
  the new test's name — this file had that pattern removed twice before
  (commits `8302a4b`, `5b62879`) for the same reason: a round-N tag means
  nothing once this lands upstream. Also removed the "(round-7 review ...)"
  tags this round's own `state.ts` comments had accumulated, which would
  have repeated the same mistake in the one file (`state.ts`) round 3
  explicitly swept clean of them.

### Commands and results

- `npm run typecheck`: pass. `npx eslint src/client/state.ts`: clean.
  `npx prettier --check` on `state.ts` and both changed test files: pass.
- `npx vitest run --no-file-parallelism state-coalescing.test.ts
  state-seed-epoch.test.ts state-completeness-flag.test.ts`: 3 files, 64
  tests, all passing (61 from round 6 + 3 new: the sticky-REST-base repro,
  and the two exhaustive enumerations).
- Reviewer's probe (fetched fresh, not committed): 0 mismatches (down from
  30 at `7c6140b0`), live sticky-REST-base repro yields `working`.
- Regression check: swapped in the `7c6140b0` `state.ts` and reran the 3 new
  tests — all 3 failed with the predicted divergence; restored the fix and
  all 3 (plus the full 64) passed again, byte-identical to the committed
  `state.ts`.
- Full suite: see the dev report addendum for the exact count at this round's
  head SHA.

## Round 8 review (REQUEST CHANGES; B1, B2, N1, nit1-5 all closed)

Full review: the round-8 review; probe: the round-8 reviewer's probe.
New head, addendum and full disposition: the dev report.

- **B1 (required):** the round-7 fix (raw deltas as an ordered list, replayed
  one at a time) made `pendingAgentDeltas` unbounded per ID: a sliding TTL
  only bounds *how long* an entry survives, not how big it gets, and an
  off-page agent is unknown to `state.agents` — hence unexpired — for as
  long as it keeps emitting, by design (that is what `dirty.unknown` is
  for). Reviewer measured 5000 retained deltas for one ID after 5000 events
  at 1/s. Ruling: option (a), exact compaction, so memory per ID is O(1)
  and replay still equals sequential application. Every field except
  `activity` is base-independent and folds eagerly (last-value-wins, each
  delta promoted first) with no loss. `activity` can depend on an unknown
  base's stickiness, but only up to the first delta whose own activity is
  neither `working` nor `''` ("unlocking") — past that point `activity`'s
  value is base-independent too and can be resolved immediately. This
  reduces to a single `CompactedDelta` object per ID
  (`{fields, activity: {locked, pending|value}}`), replacing the raw list,
  with `foldCompactedDelta` (fold one more delta in), `composeCompactedDeltas`
  (compose two runs — used to insert a delta *before* an already-compacted
  one) and `applyCompactedDelta` (apply to a real base, through
  `mergeAgentDelta`, bypassing its sticky check only for an already-resolved
  locked value). `applyDeltaStep`'s pseudo-base concept from round 6 does
  not return; `mergeAgentDelta` itself is unchanged. Epoch lists get the
  same treatment for correctness (any multi-delta run needs `CompactedDelta`
  to stay equal to sequential application), noted in the doc comment that
  their memory isn't a concern either way since an epoch lives one drain.
  Verified: reviewer's probe B (`pendingAgentDeltas.get(id).length`) now
  returns `undefined` — it's an object, not an array — after 5000 events;
  probes C (625 cases) and D (3125 cases) stay at 0 mismatches.
- **B2 (required):** no committed test covered `recordSeedEpochDeltaFirst`'s
  ordering (created's own delta must apply *before* the already-recorded
  buffered deltas). Added a reduced form of the reviewer's probe D: buffer
  two deltas, send `created` with its own activity, send one more delta,
  then seed and compare against live state. Verified it fails when the
  compose order is swapped (`composeCompactedDeltas(prev, createdOnly)`
  instead of `(createdOnly, prev)`), the equivalent of the reviewer's
  push-instead-of-unshift mutant for this round's data structure.
- **N1 (required... promoted from the prior round's non-blocking note):**
  epoch-recording was still gated on `changed`, so a delta that happened to
  be a no-op against *live* state (but not against an older REST row) was
  silently never recorded, which is itself a stale-REST-wins bug when the
  REST snapshot predates the client's own pre-epoch state (replica lag, a
  cache). Fixed by recording into open epochs unconditionally in
  `handleAgentEvent`'s known-agent/created path (ports' own no-op gate is
  unchanged — out of this round's cited scope). Added the reviewer's probe
  G as a committed test; it now matches sequential application exactly.
- **nit1:** fixed the stale "see that function's doc comment for why
  promoting before accumulating..." reference in the test oracle —
  `promoteDetailFields` doesn't say that anymore, and nothing accumulates
  that way. Trimmed to a plain "mirrors promoteDetailFields" note.
- **nit2:** dropped the remaining "(round N review)"/finding-ID prefixes
  from test names across both state test files (left over from rounds 1-2,
  previously treated as an exception for traceability — the reviewer
  pointed out that exception doesn't survive landing upstream either) and
  from a few of this round's own new `state.ts` comments.
- **nit3:** `state-seed-epoch.test.ts`'s header updated from "records the
  per-ID merged deltas" to describe the actual (now compacted, not merged
  or raw-list) representation.
- **nit4:** trimmed round-by-round design history out of `state.ts`'s
  comments; `CompactedDelta`'s own doc comment is the one place carrying
  the "why compaction is exact" reasoning, with call sites pointing at it
  in a line or two instead of repeating it.
- **nit5:** corrected the test helper doc that mislabeled the activity set
  (it said "one non-sticky value" when `working` is also non-sticky, and
  omitted that two of the five values are sticky).

### Commands and results

- `npm run typecheck`: pass. `npx eslint src/client/state.ts`: clean.
  `npx prettier --check` on `state.ts` and both changed test files: pass.
- `npx vitest run --no-file-parallelism state-coalescing.test.ts
  state-seed-epoch.test.ts state-completeness-flag.test.ts`: 3 files, 67
  tests, all passing (64 from round 7 + 3 new: the memory-bound test, the
  created-inside-epoch ordering test, and the no-op-still-recorded test).
- Fetched both the round-7 and round-8 reviewer probes fresh (not
  committed) and ran them directly: round-7 probe 0 mismatches, live
  sticky-REST-base repro `working`; round-8 probe B shows an `undefined`
  `.length` (an object, not an array) after 5000 events, C 0/625, D 0/3125,
  G now matches sequential exactly.
- Regression check: swapped in the `4cdb0b5420` `state.ts` (test files
  unchanged) and reran the 3 new tests — the memory-bound and no-op-
  recording tests failed with the predicted divergence; the ordering test
  passed (expected — round 7's `unshift`-based ordering was already
  correct, this test guards against a future regression, not round 7
  itself) and was separately confirmed to fail under a targeted mutant
  (swapping the compose order). Restored the fix and all 3 (plus the full
  67) passed again, byte-identical to the committed `state.ts`.
- Full suite: see the dev report addendum for the exact count at this
  round's head SHA.

## Round 9 review (REQUEST CHANGES; B1, N1, N2, nit1-3 all closed)

Full review: the round-9 review; repro: the round-9 reviewer's minimal
repro; fuzz: the round-9 reviewer's fuzz. New head, addendum and full
disposition: the dev report.

**Correction (round 9):** round 8's `CompactedDelta` was exact for
`activity` but not for `detail`/promoted fields — `fields` kept the most
recent raw `detail` object, and `applyCompactedDelta` passed it back
through `mergeAgentDelta`, which promoted it a second time, *after* a
later delta's own top-level field had already been folded in correctly.
A stale `detail.message` could therefore overwrite a fresher plain
`message`. This affected all three paths using `CompactedDelta` at the
time (buffer→created, known-ID epoch, unknown-ID epoch) and was a genuine
regression from `4cdb0b54`, which replayed raw deltas one at a time and so
never promoted out of order.

- **B1 (required):** `mergeAgentDelta` gained a `skipPromote` option;
  `applyCompactedDelta` calls it with promotion off, since `fields` is
  already correctly promoted per delta as it was folded in
  (`foldCompactedDelta`). Verified against the reviewer's minimal repro (2
  cases) and their 20k-case-per-path fuzz: 0 mismatches on every path with
  the fix, where before it was 1060-2038 depending on path.
- **N1 (promoted to required this round):** two related exactness gaps in
  the fold/compose rules, beyond the `detail` one above:
  - Falsy `_capabilities` (`null` or an explicit `undefined`) could
    clobber an already-folded truthy value mid-run, even though
    `mergeAgentDelta` itself always falls back to the prior value when a
    delta's own capabilities are falsy. Fixed by applying that same
    fallback inside `foldCompactedDelta` and `composeCompactedDeltas`
    (`inheritFalsyCapabilities`), not just once at final-apply time.
  - An explicit `activity: undefined` (an own key, not merely a missing
    one) unconditionally clears `activity` to `undefined` when applied
    live — `mergeAgentDelta`'s suppression check only ever fires for a
    *defined* `working`/`''` incoming value. The compacted representation
    previously treated "incoming activity is `undefined`" as "no
    information in this delta" regardless of whether the key was present,
    losing that distinction. Generalized `CompactedDelta`'s locked variant
    to `value: string | undefined` (an explicit-undefined delta "unlocks"
    to `undefined`, exactly like unlocking to any other concrete value)
    and switched the presence check to `hasOwnProperty` instead of a
    `!== undefined` comparison. Verified with the reviewer's fuzz run with
    both null-capabilities and explicit-undefined generation enabled: 0
    mismatches on every path at full scale (20k cases), where before
    enabling the fix left hundreds of mismatches per path.
- **N2 (promoted to required this round):** the same "epoch-recording
  gated on `changed`" class rev-7's N1 fixed for status/created deltas
  was still open for `ports`: moved `recordSeedEpochDelta(agentId,
  {exposedPorts})` above the equality early-return in the `ports` branch.
- **nit1:** reworded the B2 (created-first prepend ordering) test's
  comment per the reviewer's exact suggestion — the old wording described
  the mechanism backwards.
- **nit2:** fixed a stale "seedAgents, which applies it through
  mergeAgentDelta" comment — it's `applyCompactedDelta` now.
- **nit3:** the per-ID memory-bound test asserted key count and
  non-array-ness, which a list hidden *inside* one field would still
  pass. Added a `JSON.stringify(entry).length` bound, the property that
  actually matters.

**Required: committed a deterministic fuzz** (`state-compaction-fuzz.test.ts`,
new file) covering all four `CompactedDelta` paths (known-ID epoch,
unknown-ID epoch, buffer→created, buffer+created-inside-epoch+post),
always generating null capabilities and explicit-undefined keys (not
gated behind env vars, unlike the reviewer's own probe), comparing full
agent objects (own keys, explicit-undefined distinguished from absent) to
an independently-reimplemented sequential oracle. 3000 cases per path, 0
mismatches. Verified it fails — all 4 tests — against `6519b424`'s
`state.ts` with this round's test files, before restoring the fix.

### Commands and results

- `npm run typecheck`: pass. `npx eslint src/client/state.ts`: clean.
  `npx prettier --check` on `state.ts` and all three changed/added test
  files: pass.
- `npx vitest run --no-file-parallelism state-coalescing.test.ts
  state-seed-epoch.test.ts state-completeness-flag.test.ts
  state-compaction-fuzz.test.ts`: 4 files, 72 tests, all passing (67 from
  round 8 + 1 new ports N2 test + 4 new compaction-fuzz tests).
- Fetched the reviewer's min repro and fuzz fresh (not committed) and ran
  them directly against this round's `state.ts`: min repro 2/2 pass; fuzz
  (20k/path, `R9_NULLCAPS=1 R9_UNDEF=1`) 0 mismatches on all 4 paths.
- Regression check: copied `6519b424`'s `state.ts` in over this round's
  fix (test files unchanged) and reran — the 4 new compaction-fuzz tests
  and the new ports N2 test (5 total) failed with the predicted
  divergence; restored this round's `state.ts` and all 5 (plus the full
  72) passed again, byte-identical to the committed file.
- Full suite: see the dev report addendum for the exact count at this
  round's head SHA.

## Round 10 review (APPROVE; nit1, nit2 closed) + CI-safety timeout hardening

Full review: the round-10 review. First APPROVE verdict of the cycle.
Addendum and full disposition: the dev report.

- **nit1**: reworded the `CompactedDelta` doc comment's backwards
  parenthetical to the reviewer's suggested text, split out of a single
  ~15-line sentence.
- **nit2**: removed the cited "(B1, round 1 review)" tag, plus a broader
  sweep of `state.ts` and all four state test files for other round/
  finding-ID tags (caught three more inline comments and three bare
  "B2:"/"N4:" test-name prefixes) — this sweep later turned out to be
  incomplete; see round 11.
- Mid-round addition: another reviewer found two `state-compaction-fuzz.test.ts`
  tests exceeding the default 5s vitest timeout under load. Gave every
  loop-heavy test across all four state test files an explicit 60s
  timeout, not just the two flagged.
- Commit `987d2969919243c6932e64d6f83724b0ae1caff1`. Comments and timeouts
  only, no production logic change. `npm run typecheck` pass, `eslint`
  clean, `prettier --check` pass, targeted state tests 72/72, full suite
  106 files / 3044 tests, exit 0.

## Round 11 review (REQUEST CHANGES; R1 — finding-ID sweep completion)

Full review: the round-11 review. Addendum and full disposition: the dev
report.

Round 10's sweep (and every prior round's own sweep) only grepped for
tags *that round* introduced, so tags added in earlier rounds (6-9) and
never revisited survived. Round 11's reviewer grepped the whole file
history against `origin/main` and found ten remaining, none on `main`:

- `state.ts:528` `(§6.3, R2-B3, R3-B4)` → `(§6.3)`. `R2-B3`/`R3-B4` are
  the design doc's own *design*-review-round finding IDs — the same
  class of tag as the code-review "(B1, round 1 review)" tags already
  removed, just from the design doc's separate review history.
- `state.ts:500`/`:565` (two `(§7 N4)`) → `(§7)`; the `state-coalescing.test.ts`
  "W2 resync edges" describe name dropped its matching `(§7 N4, ...)`.
  `N4` is a design-doc finding ID with five separate occurrences across
  its own review rounds 1/3/4/5/6 — a bare `N4` names nothing specific.
- Upstream Gemini comment-ID tokens removed from three comments (one
  each in `state.ts`, `state-compaction-fuzz.test.ts`,
  `state-coalescing.test.ts`), reworded to describe the invariant instead
  of citing the ID. Dropped the `Gemini #...: ` prefix from the three
  test names that still had it.
- Kept `R10` (the design doc's "§14 Risks" ID, not a review finding) and
  every `W2`/`W3` label and bare `§`-section reference (design-doc
  structure, not a review artifact) — the review's own explicit
  exceptions.

Verified with the review's own grep,
`grep -nE 'R[0-9]+-[BN][0-9]+|\bN[0-9]+\b|\bB[0-9]+\b|Gemini|round [0-9]'
web/src/client/state.ts web/src/client/state*.test.ts`: zero hits.

This round's commit (SHA verified on the remote via `git ls-remote` and
reported to the EM separately — not repeated here, since this file is
itself part of that commit) touches comments and test names only;
`git diff` confirmed by inspection to touch only comment and
`it`/`describe` string-literal lines. `npm run typecheck` pass, `eslint`
clean, `prettier --check` pass, `npx vitest run --no-file-parallelism
src/client/state` 5 files / 78 tests passing. Per the EM's explicit
instruction, the full suite was not run this round.

Also this round, per the lead's hygiene standard: replaced every bucket
path in this project log with plain wording (e.g. "the round-10 review",
"the dev report") — IDs, SHAs and round numbers stay, only the bucket
paths are gone.
