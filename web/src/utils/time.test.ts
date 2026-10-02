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
 * time.ts — unit tests for the zone helpers added by tz-refactor task 12
 * (`browserTimeZone`, `isValidTimeZone`, `listTimeZones`), shaped per
 * design §2.4 and tz-refactor task 11 so task 11 can reuse them unchanged.
 */

import { closeSync, existsSync, openSync, readdirSync, readSync } from 'node:fs';

import { describe, it, expect } from 'vitest';

import { browserTimeZone, isValidTimeZone, listTimeZones } from './time.js';

describe('browserTimeZone', () => {
  it('returns a non-empty zone name matching Intl.DateTimeFormat().resolvedOptions()', () => {
    const zone = browserTimeZone();
    expect(zone).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone);
    expect(zone.length).toBeGreaterThan(0);
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
