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
 * Read-only "Plan a move to flat Runtime Brokers" section of a legacy
 * Runtime Broker's page in the Brokers area.
 *
 * It renders GET /api/v1/runtime-brokers/{id}/conversion-plan: the plan the
 * Runtime Broker reported (proposed instances, profile map, findings) and
 * what the Hub adds (bound agents, providing projects, capacity). It is
 * shown, and the plan fetched, only with the hub.flat_runtime_brokers
 * experiment on and only for a legacy Runtime Broker; the Hub checks the
 * experiment again and decides what is returned. Agents and projects the
 * caller cannot read appear only as counts. Every string is rendered as
 * text. There are no actions: applying a plan is not part of this page.
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { RuntimeBroker } from '../../shared/types.js';
import { isFlatBroker } from '../../shared/runtime-target.js';
import {
  CLI_POINTER,
  CONVERSION_PLAN_NOT_REPORTED,
  RUNTIME_BROKER_NOT_LEGACY,
  capacityOutcomeLabel,
  findingText,
  instanceRows,
  isTruncated,
  profileMapRows,
  profileRefLabel,
  splitGroups,
  type ConversionPlanFinding,
  type ConversionPlanResponse,
  type SplitGroup,
} from '../../shared/conversion-plan.js';
import { FLAT_BROKERS_FLAG, isFeatureEnabled } from '../../utils/feature-flags.js';
import { apiFetch } from '../../client/api.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';

/** The text shown for a split group whose instances differ only in unpublished settings. */
export const NOT_PUBLISHED_TEXT = `These profiles differ in a setting that is not published to the Hub; run ${CLI_POINTER} on the host to see it.`;

type LoadState =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'loaded'; plan: ConversionPlanResponse }
  | { kind: 'not-reported' }
  | { kind: 'unavailable' }
  | { kind: 'not-legacy' }
  | { kind: 'error'; status: number };

@customElement('scion-broker-area-conversion-plan')
export class ScionBrokerAreaConversionPlan extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  broker: RuntimeBroker | null = null;

  @state() private load: LoadState = { kind: 'idle' };

  /** The broker ID the plan was requested for. */
  private requestedFor = '';

  static override styles = [
    brokerAreaStyles,
    css`
      .plan-section h3 {
        font-size: 0.875rem;
        font-weight: 600;
        margin: 1rem 0 0.5rem;
      }
      .plan-table {
        border-collapse: collapse;
        font-size: 0.875rem;
        width: 100%;
      }
      .plan-table th,
      .plan-table td {
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        padding: 0.25rem 0.5rem;
        text-align: left;
        vertical-align: top;
      }
      .plan-table code,
      .plan-section code {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
      }
      .severity {
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
      }
      .severity.blocking {
        color: var(--sl-color-danger-600, #dc2626);
      }
      .plan-note {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.8125rem;
        margin: 0.5rem 0;
      }
      .plan-instance {
        margin-bottom: 0.75rem;
      }
      .split-group {
        margin: 0.25rem 0 0.5rem;
      }
    `,
  ];

  /** Whether the section is shown: the experiment is on and the broker is legacy. */
  private get active(): boolean {
    return !!this.broker && isFeatureEnabled(FLAT_BROKERS_FLAG) && !isFlatBroker(this.broker);
  }

  override updated(): void {
    if (!this.active || !this.broker || this.requestedFor === this.broker.id) return;
    this.requestedFor = this.broker.id;
    void this.fetchPlan(this.broker.id);
  }

  private async fetchPlan(id: string): Promise<void> {
    this.load = { kind: 'loading' };
    try {
      const res = await apiFetch(
        `/api/v1/runtime-brokers/${encodeURIComponent(id)}/conversion-plan`,
        {
          suppressAccessDeniedToast: true,
        }
      );
      if (this.requestedFor !== id) return;
      if (res.ok) {
        const plan = (await res.json()) as ConversionPlanResponse;
        // The broker may have changed while the body was read.
        if (this.requestedFor !== id) return;
        this.load = { kind: 'loaded', plan };
        return;
      }
      const code = await errorCode(res);
      if (this.requestedFor !== id) return;
      if (res.status === 404 && code === CONVERSION_PLAN_NOT_REPORTED) {
        this.load = { kind: 'not-reported' };
      } else if (res.status === 409 && code === RUNTIME_BROKER_NOT_LEGACY) {
        this.load = { kind: 'not-legacy' };
      } else if (res.status === 404) {
        this.load = { kind: 'unavailable' };
      } else {
        this.load = { kind: 'error', status: res.status };
      }
    } catch {
      if (this.requestedFor === id) this.load = { kind: 'error', status: 0 };
    }
  }

  private renderFinding(
    f: ConversionPlanFinding,
    groups: Map<ConversionPlanFinding, SplitGroup>
  ): TemplateResult {
    const group = groups.get(f);
    return html`
      <tr class="finding">
        <td>
          <span class="severity ${f.severity === 'blocking' ? 'blocking' : ''}">${f.severity}</span>
        </td>
        <td><code>${f.code}</code></td>
        <td><code class="finding-path">${f.path}</code></td>
        <td>${(f.profiles ?? []).join(', ')}</td>
        <td>
          <span class="finding-text">${findingText(f.code)}</span>
          ${group ? this.renderSplitGroup(group) : nothing}
        </td>
      </tr>
    `;
  }

  private renderSplitGroup(group: SplitGroup): TemplateResult {
    if (group.differences.length === 0) {
      return html`<p class="plan-note split-not-published">${NOT_PUBLISHED_TEXT}</p>`;
    }
    return html`
      <table class="plan-table split-group">
        <thead>
          <tr>
            <th>Published field</th>
            ${group.instances.map((k) => html`<th>${k}</th>`)}
          </tr>
        </thead>
        <tbody>
          ${group.differences.map(
            (d) => html`
              <tr class="split-difference">
                <td>${d.label}</td>
                ${d.values.map((v) => html`<td>${v.value}</td>`)}
              </tr>
            `
          )}
        </tbody>
      </table>
    `;
  }

  private renderPlan(r: ConversionPlanResponse): TemplateResult {
    const plan = r.plan ?? { schemaVersion: 0 };
    const instances = plan.instances ?? [];
    const findings = plan.findings ?? [];
    const mapRows = profileMapRows(plan);
    const groups = new Map<ConversionPlanFinding, SplitGroup>(
      splitGroups(plan).map((g) => [g.finding, g])
    );
    const agents = r.agents?.items ?? [];
    const hiddenAgents = r.agents?.hiddenCount ?? 0;
    const terminalAgents = r.agents?.terminalCount ?? 0;
    const projects = r.projects?.items ?? [];
    const hiddenProjects = r.projects?.hiddenCount ?? 0;
    const capacity = r.capacity;
    const computed = plan.computedAt ? formatInstantWithZone(plan.computedAt) : '';
    return html`
      ${isTruncated(plan)
        ? html`<sl-alert variant="warning" open class="plan-truncated">
            <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
            The Runtime Broker had to shorten this plan to fit the reporting limits, so it may be
            incomplete. Run <code>${CLI_POINTER}</code> on the host for the full plan.
          </sl-alert>`
        : nothing}
      ${computed
        ? html`<p class="plan-note">Computed by the Runtime Broker at ${computed}.</p>`
        : nothing}

      <h3>Proposed instances</h3>
      ${instances.length === 0
        ? html`<p class="plan-note">No instances proposed.</p>`
        : instances.map(
            (inst) => html`
              <div class="plan-instance">
                <strong>${inst.key}</strong>${inst.name && inst.name !== inst.key
                  ? html` (${inst.name})`
                  : nothing}
                <dl class="runtime-target-rows">
                  ${instanceRows(inst).map(
                    (row) =>
                      html`<dt>${row.label}</dt>
                        <dd>${row.value}</dd>`
                  )}
                </dl>
              </div>
            `
          )}
      ${mapRows.length > 0
        ? html`
            <h3>Profile map</h3>
            <table class="plan-table profile-map">
              <thead>
                <tr>
                  <th>Profile</th>
                  <th>Proposed instance</th>
                </tr>
              </thead>
              <tbody>
                ${mapRows.map(
                  (row) => html`
                    <tr>
                      <td>${row.profile}</td>
                      <td>
                        ${row.instance
                          ? row.instance
                          : html`no mapping (<code>${row.noMappingCode}</code>)`}
                      </td>
                    </tr>
                  `
                )}
              </tbody>
            </table>
          `
        : nothing}

      <h3>Findings</h3>
      ${findings.length === 0
        ? html`<p class="plan-note">No findings.</p>`
        : html`
            <table class="plan-table findings">
              <thead>
                <tr>
                  <th>Severity</th>
                  <th>Code</th>
                  <th>Settings key</th>
                  <th>Profiles</th>
                  <th>Explanation</th>
                </tr>
              </thead>
              <tbody>
                ${findings.map((f) => this.renderFinding(f, groups))}
              </tbody>
            </table>
          `}
      ${capacity
        ? html`
            <h3>Capacity</h3>
            <div class="capacity">
              <p>
                ${capacityOutcomeLabel(capacity.outcome)}${capacity.maxAgents !== undefined &&
                capacity.maxAgents !== null
                  ? html` (legacy maxAgents ${capacity.maxAgents})`
                  : nothing}
              </p>
              ${(capacity.allocations ?? []).length > 0
                ? html`<table class="plan-table">
                    <thead>
                      <tr>
                        <th>Proposed instance</th>
                        <th>maxAgents</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${capacity.allocations!.map(
                        (a) =>
                          html`<tr>
                            <td>${a.instance}</td>
                            <td>${a.maxAgents}</td>
                          </tr>`
                      )}
                    </tbody>
                  </table>`
                : nothing}
              ${[...(capacity.reasons ?? []), ...(capacity.flags ?? [])].map(
                (s) => html`<p class="plan-note capacity-note">${s}</p>`
              )}
            </div>
          `
        : nothing}

      <h3>Still bound on the Hub</h3>
      ${agents.length > 0
        ? html`
            <table class="plan-table bound-agents">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Project</th>
                  <th>Phase</th>
                  <th>Profile</th>
                  <th>Proposed instance</th>
                </tr>
              </thead>
              <tbody>
                ${agents.map(
                  (a) => html`
                    <tr>
                      <td><a href="/agents/${encodeURIComponent(a.id)}">${a.name || a.id}</a></td>
                      <td>${a.projectId ?? ''}</td>
                      <td>${a.phase ?? ''}</td>
                      <td>${a.profile ?? ''}</td>
                      <td>${profileRefLabel(a)}</td>
                    </tr>
                  `
                )}
              </tbody>
            </table>
          `
        : html`<p class="plan-note">No agents you can read are bound to this Runtime Broker.</p>`}
      ${hiddenAgents > 0
        ? html`<p class="plan-note hidden-agents">${hiddenAgents} agents not visible to you.</p>`
        : nothing}
      ${terminalAgents > 0
        ? html`<p class="plan-note">${terminalAgents} finished agents are not listed.</p>`
        : nothing}
      <p class="plan-note">
        Agents cannot move onto a flat Runtime Broker today; finish or recreate them before the host
        switches.
      </p>
      ${projects.length > 0
        ? html`
            <table class="plan-table providing-projects">
              <thead>
                <tr>
                  <th>Project</th>
                  <th>Active profile</th>
                  <th>Per-profile service accounts</th>
                </tr>
              </thead>
              <tbody>
                ${projects.map(
                  (p) => html`
                    <tr>
                      <td>${p.name || p.id}</td>
                      <td>
                        ${p.activeProfile
                          ? `${p.activeProfile.profile ?? ''} → ${profileRefLabel(p.activeProfile)}`
                          : ''}
                      </td>
                      <td>
                        ${(p.serviceAccountProfiles ?? [])
                          .map((ref) => `${ref.profile ?? ''} → ${profileRefLabel(ref)}`)
                          .join(', ')}
                      </td>
                    </tr>
                  `
                )}
              </tbody>
            </table>
          `
        : nothing}
      ${hiddenProjects > 0
        ? html`<p class="plan-note hidden-projects">
            ${hiddenProjects} projects not visible to you.
          </p>`
        : nothing}
    `;
  }

  private renderBody(): TemplateResult | typeof nothing {
    switch (this.load.kind) {
      case 'idle':
      case 'loading':
        return html`<sl-spinner label="Loading the plan"></sl-spinner>`;
      case 'not-reported':
        return html`<p class="plan-note not-reported">
          This Runtime Broker has not reported a plan. Upgrade the Runtime Broker, or run
          <code>${CLI_POINTER}</code> on the host.
        </p>`;
      case 'unavailable':
        return html`<p class="plan-note unavailable">The plan is not available.</p>`;
      case 'error':
        return html`<p class="plan-note plan-error">
          Could not load the plan${this.load.status ? ` (HTTP ${this.load.status})` : ''}.
        </p>`;
      case 'loaded':
        return this.renderPlan(this.load.plan);
      default:
        return nothing;
    }
  }

  override render(): TemplateResult | typeof nothing {
    if (!this.active || this.load.kind === 'not-legacy') return nothing;
    return html`
      <div class="section plan-section">
        <div class="section-header">
          <sl-icon name="arrow-left-right"></sl-icon>
          <h2>Plan a move to flat Runtime Brokers</h2>
        </div>
        <p class="plan-note">
          A read-only preview from the plan the Runtime Broker reports. For the full settings block,
          run <code>${CLI_POINTER}</code> on the host.
        </p>
        ${this.renderBody()}
      </div>
    `;
  }
}

/** The error code of an API error response, or "" when there is none. */
async function errorCode(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: { code?: unknown } };
    return typeof body?.error?.code === 'string' ? body.error.code : '';
  } catch {
    return '';
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-area-conversion-plan': ScionBrokerAreaConversionPlan;
  }
}
