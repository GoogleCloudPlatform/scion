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
 * "Agents & projects" tab of the Brokers area detail page (broker UI
 * Option A, slice A1): the projects this broker provides for, with their
 * agents on it, and a marker where the broker is the project's default.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { Agent } from '../../shared/types.js';
import { agentStatusBadge } from '../../shared/agent-state-display.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';

/** A project this broker provides for (GET /runtime-brokers/{id}/projects). */
export interface BrokerAreaProject {
  projectId: string;
  projectName: string;
  gitRemote?: string;
  agentCount: number;
  localPath?: string;
}

@customElement('scion-broker-area-agents')
export class ScionBrokerAreaAgents extends LitElement {
  @property({ type: Array })
  projects: BrokerAreaProject[] = [];

  @property({ type: Array })
  agents: Agent[] = [];

  /** IDs of the projects whose default Runtime Broker is this broker. */
  @property({ attribute: false })
  defaultFor: ReadonlySet<string> = new Set();

  static override styles = brokerAreaStyles;

  private renderAgentCard(agent: Agent) {
    return html`
      <a href="/agents/${agent.id}" class="agent-card">
        <div class="agent-header">
          <div>
            <h3 class="agent-name">
              <sl-icon name="cpu"></sl-icon>
              <span>${agent.name}</span>
            </h3>
            <div class="agent-meta"><sl-icon name="code-square"></sl-icon> ${agent.template}</div>
          </div>
          ${agentStatusBadge(agent, { size: 'small' })}
        </div>
        ${agent.taskSummary ? html`<div class="agent-task">${agent.taskSummary}</div>` : nothing}
      </a>
    `;
  }

  override render() {
    if (this.projects.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="folder2-open"></sl-icon>
          <h2>No Projects</h2>
          <p>This broker is not currently providing for any projects.</p>
        </div>
      `;
    }
    return html`
      ${this.projects.map((project) => {
        const projectAgents = this.agents.filter((a) => a.projectId === project.projectId);
        return html`
          <div class="project-section" data-project=${project.projectId}>
            <div class="project-section-header">
              <sl-icon name="folder-fill"></sl-icon>
              <h3>
                <a href="/projects/${project.projectId}"
                  >${project.projectName || project.projectId}</a
                >
              </h3>
              <span class="project-agent-count"
                >${projectAgents.length} agent${projectAgents.length !== 1 ? 's' : ''}</span
              >
              ${this.defaultFor.has(project.projectId)
                ? html`<span class="kind-badge default-marker">Default for project</span>`
                : nothing}
            </div>
            ${projectAgents.length > 0
              ? html`<div class="agent-grid">
                  ${projectAgents.map((agent) => this.renderAgentCard(agent))}
                </div>`
              : nothing}
          </div>
        `;
      })}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-area-agents': ScionBrokerAreaAgents;
  }
}
