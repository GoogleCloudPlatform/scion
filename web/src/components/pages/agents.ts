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
 * Agents list page component
 *
 * Displays all agents across all groves with their status
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Agent, Capabilities } from '../../shared/types.js';
import { can, isTerminalAvailable, getAgentDisplayStatus, isAgentRunning } from '../../shared/types.js';
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { listPageStyles } from '../shared/resource-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';

@customElement('scion-page-agents')
export class ScionPageAgents extends LitElement {
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
   * Agents list
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

  /**
   * Loading state for stop-all action
   */
  @state()
  private stopAllLoading = false;

  /**
   * Scope-level capabilities from the agents list response
   */
  @state()
  private scopeCapabilities: Capabilities | undefined;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  /**
   * Whether to show only agents in the user's groves or created by the user
   */
  @state()
  private showMineOnly = false;

  static override styles = [
    listPageStyles,
    css`
      .agent-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 0.75rem;
      }

      .agent-meta {
        font-size: 0.813rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }

      .agent-meta sl-icon {
        font-size: 0.875rem;
        vertical-align: -0.125em;
        opacity: 0.7;
      }

      .agent-meta .broker-link {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        color: var(--scion-text-muted, #64748b);
        text-decoration: none;
      }

      .agent-meta .broker-link:hover {
        color: var(--scion-primary, #3b82f6);
      }

      .agent-meta a {
        color: inherit;
        text-decoration: none;
      }

      .agent-meta a:hover {
        text-decoration: underline;
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

      /* Card-specific: no hover transform for agent cards (they have action buttons) */
      .agent-card {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1.5rem;
        transition: all var(--scion-transition-fast, 150ms ease);
      }

      .agent-card:hover {
        border-color: var(--scion-primary, #3b82f6);
        box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
      }

      /* Table-specific: inline action buttons */
      .table-actions {
        display: flex;
        gap: 0.375rem;
        justify-content: flex-end;
      }

      .filter-toggle {
        display: inline-flex;
      }

      .grove-link {
        color: inherit;
        text-decoration: none;
      }

      .grove-link:hover {
        text-decoration: underline;
      }

      .filter-toggle sl-button::part(base) {
        font-size: 0.8125rem;
      }
    `,
  ];

  private boundOnAgentsUpdated = this.onAgentsUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-agents') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    // Read persisted mine-only filter
    if (this.pageData?.user && localStorage.getItem('scion-filter-mine-agents') === 'true') {
      this.showMineOnly = true;
    }

    // Set SSE scope to dashboard (all grove summaries).
    // This must happen before checking hydrated data because setScope clears
    // state maps when the scope changes (e.g. from agent-detail to dashboard).
    stateManager.setScope({ type: 'dashboard' });

    // Use hydrated data from SSR if available, avoiding the initial fetch.
    // Only trust it when scope was previously null (initial SSR page load);
    // on client-side navigations the maps were just cleared by setScope above.
    // Skip hydrated data when mine-only filter is active — SSR data is unfiltered.
    const hydratedAgents = stateManager.getAgents();
    if (hydratedAgents.length > 0 && !this.showMineOnly) {
      this.agents = hydratedAgents;
      this.scopeCapabilities = stateManager.getScopeCapabilities();
      this.loading = false;
      stateManager.seedAgents(this.agents);
    } else {
      void this.loadAgents();
    }

    // Listen for real-time agent updates
    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
  }

  private onAgentsUpdated(): void {
    const updatedAgents = stateManager.getAgents();
    // Merge SSE agent deltas into local agent list
    const agentMap = new Map(this.agents.map((a) => [a.id, a]));
    for (const agent of updatedAgents) {
      const existing = agentMap.get(agent.id);
      // When "My Agents" filter is active, only update agents already in the
      // filtered list — don't add new agents that weren't in the REST response.
      // The server-side filter is the source of truth for ownership.
      if (!existing && this.showMineOnly) {
        continue;
      }
      const merged = { ...existing, ...agent } as Agent;
      // Preserve _capabilities from existing state when the delta lacks them.
      // For brand-new agents from SSE, inherit scope-level capabilities.
      if (!merged._capabilities) {
        if (existing?._capabilities) {
          merged._capabilities = existing._capabilities;
        } else if (this.scopeCapabilities) {
          merged._capabilities = this.scopeCapabilities;
        }
      }
      agentMap.set(agent.id, merged);
    }
    // Remove agents that were explicitly deleted via SSE
    const deletedIds = stateManager.getDeletedAgentIds();
    for (const id of deletedIds) {
      agentMap.delete(id);
    }
    this.agents = Array.from(agentMap.values());
  }

  private async loadAgents(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      await this.fetchAndMergeAgents();
    } catch (err) {
      console.error('Failed to load agents:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load agents';
    } finally {
      this.loading = false;
    }
  }

  private backgroundRefresh(): void {
    this.fetchAndMergeAgents().catch(err => {
      console.warn('Background refresh failed:', err);
    });
  }

  private async fetchAndMergeAgents(): Promise<void> {
    const url = this.showMineOnly ? '/api/v1/agents?mine=true' : '/api/v1/agents';
    const response = await apiFetch(url);

    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
    }

    const data = (await response.json()) as { agents?: Agent[]; _capabilities?: Capabilities } | Agent[];
    if (Array.isArray(data)) {
      this.agents = data;
      this.scopeCapabilities = undefined;
    } else {
      this.agents = data.agents || [];
      this.scopeCapabilities = data._capabilities;
    }
    stateManager.seedAgents(this.agents);
  }

  private async handleAgentAction(
    agentId: string,
    action: 'start' | 'stop' | 'delete',
    event?: MouseEvent
  ): Promise<void> {
    if (action === 'delete') {
      if (!event?.altKey && !confirm('Are you sure you want to delete this agent?')) {
        return;
      }
      // Show per-button spinner for delete; don't optimistically remove
      this.actionLoading = { ...this.actionLoading, [agentId]: true };
      this.requestUpdate();

      try {
        const response = await apiFetch(`/api/v1/agents/${agentId}`, {
          method: 'DELETE',
        });

        if (!response.ok) {
          throw new Error(await extractApiError(response, 'Failed to delete agent'));
        }

        // Server confirmed — remove from local list
        this.agents = this.agents.filter(a => a.id !== agentId);
        this.backgroundRefresh();
      } catch (err) {
        console.error('Failed to delete agent:', err);
        alert(err instanceof Error ? err.message : 'Failed to delete agent');
      } finally {
        this.actionLoading = { ...this.actionLoading, [agentId]: false };
      }
      return;
    }

    // Start/stop: apply optimistic phase update immediately
    const agentIndex = this.agents.findIndex(a => a.id === agentId);
    if (agentIndex >= 0) {
      const updated = { ...this.agents[agentIndex] };
      updated.phase = action === 'start' ? 'starting' : 'stopping';
      this.agents = [...this.agents];
      this.agents[agentIndex] = updated;
    }

    try {
      const url = action === 'start'
        ? `/api/v1/agents/${agentId}/start`
        : `/api/v1/agents/${agentId}/stop`;
      const response = await apiFetch(url, { method: 'POST' });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `Failed to ${action} agent`));
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error(`Failed to ${action} agent:`, err);
      alert(err instanceof Error ? err.message : `Failed to ${action} agent`);
      // Roll back optimistic update on failure
      this.backgroundRefresh();
    }
  }

  private hasRunningAgents(): boolean {
    return this.agents.some((a) => isAgentRunning(a));
  }

  private async handleStopAll(): Promise<void> {
    if (!confirm('Are you sure you want to stop all running agents?')) {
      return;
    }

    // Optimistic: mark all running agents as "stopping"
    this.agents = this.agents.map(a =>
      isAgentRunning(a) ? { ...a, phase: 'stopping' as const } : a
    );
    this.stopAllLoading = true;

    try {
      const response = await apiFetch('/api/v1/agents/stop-all', {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to stop all agents'));
      }

      const result = (await response.json()) as { stopped: number; failed: number };
      if (result.failed > 0) {
        alert(`Stopped ${result.stopped} agents, ${result.failed} failed.`);
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error('Failed to stop all agents:', err);
      alert(err instanceof Error ? err.message : 'Failed to stop all agents');
      this.backgroundRefresh();
    } finally {
      this.stopAllLoading = false;
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private toggleMineOnly(): void {
    this.showMineOnly = !this.showMineOnly;
    localStorage.setItem('scion-filter-mine-agents', String(this.showMineOnly));
    void this.loadAgents();
  }

  override render() {
    return html`
      <div class="header">
        <h1>Agents</h1>
        <div class="header-actions">
          ${this.pageData?.user ? html`
            <div class="filter-toggle">
              <sl-button
                size="small"
                variant=${this.showMineOnly ? 'primary' : 'default'}
                @click=${this.toggleMineOnly}
              >
                <sl-icon slot="prefix" name="person"></sl-icon>
                My Agents
              </sl-button>
            </div>
          ` : nothing}
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-agents"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'stop_all') && this.hasRunningAgents() ? html`
            <sl-button
              variant="danger"
              size="small"
              outline
              ?loading=${this.stopAllLoading}
              ?disabled=${this.stopAllLoading}
              @click=${() => this.handleStopAll()}
            >
              <sl-icon slot="prefix" name="stop-circle"></sl-icon>
              Stop All
            </sl-button>
          ` : nothing}
          ${can(this.scopeCapabilities, 'create') ? html`
            <a href="/agents/new" style="text-decoration: none;">
              <sl-button variant="primary" size="small">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                New Agent
              </sl-button>
            </a>
          ` : nothing}
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderAgents()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading agents...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Agents</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadAgents()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderAgents() {
    if (this.agents.length === 0) {
      if (this.showMineOnly) {
        return html`
          <div class="empty-state">
            <sl-icon name="person"></sl-icon>
            <h2>No Agents Found</h2>
            <p>You don't have any agents in your groves yet.</p>
          </div>
        `;
      }
      return this.renderEmptyState();
    }

    return this.viewMode === 'grid' ? this.renderGrid() : this.renderTable();
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="cpu"></sl-icon>
        <h2>No Agents Found</h2>
        <p>
          Agents are AI-powered workers that can help you with coding tasks.${can(this.scopeCapabilities, 'create') ? ' Create your first agent to get started.' : ''}
        </p>
        ${can(this.scopeCapabilities, 'create') ? html`
          <a href="/agents/new" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Create Agent
            </sl-button>
          </a>
        ` : nothing}
      </div>
    `;
  }

  private renderGrid() {
    return html`
      <div class="resource-grid">${this.agents.map((agent) => this.renderAgentCard(agent))}</div>
    `;
  }

  private renderAgentCard(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;

    return html`
      <div class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="resource-name">
              <sl-icon name="cpu"></sl-icon>
              <a href="/agents/${agent.id}" style="color: inherit; text-decoration: none;">
                ${agent.name}
              </a>
            </h3>
            <div class="agent-meta">
              ${agent.grove ? html`<div><sl-icon name="folder"></sl-icon> <a href="/groves/${agent.groveId}" @click=${(e: MouseEvent) => e.stopPropagation()}>${agent.grove}</a></div>` : ''}
              <div><sl-icon name="code-square"></sl-icon> ${agent.template}</div>
              ${agent.runtimeBrokerId
                ? html`<div>
                    <a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                      <sl-icon name="hdd-rack"></sl-icon>
                      ${agent.runtimeBrokerName || agent.runtimeBrokerId}
                    </a>
                  </div>`
                : ''}
            </div>
          </div>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          >
          </scion-status-badge>
        </div>

        ${agent.taskSummary ? html` <div class="agent-task">${agent.taskSummary}</div> ` : ''}

        <div class="agent-actions">
          ${can(agent._capabilities, 'attach') ? html`
            <sl-button
              variant="primary"
              size="small"
              href="/agents/${agent.id}/terminal"
              ?disabled=${!isTerminalAvailable(agent)}
            >
              <sl-icon slot="prefix" name="terminal"></sl-icon>
              Terminal
            </sl-button>
          ` : nothing}
          ${isAgentRunning(agent)
            ? can(agent._capabilities, 'stop') ? html`
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
              ` : nothing
            : can(agent._capabilities, 'start') ? html`
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
              ` : nothing}
          ${can(agent._capabilities, 'delete') ? html`
            <sl-button
              variant="default"
              size="small"
              outline
              ?loading=${isLoading}
              ?disabled=${isLoading}
              @click=${(e: MouseEvent) => this.handleAgentAction(agent.id, 'delete', e)}
            >
              <sl-icon slot="prefix" name="trash"></sl-icon>
            </sl-button>
          ` : nothing}
        </div>
      </div>
    `;
  }

  private renderTable() {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Grove</th>
              <th class="hide-mobile">Template</th>
              <th class="status-col">Status</th>
              <th class="hide-mobile">Task</th>
              <th style="text-align: right">Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.agents.map((agent) => this.renderAgentRow(agent))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderAgentRow(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;

    return html`
      <tr>
        <td>
          <span class="name-cell">
            <sl-icon name="cpu"></sl-icon>
            <a href="/agents/${agent.id}">${agent.name}</a>
          </span>
        </td>
        <td>${agent.grove ? html`<a href="/groves/${agent.groveId}" class="grove-link">${agent.grove}</a>` : '\u2014'}</td>
        <td class="hide-mobile">${agent.template}</td>
        <td>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          ></scion-status-badge>
        </td>
        <td class="hide-mobile">
          <span class="task-cell">${agent.taskSummary || '\u2014'}</span>
        </td>
        <td class="actions-cell">
          <span class="table-actions">
            ${can(agent._capabilities, 'attach') ? html`
              <sl-button
                variant="primary"
                size="small"
                href="/agents/${agent.id}/terminal"
                ?disabled=${!isTerminalAvailable(agent)}
              >
                <sl-icon slot="prefix" name="terminal"></sl-icon>
              </sl-button>
            ` : nothing}
            ${isAgentRunning(agent)
              ? can(agent._capabilities, 'stop') ? html`
                  <sl-button
                    variant="danger"
                    size="small"
                    outline
                    ?loading=${isLoading}
                    ?disabled=${isLoading}
                    @click=${() => this.handleAgentAction(agent.id, 'stop')}
                  >
                    <sl-icon slot="prefix" name="stop-circle"></sl-icon>
                  </sl-button>
                ` : nothing
              : can(agent._capabilities, 'start') ? html`
                  <sl-button
                    variant="success"
                    size="small"
                    outline
                    ?loading=${isLoading}
                    ?disabled=${isLoading}
                    @click=${() => this.handleAgentAction(agent.id, 'start')}
                  >
                    <sl-icon slot="prefix" name="play-circle"></sl-icon>
                  </sl-button>
                ` : nothing}
            ${can(agent._capabilities, 'delete') ? html`
              <sl-button
                variant="default"
                size="small"
                outline
                ?loading=${isLoading}
                ?disabled=${isLoading}
                @click=${(e: MouseEvent) => this.handleAgentAction(agent.id, 'delete', e)}
              >
                <sl-icon slot="prefix" name="trash"></sl-icon>
              </sl-button>
            ` : nothing}
          </span>
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agents': ScionPageAgents;
  }
}
