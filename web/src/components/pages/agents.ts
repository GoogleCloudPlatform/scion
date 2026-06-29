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
 * Displays all agents across all projects with their status
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Agent, AgentPhase, Capabilities } from '../../shared/types.js';
import { can, isTerminalAvailable, getAgentDisplayStatus, isAgentRunning } from '../../shared/types.js';

type AgentSortField = 'name' | 'status' | 'created' | 'updated';
type SortDir = 'asc' | 'desc';
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
   * Filter scope: 'all' (no filter), 'mine' (created by me), 'shared' (in shared projects)
   */
  @state()
  private agentScope: 'all' | 'mine' | 'shared' = 'all';

  @state()
  private phaseFilter: AgentPhase | '' = '';

  @state()
  private labelFilter = '';

  @state()
  private sortField: AgentSortField = 'updated';

  @state()
  private sortDir: SortDir = 'desc';

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

      /* Color-coded hover effects for action buttons (skip disabled) */
      .action-btn-danger:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-danger-bg, rgba(239, 68, 68, 0.1));
        border-color: var(--scion-danger-400, #f87171);
        color: var(--scion-danger-600, #dc2626);
      }

      .action-btn-warning:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-warning-bg, rgba(245, 158, 11, 0.1));
        border-color: var(--scion-warning-400, #fbbf24);
        color: var(--scion-warning-600, #d97706);
      }

      .action-btn-success:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-success-bg, rgba(34, 197, 94, 0.1));
        border-color: var(--scion-success-400, #4ade80);
        color: var(--scion-success-600, #16a34a);
      }

      .action-btn-primary:not([disabled])::part(base):hover {
        background: var(--scion-action-hover-primary-bg, rgba(59, 130, 246, 0.1));
        border-color: var(--scion-primary-400, #60a5fa);
        color: var(--scion-primary-600, #2563eb);
      }

      .scope-toggle {
        display: inline-flex;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .scope-toggle button {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        height: 2rem;
        border: none;
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text-muted, #64748b);
        cursor: pointer;
        padding: 0 0.625rem;
        font-size: 0.8125rem;
        font-family: inherit;
        transition: all 150ms ease;
        white-space: nowrap;
      }

      .scope-toggle button:not(:last-child) {
        border-right: 1px solid var(--scion-border, #e2e8f0);
      }

      .scope-toggle button:hover:not(.active) {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .scope-toggle button.active {
        background: var(--scion-primary, #3b82f6);
        color: white;
      }

      .scope-toggle button sl-icon {
        font-size: 0.875rem;
      }

      .project-link {
        color: inherit;
        text-decoration: none;
      }

      .project-link:hover {
        text-decoration: underline;
      }

      .filter-bar {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        margin-bottom: 1rem;
        flex-wrap: wrap;
      }

      .filter-bar .label {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
      }

      th.sortable {
        cursor: pointer;
        user-select: none;
      }

      th.sortable:hover {
        color: var(--scion-text, #1e293b);
      }

      .sort-indicator {
        display: inline-block;
        margin-left: 0.25rem;
        font-size: 0.625rem;
        vertical-align: middle;
        opacity: 0.4;
      }

      th.sorted .sort-indicator {
        opacity: 1;
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

    // Read persisted scope filter
    if (this.pageData?.user) {
      const scope = localStorage.getItem('scion-scope-agents');
      if (scope === 'mine' || scope === 'shared') {
        this.agentScope = scope;
      }
    }

    // Read persisted phase filter
    const storedPhase = localStorage.getItem('scion-filter-agents-phase');
    if (storedPhase === 'running' || storedPhase === 'stopped' || storedPhase === 'suspended' || storedPhase === 'error') {
      this.phaseFilter = storedPhase;
    }

    // Read persisted sort
    const storedSort = localStorage.getItem('scion-sort-agents');
    if (storedSort) {
      try {
        const parsed = JSON.parse(storedSort);
        if (
          parsed &&
          (parsed.field === 'name' || parsed.field === 'status' || parsed.field === 'created' || parsed.field === 'updated') &&
          (parsed.dir === 'asc' || parsed.dir === 'desc')
        ) {
          this.sortField = parsed.field;
          this.sortDir = parsed.dir;
        }
      } catch { /* ignore invalid stored sort */ }
    }

    // Set SSE scope to dashboard (all project summaries).
    // This must happen before checking hydrated data because setScope clears
    // state maps when the scope changes (e.g. from agent-detail to dashboard).
    stateManager.setScope({ type: 'dashboard' });

    // Use hydrated data from SSR if available, avoiding the initial fetch.
    // Only trust it when scope was previously null (initial SSR page load);
    // on client-side navigations the maps were just cleared by setScope above.
    // Skip hydrated data when a scope filter is active — SSR data is unfiltered.
    // Also require scope capabilities — without them the "New Agent" button
    // won't render, so we must fetch from the API to get them.
    const hydratedAgents = stateManager.getAgents();
    const hydratedCaps = stateManager.getScopeCapabilities();
    if (hydratedAgents.length > 0 && hydratedCaps && this.agentScope === 'all') {
      this.agents = hydratedAgents;
      this.scopeCapabilities = hydratedCaps;
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
      // When a scope filter is active, only update agents already in the
      // filtered list — don't add new agents that weren't in the REST response.
      // The server-side filter is the source of truth for ownership/membership.
      if (!existing && this.agentScope !== 'all') {
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
    const params = new URLSearchParams();
    if (this.agentScope !== 'all') {
      params.set('scope', this.agentScope);
    }
    if (this.labelFilter.trim() && this.labelFilter.includes('=')) {
      params.append('label', this.labelFilter.trim());
    }
    const qs = params.toString();
    const url = qs ? `/api/v1/agents?${qs}` : '/api/v1/agents';
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
    if (this.scopeCapabilities) {
      stateManager.seedScopeCapabilities(this.scopeCapabilities);
    }
  }

  private async handleAgentAction(
    agentId: string,
    action: 'start' | 'stop' | 'suspend' | 'resume' | 'delete',
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

    // Apply optimistic phase update immediately
    const optimisticPhase: Record<string, string> = {
      start: 'starting',
      stop: 'stopping',
      suspend: 'stopping',
      resume: 'starting',
    };
    const agentIndex = this.agents.findIndex(a => a.id === agentId);
    if (agentIndex >= 0) {
      const updated = { ...this.agents[agentIndex] };
      updated.phase = optimisticPhase[action] as Agent['phase'];
      this.agents = [...this.agents];
      this.agents[agentIndex] = updated;
    }

    const actionUrls: Record<string, string> = {
      start: `/api/v1/agents/${agentId}/start`,
      stop: `/api/v1/agents/${agentId}/stop`,
      suspend: `/api/v1/agents/${agentId}/suspend`,
      resume: `/api/v1/agents/${agentId}/start`,
    };

    try {
      const response = await apiFetch(actionUrls[action], { method: 'POST' });

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

  private get displayAgents(): Agent[] {
    let list = this.agents;
    if (this.phaseFilter) {
      list = list.filter(a => a.phase === this.phaseFilter);
    }
    if (this.labelFilter.trim()) {
      const parts = this.labelFilter.trim().split('=');
      const filterKey = parts[0];
      const filterValue = parts.slice(1).join('=');
      list = list.filter(a => {
        if (!a.labels) return false;
        if (filterValue) return a.labels[filterKey] === filterValue;
        return filterKey in a.labels;
      });
    }
    const sorted = [...list];
    sorted.sort((a, b) => {
      let cmp = 0;
      switch (this.sortField) {
        case 'name':
          cmp = (a.name || '').localeCompare(b.name || '');
          break;
        case 'status':
          cmp = getAgentDisplayStatus(a).localeCompare(getAgentDisplayStatus(b));
          break;
        case 'created':
          cmp = (a.created || '').localeCompare(b.created || '');
          break;
        case 'updated':
          cmp = (a.updated || '').localeCompare(b.updated || '');
          break;
      }
      return this.sortDir === 'asc' ? cmp : -cmp;
    });
    return sorted;
  }

  private formatRelativeTime(isoString: string): string {
    const date = new Date(isoString);
    if (isNaN(date.getTime())) return '—';
    const now = Date.now();
    const diffMs = now - date.getTime();
    if (diffMs < 0) return 'just now';
    const seconds = Math.floor(diffMs / 1000);
    if (seconds < 60) return 'just now';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  }

  private setPhaseFilter(phase: AgentPhase | ''): void {
    if (this.phaseFilter === phase) return;
    this.phaseFilter = phase;
    if (phase) {
      localStorage.setItem('scion-filter-agents-phase', phase);
    } else {
      localStorage.removeItem('scion-filter-agents-phase');
    }
  }

  private toggleSort(field: AgentSortField): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
    localStorage.setItem('scion-sort-agents', JSON.stringify({ field: this.sortField, dir: this.sortDir }));
  }

  private sortIndicator(field: AgentSortField): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  private setScope(scope: 'all' | 'mine' | 'shared'): void {
    if (this.agentScope === scope) return;
    this.agentScope = scope;
    if (scope === 'all') {
      localStorage.removeItem('scion-scope-agents');
    } else {
      localStorage.setItem('scion-scope-agents', scope);
    }
    void this.loadAgents();
  }

  override render() {
    return html`
      <div class="header">
        <h1>Agents</h1>
        <div class="header-actions">
          ${this.pageData?.user ? html`
            <div class="scope-toggle">
              <button
                class=${this.agentScope === 'all' ? 'active' : ''}
                title="All agents"
                @click=${() => this.setScope('all')}
              >All</button>
              <button
                class=${this.agentScope === 'mine' ? 'active' : ''}
                title="Agents I created"
                @click=${() => this.setScope('mine')}
              >
                <sl-icon name="person"></sl-icon>
                Mine
              </button>
              <button
                class=${this.agentScope === 'shared' ? 'active' : ''}
                title="Agents in shared projects"
                @click=${() => this.setScope('shared')}
              >
                <sl-icon name="people"></sl-icon>
                Shared
              </button>
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

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : html`
        ${this.renderFilterBar()}
        ${this.renderAgents()}
      `}
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

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <span class="label">Status:</span>
        <div class="scope-toggle">
          <button
            class=${this.phaseFilter === '' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('')}
          >All</button>
          <button
            class=${this.phaseFilter === 'running' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('running')}
          >Running</button>
          <button
            class=${this.phaseFilter === 'stopped' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('stopped')}
          >Stopped</button>
          <button
            class=${this.phaseFilter === 'suspended' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('suspended')}
          >Suspended</button>
          <button
            class=${this.phaseFilter === 'error' ? 'active' : ''}
            @click=${() => this.setPhaseFilter('error')}
          >Error</button>
        </div>
        <sl-input
          size="small"
          placeholder="Filter by label (key=value)"
          clearable
          .value=${this.labelFilter}
          @sl-input=${(e: Event) => {
            this.labelFilter = (e.target as HTMLElement & { value: string }).value;
          }}
          @sl-change=${() => void this.loadAgents()}
          @sl-clear=${() => { this.labelFilter = ''; void this.loadAgents(); }}
          style="max-width: 220px;"
        >
          <sl-icon slot="prefix" name="tag"></sl-icon>
        </sl-input>
        ${this.viewMode === 'grid' ? html`
          <sl-dropdown>
            <sl-button slot="trigger" size="small" outline>
              <sl-icon slot="prefix" name=${this.sortDir === 'asc' ? 'sort-alpha-down' : 'sort-alpha-down-alt'}></sl-icon>
              Sort: ${this.sortField}
            </sl-button>
            <sl-menu @sl-select=${(e: CustomEvent<{ item: { value: string } }>) => this.toggleSort(e.detail.item.value as AgentSortField)}>
              <sl-menu-item value="name" ?checked=${this.sortField === 'name'}>Name</sl-menu-item>
              <sl-menu-item value="status" ?checked=${this.sortField === 'status'}>Status</sl-menu-item>
              <sl-menu-item value="created" ?checked=${this.sortField === 'created'}>Created</sl-menu-item>
              <sl-menu-item value="updated" ?checked=${this.sortField === 'updated'}>Updated</sl-menu-item>
            </sl-menu>
          </sl-dropdown>
        ` : nothing}
      </div>
    `;
  }

  private renderAgents() {
    if (this.agents.length === 0) {
      if (this.agentScope === 'mine') {
        return html`
          <div class="empty-state">
            <sl-icon name="person"></sl-icon>
            <h2>No Agents Found</h2>
            <p>You haven't created any agents yet.</p>
          </div>
        `;
      }
      if (this.agentScope === 'shared') {
        return html`
          <div class="empty-state">
            <sl-icon name="people"></sl-icon>
            <h2>No Shared Agents</h2>
            <p>No agents have been shared with you yet.</p>
          </div>
        `;
      }
      return this.renderEmptyState();
    }

    const filtered = this.displayAgents;
    if (filtered.length === 0 && this.phaseFilter) {
      return html`
        <div class="empty-state">
          <sl-icon name="funnel"></sl-icon>
          <h2>No Matching Agents</h2>
          <p>No agents match the current filter. Try changing the status filter.</p>
        </div>
      `;
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
      <div class="resource-grid">${this.displayAgents.map((agent) => this.renderAgentCard(agent))}</div>
    `;
  }

  private renderActionButtons(agent: Agent) {
    const isLoading = this.actionLoading[agent.id] || false;

    return html`
      ${can(agent._capabilities, 'attach') ? html`
        <sl-tooltip content="Terminal">
          <span style="display: inline-flex">
            <sl-button
              class="action-btn-primary"
              variant="primary"
              size="small"
              href="/agents/${agent.id}/terminal"
              ?disabled=${!isTerminalAvailable(agent)}
              aria-label="Terminal"
            >
              <sl-icon slot="prefix" name="terminal"></sl-icon>
            </sl-button>
          </span>
        </sl-tooltip>
      ` : nothing}
      ${isAgentRunning(agent)
        ? can(agent._capabilities, 'stop') ? html`
            ${agent.harnessCapabilities?.resume?.support !== 'no' ? html`
              <sl-tooltip content="Suspend">
                <sl-button
                  class="action-btn-warning"
                  variant="warning"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'suspend')}
                  aria-label="Suspend"
                >
                  <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                </sl-button>
              </sl-tooltip>
            ` : nothing}
            <sl-tooltip content="Stop">
              <sl-button
                class="action-btn-danger"
                variant="danger"
                size="small"
                outline
                ?loading=${isLoading}
                ?disabled=${isLoading}
                @click=${() => this.handleAgentAction(agent.id, 'stop')}
                aria-label="Stop"
              >
                <sl-icon slot="prefix" name="stop-circle"></sl-icon>
              </sl-button>
            </sl-tooltip>
          ` : nothing
        : agent.phase === 'suspended'
          ? can(agent._capabilities, 'start') ? html`
              <sl-tooltip content="Resume">
                <sl-button
                  class="action-btn-success"
                  variant="success"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'resume')}
                  aria-label="Resume"
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                </sl-button>
              </sl-tooltip>
            ` : nothing
          : can(agent._capabilities, 'start') ? html`
              <sl-tooltip content="Start">
                <sl-button
                  class="action-btn-success"
                  variant="success"
                  size="small"
                  outline
                  ?loading=${isLoading}
                  ?disabled=${isLoading}
                  @click=${() => this.handleAgentAction(agent.id, 'start')}
                  aria-label="Start"
                >
                  <sl-icon slot="prefix" name="play-circle"></sl-icon>
                </sl-button>
              </sl-tooltip>
            ` : nothing}
      ${can(agent._capabilities, 'delete') ? html`
        <sl-tooltip content="Delete">
          <sl-button
            class="action-btn-danger"
            variant="default"
            size="small"
            outline
            ?loading=${isLoading}
            ?disabled=${isLoading}
            @click=${(e: MouseEvent) => this.handleAgentAction(agent.id, 'delete', e)}
            aria-label="Delete"
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
          </sl-button>
        </sl-tooltip>
      ` : nothing}
    `;
  }

  private renderAgentCard(agent: Agent) {
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
              ${agent.project ? html`<div><sl-icon name="folder"></sl-icon> <a href="/projects/${agent.projectId}" @click=${(e: MouseEvent) => e.stopPropagation()}>${agent.project}</a></div>` : ''}
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

        ${agent.labels && Object.keys(agent.labels).length > 0
          ? html`<div class="agent-labels" style="margin-top: 0.5em;">${Object.entries(agent.labels).map(
              ([k, v]) => html`<sl-tag size="small" variant="neutral" style="margin: 0.15em;">${k}: ${v}</sl-tag>`
            )}</div>`
          : ''}

        <div class="agent-actions">
          ${this.renderActionButtons(agent)}
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
              <th
                class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('name')}
              >Name <span class="sort-indicator">${this.sortIndicator('name')}</span></th>
              <th>Project</th>
              <th class="hide-mobile">Template</th>
              <th
                class="status-col sortable ${this.sortField === 'status' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('status')}
              >Status <span class="sort-indicator">${this.sortIndicator('status')}</span></th>
              <th
                class="hide-mobile sortable ${this.sortField === 'updated' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('updated')}
              >Updated <span class="sort-indicator">${this.sortIndicator('updated')}</span></th>
              <th class="hide-mobile">Task</th>
              <th style="text-align: right">Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.displayAgents.map((agent) => this.renderAgentRow(agent))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderAgentRow(agent: Agent) {
    return html`
      <tr>
        <td>
          <span class="name-cell">
            <sl-icon name="cpu"></sl-icon>
            <a href="/agents/${agent.id}">${agent.name}</a>
          </span>
        </td>
        <td>${agent.project ? html`<a href="/projects/${agent.projectId}" class="project-link">${agent.project}</a>` : '\u2014'}</td>
        <td class="hide-mobile">${agent.template}</td>
        <td>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          ></scion-status-badge>
        </td>
        <td class="hide-mobile">${agent.updated ? this.formatRelativeTime(agent.updated) : '\u2014'}</td>
        <td class="hide-mobile">
          <span class="task-cell">${agent.taskSummary || '\u2014'}</span>
        </td>
        <td class="actions-cell">
          <span class="table-actions">
            ${this.renderActionButtons(agent)}
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
