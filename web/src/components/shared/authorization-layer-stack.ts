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
 * Authorization Layer Stack
 *
 * Renders the ordered effective-access composition as a layered visualization:
 *
 *   Potential: role grants -- N permissions
 *                          |
 *   Access boundaries      +- Boundary A removes X
 *                          +- Boundary B removes Y (Z overlap)
 *                          |
 *   Intrinsic restrictions +- Credential scope removes X
 *                          +- Delegation ceiling removes Y
 *                          v
 *   Effective: M permissions
 *
 * Each layer is expandable. Boundary layers link to boundary detail pages.
 * Restriction layers show credential/status/delegation info.
 *
 * TERMINOLOGY: layers are descriptive, not an override order. Overlapping
 * removal is "removed by both," not "won by." Never use "priority",
 * "override", or "winner."
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import type { PermissionId, RedactionNotice } from '../../shared/access-boundaries.js';

// ---------------------------------------------------------------------------
// Types — local to this component
// ---------------------------------------------------------------------------

/** A boundary that participates in reducing effective permissions. */
export interface BoundaryLayer {
  id: string;
  /** `null` when redacted — user lacks permission to see boundary details. */
  name: string | null;
  /** Known values: 'active', 'scheduled', 'expired'. */
  status: string;
  removedCount: number;
  overlapCount: number;
  /** Membership paths describing how the principal is in scope. */
  membershipSummary?: string;
  redacted?: RedactionNotice;
}

/** An intrinsic restriction that reduces effective permissions. */
export interface IntrinsicRestriction {
  /** Known values: 'credential_scope', 'principal_status', 'delegation_ceiling'. */
  kind: string;
  label: string;
  removedCount: number;
  detail?: string;
}

/** Per-permission denial reason — why a specific permission is not effective. */
export type PermissionDenialReason =
  | { type: 'never_granted' }
  | { type: 'inactive_grant'; grantStatus: string }
  | { type: 'removed_by_boundaries'; boundaryNames: (string | null)[] }
  | { type: 'removed_by_restriction'; restrictionLabel: string }
  | { type: 'evaluation_failed'; correlationId: string };

/** Summary of a denied permission with its reasons. */
export interface DeniedPermission {
  permissionId: PermissionId;
  reasons: PermissionDenialReason[];
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-authorization-layer-stack')
export class ScionAuthorizationLayerStack extends LitElement {
  /** Total permissions from active role grants (potential). */
  @property({ type: Number }) potentialCount = 0;

  /** Boundaries that reduce permissions. */
  @property({ type: Array }) boundaries: BoundaryLayer[] = [];

  /** Intrinsic restrictions that reduce permissions. */
  @property({ type: Array }) restrictions: IntrinsicRestriction[] = [];

  /** Final effective permission count. */
  @property({ type: Number }) effectiveCount = 0;

  /** Denied permissions with reasons. */
  @property({ type: Array }) deniedPermissions: DeniedPermission[] = [];

  /** Which layers are expanded. */
  @state() private expandedLayers: Set<string> = new Set();

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .layer-stack {
        position: relative;
        padding-left: 1.5rem;
      }

      /* Vertical connector line */
      .layer-stack::before {
        content: '';
        position: absolute;
        left: 0.6875rem;
        top: 1.5rem;
        bottom: 1.5rem;
        width: 2px;
        background: var(--scion-border, #e2e8f0);
      }

      .layer {
        position: relative;
        margin-bottom: 0.5rem;
      }

      /* Dot on the vertical line */
      .layer::before {
        content: '';
        position: absolute;
        left: -0.9375rem;
        top: 0.875rem;
        width: 8px;
        height: 8px;
        border-radius: 50%;
        background: var(--scion-border, #e2e8f0);
        border: 2px solid var(--scion-surface, #ffffff);
      }

      .layer.potential::before {
        background: var(--sl-color-primary-500, #3b82f6);
      }

      .layer.boundaries::before {
        background: var(--sl-color-warning-500, #f59e0b);
      }

      .layer.restrictions::before {
        background: var(--sl-color-neutral-500, #6b7280);
      }

      .layer.effective::before {
        background: var(--sl-color-success-500, #22c55e);
      }

      .layer-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        min-height: 44px;
        background: var(--scion-bg-subtle, #f8fafc);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        cursor: pointer;
        user-select: none;
        transition: background 0.15s ease;
      }

      .layer-header:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .layer-header.not-expandable {
        cursor: default;
      }

      .layer-label {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        flex: 1;
      }

      .layer-count {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        white-space: nowrap;
      }

      .layer-count.potential {
        color: var(--sl-color-primary-600, #2563eb);
      }

      .layer-count.effective {
        color: var(--sl-color-success-600, #16a34a);
      }

      .layer-count.removes {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .expand-icon {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        transition: transform 0.2s ease;
      }

      .expand-icon.open {
        transform: rotate(90deg);
      }

      /* Expanded detail area */
      .layer-detail {
        margin-top: 0.25rem;
        margin-left: 0.75rem;
        padding: 0.5rem 0.75rem;
        border-left: 2px solid var(--scion-border, #e2e8f0);
        font-size: 0.8125rem;
      }

      .boundary-row,
      .restriction-row {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.375rem 0;
        color: var(--scion-text, #1e293b);
      }

      .boundary-row + .boundary-row,
      .restriction-row + .restriction-row {
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .boundary-name {
        flex: 1;
        font-weight: 500;
        overflow-wrap: anywhere;
      }

      .boundary-link {
        color: var(--sl-color-primary-600, #2563eb);
        text-decoration: none;
        cursor: pointer;
      }

      .boundary-link:hover {
        text-decoration: underline;
      }

      .boundary-redacted {
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
      }

      .removal-count {
        font-size: 0.75rem;
        font-weight: 500;
        color: var(--sl-color-danger-600, #dc2626);
        white-space: nowrap;
      }

      .overlap-note {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .restriction-kind {
        flex: 1;
        font-weight: 500;
      }

      .status-dot {
        display: inline-block;
        width: 6px;
        height: 6px;
        border-radius: 50%;
        flex-shrink: 0;
      }

      .status-dot.active {
        background: var(--sl-color-success-500, #22c55e);
      }

      .status-dot.scheduled {
        background: var(--sl-color-warning-500, #f59e0b);
      }

      .status-dot.expired {
        background: var(--sl-color-danger-500, #ef4444);
      }

      /* Denied permissions detail */
      .denied-list {
        margin-top: 0.5rem;
      }

      .denied-item {
        display: flex;
        align-items: flex-start;
        gap: 0.5rem;
        padding: 0.25rem 0;
        font-size: 0.75rem;
        color: var(--scion-text, #1e293b);
      }

      .denied-item + .denied-item {
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .denied-permission-id {
        font-family: var(--sl-font-mono, monospace);
        font-weight: 500;
        flex: 1;
        overflow-wrap: anywhere;
      }

      .denied-reason {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        text-align: right;
        max-width: 60%;
      }

      .denied-reason .reason-tag {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.625rem;
        font-weight: 500;
      }

      .reason-tag.never-granted {
        background: var(--sl-color-neutral-100, #f1f5f9);
        color: var(--sl-color-neutral-600, #475569);
      }

      .reason-tag.inactive {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .reason-tag.boundary {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .reason-tag.restriction {
        background: var(--sl-color-neutral-100, #f1f5f9);
        color: var(--sl-color-neutral-700, #334155);
      }

      .reason-tag.failed {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-800, #991b1b);
      }

      .empty-layer {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        padding: 0.25rem 0;
        font-style: italic;
      }

      .layer-label,
      .restriction-kind {
        overflow-wrap: anywhere;
      }

      .denied-permission-id {
        overflow-wrap: anywhere;
      }

      @media (max-width: 768px) {
        .layer-stack {
          padding-left: 1rem;
        }

        .layer-header {
          padding: 0.625rem 0.5rem;
          min-height: 44px;
          gap: 0.375rem;
        }

        .layer-detail {
          padding: 0.375rem 0.5rem;
        }

        .boundary-row,
        .restriction-row {
          flex-wrap: wrap;
          min-height: 44px;
        }

        .denied-item {
          flex-direction: column;
          gap: 0.25rem;
        }

        .denied-reason {
          text-align: left;
          max-width: 100%;
        }
      }

      @media (forced-colors: active) {
        .layer-stack::before {
          background: ButtonText;
        }

        .layer::before {
          border-color: Canvas;
          forced-color-adjust: none;
        }

        .layer-header {
          border-color: ButtonText;
        }

        .layer-detail {
          border-left-color: ButtonText;
        }

        .boundary-row + .boundary-row,
        .restriction-row + .restriction-row,
        .denied-item + .denied-item {
          border-top-color: ButtonText;
        }

        .status-dot {
          forced-color-adjust: none;
        }

        .reason-tag {
          border: 1px solid ButtonText;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .expand-icon {
          transition: none;
        }
        .layer-header {
          transition: none;
        }
      }
    `,
  ];

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  private toggleLayer(layerId: string): void {
    const next = new Set(this.expandedLayers);
    if (next.has(layerId)) {
      next.delete(layerId);
    } else {
      next.add(layerId);
    }
    this.expandedLayers = next;
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="layer-stack">
        ${this.renderPotentialLayer()} ${this.renderBoundariesLayer()}
        ${this.renderRestrictionsLayer()} ${this.renderEffectiveLayer()}
      </div>
    `;
  }

  private renderPotentialLayer() {
    const expanded = this.expandedLayers.has('potential');
    return html`
      <div class="layer potential">
        <div
          class="layer-header"
          @click=${() => this.toggleLayer('potential')}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.toggleLayer('potential');
            }
          }}
          tabindex="0"
          role="button"
          aria-expanded=${expanded}
        >
          <sl-icon name="chevron-right" class="expand-icon ${expanded ? 'open' : ''}"></sl-icon>
          <span class="layer-label">Potential permissions</span>
          <span class="layer-count potential">${this.potentialCount} permissions</span>
        </div>
        ${expanded ? this.renderPotentialDetail() : nothing}
      </div>
    `;
  }

  private renderPotentialDetail() {
    return html`
      <div class="layer-detail">
        <div class="empty-layer">Union of permissions from active role grants.</div>
      </div>
    `;
  }

  private renderBoundariesLayer() {
    if (this.boundaries.length === 0) return nothing;

    const expanded = this.expandedLayers.has('boundaries');
    const totalRemoved = this.boundaries.reduce((sum, b) => sum + b.removedCount, 0);

    return html`
      <div class="layer boundaries">
        <div
          class="layer-header"
          @click=${() => this.toggleLayer('boundaries')}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.toggleLayer('boundaries');
            }
          }}
          tabindex="0"
          role="button"
          aria-expanded=${expanded}
        >
          <sl-icon name="chevron-right" class="expand-icon ${expanded ? 'open' : ''}"></sl-icon>
          <span class="layer-label">Access boundaries</span>
          <span class="layer-count removes"> removes ${totalRemoved} </span>
        </div>
        ${expanded ? this.renderBoundariesDetail() : nothing}
      </div>
    `;
  }

  private renderBoundariesDetail() {
    return html`
      <div class="layer-detail">${this.boundaries.map((b) => this.renderBoundaryRow(b))}</div>
    `;
  }

  private renderBoundaryRow(boundary: BoundaryLayer) {
    return html`
      <div class="boundary-row">
        <span class="status-dot ${boundary.status}"></span>
        <span class="boundary-name">
          ${boundary.redacted || boundary.name === null
            ? html`<span class="boundary-redacted"> Access boundary (details unavailable) </span>`
            : html`<a
                class="boundary-link"
                href="/admin/access-boundaries/${encodeURIComponent(boundary.id)}"
                >${boundary.name}</a
              >`}
        </span>
        <span class="removal-count"> removes ${boundary.removedCount} </span>
        ${boundary.overlapCount > 0
          ? html`<span class="overlap-note"> (${boundary.overlapCount} removed by both) </span>`
          : nothing}
      </div>
    `;
  }

  private renderRestrictionsLayer() {
    if (this.restrictions.length === 0) return nothing;

    const expanded = this.expandedLayers.has('restrictions');
    const totalRemoved = this.restrictions.reduce((sum, r) => sum + r.removedCount, 0);

    return html`
      <div class="layer restrictions">
        <div
          class="layer-header"
          @click=${() => this.toggleLayer('restrictions')}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.toggleLayer('restrictions');
            }
          }}
          tabindex="0"
          role="button"
          aria-expanded=${expanded}
        >
          <sl-icon name="chevron-right" class="expand-icon ${expanded ? 'open' : ''}"></sl-icon>
          <span class="layer-label">Intrinsic restrictions</span>
          <span class="layer-count removes"> removes ${totalRemoved} </span>
        </div>
        ${expanded ? this.renderRestrictionsDetail() : nothing}
      </div>
    `;
  }

  private renderRestrictionsDetail() {
    return html`
      <div class="layer-detail">${this.restrictions.map((r) => this.renderRestrictionRow(r))}</div>
    `;
  }

  private renderRestrictionRow(restriction: IntrinsicRestriction) {
    return html`
      <div class="restriction-row">
        <span class="restriction-kind">${restriction.label}</span>
        <span class="removal-count">removes ${restriction.removedCount}</span>
        ${restriction.detail
          ? html`<span class="overlap-note">${restriction.detail}</span>`
          : nothing}
      </div>
    `;
  }

  private renderEffectiveLayer() {
    const expanded = this.expandedLayers.has('effective');
    const hasDenied = this.deniedPermissions.length > 0;

    return html`
      <div class="layer effective">
        <div
          class="layer-header ${hasDenied ? '' : 'not-expandable'}"
          @click=${() => {
            if (hasDenied) this.toggleLayer('effective');
          }}
          @keydown=${(e: KeyboardEvent) => {
            if (hasDenied && (e.key === 'Enter' || e.key === ' ')) {
              e.preventDefault();
              this.toggleLayer('effective');
            }
          }}
          tabindex=${hasDenied ? '0' : '-1'}
          role=${hasDenied ? 'button' : 'presentation'}
          aria-expanded=${hasDenied ? expanded : nothing}
        >
          ${hasDenied
            ? html`<sl-icon
                name="chevron-right"
                class="expand-icon ${expanded ? 'open' : ''}"
              ></sl-icon>`
            : nothing}
          <span class="layer-label">Effective permissions</span>
          <span class="layer-count effective">${this.effectiveCount} permissions</span>
        </div>
        ${expanded && hasDenied ? this.renderDeniedDetail() : nothing}
      </div>
    `;
  }

  private renderDeniedDetail() {
    return html`
      <div class="layer-detail">
        <div class="denied-list">
          ${this.deniedPermissions.map((dp) => this.renderDeniedItem(dp))}
        </div>
      </div>
    `;
  }

  private renderDeniedItem(dp: DeniedPermission) {
    return html`
      <div class="denied-item">
        <span class="denied-permission-id">${dp.permissionId}</span>
        <span class="denied-reason"> ${dp.reasons.map((r) => this.renderDenialReason(r))} </span>
      </div>
    `;
  }

  private renderDenialReason(reason: PermissionDenialReason) {
    switch (reason.type) {
      case 'never_granted':
        return html`<span class="reason-tag never-granted">
          <sl-icon name="dash-circle"></sl-icon> Never granted
        </span>`;
      case 'inactive_grant':
        return html`<span class="reason-tag inactive">
          <sl-icon name="clock"></sl-icon> Grant ${reason.grantStatus}
        </span>`;
      case 'removed_by_boundaries':
        return html`<span class="reason-tag boundary">
          <sl-icon name="shield-x"></sl-icon>
          Removed by
          ${reason.boundaryNames
            .map((n) => n ?? 'access boundary (details unavailable)')
            .join(', ')}
        </span>`;
      case 'removed_by_restriction':
        return html`<span class="reason-tag restriction">
          <sl-icon name="lock"></sl-icon> ${reason.restrictionLabel}
        </span>`;
      case 'evaluation_failed':
        return html`<span class="reason-tag failed">
          <sl-icon name="exclamation-triangle"></sl-icon>
          Evaluation failed (${reason.correlationId})
        </span>`;
      default:
        return nothing;
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-authorization-layer-stack': ScionAuthorizationLayerStack;
  }
}
