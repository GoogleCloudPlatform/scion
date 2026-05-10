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
 * Admin Groups page component
 *
 * Read-only view of all groups in the system
 */

import { LitElement, html, css } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { AdminGroup } from '../../shared/types.js';
import { extractApiError } from '../../client/api.js';

@customElement('scion-page-admin-groups')
export class ScionPageAdminGroups extends LitElement {
  @state()
  private loading = true;

  @state()
  private groups: AdminGroup[] = [];

  @state()
  private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .group-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    tr.clickable {
      cursor: pointer;
    }

    tr.clickable:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .group-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .group-icon {
      width: 2rem;
      height: 2rem;
      border-radius: 0.5rem;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .group-icon.explicit {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .group-icon.project_agents {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .group-icon sl-icon {
      font-size: 1rem;
    }

    .group-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .group-name {
      font-weight: 500;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .group-slug {
      font-size: 0.75rem;
      font-family: var(--scion-font-mono, monospace);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .type-badge.explicit {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .type-badge.project_agents {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .description-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      max-width: 300px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .labels-container {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
    }

    .label-tag {
      display: inline-flex;
      align-items: center;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.6875rem;
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }

    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadGroups();
  }

  private async loadGroups(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch('/api/v1/groups', {
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { groups?: AdminGroup[] } | AdminGroup[];
      this.groups = Array.isArray(data) ? data : data.groups || [];
    } catch (err) {
      console.error('Failed to load groups:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load groups';
    } finally {
      this.loading = false;
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
  }

  override render() {
    return html`
      <div class="header">
        <h1>Groups</h1>
        ${!this.loading && !this.error
          ? html`<span class="group-count"
              >${this.groups.length} group${this.groups.length !== 1 ? 's' : ''}</span
            >`
          : ''}
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderGroups()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading groups...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Groups</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadGroups()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderGroups() {
    if (this.groups.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="diagram-3"></sl-icon>
          <h2>No Groups Found</h2>
          <p>There are no groups configured in the system.</p>
        </div>
      `;
    }

    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Group</th>
              <th>Type</th>
              <th class="hide-mobile">Description</th>
              <th class="hide-mobile">Labels</th>
              <th class="hide-mobile">Updated</th>
            </tr>
          </thead>
          <tbody>
            ${this.groups.map((group) => this.renderGroupRow(group))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderGroupRow(group: AdminGroup) {
    const labels = group.labels ? Object.entries(group.labels) : [];

    return html`
      <tr class="clickable" @click=${() => {
        window.history.pushState({}, '', `/admin/groups/${group.id}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }}>
        <td>
          <div class="group-identity">
            <div class="group-icon ${group.groupType}">
              <sl-icon name="${group.groupType === 'project_agents' ? 'cpu' : 'people'}"></sl-icon>
            </div>
            <div class="group-info">
              <span class="group-name">${group.name}</span>
              <span class="group-slug">${group.slug}</span>
            </div>
          </div>
        </td>
        <td>
          <span class="type-badge ${group.groupType}">
            ${group.groupType === 'project_agents' ? 'project agents' : 'explicit'}
          </span>
        </td>
        <td class="hide-mobile">
          <span class="description-text">${group.description || '\u2014'}</span>
        </td>
        <td class="hide-mobile">
          ${labels.length > 0
            ? html`
                <div class="labels-container">
                  ${labels
                    .slice(0, 3)
                    .map(([key, value]) => html`<span class="label-tag">${key}=${value}</span>`)}
                  ${labels.length > 3
                    ? html`<span class="label-tag">+${labels.length - 3}</span>`
                    : ''}
                </div>
              `
            : html`<span class="meta-text">—</span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(group.updated)}</span>
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-groups': ScionPageAdminGroups;
  }
}
