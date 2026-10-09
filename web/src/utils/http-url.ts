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
 * includes U+0085), and bidirectional formatting characters (U+061C,
 * U+200E, U+200F, U+202A to U+202E, U+2066 to U+2069).
 */
const DISALLOWED = /[\s\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/;

/**
 * Whether value is an absolute http:// or https:// URL with a host and no
 * user credentials. Used to decide whether an operator-configured link is
 * rendered at all; the hub validates the same rule when the setting is
 * saved, so this is a second check on the display side.
 */
export function isHttpUrl(value: string | null | undefined): value is string {
  if (typeof value !== 'string' || value === '' || DISALLOWED.test(value)) return false;
  let u: URL;
  try {
    u = new URL(value);
  } catch {
    return false;
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
  if (!u.hostname || u.username !== '' || u.password !== '') return false;
  // new URL() accepts "http:host" and "http:/host"; require the "//" form.
  return /^https?:\/\/[^/]/i.test(value);
}
