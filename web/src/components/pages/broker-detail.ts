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
 * Broker detail page component
 *
 * Displays a single runtime broker with its info, capabilities,
 * profiles, and agents grouped by project.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, RuntimeBroker, Agent } from '../../shared/types.js';
import { getAgentDisplayStatus } from '../../shared/types.js';
import type { StatusType } from '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import '../shared/status-badge.js';

interface BrokerProjectInfo {
  projectId: string;
  projectName: string;
  gitRemote?: string;
  agentCount: number;
  localPath?: string;
}

@customElement('scion-page-broker-detail')
export class ScionPageBrokerDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  brokerId = '';

  @state()
  private loading = true;

  @state()
  private broker: RuntimeBroker | null = null;

  @state()
  private projects: BrokerProjectInfo[] = [];

  @state()
  private agents: Agent[] = [];

  @state()
  private error: string | null = null;

  private boundOnBrokersUpdated = this.onBrokersUpdated.bind(this);
  private relativeTimeInterval: ReturnType<typeof setInterval> | null = null;

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

    .header-subtitle {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
      word-break: break-all;
    }

    .stats-row {
      display: flex;
      gap: 2rem;
      margin-bottom: 2rem;
      padding: 1.25rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      flex-wrap: wrap;
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

    .stat-value-sm {
      font-size: 1rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .section {
      margin-bottom: 2rem;
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

    .capabilities-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 2rem;
    }

    .capability-tag {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.25rem 0.5rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .capability-tag.enabled {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .profiles-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 2rem;
    }

    .profile-tag {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.375rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .profile-tag.available {
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
    }

    .profile-type {
      font-size: 0.6875rem;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      opacity: 0.7;
    }

    .project-section {
      margin-bottom: 1.5rem;
    }

    .project-section-header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 1rem;
      padding-bottom: 0.5rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .project-section-header h3 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .project-section-header a {
      color: inherit;
      text-decoration: none;
    }

    .project-section-header a:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .project-section-header sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .project-agent-count {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();

    if (!this.brokerId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/brokers\/([^/]+)/);
      if (match) {
        this.brokerId = match[1];
      }
    }

    void this.loadData();

    if (this.brokerId) {
      stateManager.setScope({ type: 'broker-detail', brokerId: this.brokerId });
    }

    stateManager.addEventListener('brokers-updated', this.boundOnBrokersUpdated as EventListener);

    this.relativeTimeInterval = setInterval(() => this.requestUpdate(), 15_000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener(
      'brokers-updated',
      this.boundOnBrokersUpdated as EventListener
    );
    if (this.relativeTimeInterval) {
      clearInterval(this.relativeTimeInterval);
      this.relativeTimeInterval = null;
    }
  }

  private onBrokersUpdated(): void {
    const updatedBroker = stateManager.getBroker(this.brokerId);
    if (updatedBroker && this.broker) {
      this.broker = { ...this.broker, ...updatedBroker };
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [brokerResponse, projectsResponse, agentsResponse] = await Promise.all([
        apiFetch(`/api/v1/runtime-brokers/${this.brokerId}`),
        apiFetch(`/api/v1/runtime-brokers/${this.brokerId}/projects`),
        apiFetch(`/api/v1/agents?runtimeBrokerId=${this.brokerId}`),
      ]);

      if (!brokerResponse.ok) {
        throw new Error(await extractApiError(brokerResponse, `HTTP ${brokerResponse.status}: ${brokerResponse.statusText}`));
      }

      this.broker = (await brokerResponse.json()) as RuntimeBroker;
      dispatchPageTitle(this, this.broker.name || this.brokerId, 'Brokers');

      if (projectsResponse.ok) {
        const projectsData = (await projectsResponse.json()) as { projects?: BrokerProjectInfo[] };
        this.projects = projectsData.projects || [];
      } else {
        this.projects = [];
      }

      if (agentsResponse.ok) {
        const agentsData = (await agentsResponse.json()) as { agents?: Agent[] } | Agent[];
        this.agents = Array.isArray(agentsData) ? agentsData : agentsData.agents || [];
      } else {
        this.agents = [];
      }

      // Seed stateManager so SSE delta merging has full baseline data
      if (this.broker) {
        stateManager.seedBrokers([this.broker]);
      }
    } catch (err) {
      console.error('Failed to load broker:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load broker';
    } finally {
      this.loading = false;
    }
  }

  private getBrokerStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'online':
        return 'success';
      case 'degraded':
        return 'warning';
      case 'offline':
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

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
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

  private getAgentsForProject(projectId: string): Agent[] {
    return this.agents.filter((a) => a.projectId === projectId);
  }

  override render() {
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error) {
      return this.renderError();
    }

    if (!this.broker) {
      return this.renderError();
    }

    const totalAgents = this.agents.length;
    const subtitleParts: string[] = [];
    if (this.broker.version) subtitleParts.push(`v${this.broker.version}`);
    if (this.broker.endpoint) subtitleParts.push(this.broker.endpoint);

    return html`
      <a href="/brokers" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Brokers
      </a>

      <div class="header">
        <div class="header-info">
          <div class="header-title">
            <sl-icon name="hdd-rack"></sl-icon>
            <h1>${this.broker.name}</h1>
            <scion-status-badge
              status=${this.getBrokerStatusVariant(this.broker.status)}
              label=${this.broker.status}
              size="small"
            ></scion-status-badge>
          </div>
          ${subtitleParts.length > 0
            ? html`<div class="header-subtitle">${subtitleParts.join(' · ')}</div>`
            : ''}
        </div>
      </div>

      <div class="stats-row">
        <div class="stat">
          <span class="stat-label">Projects</span>
          <span class="stat-value">${this.projects.length}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Agents</span>
          <span class="stat-value">${totalAgents}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Created</span>
          <span class="stat-value-sm">${this.formatDate(this.broker.createdAt)}</span>
        </div>
        <div class="stat">
          <span class="stat-label">Last Heartbeat</span>
          <span class="stat-value-sm">${this.formatRelativeTime(this.broker.lastHeartbeat)}</span>
        </div>
        ${this.broker.createdBy
          ? html`
              <div class="stat">
                <span class="stat-label">Created By</span>
                <span class="stat-value-sm">${this.broker.createdByName || this.broker.createdBy}</span>
              </div>
            `
          : ''}
      </div>

      ${this.broker.capabilities ? this.renderCapabilities(this.broker.capabilities) : ''}
      ${this.broker.profiles && this.broker.profiles.length > 0
        ? this.renderProfiles(this.broker.profiles)
        : ''}
      ${this.renderProjectSections()}
    `;
  }

  private renderCapabilities(capabilities: import('../../shared/types.js').BrokerCapabilities) {
    return html`
      <div class="section">
        <div class="section-header">
          <h2>Capabilities</h2>
        </div>
        <div class="capabilities-row">
          <span class="capability-tag ${capabilities.webPTY ? 'enabled' : ''}">WebPTY</span>
          <span class="capability-tag ${capabilities.sync ? 'enabled' : ''}">Sync</span>
          <span class="capability-tag ${capabilities.attach ? 'enabled' : ''}">Attach</span>
        </div>
      </div>
    `;
  }

  private renderProfiles(profiles: import('../../shared/types.js').BrokerProfile[]) {
    return html`
      <div class="section">
        <div class="section-header">
          <h2>Profiles</h2>
        </div>
        <div class="profiles-row">
          ${profiles.map(
            (profile) => html`
              <span class="profile-tag ${profile.available ? 'available' : ''}">
                ${profile.name}
                <span class="profile-type">${profile.type}</span>
              </span>
            `
          )}
        </div>
      </div>
    `;
  }

  private renderProjectSections() {
    if (this.projects.length === 0) {
      return html`
        <div class="section">
          <div class="section-header">
            <h2>Projects</h2>
          </div>
          <div class="empty-state">
            <sl-icon name="folder2-open"></sl-icon>
            <h2>No Projects</h2>
            <p>This broker is not currently providing for any projects.</p>
          </div>
        </div>
      `;
    }

    return html`
      <div class="section">
        <div class="section-header">
          <h2>Projects</h2>
        </div>
        ${this.projects.map((project) => this.renderProjectSection(project))}
      </div>
    `;
  }

  private renderProjectSection(project: BrokerProjectInfo) {
    const projectAgents = this.getAgentsForProject(project.projectId);

    return html`
      <div class="project-section">
        <div class="project-section-header">
          <sl-icon name="folder-fill"></sl-icon>
          <h3>
            <a href="/projects/${project.projectId}">${project.projectName || project.projectId}</a>
          </h3>
          <span class="project-agent-count"
            >${projectAgents.length} agent${projectAgents.length !== 1 ? 's' : ''}</span
          >
        </div>
        ${projectAgents.length > 0
          ? html`
              <div class="agent-grid">
                ${projectAgents.map((agent) => this.renderAgentCard(agent))}
              </div>
            `
          : ''}
      </div>
    `;
  }

  private renderAgentCard(agent: Agent) {
    return html`
      <a href="/agents/${agent.id}" class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="agent-name">
              <sl-icon name="cpu"></sl-icon>
              ${agent.name}
            </h3>
            <div class="agent-meta">
              <sl-icon name="code-square"></sl-icon> ${agent.template}
            </div>
          </div>
          <scion-status-badge
            status=${getAgentDisplayStatus(agent) as StatusType}
            label=${getAgentDisplayStatus(agent)}
            size="small"
          ></scion-status-badge>
        </div>
        ${agent.taskSummary ? html`<div class="agent-task">${agent.taskSummary}</div>` : ''}
      </a>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading broker...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/brokers" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Brokers
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Broker</h2>
        <p>There was a problem loading this broker.</p>
        <div class="error-details">${this.error || 'Broker not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-broker-detail': ScionPageBrokerDetail;
  }
}
