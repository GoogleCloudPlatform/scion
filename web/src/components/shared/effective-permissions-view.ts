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
 * Effective Permissions View
 *
 * Lazily fetches and displays a user's resolved effective permissions and
 * contributing grant provenance at hub scope (`GET /api/v1/admin/effective-permissions`),
 * grouped by resource with copyable binding ID origin chips.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { stateManager } from '../../client/state.js';
import { hasAnyPermission } from '../../lib/admin-permissions.js';
import { copyWithToast } from '../../utils/clipboard.js';
import { formatRelative } from '../../utils/time.js';
import './project-picker.js';
import type { ProjectChangeDetail } from './project-picker.js';

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export interface EffectivePermissionPrincipal {
  type: string;
  id: string;
  status: string;
}

export interface EffectivePermissionScope {
  type: 'system' | 'project';
  id?: string;
  name?: string;
  slug?: string;
}

export interface EffectiveStatusGate {
  reason: 'suspended' | 'invited' | 'deleting' | 'missing';
}

export interface EffectivePermissionRoleRef {
  id: string;
  name: string;
  system: boolean;
}

export interface EffectivePermissionGrantScope {
  type: 'system' | 'project';
  id?: string;
}

export interface EffectivePermissionGroupVia {
  groupId: string;
  groupName: string;
  groupSlug: string;
  path?: string[];
}

export interface EffectivePermissionGrant {
  bindingId: string;
  role: EffectivePermissionRoleRef;
  scope: EffectivePermissionGrantScope;
  via: EffectivePermissionGroupVia | null;
  expiresAt: string | null;
}

export interface EffectivePermissionEntry {
  id: string;
  resource: string;
  action: string;
  description: string;
  appliesInProjects?: boolean;
  grants: EffectivePermissionGrant[];
}

export interface EffectivePermissionBoundaryRef {
  id: string;
  name: string;
  scopeType: string;
  scopeId?: string;
  redacted: boolean;
}

export interface EffectivePermissionRemoved {
  id: string;
  resource?: string;
  action?: string;
  description?: string;
  appliesInProjects?: boolean;
  grants: EffectivePermissionGrant[];
  removedBy: EffectivePermissionBoundaryRef[];
}

export interface EffectivePermissionSet {
  principal: EffectivePermissionPrincipal;
  scope: EffectivePermissionScope;
  statusGate: EffectiveStatusGate | null;
  evaluatedAt: string;
  permissions: EffectivePermissionEntry[];
  removed: EffectivePermissionRemoved[];
}

export interface RenderPermissionListOptions {
  filter?: string;
  onCopyBindingId?: (bindingId: string) => void;
}

export interface PermissionResourceGroup {
  resource: string;
  items: EffectivePermissionEntry[];
}

/**
 * Groups permissions by `resource` while preserving registry ordering,
 * optionally narrowed by a case-insensitive permission ID substring query.
 */
export function groupPermissionsByResource(
  permissions: ReadonlyArray<EffectivePermissionEntry>,
  filterQuery = ''
): PermissionResourceGroup[] {
  const needle = filterQuery.trim().toLowerCase();
  const filtered = needle
    ? permissions.filter((p) => p.id.toLowerCase().includes(needle))
    : permissions;

  const groups: PermissionResourceGroup[] = [];
  const byResource = new Map<string, EffectivePermissionEntry[]>();
  for (const perm of filtered) {
    let bucket = byResource.get(perm.resource);
    if (!bucket) {
      bucket = [];
      byResource.set(perm.resource, bucket);
      groups.push({ resource: perm.resource, items: bucket });
    }
    bucket.push(perm);
  }
  return groups;
}

function renderGrantChip(
  grant: EffectivePermissionGrant,
  onCopy: (bindingId: string) => void,
  isProjectView = false
): TemplateResult {
  const isDirect = grant.via === null;
  const groupLabel = grant.via?.groupName || grant.via?.groupId || '';
  const baseLabel = isDirect
    ? `${grant.role.name} · direct`
    : `${grant.role.name} · via group: ${groupLabel}`;
  const originPrefix = grant.scope.type === 'project' ? 'Project role' : 'Inherited from hub';
  const chipLabel = isProjectView ? `${originPrefix} · ${baseLabel}` : baseLabel;
  const scopeClass = isProjectView
    ? grant.scope.type === 'project'
      ? 'scope-project'
      : 'scope-inherited'
    : '';

  return html`
    <span class="grant-item">
      <sl-tooltip content=${`Binding ID: ${grant.bindingId} — click to copy`}>
        <button
          type="button"
          class="origin-chip ${isDirect ? 'direct' : 'group'} ${scopeClass}"
          @click=${(): void => onCopy(grant.bindingId)}
        >
          <sl-icon name=${isDirect ? 'person' : 'people'}></sl-icon>
          <span>${chipLabel}</span>
        </button>
      </sl-tooltip>
      ${grant.expiresAt
        ? html`<span class="expires-badge">
            <sl-icon name="clock"></sl-icon>
            expires ${formatRelative(grant.expiresAt)}
          </span>`
        : nothing}
    </span>
  `;
}

function statusGateMessage(gate: EffectiveStatusGate): string {
  switch (gate.reason) {
    case 'suspended':
      return 'Suspended: holds no effective permissions';
    case 'invited':
      return 'Invited: holds no effective permissions';
    case 'deleting':
      return 'Deleting: holds no effective permissions';
    case 'missing':
      return 'Account unavailable: holds no effective permissions';
    default: {
      const reason = String(gate.reason);
      const label = reason ? reason.charAt(0).toUpperCase() + reason.slice(1) : 'Inactive';
      return `${label}: holds no effective permissions`;
    }
  }
}

function renderApplicabilityBadge(
  appliesInProjects: boolean | undefined,
  isProjectView: boolean
): TemplateResult | typeof nothing {
  if (isProjectView || typeof appliesInProjects !== 'boolean') {
    return nothing;
  }
  return html`
    <span
      class="scope-applicability-badge ${appliesInProjects ? 'applies-all-projects' : 'hub-only'}"
      data-applies-in-projects=${appliesInProjects ? 'true' : 'false'}
    >
      ${appliesInProjects ? 'applies in all projects' : 'hub only'}
    </span>
  `;
}

function renderRemovedBoundaryChip(boundary: EffectivePermissionBoundaryRef): TemplateResult {
  const isRedacted = boundary.redacted || !boundary.name;
  const label = isRedacted ? 'Access constraint (details unavailable)' : boundary.name;
  const scopeLabel = boundary.scopeType === 'project' ? 'project' : 'hub';

  return html`
    <span
      class="removed-boundary-chip ${isRedacted ? 'redacted' : ''}"
      data-boundary-redacted=${isRedacted ? 'true' : 'false'}
    >
      <sl-icon name="shield-x"></sl-icon>
      <span>${label}</span>
      ${boundary.scopeType ? html`<span class="boundary-scope-tag">${scopeLabel}</span>` : nothing}
    </span>
  `;
}

function renderRemovedPermissionsSection(
  removed: ReadonlyArray<EffectivePermissionRemoved>,
  onCopy: (bindingId: string) => void,
  isProjectView: boolean
): TemplateResult | typeof nothing {
  if (removed.length === 0) {
    return nothing;
  }

  return html`
    <sl-details
      class="removed-permissions-details"
      summary=${`Removed by access constraints (${removed.length})`}
      @sl-show=${(e: Event): void => {
        if (e.target !== e.currentTarget) return;
      }}
      @sl-hide=${(e: Event): void => {
        if (e.target !== e.currentTarget) return;
      }}
    >
      <div class="removed-rows">
        ${removed.map(
          (item) => html`
            <div class="removed-row" data-removed-permission-id=${item.id}>
              <div class="perm-meta">
                <div class="perm-id-line">
                  <code class="perm-id removed-perm-id">${item.id}</code>
                  ${renderApplicabilityBadge(item.appliesInProjects, isProjectView)}
                </div>
                ${item.description
                  ? html`<span class="perm-desc">${item.description}</span>`
                  : nothing}
              </div>
              <div class="removed-details-col">
                <div class="removed-grants">
                  <span class="removed-label">Would have been granted by</span>
                  <div class="perm-grants">
                    ${item.grants.map((g) => renderGrantChip(g, onCopy, isProjectView))}
                  </div>
                </div>
                <div class="removed-by">
                  <span class="removed-label">Removed by access constraint</span>
                  <div class="removed-boundaries">
                    ${item.removedBy.map((b) => renderRemovedBoundaryChip(b))}
                  </div>
                </div>
              </div>
            </div>
          `
        )}
      </div>
    </sl-details>
  `;
}

/**
 * Pure presentational helper rendering an `EffectivePermissionSet` grouped by
 * resource with origin chips, binding ID tooltips, expiry badges, statusGate
 * banner, and collapsed removed-by-access-constraints list.
 */
export function renderPermissionList(
  data: EffectivePermissionSet,
  options: RenderPermissionListOptions = {}
): TemplateResult {
  const filter = options.filter ?? '';
  const onCopy =
    options.onCopyBindingId ??
    ((bindingId: string): void => {
      void copyWithToast(bindingId);
    });
  const isProjectView = data.scope.type === 'project';

  if (data.statusGate !== null) {
    return html`
      <div
        class="status-gate-banner"
        role="status"
        data-status-gate-reason=${data.statusGate.reason}
      >
        <sl-icon name="shield-exclamation"></sl-icon>
        <div class="status-gate-copy">
          <div class="status-gate-title">${statusGateMessage(data.statusGate)}</div>
          <p class="status-gate-detail">
            Stored role bindings are still listed on the Effective roles tab.
          </p>
        </div>
      </div>
    `;
  }

  const removedSection = renderRemovedPermissionsSection(data.removed ?? [], onCopy, isProjectView);

  if (data.permissions.length === 0) {
    return html`
      <div class="empty-state">
        <sl-icon name="shield"></sl-icon>
        <h3>No Effective Permissions</h3>
        <p>This user holds no active permissions at this scope.</p>
      </div>
      ${removedSection}
    `;
  }

  const groups = groupPermissionsByResource(data.permissions, filter);
  if (groups.length === 0) {
    return html`
      <div class="empty-state compact-empty">
        <p>No permissions match "${filter.trim()}".</p>
      </div>
      ${removedSection}
    `;
  }

  return html`
    <div class="permission-groups">
      ${groups.map(
        (group) => html`
          <section class="resource-group" data-resource=${group.resource}>
            <h4 class="resource-header">
              <span>${group.resource}</span>
              <span class="resource-count">(${group.items.length})</span>
            </h4>
            <div class="permission-rows">
              ${group.items.map(
                (perm) => html`
                  <div class="permission-row" data-permission-id=${perm.id}>
                    <div class="perm-meta">
                      <div class="perm-id-line">
                        <code class="perm-id">${perm.id}</code>
                        ${renderApplicabilityBadge(perm.appliesInProjects, isProjectView)}
                      </div>
                      ${perm.description
                        ? html`<span class="perm-desc">${perm.description}</span>`
                        : nothing}
                    </div>
                    <div class="perm-grants">
                      ${perm.grants.map((g) => renderGrantChip(g, onCopy, isProjectView))}
                    </div>
                  </div>
                `
              )}
            </div>
          </section>
        `
      )}
    </div>
    ${removedSection}
  `;
}

@customElement('scion-effective-permissions-view')
export class ScionEffectivePermissionsView extends LitElement {
  @property({ type: String })
  principalType = 'user';

  @property({ type: String })
  principalId = '';

  /**
   * Whether the enclosing tab is currently active. Fetches are deferred until
   * `active` becomes `true` for the current `principalId`.
   */
  @property({ type: Boolean })
  active = false;

  @state()
  private _loading = false;

  @state()
  private _error: string | null = null;

  @state()
  private _data: EffectivePermissionSet | null = null;

  @state()
  private _filter = '';

  @state()
  private _canRead = false;

  @state()
  private _permissionsChecked = false;

  @state()
  private _projectDetailsOpen = false;

  @state()
  private _projectSectionEverOpened = false;

  @state()
  private _selectedProjectId = '';

  @state()
  private _projectLoading = false;

  @state()
  private _projectError: string | null = null;

  @state()
  private _projectData: EffectivePermissionSet | null = null;

  @state()
  private _projectFilter = '';

  private _loadedPrincipalId: string | null = null;
  private _loadSeq = 0;
  private _projectLoadSeq = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .hub-section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      margin-bottom: 0.75rem;
      flex-wrap: wrap;
    }

    .hub-section-header h3 {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .perm-total {
      font-weight: 400;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .filter-input {
      min-width: 14rem;
      flex: 0 1 16rem;
    }

    .permission-groups {
      display: flex;
      flex-direction: column;
      gap: 0.875rem;
      max-height: 26rem;
      overflow-y: auto;
      padding-right: 0.125rem;
    }

    .resource-group {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
      overflow: hidden;
    }

    .resource-header {
      margin: 0;
      padding: 0.4375rem 0.75rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f8fafc);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .resource-count {
      font-weight: 500;
    }

    .permission-rows {
      display: flex;
      flex-direction: column;
    }

    .permission-row {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 0.75rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .permission-row:last-child {
      border-bottom: none;
    }

    .perm-meta {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      min-width: 0;
      flex: 1 1 14rem;
    }

    .perm-id-line {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.375rem;
    }

    .scope-applicability-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.0625rem 0.4375rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
      line-height: 1.3;
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f8fafc);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .scope-applicability-badge.applies-all-projects {
      border-color: var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .perm-id {
      font-family: var(--scion-font-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      overflow-wrap: anywhere;
    }

    .perm-desc {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .perm-grants {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 0.375rem;
      flex: 0 1 auto;
    }

    .grant-item {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      flex-wrap: wrap;
    }

    .origin-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.1875rem 0.5rem;
      border-radius: 9999px;
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f8fafc);
      color: var(--scion-text, #1e293b);
      font-family: inherit;
      font-size: 0.75rem;
      line-height: 1.25;
      cursor: pointer;
      transition: background 0.12s ease;
    }

    .origin-chip:hover {
      background: var(--sl-color-neutral-200, #e2e8f0);
    }

    .origin-chip:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: 1px;
    }

    .origin-chip.direct {
      color: var(--scion-primary, #3b82f6);
    }

    .origin-chip.scope-inherited {
      border-style: dashed;
    }

    .by-project-details {
      margin-top: 1rem;
    }

    .by-project-details::part(base) {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
    }

    .by-project-details::part(header) {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      padding: 0.75rem 1rem;
    }

    .by-project-details::part(content) {
      padding: 0 1rem 1rem 1rem;
    }

    .by-project-body {
      display: flex;
      flex-direction: column;
      gap: 0.875rem;
    }

    .project-permissions-results {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .project-results-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .project-results-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .origin-chip sl-icon {
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    .expires-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.4375rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .expires-badge sl-icon {
      font-size: 0.6875rem;
    }

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

    .empty-state {
      text-align: center;
      padding: 2rem 1.5rem;
    }

    .empty-state.compact-empty {
      padding: 1.25rem 1rem;
    }

    .empty-state sl-icon {
      font-size: 2.25rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.45;
      margin-bottom: 0.5rem;
    }

    .empty-state h3 {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      margin: 0;
    }

    .status-gate-banner {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      padding: 0.875rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--sl-color-warning-300, #fcd34d);
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-900, #78350f);
    }

    .status-gate-banner sl-icon {
      font-size: 1.25rem;
      flex-shrink: 0;
      margin-top: 0.125rem;
      color: var(--sl-color-warning-700, #b45309);
    }

    .status-gate-copy {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .status-gate-title {
      font-size: 0.875rem;
      font-weight: 600;
    }

    .status-gate-detail {
      font-size: 0.8125rem;
      margin: 0;
      opacity: 0.9;
    }

    .removed-permissions-details {
      margin-top: 0.75rem;
    }

    .removed-permissions-details::part(base) {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #ffffff);
    }

    .removed-permissions-details::part(header) {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0.75rem;
    }

    .removed-permissions-details::part(content) {
      padding: 0;
    }

    .removed-rows {
      display: flex;
      flex-direction: column;
    }

    .removed-row {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 0.75rem;
      padding: 0.625rem 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .removed-perm-id {
      text-decoration: line-through;
      color: var(--scion-text-muted, #64748b);
    }

    .removed-details-col {
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 0.375rem;
      flex: 0 1 auto;
    }

    .removed-grants,
    .removed-by {
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 0.25rem;
    }

    .removed-label {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .removed-boundaries {
      display: flex;
      flex-wrap: wrap;
      justify-content: flex-end;
      gap: 0.375rem;
    }

    .removed-boundary-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.1875rem 0.5rem;
      border-radius: 9999px;
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.75rem;
      line-height: 1.25;
    }

    .removed-boundary-chip.redacted {
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f8fafc);
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }

    .removed-boundary-chip sl-icon {
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    .boundary-scope-tag {
      font-size: 0.6875rem;
      font-style: normal;
      opacity: 0.8;
      padding-left: 0.125rem;
    }

    .ownership-footnote {
      display: flex;
      align-items: flex-start;
      gap: 0.375rem;
      margin: 0.875rem 0 0 0;
      padding-top: 0.625rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.4;
    }

    .ownership-footnote sl-icon {
      font-size: 0.875rem;
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    @media (max-width: 768px) {
      .permission-row,
      .removed-row {
        flex-direction: column;
      }

      .perm-grants,
      .removed-boundaries {
        justify-content: flex-start;
      }

      .removed-details-col,
      .removed-grants,
      .removed-by {
        align-items: flex-start;
      }
    }

    @media (forced-colors: active) {
      .resource-group,
      .origin-chip,
      .expires-badge,
      .scope-applicability-badge,
      .error-state,
      .status-gate-banner,
      .removed-boundary-chip,
      .by-project-details::part(base),
      .removed-permissions-details::part(base) {
        border: 1px solid ButtonText;
      }
    }
  `;

  /** Clears cached permission data and reloads if currently visible. */
  public invalidate(): void {
    this._loadSeq++;
    this._projectLoadSeq++;
    this._loading = false;
    this._projectLoading = false;
    this._loadedPrincipalId = null;
    this._permissionsChecked = false;
    this._data = null;
    this._error = null;
    this._projectData = null;
    this._projectError = null;
    if (this.active && this.principalId) {
      void this.loadHubPermissions();
      if (this._selectedProjectId) {
        void this.loadProjectPermissions(this._selectedProjectId);
      }
    }
  }

  override willUpdate(changed: Map<string, unknown>): void {
    if (changed.has('principalId') || changed.has('principalType')) {
      if (this._loadedPrincipalId !== this.principalId) {
        this._loadedPrincipalId = null;
        this._data = null;
        this._error = null;
        this._filter = '';
        this._projectLoadSeq++;
        this._projectDetailsOpen = false;
        this._projectSectionEverOpened = false;
        this._selectedProjectId = '';
        this._projectData = null;
        this._projectError = null;
        this._projectLoading = false;
        this._projectFilter = '';
      }
    }
    if (
      this.active &&
      this.principalId &&
      this._loadedPrincipalId !== this.principalId &&
      !this._loading &&
      !this._error
    ) {
      void this.loadHubPermissions();
    }
    if (
      this.active &&
      this.principalId &&
      this._selectedProjectId &&
      !this._projectData &&
      !this._projectLoading &&
      !this._projectError
    ) {
      void this.loadProjectPermissions(this._selectedProjectId);
    }
  }

  private async loadHubPermissions(): Promise<void> {
    if (!this.principalId) return;
    const targetPrincipal = this.principalId;
    const seq = ++this._loadSeq;

    this._loading = true;
    this._error = null;

    try {
      const status = await loadAdminStatus(stateManager.getCurrentUserId());
      if (seq !== this._loadSeq) return;
      if (status === null) {
        this._canRead = false;
        this._permissionsChecked = false;
        this._error = "Couldn't check your permissions";
        return;
      }
      this._canRead = hasAnyPermission(status, ['role_binding.read']);
      this._permissionsChecked = true;
      if (!this._canRead) {
        this._loadedPrincipalId = targetPrincipal;
        return;
      }

      const url = `/api/v1/admin/effective-permissions?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(targetPrincipal)}`;
      const res = await apiFetch(url, { suppressAccessDeniedToast: true });
      if (seq !== this._loadSeq) return;

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this._data = (await res.json()) as EffectivePermissionSet;
      this._loadedPrincipalId = targetPrincipal;
    } catch (err) {
      if (seq !== this._loadSeq) return;
      this._error = err instanceof Error ? err.message : 'Failed to load effective permissions';
    } finally {
      if (seq === this._loadSeq) {
        this._loading = false;
      }
    }
  }

  private handleProjectChange(e: CustomEvent<ProjectChangeDetail>): void {
    const detail = e.detail;
    const projectId = detail?.projectId?.trim() ?? '';
    if (!projectId) {
      this._projectLoadSeq++;
      this._selectedProjectId = '';
      this._projectData = null;
      this._projectError = null;
      this._projectLoading = false;
      this._projectFilter = '';
      return;
    }
    // Ignore intermediate search-input emissions from scion-project-picker
    // until a dropdown option is selected (non-empty displayLabel) or a full UUID is entered.
    if (detail?.displayLabel === '' && !UUID_RE.test(projectId)) {
      return;
    }
    if (detail?.displayLabel === projectId && projectId.length < 2) {
      return;
    }
    if (projectId === this._selectedProjectId && (this._projectData || this._projectLoading)) {
      return;
    }
    this._selectedProjectId = projectId;
    this._projectFilter = '';
    void this.loadProjectPermissions(projectId);
  }

  private async loadProjectPermissions(projectId: string): Promise<void> {
    if (!this.principalId || !projectId) return;
    const targetPrincipal = this.principalId;
    const seq = ++this._projectLoadSeq;

    this._projectLoading = true;
    this._projectError = null;

    try {
      const url = `/api/v1/admin/effective-permissions?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(targetPrincipal)}&projectId=${encodeURIComponent(projectId)}`;
      const res = await apiFetch(url, { suppressAccessDeniedToast: true });
      if (seq !== this._projectLoadSeq) return;

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this._projectData = (await res.json()) as EffectivePermissionSet;
    } catch (err) {
      if (seq !== this._projectLoadSeq) return;
      this._projectData = null;
      this._projectError =
        err instanceof Error ? err.message : 'Failed to load project effective permissions';
    } finally {
      if (seq === this._projectLoadSeq) {
        this._projectLoading = false;
      }
    }
  }

  private renderByProjectSection(): TemplateResult {
    return html`
      <sl-details
        class="by-project-details"
        summary="By project"
        ?open=${this._projectDetailsOpen}
        @sl-show=${(e: Event): void => {
          if (e.target !== e.currentTarget) return;
          this._projectDetailsOpen = true;
          this._projectSectionEverOpened = true;
        }}
        @sl-hide=${(e: Event): void => {
          if (e.target !== e.currentTarget) return;
          this._projectDetailsOpen = false;
        }}
      >
        ${this._projectSectionEverOpened
          ? html`
              <div class="by-project-body">
                <scion-project-picker
                  hoist
                  label="Project"
                  .value=${this._selectedProjectId}
                  @project-change=${(e: CustomEvent<ProjectChangeDetail>): void =>
                    this.handleProjectChange(e)}
                ></scion-project-picker>

                ${this._projectLoading
                  ? html`
                      <div class="loading-state">
                        <sl-spinner></sl-spinner>
                        Loading project permissions...
                      </div>
                    `
                  : this._projectError
                    ? html`
                        <div class="error-state" role="alert">
                          <span>${this._projectError}</span>
                          <sl-button
                            size="small"
                            @click=${(): Promise<void> =>
                              this.loadProjectPermissions(this._selectedProjectId)}
                            >Retry</sl-button
                          >
                        </div>
                      `
                    : this._projectData
                      ? html`
                          <div class="project-permissions-results">
                            <div class="project-results-header">
                              <h4 class="project-results-title">
                                <sl-icon name="folder"></sl-icon>
                                ${this._projectData.scope.name || this._selectedProjectId}
                                <span class="perm-total"
                                  >(${this._projectData.permissions.length})</span
                                >
                              </h4>
                              ${this._projectData.permissions.length > 0
                                ? html`
                                    <sl-input
                                      class="filter-input"
                                      size="small"
                                      clearable
                                      placeholder="Filter project permissions..."
                                      .value=${this._projectFilter}
                                      @sl-input=${(e: Event): void => {
                                        this._projectFilter =
                                          (e.target as HTMLInputElement).value ?? '';
                                      }}
                                      @sl-clear=${(): void => {
                                        this._projectFilter = '';
                                      }}
                                    >
                                      <sl-icon slot="prefix" name="search"></sl-icon>
                                    </sl-input>
                                  `
                                : nothing}
                            </div>
                            ${renderPermissionList(this._projectData, {
                              filter: this._projectFilter,
                            })}
                          </div>
                        `
                      : nothing}
              </div>
            `
          : nothing}
      </sl-details>
    `;
  }

  override render(): TemplateResult | typeof nothing {
    if (
      !this.active &&
      !this._data &&
      !this._loading &&
      !this._error &&
      !this._permissionsChecked
    ) {
      return nothing;
    }
    if (this._permissionsChecked && !this._canRead && !this._loading) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield-lock"></sl-icon>
          <h3>Permission Required</h3>
          <p>You do not have permission to view effective permissions.</p>
        </div>
      `;
    }

    if (this._loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          Loading effective permissions...
        </div>
      `;
    }

    if (this._error) {
      return html`
        <div class="error-state" role="alert">
          <span>${this._error}</span>
          <sl-button size="small" @click=${(): Promise<void> => this.loadHubPermissions()}
            >Retry</sl-button
          >
        </div>
      `;
    }

    if (!this._data) {
      return nothing;
    }

    return html`
      <div class="hub-section-header">
        <h3>
          Hub permissions
          <span class="perm-total">(${this._data.permissions.length})</span>
        </h3>
        ${this._data.permissions.length > 0
          ? html`
              <sl-input
                class="filter-input"
                size="small"
                clearable
                placeholder="Filter by permission ID..."
                .value=${this._filter}
                @sl-input=${(e: Event): void => {
                  this._filter = (e.target as HTMLInputElement).value ?? '';
                }}
                @sl-clear=${(): void => {
                  this._filter = '';
                }}
              >
                <sl-icon slot="prefix" name="search"></sl-icon>
              </sl-input>
            `
          : nothing}
      </div>
      ${renderPermissionList(this._data, { filter: this._filter })} ${this.renderByProjectSection()}
      <p class="ownership-footnote">
        <sl-icon name="info-circle"></sl-icon>
        <span
          >Some permissions (for example attaching to your own agents) are granted per resource by
          ownership and are not listed here.</span
        >
      </p>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-permissions-view': ScionEffectivePermissionsView;
  }
}
