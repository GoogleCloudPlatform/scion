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
 * The link under a message context menu's target, for its "Copy link
 * address" item.
 */

/** Schemes whose addresses the menu offers to copy. */
const COPYABLE_PROTOCOLS = new Set(['http:', 'https:', 'mailto:']);

/**
 * The resolved address of the HTML link the event targets (the target or an
 * ancestor), or null when there is none or its scheme is not copyable.
 *
 * `path` is the event's composed path, so links inside a message's shadow
 * root are found; the walk stops at `host` (the message element). The
 * address is the anchor's `href` property, resolved as the browser shows
 * it. Only http, https and mailto addresses qualify, whatever the page's
 * origin; every other scheme yields null, and so does an empty or bare "#"
 * href (the file-path links), which has no address of its own.
 */
export function copyableLinkHref(
  path: readonly EventTarget[],
  host: EventTarget | null
): string | null {
  for (const node of path) {
    if (node === host) break;
    if (!(node instanceof HTMLAnchorElement) || !node.hasAttribute('href')) continue;
    // A bare "#" (the file-path links) has no address of its own to copy.
    const raw = (node.getAttribute('href') ?? '').trim();
    if (raw === '' || raw === '#') return null;
    let url: URL;
    try {
      url = new URL(node.href);
    } catch {
      return null;
    }
    return COPYABLE_PROTOCOLS.has(url.protocol) ? url.href : null;
  }
  return null;
}
