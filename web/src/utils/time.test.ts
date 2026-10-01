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
  it('rejects names that differ from Intl only by case', () => {
    expect(isValidTimeZone('asia/tokyo')).toBe(false);
    expect(isValidTimeZone('utc')).toBe(false);
    expect(isValidTimeZone('Utc')).toBe(false);
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

  it('rejects tzdata names that are not a portable IANA zone, matching the server denylist', () => {
    // Intl.DateTimeFormat already throws for all four, so no explicit
    // denylist is needed on the client side — this just locks that in.
    expect(isValidTimeZone('Local')).toBe(false);
    expect(isValidTimeZone('localtime')).toBe(false);
    expect(isValidTimeZone('posixrules')).toBe(false);
    expect(isValidTimeZone('Factory')).toBe(false);
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
