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
 * Format a date string as a relative time description (e.g. "3 hours ago").
 *
 * Uses `Intl.RelativeTimeFormat` for locale-aware output.
 * Returns the original string on parse failure.
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

    const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

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
