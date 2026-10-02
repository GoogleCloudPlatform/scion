# tz-refactor task 8: real-binary timestamp contract test

Closes ptone/scion#2501. Refs ptone/scion#2457. Design: tz-refactor design §2.2 "Contract test", AC1.

## What changed

- New `pkg/hub/tzcontract` (build tag `tzcontract`, test-only). `TestTimestampContract` builds
  `cmd/scion` (or uses `SCION_TZ_CONTRACT_BIN`) and starts
  `scion server start --foreground --enable-hub --enable-runtime-broker=false --dev-auth` with a
  minimal environment (`PATH`, `HOME`, `TZ`, plus the Postgres driver/URL env vars). SQLite always
  runs. Postgres runs when `SCION_TEST_POSTGRES_URL` is set; each case gets a fresh database,
  which is dropped afterwards.
- `TZ=Asia/Tokyo` case: it writes these instants, then reads them back over REST and SSE:
  - project and agent times;
  - an agent message (REST list and the `agent.<id>.message` SSE event);
  - a chat-v2 topic, message (send response, history, `project.<id>.chat.message` SSE), topic
    `lastActivityAt` and message edit `editedAt`;
  - a scheduled event with a `+02:00` `fireAt` (create, GET, list);
  - a notification (REST and the millisecond `notification.created` SSE event);
  - a role definition and binding;
  - an access-constraint preview and an access-constraint `appliesWhen` window sent with `+02:00`.

  Each value must be in UTC with a `Z` suffix, non-zero, and the same instant (to the microsecond)
  or inside the write window. Every structured server log line must have a `Z` time.
- SQLite only: after the Tokyo case, the hub is stopped and legacy rows are seeded:
  - `projects.created` as ent's old `'… +0900 JST m=…'` text;
  - three `webchat_topic` rows with `created_at`/`last_activity_at` in the three legacy forms
    (RFC 3339 `+09:00`, `String()` `JST m=`, `String()` `+0545 +0545`);
  - `webchat_message_ext.edited_at` in the `JST m=` form.

  The hub restarts under Tokyo and each row must come back as `Z` at the right instant.
- `TZ=Asia/Kathmandu` case: three running agents, a `+02:00` scheduled event and a chat message.
  The hub is stopped, then one agent's `last_seen` and another's `last_activity_event` are moved
  back 10 minutes. After restart, the startup scheduler pass must mark exactly the stale agent
  `offline` and the stalled agent `stalled`, and the healthy one stays `working`. On SQLite, every
  ent `DATETIME` value must be in canonical UTC `String()` form, and every `webchat_*` `*_at` value
  must be RFC 3339 `Z`. The scan runs before and after the restart.
- `make test-tz-contract` runs it. The target fails if `SCION_TEST_POSTGRES_URL` is set but the
  Postgres cases did not pass. CI runs it as one extra step in the existing Postgres job
  (`launch-store-postgres-tests`).

## Scope substitutions (accepted by the engineering manager)

- **Webchat touch → chat-v2 topic activity and message edit.** `TouchThread`/`RecordChannel` are
  reachable only from `/api/v1/broker/inbound`. That handler dispatches to a live broker transport
  before it touches the thread, and no API sets a broker endpoint without a running broker.
  `GetThreads` has no route, and attachment `createdAt` is never served. The test instead covers
  `webchat_topic.last_activity_at`/`created_at` (thread list) and `webchat_message_ext.edited_at`
  (history). Both read through `parseSQLiteTime` → `parseGoTimeString`. The thread and attachment
  legacy reads are covered by the tz-refactor task 3 unit tests in
  `pkg/hub/webchannel_store_time_test.go`: `TestWebChatTime_TouchThreadRoundTrip`,
  `TestWebChatTime_RecordChannelStoresUTC`, `TestWebChatTime_AttachmentRoundTrip`,
  `TestWebChatTime_LegacyThreadRows` and `TestWebChatTime_LegacyRowsThroughParseSQLiteTime`.
- **Legacy seeds** use those same tables (plus one ent `projects` row) for the same reason.
- **Policy `validFrom` → access-constraint `appliesWhen` window.** The policy API returns 410, so
  there is no policy write path. The access-constraint window is the remaining user-supplied
  validity window normalised at ingest (tz-refactor task 4).
- **fireAt: no handler or store edits.** The `+02:00` `fireAt` passes as-is: the create handler
  re-fetches from the store, and the tz-refactor task 2 ent mutation hook normalises the write.
  `pkg/hub/handlers_scheduled_events.go` and `pkg/store/entadapter/schedule_store.go` are
  deliberately untouched (ptone/scion#2476).
- **Agent message REST read.** The agent message is read back from `GET /api/v1/agents/{id}/messages`.
  `GET /api/v1/messages` is the caller's own inbox (recipient = the user), so it never lists a
  user-to-agent message. Putting an agent-to-user message in it would need an agent credential.

## Notes from building it

- Postgres keeps microseconds, while some responses and SSE events echo the in-memory nanosecond
  value. Instant equality is therefore checked to the microsecond.
- With Postgres, a project-scoped SSE subject needs a new LISTEN channel, which the listener adds
  on a one-second poll. The test waits 2.5 s after subscribing.
- Constraint creation needs a direct `access_constraint.admin` holder at the scope. Its
  most-restrictive check treats a future window as active, so the test binds the dev user to a
  project role and keeps `access_constraint.admin` in the constraint's maximum permissions.

## Prove-It

The pin seam was replaced with a no-op (`var pinProcessUTC = func() { _ = util.PinProcessUTC }`)
and the binary rebuilt. Both backends and both TZ cases then failed, with 64 assertion failures:
`+09:00`/`+05:45` values on project, agent, message, chat, scheduled-event `fireAt`, notification,
preview and access-constraint fields, and local-zone server log times. The thresholds, the SQLite
readability scan and the legacy reads still passed without the pin, because the tz-refactor task 2
store boundary covers them. The source was restored before committing.

## Tests

Run with test-process `TZ=UTC`, `Asia/Tokyo` and `Asia/Kathmandu` (the hub's TZ is set by the
test itself): all pass on SQLite and on Postgres 16.

## Follow-ups (not done here)

- A git-remote project created through `POST /api/v1/projects` had no `#general` topic in the
  Postgres run (seen, not investigated). The test creates its own topics.
