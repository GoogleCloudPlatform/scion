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
 * Groves list page component
 *
 * Displays all groves (project workspaces) with their status and agent counts
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Grove } from '../../shared/types.js';
import '../shared/status-badge.js';

@customElement('scion-page-groves')
export class ScionPageGroves extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Loading state
   */
  @state()
  private loading = true;

  /**
   * Groves list
   */
  @state()
  private groves: Grove[] = [];

  /**
   * Error message if loading failed
   */
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

    .grove-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
      gap: 1.5rem;
    }

    .grove-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      transition: all var(--scion-transition-fast, 150ms ease);
      cursor: pointer;
      text-decoration: none;
      color: inherit;
      display: block;
    }

    .grove-card:hover {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
      transform: translateY(-2px);
    }

    .grove-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .grove-name {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .grove-name sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .grove-path {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
      font-family: var(--scion-font-mono, monospace);
      word-break: break-all;
    }

    .grove-stats {
      display: flex;
      gap: 1.5rem;
      margin-top: 1rem;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .stat {
      display: flex;
      flex-direction: column;
    }

    .stat-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .stat-value {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state sl-icon {
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
      margin: 0 0 1.5rem 0;
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadGroves();
  }

  private async loadGroves(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch('/api/groves', {
        credentials: 'include',
      });

      if (!response.ok) {
        const errorData = (await response.json().catch(() => ({}))) as { message?: string };
        throw new Error(errorData.message || `HTTP ${response.status}: ${response.statusText}`);
      }

      const data = (await response.json()) as { groves?: Grove[] } | Grove[];
      this.groves = Array.isArray(data) ? data : data.groves || [];
    } catch (err) {
      console.error('Failed to load groves:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load groves';
    } finally {
      this.loading = false;
    }
  }

  private getStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'active':
        return 'success';
      case 'inactive':
        return 'neutral';
      case 'error':
        return 'danger';
      default:
        return 'neutral';
    }
  }

  private formatDate(dateString: string): string {
    try {
      const date = new Date(dateString);
      return new Intl.RelativeTimeFormat('en', { numeric: 'auto' }).format(
        Math.round((date.getTime() - Date.now()) / (1000 * 60 * 60 * 24)),
        'day'
      );
    } catch {
      return dateString;
    }
  }

  override render() {
    return html`
      <div class="header">
        <h1>Groves</h1>
        <sl-button variant="primary" size="small" disabled>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          New Grove
        </sl-button>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderGroves()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading groves...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Groves</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadGroves()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderGroves() {
    if (this.groves.length === 0) {
      return this.renderEmptyState();
    }

    return html`
      <div class="grove-grid">${this.groves.map((grove) => this.renderGroveCard(grove))}</div>
    `;
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="folder2-open"></sl-icon>
        <h2>No Groves Found</h2>
        <p>
          Groves are project workspaces that contain your agents. Create your first grove to get
          started, or run
          <code>scion init</code> in a project directory.
        </p>
        <sl-button variant="primary" disabled>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Grove
        </sl-button>
      </div>
    `;
  }

  private renderGroveCard(grove: Grove) {
    return html`
      <a href="/groves/${grove.id}" class="grove-card">
        <div class="grove-header">
          <div>
            <h3 class="grove-name">
              <sl-icon name="folder-fill"></sl-icon>
              ${grove.name}
            </h3>
            <div class="grove-path">${grove.path}</div>
          </div>
          <scion-status-badge
            status=${this.getStatusVariant(grove.status)}
            label=${grove.status}
            size="small"
          >
          </scion-status-badge>
        </div>
        <div class="grove-stats">
          <div class="stat">
            <span class="stat-label">Agents</span>
            <span class="stat-value">${grove.agentCount}</span>
          </div>
          <div class="stat">
            <span class="stat-label">Updated</span>
            <span class="stat-value" style="font-size: 0.875rem; font-weight: 500;">
              ${this.formatDate(grove.updatedAt)}
            </span>
          </div>
        </div>
      </a>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-groves': ScionPageGroves;
  }
}
