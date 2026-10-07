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
 * Agents card for the health dashboard (ptone/scion#3587).
 *
 * Shows phase counts in lifecycle order, active vs total, the errored share
 * of non-stopped agents, and one group per problem kind (errored, crashed,
 * offline) with linked "project / agent" items and "+N more" past the cap.
 * Stalled and suspended agents are not health signals and are never shown.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/** One phase count; the server sends these in lifecycle order. */
export interface HealthPhaseCount {
  phase: string;
  count: number;
}

/** One agent in a problem group. */
export interface HealthAgentRef {
  id: string;
  name: string;
  project_id: string;
  /** Empty when the hub could not resolve the project. */
  project_slug: string;
  /** Empty when the agent is not placed on a broker. */
  broker_id: string;
}

/** A problem kind with its true count and capped items. */
export interface HealthAgentGroup {
  kind: string;
  count: number;
  items: HealthAgentRef[];
}

/** The summary's agents block. */
export interface HealthSummaryAgents {
  total: number;
  active: number;
  /** Agents in phase error or with activity crashed, stopped excluded. */
  errored: number;
  /** Non-deleted agents not in phase stopped. */
  considered: number;
  by_phase: HealthPhaseCount[];
  problems: HealthAgentGroup[];
}

/** Lifecycle order, mirroring state.Phases() on the hub. */
export const PHASE_ORDER: readonly string[] = [
  'created',
  'provisioning',
  'cloning',
  'starting',
  'running',
  'suspended',
  'stopping',
  'stopped',
  'error',
];

/** Display order and labels of the problem groups. */
const GROUP_ORDER: readonly string[] = ['errored', 'crashed', 'offline'];
const GROUP_LABELS: Record<string, string> = {
  errored: 'Errored',
  crashed: 'Crashed',
  offline: 'Offline',
};

/**
 * Phase counts in lifecycle order, unknown phases after them sorted by
 * name, zero counts dropped. The server already orders the list; sorting
 * again keeps the chips stable even if it did not.
 */
export function orderPhases(phases: readonly HealthPhaseCount[]): HealthPhaseCount[] {
  const rank = (p: string): number => {
    const i = PHASE_ORDER.indexOf(p);
    return i === -1 ? PHASE_ORDER.length : i;
  };
  return phases
    .filter((p) => p.count > 0)
    .sort((a, b) => rank(a.phase) - rank(b.phase) || a.phase.localeCompare(b.phase));
}

/** Problem groups in display order; unknown kinds and empty groups are dropped. */
export function orderGroups(groups: readonly HealthAgentGroup[]): HealthAgentGroup[] {
  return GROUP_ORDER.flatMap((kind) => groups.filter((g) => g.kind === kind && g.count > 0));
}

/** "project / agent", so same-named agents in different projects differ. */
export function agentRefLabel(ref: HealthAgentRef): string {
  const project = ref.project_slug || ref.project_id || 'unknown project';
  return `${project} / ${ref.name || ref.id}`;
}

/** The agent detail page for a reference. */
export function agentRefHref(ref: HealthAgentRef): string {
  return `/agents/${encodeURIComponent(ref.id)}`;
}

/** Number of agents in the group not listed (count is the true count). */
export function moreCount(group: HealthAgentGroup): number {
  return Math.max(0, group.count - group.items.length);
}

/** "N of M (x%)" for the errored share of non-stopped agents. */
export function erroredShare(agents: Pick<HealthSummaryAgents, 'errored' | 'considered'>): string {
  if (!agents.considered) return `${agents.errored} of 0`;
  const pct = (agents.errored / agents.considered) * 100;
  const shown = pct > 0 && pct < 1 ? '<1' : String(Math.round(pct));
  return `${agents.errored} of ${agents.considered} (${shown}%)`;
}

@customElement('scion-health-agents-card')
export class ScionHealthAgentsCard extends LitElement {
  @property({ attribute: false })
  agents: HealthSummaryAgents | null = null;

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

    .totals,
    .phases {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem 1.5rem;
      font-size: 0.9375rem;
      color: var(--scion-text);
      margin: 0 0 0.75rem 0;
      padding: 0;
      list-style: none;
    }

    .phases {
      gap: 0.375rem 1rem;
      font-size: 0.875rem;
    }

    .stat {
      font-weight: 600;
      font-variant-numeric: tabular-nums;
    }

    .label {
      color: var(--scion-text-muted);
    }

    .group {
      margin-top: 0.75rem;
    }

    .group-head {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text);
      margin: 0 0 0.375rem 0;
    }

    .pill {
      display: inline-block;
      padding: 0.0625rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 600;
      font-variant-numeric: tabular-nums;
    }

    .tone-bad {
      background: var(--scion-badge-danger-bg);
      color: var(--scion-badge-danger-text);
    }

    .tone-warn {
      background: var(--scion-badge-warning-bg);
      color: var(--scion-badge-warning-text);
    }

    ul.items {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem 1rem;
      margin: 0;
      padding: 0;
      list-style: none;
      font-size: 0.875rem;
    }

    ul.items li {
      overflow-wrap: anywhere;
    }

    a {
      color: var(--scion-text);
      text-decoration: underline;
      text-decoration-color: var(--scion-border-hover);
      text-underline-offset: 2px;
    }

    a:hover {
      text-decoration-color: currentColor;
    }

    .more,
    .empty {
      color: var(--scion-text-muted);
      font-size: 0.875rem;
    }
  `;

  override render(): TemplateResult {
    const a = this.agents;
    if (!a) {
      return html`
        <div class="card">
          <div class="card-title">Agents</div>
          <div class="empty">Agent data not available</div>
        </div>
      `;
    }
    const groups = orderGroups(a.problems ?? []);
    return html`
      <div class="card">
        <div class="card-title">Agents</div>
        <ul class="totals">
          <li class="active">
            <span class="stat">${a.active}</span> <span class="label">active</span>
          </li>
          <li class="total">
            <span class="stat">${a.total}</span> <span class="label">total</span>
          </li>
          <li class="errored-share">
            <span class="label">Errored</span> <span class="stat">${erroredShare(a)}</span>
          </li>
        </ul>
        ${(a.by_phase ?? []).length > 0
          ? html`<ul class="phases">
              ${orderPhases(a.by_phase).map(
                (p) =>
                  html`<li data-phase=${p.phase}>
                    <span class="stat">${p.count}</span>
                    <span class="label">${p.phase || 'unknown'}</span>
                  </li>`
              )}
            </ul>`
          : nothing}
        ${groups.length === 0
          ? html`<div class="empty">No agents need attention</div>`
          : groups.map((g) => this.renderGroup(g))}
      </div>
    `;
  }

  private renderGroup(g: HealthAgentGroup): TemplateResult {
    const more = moreCount(g);
    return html`
      <div class="group" data-kind=${g.kind}>
        <div class="group-head">
          <span>${GROUP_LABELS[g.kind] ?? g.kind}</span>
          <span class="pill ${g.kind === 'offline' ? 'tone-warn' : 'tone-bad'}">${g.count}</span>
        </div>
        <ul class="items">
          ${g.items.map(
            (r) =>
              html`<li>
                <a href=${agentRefHref(r)} data-agent-id=${r.id}>${agentRefLabel(r)}</a>
              </li>`
          )}
          ${more > 0 ? html`<li class="more">+${more} more</li>` : nothing}
        </ul>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-health-agents-card': ScionHealthAgentsCard;
  }
}
