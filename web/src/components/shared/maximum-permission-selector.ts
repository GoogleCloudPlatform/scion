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
 * Maximum Permission Selector (Step 4)
 *
 * The most complex sub-component of the access boundary editor.
 *
 * - Fetches the permission registry from the backend
 * - Groups permissions by resource family
 * - Search/filter across permission ID and description
 * - Each permission shows: ID, description, resource family, retained/removed state
 * - Filter views: All / Retained / Removed
 * - Per-group bulk actions: "Select visible" / "Clear group" (scoped to current group)
 * - NO initial "select all" default — user must make deliberate selections
 * - NO global "select every permission" one-click action
 * - Sticky summary: total registry count, retained count, removed count, newly registered
 * - Keyboard operable (tab through permissions, space/enter to toggle)
 * - Long IDs: wrap or truncate with accessible tooltip + copy action
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';
import { classMap } from 'lit/directives/class-map.js';

import { apiFetch } from '../../client/api.js';
import type { PermissionId } from '../../shared/access-boundaries.js';

export interface PermissionChangeDetail {
  retainedPermissions: PermissionId[];
  totalCount: number;
}

/** A single permission as fetched from the registry. */
interface RegistryPermission {
  id: PermissionId;
  description: string;
  resourceFamily: string;
}

type FilterView = 'all' | 'retained' | 'removed';

@customElement('scion-maximum-permission-selector')
export class ScionMaximumPermissionSelector extends LitElement {
  /** Currently retained permission IDs. */
  @property({ type: Array }) retainedPermissions: PermissionId[] = [];

  /** Permission IDs that are newly registered since the last revision (edit mode). */
  @property({ type: Array }) newSincePermissionIds: PermissionId[] = [];

  @state() private registryPermissions: RegistryPermission[] = [];
  @state() private loading = true;
  @state() private loadError = '';
  @state() private searchQuery = '';
  @state() private filterView: FilterView = 'all';
  @state() private collapsedGroups: Set<string> = new Set();
  @state() private copiedId: string | null = null;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadRegistry();
  }

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .permission-selector {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .controls {
        display: flex;
        gap: 0.75rem;
        align-items: center;
        flex-wrap: wrap;
      }

      .search-input {
        flex: 1;
        min-width: 200px;
      }

      .filter-tabs {
        display: flex;
        gap: 0.25rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        padding: 0.125rem;
      }

      .filter-tab {
        padding: 0.375rem 0.75rem;
        min-height: 44px;
        font-size: 0.8125rem;
        font-weight: 500;
        border: none;
        background: none;
        border-radius: calc(var(--scion-radius, 0.5rem) - 0.125rem);
        cursor: pointer;
        color: var(--scion-text-muted, #64748b);
        transition: all 0.15s ease;
        font-family: inherit;
        white-space: nowrap;
      }

      .filter-tab:hover {
        color: var(--scion-text, #1e293b);
      }

      .filter-tab.active {
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text, #1e293b);
        box-shadow: 0 1px 2px rgba(0, 0, 0, 0.05);
      }

      .sticky-summary {
        position: sticky;
        top: 0;
        z-index: 10;
        display: flex;
        gap: 1rem;
        align-items: center;
        flex-wrap: wrap;
        padding: 0.75rem 1rem;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
      }

      .summary-item {
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .summary-label {
        color: var(--scion-text-muted, #64748b);
      }

      .summary-value {
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .summary-value.retained {
        color: var(--sl-color-success-600, #16a34a);
      }

      .summary-value.removed {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .summary-value.new-since {
        color: var(--sl-color-warning-600, #ca8a04);
      }

      .permission-groups {
        display: flex;
        flex-direction: column;
        gap: 0.75rem;
      }

      .permission-group {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        overflow: hidden;
      }

      .group-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: 0.625rem 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        cursor: pointer;
        user-select: none;
      }

      .group-header:hover {
        background: var(--sl-color-neutral-100, #e7edf3);
      }

      .group-header-left {
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .group-chevron {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        transition: transform 0.15s ease;
      }

      .group-chevron.collapsed {
        transform: rotate(-90deg);
      }

      .group-name {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .group-count {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-weight: 400;
      }

      .group-actions {
        display: flex;
        gap: 0.25rem;
      }

      .group-action-btn {
        font-size: 0.75rem;
        padding: 0.125rem 0.5rem;
        min-height: 44px;
        min-width: 44px;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--scion-surface, #ffffff);
        cursor: pointer;
        color: var(--scion-text-muted, #64748b);
        font-family: inherit;
        transition: all 0.15s ease;
      }

      .group-action-btn:hover {
        border-color: var(--sl-color-primary-300, #93c5fd);
        color: var(--sl-color-primary-600, #2563eb);
      }

      .group-permissions {
        display: flex;
        flex-direction: column;
      }

      .permission-row {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        padding: 0.5rem 1rem;
        min-height: 44px;
        border-bottom: 1px solid var(--scion-border-light, #f1f5f9);
        transition: background-color 0.1s ease;
      }

      .permission-row:last-child {
        border-bottom: none;
      }

      .permission-row:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .permission-row:focus-visible {
        outline: 2px solid var(--sl-color-primary-600, #2563eb);
        outline-offset: -2px;
      }

      .permission-toggle {
        flex-shrink: 0;
      }

      .permission-info {
        flex: 1;
        min-width: 0;
      }

      .permission-id-row {
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .permission-id {
        font-size: 0.8125rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        font-family: var(--sl-font-mono, monospace);
        word-break: break-all;
        overflow-wrap: anywhere;
      }

      .permission-new-badge {
        flex-shrink: 0;
      }

      .permission-copy-btn {
        flex-shrink: 0;
        opacity: 0;
        transition: opacity 0.15s ease;
      }

      .permission-row:hover .permission-copy-btn {
        opacity: 1;
      }

      .permission-description {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
        line-height: 1.4;
      }

      .permission-status {
        flex-shrink: 0;
        font-size: 0.75rem;
        font-weight: 500;
      }

      .permission-status.retained {
        color: var(--sl-color-success-600, #16a34a);
      }

      .permission-status.removed {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .loading-state,
      .error-state,
      .empty-state {
        text-align: center;
        padding: 3rem 2rem;
        color: var(--scion-text-muted, #64748b);
      }

      .error-state {
        color: var(--sl-color-danger-600, #dc2626);
      }

      @media (max-width: 768px) {
        .controls {
          flex-direction: column;
          align-items: stretch;
        }

        .search-input {
          min-width: 0;
          width: 100%;
        }

        .filter-tabs {
          justify-content: center;
        }

        .sticky-summary {
          flex-direction: column;
          gap: 0.5rem;
          align-items: flex-start;
        }

        .permission-row {
          flex-wrap: wrap;
        }

        .permission-info {
          flex-basis: 100%;
          order: 2;
          padding-left: 2rem;
        }

        .group-header {
          flex-wrap: wrap;
          gap: 0.5rem;
        }

        .group-actions {
          width: 100%;
          justify-content: flex-end;
        }
      }

      @media (forced-colors: active) {
        .filter-tab.active {
          border: 2px solid Highlight;
        }

        .permission-group {
          border: 2px solid ButtonText;
        }

        .group-header {
          border-bottom: 2px solid ButtonText;
        }

        .permission-row:focus-visible {
          outline: 2px solid Highlight;
        }

        .group-action-btn {
          border: 1px solid ButtonText;
        }

        .group-action-btn:hover {
          border-color: Highlight;
        }

        .permission-status.retained {
          color: ButtonText;
        }

        .permission-status.removed {
          color: ButtonText;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .filter-tab,
        .group-action-btn,
        .permission-row,
        .permission-copy-btn,
        .group-chevron {
          transition: none;
        }
      }
    `,
  ];

  private async loadRegistry(): Promise<void> {
    this.loading = true;
    this.loadError = '';
    try {
      const response = await apiFetch('/api/v1/admin/permissions');
      if (!response.ok) {
        throw new Error(`Failed to load permission registry: ${response.statusText}`);
      }
      const data = (await response.json()) as {
        permissions?: Array<{
          id: string;
          description?: string;
          resourceFamily?: string;
        }>;
      };
      this.registryPermissions = (data.permissions || []).map((p) => ({
        id: p.id,
        description: p.description || '',
        resourceFamily: p.resourceFamily || 'Other',
      }));
    } catch (err) {
      this.loadError = err instanceof Error ? err.message : 'Failed to load permissions';
      console.error('Failed to load permission registry:', err);
    } finally {
      this.loading = false;
    }
  }

  private isRetained(permissionId: PermissionId): boolean {
    return this.retainedPermissions.includes(permissionId);
  }

  private togglePermission(permissionId: PermissionId): void {
    if (this.isRetained(permissionId)) {
      this.retainedPermissions = this.retainedPermissions.filter((id) => id !== permissionId);
    } else {
      this.retainedPermissions = [...this.retainedPermissions, permissionId];
    }
    this.emitChange();
  }

  private selectGroupVisible(groupPermissions: RegistryPermission[]): void {
    const filteredIds = this.applyFiltersToPermissions(groupPermissions).map((p) => p.id);
    const newRetained = new Set(this.retainedPermissions);
    for (const id of filteredIds) {
      newRetained.add(id);
    }
    this.retainedPermissions = [...newRetained];
    this.emitChange();
  }

  private clearGroup(groupPermissions: RegistryPermission[]): void {
    const groupIds = new Set(groupPermissions.map((p) => p.id));
    this.retainedPermissions = this.retainedPermissions.filter((id) => !groupIds.has(id));
    this.emitChange();
  }

  private emitChange(): void {
    this.dispatchEvent(
      new CustomEvent<PermissionChangeDetail>('permission-change', {
        detail: {
          retainedPermissions: [...this.retainedPermissions],
          totalCount: this.registryPermissions.length,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private async copyPermissionId(permissionId: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(permissionId);
      this.copiedId = permissionId;
      setTimeout(() => {
        this.copiedId = null;
      }, 2000);
    } catch {
      // Clipboard not available — silently fail
    }
  }

  private toggleGroupCollapse(groupName: string): void {
    const updated = new Set(this.collapsedGroups);
    if (updated.has(groupName)) {
      updated.delete(groupName);
    } else {
      updated.add(groupName);
    }
    this.collapsedGroups = updated;
  }

  private applyFiltersToPermissions(permissions: RegistryPermission[]): RegistryPermission[] {
    let filtered = permissions;

    // Text search
    if (this.searchQuery) {
      const q = this.searchQuery.toLowerCase();
      filtered = filtered.filter(
        (p) => p.id.toLowerCase().includes(q) || p.description.toLowerCase().includes(q)
      );
    }

    // Retained/removed filter
    if (this.filterView === 'retained') {
      filtered = filtered.filter((p) => this.isRetained(p.id));
    } else if (this.filterView === 'removed') {
      filtered = filtered.filter((p) => !this.isRetained(p.id));
    }

    return filtered;
  }

  private getGroupedPermissions(): Map<string, RegistryPermission[]> {
    const groups = new Map<string, RegistryPermission[]>();
    for (const perm of this.registryPermissions) {
      const family = perm.resourceFamily || 'Other';
      const existing = groups.get(family);
      if (existing) {
        existing.push(perm);
      } else {
        groups.set(family, [perm]);
      }
    }
    return groups;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading permission registry...</p>
        </div>
      `;
    }

    if (this.loadError) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-circle" style="font-size: 2rem"></sl-icon>
          <p>${this.loadError}</p>
          <sl-button variant="default" size="small" @click=${() => void this.loadRegistry()}>
            Retry
          </sl-button>
        </div>
      `;
    }

    const totalCount = this.registryPermissions.length;
    const retainedCount = this.retainedPermissions.length;
    const removedCount = totalCount - retainedCount;
    const newSinceCount = this.newSincePermissionIds.length;
    const groups = this.getGroupedPermissions();

    return html`
      <div class="permission-selector">
        <div class="controls">
          <sl-input
            class="search-input"
            placeholder="Search permissions..."
            size="small"
            clearable
            value=${this.searchQuery}
            @sl-input=${(e: Event) => {
              this.searchQuery = (e.target as HTMLInputElement).value;
            }}
            @sl-clear=${() => {
              this.searchQuery = '';
            }}
          >
            <sl-icon name="search" slot="prefix"></sl-icon>
          </sl-input>

          <div class="filter-tabs" role="tablist" aria-label="Filter permissions">
            ${(['all', 'retained', 'removed'] as FilterView[]).map(
              (view) => html`
                <button
                  id="perm-tab-${view}"
                  class=${classMap({ 'filter-tab': true, active: this.filterView === view })}
                  role="tab"
                  tabindex=${this.filterView === view ? '0' : '-1'}
                  aria-selected=${this.filterView === view ? 'true' : 'false'}
                  aria-controls="perm-filter-panel"
                  @click=${() => {
                    this.filterView = view;
                  }}
                  @keydown=${(e: KeyboardEvent) => {
                    const views: FilterView[] = ['all', 'retained', 'removed'];
                    const idx = views.indexOf(view);
                    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') {
                      e.preventDefault();
                      const next = views[(idx + 1) % views.length];
                      this.filterView = next;
                      requestAnimationFrame(() => {
                        const tabs = this.renderRoot.querySelectorAll('[role="tab"]');
                        (tabs[(idx + 1) % views.length] as HTMLElement)?.focus();
                      });
                    } else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
                      e.preventDefault();
                      const prev = views[(idx - 1 + views.length) % views.length];
                      this.filterView = prev;
                      requestAnimationFrame(() => {
                        const tabs = this.renderRoot.querySelectorAll('[role="tab"]');
                        (tabs[(idx - 1 + views.length) % views.length] as HTMLElement)?.focus();
                      });
                    }
                  }}
                >
                  ${view === 'all' ? 'All' : view === 'retained' ? 'Retained' : 'Removed'}
                </button>
              `
            )}
          </div>
        </div>

        <div id="perm-filter-panel" role="tabpanel" aria-labelledby="perm-tab-${this.filterView}">
          <div class="sticky-summary">
            <div class="summary-item">
              <span class="summary-label">Registry:</span>
              <span class="summary-value">${totalCount}</span>
            </div>
            <div class="summary-item">
              <span class="summary-label">Retained:</span>
              <span class="summary-value retained">${retainedCount}</span>
            </div>
            <div class="summary-item">
              <span class="summary-label">Removed:</span>
              <span class="summary-value removed">${removedCount}</span>
            </div>
            ${newSinceCount > 0
              ? html`
                  <div class="summary-item">
                    <sl-tooltip
                      content="Permissions registered since last revision — removed by default"
                    >
                      <span class="summary-label">New since last edit:</span>
                      <span class="summary-value new-since">${newSinceCount}</span>
                    </sl-tooltip>
                  </div>
                `
              : nothing}
          </div>

          <div class="permission-groups">
            ${[...groups.entries()].map(([groupName, groupPermissions]) => {
              const filteredPermissions = this.applyFiltersToPermissions(groupPermissions);
              const isCollapsed = this.collapsedGroups.has(groupName);

              if (filteredPermissions.length === 0 && this.searchQuery) {
                return nothing;
              }

              const groupRetainedCount = groupPermissions.filter((p) =>
                this.isRetained(p.id)
              ).length;

              return html`
                <div class="permission-group">
                  <div
                    class="group-header"
                    @click=${() => this.toggleGroupCollapse(groupName)}
                    @keydown=${(e: KeyboardEvent) => {
                      if (e.key === 'Enter' || e.key === ' ') {
                        e.preventDefault();
                        this.toggleGroupCollapse(groupName);
                      }
                    }}
                    tabindex="0"
                    role="button"
                    aria-expanded=${!isCollapsed ? 'true' : 'false'}
                  >
                    <div class="group-header-left">
                      <sl-icon
                        class=${classMap({ 'group-chevron': true, collapsed: isCollapsed })}
                        name="chevron-down"
                      ></sl-icon>
                      <span class="group-name">${groupName}</span>
                      <span class="group-count">
                        (${groupRetainedCount}/${groupPermissions.length} retained)
                      </span>
                    </div>
                    <div class="group-actions" @click=${(e: Event) => e.stopPropagation()}>
                      <button
                        class="group-action-btn"
                        @click=${() => this.selectGroupVisible(groupPermissions)}
                        title="Select visible permissions in this group"
                      >
                        Select visible
                      </button>
                      <button
                        class="group-action-btn"
                        @click=${() => this.clearGroup(groupPermissions)}
                        title="Clear all permissions in this group"
                      >
                        Clear group
                      </button>
                    </div>
                  </div>
                  ${!isCollapsed
                    ? html`
                        <div class="group-permissions">
                          ${filteredPermissions.length === 0
                            ? html`
                                <div
                                  class="permission-row"
                                  style="justify-content: center; color: var(--scion-text-muted)"
                                >
                                  No permissions match the current filter
                                </div>
                              `
                            : filteredPermissions.map((perm) => this.renderPermissionRow(perm))}
                        </div>
                      `
                    : nothing}
                </div>
              `;
            })}
          </div>
        </div>
      </div>
    `;
  }

  private renderPermissionRow(perm: RegistryPermission) {
    const retained = this.isRetained(perm.id);
    const isNew = this.newSincePermissionIds.includes(perm.id);

    return html`
      <div
        class="permission-row"
        tabindex="0"
        role="checkbox"
        aria-checked=${retained ? 'true' : 'false'}
        aria-label="${perm.id}: ${perm.description}"
        @keydown=${(e: KeyboardEvent) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            this.togglePermission(perm.id);
          }
        }}
      >
        <sl-checkbox
          class="permission-toggle"
          ?checked=${retained}
          @sl-change=${() => this.togglePermission(perm.id)}
        ></sl-checkbox>
        <div class="permission-info">
          <div class="permission-id-row">
            <sl-tooltip content=${perm.id} placement="top">
              <span class="permission-id">${perm.id}</span>
            </sl-tooltip>
            ${isNew
              ? html` <sl-badge variant="warning" class="permission-new-badge" pill>new</sl-badge> `
              : nothing}
            <sl-icon-button
              class="permission-copy-btn"
              name=${this.copiedId === perm.id ? 'clipboard-check' : 'clipboard'}
              label="Copy permission ID"
              style="font-size: 0.75rem"
              @click=${(e: Event) => {
                e.stopPropagation();
                void this.copyPermissionId(perm.id);
              }}
            ></sl-icon-button>
          </div>
          ${perm.description
            ? html`<div class="permission-description">${perm.description}</div>`
            : nothing}
        </div>
        <span class=${classMap({ 'permission-status': true, retained, removed: !retained })}>
          ${retained ? 'Retained' : 'Removed'}
        </span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-maximum-permission-selector': ScionMaximumPermissionSelector;
  }
}
