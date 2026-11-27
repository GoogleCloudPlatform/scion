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
 * Reusable Track 1 measurement_status badge component.
 *
 * Displays the measurement coverage status pill (measured, partial,
 * calls_only, unmeasured, legacy_client, stale) and optional unmeasured/stale
 * agent counts alongside usage totals.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { UsageMeasurementStatus } from '../../shared/types.js';
import { formatNumber } from '../../utils/format-number.js';

export type MeasurementBadgeVariant = 'success' | 'warning' | 'neutral';

/**
 * Map measurement_status to a Shoelace badge variant and human-readable label.
 */
export function measurementStatusDisplay(status: UsageMeasurementStatus): {
  variant: MeasurementBadgeVariant;
  label: string;
} {
  switch (status) {
    case 'measured':
      return { variant: 'success', label: 'Measured' };
    case 'stale':
      return { variant: 'warning', label: 'Stale' };
    case 'partial':
      return { variant: 'warning', label: 'Partial' };
    case 'calls_only':
      return { variant: 'warning', label: 'Calls only' };
    case 'legacy_client':
      return { variant: 'neutral', label: 'Legacy client' };
    case 'unmeasured':
    default:
      return { variant: 'neutral', label: 'Unmeasured' };
  }
}

/**
 * Format inline unmeasured/stale agent count text, or null when both are 0.
 */
export function formatMeasurementAgentCounts(
  unmeasuredAgents: number,
  staleAgents: number
): string | null {
  const parts: string[] = [];
  if (unmeasuredAgents > 0) {
    parts.push(`${formatNumber(unmeasuredAgents)} unmeasured`);
  }
  if (staleAgents > 0) {
    parts.push(`${formatNumber(staleAgents)} stale`);
  }
  return parts.length > 0 ? parts.join(' · ') : null;
}

@customElement('scion-measurement-status-badge')
export class ScionMeasurementStatusBadge extends LitElement {
  @property({ type: String })
  status: UsageMeasurementStatus = 'unmeasured';

  @property({ type: Number })
  unmeasuredAgents = 0;

  @property({ type: Number })
  staleAgents = 0;

  static override styles = css`
    :host {
      display: inline-flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.375rem;
      vertical-align: middle;
    }

    .counts {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      font-variant-numeric: tabular-nums;
      white-space: nowrap;
    }
  `;

  override render(): TemplateResult {
    const info = measurementStatusDisplay(this.status);
    const countsText = formatMeasurementAgentCounts(this.unmeasuredAgents, this.staleAgents);

    return html`
      <sl-badge
        variant=${info.variant}
        data-testid="measurement-status-pill"
        data-status=${this.status}
      >
        ${info.label}
      </sl-badge>
      ${countsText
        ? html`<span class="counts" data-testid="measurement-status-counts">${countsText}</span>`
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-measurement-status-badge': ScionMeasurementStatusBadge;
  }
}
