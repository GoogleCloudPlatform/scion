# webperf C6: log viewers with sorted insert, rAF batching and keyed rows

**Date**: 2026-10-19
**Issue**: ptone/scion#5310 (epic #5301)

## Summary

The agent log, unified (admin diagnostics) log and agent message viewers used to
rebuild their whole buffer on every streamed line. They copied the dedup map into an
array, sorted it with `new Date()` inside the comparator, rendered the line straight away,
and formatted every row's timestamps on every render. A busy stream at the 2000-entry
cap therefore paid roughly O(n log n) sorting plus 2n `Intl.DateTimeFormat#format` calls
per line.

Changes:

- **New `web/src/client/sorted-log-buffer.ts`**:
  - `SortedLogBuffer<T extends { ts: number }>` is an ascending array plus a key map. It appends in O(1) when `ts >= last.ts`, otherwise binary-inserts (after equal timestamps), and trims to `cap` with one `splice` that keeps the newest entries. NaN timestamps sort first.
  - `FrameBatcher` coalesces render requests to one per `requestAnimationFrame`.
- **Viewers**: each buffered entry is wrapped in a row object that holds the numeric `ts`, the formatted strings, and the zone they were formatted in. All of it is computed at ingest. Render reuses the strings and re-formats only after a display-zone change. Rows render through `repeat(..., insertId)`, with each item template emitting its date divider and detail row. Newest-first viewers iterate the ascending buffer backwards.
- **Unified viewer**: the "new entries below" counter is now a plain accessor that schedules the frame batch. It used to be `@state`, and the stream listener bumped it per line, which would have forced one render per line while the user was scrolled up.

## Decisions (agreed with webperf-em)

- **Apply immediately, defer only the render.** The design said to buffer incoming lines and apply them per frame. Browsers do not run rAF in hidden tabs, so a pending queue would grow without bound in a background tab. Each line therefore goes into the capped buffer right away, and only `requestUpdate` waits for the frame.
- **Format count = fields × new entries.** Each row has two formatted fields: date divider and time, or time and tooltip. The acceptance test asserts `2 × new entries` and checks that the count does not grow with re-renders.
- **Stream code untouched.** Every `startStream`/`stopStream` block is byte-identical to main. The listeners still call `mergeEntries`/`mergeHubMessages`, so the #5238 fold (which keeps log, message and diagnostics streams separate anyway) cannot conflict.

## Tests

- `client/sorted-log-buffer.test.ts`:
  - in-order appends are O(1): a counting `ts` getter shows at most 2 reads per append;
  - out-of-order lines land at the right index;
  - cap trimming keeps the newest N;
  - plus checks for dedup, NaN, clear, and `FrameBatcher`.
- `components/shared/log-viewer-batching.test.ts`, per viewer, driven through a fake `EventSource` so the real listeners run:
  - "500 streamed lines inside one frame produce exactly 1 render": a `render` spy shows 0 renders while the lines arrive and 1 after the frame;
  - the `Intl.DateTimeFormat` format count (counted through the prototype's `format` getter) equals new entries, not entries × renders;
  - keyed rows: each line keeps its own DOM node when lines are inserted before and after it (checked per key; fails if `repeat` is replaced by `map`);
  - one re-format per row after a display-zone change;
  - disconnect cancels the pending frame (no render or scroll on the detached element), and reconnect renders the skipped lines.
- The existing `log-viewer-timezone` and `agent-message-viewer` tests now ingest through `mergeEntries`/`mergeHubMessages` and run rAF synchronously. They used to assign arrays to the removed `entries`/`messages` fields.

## Learnings

- A pending animation frame retains its element. In a hidden tab rAF never fires, so a removed viewer stays alive until the tab is visible again. Cancel the frame in `disconnectedCallback`. Lit does not re-render on reconnect, so `connectedCallback` must request an update for anything the cancelled frame skipped.
- A keyed-rows test must check node identity per key, with an insertion in the middle. Lit's unkeyed rendering also reuses nodes by index, so a simple "old nodes are still there" check passes without `repeat`.
- `Intl.DateTimeFormat.prototype.format` is an accessor (a getter that returns a bound function), so `vi.spyOn(proto, 'format')` does not work. Redefine the getter and count its accesses instead.
- Lit already batches synchronous updates into one microtask. A "1 render per burst" test therefore has to let a microtask pass between lines (`await el.updateComplete`), or it passes even without batching. The new tests fail on the old code: 7 of 14.
