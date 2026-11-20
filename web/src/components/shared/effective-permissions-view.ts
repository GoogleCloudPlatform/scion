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

/**
 * Pure presentational helper rendering an `EffectivePermissionSet` grouped by
 * resource with origin chips, binding ID tooltips, and expiry badges.
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

  if (data.permissions.length === 0) {
    return html`
      <div class="empty-state">
        <sl-icon name="shield"></sl-icon>
        <h3>No Effective Permissions</h3>
        <p>This user holds no active permissions at this scope.</p>
      </div>
    `;
  }

  const groups = groupPermissionsByResource(data.permissions, filter);
  if (groups.length === 0) {
    return html`
      <div class="empty-state compact-empty">
        <p>No permissions match "${filter.trim()}".</p>
      </div>
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
                      <code class="perm-id">${perm.id}</code>
                      ${perm.description
                        ? html`<span class="perm-desc">${perm.description}</span>`
                        : nothing}
                    </div>
                    <div class="perm-grants">
                      ${perm.grants.map((g) =>
                        renderGrantChip(g, onCopy, data.scope.type === 'project')
                      )}
                    </div>
                  </div>
                `
              )}
            </div>
          </section>
        `
      )}
    </div>
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

    @media (max-width: 768px) {
      .permission-row {
        flex-direction: column;
      }

      .perm-grants {
        justify-content: flex-start;
      }
    }

    @media (forced-colors: active) {
      .resource-group,
      .origin-chip,
      .expires-badge,
      .error-state,
      .by-project-details::part(base) {
        border: 1px solid ButtonText;
      }
    }
  `;

  /** Clears cached permission data and reloads if currently visible. */
  public invalidate(): void {
    this._loadedPrincipalId = null;
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
      !this._loading
    ) {
      void this.loadHubPermissions();
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
      this._canRead = hasAnyPermission(status, ['role_binding.read']);
      this._permissionsChecked = status !== null;
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
    if (!this.active && !this._data && !this._loading && !this._error) {
      return nothing;
    }
    if (this._permissionsChecked && !this._canRead && !this._loading) {
      return nothing;
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
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-permissions-view': ScionEffectivePermissionsView;
  }
}
