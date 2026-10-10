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
 * Characters never allowed in the URL, matching the hub's rule of record
 * (pkg/config ValidateMonitoringDashboardURL): whitespace (JS \s covers
 * Unicode White_Space such as U+00A0, U+2028, U+2029, plus U+FEFF), C0, DEL
 * and C1 control characters (U+0000 to U+001F, U+007F to U+009F, which
 * includes U+0085), bidirectional formatting characters (U+061C, U+200E,
 * U+200F, U+202A to U+202E, U+2066 to U+2069), invisible format
 * characters (U+00AD, U+180E, U+200B to U+200D, U+2060) and U+FFFD.
 */
const DISALLOWED =
  // The control-character ranges are intentional (see ptone/scion#4126).
  // eslint-disable-next-line no-control-regex
  /[\s\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u202a-\u202e\u2060\u2066-\u2069\ufffd]/;

/**
 * Longest accepted URL in characters (Unicode code points), as the hub's
 * MonitoringDashboardURLMaxLength and the settings schema's maxLength.
 */
export const HTTP_URL_MAX_LENGTH = 2048;

/**
 * Whether value is an absolute http:// or https:// URL with a host and no
 * user credentials, whose port (if any) is 1 to 65535, at most
 * HTTP_URL_MAX_LENGTH characters long. Used to decide
 * whether an operator-configured link is rendered at all; the hub
 * validates the same rule when the setting is saved, so this is a second
 * check on the display side.
 */
export function isHttpUrl(value: string | null | undefined): value is string {
  if (typeof value !== 'string' || value === '' || DISALLOWED.test(value)) return false;
  // Count code points, not UTF-16 units, so the limit matches the hub's.
  if (Array.from(value).length > HTTP_URL_MAX_LENGTH) return false;
  let u: URL;
  try {
    u = new URL(value);
  } catch {
    return false;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
  if (!u.hostname || u.username !== '' || u.password !== '') return false;
  // new URL() rejects ports above 65535 but accepts 0; the hub requires 1 to 65535.
  if (u.port === '0') return false;
  // new URL() accepts "http:host" and "http:/host"; require the "//" form.
  return /^https?:\/\/[^/]/i.test(value);
}
