# Native Chat Quick Command Palette — Phase 2 (conversation groups)

**Date:** 2026-09-29
**Design:** `/scion-volumes/scratchpad/projects/native-chat/palette/design.md`
**Branch:** `scion/nc-palette-conversations`
**Base:** `origin/scion/nc-palette-dm-slice` at the squashed phase-1 head `85f8f4ad0bbf6957ed49a7083815edd98c5098e2`

## Problem

Phase 1 shipped one vertical slice of the quick command palette: real paginated Agents + DMs, the
matcher, and the single-shortcut-owner/focus infrastructure, all behind `web.native_chat_palette`.
Phase 2 fans out to the other two real-data groups (People, Threads), builds the four-group keyboard
infrastructure those groups need (Documents arrives in phase 4), and fixes the pre-existing
wrong-project/encoding defect the investigation found in the legacy switcher — applying the same fix to
the new palette's Threads selection so both share one implementation.

## Approach

- **`client/chat-palette-data.ts`** — added `fetchAllPaletteUsers`/`buildUserCandidates` (People, same
  pagination/exclusion contract as phase 1's Agents) and `fetchPaletteSpaces`/
  `fetchPaletteThreadsForSpace`/`buildThreadCandidates` (Threads), bounded to 4 concurrent per-space
  thread requests via a small worker-pool helper (not batch chunking, so a slow request never idles the
  other three workers). `ChatPaletteDataController` now tracks a separate generation/AbortController pair
  per group (Agents/People/Threads can load concurrently without one group's supersede aborting
  another's in-flight request) and tracks per-space thread state so `retryThreadsGroup` can re-fetch only
  the spaces that failed last time, merging with the other spaces' already-successful threads rather than
  redoing the whole group. A partial thread-list failure keeps the group `ready` with `incomplete: true`
  and its successful rows selectable, rather than hiding them behind an `error` state; a total failure
  (every attempted space failed) still surfaces as a real error, so a failed group is never silently
  presented as merely empty.
- **`client/chat-palette-types.ts`** — added `PaletteThreadTarget`, `threadCandidateId`, and
  `GroupState.incomplete`. Pulled the four-group reading order out into one exported
  `PALETTE_GROUP_ORDER` constant that both the ranking comparator (`chat-palette-match.ts`) and the
  palette's own Tab/Shift+Tab cycling (`chat-switcher.ts`) now derive from, so the two can't independently
  drift apart the way two hardcoded copies of the same order eventually would.
- **`components/shared/chat/chat-switcher.ts`** — renders Threads and People alongside Agents. Tab/
  Shift+Tab now cycles the *nonempty* groups in reading order (wrapping, skipping groups with zero
  matches for the current query, recomputed fresh on every press so a group that gains a match "joins
  the next traversal" automatically). Up/Down now wrap within the active candidate's own group only
  (phase 1's version moved through the whole combined ranked list, which was correct only because there
  was exactly one group). Added a 10-row-per-group visible cap with a "show more" control, and an
  auto-expand path that keeps the active option mounted whenever Tab/Up/Down/a background refresh selects
  a candidate beyond the cap. Threads gets its own "some results couldn't load" notice with a scoped
  retry, alongside the existing per-group loading/error/empty states.
- **`components/pages/chat.ts`** — `navigateToThread` was extracted out of `handleThreadSelect` (the
  rail's `thread-select` handler) so the palette's new Threads-group selection can call the exact same
  wrong-project-fix logic instead of a second implementation: prefer the target's own carried
  `projectSlug`, then a locally cached one, and only fall back to `/chat/space/{projectId}/thread/{id}`
  when neither is known — never guessing another project's slug. The legacy flat switcher's
  `handleSwitcherSelect` had the actual pre-existing bug (its own `SwitcherConversation` entries never
  carry a slug at all, unlike the new palette's Threads candidates, which fetch it directly): deleted its
  "fall back to the first known slug" branch and applied the same projectId-fallback fix, plus missing
  `encodeURIComponent` calls the investigation had also flagged. Added the 30-second per-group cache
  (`_shouldUseCachedPaletteGroup`), SSE-driven invalidation (`chat-message-received`,
  `chat-topic-updated`, `agent-created`, `agents-updated`, `chat-dm-promoted`, each mapped to the specific
  group(s) it can affect) and a 500ms debounced refresh while the palette stays open — deferred from
  phase 1 specifically because it needed to be built once against real multi-group shapes rather than
  guessed from the Agents-only slice.
- **`e2e/chat-palette/`** — extended the existing fixture's mock API with opt-in People/Threads/spaces
  fixture data (every phase 1 spec's original zero-threads/zero-people behavior is the default, so none
  of them needed to change their own assertions — only one needed re-scoping, see below) and two new
  specs for the two Chromium ACs this phase adds.

## A genuine Phase 1 test needing re-scoping, not a regression

`agent-selection.pw.ts`'s "a non-viable agent never appears" test asserted an unscoped
`.palette-empty` element was visible for a query matching nothing. Once Threads and People also render
(and also show their own empty state for the same non-matching query), that assertion became a real
3-way strict-mode violation in Playwright — fixed by scoping it to the Agents group's own heading
container. This is the expected effect of Phase 2 actually wiring in the other two groups, not a
behavioral regression in anything phase 1 shipped.

## A design/backend mismatch found and flagged, not fixed

The real backend's `store.User.Status` enum has exactly three values: `active`, `suspended`, `invited`
— there is no `"disabled"` status anywhere in the schema. The palette's People-group exclusion (this
phase, matching the pre-existing `loadHubMembers` convention in the same file) checks the literal string
`status === 'disabled'`, which can therefore never match a real `GET /api/v1/users` response. Confirmed
empirically in the real-API smoke: a seeded `status: "suspended"` user is correctly present in the real
People group, not excluded. This is a genuine design/backend gap (what should "disabled" mean against
today's real enum?) outside this phase's file ownership to resolve unilaterally — flagged to
nc-palette-em rather than guessed at. Unit-level exclusion of a literal `'disabled'` fixture value is
still fully covered and correct.

## An emergent finding while smoke-testing "missing slug from the map" against real data

Reproducing AC 2.3's exact scenario (a project whose slug the client hasn't cached) against real seeded
projects surfaced a real difference between the two navigation paths that share the wrong-project fix:
the new palette's Threads candidates always carry their own project's slug directly (fetched fresh from
the same real spaces endpoint), so the projectId-fallback branch is not reachable through the palette
with real backend data — real projects always have a real slug. The legacy switcher's own conversation
entries never carry a slug at all, so its fallback *is* reachable, and — because `handleSwitcherSelect`
navigates via full page recreation (`navigateTo`) rather than `pushState` — a stale-slug fallback URL is
immediately self-corrected by the freshly recreated page's own rail load re-fetching real truth. Neither
path ever resolves to the wrong project in a live run; the exact URL shape briefly shown differs for a
reason that has nothing to do with correctness. Recorded as an explicit empirical finding in
`evidence/phase2.md` rather than asserting a literal fallback-URL match that would not have held.

## Gate results

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` (project + `e2e/chat-palette/tsconfig.json`) | Clean |
| `npx vitest run` (palette types/matcher/data, chat-switcher palette suites, chat.ts palette suites) | 662/662 passed |
| Chromium: full `e2e/chat-palette/` suite (Phase 1 + Phase 2) | 42/42 passed |
| Chromium: the 9 new Phase 2 specs at `--repeat-each=10` | 90/90 passed |
| Real-API smoke (local ephemeral hub, `--dev-auth`, seeded users/projects/threads/DM) | Real `GET /api/v1/users`, `GET /api/v1/chat/spaces`, `GET /api/v1/chat/spaces/{id}/threads`, `GET /api/v1/chat/dms`, and a real user-to-user DM send all verified against the actual handler chain and the real compiled UI |

Full evidence, exact commands, and output excerpts:
`/scion-volumes/scratchpad/projects/native-chat/palette/evidence/phase2.md`. Mutation table:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase2-r0-notes.md`.
