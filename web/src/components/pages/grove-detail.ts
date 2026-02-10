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
 * Grove detail page component
 *
 * Displays a single grove with its agents and settings
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Grove, Agent } from '../../shared/types.js';
import '../shared/status-badge.js';

@customElement('scion-page-grove-detail')
export class ScionPageGroveDetail extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Grove ID from URL
   */
  @property({ type: String })
  groveId = '';

  /**
   * Loading state
   */
  @state()
  private loading = true;

  /**
   * Grove data
   */
  @state()
  private grove: Grove | null = null;

  /**
   * Agents in this grove
   */
  @state()
  private agents: Agent[] = [];

  /**
   * Error message if loading failed
   */
  @state()
  private error: string | null = null;

  /**
   * Loading state for actions
   */
  @state()
  private actionLoading: Record<string, boolean> = {};

  static override styles = css`
    :host {
      display: block;
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
      margin-bottom: 0.5rem;
    }

    .header-title sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-path {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
      word-break: break-all;
    }

    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .stats-row {
      display: flex;
      gap: 2rem;
      margin-bottom: 2rem;
      padding: 1.25rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
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
      margin-bottom: 0.25rem;
    }

    .stat-value {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
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

    .agent-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
      gap: 1.5rem;
    }

    .agent-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      transition: all var(--scion-transition-fast, 150ms ease);
      text-decoration: none;
      color: inherit;
      display: block;
    }

    .agent-card:hover {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
    }

    .agent-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 0.75rem;
    }

    .agent-name {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .agent-name sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .agent-template {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .agent-task {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      margin-top: 0.75rem;
      padding: 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .agent-actions {
      display: flex;
      gap: 0.5rem;
      margin-top: 1rem;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      // Load grove and agents in parallel
      const [groveResponse, agentsResponse] = await Promise.all([
        fetch(`/api/groves/${this.groveId}`, { credentials: 'include' }),
        fetch(`/api/groves/${this.groveId}/agents`, { credentials: 'include' }),
      ]);

      if (!groveResponse.ok) {
        const errorData = (await groveResponse.json().catch(() => ({}))) as { message?: string };
        throw new Error(
          errorData.message || `HTTP ${groveResponse.status}: ${groveResponse.statusText}`
        );
      }

      this.grove = (await groveResponse.json()) as Grove;

      if (agentsResponse.ok) {
        const agentsData = (await agentsResponse.json()) as { agents?: Agent[] } | Agent[];
        this.agents = Array.isArray(agentsData) ? agentsData : agentsData.agents || [];
      } else {
        // Fallback: if grove-scoped agents endpoint fails, try filtering from all agents
        this.agents = [];
      }
    } catch (err) {
      console.error('Failed to load grove:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load grove';
    } finally {
      this.loading = false;
    }
  }

  private getStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'active':
      case 'running':
        return 'success';
      case 'inactive':
      case 'stopped':
        return 'neutral';
      case 'provisioning':
        return 'warning';
      case 'error':
        return 'danger';
      default:
        return 'neutral';
    }
  }

  private formatDate(dateString: string): string {
    try {
      const date = new Date(dateString);
      return new Intl.DateTimeFormat('en', {
        month: 'short',
        day: 'numeric',
        year: 'numeric',
        hour: '2-digit',
        minute: '2-digit',
      }).format(date);
    } catch {
      return dateString;
    }
  }

  private async handleAgentAction(
    agentId: string,
    action: 'start' | 'stop' | 'delete'
  ): Promise<void> {
    this.actionLoading = { ...this.actionLoading, [agentId]: true };

    try {
      let response: Response;

      switch (action) {
        case 'start':
          response = await fetch(`/api/agents/${agentId}/start`, {
            method: 'POST',
            credentials: 'include',
          });
          break;
        case 'stop':
          response = await fetch(`/api/agents/${agentId}/stop`, {
            method: 'POST',
            credentials: 'include',
          });
          break;
        case 'delete':
          if (!confirm('Are you sure you want to delete this agent?')) {
            this.actionLoading = { ...this.actionLoading, [agentId]: false };
            return;
          }
          response = await fetch(`/api/agents/${agentId}`, {
            method: 'DELETE',
            credentials: 'include',
          });
          break;
      }

      if (!response.ok) {
        const errorData = (await response.json().catch(() => ({}))) as { message?: string };
        throw new Error(errorData.message || `Failed to ${action} agent`);
      }

      // Reload data to reflect changes
      await this.loadData();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      alert(err instanceof Error ? err.message : `Failed to ${action} agent`);
    } finally {
      this.actionLoading = { ...this.actionLoading, [agentId]: false };
    }
  }

  override render() {
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error) {
      return this.renderError();
    }

    if (!this.grove) {
      return this.renderError();
    }

    return html`
      <a href="/groves" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Groves
      </a>

      <div class="header">
        <div class="header-info">
          <div class="header-title">
            <sl-icon name="folder-fill"></sl-icon>
            <h1>${this.grove.name}</h1>
            <scion-status-badge
              status=${this.getStatusVariant(this.grove.status)}
              label=${this.grove.status}
              size="small"
            ></scion-status-badge>
          </div>
          <div class="header-path">${this.grove.path}</div>
        </div>
        <div class="header-actions">
          <sl-button variant="primary" size="small" disabled>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            New Agent
          </sl-button>
          <sl-button size="small" disabled>
            <sl-icon slot="prefix" name="gear"></sl-icon>
            Settings
          </sl-button>
        </div>
      </div>

      <div class="stats-row">
        <div class="stat">
          <span class="stat-label">Agents</span>
          <span class="stat-value">${this.agents.length}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Running</span>
          <span class="stat-value"
            >${this.agents.filter((a) => a.status === 'running').length}</span
          >
        </div>
        <div class="stat">
          <span class="stat-label">Created</span>
          <span class="stat-value" style="font-size: 1rem; font-weight: 500;">
            ${this.formatDate(this.grove.createdAt)}
          </span>
        </div>
        <div class="stat">
          <span class="stat-label">Updated</span>
          <span class="stat-value" style="font-size: 1rem; font-weight: 500;">
            ${this.formatDate(this.grove.updatedAt)}
          </span>
        </div>
      </div>

      <div class="section-header">
        <h2>Agents</h2>
      </div>

      ${this.agents.length === 0 ? this.renderEmptyAgents() : this.renderAgentGrid()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading grove...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/groves" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Groves
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Grove</h2>
        <p>There was a problem loading this grove.</p>
        <div class="error-details">${this.error || 'Grove not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderEmptyAgents() {
    return html`
      <div class="empty-state">
        <sl-icon name="cpu"></sl-icon>
        <h2>No Agents</h2>
        <p>This grove doesn't have any agents yet. Create your first agent to get started.</p>
        <sl-button variant="primary" disabled>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Agent
        </sl-button>
      </div>
    `;
  }

  private renderAgentGrid() {
    return html`
      <div class="agent-grid">${this.agents.map((agent) => this.renderAgentCard(agent))}</div>
    `;
  }

  private renderAgentCard(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;

    return html`
      <div class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="agent-name">
              <sl-icon name="cpu"></sl-icon>
              <a href="/agents/${agent.id}" style="color: inherit; text-decoration: none;">
                ${agent.name}
              </a>
            </h3>
            <div class="agent-template">${agent.template}</div>
          </div>
          <scion-status-badge
            status=${this.getStatusVariant(agent.status)}
            label=${agent.status}
            size="small"
          ></scion-status-badge>
        </div>

        ${agent.taskSummary ? html`<div class="agent-task">${agent.taskSummary}</div>` : ''}

        <div class="agent-actions">
          <sl-button
            variant="primary"
            size="small"
            href="/agents/${agent.id}/terminal"
            ?disabled=${agent.status !== 'running'}
          >
            <sl-icon slot="prefix" name="terminal"></sl-icon>
            Terminal
          </sl-button>
          ${agent.status === 'running'
            ? html`
                <sl-button
                  variant="danger"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'stop')}
                >
                  <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                  Stop
                </sl-button>
              `
            : html`
                <sl-button
                  variant="success"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'start')}
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  Start
                </sl-button>
              `}
          <sl-button
            variant="default"
            size="small"
            outline
            ?loading=${isLoading}
            ?disabled=${isLoading}
            @click=${() => this.handleAgentAction(agent.id, 'delete')}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
          </sl-button>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-grove-detail': ScionPageGroveDetail;
  }
}
