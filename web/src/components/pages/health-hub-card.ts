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
 * Hub card for the health dashboard (ptone/scion#3595).
 *
 * Lists every hub check as a row (name, status), with the database folded
 * in: the database row carries the connection pool as a sub-block. The
 * checks and figures are those of the hub instance that served the
 * summary ("this instance").
 *
 * While the service account assignment check cannot run (the summary's
 * service_account_check section is present), the card also shows that
 * hub-level diagnostic: the server's remedy and its docs link.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { healthPillStyles, healthTone } from './health-status.js';

/** The summary's hub block. */
export interface HealthSummaryHub {
  status: string;
  /** The hub instance that served this summary ("this instance"). */
  instance_id: string;
  version: string;
  uptime: string;
  connected_brokers: number;
  active_agents: number;
  projects: number;
  /** The hub's /healthz check map. */
  checks?: Record<string, string>;
  /** Non-healthy checks as "key: value" — the cause of a degraded/unhealthy hub. */
  unhealthy_checks?: string[];
}

/** The summary's database block (pool counters from sql.DBStats). */
export interface HealthSummaryDatabase {
  status: string;
  pool_active: number;
  /** 0 when the pool has no limit. */
  pool_max: number;
  pool_wait_count_total: number;
  pool_idle: number;
}

/** The service_account_check section of GET /api/v1/admin/health/summary. */
export interface HealthSummaryServiceAccountCheck {
  status: string;
  cause: string;
  remedy: string;
  docs_url: string;
  since: string;
  last_seen: string;
}

/** One check row of the card. */
export interface HubCheckRow {
  name: string;
  status: string;
}

/** The check name the database row uses, and that carries the pool sub-block. */
export const DATABASE_CHECK = 'database';

/**
 * Every hub check as a row, sorted by name. When the check map has no
 * database entry, the database block's own status is shown as that row,
 * so the folded-in database is always listed.
 */
export function hubCheckRows(
  hub: HealthSummaryHub,
  database: HealthSummaryDatabase | null | undefined
): HubCheckRow[] {
  const rows = Object.entries(hub.checks ?? {}).map(([name, status]) => ({ name, status }));
  if (database && !rows.some((r) => r.name === DATABASE_CHECK)) {
    rows.push({ name: DATABASE_CHECK, status: database.status });
  }
  return rows.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

/** "Pool 3/25 in use, 2 idle"; without a limit, "Pool 3 in use, 2 idle". */
export function poolSummary(db: HealthSummaryDatabase): string {
  const inUse = db.pool_max > 0 ? `${db.pool_active}/${db.pool_max}` : `${db.pool_active}`;
  return `Pool ${inUse} in use, ${db.pool_idle} idle`;
}

@customElement('scion-health-hub-card')
export class ScionHealthHubCard extends LitElement {
  @property({ attribute: false })
  hub: HealthSummaryHub | null = null;

  @property({ attribute: false })
  database: HealthSummaryDatabase | null = null;

  /** Present only while the service account assignment check cannot run. */
  @property({ attribute: false })
  serviceAccountCheck: HealthSummaryServiceAccountCheck | null = null;

  static override styles = [
    healthPillStyles,
    css`
      :host {
        display: block;
      }

      .card {
        background: var(--scion-surface);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius-lg);
        padding: 1.25rem;
        height: 100%;
        box-sizing: border-box;
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

      .scope {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
      }

      ul.checks {
        margin: 0 0 0.75rem 0;
        padding: 0;
        list-style: none;
      }

      ul.checks li {
        padding: 0.375rem 0;
        border-bottom: 1px solid var(--scion-border);
        font-size: 0.875rem;
        color: var(--scion-text);
      }

      ul.checks li:last-child {
        border-bottom: none;
      }

      .check {
        display: flex;
        justify-content: space-between;
        align-items: center;
        gap: 1rem;
      }

      .check .name {
        overflow-wrap: anywhere;
      }

      .check .pill {
        white-space: normal;
        text-align: right;
      }

      .pool {
        font-size: 0.8125rem;
        color: var(--scion-text-muted);
        padding: 0.125rem 0 0 0.75rem;
      }

      .stat-row {
        display: flex;
        justify-content: space-between;
        font-size: 0.875rem;
        padding: 0.25rem 0;
        color: var(--scion-text);
      }

      .stat-row .label {
        color: var(--scion-text-muted);
      }

      .empty {
        color: var(--scion-text-muted);
        font-size: 0.875rem;
        margin: 0 0 0.75rem 0;
      }

      .sa-check {
        border-top: 1px solid var(--scion-border);
        border-bottom: 1px solid var(--scion-border);
        padding: 0.75rem 0;
        margin: 0 0 0.75rem 0;
        font-size: 0.875rem;
        color: var(--scion-text);
      }

      .sa-check-head {
        display: flex;
        justify-content: space-between;
        align-items: center;
        gap: 1rem;
        font-weight: 600;
      }

      .sa-check-remedy {
        margin: 0.5rem 0;
        overflow-wrap: anywhere;
      }

      .sa-check a {
        color: var(--scion-text);
        text-decoration: underline;
        text-decoration-color: var(--scion-border-hover);
        text-underline-offset: 2px;
      }

      .sa-check a:hover {
        text-decoration-color: currentColor;
      }
    `,
  ];

  override render(): TemplateResult {
    const hub = this.hub;
    if (!hub) {
      return html`<div class="card">
        <div class="card-head"><span class="card-title">Hub</span></div>
        <div class="empty">Hub data not available</div>
      </div>`;
    }
    const rows = hubCheckRows(hub, this.database);
    return html`
      <section class="card" aria-labelledby="hub-title">
        <div class="card-head">
          <span class="card-title" id="hub-title">Hub</span>
          <span class="pill tone-${healthTone(hub.status)}" data-role="hub-status"
            >${hub.status || 'unknown'}</span
          >
        </div>
        ${rows.length > 0
          ? html`<ul class="checks">
              ${rows.map((r) => this.renderCheck(r))}
            </ul>`
          : html`<div class="empty">No checks reported</div>`}
        ${this.renderServiceAccountCheck()}
        <div class="stat-row"><span class="label">Uptime</span><span>${hub.uptime}</span></div>
        <div class="stat-row"><span class="label">Version</span><span>${hub.version}</span></div>
        <div class="stat-row">
          <span class="label">Connected brokers</span><span>${hub.connected_brokers}</span>
        </div>
        <div class="stat-row">
          <span class="label">Active agents</span><span>${hub.active_agents}</span>
        </div>
        <div class="stat-row"><span class="label">Projects</span><span>${hub.projects}</span></div>
        <div class="scope">Checks and figures from this instance</div>
      </section>
    `;
  }

  /** The service account check diagnostic; nothing when the section is absent. */
  private renderServiceAccountCheck(): TemplateResult | typeof nothing {
    const c = this.serviceAccountCheck;
    if (!c) return nothing;
    return html`<div class="sa-check" data-role="sa-check">
      <div class="sa-check-head">
        <span>Service Account Assignment Check</span>
        <span class="pill tone-${healthTone(c.status)}">Cannot run</span>
      </div>
      <p class="sa-check-remedy">${c.remedy}</p>
      ${(c.docs_url ?? '').startsWith('https://')
        ? html`<a href=${c.docs_url} target="_blank" rel="noopener noreferrer"
            >Access the hub's identity needs</a
          >`
        : nothing}
    </div>`;
  }

  private renderCheck(r: HubCheckRow): TemplateResult {
    const db = r.name === DATABASE_CHECK ? this.database : null;
    return html`<li data-check=${r.name}>
      <div class="check">
        <span class="name">${r.name}</span>
        <span class="pill tone-${healthTone(r.status)}">${r.status || 'unknown'}</span>
      </div>
      ${db ? html`<div class="pool" data-role="pool">${poolSummary(db)}</div>` : nothing}
    </li>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-hub-card': ScionHealthHubCard;
  }
}
