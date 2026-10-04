# tz-refactor task 21b: format scan with no allowlist

Refs ptone/scion#2514, ptone/scion#2457. Part (a) landed as GoogleCloudPlatform/scion#2374.

## What changed

- `web/src/utils/format-scan.test.ts` has no allowlist any more. The only file
  allowed to contain a banned token (`toLocale*String(`, `Intl.DateTimeFormat`,
  `Intl.RelativeTimeFormat`, `hour12`) is `web/src/utils/time.ts` (AC17).
- Removed `ALLOWLIST`, its skip in the scan loop, and the two tests that guarded
  the list's contents.
- The main test is named after the AC17 rule. A separate test asserts that no
  file other than `utils/time.ts` contains `hour12`, and a small guard checks
  that the exempt path still exists, so a rename of `time.ts` cannot silently
  leave the exemption pointing at nothing.
- The header comment now describes the single exemption instead of a shrinking
  list. The `BANNED_PATTERN` self-tests and the `formatNumber` /
  `isValidTimeZone` non-flag test are unchanged.

## Checks

- No other allowlist or exception mechanism for this scan exists in `web/src`,
  `web/scripts`, `hack/`, the Makefile or CI, and no doc in `web/AGENTS.md`,
  `docs-site/` or `.design/` (outside the project log) describes one.
- The midnight test plan item is already covered by
  `web/src/components/shared/log-viewer-timezone.test.ts` (from part a) for the
  agent log, agent message and unified log viewers, so no new test was needed.
- `npx vitest run src/utils/format-scan.test.ts src/components/shared/log-viewer-timezone.test.ts --maxWorkers=2`: 13 passed.
  With `hour12` temporarily appended to a non-exempt file, both scan
  assertions fail as expected.
- `npm run typecheck`: clean. `prettier --check` on the changed file: clean.
  `eslint` cannot parse any `*.test.ts` file, because `tsconfig.json` excludes
  them from the typed-lint project. That is true on upstream main too and is
  unrelated to this change.

## Addendum: chat compact ages through formatRelative

The scope grew to include the chat compact-age swap that tz-refactor task 21
part (a) deferred until `formatRelative` gained a `style` option.

- **What changed.** `chat-members.ts` `formatRelativeTime` and `chat-search.ts`
  `formatTime` now render ages under 7 days, and future instants, with
  `formatRelative(iso, { style: 'narrow' })`. Older instants keep their
  absolute-date branches and titles unchanged.
- **Behaviour deltas** (narrow `en` output verified in this Node):
  - Strings: "5 min ago" / "3 hr ago" in members, and "5m ago" / "3h ago" in
    search, both become "5m ago" / "3h ago". "just now" (members) and "now"
    (search) both become "now". "59s ago" now appears under a minute.
  - Rounding: the old ladders floored each value. `formatRelative` uses
    `Math.round`, so 59m40s reads "1h ago", 23h40m reads "yesterday", and
    6d14h reads "7d ago" while still on the relative branch. An exact past
    half rounds toward zero (59.5 minutes reads "59m ago").
  - Future instants used to read "just now"/"now"; they now read "in 5m".
    Other list views built on `formatRelative` clamp future instants to
    "just now" as clock skew, so this is the one difference from them.
- **Tests.** `chat-relative-dates.test.ts` uses a fixed clock with boundary
  cases for both components (now, 59s, 59m40s, the exact half, 23h40m, 6d,
  6d14h, +5m, and the switch at exactly 7 days). `chat-members.test.ts`
  expects "10m ago".
- **Other ladders.** A grep of `web/src` found no other hand-rolled "ago"
  ladder. The remaining relative-time helpers already call `formatRelative`.
