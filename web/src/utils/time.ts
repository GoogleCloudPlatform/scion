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
 * Reports whether `zone` is a time zone name `Intl` accepts (e.g. an IANA
 * name such as "Asia/Tokyo", or "UTC").
 *
 * This is the one place in the web app allowed to probe `Intl.DateTimeFormat`
 * for this purpose; every zone-name check elsewhere should call this
 * function instead of constructing its own `Intl.DateTimeFormat`.
 */
export function isValidTimeZone(zone: string): boolean {
  if (!zone) return false;
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
