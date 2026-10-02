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

import { readdirSync } from 'node:fs';

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

  // tz-refactor task 12 review round 1, R1-3: isValidTimeZone must agree
  // with the server's validator (Go's time.LoadLocation), which Intl alone
  // is looser than in these ways.
  it('rejects lowercase names Intl matches case-insensitively', () => {
    expect(isValidTimeZone('asia/tokyo')).toBe(false);
    expect(isValidTimeZone('utc')).toBe(false);
  });

  // "Utc" has valid IANA *shape* (one segment, starts with an uppercase
  // letter) and Intl resolves it case-insensitively to "UTC" without
  // throwing, so the R3-1 shape rule accepts it, even though Go's
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

  // tz-refactor task 12 review round 2, R2-2: round 1's case check only
  // caught a name resolving to a case variant of *itself*. A lowercase
  // alias resolves to a *different* canonical string, so it slipped
  // through — e.g. "asia/kolkata" resolves to "Asia/Calcutta", not
  // "Asia/kolkata", so it isn't a same-string case variant. Go's
  // time.LoadLocation rejects every one of these lowercase forms.
  it('rejects lowercase aliases that Go rejects, even though Intl resolves them', () => {
    expect(isValidTimeZone('asia/kolkata')).toBe(false);
    expect(isValidTimeZone('us/pacific')).toBe(false);
    expect(isValidTimeZone('gmt')).toBe(false);
    expect(isValidTimeZone('europe/kyiv')).toBe(false);
    // A mixed-case variant with a lowercase segment is rejected the same
    // way: every '/'-segment, not just the first, must start uppercase.
    expect(isValidTimeZone('Asia/kolkata')).toBe(false);
  });

  // tz-refactor task 12 review round 3, R3-1: round 2's fix (the previous
  // version of this test) swapped one bug for a worse one. It accepted a
  // name only if it resolved to itself or was an exact-case member of a
  // 7-entry hand-picked set, which falsely rejected dozens of real IANA
  // names Go accepts — including current IANA canonical names, not just
  // backward-compatibility aliases. The current rule (shape-plus-Intl) has
  // no hand-picked set to be incomplete, so these are no longer special
  // cases, just ordinary names that happen not to resolve to themselves.
  it('accepts real IANA names that do not resolve to themselves, with no hand-picked list', () => {
    expect(isValidTimeZone('Asia/Kolkata')).toBe(true);
    expect(isValidTimeZone('US/Pacific')).toBe(true);
    expect(isValidTimeZone('GMT')).toBe(true);
    expect(isValidTimeZone('Etc/GMT+5')).toBe(true);
    // EST5EDT resolves to "America/New_York" in Node 24, not to itself —
    // round 2's response claimed otherwise; that claim was wrong, and this
    // is exactly the class of name round 2's rule depended on getting
    // lucky about.
    expect(isValidTimeZone('EST5EDT')).toBe(true);
    // Named in review round 3 as falsely rejected by round 2's rule:
    // current IANA canonical names (not links/aliases at all) and the
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

/**
 * Non-zone entries under a typical /usr/share/zoneinfo tree: indices,
 * metadata and legacy-link bookkeeping files, plus the tzdata-own
 * non-portable names time.ts's isValidTimeZone correctly rejects (those
 * have their own dedicated test above; excluded here so this scan is only
 * about *false rejects*, not re-proving the denylist).
 */
const ZONEINFO_NON_ZONE_ENTRIES = new Set([
  'iso3166.tab',
  'leap-seconds.list',
  'leapseconds',
  'tzdata.zi',
  'zone.tab',
  'zone1970.tab',
  'zonenow.tab',
  'localtime',
  'posixrules',
  'Factory',
  // "right" and "posix" are whole-tree duplicates of the same zone data
  // (right/ with leap seconds baked in, posix/ without), not zone-name
  // path components — "right/Africa/Abidjan" isn't itself an IANA name,
  // Go's time.LoadLocation doesn't accept that prefixed form either, and
  // "Africa/Abidjan" (without the prefix) is already covered via the main
  // tree. A fuller tzdata package (e.g. GitHub Actions' ubuntu-latest
  // runner, unlike this container's slimmer one) ships both trees.
  'right',
  'posix',
]);

/** Recursively lists zone names under `dir` (e.g. "Asia/Tokyo", "CET"). */
function listSystemZoneNames(dir: string, prefix = ''): string[] {
  const names: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (ZONEINFO_NON_ZONE_ENTRIES.has(entry.name)) continue;
    const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
    if (entry.isDirectory()) {
      names.push(...listSystemZoneNames(`${dir}/${entry.name}`, rel));
    } else {
      // Regular files and symlinks (e.g. "UTC" -> "Etc/UTC") are both leaf
      // zone entries; dirent.isDirectory() does not follow symlinks, so a
      // symlinked zone name is correctly treated as a leaf here.
      names.push(rel);
    }
  }
  return names;
}

describe('isValidTimeZone against the system zone database', () => {
  // tz-refactor task 12 review round 3, R3-1: the regression class here is
  // "a hand-picked list of exceptions is always incomplete." A test that
  // only checks a dozen hand-picked names (the describe block above) can't
  // catch a recurrence of that same mistake. /usr/share/zoneinfo is a
  // broad, not-hand-picked-by-this-PR source of real zone names — including
  // backward-compatibility links this container's tzdata package ships —
  // to check isValidTimeZone against in bulk.
  it('accepts every name in the system zone database (0 false rejects)', () => {
    let names: string[];
    try {
      names = listSystemZoneNames('/usr/share/zoneinfo');
    } catch {
      // No zoneinfo directory on this platform (e.g. some CI images, or a
      // non-Linux dev machine running `npm test`). Skip rather than fail —
      // the hand-picked-name tests above still cover the specific
      // regression names explicitly, platform-independently.
      return;
    }
    expect(names.length).toBeGreaterThan(50);
    const falseRejects = names.filter((n) => !isValidTimeZone(n));
    expect(falseRejects).toEqual([]);
  });
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
    // under R1-3's stricter case/offset rules.
    const zones = listTimeZones();
    for (const zone of zones) {
      expect(isValidTimeZone(zone)).toBe(true);
    }
  });
});
