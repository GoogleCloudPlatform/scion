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

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import type { PageData, Agent } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import type { Orientation } from '../../shared/lineage.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/view-toggle.js';
import '../shared/agent-tree-view.js';

/**
 * Standalone agent graph page — fetches its own data and owns a cross-project
 * filter dropdown. Delegates all graph rendering to <scion-agent-tree-view>.
 *
 * This page remains useful for deep-linkable, cross-project graph views
 * (e.g. /agents/graph?project=<id>&focus=<agent-id>).  When the user picks
 * grid or list from the toggle the page navigates back to the appropriate
 * agents page.
 */
@customElement('scion-page-agent-graph')
export class AgentGraphPage extends LitElement {
  @property({ type: Object }) pageData?: PageData;

  @state() private agents: Agent[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private projectFilter = '';
  /** Graph flow direction, persisted in the URL as ?dir=horizontal (absent = vertical). */
  @state() private orientation: Orientation = 'vertical';
  /** True when the page was entered with a ?project= param already set in the URL. */
  private initiallyProjectScoped = false;
  /** Agent ID to center on load (?focus=<agent-id> deep link) */
  private focusId = '';

  private boundOnAgentsUpdated = () => this.onAgentsUpdated();
  private refetchTimer: number | undefined;

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.setScope({ type: 'dashboard' });

    const params = new URLSearchParams(window.location.search);
    this.projectFilter = params.get('project') || '';
    this.initiallyProjectScoped = !!params.get('project');
    this.focusId = params.get('focus') || '';
    this.orientation = params.get('dir') === 'horizontal' ? 'horizontal' : 'vertical';

    const hydrated = stateManager.getAgents();
    if (hydrated.length > 0) {
      this.agents = hydrated;
      this.loading = false;
    } else {
      void this.loadAgents();
    }

    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    if (this.refetchTimer !== undefined) {
      window.clearTimeout(this.refetchTimer);
      this.refetchTimer = undefined;
    }
  }

  private onAgentsUpdated(): void {
    const updated = stateManager.getAgents();
    const prev = new Map(this.agents.map((a) => [a.id, a]));
    let ancestryGap = false;
    this.agents = updated.map((a) => {
      if (a.ancestry && a.ancestry.length > 0) return a;
      const old = prev.get(a.id);
      if (old?.ancestry?.length) return { ...a, ancestry: old.ancestry };
      ancestryGap = true;
      return a;
    });
    if (ancestryGap && this.refetchTimer === undefined) {
      this.refetchTimer = window.setTimeout(() => {
        this.refetchTimer = undefined;
        void this.fetchAgents(true);
      }, 800);
    }
  }

  private async loadAgents(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      await this.fetchAgents(false);
    } finally {
      this.loading = false;
    }
  }

  private async fetchAgents(quiet: boolean): Promise<void> {
    try {
      const response = await apiFetch('/api/v1/agents');
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }
      const data = (await response.json()) as { agents?: Agent[] } | Agent[];
      this.agents = Array.isArray(data) ? data : data.agents || [];
      stateManager.seedAgents(this.agents);
    } catch (err) {
      console.error('Failed to load agents:', err);
      if (!quiet) {
        this.error = err instanceof Error ? err.message : 'Failed to load agents';
      }
    }
  }

  /** Agents after the project filter is applied */
  private get visibleAgents(): Agent[] {
    if (!this.projectFilter) return this.agents;
    return this.agents.filter((a) => a.projectId === this.projectFilter);
  }

  private get projects(): Array<{ id: string; name: string }> {
    const seen = new Map<string, string>();
    for (const agent of this.agents) {
      if (agent.projectId && !seen.has(agent.projectId)) {
        seen.set(agent.projectId, agent.project || agent.projectId);
      }
    }
    return Array.from(seen, ([id, name]) => ({ id, name })).sort((a, b) =>
      a.name.localeCompare(b.name)
    );
  }

  private onProjectFilterChange(e: Event): void {
    const value = (e.target as HTMLSelectElement & { value: string }).value;
    this.projectFilter = value;
    const url = new URL(window.location.href);
    if (value) {
      url.searchParams.set('project', value);
    } else {
      url.searchParams.delete('project');
    }
    window.history.replaceState({}, '', url);
  }

  /** Keeps the URL's ?dir= param in sync with the graph orientation toggle. */
  private onOrientationChange(e: CustomEvent<{ orientation: Orientation }>): void {
    this.orientation = e.detail.orientation;
    const url = new URL(window.location.href);
    if (this.orientation === 'horizontal') {
      url.searchParams.set('dir', 'horizontal');
    } else {
      url.searchParams.delete('dir');
    }
    window.history.replaceState({}, '', url);
  }

  /** Grid/list picks from the toggle navigate back to the agents list. */
  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    const mode = e.detail.view;
    if (mode === 'graph') return;
    if (this.initiallyProjectScoped && this.projectFilter) {
      localStorage.setItem('scion-view-project-agents', mode);
      window.history.pushState({}, '', `/projects/${this.projectFilter}`);
    } else {
      localStorage.setItem('scion-view-agents', mode);
      window.history.pushState({}, '', '/agents');
    }
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  override render() {
    return html`
      <div style="padding: var(--sl-spacing-large, 1.25rem);">
        <div
          style="display: flex; align-items: center; justify-content: space-between; gap: 1rem; margin-bottom: 1rem; flex-wrap: wrap;"
        >
          <h1 style="margin: 0; font-size: 1.5rem;">Agents</h1>
          <div style="display: flex; align-items: center; gap: 0.75rem; flex-wrap: wrap;">
            ${this.initiallyProjectScoped
              ? nothing
              : html`
                  <sl-select
                    size="small"
                    placeholder="All projects"
                    clearable
                    value=${this.projectFilter}
                    @sl-change=${this.onProjectFilterChange}
                    style="min-width: 180px"
                  >
                    ${this.projects.map(
                      (p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`
                    )}
                  </sl-select>
                `}
            <scion-view-toggle view="graph" @view-change=${this.onViewChange}></scion-view-toggle>
          </div>
        </div>
        ${this.loading
          ? html`
              <div
                style="display: flex; flex-direction: column; align-items: center; gap: 0.75rem; padding: 3rem 1rem; color: var(--sl-color-neutral-600); text-align: center;"
              >
                <sl-spinner></sl-spinner>
                <p>Loading agents...</p>
              </div>
            `
          : this.error
            ? html`
                <div
                  style="display: flex; flex-direction: column; align-items: center; gap: 0.75rem; padding: 3rem 1rem; color: var(--sl-color-danger-600); text-align: center;"
                >
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <p>${this.error}</p>
                  <sl-button size="small" @click=${() => this.loadAgents()}>Retry</sl-button>
                </div>
              `
            : html`
                <scion-agent-tree-view
                  .agents=${this.visibleAgents}
                  focusId=${this.focusId}
                  orientation=${this.orientation}
                  @orientation-change=${this.onOrientationChange}
                ></scion-agent-tree-view>
              `}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-graph': AgentGraphPage;
  }
}
