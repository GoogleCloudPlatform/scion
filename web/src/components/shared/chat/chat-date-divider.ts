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
 * Shared date-separator markup, styles, and formatting used by both the main
 * chat timeline (chat-thread.ts) and the inter-agent message marker
 * (chat-interagent-marker.ts). There is exactly one visual style for "a day
 * boundary in a conversation" — this module is the single source of it so
 * the two components never drift apart.
 */

import { html, css } from 'lit';
import type { TemplateResult } from 'lit';
import { formatInstant, zoneLabel } from '../../../utils/time.js';

/**
 * Format an ISO timestamp into the shared date-separator label (the
 * effective display zone, e.g. "Sep 23, 2026"), or '' if the timestamp is
 * invalid.
 *
 * This is also used as a day-*grouping* key (chat-thread.ts, run-length
 * comparison between consecutive messages), so it stays a pure date string —
 * `renderDateDivider` is what adds the zone to the rendered row.
 */
export function formatChatDate(iso: string): string {
  return formatInstant(iso, 'date');
}

/**
 * Render the shared date-separator row for a given date label, with the
 * effective zone appended (review R1-3, AC4: "a zone label"), e.g.
 * "Sep 23, 2026 · Asia/Tokyo".
 */
export function renderDateDivider(dateStr: string): TemplateResult {
  return html`
    <div class="date-divider">
      <span class="date-label">${dateStr}${dateStr ? html` · ${zoneLabel()}` : ''}</span>
    </div>
  `;
}

/** Styles for `renderDateDivider` — include in any component that uses it. */
export const chatDateDividerStyles = css`
  .date-divider {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.75rem 1rem 0.25rem;
  }

  .date-divider::before,
  .date-divider::after {
    content: '';
    flex: 1;
    height: 1px;
    background: var(--scion-border, #e2e8f0);
  }

  .date-label {
    font-size: var(--chat-fs-sm);
    font-weight: 600;
    color: var(--scion-text-muted, #64748b);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    white-space: nowrap;
  }
`;
