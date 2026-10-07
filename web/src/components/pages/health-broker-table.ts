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
 * Runtime broker table for the health dashboard (ptone/scion#3582).
 *
 * One compact row per runtime broker from the summary's runtime_brokers
 * list. A null field means the broker did not report it and renders as a
 * neutral value: a dash for runtime, an empty cell for workspace storage,
 * "never" for the last heartbeat.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { ifDefined } from 'lit/directives/if-defined.js';

import { formatRelative } from '../../utils/time.js';

/** One runtime broker row of GET /api/v1/admin/health/summary. */
export interface HealthSummaryBroker {
  id: string;
  name: string;
  /** Empty when the broker never reported a version. */
  version: string;
  /** Liveness: online, offline or degraded. */
  status: string;
  /** Null when the broker has never sent a heartbeat. */
  last_heartbeat: string | null;
  /** Null when the broker reported no runtime profile. */
  runtime: { type: string; profile?: string } | null;
  /** Null when the broker never reported its workspace storage. */
  workspace_storage: { backend: string; nfs_healthy?: boolean } | null;
  /** Per-broker agent counts: running, and needing attention. */
  agents?: { running?: number; attention?: number };
}

/** The summary's runtime_brokers block. */
export interface HealthSummaryBrokerList {
  items: HealthSummaryBroker[];
  /** All runtime brokers, including those past the row cap. */
  total: number;
  truncated: boolean;
}

/**
 * Formats a broker heartbeat as a relative age. A null, undefined or
 * empty value, the Go zero time (`0001-01-01T00:00:00Z`), or any other
 * non-positive instant (the Unix epoch itself or any earlier time) means
 * the heartbeat was never reported and renders as "never". An unparsable
 * value renders as "unknown" and a future instant as "just now".
 */
export function formatHeartbeatAge(isoDate: string | null | undefined): string {
  if (!isoDate) return 'never';
  const ms = new Date(isoDate).getTime();
  if (Number.isNaN(ms)) return 'unknown';
  if (ms <= 0) return 'never';
  // A future instant is clock skew between hub and browser.
  if (ms > Date.now()) return 'just now';
  return formatRelative(isoDate, { style: 'narrow' });
}

/**
 * Agents cell: "running / needing attention", or null when the broker row
 * carries no agent counts.
 */
export function agentsCell(
  b: HealthSummaryBroker
): { running: number; attention: number; title: string } | null {
  const a = b.agents;
  if (!a || typeof a.running !== 'number' || typeof a.attention !== 'number') return null;
  return {
    running: a.running,
    attention: a.attention,
    title: `${a.running} running, ${a.attention} needing attention`,
  };
}

/** True when the row needs a look: not online, or an unhealthy NFS share. */
export function brokerHasProblem(b: HealthSummaryBroker): boolean {
  return b.status !== 'online' || b.workspace_storage?.nfs_healthy === false;
}

/** Problems first, then by display name, then by ID. */
export function sortBrokers(items: readonly HealthSummaryBroker[]): HealthSummaryBroker[] {
  return [...items].sort((a, b) => {
    const pa = brokerHasProblem(a) ? 0 : 1;
    const pb = brokerHasProblem(b) ? 0 : 1;
    if (pa !== pb) return pa - pb;
    const byName = (a.name || a.id).localeCompare(b.name || b.id);
    return byName !== 0 ? byName : a.id.localeCompare(b.id);
  });
}

/** Workspace storage cell text and tone; empty text when not reported. */
export function storageCell(b: HealthSummaryBroker): {
  text: string;
  tone: 'ok' | 'bad' | 'plain';
} {
  const s = b.workspace_storage;
  if (!s || !s.backend) return { text: '', tone: 'plain' };
  if (s.backend === 'nfs') {
    if (s.nfs_healthy === true) return { text: 'NFS ✓', tone: 'ok' };
    if (s.nfs_healthy === false) return { text: 'NFS ✗', tone: 'bad' };
    return { text: 'NFS', tone: 'plain' };
  }
  return { text: s.backend, tone: 'plain' };
}

function statusTone(status: string): 'ok' | 'bad' | 'warn' | 'neutral' {
  switch (status) {
    case 'online':
      return 'ok';
    case 'offline':
      return 'bad';
    case 'degraded':
      return 'warn';
    default:
      return 'neutral';
  }
}

@customElement('scion-health-broker-table')
export class ScionHealthBrokerTable extends LitElement {
  @property({ attribute: false })
  brokers: HealthSummaryBrokerList | null = null;

  static override styles = css`
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
    .note {
      font-size: 0.875rem;
      color: var(--scion-text-muted);
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

    td.name {
      white-space: normal;
      overflow-wrap: anywhere;
      min-width: 8rem;
    }

    td.num {
      font-variant-numeric: tabular-nums;
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

    .muted {
      color: var(--scion-text-muted);
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

  override render(): TemplateResult {
    const list = this.brokers;
    const items = list?.items ?? [];
    if (items.length === 0) {
      return html`
        <div class="card">
          <div class="card-title">Runtime brokers</div>
          <div class="empty">No runtime brokers registered</div>
        </div>
      `;
    }
    return html`
      <div class="card">
        <div class="card-title">Runtime brokers</div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th scope="col">Name</th>
                <th scope="col">Status</th>
                <th scope="col">Runtime</th>
                <th scope="col">Workspace storage</th>
                <th scope="col" title="Running / needing attention">Agents</th>
                <th scope="col">Version</th>
                <th scope="col">Last heartbeat</th>
              </tr>
            </thead>
            <tbody>
              ${sortBrokers(items).map((b) => this.renderRow(b))}
            </tbody>
          </table>
        </div>
        ${list?.truncated
          ? html`<div class="note">Showing ${items.length} of ${list.total}</div>`
          : nothing}
      </div>
    `;
  }

  private renderRow(b: HealthSummaryBroker): TemplateResult {
    const storage = storageCell(b);
    const runtime = b.runtime?.type;
    const profile = b.runtime?.profile;
    const agents = agentsCell(b);
    return html`
      <tr data-broker-id=${b.id}>
        <td class="name"><a href="/brokers/${encodeURIComponent(b.id)}">${b.name || b.id}</a></td>
        <td class="status">
          <span class="pill tone-${statusTone(b.status)}">${b.status || 'unknown'}</span>
        </td>
        <td
          class="runtime"
          title=${ifDefined(profile && profile !== runtime ? `profile ${profile}` : undefined)}
        >
          ${runtime ? runtime : html`<span class="muted">—</span>`}
        </td>
        <td class="storage">
          ${storage.tone === 'plain'
            ? storage.text
            : html`<span class="pill tone-${storage.tone}">${storage.text}</span>`}
        </td>
        <td class="agents num" title=${ifDefined(agents?.title)}>
          ${agents
            ? html`${agents.running} /
                <span class=${agents.attention > 0 ? 'pill tone-warn attention' : 'attention'}
                  >${agents.attention}</span
                >`
            : html`<span class="muted">—</span>`}
        </td>
        <td class="version">${b.version || html`<span class="muted">—</span>`}</td>
        <td class="heartbeat" title=${ifDefined(b.last_heartbeat || undefined)}>
          ${formatHeartbeatAge(b.last_heartbeat)}
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-broker-table': ScionHealthBrokerTable;
  }
}
