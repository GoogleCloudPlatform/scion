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
 * Home/Dashboard page component
 *
 * Displays an overview of the system status with Shoelace components
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Agent, Project, Capabilities } from '../../shared/types.js';
import { can, isAgentRunning } from '../../shared/types.js';
import '../shared/status-badge.js';
import { stateManager } from '../../client/state.js';
import type { AgentsChangedDetail } from '../../client/state.js';
import { apiFetch } from '../../client/api.js';
import { AgentMemberIndex } from '../../client/agent-member-index.js';
import { AgentSeedEpoch } from '../../client/agent-seed-epoch.js';
import { dropTombstoned, dropTombstonedPairs } from '../../client/agent-merge.js';
import { formatNumber } from '../../utils/format-number.js';
import {
  fetchHubProjectCapabilities,
  seedHubProjectCapabilities,
} from '../../client/hub-capabilities.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';

/**
 * Home's agents request: one agent, plus the complete set when it fits in
 * 500, and the counts. A complete response is today's full list; otherwise
 * the counts come from `stats`.
 */
const HOME_AGENTS_URL = '/api/v1/agents?sort=updated&dir=desc&limit=1&fit=500&stats=1';

/** The parts of the global agents response home reads. */
interface HomeAgentsResponse {
  agents?: Agent[];
  nextCursor?: string;
  complete?: boolean;
  stats?: { total: number; running: number; agents?: Array<[string, string]> };
}

interface InviteStats {
  pendingInvites: number;
  totalRedemptions: number;
  allowListCount: number;
  recentRedemptions: {
    id: string;
    codePrefix: string;
    useCount: number;
    maxUses: number;
    expiresAt: string;
    note: string;
    created: string;
  }[];
}

@customElement('scion-page-home')
export class ScionPageHome extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private agents: Agent[] = [];

  @state()
  private projects: Project[] = [];

  @state()
  private inviteStats: InviteStats | null = null;

  /**
   * Hub-scope project capabilities (`_capabilities` of GET /api/v1/projects).
   * Gates the "Create Project" quick action; undefined hides it (fail-closed).
   */
  @state()
  private projectScopeCapabilities: Capabilities | undefined;

  /**
   * The agent counts when the last agents response was not the complete
   * set (more than 500 agents): seeded from `stats` and kept live under
   * the dashboard add rule (every agent the dashboard scope delivers).
   * `null` while `this.agents` is the complete set. Above 2,000 agents it
   * is a count-only snapshot.
   */
  private memberIndex: AgentMemberIndex | null = null;

  /** Count-only mode: a live change may have changed the snapshot counts. */
  @state()
  private countsMayHaveChanged = false;

  /** Forces a re-render when the member index changes. */
  @state()
  private countsTick = 0;

  @state()
  private countsLoading = false;

  private agentsLoadSeq = 0;

  private boundOnAgentsUpdated = this.onAgentsUpdated.bind(this);
  private boundOnAgentsChanged = this.onAgentsChanged.bind(this);
  private boundOnProjectsUpdated = this.onProjectsUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.setScope({ type: 'dashboard' });

    // Subscribe before snapshot so no deltas are missed between read and listen
    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.addEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.addEventListener('projects-updated', this.boundOnProjectsUpdated as EventListener);

    // Use hydrated data if available, avoiding unnecessary fetches on SSR load
    // or when navigating back from a page that already populated the state.
    this.agents = stateManager.getAgents();
    this.projects = stateManager.getProjects();
    this.projectScopeCapabilities = stateManager.getScopeCapabilities('project');

    if (this.agents.length === 0 && this.projects.length === 0) {
      void this.loadData();
    } else if (stateManager.isAgentSetComplete('compact')) {
      // State holds every dashboard agent: its counts are exact.
      if (!this.projectScopeCapabilities) {
        // Hydrated from another page (e.g. Agents) that did not carry project
        // scope capabilities. Ask the shared helper rather than refetching
        // everything.
        void this.loadProjectScopeCapabilities();
      }
    } else if (this.projects.length > 0) {
      // State may hold only a subset of the agents (a label, mine or shared
      // load, or one page): the projects are kept, the agents are counted.
      void this.loadAgentCounts();
      if (!this.projectScopeCapabilities) void this.loadProjectScopeCapabilities();
    } else {
      void this.loadData();
    }
  }

  private async loadProjectScopeCapabilities(): Promise<void> {
    const caps = await fetchHubProjectCapabilities();
    if (!this.isConnected || stateManager.currentScope?.type !== 'dashboard') return;
    this.projectScopeCapabilities = caps;
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.removeEventListener('agents-changed', this.boundOnAgentsChanged as EventListener);
    stateManager.removeEventListener(
      'projects-updated',
      this.boundOnProjectsUpdated as EventListener
    );
  }

  private onAgentsUpdated(): void {
    this.agents = stateManager.getAgents();
  }

  private onProjectsUpdated(): void {
    this.projects = stateManager.getProjects();
  }

  /**
   * Keeps the member index live: a delete leaves it, and every agent the
   * dashboard scope delivers is a member (the dashboard add rule). In
   * count-only mode the snapshot is not adjusted; any change shows the chip.
   */
  private onAgentsChanged(e: Event): void {
    const index = this.memberIndex;
    if (!index) return;
    const detail = (e as CustomEvent<{ data: AgentsChangedDetail }>).detail.data;
    if (index.countOnly) {
      if (detail.upserted.length > 0 || detail.deleted.length > 0 || detail.unknown.size > 0) {
        this.countsMayHaveChanged = true;
      }
      return;
    }
    for (const id of detail.deleted) index.delete(id);
    for (const id of detail.upserted) {
      const agent = stateManager.getAgent(id);
      if (agent) index.set(id, agent.phase);
    }
    for (const [id, delta] of detail.unknown) {
      if (delta.phase && index.has(id)) index.set(id, delta.phase);
    }
    this.countsTick++;
  }

  private get activeAgentCount(): number {
    if (this.memberIndex) return this.memberIndex.stats.running;
    return this.agents.filter((a) => isAgentRunning(a)).length;
  }

  /**
   * Apply home's agents response, seeded under `epoch`. Complete: today's
   * full list, and the state store then holds every dashboard agent.
   * Otherwise: the one agent is seeded and the counts come from `stats`.
   */
  private applyAgentsResponse(data: HomeAgentsResponse, epoch: AgentSeedEpoch): void {
    const deleted = stateManager.getDeletedAgentIds();
    const agents = dropTombstoned(data.agents || [], deleted);
    if (data.complete) {
      this.agents = epoch.seed(agents, { partial: false, isMember: () => true }).agents;
      this.memberIndex = null;
      this.countsMayHaveChanged = false;
      stateManager.markAgentSetComplete('full');
      return;
    }
    epoch.seed(agents, { partial: false, isMember: () => true });
    const stats = data.stats ?? { total: 0, running: 0, agents: [] };
    const index = new AgentMemberIndex();
    if (stats.agents) {
      index.seed(dropTombstonedPairs(stats.agents, deleted));
      // Changes that landed while the request was in flight.
      for (const id of epoch.changedIds) {
        const agent = stateManager.getAgent(id);
        if (agent) index.set(id, agent.phase);
      }
    } else {
      index.seedCounts(stats.total, stats.running);
    }
    this.memberIndex = index;
    this.countsMayHaveChanged = false;
    this.agents = stateManager.getAgents();
  }

  /**
   * Fetch home's agents request and apply it under a seed epoch. A server
   * without sorted mode answers with a legacy page (no `complete`); then
   * today's unsorted request is sent once instead. Returns false when the
   * page left the dashboard scope or a newer load superseded this one.
   */
  private async fetchAgents(): Promise<boolean> {
    const seq = ++this.agentsLoadSeq;
    const current = (): boolean =>
      seq === this.agentsLoadSeq &&
      this.isConnected &&
      stateManager.currentScope?.type === 'dashboard';
    const epoch = new AgentSeedEpoch();
    try {
      const resp = await apiFetch(HOME_AGENTS_URL);
      if (!current() || !resp.ok) return false;
      const body = (await resp.json()) as HomeAgentsResponse | Agent[];
      if (!current()) return false;
      if (!Array.isArray(body) && body.complete !== undefined) {
        this.applyAgentsResponse(body, epoch);
        return true;
      }
    } finally {
      epoch.close();
    }
    return this.fetchLegacyAgents(current);
  }

  /** Today's unsorted agents request, for a server without sorted mode. */
  private async fetchLegacyAgents(current: () => boolean): Promise<boolean> {
    const epoch = new AgentSeedEpoch();
    try {
      const resp = await apiFetch('/api/v1/agents');
      if (!current() || !resp.ok) return false;
      const data = (await resp.json()) as HomeAgentsResponse | Agent[];
      if (!current()) return false;
      const agents = Array.isArray(data) ? data : data.agents || [];
      this.agents = epoch.seed(dropTombstoned(agents, stateManager.getDeletedAgentIds()), {
        partial: false,
        isMember: () => true,
      }).agents;
      this.memberIndex = null;
      if (!Array.isArray(data) && !data.nextCursor) stateManager.markAgentSetComplete('full');
      return true;
    } finally {
      epoch.close();
    }
  }

  /** The agents half of `loadData` alone, when the projects are already in state. */
  private async loadAgentCounts(): Promise<void> {
    this.countsLoading = true;
    try {
      await this.fetchAgents();
    } catch (err) {
      console.error('Failed to load agents for dashboard:', err);
    } finally {
      this.countsLoading = false;
    }
  }

  private async loadData(): Promise<void> {
    try {
      const isAdmin = this.pageData?.user?.role === 'admin';
      const [, projectsResp, inviteStatsResp] = await Promise.all([
        this.fetchAgents(),
        apiFetch('/api/v1/projects'),
        isAdmin
          ? apiFetch('/api/v1/admin/invites/stats', {
              suppressAccessDeniedToast: true,
            }).catch(() => null)
          : Promise.resolve(null),
      ]);

      if (!this.isConnected || stateManager.currentScope?.type !== 'dashboard') return;

      if (projectsResp.ok) {
        const data = (await projectsResp.json()) as
          | { projects?: Project[]; _capabilities?: Capabilities }
          | Project[];
        if (!this.isConnected || stateManager.currentScope?.type !== 'dashboard') return;
        const projects = Array.isArray(data) ? data : data.projects || [];
        this.projects = projects;
        stateManager.seedProjects(projects);
        const caps = Array.isArray(data) ? undefined : data._capabilities;
        this.projectScopeCapabilities = caps;
        if (caps) {
          stateManager.seedScopeCapabilities('project', caps);
          seedHubProjectCapabilities(caps);
        }
      }

      if (inviteStatsResp?.ok) {
        const stats = (await inviteStatsResp.json()) as InviteStats;
        if (!this.isConnected || stateManager.currentScope?.type !== 'dashboard') return;
        this.inviteStats = stats;
      }
    } catch (err) {
      console.error('Failed to load data for dashboard:', err);
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .hero {
      background: linear-gradient(
        135deg,
        var(--scion-primary, #3b82f6) 0%,
        var(--scion-primary-700, #1d4ed8) 100%
      );
      color: white;
      padding: 2rem;
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin-bottom: 2rem;
    }

    .hero h1 {
      font-size: 1.75rem;
      font-weight: 700;
      margin: 0 0 0.5rem 0;
    }

    .hero p {
      font-size: 1rem;
      opacity: 0.9;
      margin: 0;
    }

    .stats {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
      gap: 1.5rem;
      margin-bottom: 2rem;
    }

    .stat-card {
      background: var(--scion-surface, #ffffff);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      box-shadow: var(--scion-shadow, 0 1px 3px rgba(0, 0, 0, 0.1));
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .stat-card h3 {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 0.5rem 0;
    }

    .stat-value {
      font-size: 2rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .stat-change {
      font-size: 0.875rem;
      margin-top: 0.5rem;
      color: var(--scion-text-muted, #64748b);
    }

    .section-title {
      font-size: 1.25rem;
      font-weight: 600;
      margin-bottom: 1rem;
      color: var(--scion-text, #1e293b);
    }

    .quick-actions {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 1rem;
    }

    .action-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem;
      display: flex;
      align-items: center;
      gap: 1rem;
      cursor: pointer;
      transition: all var(--scion-transition-fast, 150ms ease);
      text-decoration: none;
      color: inherit;
    }

    .action-card:hover {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
      transform: translateY(-2px);
    }

    .action-icon {
      width: 3rem;
      height: 3rem;
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-primary-50, #eff6ff);
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
    }

    .action-icon sl-icon {
      font-size: 1.5rem;
    }

    .action-text h4 {
      font-size: 1rem;
      font-weight: 600;
      margin: 0 0 0.25rem 0;
      color: var(--scion-text, #1e293b);
    }

    .action-text p {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    /* Recent activity section */
    .activity-section {
      margin-top: 2rem;
    }

    .activity-list {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .activity-item {
      display: flex;
      align-items: center;
      gap: 1rem;
      padding: 1rem 1.25rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .activity-item:last-child {
      border-bottom: none;
    }

    .activity-icon {
      width: 2.5rem;
      height: 2.5rem;
      border-radius: 50%;
      background: var(--scion-bg-subtle, #f1f5f9);
      display: flex;
      align-items: center;
      justify-content: center;
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    .activity-content {
      flex: 1;
      min-width: 0;
    }

    .activity-title {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .activity-time {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .counts-note {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
    }

    .counts-chip {
      cursor: pointer;
    }

    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state > sl-icon {
      font-size: 3rem;
      margin-bottom: 1rem;
      opacity: 0.5;
    }
  `;

  override render() {
    const userName = this.pageData?.user?.name?.split(' ')[0] || 'there';

    return html`
      <div class="hero">
        <h1>Welcome back, ${userName}!</h1>
        <p>Here's what's happening with your agents today.</p>
      </div>

      <div class="stats">
        <div class="stat-card">
          <h3>Active Agents</h3>
          <div class="stat-value">
            <span>${this.activeAgentCount}</span>
          </div>
          ${this.memberIndex?.countOnly
            ? this.renderCountOnlyNote()
            : html`<div class="stat-change">
                <scion-status-badge
                  status="success"
                  label="Ready"
                  size="small"
                ></scion-status-badge>
              </div>`}
        </div>
        <div class="stat-card">
          <h3>Projects</h3>
          <div class="stat-value">${this.projects.length}</div>
          <div class="stat-change">Project workspaces</div>
        </div>
        <div class="stat-card">
          <h3>Pending Invites</h3>
          <div class="stat-value">${this.inviteStats?.pendingInvites ?? '--'}</div>
          <div class="stat-change">
            ${this.inviteStats ? `${this.inviteStats.totalRedemptions} total redemptions` : ''}
          </div>
        </div>
        <div class="stat-card">
          <h3>Allow List</h3>
          <div class="stat-value">${this.inviteStats?.allowListCount ?? '--'}</div>
          <div class="stat-change">Authorized users</div>
        </div>
      </div>

      <h2 class="section-title">Quick Actions</h2>
      <div class="quick-actions">
        <a href="/agents/new" class="action-card">
          <div class="action-icon">
            <sl-icon name="plus-lg"></sl-icon>
          </div>
          <div class="action-text">
            <h4>Create Agent</h4>
            <p>Spin up a new AI agent</p>
          </div>
        </a>
        ${can(this.projectScopeCapabilities, 'create')
          ? html`
              <a href="/projects/new" class="action-card">
                <div class="action-icon">
                  <sl-icon name="folder-plus"></sl-icon>
                </div>
                <div class="action-text">
                  <h4>Create Project</h4>
                  <p>Add a project workspace</p>
                </div>
              </a>
            `
          : nothing}
        <a href="/projects" class="action-card">
          <div class="action-icon">
            <sl-icon name="folder"></sl-icon>
          </div>
          <div class="action-text">
            <h4>View Projects</h4>
            <p>Browse project workspaces</p>
          </div>
        </a>
        <a href="/agents" class="action-card">
          <div class="action-icon">
            <sl-icon name="terminal"></sl-icon>
          </div>
          <div class="action-text">
            <h4>Open Terminal</h4>
            <p>Connect to running agent</p>
          </div>
        </a>
      </div>

      <div class="activity-section">
        <h2 class="section-title">Recent Activity</h2>
        <div class="activity-list">
          ${this.inviteStats && this.inviteStats.recentRedemptions.length > 0
            ? this.inviteStats.recentRedemptions.map(
                (r) => html`
                  <div class="activity-item">
                    <div class="activity-icon">
                      <sl-icon name="person-plus"></sl-icon>
                    </div>
                    <div class="activity-content">
                      <p class="activity-title">
                        Invite <code>${r.codePrefix}...</code> redeemed
                        (${r.useCount}/${r.maxUses > 0 ? r.maxUses : '∞'} uses)
                      </p>
                      <p class="activity-time">
                        ${r.note ? r.note + ' • ' : ''}${this.formatRelativeTime(r.created)}
                      </p>
                    </div>
                  </div>
                `
              )
            : html`
                <div class="empty-state">
                  <sl-icon name="clock-history"></sl-icon>
                  <p>No recent activity to display.<br />Start by creating your first agent.</p>
                  <a
                    href="/agents/new"
                    style="text-decoration: none; margin-top: 1rem; display: inline-block;"
                  >
                    <sl-button variant="primary">
                      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                      Create Agent
                    </sl-button>
                  </a>
                </div>
              `}
        </div>
      </div>
    `;
  }

  /**
   * Count-only mode (more than 2,000 agents): the count is the last
   * refresh's snapshot. A live change shows the chip; a click refreshes the
   * counts with one agents request.
   */
  private renderCountOnlyNote(): TemplateResult {
    return html`<div class="stat-change counts-note">
      <span>${formatNumber(this.memberIndex?.stats.total ?? 0)} agents, as of last refresh</span>
      ${this.countsMayHaveChanged
        ? html`<sl-tag
            class="counts-chip"
            size="small"
            variant="primary"
            pill
            @click=${(): void => this.onCountsChip()}
          >
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            counts may have changed · Refresh
          </sl-tag>`
        : nothing}
    </div>`;
  }

  private onCountsChip(): void {
    if (this.countsLoading) return;
    void this.loadAgentCounts();
  }

  private formatRelativeTime(dateStr: string): string {
    if (!dateStr) return '';
    const ms = new Date(dateStr).getTime();
    if (Number.isNaN(ms)) return dateStr;
    const diffMs = Date.now() - ms;
    // A future instant is clock skew between hub and browser.
    if (diffMs < 0) return 'just now';
    if (diffMs < 30 * 24 * 60 * 60 * 1000) return formatRelative(dateStr, { style: 'narrow' });
    return formatInstantWithZone(dateStr, 'date');
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-home': ScionPageHome;
  }
}
