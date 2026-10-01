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
 * Exact-case alias and backward-compatibility names `time.LoadLocation`
 * accepts that are not themselves canonical per `Intl.supportedValuesOf`
 * (ICU resolves them to a different name) — e.g. "Asia/Kolkata" resolves to
 * "Asia/Calcutta". `isValidTimeZone` accepts a name here only with this
 * exact casing; a lowercase variant like "asia/kolkata" is still rejected
 * (tz-refactor task 12 review round 2, R2-2).
 *
 * Kept deliberately small: the four `SEARCH_ALIAS_HINTS` targets from
 * `timezone-picker.ts` (so a search for one of those bare names can offer
 * and then accept the full alias it names), plus a few commonly-typed
 * backward/POSIX-style names.
 */
const KNOWN_ALIAS_TIMEZONE_NAMES: ReadonlySet<string> = new Set([
  'Asia/Kolkata',
  'Europe/Kyiv',
  'Asia/Kathmandu',
  'Asia/Ho_Chi_Minh',
  'US/Pacific',
  'GMT',
  'EST5EDT',
]);

/**
 * Reports whether `zone` is a time zone name the server-side resolver
 * (Go's `time.LoadLocation`, used for `agent_defaults.default_timezone` and
 * the per-user display-timezone preference) would also accept.
 *
 * This is the one place in the web app allowed to probe `Intl.DateTimeFormat`
 * for this purpose; every zone-name check elsewhere should call this
 * function instead of constructing its own `Intl.DateTimeFormat`.
 *
 * `Intl.DateTimeFormat` alone is looser than `time.LoadLocation` in ways
 * this function corrects, so the two validators agree:
 * - **Case and aliases (tz-refactor task 12 review rounds 1 and 2, R1-3 and
 *   R2-2).** `Intl` matches zone names case-insensitively, and resolves
 *   many aliases to a *different* canonical string regardless of case
 *   ("Asia/Kolkata" and "asia/kolkata" both resolve to "Asia/Calcutta").
 *   `time.LoadLocation` accepts neither a case variant nor a lowercase
 *   alias, but does accept a handful of exact-case aliases. So a name is
 *   valid only if it resolves to *itself* exactly (covers both ordinary
 *   canonical names and any alias that happens to resolve to itself,
 *   `EST5EDT`-style and `Etc/GMT+5`-style names included), or it is an
 *   exact-case match in `KNOWN_ALIAS_TIMEZONE_NAMES` (aliases that resolve
 *   to a different canonical string but that Go still accepts).
 * - **Offsets.** `Intl` accepts numeric offset IDs like "+05:30" (which
 *   resolve to themselves, so the exact-match rule above would not catch
 *   them); `time.LoadLocation` rejects them. Rejected explicitly.
 *
 * It also rejects tzdata's own non-portable names ("Local", "localtime",
 * "posixrules", "Factory") the same way the server's denylist does, but
 * needs no explicit list for them: `Intl.DateTimeFormat` already throws for
 * all four.
 */
export function isValidTimeZone(zone: string): boolean {
  if (!zone) return false;
  if (zone.startsWith('+') || zone.startsWith('-')) return false;
  let resolved: string;
  try {
    resolved = new Intl.DateTimeFormat('en', { timeZone: zone }).resolvedOptions().timeZone;
  } catch {
    return false;
  }
  return resolved === zone || KNOWN_ALIAS_TIMEZONE_NAMES.has(zone);
}

/**
 * Returns the IANA time zone names the browser supports, plus "UTC" (in
 * case the runtime's list omits it).
 */
export function listTimeZones(): string[] {
  const zones = Intl.supportedValuesOf('timeZone');
  return zones.includes('UTC') ? zones : [...zones, 'UTC'];
}
