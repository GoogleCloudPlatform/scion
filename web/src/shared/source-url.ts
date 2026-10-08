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

/** How a stored source is shown: its text, and a link target when it is a web URL. */
export interface SourceDisplay {
  text: string;
  href: string | null;
}

/** Label shown for sources that are not web or built-in URLs. */
export const NON_WEB_SOURCE_LABEL = 'non-web source';

/**
 * Describes a stored source for display. The display never shows credentials
 * embedded in a source string: http(s) URLs are shown and linked without any
 * username or password (and without a git+ prefix), builtin:// URLs are shown
 * as text without any username or password, and every other source is shown
 * as a fixed label rather than its raw text. Returns null for an empty source.
 */
export function describeSourceUrl(sourceUrl: string | undefined | null): SourceDisplay | null {
  const raw = sourceUrl?.trim();
  if (!raw) return null;
  let u: URL;
  try {
    u = new URL(raw.replace(/^git\+/, ''));
  } catch {
    return { text: NON_WEB_SOURCE_LABEL, href: null };
  }
  if (!u.host) return { text: NON_WEB_SOURCE_LABEL, href: null };
  u.username = '';
  u.password = '';
  if (u.protocol === 'https:' || u.protocol === 'http:') {
    return { text: u.toString(), href: u.toString() };
  }
  if (u.protocol === 'builtin:') {
    return { text: u.toString(), href: null };
  }
  return { text: NON_WEB_SOURCE_LABEL, href: null };
}
