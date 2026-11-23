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

import { html, type TemplateResult } from 'lit';
import type { ViewSchema } from '../../../shared/filter-query/index.js';

/**
 * Render the inline grammar cheat-sheet and key table for `schema`.
 */
export function renderFilterHelp(schema: ViewSchema): TemplateResult {
  return html`
    <div class="help-popover" role="dialog" aria-label="Filter syntax help">
      <div class="help-title">Filter syntax</div>
      <div class="help-cheat">
        <code>key:val</code> match &middot; <code>key:a,b</code> either &middot;
        <code>-key:val</code> exclude &middot; <code>"a b"</code> quote spaces
      </div>
      ${schema.textHelp ? html`<div class="help-text">${schema.textHelp}.</div>` : null}
      <table class="help-table">
        <thead>
          <tr>
            <th>Key</th>
            <th>Description</th>
          </tr>
        </thead>
        <tbody>
          ${schema.keys.map(
            (k) => html`
              <tr>
                <td><code>${k.name}:</code></td>
                <td>
                  ${k.help}
                  ${k.enum?.length
                    ? html`<div class="help-values">${k.enum.join(', ')}</div>`
                    : null}
                </td>
              </tr>
            `
          )}
        </tbody>
      </table>
    </div>
  `;
}
