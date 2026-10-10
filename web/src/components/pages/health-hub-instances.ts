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
 * Hub instances table for the health dashboard (health dashboard F3,
 * ptone/scion#4136).
 *
 * One row per hub instance (process) from the summary's hub_instances
 * section, which the hub reads from its registry table only. State is
 * computed by the hub when it builds the summary: live, stale (no write for
 * 45 s) or stopped. Uptime and "last seen" are computed from the section's
 * as_of, the database clock that also wrote started_at and last_seen, so
 * they use one clock, neither the browser's nor the serving hub's. A
 * section without as_of falls back to the summary's generated_at.
 *
 * Each row shows the instance's own failing checks under its status, its
 * own database connection pool (in use / limit) and the counts of the
 * integrations it runs, by health, as last written to its registry row;
 * the hub that serves the summary reads no pool and queries no plugin
 * itself. The counts are shown to every caller; integration names are not
 * part of this table.
 *
 * The section is absent when an older hub replica served the summary
 * (during a rollout): the dashboard then hides this table. A null section
 * means the registry could not be read.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { healthPillStyles, healthTone, type HealthTone } from './health-status.js';
import type { HealthSummaryIntegrationCounts } from './health-integrations.js';

/** One hub instance of GET /api/v1/admin/health/summary. */
export interface HealthHubInstance {
  id: string;
  /** Short display name (pod name, Cloud Run revision or host name). */
  label: string;
  version: string;
  /** live, stale or stopped. */
  state: string;
  /** True for the instance that built this summary. */
  serving: boolean;
  /** RFC 3339; the instance's first registry write. */
  started_at: string;
  /** RFC 3339; the instance's last registry write. */
  last_seen: string;
  /** Null unless the instance stopped cleanly. */
  stopped_at: string | null;
  /** Last reported status: healthy, degraded or unhealthy. */
  status: string;
  /** Last reported checks; fixed words only. */
  checks?: Record<string, string>;
  /** Last reported database connection pool; null when none was reported. */
  database?: HealthHubInstanceDB | null;
  /** Counts of the integrations the instance last reported, by health. */
  integration_counts?: HealthSummaryIntegrationCounts;
  /** True when the instance cut its integration list to fit its row. */
  integrations_truncated?: boolean;
}

/** One hub instance's database connection pool. */
export interface HealthHubInstanceDB {
  pool_active: number;
  pool_idle: number;
  /** 0 when the pool has no limit. */
  pool_max: number;
  /** Cumulative waits for a connection since the instance started. */
  pool_wait_count_total: number;
}

/** The summary's hub_instances block. */
export interface HealthSummaryHubInstances {
  /** RFC 3339; the database clock the section was computed at. */
  as_of?: string;
  items: HealthHubInstance[];
  /** Live instances, including any past the row cap. */
  live: number;
  /** All listed instances, including any past the row cap. */
  total: number;
  truncated: boolean;
}

/**
 * Formats a duration in milliseconds compactly with its two largest units:
 * "3d 4h", "2h 5m", "4m 10s", "12s". A negative or non-finite value
 * (clock skew, bad data) renders as an empty string.
 */
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '';
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${sec}s`;
  return `${sec}s`;
}

function diffMs(
  laterIso: string | null | undefined,
  earlierIso: string | null | undefined
): number {
  if (!laterIso || !earlierIso) return Number.NaN;
  return new Date(laterIso).getTime() - new Date(earlierIso).getTime();
}

/**
 * Uptime of a live instance: the reference time (as_of) minus started_at.
 * Empty for a stale or stopped instance, whose uptime is not known.
 */
export function instanceUptime(i: HealthHubInstance, referenceTime: string): string {
  if (i.state !== 'live') return '';
  return formatDuration(diffMs(referenceTime, i.started_at));
}

/** "12s ago" from the reference time (as_of) and last_seen; empty when unknown. */
export function instanceLastSeen(i: HealthHubInstance, referenceTime: string): string {
  const d = formatDuration(Math.max(0, diffMs(referenceTime, i.last_seen)));
  return d ? `${d} ago` : '';
}

/** Check values that count as passing. */
const PASSING_CHECK_VALUES = new Set(['healthy', 'available']);

/**
 * The instance's non-passing checks as "name: value", sorted by name. A
 * check passes when its value is healthy or available.
 */
export function failingChecks(checks: Record<string, string> | undefined): string[] {
  return Object.entries(checks ?? {})
    .filter(([, v]) => !PASSING_CHECK_VALUES.has(v))
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([k, v]) => `${k}: ${v}`);
}

/** "3/25" (in use / limit); without a limit, "3". */
export function poolUsage(db: HealthHubInstanceDB): string {
  return db.pool_max > 0 ? `${db.pool_active}/${db.pool_max}` : `${db.pool_active}`;
}

/** The pool cell's tooltip: "3 in use, 2 idle, limit 25, 4 waits". */
export function poolDetail(db: HealthHubInstanceDB): string {
  const limit = db.pool_max > 0 ? `limit ${db.pool_max}` : 'no limit';
  const waits = db.pool_wait_count_total === 1 ? 'wait' : 'waits';
  return `${db.pool_active} in use, ${db.pool_idle} idle, ${limit}, ${db.pool_wait_count_total} ${waits}`;
}

/**
 * The Integrations cell: the total, then the non-healthy counts, e.g.
 * "3 (1 unhealthy, 1 unknown)"; "2" when all are healthy; "+" after the
 * total when the instance cut its list. Empty when the instance runs none.
 */
export function integrationCountsText(
  c: HealthSummaryIntegrationCounts | null | undefined,
  truncated = false
): string {
  if (!c || (c.total ?? 0) <= 0) return '';
  const parts = (['unhealthy', 'degraded', 'unknown'] as const)
    .filter((k) => (c[k] ?? 0) > 0)
    .map((k) => `${c[k]} ${k}`);
  const total = `${c.total}${truncated ? '+' : ''}`;
  return parts.length > 0 ? `${total} (${parts.join(', ')})` : total;
}

/** The Integrations cell's tooltip: every count, healthy first. */
export function integrationCountsDetail(
  c: HealthSummaryIntegrationCounts | null | undefined
): string {
  if (!c || (c.total ?? 0) <= 0) return '';
  return `${c.healthy} healthy, ${c.degraded} degraded, ${c.unhealthy} unhealthy, ${c.unknown} unknown`;
}

/**
 * Instance labels by instance ID, for naming the instances that run an
 * integration. An instance without a label maps to its ID.
 */
export function hubInstanceLabels(
  list: HealthSummaryHubInstances | null | undefined
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const i of list?.items ?? []) out[i.id] = i.label || i.id;
  return out;
}

/** The tone of an instance state: live ok, stale warn, stopped neutral. */
export function instanceStateTone(state: string): HealthTone {
  switch (state) {
    case 'live':
      return 'ok';
    case 'stale':
      return 'warn';
    default:
      return 'neutral';
  }
}

@customElement('scion-health-hub-instances')
export class ScionHealthHubInstances extends LitElement {
  /** The summary's hub_instances; null when the hub could not read it. */
  @property({ attribute: false })
  instances: HealthSummaryHubInstances | null = null;

  /** The summary's generated_at; used for uptime and age only when as_of is missing. */
  @property({ attribute: false })
  generatedAt = '';

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
      }

      .card-title {
        font-size: 0.875rem;
        font-weight: 600;
        color: var(--scion-text-muted);
        text-transform: uppercase;
        letter-spacing: 0.05em;
        margin: 0 0 0.75rem 0;
      }

      .empty,
      .note,
      .muted {
        color: var(--scion-text-muted);
      }

      .empty,
      .note {
        font-size: 0.875rem;
      }

      .note {
        margin-top: 0.5rem;
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

      td.label {
        white-space: normal;
        overflow-wrap: anywhere;
        min-width: 8rem;
        font-weight: 600;
      }

      .serving {
        font-weight: 400;
        color: var(--scion-text-muted);
      }

      td.num {
        font-variant-numeric: tabular-nums;
      }

      td.status {
        white-space: normal;
      }

      ul.failing {
        margin: 0.25rem 0 0 0;
        padding: 0;
        list-style: none;
        font-size: 0.8125rem;
        color: var(--scion-text-muted);
        overflow-wrap: anywhere;
      }
    `,
  ];

  override render(): TemplateResult {
    const list = this.instances;
    if (!list) {
      return html`
        <div class="card">
          <div class="card-title">Hub instances</div>
          <div class="empty">Hub instance data not available</div>
        </div>
      `;
    }
    const items = list.items ?? [];
    if (items.length === 0) {
      return html`
        <div class="card">
          <div class="card-title">Hub instances</div>
          <div class="empty">No hub instance is reporting</div>
        </div>
      `;
    }
    return html`
      <div class="card">
        <div class="card-title">Hub instances</div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Label</th>
                <th scope="col">State</th>
                <th scope="col">Version</th>
                <th scope="col">Uptime</th>
                <th scope="col">Status</th>
                <th scope="col">DB pool</th>
                <th scope="col">Integrations</th>
                <th scope="col">Last seen</th>
              </tr>
            </thead>
            <tbody>
              ${items.map((i) => this.renderRow(i))}
            </tbody>
          </table>
        </div>
        ${list.truncated
          ? html`<div class="note">Showing ${items.length} of ${list.total}</div>`
          : nothing}
      </div>
    `;
  }

  /** The reference time for uptime and age: as_of, else generated_at. */
  private referenceTime(): string {
    return this.instances?.as_of || this.generatedAt;
  }

  private renderRow(i: HealthHubInstance): TemplateResult {
    const ref = this.referenceTime();
    const uptime = instanceUptime(i, ref);
    const lastSeen = instanceLastSeen(i, ref);
    const live = i.state === 'live';
    const failing = failingChecks(i.checks);
    const db = i.database;
    return html`
      <tr data-instance-id=${i.id} data-state=${i.state}>
        <td class="label" title=${i.id}>
          ${i.label || i.id}${i.serving
            ? html` <span class="serving">(this instance)</span>`
            : nothing}
        </td>
        <td class="state">
          <span class="pill tone-${instanceStateTone(i.state)}">${i.state || 'unknown'}</span>
        </td>
        <td class="version">${i.version || html`<span class="muted">—</span>`}</td>
        <td class="uptime num">${uptime || html`<span class="muted">—</span>`}</td>
        <td class="status">
          ${live
            ? html`<span class="pill tone-${healthTone(i.status)}">${i.status || 'unknown'}</span>`
            : html`<span class="muted">last reported: ${i.status || 'unknown'}</span>`}
          ${failing.length > 0
            ? html`<ul class="failing" data-role="failing-checks">
                ${failing.map((c) => html`<li>${c}</li>`)}
              </ul>`
            : nothing}
        </td>
        <td class="pool num" data-role="pool" title=${db ? poolDetail(db) : ''}>
          ${db ? poolUsage(db) : html`<span class="muted">—</span>`}
        </td>
        <td
          class="integrations num"
          data-role="integrations"
          title=${integrationCountsDetail(i.integration_counts)}
        >
          ${integrationCountsText(i.integration_counts, i.integrations_truncated === true) ||
          html`<span class="muted">—</span>`}
        </td>
        <td class="last-seen num" title=${i.last_seen}>
          ${lastSeen || html`<span class="muted">—</span>`}
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-hub-instances': ScionHealthHubInstances;
  }
}
