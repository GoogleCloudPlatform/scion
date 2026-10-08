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
 * Helpers for resource source URLs (the URL a template or harness config was
 * imported from).
 */

/**
 * Whether a template's stored source URL is one the hub can refresh from: an
 * https URL on github.com. Built-in (builtin://) and empty sources are not.
 * The hub applies the full check; this only decides whether to offer the
 * action.
 */
export function isTemplateSourceRefreshable(sourceUrl: string | undefined | null): boolean {
  if (!sourceUrl) return false;
  try {
    const u = new URL(sourceUrl.trim());
    return (
      u.protocol === 'https:' &&
      u.hostname.toLowerCase() === 'github.com' &&
      u.username === '' &&
      u.password === ''
    );
  } catch {
    return false;
  }
}

/**
 * Returns a source URL for display and linking: the git+ prefix and any
 * username or password are removed. Returns null for anything that is not
 * an http(s) URL, so it is never used as a link target.
 */
export function displaySourceUrl(sourceUrl: string | undefined | null): string | null {
  if (!sourceUrl) return null;
  try {
    const u = new URL(sourceUrl.trim().replace(/^git\+/, ''));
    if (u.protocol !== 'https:' && u.protocol !== 'http:') return null;
    u.username = '';
    u.password = '';
    return u.toString();
  } catch {
    return null;
  }
}
