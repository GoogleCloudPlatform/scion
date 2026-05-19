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
 * Admin Group detail page component
 *
 * Shows group info and delegates member management to the shared
 * group-member-editor component.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { AdminGroup } from '../../shared/types.js';
import '../shared/group-member-editor.js';
import { extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';

@customElement('scion-page-admin-group-detail')
export class ScionPageAdminGroupDetail extends LitElement {
  @state()
  private groupId = '';

  @state()
  private loading = true;

  @state()
  private group: AdminGroup | null = null;

  @state()
  private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .header-info {
      flex: 1;
    }

    .header-title {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.25rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-slug {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .group-icon {
      width: 2.5rem;
      height: 2.5rem;
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
      font-size: 1.25rem;
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

    .details-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem;
      margin-bottom: 2rem;
    }

    .details-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1rem;
    }

    .detail-item {
      display: flex;
      flex-direction: column;
    }

    .detail-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
      margin-bottom: 0.25rem;
    }

    .detail-value {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .detail-value.mono {
      font-family: var(--scion-font-mono, monospace);
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
      .details-grid {
        grid-template-columns: 1fr 1fr;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/admin\/groups\/([^/]+)/);
      if (match) {
        this.groupId = match[1];
      }
    }
    void this.loadGroup();
  }

  private async loadGroup(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch(`/api/v1/groups/${encodeURIComponent(this.groupId)}`, {
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.group = (await response.json()) as AdminGroup;
      dispatchPageTitle(this, this.group.name || this.groupId, 'Groups');
    } catch (err) {
      console.error('Failed to load group:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load group';
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
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error || !this.group) {
      return this.renderError();
    }

    const labels = this.group.labels ? Object.entries(this.group.labels) : [];

    return html`
      <a href="/admin/groups" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Groups
      </a>

      <div class="header">
        <div class="header-info">
          <div class="header-title">
            <div class="group-icon ${this.group.groupType}">
              <sl-icon name="${this.group.groupType === 'project_agents' ? 'cpu' : 'people'}"></sl-icon>
            </div>
            <h1>${this.group.name}</h1>
            <span class="type-badge ${this.group.groupType}">
              ${this.group.groupType === 'project_agents' ? 'project agents' : 'explicit'}
            </span>
          </div>
          <span class="header-slug">${this.group.slug}</span>
        </div>
      </div>

      <div class="details-card">
        <div class="details-grid">
          <div class="detail-item">
            <span class="detail-label">Description</span>
            <span class="detail-value">${this.group.description || '\u2014'}</span>
          </div>
          <div class="detail-item">
            <span class="detail-label">Owner</span>
            <span class="detail-value mono">${this.group.ownerId || '\u2014'}</span>
          </div>
          <div class="detail-item">
            <span class="detail-label">Created</span>
            <span class="detail-value">${this.formatRelativeTime(this.group.created)}</span>
          </div>
          <div class="detail-item">
            <span class="detail-label">Updated</span>
            <span class="detail-value">${this.formatRelativeTime(this.group.updated)}</span>
          </div>
          ${labels.length > 0
            ? html`
                <div class="detail-item">
                  <span class="detail-label">Labels</span>
                  <div class="labels-container">
                    ${labels.map(
                      ([key, value]) => html`<span class="label-tag">${key}=${value}</span>`
                    )}
                  </div>
                </div>
              `
            : nothing}
          ${this.group.projectId
            ? html`
                <div class="detail-item">
                  <span class="detail-label">Project</span>
                  <span class="detail-value mono">${this.group.projectId}</span>
                </div>
              `
            : nothing}
        </div>
      </div>

      <scion-group-member-editor
        groupId=${this.group.id}
        ?readOnly=${this.group.groupType === 'project_agents'}
      ></scion-group-member-editor>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading group...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/admin/groups" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Groups
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Group</h2>
        <p>There was a problem loading this group.</p>
        <div class="error-details">${this.error || 'Group not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadGroup()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-group-detail': ScionPageAdminGroupDetail;
  }
}
