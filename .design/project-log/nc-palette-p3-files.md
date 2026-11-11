# Native Chat Quick Command Palette — Phase 3 (recent files and extracted viewer)

**Date:** 2026-09-29
**Design:** `/scion-volumes/scratchpad/projects/native-chat/palette/design.md`
**Branch:** `scion/nc-palette-files`

## Problem

Give the palette's future "Documents" quadrant something real to show: a client-side, identity-scoped
index of the 50 most recent distinct files (attachments and detected container paths) a user has
encountered in chat — populated by real message traffic, not a stub — and a single reusable preview
component so a palette document selection, the existing thread path-link click, and the existing
message attachment expansion all render through the same code. Phase 3 does not mount anything at the
palette/page level (phase 4's job); it only builds the store, the capture hooks, and the extracted
viewer, and proves both existing entry points still work correctly through it.

## Approach

- **`utils/chat-file-links.ts`** — single source of truth for "what is a file": the container-path
  regex and `EXTENSIONLESS_FILES` set moved out of `chat-message.ts`'s `ENTITY_PATTERNS`, plus
  `parseContainerPath`/`buildFileApiUrl` moved out of `chat-thread.ts` (both re-exported from their
  original homes for existing callers/tests). New: `extractContainerPaths`, which additionally
  excludes any match sitting inside an `http(s)://` destination — ptone's "files only, never URLs"
  decision — and `resolveMessageProjectId`, a pure function extracted from `chat-thread.ts`'s
  `resolvePathLinkProjectId` doc comment/logic so path clicks and the recorder share one resolution
  order (thread project; DM: `senderProjectId` → `projectId` → peer-agent project → none).
- **`client/chat-recent-files.ts`** — the identity-scoped singleton. `setScope`/`ingest`/`snapshot`/
  `subscribe`/`clearForLogout`. Identity dedupe (attachment by ID; path by project+kind+dirName+
  filePath, so the two shared-dir spellings collapse to one identity) with newest-`sentAt`-wins and a
  deterministic tie-break; a 50-item cap shared across attachments and paths; a provisional/correction
  path for an own-send's client-guessed timestamp (the send response never carries the server's
  `createdAt`) being replaced by the later authoritative copy even when the corrected time is earlier;
  localStorage persistence keyed by `[origin, baseUrl, userId]` with cross-tab `storage`-event merging
  and a scope-generation guard so a response from a superseded identity can never repopulate a
  different account's store.
- **`components/shared/chat/chat-file-preview.ts`** — the reusable `<scion-chat-file-preview>`,
  replacing both `chat-thread.ts`'s path-link viewer and `chat-message.ts`'s attachment-expansion
  overlay. Each target change starts a new generation and `AbortController`; a response for a
  superseded generation is discarded without ever publishing a stale error (the phase-1 lesson on
  cancelled/superseded loads, now applied to nested-shadow-root preview state instead of a palette
  data controller). Image previews are now fetched (not linked directly) so 403/404/offline states are
  uniform across image/text/markdown/code, with object-URL revocation on replacement/close/disconnect.
- **Capture hooks in `chat-thread.ts`**: initial v2 history, history/backfill/around-message merges (a
  single `recordRecentFilesForHistory` helper covers `fetchHistoryV2`, `runBackfillV2`, and
  `fetchAroundMessage` — all three share the same response shape), `onChatMessageReceived`'s full-
  payload branch, and `handleChatSendV2`'s success branch (provisional, corrected later). Each async
  hook captures the store's scope generation *before* its request starts, not after it resolves.
- **Logout lifecycle**: `client/main.ts` calls `chatRecentFiles.setScope()` alongside the existing
  `chatNotifications.start()` call (once the authenticated user's ID is known), and
  `chatRecentFiles.clearForLogout()` inside the existing `ACCOUNT_TEARDOWN_EVENT` listener, gated on
  `reason === 'logout'` specifically (an auth-expiry teardown may resume the same account after
  re-auth, so it doesn't clear recents).
- **`e2e/chat-file-preview/`** — a new Chromium fixture (mirrors `e2e/chat-palette`'s convention)
  mounting the real `<scion-chat-thread>`/`<scion-chat-message>` with endpoint-shaped request
  interception, proving the migrated viewers still render real image/text/markdown/code content
  through the extracted component's nested shadow root — the composedPath/event-retargeting scenario
  happy-dom cannot simulate.

## A real bug found by mutation testing

`persist()`'s original read-merge-write against localStorage used the same newest-`sentAt`-wins
comparator as the cross-tab merge path. That's wrong for its own writes: a provisional own-send record
gets corrected in memory by `applyOne`'s messageId-matched override (even to an *earlier* timestamp,
by design), but `persist()` then re-read the stale on-disk copy of this tab's own earlier write and
the naive comparator judged the on-disk (provisional, "newer" by the wrong metric) copy as winning —
silently undoing the correction on every write. Fixed by giving `persist()` its own
`reconcileOwnWrite()`: in-memory records win for any key this store already tracks; only genuinely new
keys (a concurrent write from another tab this store hasn't merged yet) are pulled in from disk. The
naive `mergeRecords()` is now used only for reconciling an incoming `storage` event from another tab,
where neither side has "authority" over the other and newest-wins is the right rule.

A second, smaller gap: `applyOne`'s changed/unchanged return only compared `sentAt`/name/target, so a
tie-break-only replacement (identical `sentAt`/name/target, different `conversationKey`) updated
`this.records` in memory but silently skipped `persist()`/`notify()`. Fixed by including
`messageId`/`conversationKey` in the comparison.

Both found by systematically mutating every guard/branch in the new production code and confirming a
test failed (see `evidence/phase3.md`'s mutation table); several other mutations found *test* gaps
over already-correct code (e.g. a storage-event key-match guard whose only test used an
already-invalid payload, masking the guard itself), closed with new tests before reporting done.

## Scope boundaries kept

No palette/page-level mounting: the Documents quadrant, its candidate mapping, and the page-level
preview host are phase 4's job. `chat.ts` was not touched. No change to `chat-palette-types.ts` — the
new `RecentFile`/`PreviewTarget` types live in their own modules (`chat-recent-files.ts`,
`chat-file-preview.ts`) per the brief, so phase 4 can reference them without this phase needing to
touch the shared types file.

## Gate results

| Gate | Result |
| --- | --- |
| `npx tsc --noEmit` (project + `e2e/chat-file-preview/tsconfig.json`) | Clean |
| `npx vitest run` (this phase's new/changed test files) | 258/258 passed |
| `npx vitest run` (broader chat regression sweep: `src/components/shared/chat`, `chat-recent-files.test.ts`, `chat-file-links.test.ts`, `chat.test.ts`) | 493/493 passed |
| Chromium: `e2e/chat-file-preview/extraction.pw.ts` | 6/6 passed; `--repeat-each=10` → 60/60, no flakiness |
| `npm run lint` (phase-touched files) | Clean; `chat-thread.ts`/`chat-message.ts` pre-existing lint debt unchanged (diffed against the base commit) |
| `npm run build` | Clean |
| Real-API smoke (local ephemeral hub, `--dev-auth --enable-test-login`) | Real attachment upload+download byte match; real project-scoped workspace-file upload+download byte match; 404 for a missing file/project/attachment; 403 for a genuinely unprivileged real user (minted via `--enable-test-login`, since the dev-auth token itself is an unscoped super-admin) against both a project's workspace file and a project-scoped attachment |

Full evidence, exact commands, and output excerpts:
`/scion-volumes/scratchpad/projects/native-chat/palette/evidence/phase3.md`.

## Addendum: review round 1

A first independent review (`nc-palette-p3-rev`) returned REQUEST CHANGES: dialog sizing regressed
during extraction (restored, and its width is now Chromium-asserted); several concurrency guards,
capture-hook wiring paths, and persistence/validation/sync rules had no test that failed when the
underlying guard was removed (all now individually discriminated, ~40 new tests, each
mutation-verified); a live SSE message with no timestamp could incorrectly "correct" a pending
provisional record (fixed — capture is skipped when the payload carries no real time); a
mention-fan-out copy could be recorded (fixed — excluded, matching the display filter); and
container-path handling was hardened against malformed/unsafe input at every layer (recorder,
parser, and hydrate/storage validation), with the URL builder independently re-validating before
constructing a request. One evidence citation was corrected to describe what its test actually
proves. Full disposition table and mutation results:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase3-r1-response.md`.

## Addendum: review round 2

A second independent review (`nc-palette-p3-rev-2`) returned REQUEST CHANGES: the project identifier
placed into a file-route URL and into persisted records was not validated the same way a file path
already was, so a `.`/`..` value could change which route a request actually reached (now validated
identically at every layer it's used, with a parametrized test table and each condition
mutation-verified); a send, or a page of history, whose response resolved after the thread had moved
on to a different conversation could be attributed to the wrong conversation/project (fixed by
snapshotting the conversation and resolved project before the request starts, and by re-checking
identity again after the response body is parsed); the attachment-expansion overlay was rebuilding
its target on every render, which the extracted preview reads as "switch files," so an unrelated
re-render silently refetched and reset it (fixed by building the target once and holding it in
state); the malformed-input test matrix from round 1 had gaps and a few cases masked by an unrelated,
coarser check, now completed and unmasked at every layer named; several of round 1's own
mutation-testing survivors were still open, now closed (two are documented in the test file itself as
genuinely impossible to isolate through the public API, rather than forced into a misleading test);
and two claims in round 1's own written response were checked again and found to be inaccurate,
corrected in place with the honest re-verified result. Full disposition table and mutation results:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase3-r2-response.md`.

## Addendum: review round 3

A third independent review (`nc-palette-p3-rev-3`) returned REQUEST CHANGES, plus a standing
requirement that any function building a viewer/preview fetch URL independently re-parse its own
output and check it against the exact route shape it's meant to produce, on top of whatever validation
already ran beforehand. The review found one identifier reaching a URL had none of the protections a
similar one already had (now validated and route-checked the same way, with the URL builder moved
next to its sibling so both live under one shared pattern instead of drifting apart per component);
the malformed-input test matrix still had a few incomplete or masked layers (completed and unmasked);
an unsafe value could crash the viewer's own render with no way to dismiss or retry (fixed — the
same value is now handled the same defensive way in both places that use it); two race-guard paths had
no test proving they actually mattered (both pinned); and, a second time this phase, some claims in an
earlier response and in the evidence file didn't match what re-running them actually showed (corrected
in place, with every number in this round's response pasted directly from command output rather than
carried forward from an earlier round). Full disposition table, an inventory of every URL-building
site and every field that can reach one (written before this round's code changes, per the review's
own instruction), and mutation results:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase3-r3-response.md`.

## Addendum: review round 4

A fourth independent review (`nc-palette-p3-rev-4`) returned REQUEST CHANGES. The sink-side URL-shape
pin added the previous round checked a normalized copy of a builder's output rather than the raw
string, so a traversal that rebuilds a same-shape route — including one retargeting a different
project or shared directory entirely — passed the pin undetected; the pin now splits the raw string
itself and pins every segment a builder already knows the expected value of to that exact literal, with
a final raw-vs-normalized equality check as a backstop. Each builder's own call to the pin had no test
that would fail if the call were deleted; each is now proven independently, both directly and through
the extracted viewer, by giving the builders an internal, unvalidated core the tests can call with
source-side validation skipped. A stale identifier and an internal builder error reaching the user in a
dialog are both fixed. For the fourth time this phase, some claims in an earlier response and the
evidence file didn't match what re-running them showed; all are corrected in place, each checked
against freshly-run output. Mid-round, the hygiene rule from the previous addendum was expanded to also
cover hidden round/finding IDs in identifiers, narrative about an earlier state of the branch, and
design-doc/acceptance-criteria/scratchpad-path pointers in source; the whole phase's diff was swept
again against the expanded rule, reading every added comment, test title and identifier by hand. The
branch was also rebased onto an amended upstream phase-1 head during this round, with every gate
re-run in full on the rebased head before pushing. Full disposition table, mutation results including
three delete-and-restore proofs, and the evidence corrections:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase3-r4-response.md`.

## Addendum: review round 5

A fifth independent review (`nc-palette-p3-rev-5`) returned REQUEST CHANGES. The WHATWG URL parser
deletes every ASCII tab, CR and LF from its input before resolving dot segments, so a raw dot
segment split by one of those characters becomes a real `..` under normalization even though the
per-segment decode-and-compare check never sees it (the character is untouched by decoding and the
comparison only matches literal `.`/`..`); the sink pin's own raw-vs-normalized equality check was
already the sole defence against this, but nothing tested it and a comment wrongly called the check
not uniquely load-bearing. Both are now fixed: dedicated tests prove the equality check alone blocks
the class, with a mutation proof that removing it (or just its path-equality half) reopens the gap,
and the comment states plainly that this check is the sole defence here. The same treatment applies
to a raw trailing fragment delimiter, which only an earlier raw-string scan catches. A second
finding showed that a pin's literal-value slots, previously described as catching a same-shape
retarget, are in fact tautological — a builder always compares its own value against itself — so the
claim is corrected wherever it appeared, attributing the real protection to the two checks named
above. Several stale counts in the evidence file and a prior round's response are corrected against
freshly re-run output, four more hygiene references found on a repeat sweep are fixed, a
contradictory test title is corrected, and an unreachable guard is deleted. The branch was rebased
once this round, onto phase 1's declared final head (the previous rebase, onto an amended phase-1
head, was round 4's), with every gate, including both Chromium suites stress-tested at ten repeats,
re-run in full afterward. Full disposition table, mutation results, and a claims audit backing every
coverage, equivalence and count claim with a freshly-run command and its output:
`/scion-volumes/scratchpad/projects/native-chat/palette/reviews/phase3-r5-response.md`.
