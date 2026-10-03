/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * Timezone-aware date/time formatting and parsing for the web client
 * (tz-refactor task 11, design.md §2.4 "Edge formatting and parsing").
 *
 * This is the **only** module allowed to call `Intl.DateTimeFormat`,
 * `Intl.RelativeTimeFormat`, `toLocaleString`/`toLocaleDateString`/
 * `toLocaleTimeString` or `hour12`: a vitest source-scan test
 * (`format-scan.test.ts`) enforces this across the rest of `web/src`
 * (`Intl.supportedValuesOf` and `Intl.NumberFormat` are not banned there,
 * but every zone check, zone list and clock/relative-time formatter in the
 * web app should still import from here instead of constructing its own).
 *
 * **Clock and locale [decided, D4].** The locale is one fixed constant,
 * `FORMAT_LOCALE`, and every clock time uses `hourCycle: 'h23'` — never
 * `hour12: false`, which some engines map to `h24` for `en-US` (midnight
 * would render as `24:00` instead of `00:00`). Locale and 12/24-hour
 * *preference* are deferred (ptone/scion#1056, narrowed by this issue, never
 * closed by it).
 */

// ---------------------------------------------------------------------------
// Locale (D4: fixed; not read from `navigator.language`, deferred)
// ---------------------------------------------------------------------------

/** The one locale constant every formatter in this module uses (D4). */
export const FORMAT_LOCALE = 'en';

// ---------------------------------------------------------------------------
// Zone helpers
//
// `browserTimeZone`, `isValidTimeZone` and `listTimeZones` are owned by
// tz-refactor task 12 (ptone/scion#2533): task 11 rebased onto it and
// dropped its own (textually near-identical) copies in favour of these, per
// review round 1 finding R1-2. Any future change to these three belongs in
// task 12's lineage.
// ---------------------------------------------------------------------------

/**
 * Returns the browser's resolved IANA time zone (e.g. "America/New_York").
 *
 * This is the fallback used wherever a user has not chosen an explicit
 * display zone: `preferences.timezone`, else this value.
 */
export function browserTimeZone(): string {
  return Intl.DateTimeFormat().resolvedOptions().timeZone;
}

/**
 * Matches the IANA time zone name shape: every `/`-separated segment starts
 * with an uppercase ASCII letter (e.g. "Asia/Tokyo", "Etc/GMT+5", "CET").
 * Also rejects the empty string and a numeric offset ID like "+05:30" (no
 * segment starts with a letter at all).
 */
const IANA_NAME_SHAPE = /^[A-Z][A-Za-z0-9_+-]*(\/[A-Z][A-Za-z0-9_+-]*)*$/;

/**
 * Reports whether `zone` is a time zone name the server-side resolver
 * (Go's `time.LoadLocation`, used for `agent_defaults.default_timezone` and
 * the per-user display-timezone preference) would also accept.
 *
 * This is the one place in the web app allowed to probe `Intl.DateTimeFormat`
 * for this purpose; every zone-name check elsewhere should call this
 * function instead of constructing its own `Intl.DateTimeFormat`.
 *
 * `Intl.DateTimeFormat` alone is looser than `time.LoadLocation` in one way
 * this function corrects: `Intl` matches zone names case-insensitively
 * ("asia/tokyo", "utc" both resolve). `time.LoadLocation` does not.
 *
 * The check is a *shape* test, not a resolution-identity test: every IANA
 * name's `/`-separated segments start with an uppercase ASCII letter
 * (`IANA_NAME_SHAPE`), and `Intl` decides the rest (accept or throw). Two
 * simpler approaches were tried and found wrong:
 * - Rejecting a name only when it resolves to a case variant of *itself*
 *   misses a lowercase *alias*, which resolves to a *different* canonical
 *   string regardless of case ("asia/kolkata"/"Asia/Kolkata" both resolve
 *   to "Asia/Calcutta") — so a lowercase alias like "asia/kolkata" would
 *   still pass.
 * - Requiring a name to either resolve to itself or be an exact-case member
 *   of a small hand-picked alias set is incomplete by construction: V8/ICU
 *   canonicalizes every IANA *link* name to CLDR's own pick, and there are
 *   far more such links than any hand-picked set can cover — this falsely
 *   rejects dozens of names Go accepts, including current IANA canonical
 *   names like "America/Nuuk" and "Asia/Yangon" and the common "Etc/UTC"
 *   (measured 52 of 484 system zone names falsely rejected).
 *
 * The shape check above has none of that: measured 0 false rejects over
 * the same 484 names. It also makes an explicit offset-ID check redundant
 * (no segment of "+05:30" starts with a letter) and needs no denylist for
 * tzdata's own non-portable names ("Local", "localtime", "posixrules",
 * "Factory"): `Intl.DateTimeFormat` already throws for all four.
 *
 * **Residual false accept, accepted:** an unusual but shape-valid
 * capitalization `Intl` happens to still resolve case-insensitively (e.g.
 * "Utc", or an all-caps "ASIA/TOKYO") passes here but is rejected by
 * `time.LoadLocation`. The server's 422 remains authoritative for these;
 * they are not real zone names anyone would intentionally type.
 */
export function isValidTimeZone(zone: string): boolean {
  if (!IANA_NAME_SHAPE.test(zone)) return false;
  try {
    new Intl.DateTimeFormat('en', { timeZone: zone });
    return true;
  } catch {
    return false;
  }
}

/**
 * Returns the IANA time zone names the browser supports, plus "UTC" (in
 * case the runtime's list omits it).
 */
export function listTimeZones(): string[] {
  const zones = Intl.supportedValuesOf('timeZone');
  return zones.includes('UTC') ? zones : [...zones, 'UTC'];
}

// ---------------------------------------------------------------------------
// Effective-zone store (design.md §3 A (a))
//
// Formatters in this module read the *effective zone* from here instead of
// fixing it at module load. `setPreferredTimeZone` (called once from
// `/auth/me` at startup, again on an SSE reconnect's live re-check, and
// again locally after the user's own `preferences.timezone` PATCH) dispatches
// `DISPLAY_TIMEZONE_CHANGED_EVENT` on `window` when the value actually
// changes.
//
// That event does NOT re-render anything by itself (review R1-4): a
// formatter call only ever reads the *current* zone at the moment it runs.
// A long-lived Lit component that renders a time must subscribe to reflect
// a later change. Use `DisplayZoneController` (review R3-2; see
// `utils/display-zone-controller.ts`) rather than hand-rolling an
// addEventListener/removeEventListener pair in
// connectedCallback/disconnectedCallback — it is the one, leak-free,
// reused-by-every-P3-surface implementation of exactly that:
//
//   readonly _zone = new DisplayZoneController(this);
//
// For any absolute time that needs the zone label alongside it (not just a
// live re-render), use `formatInstantWithZone` below instead of composing
// `formatInstant`/`zoneLabel` yourself.
//
// `DisplayZoneController` is necessary but not sufficient for every
// consumer (review R4-1). It re-renders a component that calls a formatter
// fresh on every render — `requestUpdate()` is enough there, because the
// next render reads the new zone. It is NOT enough for a component that
// caches a *wall-clock string* derived from the effective zone in state —
// for example a `datetime-local` input's value, pre-populated once via
// `toWallClockInput` and then left alone. That cached string still shows
// the old zone after the event fires, while a later edit parses it (or an
// untouched sibling field) in the new one, silently shifting the instant a
// cached-but-unedited field represents. Such a component must also
// re-derive each cached string from the instant it represents — round-trip
// it through the *previous* zone back to an ISO instant, then back to a
// wall-clock string in the *new* zone — in `willUpdate`, not just call
// `requestUpdate()`. See `access-boundary-schedule-editor.ts`'s
// `rebaseCachedStrings` for the reference implementation. Track the zone
// those strings were derived in, and keep that tracked zone in sync at
// *every* lifecycle point that touches the cached strings (`willUpdate`,
// `connectedCallback`, a prop-change handler, ...) — not just `willUpdate`
// (review R5-1) — by always doing the same two things together, not one
// without the other (review R6-1): first round-trip any string you are
// *keeping* from the tracked zone to the current one, then (re)derive the
// rest from their instants, and only then record the current zone as
// tracked. A lifecycle point that updates the tracked zone without also
// rebasing a string it leaves untouched — e.g. a retained, uncommitted
// typed value with no backing prop — marks that string as already current
// without converting it, which silently corrupts it on the very next zone
// change.
// ---------------------------------------------------------------------------

/** The user's `preferences.timezone`, or `''` for Auto. */
let preferredTimeZone = '';

/**
 * Dispatched on `window` when `setPreferredTimeZone` actually changes the
 * stored preference (not on a no-op set of the same value). Carries no
 * detail; listeners call `effectiveTimeZone()`/`zoneLabel()` themselves.
 */
export const DISPLAY_TIMEZONE_CHANGED_EVENT = 'scion-display-timezone-changed';

/**
 * Sets the user's preferred display zone (`preferences.timezone`). Pass
 * `''`, `null` or `undefined` for Auto. A value that fails `isValidTimeZone`
 * — including one the hub rejected — falls back to Auto rather than
 * throwing, matching the backend contract (tz-refactor task 10,
 * ptone/scion#2526): "a value the hub rejects must make `isValidTimeZone`
 * fall back to Auto, never throw."
 *
 * Dispatches `DISPLAY_TIMEZONE_CHANGED_EVENT` on `window` when the
 * effective preference changes; a no-op set (including two different
 * invalid values, which both resolve to `''`) dispatches nothing.
 */
export function setPreferredTimeZone(zone: string | null | undefined): void {
  const next = zone && isValidTimeZone(zone) ? zone : '';
  if (next === preferredTimeZone) return;
  preferredTimeZone = next;
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new Event(DISPLAY_TIMEZONE_CHANGED_EVENT));
  }
}

/** Returns the raw preference (`''` means Auto), without the browser fallback. */
export function getPreferredTimeZone(): string {
  return preferredTimeZone;
}

/**
 * Returns the *effective* display zone: `preferences.timezone` if set, else
 * `browserTimeZone()` (design.md §3 A (a)). Every formatter in this module
 * that renders an absolute time uses this.
 */
export function effectiveTimeZone(): string {
  return preferredTimeZone || currentBrowserTimeZone();
}

/** `browserTimeZone()`, held until the current synchronous task finishes. */
let browserTimeZoneMemo: string | null = null;

/**
 * `browserTimeZone()` constructs an `Intl.DateTimeFormat` on every call, so
 * a render that formats a value per row (the file browser, ptone/scion#2382)
 * would build one per row whenever no preference is set. This holds the
 * value for the rest of the current synchronous task and drops it in a
 * microtask, so one render resolves the zone once while a later render still
 * sees a mid-session change of the operating-system zone.
 */
function currentBrowserTimeZone(): string {
  if (browserTimeZoneMemo === null) {
    browserTimeZoneMemo = browserTimeZone();
    queueMicrotask(() => {
      browserTimeZoneMemo = null;
    });
  }
  return browserTimeZoneMemo;
}

/** Label for "Times in: <zone>" affordances next to clocks and date inputs. */
export function zoneLabel(): string {
  return effectiveTimeZone();
}

// ---------------------------------------------------------------------------
// Absolute-time formatting
// ---------------------------------------------------------------------------

/**
 * `formatInstant` rendering styles:
 * - `'time'`: `18:00` (hour and minute only).
 * - `'time-seconds'`: `18:00:05` (log viewers, task 21).
 * - `'time-millis'`: `18:00:05.123` (log viewers that show milliseconds,
 *   tz-refactor task 21).
 * - `'date'`: `Sep 23, 2026` (no time).
 * - `'datetime'`: `Sep 23, 18:00` (no year — for compact UI like the
 *   inter-agent message marker).
 * - `'datetime-full'`: `Sep 23, 2026, 18:00` (with year — admin and
 *   access-boundary pages, token/expiry columns, task 20).
 *
 * Additive only: a later P3 issue (tasks 19-21) may add further styles here,
 * but must not change what an existing style renders, since that would
 * silently reformat every current caller.
 */
export type InstantStyle =
  | 'time'
  | 'time-seconds'
  | 'time-millis'
  | 'date'
  | 'datetime'
  | 'datetime-full';

/**
 * Formatters already built, keyed by style and zone. Constructing an
 * `Intl.DateTimeFormat` is comparatively expensive (locale data lookup), and
 * callers such as the file browser format a value per row on every render
 * (ptone/scion#2382), so each style/zone pair is built once and reused.
 * `FORMAT_LOCALE` is constant, so it is not part of the key; a zone change
 * simply selects (or builds) the new zone's formatter.
 */
const instantFormatterCache = new Map<string, Intl.DateTimeFormat>();

function instantFormatter(style: InstantStyle, zone: string): Intl.DateTimeFormat {
  const key = `${style}|${zone}`;
  let formatter = instantFormatterCache.get(key);
  if (!formatter) {
    formatter = buildInstantFormatter(style, zone);
    instantFormatterCache.set(key, formatter);
  }
  return formatter;
}

function buildInstantFormatter(style: InstantStyle, zone: string): Intl.DateTimeFormat {
  switch (style) {
    case 'time':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit',
      });
    case 'time-seconds':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      });
    case 'time-millis':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        fractionalSecondDigits: 3,
      });
    case 'date':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        year: 'numeric',
        month: 'short',
        day: 'numeric',
      });
    case 'datetime':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        month: 'short',
        day: 'numeric',
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit',
      });
    case 'datetime-full':
      return new Intl.DateTimeFormat(FORMAT_LOCALE, {
        timeZone: zone,
        year: 'numeric',
        month: 'short',
        day: 'numeric',
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit',
      });
  }
}

/**
 * Formats an ISO instant in the effective display zone (D4: always 24-hour,
 * `hourCycle: 'h23'`, never `hour12: false`). Returns `''` on an unparsable
 * `iso`.
 */
export function formatInstant(iso: string, style: InstantStyle = 'datetime'): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  return instantFormatter(style, effectiveTimeZone()).format(date);
}

/**
 * The canonical labelled form for an absolute time (review R2-4): formats
 * `iso` with `formatInstant` and appends the effective zone in parentheses,
 * e.g. `"Sep 24, 2026, 00:00 (Asia/Tokyo)"`. Tasks 19 and 20's AC ("a zone
 * label where absolute") should use this instead of composing
 * `formatInstant`/`zoneLabel` themselves, so every P3 surface uses the same
 * format — this PR's own native-chat migration open-coded this three times
 * before extracting it here. Returns `''` on an unparsable `iso`.
 */
export function formatInstantWithZone(iso: string, style: InstantStyle = 'datetime-full'): string {
  const formatted = formatInstant(iso, style);
  return formatted ? `${formatted} (${zoneLabel()})` : '';
}

// ---------------------------------------------------------------------------
// Relative-time formatting
// ---------------------------------------------------------------------------

/**
 * Format a date string as a relative time description (e.g. "3 hours ago").
 *
 * Uses `Intl.RelativeTimeFormat` for locale-aware output. Past only (positive
 * elapsed time); see `formatRelative` for a past-and-future version.
 * Returns the original string on parse failure.
 *
 * Kept for its many existing call sites across `web/src`; new code should
 * prefer `formatRelative`, which also handles future instants.
 */
export function formatRelativeTime(dateString: string): string {
  try {
    const date = new Date(dateString);
    if (isNaN(date.getTime())) return dateString;
    const diffMs = Date.now() - date.getTime();
    const diffSeconds = Math.round(diffMs / 1000);
    const diffMinutes = Math.round(diffMs / (1000 * 60));
    const diffHours = Math.round(diffMs / (1000 * 60 * 60));
    const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

    const rtf = new Intl.RelativeTimeFormat(FORMAT_LOCALE, { numeric: 'auto' });

    if (Math.abs(diffSeconds) < 60) {
      return rtf.format(-diffSeconds, 'second');
    } else if (Math.abs(diffMinutes) < 60) {
      return rtf.format(-diffMinutes, 'minute');
    } else if (Math.abs(diffHours) < 24) {
      return rtf.format(-diffHours, 'hour');
    } else {
      return rtf.format(-diffDays, 'day');
    }
  } catch {
    return dateString;
  }
}

/** Options for `formatRelative`. */
export interface FormatRelativeOptions {
  style?: Intl.RelativeTimeFormatStyle;
}

/**
 * Formats an ISO instant as a relative time description, for both past
 * ("3 hours ago") and future ("in 3 hours") instants. Zone-independent
 * (a duration, not a wall-clock time), so it uses only `FORMAT_LOCALE`.
 * Returns the original string on parse failure.
 *
 * `options.style` selects the `Intl.RelativeTimeFormat` style: `'long'`
 * (default, "5 minutes ago"), `'short'` ("5 min. ago") or `'narrow'`
 * ("5m ago"), for compact cells such as trays and dense tables.
 */
export function formatRelative(iso: string, options: FormatRelativeOptions = {}): string {
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;

  const diffMs = date.getTime() - Date.now();
  const diffSeconds = Math.round(diffMs / 1000);
  const diffMinutes = Math.round(diffMs / (1000 * 60));
  const diffHours = Math.round(diffMs / (1000 * 60 * 60));
  const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

  const rtf = new Intl.RelativeTimeFormat(FORMAT_LOCALE, {
    numeric: 'auto',
    style: options.style ?? 'long',
  });

  if (Math.abs(diffSeconds) < 60) return rtf.format(diffSeconds, 'second');
  if (Math.abs(diffMinutes) < 60) return rtf.format(diffMinutes, 'minute');
  if (Math.abs(diffHours) < 24) return rtf.format(diffHours, 'hour');
  return rtf.format(diffDays, 'day');
}

// ---------------------------------------------------------------------------
// Wall-clock parsing (`datetime-local` inputs)
// ---------------------------------------------------------------------------

interface WallClockParts {
  year: number;
  month: number; // 1-12
  day: number;
  hour: number;
  minute: number;
  second: number;
}

/**
 * Builds an epoch-ms timestamp from UTC field values. Unlike `Date.UTC`,
 * this does **not** remap a two-digit year into 19xx (the
 * `Date.UTC`/`new Date(y, ...)` two-digit-year special case in the spec) —
 * `setUTCFullYear` takes the year literally. Used everywhere this module
 * would otherwise call `Date.UTC`, including inside `offsetMinutesAt`'s
 * reconstruction from formatted parts — year 50 round-tripped through
 * `Date.UTC` there is just as wrong as it would be in `parseWallClockValue`.
 */
function utcMsFromFields(
  year: number,
  month0: number,
  day: number,
  hour: number,
  minute: number,
  second: number
): number {
  const d = new Date(0);
  d.setUTCFullYear(year, month0, day);
  d.setUTCHours(hour, minute, second, 0);
  return d.getTime();
}

function utcMsFromParts(parts: WallClockParts): number {
  return utcMsFromFields(
    parts.year,
    parts.month - 1,
    parts.day,
    parts.hour,
    parts.minute,
    parts.second
  );
}

/**
 * Parses a `datetime-local` input value (`YYYY-MM-DDTHH:mm[:ss]`), rejecting
 * both a malformed string and a malformed-but-parseable one — an
 * out-of-range field (`"2026-13-40T25:00"`) that `Date`/`Date.UTC` would
 * otherwise silently roll over into a different, unintended date. Valid
 * fields round-trip exactly through `utcMsFromParts`; anything that doesn't
 * is rejected.
 */
function parseWallClockValue(value: string): WallClockParts | null {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(value);
  if (!m) return null;
  const parts: WallClockParts = {
    year: Number(m[1]),
    month: Number(m[2]),
    day: Number(m[3]),
    hour: Number(m[4]),
    minute: Number(m[5]),
    second: m[6] ? Number(m[6]) : 0,
  };
  const ms = utcMsFromParts(parts);
  const d = new Date(ms);
  const roundTrips =
    d.getUTCFullYear() === parts.year &&
    d.getUTCMonth() === parts.month - 1 &&
    d.getUTCDate() === parts.day &&
    d.getUTCHours() === parts.hour &&
    d.getUTCMinutes() === parts.minute &&
    d.getUTCSeconds() === parts.second;
  return roundTrips ? parts : null;
}

/**
 * Returns `zone`'s UTC offset, in minutes (local = utc + offset), at the
 * instant `utcMs`. Implemented by formatting `utcMs` in `zone` and comparing
 * the printed wall-clock fields against `utcMs` interpreted as UTC — this
 * avoids relying on `timeZoneName: 'longOffset'`, whose support varies by
 * engine, and never itself needs `hour12`/`hourCycle: 'h12'`.
 */
function offsetMinutesAt(zone: string, utcMs: number): number {
  const parts = new Intl.DateTimeFormat(FORMAT_LOCALE, {
    timeZone: zone,
    hourCycle: 'h23',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).formatToParts(new Date(utcMs));
  const get = (type: Intl.DateTimeFormatPartTypes): number =>
    Number(parts.find((p) => p.type === type)?.value ?? '0');
  // formatToParts never reports hour "24" with hourCycle 'h23'.
  const asUtc = utcMsFromFields(
    get('year'),
    get('month') - 1,
    get('day'),
    get('hour'),
    get('minute'),
    get('second')
  );
  return (asUtc - utcMs) / 60000;
}

/** One day in milliseconds, used to probe on either side of a wall-clock time. */
const ONE_DAY_MS = 24 * 60 * 60 * 1000;

/**
 * Resolves wall-clock components in `zone` to a UTC instant (epoch ms),
 * matching Temporal's `"compatible"` disambiguation (design.md §2.4):
 * a time in a **gap** (spring-forward) moves forward by the gap length; a
 * time in an **overlap** (fall-back) takes the earlier of the two possible
 * instants.
 *
 * **Why probe a day on either side, not just seed-and-iterate (review R1-1).**
 * An earlier version seeded a single offset guess from `localTs` itself (the
 * wall-clock fields read as a UTC instant) and iterated from there. That
 * finds the correct instant for a zone *behind* UTC (the seed falls before
 * the real transition), but for a zone *ahead* of UTC the seed already falls
 * *after* the transition, so the iteration only ever rediscovers the later
 * (standard-time) candidate of an overlap and never the earlier
 * (daylight-time) one "compatible" requires — wrong for all of Europe,
 * Australia and New Zealand.
 *
 * Instead, probe the zone's offset a full day before and a full day after
 * `localTs` (`oA`, `oB`) — far enough that a single DST transition can't
 * separate both probes from the transition itself, so one of them always
 * reflects the pre-transition offset and the other the post-transition one
 * (or they're equal, when no transition is nearby). Build one UTC candidate
 * from each, and check which candidate(s) actually format back to `parts`
 * in `zone` (i.e. the offset at the candidate matches the offset used to
 * build it):
 * - **Both valid (overlap):** two real instants map to this wall-clock
 *   time; return the earlier.
 * - **One valid:** the ordinary, unambiguous case.
 * - **Neither valid (gap):** the wall-clock time doesn't exist; shift
 *   forward by the gap length by applying the smaller (pre-transition)
 *   offset, which is what `candidateA`/`candidateB` would have done had it
 *   existed.
 */
function wallClockToUtcMs(parts: WallClockParts, zone: string): number {
  const localTs = utcMsFromParts(parts);

  const oA = offsetMinutesAt(zone, localTs - ONE_DAY_MS);
  const oB = offsetMinutesAt(zone, localTs + ONE_DAY_MS);

  const candidateA = localTs - oA * 60000;
  const candidateB = localTs - oB * 60000;

  const validA = offsetMinutesAt(zone, candidateA) === oA;
  const validB = offsetMinutesAt(zone, candidateB) === oB;

  if (validA && validB) return Math.min(candidateA, candidateB);
  if (validA) return candidateA;
  if (validB) return candidateB;
  return localTs - Math.min(oA, oB) * 60000;
}

/**
 * Converts a `datetime-local` input value to a UTC ISO instant, interpreting
 * it as wall-clock time **in `zone`** (not the browser's zone, which is what
 * `new Date(value).toISOString()` does today). Returns `''` if `value`
 * doesn't parse, has an out-of-range field, or `zone` is not a zone
 * `isValidTimeZone` accepts (never throws, review R1-7).
 */
export function parseWallClock(value: string, zone: string): string {
  if (!isValidTimeZone(zone)) return '';
  const parts = parseWallClockValue(value);
  if (!parts) return '';
  const utcMs = wallClockToUtcMs(parts, zone);
  if (!Number.isFinite(utcMs)) return '';
  return new Date(utcMs).toISOString();
}

/**
 * The inverse of `parseWallClock`: formats a UTC ISO instant as a
 * `datetime-local` input value (`YYYY-MM-DDTHH:mm`) in `zone`. Returns `''`
 * on an unparsable `iso` or an invalid `zone` (never throws, review R1-7).
 */
export function toWallClockInput(iso: string, zone: string): string {
  if (!isValidTimeZone(zone)) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const parts = new Intl.DateTimeFormat(FORMAT_LOCALE, {
    timeZone: zone,
    hourCycle: 'h23',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  }).formatToParts(date);
  const get = (type: Intl.DateTimeFormatPartTypes): string =>
    parts.find((p) => p.type === type)?.value ?? '00';
  // `year: 'numeric'` prints a year below 1000 without leading zeros (e.g.
  // "50"), which parseWallClock's \d{4} regex — and a real `datetime-local`
  // input — both reject. Pad so the round trip holds (review R2-5).
  const year = get('year').padStart(4, '0');
  return `${year}-${get('month')}-${get('day')}T${get('hour')}:${get('minute')}`;
}
