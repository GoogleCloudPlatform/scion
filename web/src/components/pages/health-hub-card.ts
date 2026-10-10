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
 * Hub card for the health dashboard (ptone/scion#3595, fleet view
 * ptone/scion#4140).
 *
 * Describes the whole fleet of hub instances, as the hub computes it from
 * its registry rows: the fleet status, "N of M instances healthy" (live
 * instances only), the version (or "mixed") and every non-healthy check of
 * a live instance, each labelled with its instance and linked to that
 * instance's row in the Hub instances table (health-hub-instances.ts).
 * Uptime, per-instance checks and the database pool are in that table.
 *
 * While the service account assignment check cannot run on a live
 * instance (the summary's service_account_check section is present), the
 * card also shows that diagnostic: the server's remedy, the instances that
 * report it and its docs link.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import { healthPillStyles, healthTone } from './health-status.js';

/** Live hub instances counted by their last reported status. */
export interface HealthSummaryHubFleet {
  live: number;
  healthy: number;
  degraded: number;
  unhealthy: number;
}

/** One non-healthy check of one live hub instance. */
export interface HealthSummaryHubCheck {
  instance_id: string;
  instance_label: string;
  name: string;
  /** Fixed word: unhealthy, degraded, unavailable or unknown. */
  value: string;
}

/** The summary's hub block: the whole fleet of hub instances. */
export interface HealthSummaryHub {
  /** Fleet status: healthy, degraded or unhealthy; unknown when not reported. */
  status: string;
  /** The hub instance that served this summary ("this instance"). */
  instance_id: string;
  /** The live instances' version, "mixed" when they differ, empty when none is live. */
  version: string;
  connected_brokers: number;
  active_agents: number;
  projects: number;
  /** Null when the hub instance data could not be read. */
  instances?: HealthSummaryHubFleet | null;
  /** Non-healthy checks of live instances, critical checks first. */
  unhealthy_checks?: HealthSummaryHubCheck[];
}

/** The service_account_check section of GET /api/v1/admin/health/summary. */
export interface HealthSummaryServiceAccountCheck {
  status: string;
  cause: string;
  remedy: string;
  docs_url: string;
  /** Labels of the live hub instances that report it, sorted. */
  instances?: string[];
}

/**
 * The fragment ID of a hub instance's row in the Hub instances table, so
 * an item about the instance can link to it.
 */
export function hubInstanceAnchor(instanceId: string): string {
  return `hub-instance-${encodeURIComponent(instanceId)}`;
}

/**
 * "2 of 3 instances healthy" from the fleet counts (live instances only);
 * "No hub instance is reporting" when none is live; empty when the counts
 * were not reported.
 */
export function fleetHealthyText(fleet: HealthSummaryHubFleet | null | undefined): string {
  if (!fleet) return '';
  if (fleet.live <= 0) return 'No hub instance is reporting';
  return `${fleet.healthy} of ${fleet.live} ${fleet.live === 1 ? 'instance' : 'instances'} healthy`;
}

@customElement('scion-health-hub-card')
export class ScionHealthHubCard extends LitElement {
  @property({ attribute: false })
  hub: HealthSummaryHub | null = null;

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

      .fleet {
        font-size: 0.875rem;
        color: var(--scion-text);
        margin: 0 0 0.75rem 0;
      }

      .check a {
        color: var(--scion-text);
        text-decoration: underline;
        text-decoration-color: var(--scion-border-hover);
        text-underline-offset: 2px;
      }

      .check a:hover {
        text-decoration-color: currentColor;
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

      .sa-check-instances {
        margin: 0 0 0.5rem 0;
        color: var(--scion-text-muted);
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
    const checks = hub.unhealthy_checks ?? [];
    const fleet = fleetHealthyText(hub.instances);
    return html`
      <section class="card" aria-labelledby="hub-title">
        <div class="card-head">
          <span class="card-title" id="hub-title">Hub</span>
          <span class="pill tone-${healthTone(hub.status)}" data-role="hub-status"
            >${hub.status || 'unknown'}</span
          >
        </div>
        <div class="fleet" data-role="fleet">${fleet || 'Hub instance data not available'}</div>
        ${checks.length > 0
          ? html`<ul class="checks" data-role="failing-checks">
              ${checks.map((c) => this.renderCheck(c))}
            </ul>`
          : nothing}
        ${this.renderServiceAccountCheck()}
        <div class="stat-row">
          <span class="label">Version</span><span data-role="version">${hub.version || '—'}</span>
        </div>
        <div class="stat-row">
          <span class="label">Connected brokers</span><span>${hub.connected_brokers}</span>
        </div>
        <div class="stat-row">
          <span class="label">Active agents</span><span>${hub.active_agents}</span>
        </div>
        <div class="stat-row"><span class="label">Projects</span><span>${hub.projects}</span></div>
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
      ${(c.instances ?? []).length > 0
        ? html`<p class="sa-check-instances" data-role="sa-check-instances">
            On ${(c.instances ?? []).join(', ')}
          </p>`
        : nothing}
      ${(c.docs_url ?? '').startsWith('https://')
        ? html`<a href=${c.docs_url} target="_blank" rel="noopener noreferrer"
            >Access the hub's identity needs</a
          >`
        : nothing}
    </div>`;
  }

  private renderCheck(c: HealthSummaryHubCheck): TemplateResult {
    const label = c.instance_label || c.instance_id;
    return html`<li data-check=${c.name} data-instance-id=${c.instance_id}>
      <div class="check">
        <span class="name"
          >${c.name} on
          <a href="#${hubInstanceAnchor(c.instance_id)}" title=${c.instance_id}>${label}</a></span
        >
        <span class="pill tone-${healthTone(c.value)}">${c.value || 'unknown'}</span>
      </div>
    </li>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-hub-card': ScionHealthHubCard;
  }
}
