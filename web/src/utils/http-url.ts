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
 * Whether value is an absolute http:// or https:// URL with a host and no
 * user credentials. Used to decide whether an operator-configured link is
 * rendered at all; the hub validates the same rule when the setting is
 * saved, so this is a second check on the display side.
 */
export function isHttpUrl(value: string | null | undefined): value is string {
  if (typeof value !== 'string' || value === '' || /[\s\u0000-\u001f\u007f]/.test(value)) {
    return false;
  }
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
