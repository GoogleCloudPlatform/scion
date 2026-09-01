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
 * Effective Role Provenance & Access Composition Display
 *
 * Shows a principal's effective roles with provenance and layered
 * effective-access composition:
 *
 * 1. Assigned roles / Potential permissions: direct/group provenance,
 *    active/scheduled/expired assignment state, union of permissions
 *    from active grants.
 * 2. Access boundaries: named active/scheduled boundaries with membership
 *    paths and their impact.
 * 3. Intrinsic restrictions: credential scope, principal status, delegation
 *    ceiling.
 * 4. Effective permissions: final set after all layers applied.
 *
 * TERMINOLOGY: layers are descriptive, not an override order. Overlapping
 * removal is "removed by both," not "won by." Never use "priority",
 * "override", or "winner."
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { getLifecycleStatus, formatDateTime } from './role-binding-utils.js';

import type { RedactionNotice } from '../../shared/access-boundaries.js';

import type {
  BoundaryLayer,
  IntrinsicRestriction,
  DeniedPermission,
  PermissionDenialReason,
} from './authorization-layer-stack.js';
import './authorization-layer-stack.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface EffectiveRoleBinding {
  id: string;
  roleDefinitionId: string;
  roleName: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  scopeDisplayName?: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
  /** How the binding was obtained: 'direct' or the group that grants it. */
  source: 'direct' | string;
  /** When source is not 'direct', this holds the group display name. */
  sourceGroupName?: string;
}

/** Shape of the access-explain API response. */
interface AccessExplainResponse {
  principalType?: string;
  principalId?: string;
  potentialPermissionCount?: number;
  effectivePermissionCount?: number;
  boundaries?: AccessExplainBoundary[];
  restrictions?: AccessExplainRestriction[];
  deniedPermissions?: AccessExplainDeniedPermission[];
  redacted?: RedactionNotice;
}

interface AccessExplainBoundary {
  id: string;
  name?: string | null;
  status?: string;
  removedCount?: number;
  overlapCount?: number;
  membershipSummary?: string;
  redacted?: RedactionNotice;
}

interface AccessExplainRestriction {
  kind?: string;
  label?: string;
  removedCount?: number;
  detail?: string;
}

interface AccessExplainDeniedPermission {
  permissionId?: string;
  reasons?: Array<{
    type?: string;
    grantStatus?: string;
    boundaryNames?: (string | null)[];
    restrictionLabel?: string;
    correlationId?: string;
  }>;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-effective-role-provenance')
export class ScionEffectiveRoleProvenance extends LitElement {
  /** The principal type: 'user' or 'agent'. */
  @property() principalType: 'user' | 'agent' = 'user';

  /** The principal's ID. */
  @property() principalId = '';

  /** Whether to render in compact card layout. */
  @property({ type: Boolean }) compact = false;

  /** Section title override. */
  @property() sectionTitle = 'Effective Roles';

  @state() private loading = true;
  @state() private bindings: EffectiveRoleBinding[] = [];
  @state() private error: string | null = null;

  // Explain layer state
  @state() private explainLoading = false;
  @state() private explainError: string | null = null;
  @state() private potentialCount = 0;
  @state() private effectiveCount = 0;
  @state() private boundaries: BoundaryLayer[] = [];
  @state() private restrictions: IntrinsicRestriction[] = [];
  @state() private deniedPermissions: DeniedPermission[] = [];
  @state() private explainRedacted?: RedactionNotice;
  @state() private showLayers = false;
  /** Whether explain layers have been successfully loaded at least once. */
  @state() private _explainLoaded = false;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .section {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1.5rem;
        margin-bottom: 1.5rem;
      }

      .section-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        margin-bottom: 1rem;
      }

      .section-header h2 {
        font-size: 1.125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0;
      }

      .role-count {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        font-weight: 400;
        margin-left: 0.5rem;
      }

      .standalone-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        margin-bottom: 1rem;
      }

      .standalone-header h2 {
        font-size: 1.125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0;
      }

      /* Role cards list */
      .role-list {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }

      .role-card {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        padding: 0.75rem 1rem;
        background: var(--scion-bg-subtle, #f8fafc);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        gap: 1rem;
      }

      .role-card:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .role-card-left {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
        min-width: 0;
        flex: 1;
      }

      .role-name {
        font-weight: 600;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .role-scope {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .scope-tag {
        display: inline-flex;
        align-items: center;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      /* Provenance badge */
      .provenance {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        font-size: 0.75rem;
      }

      .provenance.direct {
        color: var(--sl-color-primary-600, #2563eb);
      }

      .provenance.group {
        color: var(--sl-color-warning-600, #d97706);
      }

      .provenance sl-icon {
        font-size: 0.75rem;
      }

      /* Lifecycle status badge */
      .role-card-right {
        display: flex;
        flex-direction: column;
        align-items: flex-end;
        gap: 0.25rem;
        flex-shrink: 0;
      }

      .status-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
      }

      .status-badge.active {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .status-badge.expired {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .status-badge.pending {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .lifecycle-info {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      /* Empty state */
      .empty-state {
        text-align: center;
        padding: 2rem 1.5rem;
      }

      .empty-state sl-icon {
        font-size: 2.5rem;
        color: var(--scion-text-muted, #64748b);
        opacity: 0.4;
        margin-bottom: 0.75rem;
      }

      .empty-state h3 {
        font-size: 1rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0 0 0.25rem 0;
      }

      .empty-state p {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        margin: 0;
      }

      /* Loading / Error */
      .loading-state {
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 2rem;
        color: var(--scion-text-muted, #64748b);
        gap: 0.75rem;
        font-size: 0.875rem;
      }

      .error-state {
        color: var(--sl-color-danger-600, #dc2626);
        font-size: 0.875rem;
        padding: 0.75rem 1rem;
        background: var(--sl-color-danger-50, #fef2f2);
        border-radius: var(--scion-radius, 0.5rem);
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.5rem;
      }

      /* Layers section */
      .layers-toggle {
        margin-top: 1rem;
        padding-top: 0.75rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .layers-toggle-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        cursor: pointer;
        user-select: none;
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--sl-color-primary-600, #2563eb);
        padding: 0.25rem 0;
      }

      .layers-toggle-header:hover {
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .layers-toggle-header sl-icon {
        font-size: 0.75rem;
        transition: transform 0.2s ease;
      }

      .layers-toggle-header sl-icon.open {
        transform: rotate(90deg);
      }

      .layers-content {
        margin-top: 0.75rem;
      }

      .explain-error {
        font-size: 0.8125rem;
        color: var(--sl-color-danger-600, #dc2626);
        padding: 0.5rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .explain-loading {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem;
      }

      .redaction-notice {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
        padding: 0.375rem 0;
      }

      @media (max-width: 768px) {
        .role-card {
          flex-direction: column;
          gap: 0.5rem;
        }

        .role-card-right {
          align-items: flex-start;
        }
      }

      .role-name {
        overflow-wrap: anywhere;
      }

      .scope-tag {
        overflow-wrap: anywhere;
      }

      @media (forced-colors: active) {
        .section {
          border-color: ButtonText;
        }

        .role-card {
          border-color: ButtonText;
        }

        .status-badge {
          border: 1px solid ButtonText;
        }

        .error-state {
          border: 1px solid ButtonText;
        }

        .layers-toggle {
          border-top-color: ButtonText;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .layers-toggle-header sl-icon {
          transition: none;
        }
      }
    `,
  ];

  /** Guard to prevent double-fetch when connectedCallback and updated both fire. */
  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.principalId) {
      this._initialLoadDone = true;
      void this.loadEffectiveRoles();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if ((changed.has('principalId') || changed.has('principalType')) && this.principalId) {
      // Skip if connectedCallback already triggered the initial load.
      if (!this._initialLoadDone) {
        void this.loadEffectiveRoles();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadEffectiveRoles(): Promise<void> {
    if (!this.principalId) return;

    this.loading = true;
    this.error = null;

    try {
      // Fetch role bindings for this principal (direct and group-derived)
      const url = `/api/v1/admin/role-bindings?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}&includeGroupDerived=true`;
      const res = await apiFetch(url);

      if (!res.ok) {
        // Fall back to fetching just the direct bindings without the
        // includeGroupDerived parameter (which may not be supported yet).
        const fallbackUrl = `/api/v1/admin/role-bindings?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}`;
        const fallbackRes = await apiFetch(fallbackUrl);

        if (!fallbackRes.ok) {
          throw new Error(await extractApiError(fallbackRes, `HTTP ${fallbackRes.status}`));
        }

        const data = (await fallbackRes.json()) as {
          items?: EffectiveRoleBinding[];
        };
        this.bindings = (data.items || []).map((b) => ({
          ...b,
          source: b.source || 'direct',
        }));
      } else {
        const data = (await res.json()) as {
          items?: EffectiveRoleBinding[];
        };
        this.bindings = (data.items || []).map((b) => ({
          ...b,
          source: b.source || 'direct',
        }));
      }
    } catch (err) {
      console.error('Failed to load effective roles:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load effective roles';
    } finally {
      this.loading = false;
    }
  }

  private async loadExplainLayers(): Promise<void> {
    if (!this.principalId) return;

    this.explainLoading = true;
    this.explainError = null;

    try {
      const url = `/api/v1/admin/access-explain?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}`;
      const res = await apiFetch(url);

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      const data = (await res.json()) as AccessExplainResponse;

      this.potentialCount = data.potentialPermissionCount ?? 0;
      this.effectiveCount = data.effectivePermissionCount ?? 0;

      // Map boundaries — preserve redaction
      this.boundaries = (data.boundaries ?? []).map((b): BoundaryLayer => {
        const layer: BoundaryLayer = {
          id: b.id,
          name: b.redacted ? null : (b.name ?? null),
          status: b.status ?? 'active',
          removedCount: b.removedCount ?? 0,
          overlapCount: b.overlapCount ?? 0,
        };
        if (b.membershipSummary !== undefined) layer.membershipSummary = b.membershipSummary;
        if (b.redacted !== undefined) layer.redacted = b.redacted;
        return layer;
      });

      // Map restrictions
      this.restrictions = (data.restrictions ?? []).map((r): IntrinsicRestriction => {
        const restriction: IntrinsicRestriction = {
          kind: r.kind ?? 'unknown',
          label: r.label ?? r.kind ?? 'Unknown restriction',
          removedCount: r.removedCount ?? 0,
        };
        if (r.detail !== undefined) restriction.detail = r.detail;
        return restriction;
      });

      // Map denied permissions with reasons
      this.deniedPermissions = (data.deniedPermissions ?? []).map(
        (dp): DeniedPermission => ({
          permissionId: dp.permissionId ?? '',
          reasons: (dp.reasons ?? []).map((r): PermissionDenialReason => {
            switch (r.type) {
              case 'never_granted':
                return { type: 'never_granted' };
              case 'inactive_grant':
                return { type: 'inactive_grant', grantStatus: r.grantStatus ?? 'inactive' };
              case 'removed_by_boundaries':
                return { type: 'removed_by_boundaries', boundaryNames: r.boundaryNames ?? [] };
              case 'removed_by_restriction':
                return {
                  type: 'removed_by_restriction',
                  restrictionLabel: r.restrictionLabel ?? '',
                };
              case 'evaluation_failed':
                return { type: 'evaluation_failed', correlationId: r.correlationId ?? '' };
              default:
                return { type: 'never_granted' };
            }
          }),
        })
      );

      if (data.redacted !== undefined) {
        this.explainRedacted = data.redacted;
      }

      this._explainLoaded = true;
    } catch (err) {
      console.error('Failed to load access explain:', err);
      this.explainError = err instanceof Error ? err.message : 'Failed to load access layers';
    } finally {
      this.explainLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderStandalone();
  }

  private renderStandalone() {
    return html`
      <div class="standalone-header">
        <h2>
          ${this.sectionTitle}
          <span class="role-count">(${this.bindings.length})</span>
        </h2>
      </div>
      ${this.renderContent()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section">
        <div class="section-header">
          <h2>
            ${this.sectionTitle}
            <span class="role-count">(${this.bindings.length})</span>
          </h2>
        </div>
        ${this.renderContent()}
      </div>
    `;
  }

  private renderContent() {
    if (this.loading) {
      return html`
        <div class="loading-state"><sl-spinner></sl-spinner> Loading effective roles...</div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state" role="alert">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadEffectiveRoles()}> Retry </sl-button>
        </div>
      `;
    }

    if (this.bindings.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield"></sl-icon>
          <h3>No Roles Assigned</h3>
          <p>This ${this.principalType} does not have any role assignments.</p>
        </div>
      `;
    }

    return html`
      <div class="role-list">${this.bindings.map((binding) => this.renderRoleCard(binding))}</div>
      ${this.renderLayersSection()}
    `;
  }

  private renderRoleCard(binding: EffectiveRoleBinding) {
    const status = getLifecycleStatus(binding);

    return html`
      <div class="role-card">
        <div class="role-card-left">
          <span class="role-name">${binding.roleName || binding.roleDefinitionId}</span>
          <div class="role-scope">
            <span class="scope-tag">${binding.scopeType}</span>
            ${binding.scopeId
              ? html`<span>${binding.scopeDisplayName || binding.scopeId}</span>`
              : ''}
          </div>
          <div class="provenance ${binding.source === 'direct' ? 'direct' : 'group'}">
            ${binding.source === 'direct'
              ? html`<sl-icon name="person-check"></sl-icon> Direct`
              : html`<sl-icon name="diagram-3"></sl-icon> Via group:
                  ${binding.sourceGroupName || binding.source}`}
          </div>
        </div>
        <div class="role-card-right">
          <span class="status-badge ${status}">
            <sl-icon
              name=${status === 'active'
                ? 'check-circle'
                : status === 'expired'
                  ? 'x-circle'
                  : 'clock'}
            ></sl-icon>
            ${status === 'active' ? 'Active' : status === 'expired' ? 'Expired' : 'Scheduled'}
          </span>
          ${binding.expiresAt && status !== 'expired'
            ? html`<span class="lifecycle-info">
                Expires ${formatDateTime(binding.expiresAt)}
              </span>`
            : ''}
          ${binding.notBefore && status === 'pending'
            ? html`<span class="lifecycle-info">
                Activates ${formatDateTime(binding.notBefore)}
              </span>`
            : ''}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Layers section — effective-access composition
  // ---------------------------------------------------------------------------

  private renderLayersSection() {
    return html`
      <div class="layers-toggle">
        <div
          class="layers-toggle-header"
          @click=${() => this.handleLayersToggle()}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.handleLayersToggle();
            }
          }}
          tabindex="0"
          role="button"
          aria-expanded=${this.showLayers}
        >
          <sl-icon name="chevron-right" class=${this.showLayers ? 'open' : ''}></sl-icon>
          Effective access composition
        </div>
        ${this.showLayers ? this.renderLayersContent() : nothing}
      </div>
    `;
  }

  private handleLayersToggle(): void {
    this.showLayers = !this.showLayers;
    if (this.showLayers && !this.explainLoading && !this._explainLoaded && !this.explainError) {
      void this.loadExplainLayers();
    }
  }

  private renderLayersContent() {
    if (this.explainLoading) {
      return html`
        <div class="layers-content">
          <div class="explain-loading"><sl-spinner></sl-spinner> Loading access layers...</div>
        </div>
      `;
    }

    if (this.explainError) {
      return html`
        <div class="layers-content">
          <div class="explain-error" role="alert">
            <sl-icon name="exclamation-triangle"></sl-icon>
            ${this.explainError}
            <sl-button size="small" @click=${() => this.loadExplainLayers()}> Retry </sl-button>
          </div>
        </div>
      `;
    }

    return html`
      <div class="layers-content">
        ${this.explainRedacted
          ? html`<div class="redaction-notice">${this.explainRedacted.message}</div>`
          : nothing}
        <scion-authorization-layer-stack
          .potentialCount=${this.potentialCount}
          .boundaries=${this.boundaries}
          .restrictions=${this.restrictions}
          .effectiveCount=${this.effectiveCount}
          .deniedPermissions=${this.deniedPermissions}
        ></scion-authorization-layer-stack>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-role-provenance': ScionEffectiveRoleProvenance;
  }
}
