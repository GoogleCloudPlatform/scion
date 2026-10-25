# Starred messages, phase 1: one complete thread-message slice

**Date:** 2026-10-23
**Issue:** ptone/scion#5798 (epic ptone/scion#5797)
**Experiment:** web.chat_starred_messages

## What shipped

- **Store:** `webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at)` in both
  chat stores. The add is a guarded insert (message exists, in the conversation's history scope, not
  deleted, topic live) that keeps the first `starred_at`. SQLite stores `starred_at` as fixed-width TEXT
  (nine fraction digits) so the keyset compares lexically; PostgreSQL uses TIMESTAMPTZ.
- **Routes:** `/api/v1/chat/stars` (list), `/stars/state`, `/stars/{messageId}` (PUT, DELETE, GET),
  404 while the experiment is off. Every read and add resolves current read access through the shared
  read-target resolver (`chat_read_target.go`, extracted from history so the two cannot drift). DELETE is
  ownership only and always 204.
- **Realtime:** a content-free `user.<id>.chat.stars` invalidation; clients refetch.
- **Web:** the `ChatStarsController`, the menu toggle, the indicator and the header list with jump.

## Things worth knowing

- **utc-timestamp-normalize** takes every `*_at` column of every `webchat_*` table on SQLite. The star
  table's `starred_at` is excluded by name (`webchatFixedWidthTimeColumns` in entadapter); any future
  fixed-width time column must be added there, or the maintenance run would trim it.
- **Shared enumerations** a new route or experiment must update, which are easy to miss: the
  live-inventory suffix exclusions
  (`authzop_catalog_method_inventory_test.go`), the bearer disposition matrix, the route classification
  map, and the perf-budget web fixture (`web/e2e-perf/budgets/fixture-schema.json` and `fixture.mjs`,
  which list every web-layer experiment).
- **State requests:** the controller fetches star state once per history load (initial, older, around,
  catch-up), never per live message; a live message starts unstarred unless a refresh is due or failed.
  A state answer never overwrites a newer local mutation or an invalidation that arrived while it ran.
- **`/stars/state`** answers the resolver's error for a single-conversation batch and leaves out an
  unresolved conversation only when the batch mixes conversations.
- **The root Playwright suite** (`web/playwright.config.ts`, real hub) is not run by upstream CI, so a
  spec there can sit unrun. Its global setup runs `make build` and `npm run build` itself (there is no
  prebuilt switch) and launches its own browser with Playwright's defaults (the headless shell), which
  ignores the config's `executablePath`.
- **Writing a real-hub chat spec:** seeding through the send API meets the per-sender limit (30 sends a
  minute for a human; honour the 429 Retry-After); `#msg-<id>` is the `<scion-chat-message>` element
  itself, not a wrapper; at 768 px or narrower a message's menu is the bottom action sheet
  (`scion-action-sheet[open] .item`), not the `.context-menu-item` popup. The mocked chat harness
  (`web/e2e/chat-mobile`) reproduces the UI steps without building the hub.
- **Run the repo's static guards and the small package suites before the heavy ones.** Two late
  fixes in this phase came from checks that cost seconds, found after the long pkg/hub shard runs:
  - `hack/check-hub-store-reassign.sh` rejects a test that swaps `srv.store` mid-test (audit
    goroutines started during setup read it). Simulate store faults with `installStoreFault` /
    `testServerWithStoreFault` from `pkg/hub/store_fault_helpers_test.go`, installed right after
    the server is built and before the first request, and call `Arm()` where the test needs the
    fault.
  - `pkg/util/logging` `TestRequestPathSites` lists every place a handler reads the request path. A
    new route handler that parses `r.URL.Path` needs a `requestPathSites` entry with the accurate
    class (routing, comparison, ...), the same as its sibling handlers; a path that reaches a log
    call must go through `logging.RequestPath`.
- **Shard placement moves with file size.** A small test edit moved `handlers_chat_stars_test.go`
  from pkg/hub shard 5 to shard 6 and shifted other tests between shards; re-locate targets with
  `hack/hubshard` at the exact commit before choosing which shard to run.

## Follow-ups

Later phases of the epic (ptone/scion#5797) carry the open items, including the utc-timestamp-normalize
operation's seeded description, which still says it rewrites every webchat time column.
