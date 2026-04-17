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

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Agent, Grove } from '../../shared/types.js';
import { isAgentRunning } from '../../shared/types.js';
import '../shared/status-badge.js';
import { stateManager } from '../../client/state.js';
import { apiFetch } from '../../client/api.js';

@customElement('scion-page-home')
export class ScionPageHome extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private agents: Agent[] = [];

  @state()
  private groves: Grove[] = [];

  private boundOnAgentsUpdated = this.onAgentsUpdated.bind(this);
  private boundOnGrovesUpdated = this.onGrovesUpdated.bind(this);

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.setScope({ type: 'dashboard' });
    
    this.agents = stateManager.getAgents();
    this.groves = stateManager.getGroves();

    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.addEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);

    void this.loadData();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    stateManager.removeEventListener('groves-updated', this.boundOnGrovesUpdated as EventListener);
  }

  private onAgentsUpdated(): void {
    this.agents = stateManager.getAgents();
  }

  private onGrovesUpdated(): void {
    this.groves = stateManager.getGroves();
  }

  private get activeAgentCount(): number {
    return this.agents.filter(a => isAgentRunning(a)).length;
  }

  private async loadData(): Promise<void> {
    try {
      const [agentsResp, grovesResp] = await Promise.all([
        apiFetch('/api/v1/agents'),
        apiFetch('/api/v1/groves')
      ]);

      if (!this.isConnected || stateManager.currentScope?.type !== 'dashboard') return;

      if (agentsResp.ok) {
        const data = await agentsResp.json();
        const agents = Array.isArray(data) ? data : data.agents || [];
        this.agents = agents;
        stateManager.seedAgents(agents);
      }

      if (grovesResp.ok) {
        const data = await grovesResp.json();
        const groves = Array.isArray(data) ? data : data.groves || [];
        this.groves = groves;
        stateManager.seedGroves(groves);
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
          <div class="stat-change">
            <scion-status-badge status="success" label="Ready" size="small"></scion-status-badge>
          </div>
        </div>
        <div class="stat-card">
          <h3>Groves</h3>
          <div class="stat-value">${this.groves.length}</div>
          <div class="stat-change">Project workspaces</div>
        </div>
        <div class="stat-card">
          <h3>Tasks Completed</h3>
          <div class="stat-value">--</div>
          <div class="stat-change">This week</div>
        </div>
        <div class="stat-card">
          <h3>System Status</h3>
          <div class="stat-value">
            <scion-status-badge status="healthy" size="large" label="Healthy"></scion-status-badge>
          </div>
          <div class="stat-change">All systems operational</div>
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
        <a href="/groves/new" class="action-card">
          <div class="action-icon">
            <sl-icon name="folder-plus"></sl-icon>
          </div>
          <div class="action-text">
            <h4>Create Grove</h4>
            <p>Add a project workspace</p>
          </div>
        </a>
        <a href="/groves" class="action-card">
          <div class="action-icon">
            <sl-icon name="folder"></sl-icon>
          </div>
          <div class="action-text">
            <h4>View Groves</h4>
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
          <div class="empty-state">
            <sl-icon name="clock-history"></sl-icon>
            <p>No recent activity to display.<br />Start by creating your first agent.</p>
            <a href="/agents/new" style="text-decoration: none; margin-top: 1rem; display: inline-block;">
              <sl-button variant="primary">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create Agent
              </sl-button>
            </a>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-home': ScionPageHome;
  }
}
