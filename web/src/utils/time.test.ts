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
 * time.ts — unit tests.
 *
 * `browserTimeZone`/`isValidTimeZone`/`listTimeZones` are owned by
 * tz-refactor task 12; their tests (including the system-zoneinfo scan) stay
 * in task 12's lineage below, unchanged since the task 11 rebase (review
 * round 1, R1-2). Everything else here (the effective-zone store,
 * `formatInstant`, `formatRelative`/`formatRelativeTime`,
 * `parseWallClock`/`toWallClockInput`) is tz-refactor task 11's.
 */

import { closeSync, existsSync, openSync, readdirSync, readSync } from 'node:fs';

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';
import {
  FORMAT_LOCALE,
  browserTimeZone,
  isValidTimeZone,
  listTimeZones,
  setPreferredTimeZone,
  getPreferredTimeZone,
  effectiveTimeZone,
  zoneLabel,
  dayBucketZoneLabel,
  dayBucketAxisTitle,
  DISPLAY_TIMEZONE_CHANGED_EVENT,
  formatInstant,
  formatInstantWithZone,
  formatRelative,
  formatRelativeTime,
  parseWallClock,
  toWallClockInput,
} from './time.js';

// ---------------------------------------------------------------------------
// tz-refactor task 12 (ptone/scion#2533): browserTimeZone / isValidTimeZone /
// listTimeZones
// ---------------------------------------------------------------------------

describe('browserTimeZone', () => {
  it('returns a non-empty zone name matching Intl.DateTimeFormat().resolvedOptions()', () => {
    const zone = browserTimeZone();
    expect(zone).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
    expect(zone.length).toBeGreaterThan(0);
  });

  it('returns the ambient (vitest-pinned) zone', () => {
    // web/vitest.config.ts pins TZ=UTC for exactly this reason.
    expect(browserTimeZone()).toBe('UTC');
  });
});

describe('isValidTimeZone', () => {
  it('accepts UTC', () => {
    expect(isValidTimeZone('UTC')).toBe(true);
  });

  it('accepts IANA names, including a numeric-abbreviation zone (Kathmandu)', () => {
    expect(isValidTimeZone('Asia/Tokyo')).toBe(true);
    expect(isValidTimeZone('Asia/Kathmandu')).toBe(true);
    expect(isValidTimeZone('America/New_York')).toBe(true);
  });

  it('rejects the empty string', () => {
    expect(isValidTimeZone('')).toBe(false);
  });

  it('rejects a name Intl does not recognize', () => {
    expect(isValidTimeZone('Not/A/Timezone')).toBe(false);
    expect(isValidTimeZone('Mars/Olympus_Mons')).toBe(false);
  });

  it('does not throw on malformed input', () => {
    expect(() => isValidTimeZone('💥')).not.toThrow();
    expect(isValidTimeZone('💥')).toBe(false);
  });

  // isValidTimeZone must agree with the server's validator (Go's
  // time.LoadLocation), which Intl alone is looser than in these ways.
  it('rejects lowercase names Intl matches case-insensitively', () => {
    expect(isValidTimeZone('asia/tokyo')).toBe(false);
    expect(isValidTimeZone('utc')).toBe(false);
  });

  // "Utc" has valid IANA *shape* (one segment, starts with an uppercase
  // letter) and Intl resolves it case-insensitively to "UTC" without
  // throwing, so the shape rule accepts it, even though Go's
  // time.LoadLocation does not. This is the documented, accepted residual
  // false-accept (see isValidTimeZone's doc comment): nobody intentionally
  // types "Utc", and the server's 422 remains authoritative for it.
  it('accepts an unusual capitalization Intl still resolves (a documented residual gap)', () => {
    expect(isValidTimeZone('Utc')).toBe(true);
  });

  it('rejects numeric offset IDs', () => {
    expect(isValidTimeZone('+05:30')).toBe(false);
    expect(isValidTimeZone('-07:00')).toBe(false);
  });

  it('still accepts genuine aliases that resolve to a different (not just differently-cased) name', () => {
    // Asia/Kathmandu -> Asia/Katmandu, Asia/Calcutta -> Asia/Calcutta,
    // Europe/Kyiv -> Europe/Kiev: real IANA names the server's
    // time.LoadLocation also accepts, so the client must not be stricter.
    expect(isValidTimeZone('Asia/Kathmandu')).toBe(true);
    expect(isValidTimeZone('Asia/Katmandu')).toBe(true);
    expect(isValidTimeZone('Asia/Calcutta')).toBe(true);
    expect(isValidTimeZone('Europe/Kyiv')).toBe(true);
    expect(isValidTimeZone('Europe/Kiev')).toBe(true);
  });

  // A case check that only catches a name resolving to a case variant of
  // *itself* misses a lowercase alias, which resolves to a *different*
  // canonical string, so it slips through — e.g. "asia/kolkata" resolves to
  // "Asia/Calcutta", not "Asia/kolkata", so it isn't a same-string case
  // variant. Go's time.LoadLocation rejects every one of these lowercase
  // forms.
  it('rejects lowercase aliases that Go rejects, even though Intl resolves them', () => {
    expect(isValidTimeZone('asia/kolkata')).toBe(false);
    expect(isValidTimeZone('us/pacific')).toBe(false);
    expect(isValidTimeZone('gmt')).toBe(false);
    expect(isValidTimeZone('europe/kyiv')).toBe(false);
    // A mixed-case variant with a lowercase segment is rejected the same
    // way: every '/'-segment, not just the first, must start uppercase.
    expect(isValidTimeZone('Asia/kolkata')).toBe(false);
  });

  // A rule that accepts a name only if it resolves to itself or is an
  // exact-case member of a hand-picked set falsely rejects dozens of real
  // IANA names Go accepts — including current IANA canonical names, not
  // just backward-compatibility aliases. The shape-plus-Intl rule has no
  // hand-picked set to be incomplete, so these are no longer special cases,
  // just ordinary names that happen not to resolve to themselves.
  it('accepts real IANA names that do not resolve to themselves, with no hand-picked list', () => {
    expect(isValidTimeZone('Asia/Kolkata')).toBe(true);
    expect(isValidTimeZone('US/Pacific')).toBe(true);
    expect(isValidTimeZone('GMT')).toBe(true);
    expect(isValidTimeZone('Etc/GMT+5')).toBe(true);
    // EST5EDT resolves to "America/New_York" in Node 24, not to itself —
    // exactly the class of name a resolves-to-itself check depends on
    // getting lucky about.
    expect(isValidTimeZone('EST5EDT')).toBe(true);
    // Real IANA names falsely rejected by a resolves-to-itself-or-hand-picked-set
    // rule: current IANA canonical names (not links/aliases at all) and the
    // very common Etc/UTC.
    expect(isValidTimeZone('Etc/UTC')).toBe(true);
    expect(isValidTimeZone('US/Eastern')).toBe(true);
    expect(isValidTimeZone('America/Nuuk')).toBe(true);
    expect(isValidTimeZone('Asia/Yangon')).toBe(true);
    expect(isValidTimeZone('Pacific/Kanton')).toBe(true);
    expect(isValidTimeZone('America/Argentina/Buenos_Aires')).toBe(true);
    expect(isValidTimeZone('CET')).toBe(true);
  });

  it('rejects tzdata names that are not a portable IANA zone, matching the server denylist', () => {
    // Intl.DateTimeFormat already throws for all four, so no explicit
    // denylist is needed on the client side — this just locks that in.
    expect(isValidTimeZone('Local')).toBe(false);
    expect(isValidTimeZone('localtime')).toBe(false);
    expect(isValidTimeZone('posixrules')).toBe(false);
    expect(isValidTimeZone('Factory')).toBe(false);
  });
});

const ZONEINFO_DIR = '/usr/share/zoneinfo';

/**
 * "right" and "posix" are whole-tree duplicates of the same zone data
 * (right/ with leap seconds baked in, posix/ without) under a path prefix
 * that isn't itself part of any IANA name — not because the prefixed form
 * is universally rejected. Go's `time.LoadLocation` resolves against the
 * *host's* zoneinfo directory, so `LoadLocation("right/Africa/Abidjan")`
 * actually succeeds on a host whose tree has it; the
 * server denylists both prefixes explicitly for exactly that reason
 * (`validateIANATimezone`, pkg/hub/timezone_validate.go). Excluded from
 * this scan because "Africa/Abidjan" without the prefix already covers the
 * same real zone via the main tree, so including the prefixed duplicate
 * would only test path-prefix handling, not zone-name coverage. "localtime",
 * "posixrules" and "Factory" are real TZif files but are tzdata's own
 * non-portable entries, with their own dedicated rejection test above —
 * excluded here so this scan is only about *false rejects* of real zone
 * names, not re-proving the denylist.
 */
const ZONEINFO_EXCLUDED_NAMES = new Set(['right', 'posix', 'localtime', 'posixrules', 'Factory']);

const TZIF_MAGIC = Buffer.from('TZif');

/**
 * Reports whether `path` is a compiled zoneinfo entry: a file (or a symlink
 * to one — `openSync`/`readSync` follow symlinks) whose first 4 bytes are
 * the TZif magic (RFC 8536 §3.1). This is what tells a real zone file apart
 * from the tree's metadata/index files (`zone.tab`, `zone1970.tab`,
 * `zonenow.tab`, `iso3166.tab`, `leapseconds`, `leap-seconds.list`,
 * `tzdata.zi`, macOS's `+VERSION`, `SECURITY`, ...) without having to name
 * every one of them — the same "hand-picked list is always incomplete"
 * failure mode fixed for `isValidTimeZone` itself.
 */
function isTZifFile(path: string): boolean {
  let fd: number;
  try {
    fd = openSync(path, 'r');
  } catch {
    return false;
  }
  try {
    const buf = Buffer.alloc(4);
    const bytesRead = readSync(fd, buf, 0, 4, 0);
    return bytesRead === 4 && buf.equals(TZIF_MAGIC);
  } catch {
    return false;
  } finally {
    closeSync(fd);
  }
}

/** Recursively lists zone names under `dir` (e.g. "Asia/Tokyo", "CET"). */
function listSystemZoneNames(dir: string, prefix = ''): string[] {
  const names: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (ZONEINFO_EXCLUDED_NAMES.has(entry.name)) continue;
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
    const full = `${dir}/${entry.name}`;
    if (entry.isDirectory()) {
      names.push(...listSystemZoneNames(full, rel));
    } else if (isTZifFile(full)) {
      names.push(rel);
    }
    // else: not a TZif file (an index/metadata entry) — skip.
  }
  return names;
}

describe('isValidTimeZone against the system zone database', () => {
  // The regression class here is "a hand-picked list of exceptions is
  // always incomplete." A test that only checks a dozen hand-picked names
  // (the describe block above) can't catch a recurrence of that same
  // mistake. /usr/share/zoneinfo is a broad, not-hand-picked-by-this-PR
  // source of real zone names — including backward-compatibility links
  // this container's tzdata package ships — to check isValidTimeZone
  // against in bulk.
  //
  // skipIf (not a try/catch around a missing directory) makes a skip show
  // up as a skip, not a silent pass: a CI image without tzdata would
  // otherwise quietly lose this guard.
  it.skipIf(!existsSync(ZONEINFO_DIR))(
    'accepts every name in the system zone database (0 false rejects)',
    () => {
      const names = listSystemZoneNames(ZONEINFO_DIR);
      expect(names.length).toBeGreaterThan(50);
      const falseRejects = names.filter((n) => !isValidTimeZone(n));
      expect(falseRejects).toEqual([]);
    }
  );
});

describe('listTimeZones', () => {
  it('returns a non-empty list of distinct zone names', () => {
    const zones = listTimeZones();
    expect(zones.length).toBeGreaterThan(0);
    expect(new Set(zones).size).toBe(zones.length);
  });

  it('includes UTC even when the runtime list omits it', () => {
    const zones = listTimeZones();
    expect(zones).toContain('UTC');
    expect(zones.filter((z) => z === 'UTC')).toHaveLength(1);
  });

  it('includes ordinary IANA names', () => {
    const zones = listTimeZones();
    expect(zones).toContain('Asia/Tokyo');
    // ICU's canonical identifier is "Asia/Katmandu" (no "h"); "Asia/Kathmandu"
    // is a valid alias accepted by Intl.DateTimeFormat (see isValidTimeZone
    // tests) but is not itself returned by supportedValuesOf().
    expect(zones).toContain('Asia/Katmandu');
  });

  it('every returned name is itself valid per isValidTimeZone', () => {
    // Cross-consistency between the two helpers: every name listTimeZones()
    // returns (400+ of them) must itself pass isValidTimeZone, including
    // under isValidTimeZone's stricter case/offset rules.
    const zones = listTimeZones();
    for (const zone of zones) {
      expect(isValidTimeZone(zone)).toBe(true);
    }
  });
});

// ---------------------------------------------------------------------------
// tz-refactor task 11: FORMAT_LOCALE, effective-zone store, formatInstant,
// formatRelative/formatRelativeTime, parseWallClock/toWallClockInput
// ---------------------------------------------------------------------------

describe('FORMAT_LOCALE', () => {
  it('is the single fixed locale (D4)', () => {
    expect(FORMAT_LOCALE).toBe('en');
  });
});

describe('effective-zone store', () => {
  afterEach(() => {
    setPreferredTimeZone('');
  });

  it('defaults to Auto (browser zone)', () => {
    expect(getPreferredTimeZone()).toBe('');
    expect(effectiveTimeZone()).toBe(browserTimeZone());
    expect(zoneLabel()).toBe(browserTimeZone());
  });

  it('prefers an explicit preference over the browser zone', () => {
    setPreferredTimeZone('Asia/Tokyo');
    expect(getPreferredTimeZone()).toBe('Asia/Tokyo');
    expect(effectiveTimeZone()).toBe('Asia/Tokyo');
    expect(zoneLabel()).toBe('Asia/Tokyo');
  });

  it('clearing to "" (Auto) falls back to the browser zone with no reload', () => {
    setPreferredTimeZone('Asia/Tokyo');
    setPreferredTimeZone('');
    expect(effectiveTimeZone()).toBe(browserTimeZone());
  });

  it('falls back to Auto for null/undefined', () => {
    setPreferredTimeZone('Asia/Tokyo');
    setPreferredTimeZone(undefined);
    expect(getPreferredTimeZone()).toBe('');
    setPreferredTimeZone('Asia/Tokyo');
    setPreferredTimeZone(null);
    expect(getPreferredTimeZone()).toBe('');
  });

  it('a hub-rejected (invalid) value falls back to Auto, never throws', () => {
    expect(() => setPreferredTimeZone('Not/AZone')).not.toThrow();
    setPreferredTimeZone('Not/AZone');
    expect(getPreferredTimeZone()).toBe('');
  });

  describe('DISPLAY_TIMEZONE_CHANGED_EVENT (review R1-4)', () => {
    it('fires when the effective preference actually changes', () => {
      const handler = vi.fn();
      window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      try {
        setPreferredTimeZone('Asia/Tokyo');
        expect(handler).toHaveBeenCalledTimes(1);
        setPreferredTimeZone('Asia/Kathmandu');
        expect(handler).toHaveBeenCalledTimes(2);
      } finally {
        window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      }
    });

    it('does not fire on a no-op set of the same value', () => {
      setPreferredTimeZone('Asia/Tokyo');
      const handler = vi.fn();
      window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      try {
        setPreferredTimeZone('Asia/Tokyo');
        expect(handler).not.toHaveBeenCalled();
      } finally {
        window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      }
    });

    it('does not fire when two different invalid values both resolve to Auto', () => {
      setPreferredTimeZone('Not/AZone');
      const handler = vi.fn();
      window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      try {
        setPreferredTimeZone('Also/NotAZone');
        expect(handler).not.toHaveBeenCalled();
      } finally {
        window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, handler);
      }
    });
  });
});

describe('dayBucketZoneLabel / dayBucketAxisTitle (ptone/scion#3370)', () => {
  it('names the zone the hub reports', () => {
    expect(dayBucketZoneLabel('America/Chicago')).toBe('America/Chicago');
    expect(dayBucketAxisTitle('America/Chicago')).toBe('Day (America/Chicago)');
    expect(dayBucketAxisTitle('Asia/Kathmandu')).toBe('Day (Asia/Kathmandu)');
  });

  it('keeps "(UTC)" for UTC and for a hub that reports no zone', () => {
    expect(dayBucketAxisTitle('UTC')).toBe('Day (UTC)');
    expect(dayBucketAxisTitle('')).toBe('Day (UTC)');
    expect(dayBucketAxisTitle(undefined)).toBe('Day (UTC)');
    expect(dayBucketAxisTitle(null)).toBe('Day (UTC)');
  });

  it('ignores the display-zone preference: the label is what the hub bucketed by', () => {
    setPreferredTimeZone('Asia/Tokyo');
    try {
      expect(dayBucketAxisTitle('UTC')).toBe('Day (UTC)');
    } finally {
      setPreferredTimeZone('');
    }
  });
});

describe('formatInstant', () => {
  afterEach(() => setPreferredTimeZone(''));

  it('formats in the effective zone, not the browser zone', () => {
    setPreferredTimeZone('Asia/Tokyo');
    // 2026-01-15T00:00:00Z -> 09:00 JST same day.
    expect(formatInstant('2026-01-15T00:00:00Z', 'time')).toBe('09:00');
    expect(formatInstant('2026-01-15T00:00:00Z', 'date')).toBe('Jan 15, 2026');
  });

  it('midnight renders as 00:00, never 24:00 (D4)', () => {
    setPreferredTimeZone('UTC');
    expect(formatInstant('2026-01-15T00:00:00Z', 'time')).toBe('00:00');
  });

  it('a preference that differs from the browser zone changes the rendered time', () => {
    // browserTimeZone() is pinned to UTC in this test run.
    setPreferredTimeZone('Asia/Kathmandu'); // +05:45, no DST
    expect(formatInstant('2026-06-01T12:00:00Z', 'time')).toBe('17:45');
  });

  it('falls back to the browser zone when no preference is set', () => {
    expect(formatInstant('2026-01-15T03:04:00Z', 'time')).toBe('03:04');
  });

  it('returns "" for an unparsable instant', () => {
    expect(formatInstant('not-a-date')).toBe('');
  });

  it('datetime style omits the year (compact UI)', () => {
    setPreferredTimeZone('UTC');
    expect(formatInstant('2026-09-23T14:15:00Z', 'datetime')).toBe('Sep 23, 14:15');
  });

  it('datetime-full style includes the year', () => {
    setPreferredTimeZone('UTC');
    expect(formatInstant('2026-09-23T14:15:00Z', 'datetime-full')).toBe('Sep 23, 2026, 14:15');
  });

  it('time-seconds style includes seconds', () => {
    setPreferredTimeZone('UTC');
    expect(formatInstant('2026-09-23T14:15:05Z', 'time-seconds')).toBe('14:15:05');
  });

  it('time-millis style includes milliseconds, midnight as 00:00:00.000', () => {
    setPreferredTimeZone('Asia/Tokyo');
    // 15:00:00.000Z is midnight the next day in Tokyo (+09:00).
    expect(formatInstant('2026-09-23T15:00:00.000Z', 'time-millis')).toBe('00:00:00.000');
    expect(formatInstant('2026-09-23T15:04:05.007Z', 'time-millis')).toBe('00:04:05.007');
  });
});

describe('formatInstant formatter reuse (ptone/scion#2382)', () => {
  afterEach(() => {
    setPreferredTimeZone('');
    vi.restoreAllMocks();
  });

  it('builds a style/zone formatter once and reuses it on later calls', () => {
    setPreferredTimeZone('Asia/Kathmandu');
    // Warm this style/zone pair; it may or may not already be cached.
    formatInstant('2026-01-15T00:00:00Z', 'datetime-full');
    const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');
    for (let i = 0; i < 100; i++) {
      formatInstant('2026-01-15T00:00:00Z', 'datetime-full');
    }
    expect(ctorSpy).not.toHaveBeenCalled();
  });

  it('with no preference, resolves the browser zone once per synchronous task', async () => {
    formatInstant('2026-01-15T00:00:00Z', 'time');
    await Promise.resolve();
    const ctorSpy = vi.spyOn(Intl, 'DateTimeFormat');
    for (let i = 0; i < 100; i++) {
      formatInstant('2026-01-15T00:00:00Z', 'time');
    }
    expect(ctorSpy).toHaveBeenCalledTimes(1);
    // The held value is dropped once the task ends, so a later task (a later
    // render) resolves the browser zone again.
    await Promise.resolve();
    formatInstant('2026-01-15T00:00:00Z', 'time');
    expect(ctorSpy).toHaveBeenCalledTimes(2);
  });

  it('a zone change selects the new zone formatter, not the cached one', () => {
    setPreferredTimeZone('Asia/Tokyo');
    expect(formatInstant('2026-01-15T15:00:00Z', 'time')).toBe('00:00');
    setPreferredTimeZone('America/New_York');
    expect(formatInstant('2026-01-15T15:00:00Z', 'time')).toBe('10:00');
    setPreferredTimeZone('Asia/Tokyo');
    expect(formatInstant('2026-01-15T15:00:00Z', 'time')).toBe('00:00');
  });
});

describe('formatInstantWithZone (review R2-4)', () => {
  afterEach(() => setPreferredTimeZone(''));

  it('appends the effective zone in parentheses, defaulting to datetime-full', () => {
    setPreferredTimeZone('Asia/Tokyo');
    expect(formatInstantWithZone('2026-09-23T14:15:00Z')).toBe('Sep 23, 2026, 23:15 (Asia/Tokyo)');
  });

  it('honours an explicit style', () => {
    setPreferredTimeZone('UTC');
    expect(formatInstantWithZone('2026-09-23T14:15:00Z', 'time')).toBe('14:15 (UTC)');
  });

  it('falls back to the browser zone label when no preference is set', () => {
    expect(formatInstantWithZone('2026-01-15T03:04:00Z', 'time')).toBe('03:04 (UTC)');
  });

  it('returns "" for an unparsable instant', () => {
    expect(formatInstantWithZone('not-a-date')).toBe('');
  });
});

describe('formatRelativeTime (past-only, many existing call sites)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('formats a past instant', () => {
    vi.setSystemTime(new Date('2026-01-15T00:10:00Z'));
    expect(formatRelativeTime('2026-01-15T00:00:00Z')).toBe('10 minutes ago');
  });
});

describe('formatRelative (past and future)', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('formats a past instant', () => {
    vi.setSystemTime(new Date('2026-01-15T03:00:00Z'));
    expect(formatRelative('2026-01-15T00:00:00Z')).toBe('3 hours ago');
  });

  it('formats a future instant', () => {
    vi.setSystemTime(new Date('2026-01-15T00:00:00Z'));
    expect(formatRelative('2026-01-15T03:00:00Z')).toBe('in 3 hours');
  });

  it('returns the original string on parse failure', () => {
    expect(formatRelative('not-a-date')).toBe('not-a-date');
  });

  it('formats seconds, and "now" for the current instant', () => {
    vi.setSystemTime(new Date('2026-01-15T00:00:30Z'));
    expect(formatRelative('2026-01-15T00:00:00Z')).toBe('30 seconds ago');
    expect(formatRelative('2026-01-15T00:01:00Z')).toBe('in 30 seconds');
    expect(formatRelative('2026-01-15T00:00:30Z')).toBe('now');
  });

  it('formats days past and future (no hours cap)', () => {
    vi.setSystemTime(new Date('2026-01-15T00:00:00Z'));
    expect(formatRelative('2026-01-12T00:00:00Z')).toBe('3 days ago');
    expect(formatRelative('2026-01-18T00:00:00Z')).toBe('in 3 days');
    expect(formatRelative('2026-01-14T00:00:00Z')).toBe('yesterday');
    expect(formatRelative('2026-01-16T00:00:00Z')).toBe('tomorrow');
  });

  it('accepts a style option for compact cells', () => {
    vi.setSystemTime(new Date('2026-01-15T03:00:00Z'));
    expect(formatRelative('2026-01-15T00:00:00Z', { style: 'long' })).toBe('3 hours ago');
    expect(formatRelative('2026-01-15T00:00:00Z', { style: 'short' })).toBe('3 hr. ago');
    expect(formatRelative('2026-01-15T00:00:00Z', { style: 'narrow' })).toBe('3h ago');
    expect(formatRelative('2026-01-15T02:55:00Z', { style: 'narrow' })).toBe('5m ago');
    expect(formatRelative('2026-01-13T03:00:00Z', { style: 'narrow' })).toBe('2d ago');
    expect(formatRelative('2026-01-18T03:00:00Z', { style: 'narrow' })).toBe('in 3d');
  });
});

describe('parseWallClock / toWallClockInput', () => {
  it('round-trips an ordinary (non-transition) wall-clock time', () => {
    const iso = parseWallClock('2026-06-15T09:30', 'Asia/Tokyo');
    expect(iso).toBe('2026-06-15T00:30:00.000Z');
    expect(toWallClockInput(iso, 'Asia/Tokyo')).toBe('2026-06-15T09:30');
  });

  it('interprets the value in the given zone, not the ambient (browser) zone', () => {
    // Ambient/browser zone is pinned to UTC; Kathmandu is +05:45.
    const iso = parseWallClock('2026-03-01T09:00', 'Asia/Kathmandu');
    expect(iso).toBe('2026-03-01T03:15:00.000Z');
  });

  it('a preference that differs from the browser zone changes the stored instant', () => {
    // Same wall-clock input, two different zones, two different instants.
    const tokyo = parseWallClock('2026-03-01T09:00', 'Asia/Tokyo');
    const kathmandu = parseWallClock('2026-03-01T09:00', 'Asia/Kathmandu');
    expect(tokyo).not.toBe(kathmandu);
    expect(tokyo).toBe('2026-03-01T00:00:00.000Z');
    expect(kathmandu).toBe('2026-03-01T03:15:00.000Z');
  });

  it('returns "" for an unparsable datetime-local value', () => {
    expect(parseWallClock('garbage', 'UTC')).toBe('');
    expect(parseWallClock('', 'UTC')).toBe('');
  });

  it('toWallClockInput returns "" for an unparsable instant', () => {
    expect(toWallClockInput('garbage', 'UTC')).toBe('');
  });

  // Review R1-7: an invalid zone must fall back to '', never throw.
  it('returns "" for an invalid zone, never throws (review R1-7)', () => {
    expect(() => parseWallClock('2026-01-01T00:00', 'Bogus/Zone')).not.toThrow();
    expect(parseWallClock('2026-01-01T00:00', 'Bogus/Zone')).toBe('');
    expect(() => toWallClockInput('2026-01-01T00:00:00Z', 'Bogus/Zone')).not.toThrow();
    expect(toWallClockInput('2026-01-01T00:00:00Z', 'Bogus/Zone')).toBe('');
  });

  // Review R1-7: out-of-range fields must be rejected, not silently rolled
  // over into a different date by Date/Date.UTC.
  it('rejects out-of-range fields instead of silently rolling them over (review R1-7)', () => {
    expect(parseWallClock('2026-13-40T25:00', 'UTC')).toBe('');
    expect(parseWallClock('2026-02-30T00:00', 'UTC')).toBe('');
    expect(parseWallClock('2026-01-01T24:00', 'UTC')).toBe('');
    expect(parseWallClock('2026-01-01T00:60', 'UTC')).toBe('');
  });

  // Review R1-7: a two-digit year must not be remapped into 19xx the way
  // `Date.UTC`/`new Date(y, ...)` does for 0 <= y <= 99.
  it('does not remap a two-digit year into 19xx (review R1-7)', () => {
    expect(parseWallClock('0050-01-01T00:00', 'UTC')).toBe('0050-01-01T00:00:00.000Z');
  });

  // Review R2-5: year: 'numeric' prints a year below 1000 without leading
  // zeros ("50"), which both parseWallClock's \d{4} regex and a real
  // datetime-local input reject — toWallClockInput must pad it.
  it('round-trips a year below 1000 through toWallClockInput (review R2-5)', () => {
    const input = toWallClockInput('0050-06-01T12:00:00Z', 'UTC');
    expect(input).toBe('0050-06-01T12:00');
    expect(parseWallClock(input, 'UTC')).toBe('0050-06-01T12:00:00.000Z');
  });

  it('seconds in a datetime-local value are preserved', () => {
    expect(parseWallClock('2026-06-15T09:30:45', 'UTC')).toBe('2026-06-15T09:30:45.000Z');
  });

  describe('DST gap (America/New_York spring-forward, 2026-03-08 02:00->03:00)', () => {
    it('a wall-clock time inside the gap moves forward by the gap length', () => {
      // 02:30 does not exist; "compatible" disambiguation shifts it to 03:30 EDT.
      const iso = parseWallClock('2026-03-08T02:30', 'America/New_York');
      expect(iso).toBe('2026-03-08T07:30:00.000Z'); // 03:30 EDT (UTC-4)
      expect(toWallClockInput(iso, 'America/New_York')).toBe('2026-03-08T03:30');
    });

    it('times on either side of the gap are unaffected', () => {
      expect(parseWallClock('2026-03-08T01:30', 'America/New_York')).toBe(
        '2026-03-08T06:30:00.000Z'
      ); // 01:30 EST (UTC-5)
      expect(parseWallClock('2026-03-08T03:30', 'America/New_York')).toBe(
        '2026-03-08T07:30:00.000Z'
      ); // 03:30 EDT (UTC-4) - already valid, unaffected
    });
  });

  describe('DST overlap, zones BEHIND UTC (America/New_York fall-back, 2026-11-01 02:00->01:00)', () => {
    it('a wall-clock time inside the overlap takes the earlier (pre-transition) offset', () => {
      // 01:30 occurs twice: once at 05:30Z (EDT, UTC-4) and once at 06:30Z
      // (EST, UTC-5). "compatible" disambiguation takes the earlier instant.
      const iso = parseWallClock('2026-11-01T01:30', 'America/New_York');
      expect(iso).toBe('2026-11-01T05:30:00.000Z');
    });
  });

  // Review R1-1: a single-seeded-probe implementation returns the LATER
  // instant for every overlap in a zone ahead of UTC, because the seed
  // (wall-clock fields read as UTC) already falls after the real transition.
  // These three zones (two full-hour DST, one 30-minute DST) pin the fix.
  describe('DST overlap, zones AHEAD of UTC (review R1-1)', () => {
    it('Europe/Berlin fall-back (2026-10-25 03:00 CEST -> 02:00 CET) takes the earlier (CEST) instant', () => {
      const iso = parseWallClock('2026-10-25T02:30', 'Europe/Berlin');
      expect(iso).toBe('2026-10-25T00:30:00.000Z'); // 02:30 CEST (UTC+2)
      expect(toWallClockInput(iso, 'Europe/Berlin')).toBe('2026-10-25T02:30');
    });

    it('Europe/Berlin spring-forward gap (2026-03-29 02:00->03:00) moves forward by the gap length', () => {
      const iso = parseWallClock('2026-03-29T02:30', 'Europe/Berlin');
      expect(iso).toBe('2026-03-29T01:30:00.000Z'); // 03:30 CEST (UTC+2)
    });

    it('Australia/Sydney fall-back (2026-04-05 03:00 DST -> 02:00 standard) takes the earlier instant', () => {
      const iso = parseWallClock('2026-04-05T02:30', 'Australia/Sydney');
      expect(iso).toBe('2026-04-04T15:30:00.000Z');
    });

    it('Australia/Lord_Howe 30-minute fall-back takes the earlier instant', () => {
      // Lord Howe: standard UTC+10:30, DST UTC+11:00 (a 30-minute shift, not
      // the usual hour) — exercises that the fix is offset-size agnostic.
      const iso = parseWallClock('2026-04-05T01:45', 'Australia/Lord_Howe');
      expect(iso).toBe('2026-04-04T14:45:00.000Z');
    });
  });

  describe('fixed-offset zones with no DST (Asia/Kathmandu, +05:45)', () => {
    it('round-trips with no ambiguity', () => {
      const iso = parseWallClock('2026-06-15T12:00', 'Asia/Kathmandu');
      expect(iso).toBe('2026-06-15T06:15:00.000Z');
      expect(toWallClockInput(iso, 'Asia/Kathmandu')).toBe('2026-06-15T12:00');
    });
  });
});
