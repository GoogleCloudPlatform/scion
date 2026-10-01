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
 * Reports whether `zone` is a time zone name the server-side resolver
 * (Go's `time.LoadLocation`, used for `agent_defaults.default_timezone` and
 * the per-user display-timezone preference) would also accept.
 *
 * This is the one place in the web app allowed to probe `Intl.DateTimeFormat`
 * for this purpose; every zone-name check elsewhere should call this
 * function instead of constructing its own `Intl.DateTimeFormat`.
 *
 * `Intl.DateTimeFormat` alone is looser than `time.LoadLocation` in two ways
 * this function corrects, so the two validators agree (tz-refactor task 12
 * review round 1, R1-3):
 * - **Case.** `Intl` matches zone names case-insensitively (`asia/tokyo`,
 *   `utc` both resolve), `time.LoadLocation` does not. Rejected by checking
 *   whether `resolvedOptions().timeZone` matches `zone` case-insensitively
 *   but not exactly — which still *accepts* a genuine alias such as
 *   "Asia/Kathmandu" (resolves to "Asia/Katmandu": a different string, not a
 *   same-string case variant).
 * - **Offsets.** `Intl` accepts numeric offset IDs like "+05:30" (and
 *   resolves them to themselves, so the case check above would not catch
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
  if (resolved !== zone && resolved.toLowerCase() === zone.toLowerCase()) return false;
  return true;
}

/**
 * Returns the IANA time zone names the browser supports, plus "UTC" (in
 * case the runtime's list omits it).
 */
export function listTimeZones(): string[] {
  const zones = Intl.supportedValuesOf('timeZone');
  return zones.includes('UTC') ? zones : [...zones, 'UTC'];
}
