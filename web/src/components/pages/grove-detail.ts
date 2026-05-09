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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Grove, Agent, Capabilities } from '../../shared/types.js';
import { can, canAny, getAgentDisplayStatus, isAgentRunning, isTerminalAvailable, isSharedWorkspace } from '../../shared/types.js';
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import '../shared/git-remote-display.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';
import '../shared/agent-message-viewer.js';
import '../shared/file-browser.js';
import '../shared/file-editor.js';
import { WorkspaceFileBrowserDataSource, SharedDirFileBrowserDataSource } from '../shared/file-browser.js';
import type { FileBrowserDataSource } from '../shared/file-browser.js';
import { WorkspaceFileEditorDataSource, SharedDirFileEditorDataSource } from '../shared/file-editor.js';
import type { FileEditorDataSource } from '../shared/file-editor.js';

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

  /**
   * Scope-level capabilities from the agents list response
   */
  @state()
  private agentScopeCapabilities: Capabilities | undefined;

  /**
   * Active file tab key ('workspace' or shared dir name)
   */
  @state()
  private activeFileTab = 'workspace';

  /**
   * Per-tab file browser data sources keyed by tab name
   */
  private fileBrowserDataSources: Record<string, FileBrowserDataSource> = {};

  /**
   * Loading state for stop-all action
   */
  @state()
  private stopAllLoading = false;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  /**
   * Whether a git pull is in progress
   */
  @state()
  private pullLoading = false;

  /**
   * Result of the last git pull operation
   */
  @state()
  private pullResult: {
    status: string;
    updated?: boolean;
    commits?: { hash: string; subject: string }[];
    error?: string;
  } | null = null;

  /**
   * Whether the messages section is expanded (lazy-load trigger)
   */
  @state()
  private messagesExpanded = false;

  /**
   * Path of the file currently open in the editor (null = editor closed, '' = new file)
   */
  @state()
  private editingFilePath: string | null = null;

  /**
   * Whether to open the editor initially in preview mode (for .md eye icon)
   */
  @state()
  private editorInitialPreview = false;

  /**
   * Per-tab editor data sources keyed by tab name
   */
  private editorDataSources: Record<string, FileEditorDataSource> = {};

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

    .agent-meta {
      font-size: 0.813rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .agent-meta sl-icon {
      font-size: 0.875rem;
      vertical-align: -0.125em;
      opacity: 0.7;
    }

    .broker-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
    }

    .broker-link:hover {
      color: var(--scion-primary, #3b82f6);
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

    .agent-table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .agent-table-container table {
      width: 100%;
      border-collapse: collapse;
    }

    .agent-table-container th {
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

    .agent-table-container td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    .agent-table-container tr:last-child td {
      border-bottom: none;
    }

    .agent-table-container tr:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .agent-table-container .name-cell {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-weight: 500;
    }

    .agent-table-container .name-cell sl-icon {
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .agent-table-container .name-cell a {
      color: inherit;
      text-decoration: none;
    }

    .agent-table-container .name-cell a:hover {
      text-decoration: underline;
    }

    .agent-table-container .status-col {
      min-width: 11rem;
    }

    .agent-table-container .task-cell {
      max-width: 250px;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .agent-table-container .actions-cell {
      text-align: right;
      white-space: nowrap;
    }

    .table-actions {
      display: flex;
      gap: 0.375rem;
      justify-content: flex-end;
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

    .header-path a {
      color: inherit;
      text-decoration: none;
    }

    .header-path a:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .workspace-section {
      margin-top: 2rem;
      margin-bottom: 2rem;
    }

    .workspace-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .workspace-header-left {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .workspace-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .files-tab-group {
      margin-bottom: 0;
    }

    .files-tab-group::part(base) {
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .files-tab-group::part(body) {
      padding: 0;
    }

    .editor-back-row {
      margin-bottom: 0.5rem;
    }

    .tab-label-truncated {
      max-width: 10rem;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      display: inline-block;
      vertical-align: bottom;
    }

    .pull-commits {
      margin-top: 0.375rem;
      max-height: 8rem;
      overflow-y: auto;
      font-family: var(--sl-font-mono, monospace);
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .pull-commits .commit-hash {
      color: var(--sl-color-primary-600, #2563eb);
      margin-right: 0.375rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  private boundOnAgentsUpdated = this.onAgentsUpdated.bind(this);
  private boundOnGrovesUpdated = this.onGrovesUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();
    // SSR property bindings (.groveId=) aren't restored during client-side
    // hydration for top-level page components. Fall back to URL parsing.
    if (!this.groveId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/groves\/([^/]+)/);
      if (match) {
        this.groveId = match[1];
      }
    }

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-grove-agents') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    void this.loadData();

    // Set SSE scope to this grove (receives all agent events within grove)
    if (this.groveId) {
      stateManager.setScope({ type: 'grove', groveId: this.groveId });
    }

    // Listen for real-time updates
    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.addEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.removeEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
  }

  private onAgentsUpdated(): void {
    const updatedAgents = stateManager.getAgents();
    // Merge SSE agent deltas into local agent list
    const agentMap = new Map(this.agents.map((a) => [a.id, a]));
    // Lazily derive scope capabilities from existing agents if not yet set.
    if (!this.agentScopeCapabilities) {
      for (const a of agentMap.values()) {
        if (a._capabilities) {
          this.agentScopeCapabilities = a._capabilities;
          break;
        }
      }
    }
    for (const agent of updatedAgents) {
      if (agent.groveId === this.groveId || agentMap.has(agent.id)) {
        const existing = agentMap.get(agent.id);
        const merged = { ...existing, ...agent } as Agent;
        // New agents from SSE don't carry per-resource _capabilities.
        // Inherit scope-level capabilities so action buttons render.
        if (!merged._capabilities) {
          if (existing?._capabilities) {
            merged._capabilities = existing._capabilities;
          } else if (this.agentScopeCapabilities) {
            merged._capabilities = this.agentScopeCapabilities;
          }
        }
        agentMap.set(agent.id, merged);
      }
    }
    // Remove agents that were explicitly deleted via SSE
    const deletedIds = stateManager.getDeletedAgentIds();
    for (const id of deletedIds) {
      agentMap.delete(id);
    }
    this.agents = Array.from(agentMap.values());
  }

  private onGrovesUpdated(): void {
    const updatedGrove = stateManager.getGrove(this.groveId);
    if (updatedGrove && this.grove) {
      this.grove = { ...this.grove, ...updatedGrove };
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      // Load grove and agents in parallel
      const [groveResponse, agentsResponse] = await Promise.all([
        apiFetch(`/api/v1/groves/${this.groveId}`),
        apiFetch(`/api/v1/groves/${this.groveId}/agents`),
      ]);

      if (!groveResponse.ok) {
        throw new Error(await extractApiError(groveResponse, `HTTP ${groveResponse.status}: ${groveResponse.statusText}`));
      }

      this.grove = (await groveResponse.json()) as Grove;
      dispatchPageTitle(this, this.grove.name || this.groveId, 'Groves');

      if (agentsResponse.ok) {
        const agentsData = (await agentsResponse.json()) as
          | { agents?: Agent[]; _capabilities?: Capabilities }
          | Agent[];
        if (Array.isArray(agentsData)) {
          this.agents = agentsData;
          this.agentScopeCapabilities = undefined;
        } else {
          this.agents = agentsData.agents || [];
          this.agentScopeCapabilities = agentsData._capabilities;
        }
        // Derive scope capabilities from per-agent capabilities when the
        // response doesn't include a top-level _capabilities field.
        if (!this.agentScopeCapabilities) {
          this.agentScopeCapabilities = this.agents.find(a => a._capabilities)?._capabilities;
        }
      } else {
        // Fallback: if grove-scoped agents endpoint fails, try filtering from all agents
        this.agents = [];
        this.agentScopeCapabilities = undefined;
      }

      // Seed stateManager so SSE delta merging has full baseline data
      stateManager.seedAgents(this.agents);
      if (this.grove) {
        stateManager.seedGroves([this.grove]);
      }

      // Pre-create data sources for file tabs (the component loads files on connect)
      if (this.grove && (!this.grove.gitRemote || isSharedWorkspace(this.grove))) {
        this.getTabDataSource('workspace');
      }
      // For git-based groves (non-shared) with shared dirs, activate the first shared dir
      if (this.grove && this.grove.gitRemote && !isSharedWorkspace(this.grove) && this.grove.sharedDirs?.length) {
        this.activeFileTab = this.grove.sharedDirs[0].name;
        this.getTabDataSource(this.grove.sharedDirs[0].name);
      }

      // Auto-discover GitHub App installation if grove has a GitHub remote but no installation
      if (this.grove && this.grove.gitRemote && /github\.com[/:]/.test(this.grove.gitRemote) && this.grove.githubInstallationId == null) {
        void this.autoDiscoverGitHubApp();
      }
    } catch (err) {
      console.error('Failed to load grove:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load grove';
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
    const response = await apiFetch(`/api/v1/groves/${this.groveId}/agents`);
    if (!response.ok) return;

    const data = (await response.json()) as
      | { agents?: Agent[]; _capabilities?: Capabilities }
      | Agent[];
    if (Array.isArray(data)) {
      this.agents = data;
      this.agentScopeCapabilities = undefined;
    } else {
      this.agents = data.agents || [];
      this.agentScopeCapabilities = data._capabilities;
    }
    if (!this.agentScopeCapabilities) {
      this.agentScopeCapabilities = this.agents.find(a => a._capabilities)?._capabilities;
    }
    stateManager.seedAgents(this.agents);
  }

  private async autoDiscoverGitHubApp(): Promise<void> {
    try {
      // Check if the hub has a GitHub App configured
      const configRes = await apiFetch('/api/v1/github-app');
      if (!configRes.ok) return;
      const configData = (await configRes.json()) as { configured: boolean };
      if (!configData.configured) return;

      // Trigger discovery — the hub will match installations to this grove's git remote
      const discoverRes = await apiFetch('/api/v1/github-app/installations/discover', { method: 'POST' });
      if (!discoverRes.ok) return;

      // Reload grove data to pick up the newly associated installation
      const groveRes = await apiFetch(`/api/v1/groves/${this.groveId}`);
      if (groveRes.ok) {
        this.grove = (await groveRes.json()) as Grove;
        stateManager.seedGroves([this.grove]);
      }
    } catch {
      // Non-critical — grove just won't show GitHub icon until settings page is visited
    }
  }

  private renderGroveIcon() {
    return html`<sl-icon name="folder-fill"></sl-icon>`;
  }

  private renderLinkedBadge() {
    if (!this.grove || this.grove.groveType !== 'linked') return nothing;
    return html` <sl-tooltip content="Linked grove"><sl-icon name="link-45deg" style="font-size: 0.875rem; vertical-align: middle; opacity: 0.7;"></sl-icon></sl-tooltip>`;
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

  private getTabDataSource(tabName: string): FileBrowserDataSource {
    if (!this.fileBrowserDataSources[tabName]) {
      if (tabName === 'workspace') {
        this.fileBrowserDataSources[tabName] = new WorkspaceFileBrowserDataSource(this.groveId);
      } else {
        this.fileBrowserDataSources[tabName] = new SharedDirFileBrowserDataSource(this.groveId, tabName);
      }
    }
    return this.fileBrowserDataSources[tabName];
  }

  private getEditorDataSource(tabName: string): FileEditorDataSource {
    if (!this.editorDataSources[tabName]) {
      if (tabName === 'workspace') {
        this.editorDataSources[tabName] = new WorkspaceFileEditorDataSource(this.groveId);
      } else {
        this.editorDataSources[tabName] = new SharedDirFileEditorDataSource(this.groveId, tabName);
      }
    }
    return this.editorDataSources[tabName];
  }

  private handleFileEditRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = false;
  }

  private handleFilePreviewRequested(e: CustomEvent<{ path: string }>): void {
    this.editingFilePath = e.detail.path;
    this.editorInitialPreview = true;
  }

  private handleFileCreateRequested(): void {
    this.editingFilePath = '';
    this.editorInitialPreview = false;
  }

  private handleEditorClosed(): void {
    this.editingFilePath = null;
    this.editorInitialPreview = false;
  }

  private handleFileSaved(): void {
    // Refresh the file browser to reflect the saved/new file
    this.refreshActiveFileBrowser();
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
      this.backgroundRefresh();
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private hasRunningAgents(): boolean {
    return this.agents.some((a) => isAgentRunning(a));
  }

  private async handleStopAll(): Promise<void> {
    const isGroveAdmin = can(this.grove?._capabilities, 'manage');
    const confirmMsg = isGroveAdmin
      ? 'Are you sure you want to stop all running agents in this grove?'
      : 'Are you sure you want to stop all of your running agents in this grove?';
    if (!confirm(confirmMsg)) {
      return;
    }

    // Optimistic: mark running agents as "stopping"
    this.agents = this.agents.map(a =>
      isAgentRunning(a) ? { ...a, phase: 'stopping' as const } : a
    );
    this.stopAllLoading = true;

    try {
      const response = await apiFetch(`/api/v1/groves/${this.groveId}/agents/stop-all`, {
        method: 'POST',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to stop agents'));
      }

      const result = (await response.json()) as { stopped: number; failed: number; scope?: string };
      if (result.failed > 0) {
        alert(`Stopped ${result.stopped} agents, ${result.failed} failed.`);
      }

      this.backgroundRefresh();
    } catch (err) {
      console.error('Failed to stop agents:', err);
      alert(err instanceof Error ? err.message : 'Failed to stop agents');
      this.backgroundRefresh();
    } finally {
      this.stopAllLoading = false;
    }
  }

  private async handlePullLatest(): Promise<void> {
    this.pullLoading = true;
    this.pullResult = null;

    try {
      const response = await apiFetch(`/api/v1/groves/${this.groveId}/workspace/pull`, {
        method: 'POST',
      });

      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const result = (await response.json()) as any;

      if (!response.ok) {
        // Extract error message from structured APIError or legacy format
        const apiErr = result?.error;
        let errorMsg = (typeof apiErr === 'object' ? apiErr?.message : null)
          || result?.detail || result?.error || 'Pull failed';
        // Append guidance hint if available
        const guidance = apiErr?.details?.guidance;
        if (guidance) {
          errorMsg += ` — ${guidance}`;
        }
        this.pullResult = { status: 'error', error: errorMsg };
        return;
      }

      this.pullResult = { status: 'ok', updated: result.updated, commits: result.commits };
      // Refresh file list after pull
      this.refreshActiveFileBrowser();
    } catch (err) {
      this.pullResult = { status: 'error', error: err instanceof Error ? err.message : 'Pull failed' };
    } finally {
      this.pullLoading = false;
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
            ${this.renderGroveIcon()}
            <h1>${this.grove.name}${this.renderLinkedBadge()}</h1>
          </div>
          <div class="header-path"><scion-git-remote-display .grove=${this.grove}></scion-git-remote-display></div>
        </div>
        <div class="header-actions">
          ${can(this.agentScopeCapabilities, 'create')
            ? html`
                <a href="/agents/new?groveId=${this.groveId}" style="text-decoration: none;">
                  <sl-button variant="primary" size="small">
                    <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                    New Agent
                  </sl-button>
                </a>
              `
            : nothing}
          ${this.grove && isSharedWorkspace(this.grove) && can(this.grove?._capabilities, 'update')
            ? html`
                <sl-button
                  size="small"
                  ?loading=${this.pullLoading}
                  ?disabled=${this.pullLoading}
                  @click=${() => this.handlePullLatest()}
                >
                  <sl-icon slot="prefix" name="arrow-down-circle"></sl-icon>
                  Pull Latest
                </sl-button>
              `
            : nothing}
          ${canAny(this.grove?._capabilities, 'update', 'delete', 'manage')
            ? html`
                <a href="/groves/${this.groveId}/settings" style="text-decoration: none;">
                  <sl-button size="small">
                    <sl-icon slot="prefix" name="gear"></sl-icon>
                    Settings
                  </sl-button>
                </a>
              `
            : nothing}
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
            >${this.agents.filter((a) => isAgentRunning(a)).length}</span
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

      ${this.pullResult
        ? html`
            <sl-alert
              variant=${this.pullResult.status === 'ok' ? 'success' : 'danger'}
              open
              closable
              @sl-after-hide=${() => { this.pullResult = null; }}
            >
              <sl-icon slot="icon" name=${this.pullResult.status === 'ok' ? 'check-circle' : 'exclamation-triangle'}></sl-icon>
              ${this.pullResult.status === 'ok'
                ? this.pullResult.updated && this.pullResult.commits && this.pullResult.commits.length > 0
                  ? html`
                      Pulled ${this.pullResult.commits.length} commit${this.pullResult.commits.length === 1 ? '' : 's'}
                      <div class="pull-commits">
                        ${this.pullResult.commits.map(
                          (c) => html`<div><span class="commit-hash">${c.hash}</span>${c.subject}</div>`,
                        )}
                      </div>
                    `
                  : 'Already up to date.'
                : (this.pullResult.error || 'Pull failed.')}
            </sl-alert>
          `
        : nothing}

      <div class="section-header">
        <h2>Agents</h2>
        <div style="display: flex; align-items: center; gap: 0.75rem;">
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-grove-agents"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.agentScopeCapabilities, 'stop_all') && this.hasRunningAgents() ? html`
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
        </div>
      </div>

      ${this.agents.length === 0
        ? this.renderEmptyAgents()
        : this.viewMode === 'grid' ? this.renderAgentGrid() : this.renderAgentTable()}

      ${this.grove?.cloudLogging ? this.renderMessagesSection() : nothing}

      ${this.shouldShowFilesSection() ? this.renderFilesSection() : ''}
    `;
  }

  private handleMessagesToggle(): void {
    if (this.messagesExpanded) {
      // Collapse: stop streaming and reset loaded state so next expand reloads
      const viewer = this.shadowRoot?.querySelector(
        'scion-agent-message-viewer'
      ) as import('../shared/agent-message-viewer.js').ScionAgentMessageViewer | null;
      viewer?.stopStream();
      viewer?.resetLoaded();
      this.messagesExpanded = false;
    } else {
      this.messagesExpanded = true;
      this.updateComplete.then(() => {
        const viewer = this.shadowRoot?.querySelector(
          'scion-agent-message-viewer'
        ) as import('../shared/agent-message-viewer.js').ScionAgentMessageViewer | null;
        viewer?.loadMessages();
      });
    }
  }

  private renderMessagesSection() {
    return html`
      <div class="workspace-section">
        <div class="section-header" style="cursor: pointer;" @click=${this.handleMessagesToggle}>
          <h2>
            <sl-icon name=${this.messagesExpanded ? 'chevron-down' : 'chevron-right'}
              style="font-size: 0.875rem; vertical-align: middle; margin-right: 0.25rem;"></sl-icon>
            Messages
          </h2>
        </div>
        ${this.messagesExpanded ? html`
          <scion-agent-message-viewer
            logsUrl=${`/api/v1/groves/${this.groveId}/message-logs`}
            streamUrl=${`/api/v1/groves/${this.groveId}/message-logs/stream`}
            broadcastUrl=${`/api/v1/groves/${this.groveId}/broadcast`}
            ?canSend=${true}
          ></scion-agent-message-viewer>
        ` : nothing}
      </div>
    `;
  }

  private shouldShowFilesSection(): boolean {
    if (!this.grove) return false;
    // Hub-native groves and shared-workspace git groves always show files
    if (!this.grove.gitRemote || isSharedWorkspace(this.grove)) return true;
    // Per-agent git groves show only when shared dirs exist
    return (this.grove.sharedDirs?.length ?? 0) > 0;
  }

  private getFileTabs(): Array<{ key: string; label: string }> {
    const tabs: Array<{ key: string; label: string }> = [];
    // Hub-native groves and shared-workspace git groves get a workspace tab
    if (this.grove && (!this.grove.gitRemote || isSharedWorkspace(this.grove))) {
      tabs.push({ key: 'workspace', label: 'workspace' });
    }
    // Add one tab per shared dir
    for (const dir of this.grove?.sharedDirs ?? []) {
      tabs.push({ key: dir.name, label: dir.name });
    }
    return tabs;
  }

  private truncateTabLabel(label: string): string {
    if (label.length <= 20) return label;
    return '\u2026' + label.slice(label.length - 18);
  }

  private onFileTabChange(e: CustomEvent<{ name: string }>): void {
    const panel = e.detail.name;
    if (!panel) return;
    this.activeFileTab = panel;
  }

  private refreshActiveFileBrowser(): void {
    const browser = this.shadowRoot?.querySelector(
      `scion-file-browser[data-tab="${this.activeFileTab}"]`
    ) as import('../shared/file-browser.js').ScionFileBrowser | null;
    browser?.loadFiles();
  }

  private renderFilesSection() {
    const tabs = this.getFileTabs();
    const isEditable = can(this.grove?._capabilities, 'update');
    const isEditorOpen = this.editingFilePath !== null;

    return html`
      <div class="workspace-section">
        <div class="workspace-header">
          <div class="workspace-header-left">
            <h2>Files</h2>
          </div>
        </div>

        ${isEditorOpen
          ? html`
              <div class="editor-back-row">
                <sl-button size="small" variant="text" @click=${this.handleEditorClosed}>
                  <sl-icon slot="prefix" name="arrow-left"></sl-icon>
                  Back to files
                </sl-button>
              </div>
              <scion-file-editor
                .filePath=${this.editingFilePath || ''}
                .dataSource=${this.getEditorDataSource(this.activeFileTab)}
                ?readonly=${!isEditable}
                ?initialPreview=${this.editorInitialPreview}
                @file-saved=${this.handleFileSaved}
                @editor-closed=${this.handleEditorClosed}
              ></scion-file-editor>
            `
          : html`
              <div class="files-tab-header">
                <sl-tab-group class="files-tab-group" @sl-tab-show=${this.onFileTabChange}>
                  ${tabs.map(
                    (tab) => html`
                      <sl-tab slot="nav" panel=${tab.key} ?active=${tab.key === this.activeFileTab}>
                        <span class="tab-label-truncated" title=${tab.label}>${this.truncateTabLabel(tab.label)}</span>
                      </sl-tab>
                    `
                  )}
                  ${tabs.map(
                    (tab) => html`
                      <sl-tab-panel name=${tab.key}>
                        <scion-file-browser
                          data-tab=${tab.key}
                          .dataSource=${this.getTabDataSource(tab.key)}
                          ?editable=${isEditable}
                          ?showArchive=${tab.key === 'workspace'}
                          @file-edit-requested=${this.handleFileEditRequested}
                          @file-preview-requested=${this.handleFilePreviewRequested}
                          @file-create-requested=${this.handleFileCreateRequested}
                        ></scion-file-browser>
                      </sl-tab-panel>
                    `
                  )}
                </sl-tab-group>
              </div>
            `}
      </div>
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
        <p>
          This grove doesn't have any agents
          yet.${can(this.agentScopeCapabilities, 'create')
            ? ' Create your first agent to get started.'
            : ''}
        </p>
        ${can(this.agentScopeCapabilities, 'create')
          ? html`
              <a href="/agents/new?groveId=${this.groveId}" style="text-decoration: none;">
                <sl-button variant="primary">
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  New Agent
                </sl-button>
              </a>
            `
          : nothing}
      </div>
    `;
  }

  private renderAgentGrid() {
    return html`
      <div class="agent-grid">${this.agents.map((agent) => this.renderAgentCard(agent))}</div>
    `;
  }

  private renderAgentTable() {
    return html`
      <div class="agent-table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th class="hide-mobile">Template</th>
              <th class="hide-mobile">Broker</th>
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
        <td class="hide-mobile">${agent.template}</td>
        <td class="hide-mobile">
          ${agent.runtimeBrokerId
            ? html`<a href="/brokers/${agent.runtimeBrokerId}" class="broker-link">
                <sl-icon name="hdd-rack"></sl-icon>
                ${agent.runtimeBrokerName || agent.runtimeBrokerId}
              </a>`
            : '\u2014'}
        </td>
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
                  ${agent.harnessCapabilities?.resume?.support !== 'no' ? html`
                    <sl-button
                      variant="warning"
                      size="small"
                      outline
                      ?loading=${isLoading}
                      ?disabled=${isLoading}
                      @click=${() => this.handleAgentAction(agent.id, 'suspend')}
                    >
                      <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                    </sl-button>
                  ` : nothing}
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
              : agent.phase === 'suspended'
                ? can(agent._capabilities, 'start') ? html`
                    <sl-button
                      variant="success"
                      size="small"
                      outline
                      ?loading=${isLoading}
                      ?disabled=${isLoading}
                      @click=${() => this.handleAgentAction(agent.id, 'resume')}
                    >
                      <sl-icon slot="prefix" name="play-circle"></sl-icon>
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
            <div class="agent-meta">
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
          ></scion-status-badge>
        </div>

        ${agent.taskSummary ? html`<div class="agent-task">${agent.taskSummary}</div>` : ''}

        <div class="agent-actions">
          ${can(agent._capabilities, 'attach')
            ? html`
                <sl-button
                  variant="primary"
                  size="small"
                  href="/agents/${agent.id}/terminal"
                  ?disabled=${!isTerminalAvailable(agent)}
                >
                  <sl-icon slot="prefix" name="terminal"></sl-icon>
                  Terminal
                </sl-button>
              `
            : nothing}
          ${isAgentRunning(agent)
            ? can(agent._capabilities, 'stop')
              ? html`
                  ${agent.harnessCapabilities?.resume?.support !== 'no'
                    ? html`
                        <sl-button
                          variant="warning"
                          size="small"
                          outline
                          ?loading=${isLoading}
                          ?disabled=${isLoading}
                          @click=${() => this.handleAgentAction(agent.id, 'suspend')}
                        >
                          <sl-icon slot="prefix" name="pause-circle"></sl-icon>
                          Suspend
                        </sl-button>
                      `
                    : nothing}
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
              : nothing
            : agent.phase === 'suspended'
              ? can(agent._capabilities, 'start')
                ? html`
                    <sl-button
                      variant="success"
                      size="small"
                      outline
                      ?loading=${isLoading}
                      ?disabled=${isLoading}
                      @click=${() => this.handleAgentAction(agent.id, 'resume')}
                    >
                      <sl-icon slot="prefix" name="play-circle"></sl-icon>
                      Resume
                    </sl-button>
                  `
                : nothing
              : can(agent._capabilities, 'start')
                ? html`
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
                  `
                : nothing}
          ${can(agent._capabilities, 'delete')
            ? html`
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
              `
            : nothing}
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
