# Chat retention Phase 1: web

Web half of chat message retention Phase 1 (design: chat message retention
v2, sections 4.1, 7.1, 8.1 and 8.2). Built on the hub half
(`hub.chat_retention`, `/api/v1/admin/chat-retention`, the history
`retention` marker and `unavailable` reply previews).

## What changed

- `web/src/components/pages/admin-chat-retention.ts`: a new
  `<scion-admin-chat-retention>` panel in the Hub Server tab of Admin > Server
  Config. It renders only while the `hub.chat_retention` web flag is on
  (`CHAT_RETENTION_FLAG` in `utils/feature-flags.ts`) and loads when the flag
  turns on after mount. Fields: the enable switch, the retention window
  (empty = keep forever, sent as an explicit `null`; 7 to 36500 days) and
  the four sweep tuning values in a collapsed native `<details>` Advanced
  area. The last applied cutoff is shown read-only. Saves send every field
  with `expected_revision`; a 409 reloads the current values, a 400
  `validation_failed` lists the hub's per-field errors, and a 404 says the
  experiment is off on the hub. Form validation mirrors the hub's bounds.
  A warning states the window that will be applied when the sweep is
  enabled with a window. The panel is its own component so the 7400-line
  server config page gains one tag.
- `chat-date-divider.ts`: `retentionDividerText` and `renderRetentionDivider`
  ("Messages before <date> are removed by this hub's retention policy."),
  with the date in the date dividers' format and display zone.
- `chat-thread.ts`: keeps `retention.purgedBefore` from the oldest history
  page (the page without `nextCursor`, also an around or refetched window)
  and renders the divider above the first row, and on an otherwise empty
  thread. Not shown in the starred-only center or with the flag off; reset
  on conversation switch and on jump to latest.
- `chat-message.ts`: `ReplyPreviewInfo` (with `unavailable?`). An
  unavailable preview renders as a muted, italic quote with no sender and no
  click handler (no jump to a message that no longer exists).

## Tests

- `admin-chat-retention.test.ts`: body building and bounds, hub error
  parsing, flag gating (including the flag turning on after mount), save
  with `expected_revision`, client and hub validation errors, 409 reload,
  404.
- `chat-thread-retention.test.ts`: divider shown on the oldest page and on
  an emptied thread; hidden without a marker, with older pages left, with
  the flag off and after switching conversation; the unavailable preview
  reaches the message and renders muted.
- `chat-message.test.ts`, `chat-date-divider.test.ts`: extended.

## Findings

- In a container, `ulimit -v` breaks node and vitest (V8 reserves large
  PROT_NONE ranges; vitest aborts with "memory allocation of 224 bytes
  failed"). `ulimit -d` (RLIMIT_DATA) works as a memory cap for node and
  chromium.
- The repository-wide secret-shape scan flags
  `admin-server-config.ts` for an existing private-key placeholder string
  in the GitHub App key input; it predates this change.

## Verification

- `npm ci`, `npm run typecheck`, `npm run lint` (0 errors) and
  `npm run format:check` pass.
- `npx vitest run --pool=forks --maxWorkers=2` on the new and touched test
  files plus `admin-server-config*` and `shoelace-registration`: 8 files,
  430 tests passed.
- Live check: a loopback hub built from this branch merged with the hub half
  (hosted mode, Hub and web only, dev auth, isolated HOME). Steps: seed a
  topic with four messages, the third replying to the first; backdate the
  first two messages (20 and 15 days) and the topic and conversation
  (30 days) in SQLite; turn on `hub.chat_retention`; set a 7-day window
  with a 1-minute tick. The sweep purged the 2 messages. The history page
  returned `retention.purgedBefore` and an `unavailable` preview. The
  panel, the divider and the muted quote rendered as intended in
  headless chromium.

## Notes for the live setup

- In hosted mode dev auth does not create a browser session on its own:
  POST `/auth/dev-launch` with the dev token returns a one-time
  `/auth/dev-signin` link that does.
- The topic route is `/chat/<project-slug>/<topic-id>`; a project ID in
  place of the slug opens the chat page with no thread selected.
- SQLite stores `messages.created` and `conversations.created_at` as Go
  `time.Time` strings (`2006-01-02 15:04:05.999999999 -0700 MST`) and
  `webchat_topic.created_at` as RFC 3339; backdating must keep each format.

## Possible follow-up

- The panel's warning reads "Once saved, messages older than N days are
  permanently deleted" even when that setting is already saved; "While
  enabled, ..." would be more accurate.
