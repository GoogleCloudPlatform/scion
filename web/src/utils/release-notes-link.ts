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

import { html } from 'lit';
import type { TemplateResult } from 'lit';

import { safeHttpsUrl } from './http-url.js';

/**
 * A release-notes link for a URL that comes from outside the hub (the
 * release manifest or the GitHub release catalog): a link only for an
 * https URL, otherwise the value as plain text (ptone/scion#6134).
 */
export function renderReleaseNotesLink(url: string): TemplateResult {
  const href = safeHttpsUrl(url);
  return href
    ? html`<a href="${href}" target="_blank" rel="noopener noreferrer" class="release-notes-link">
        View Release Notes
        <sl-icon name="box-arrow-up-right" style="font-size: 0.75rem;"></sl-icon>
      </a>`
    : html`<span class="release-notes-text">${url}</span>`;
}
