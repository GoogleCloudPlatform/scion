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
 * Per-account status sections (ptone/scion#4018): verification, mapping per
 * Kubernetes broker profile, Workload Identity binding, the defaults that
 * point at the account, the agents using it, and the next step.
 *
 * Presentational only: the caller fetches the project-relative status view
 * (saStatusUrl) and passes it in, so the page that also needs the account's
 * identity from the same response does not fetch it twice.
 *
 * Every value is rendered as the hub returned it. In particular the binding
 * is shown as the hub states it (only "unknown" today) and never upgraded,
 * and the agents list is already limited to agents the caller may see.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type {
  GCPServiceAccountProfileMapping,
  GCPServiceAccountStatus,
} from '../../shared/types.js';
import { formatRelative } from '../../utils/time.js';

/** Human text for the broker's incomplete-report codes. */
const INCOMPLETE_REASON_TEXT: Record<string, string> = {
  list_failed: 'the broker could not list Kubernetes service accounts',
  unavailable: 'discovery could not run',
  pending: 'discovery has not finished yet',
};

const MAPPING_STATE_TEXT: Record<string, string> = {
  mapped: 'Mapped',
  not_mapped: 'Not mapped',
  unknown: 'Unknown',
  not_reported: 'Not reported',
};

const BINDING_STATE_TEXT: Record<string, string> = {
  bound: 'Bound',
  not_bound: 'Not bound',
  unknown: 'Unknown',
};

/** incompleteReasonText renders a broker incomplete-report code. */
export function incompleteReasonText(reason: string | undefined): string {
  if (!reason) return 'unknown reason';
  return INCOMPLETE_REASON_TEXT[reason] ?? reason.replace(/_/g, ' ');
}

/**
 * unknownReasonText says why an unmapped account's state is unknown rather
 * than "not mapped": the report is not authoritative.
 */
export function unknownReasonText(m: GCPServiceAccountProfileMapping): string {
  switch (m.unknownReason) {
    case 'report_stale': {
      const age = ago(m.reportedAt);
      return age
        ? `the broker's report is stale (reported ${age})`
        : "the broker's report is stale";
    }
    case 'report_incomplete':
      return `the broker's report is incomplete: ${incompleteReasonText(m.incompleteReason)}`;
    case 'report_old_version':
      return "the broker's report is too old a version to tell";
    case 'report_missing':
      return 'there is no stored report from the broker';
    default:
      return m.unknownReason ? m.unknownReason.replace(/_/g, ' ') : 'the report is not conclusive';
  }
}

/** ago renders an ISO instant as a relative age; future instants are skew. */
function ago(iso: string | undefined): string {
  if (!iso) return '';
  const ms = new Date(iso).getTime();
  if (Number.isNaN(ms)) return '';
  if (ms > Date.now()) return 'just now';
  return formatRelative(iso);
}

@customElement('scion-gcp-service-account-status')
export class ScionGCPServiceAccountStatus extends LitElement {
  /** The status view to render. Nothing renders without one. */
  @property({ attribute: false }) status: GCPServiceAccountStatus | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem 1.5rem;
      margin-top: 1rem;
      font-size: 0.875rem;
    }

    h3 {
      margin: 0 0 0.75rem;
      font-size: 0.9375rem;
      font-weight: 600;
    }

    .muted {
      color: var(--scion-text-muted, #64748b);
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th,
    td {
      text-align: left;
      padding: 0.375rem 0.75rem 0.375rem 0;
      vertical-align: top;
    }

    th {
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
    }

    .note {
      display: flex;
      gap: 0.375rem;
      align-items: center;
      color: var(--sl-color-warning-700, #b45309);
      margin-top: 0.25rem;
    }

    ul {
      margin: 0;
      padding-left: 1.25rem;
    }

    .next-step {
      display: flex;
      gap: 0.5rem;
      align-items: flex-start;
    }
  `;

  override render() {
    const st = this.status;
    if (!st) return nothing;
    return html`
      ${this.renderVerification(st)} ${this.renderMappings(st)} ${this.renderBinding(st)}
      ${this.renderDefaults(st)} ${this.renderAgents(st)} ${this.renderNextStep(st)}
    `;
  }

  private renderVerification(st: GCPServiceAccountStatus) {
    const v = st.verification;
    const checked = ago(v.verifiedAt);
    return html`
      <section data-section="verification">
        <h3>Verification</h3>
        ${v.verified
          ? html`<sl-badge variant="success">Verified</sl-badge>`
          : v.status === 'failed'
            ? html`<sl-badge variant="danger">Failed</sl-badge>`
            : html`<sl-badge variant="warning">Unverified</sl-badge>`}
        ${checked ? html`<span class="muted">checked ${checked}</span>` : nothing}
        ${v.error ? html`<div class="muted">Last error: ${v.error}</div>` : nothing}
      </section>
    `;
  }

  private renderMappings(st: GCPServiceAccountStatus) {
    return html`
      <section data-section="mappings">
        <h3>Mapping per Kubernetes broker profile</h3>
        <p class="muted">
          Mappings are owned by each broker and shown read-only. A broker operator changes them in
          kubernetes_service_account_mappings.
        </p>
        ${st.mappings.length === 0
          ? html`<div class="muted">No Kubernetes broker profiles in this project.</div>`
          : html`
              <table>
                <thead>
                  <tr>
                    <th>Profile</th>
                    <th>State</th>
                    <th>Kubernetes service account</th>
                    <th>Namespace</th>
                    <th>Source</th>
                    <th>Reported</th>
                  </tr>
                </thead>
                <tbody>
                  ${st.mappings.map((m) => this.renderMappingRow(m))}
                </tbody>
              </table>
            `}
      </section>
    `;
  }

  private renderMappingRow(m: GCPServiceAccountProfileMapping) {
    const variant =
      m.state === 'mapped' ? 'success' : m.state === 'not_mapped' ? 'warning' : 'neutral';
    // An unknown state carries its own reason, which covers an incomplete
    // report; the separate incomplete note is for the other states.
    const unknown = m.state === 'unknown';
    const reported = ago(m.reportedAt);
    return html`
      <tr data-profile=${m.profile}>
        <td>
          ${m.brokerName}/${m.profile}
          ${m.ambiguous
            ? html`<div class="note" data-note="ambiguous">
                <sl-icon name="exclamation-triangle"></sl-icon>
                Ambiguous: more than one Kubernetes service account is annotated with this account,
                so the broker refuses it.
              </div>`
            : nothing}
          ${unknown
            ? html`<div class="note" data-note="unknown">
                <sl-icon name="exclamation-triangle"></sl-icon>
                Unknown: ${unknownReasonText(m)}.
              </div>`
            : nothing}
          ${m.incomplete && !unknown
            ? html`<div class="note" data-note="incomplete">
                <sl-icon name="exclamation-triangle"></sl-icon>
                Report incomplete: ${incompleteReasonText(m.incompleteReason)}.
              </div>`
            : nothing}
        </td>
        <td><sl-badge variant=${variant}>${MAPPING_STATE_TEXT[m.state] ?? m.state}</sl-badge></td>
        <td>${m.kubernetesServiceAccount || '—'}</td>
        <td>${m.namespace || '—'}</td>
        <td>${m.source || '—'}</td>
        <td>${reported || '—'}</td>
      </tr>
    `;
  }

  private renderBinding(st: GCPServiceAccountStatus) {
    const b = st.workloadIdentityBinding;
    return html`
      <section data-section="binding">
        <h3>Workload Identity binding</h3>
        ${BINDING_STATE_TEXT[b.state] ?? b.state}
        ${b.reason ? html`<span class="muted">(${b.reason})</span>` : nothing}
      </section>
    `;
  }

  private renderDefaults(st: GCPServiceAccountStatus) {
    return html`
      <section data-section="defaults">
        <h3>Default for</h3>
        ${st.defaultFor.length === 0
          ? html`<div class="muted">None</div>`
          : html`<ul>
              ${st.defaultFor.map(
                (d) =>
                  html`<li>
                    ${d.kind === 'profile'
                      ? `Profile ${d.profile ?? ''}`
                      : d.kind === 'project'
                        ? 'Project default'
                        : d.kind === 'hub'
                          ? 'Hub default'
                          : d.kind}
                  </li>`
              )}
            </ul>`}
      </section>
    `;
  }

  private renderAgents(st: GCPServiceAccountStatus) {
    const { count, names } = st.agents;
    const more = count - names.length;
    return html`
      <section data-section="agents">
        <h3>Agents using it (${count})</h3>
        ${count === 0
          ? html`<div class="muted">None</div>`
          : html`<ul>
              ${names.map((n) => html`<li>${n}</li>`)}
              ${more > 0 ? html`<li class="muted">and ${more} more</li>` : nothing}
            </ul>`}
      </section>
    `;
  }

  private renderNextStep(st: GCPServiceAccountStatus) {
    const done = st.nextStep.code === 'none';
    return html`
      <section data-section="next-step">
        <h3>Next step</h3>
        <div class="next-step">
          <sl-icon name=${done ? 'check-circle' : 'info-circle'}></sl-icon>
          <span>${st.nextStep.message}</span>
        </div>
      </section>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-gcp-service-account-status': ScionGCPServiceAccountStatus;
  }
}
