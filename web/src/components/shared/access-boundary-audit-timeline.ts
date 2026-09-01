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
 * Access Boundary Audit Timeline
 *
 * Displays a pageable timeline of audit events for an access boundary.
 * Each event shows: type, actor, time, classification, revision changes,
 * preview/audit IDs, and counts.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';

import type {
  AccessBoundaryAuditEvent,
  PageToken,
  MutationClassification,
} from '../../shared/access-boundaries.js';

/** Event detail for requesting a new page of audit events. */
export interface AuditPageRequestDetail {
  pageToken: PageToken;
}

@customElement('scion-access-boundary-audit-timeline')
export class ScionAccessBoundaryAuditTimeline extends LitElement {
  @property({ type: Array }) events: AccessBoundaryAuditEvent[] = [];
  @property() nextPageToken: PageToken | undefined;
  @property({ type: Number }) totalCount = 0;
  @property({ type: Boolean }) loading = false;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .timeline {
        position: relative;
        padding-left: 1.25rem;
      }

      .timeline::before {
        content: '';
        position: absolute;
        left: 0.375rem;
        top: 0;
        bottom: 0;
        width: 2px;
        background: var(--scion-border, #e2e8f0);
      }

      .timeline-event {
        position: relative;
        padding: 0.75rem 0 0.75rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .timeline-event:last-child {
        border-bottom: none;
      }

      .timeline-dot {
        position: absolute;
        left: -1.25rem;
        top: 1rem;
        width: 0.75rem;
        height: 0.75rem;
        border-radius: 50%;
        border: 2px solid var(--scion-surface, #ffffff);
        z-index: 1;
      }

      .timeline-dot.created {
        background: var(--sl-color-success-500, #22c55e);
      }

      .timeline-dot.updated {
        background: var(--sl-color-primary-500, #3b82f6);
      }

      .timeline-dot.deleted {
        background: var(--sl-color-danger-500, #ef4444);
      }

      .timeline-dot.rejected {
        background: var(--sl-color-warning-500, #f59e0b);
      }

      .timeline-dot.recovery_disabled {
        background: var(--sl-color-danger-700, #b91c1c);
      }

      .event-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        flex-wrap: wrap;
        margin-bottom: 0.25rem;
      }

      .event-type {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .event-classification {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
      }

      .event-classification.tighten {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .event-classification.relax {
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .event-classification.mixed {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
        border: 1px solid var(--sl-color-warning-200, #fde68a);
      }

      .event-outcome-rejected {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .event-time {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .event-body {
        font-size: 0.8125rem;
        color: var(--scion-text, #1e293b);
      }

      .event-actor {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-bottom: 0.25rem;
      }

      .event-meta {
        display: flex;
        flex-wrap: wrap;
        gap: 0.75rem;
        margin-top: 0.375rem;
      }

      .meta-item {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .meta-label {
        font-weight: 600;
      }

      .meta-value {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.625rem;
      }

      .event-counts {
        display: flex;
        gap: 0.75rem;
        margin-top: 0.25rem;
        font-size: 0.75rem;
      }

      .count-loses {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .count-regains {
        color: var(--sl-color-success-600, #16a34a);
      }

      .count-affected {
        color: var(--scion-text-muted, #64748b);
      }

      .revision-change {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .rejection-info {
        margin-top: 0.25rem;
        padding: 0.375rem 0.5rem;
        background: var(--sl-color-danger-50, #fef2f2);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.75rem;
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .pagination {
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 1rem;
        padding: 0.75rem 0;
      }

      .empty-state {
        text-align: center;
        padding: 1.5rem;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      .loading-overlay {
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 2rem;
      }

      @media (max-width: 768px) {
        .timeline {
          padding-left: 0.75rem;
        }

        .timeline-event {
          padding: 0.5rem 0 0.5rem 0.75rem;
        }

        .event-header {
          flex-direction: column;
          align-items: flex-start;
          gap: 0.25rem;
        }

        .event-meta {
          flex-direction: column;
          gap: 0.25rem;
        }

        .event-counts {
          flex-wrap: wrap;
        }

        .meta-value {
          overflow-wrap: anywhere;
        }
      }

      @media (forced-colors: active) {
        .timeline::before {
          background: ButtonText;
        }

        .timeline-dot {
          border-color: Canvas;
          forced-color-adjust: none;
        }

        .timeline-event {
          border-bottom-color: ButtonText;
        }

        .event-classification,
        .event-outcome-rejected {
          border: 1px solid ButtonText;
        }

        .rejection-info {
          border: 1px solid ButtonText;
        }
      }
    `,
  ];

  private eventTypeLabel(eventType: string): string {
    switch (eventType) {
      case 'boundary.created':
        return 'Created';
      case 'boundary.updated':
        return 'Updated';
      case 'boundary.deleted':
        return 'Deleted';
      case 'boundary.commit_rejected':
        return 'Commit rejected';
      case 'boundary.recovery_disabled':
        return 'Recovery disabled';
      default:
        return eventType;
    }
  }

  private eventDotClass(eventType: string): string {
    switch (eventType) {
      case 'boundary.created':
        return 'created';
      case 'boundary.updated':
        return 'updated';
      case 'boundary.deleted':
        return 'deleted';
      case 'boundary.commit_rejected':
        return 'rejected';
      case 'boundary.recovery_disabled':
        return 'recovery_disabled';
      default:
        return 'updated';
    }
  }

  private classificationLabel(c: MutationClassification): string {
    switch (c) {
      case 'tighten':
        return 'Tightening';
      case 'relax':
        return 'Relaxation';
      case 'mixed':
        return 'Mixed';
      default:
        return c;
    }
  }

  private formatDatetime(iso: string): string {
    try {
      const date = new Date(iso);
      if (isNaN(date.getTime())) return iso;
      return date.toLocaleString(undefined, {
        dateStyle: 'medium',
        timeStyle: 'short',
      });
    } catch {
      return iso;
    }
  }

  private actorDisplay(actor: AccessBoundaryAuditEvent['actor']): string {
    if (!actor.principal) return '(unknown actor)';
    const name = actor.principal.displayName ?? actor.principal.id;
    const type = actor.principal.type === 'agent' ? 'Agent' : 'User';
    return `${type}: ${name}`;
  }

  private requestPage(token: PageToken): void {
    this.dispatchEvent(
      new CustomEvent<AuditPageRequestDetail>('audit-page-request', {
        detail: { pageToken: token },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderEvent(event: AccessBoundaryAuditEvent) {
    return html`
      <div class="timeline-event" role="listitem">
        <div class="timeline-dot ${this.eventDotClass(event.eventType)}"></div>

        <div class="event-header">
          <span class="event-type">${this.eventTypeLabel(event.eventType)}</span>
          ${event.classification
            ? html`
                <span class="event-classification ${event.classification}">
                  ${this.classificationLabel(event.classification)}
                </span>
              `
            : nothing}
          ${event.outcome === 'rejected'
            ? html`<span class="event-outcome-rejected">Rejected</span>`
            : nothing}
          <time class="event-time" datetime="${event.occurredAt}"
            >${this.formatDatetime(event.occurredAt)}</time
          >
        </div>

        <div class="event-body">
          <div class="event-actor">
            ${this.actorDisplay(event.actor)}
            ${event.actor.credentialType ? ` (${event.actor.credentialType})` : ''}
          </div>

          ${event.changeSummary
            ? html`
                <div class="event-counts">
                  <span class="count-affected">
                    ${event.changeSummary.affectedPrincipalCount} affected
                  </span>
                  ${event.changeSummary.losingPrincipalCount > 0
                    ? html`<span class="count-loses">
                        − ${event.changeSummary.losingPrincipalCount} losing
                      </span>`
                    : nothing}
                  ${event.changeSummary.regainingPrincipalCount > 0
                    ? html`<span class="count-regains">
                        + ${event.changeSummary.regainingPrincipalCount} regaining
                      </span>`
                    : nothing}
                </div>
              `
            : nothing}
          ${event.outcome === 'rejected' && event.rejectionCode
            ? html`
                <div class="rejection-info">
                  ${event.rejectionCode}${event.reason ? `: ${event.reason}` : ''}
                </div>
              `
            : nothing}

          <div class="event-meta">
            ${event.revisionBefore !== null || event.revisionAfter !== null
              ? html`
                  <span class="meta-item">
                    <span class="meta-label">Revision:</span>
                    <span class="revision-change">
                      ${event.revisionBefore ?? '—'} → ${event.revisionAfter ?? '—'}
                    </span>
                  </span>
                `
              : nothing}
            ${event.previewId
              ? html`
                  <span class="meta-item">
                    <span class="meta-label">Preview:</span>
                    <span class="meta-value">${event.previewId}</span>
                  </span>
                `
              : nothing}
            <span class="meta-item">
              <span class="meta-label">Audit:</span>
              <span class="meta-value">${event.id}</span>
            </span>
            <span class="meta-item">
              <span class="meta-label">Correlation:</span>
              <span class="meta-value">${event.correlationId}</span>
            </span>
          </div>
        </div>
      </div>
    `;
  }

  override render() {
    if (this.loading && this.events.length === 0) {
      return html`
        <div class="loading-overlay" role="status" aria-live="polite">
          <sl-spinner></sl-spinner>
          <span class="sr-only">Loading audit events</span>
        </div>
      `;
    }

    if (this.events.length === 0) {
      return html`<div class="empty-state">No audit events</div>`;
    }

    return html`
      <div class="timeline" role="list" aria-label="Access boundary audit timeline">
        ${this.events.map((event) => this.renderEvent(event))}
      </div>

      ${this.nextPageToken
        ? html`
            <div class="pagination">
              <sl-button
                variant="default"
                size="small"
                ?loading=${this.loading}
                @click=${() => this.requestPage(this.nextPageToken!)}
              >
                Load older events
              </sl-button>
            </div>
          `
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-audit-timeline': ScionAccessBoundaryAuditTimeline;
  }
}
