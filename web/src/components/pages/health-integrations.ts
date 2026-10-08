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
 * Integrations section for the health dashboard (ptone/scion#3584).
 *
 * One compact row per chat or messaging plugin from the summary's
 * integrations list. The section renders nothing when there are no
 * plugins. Plugin messages and details are not part of the summary; the
 * Integrations admin page has them.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/** One integration row of GET /api/v1/admin/health/summary. */
export interface HealthSummaryIntegration {
  name: string;
  platform: string;
  /** healthy, degraded, unhealthy or unknown. */
  health: string;
  connected: boolean;
  /** Empty when the plugin could not be queried. */
  version: string;
  /** Fixed server reason, e.g. "not managed by this hub instance". */
  reason?: string;
}

/** The Integrations admin page. */
export const INTEGRATIONS_PAGE = '/admin/integrations';

export function integrationHealthTone(health: string): 'ok' | 'warn' | 'bad' | 'neutral' {
  switch (health) {
    case 'healthy':
      return 'ok';
    case 'degraded':
      return 'warn';
    case 'unhealthy':
      return 'bad';
    default:
      return 'neutral';
  }
}

@customElement('scion-health-integrations')
export class ScionHealthIntegrations extends LitElement {
  @property({ attribute: false })
  integrations: HealthSummaryIntegration[] | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    :host([hidden]) {
      display: none;
    }

    .card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1.25rem;
    }

    .card-head {
      display: flex;
      align-items: baseline;
      justify-content: space-between;
      gap: 1rem;
      margin: 0 0 0.75rem 0;
    }

    .card-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text-muted);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .manage {
      font-size: 0.8125rem;
      font-weight: 400;
    }

    .table-wrap {
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
      color: var(--scion-text);
    }

    th {
      text-align: left;
      font-weight: 600;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      padding: 0.375rem 0.75rem 0.375rem 0;
      border-bottom: 1px solid var(--scion-border);
      white-space: nowrap;
    }

    td {
      padding: 0.375rem 0.75rem 0.375rem 0;
      border-bottom: 1px solid var(--scion-border);
      vertical-align: middle;
      white-space: nowrap;
    }

    tr:last-child td {
      border-bottom: none;
    }

    td.name {
      white-space: normal;
      overflow-wrap: anywhere;
      min-width: 8rem;
    }

    td.health {
      white-space: normal;
    }

    a {
      color: var(--scion-text);
      font-weight: 600;
      text-decoration: underline;
      text-decoration-color: var(--scion-border-hover);
      text-underline-offset: 2px;
    }

    a:hover {
      text-decoration-color: currentColor;
    }

    .muted,
    .reason {
      color: var(--scion-text-muted);
    }

    .reason {
      font-size: 0.8125rem;
      margin-left: 0.375rem;
    }

    .pill {
      display: inline-block;
      padding: 0.0625rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 600;
    }

    .tone-ok {
      background: var(--scion-badge-success-bg);
      color: var(--scion-badge-success-text);
    }

    .tone-bad {
      background: var(--scion-badge-danger-bg);
      color: var(--scion-badge-danger-text);
    }

    .tone-warn {
      background: var(--scion-badge-warning-bg);
      color: var(--scion-badge-warning-text);
    }

    .tone-neutral {
      background: var(--scion-badge-neutral-bg);
      color: var(--scion-badge-neutral-text);
    }
  `;

  override render(): TemplateResult | typeof nothing {
    const items = this.integrations ?? [];
    if (items.length === 0) return nothing;
    return html`
      <section class="card" aria-labelledby="integrations-title">
        <div class="card-head">
          <span class="card-title" id="integrations-title">Integrations</span>
          <a class="manage" href=${INTEGRATIONS_PAGE}>Manage integrations</a>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Platform</th>
                <th scope="col">Health</th>
                <th scope="col">Connected</th>
                <th scope="col">Version</th>
              </tr>
            </thead>
            <tbody>
              ${items.map((it) => this.renderRow(it))}
            </tbody>
          </table>
        </div>
      </section>
    `;
  }

  private renderRow(it: HealthSummaryIntegration): TemplateResult {
    const health = it.health || 'unknown';
    return html`
      <tr data-integration=${it.name}>
        <td class="name">
          <a href="${INTEGRATIONS_PAGE}/${encodeURIComponent(it.name)}">${it.name}</a>
        </td>
        <td class="platform">${it.platform || html`<span class="muted">—</span>`}</td>
        <td class="health">
          <span class="pill tone-${integrationHealthTone(health)}">${health}</span>
          ${it.reason ? html`<span class="reason">${it.reason}</span>` : nothing}
        </td>
        <td class="connected">${it.connected ? 'yes' : 'no'}</td>
        <td class="version">${it.version || html`<span class="muted">—</span>`}</td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-integrations': ScionHealthIntegrations;
  }
}
