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
 * Project Triggers page (`/projects/:id/triggers`, Stage 2 §9.1, GAR G7).
 *
 * Gated on `hub.triggers`: renders `<scion-page-404>` and makes zero API
 * requests while the flag is off.
 *
 * Supports deep links: `?tab=endpoints|rules|events|history&new=1&source=<kind>&preset=<id>`.
 * Renders registered per-source cards in the Sources header above the tabs.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { current, projectTrail, projectsRoot, type Named } from '../../client/crumbs.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { projectDisplayName, projectLabelNow } from '../../client/project-label.js';
import { FEATURE_FLAGS_CHANGED_EVENT, triggersEnabled } from '../../utils/feature-flags.js';
import '../shared/breadcrumb.js';
import './not-found.js';
import {
  ensureExtensionLoaded,
  getRegisteredSourceCards,
  type ProjectAgentOption,
  type TriggerDefinition,
  type WebhookEndpoint,
} from '../triggers/index.js';
import '../triggers/webhook-endpoint-list.js';
import '../triggers/trigger-list.js';
import '../triggers/trigger-event-list.js';
import '../triggers/trigger-fire-list.js';
import type { ScionTriggerEventList } from '../triggers/trigger-event-list.js';
import type { ScionTriggerList } from '../triggers/trigger-list.js';

interface ProjectSummary {
  id: string;
  name: string;
  slug?: string;
}

type TriggersTab = 'endpoints' | 'rules' | 'events' | 'history';

const VALID_TABS = new Set<TriggersTab>(['endpoints', 'rules', 'events', 'history']);

@customElement('scion-page-project-triggers')
export class ScionPageProjectTriggers extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state() private enabled = false;
  @state() private project: ProjectSummary | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;

  @state() private activeTab: TriggersTab = 'endpoints';
  @state() private pendingCreateSource = '';
  @state() private pendingCreatePreset = '';

  @state() private agents: ProjectAgentOption[] = [];
  @state() private endpoints: WebhookEndpoint[] = [];
  @state() private triggers: TriggerDefinition[] = [];

  private boundOnFeatureFlagsChanged = (): void => {
    const next = triggersEnabled();
    if (next === this.enabled) return;
    this.enabled = next;
    if (next) {
      this.bootWhenEnabled();
    }
  };

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.375rem;
    }

    .header sl-icon {
      font-size: 1.375rem;
      color: var(--scion-primary);
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text);
      margin: 0;
    }

    .subtitle {
      font-size: 0.875rem;
      color: var(--scion-text-muted);
      margin: 0 0 1.25rem 0;
    }

    .sources-header {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
      gap: 1rem;
      margin-bottom: 1.25rem;
    }

    .tab-panel-body {
      padding-top: 1rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface);
      border: 1px solid var(--sl-color-danger-200);
      border-radius: var(--scion-radius-lg);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text);
      margin: 0 0 0.5rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius);
      color: var(--sl-color-danger-700);
      margin-bottom: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    this.enabled = triggersEnabled();
    if (this.enabled) {
      this.bootWhenEnabled();
    } else {
      this.loading = false;
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
  }

  private bootWhenEnabled(): void {
    this.extractRouteParams();
    for (const { entry } of getRegisteredSourceCards()) {
      if (entry.load) {
        void ensureExtensionLoaded(entry).then(() => this.requestUpdate());
      }
    }
    void this.loadProjectAndAgents();
  }

  private extractRouteParams(): void {
    if (!this.projectId) {
      const path = this.pageData?.path || window.location.pathname;
      const match = path.match(/\/projects\/([^/]+)\/triggers/);
      if (match?.[1]) {
        try {
          this.projectId = decodeURIComponent(match[1]);
        } catch {
          this.error = 'Invalid project ID in URL';
        }
      }
    }

    const params = new URLSearchParams(window.location.search);
    const tabParam = params.get('tab') as TriggersTab | null;
    if (tabParam && VALID_TABS.has(tabParam)) {
      this.activeTab = tabParam;
    }
    if (params.get('new') === '1') {
      this.activeTab = 'rules';
      this.pendingCreateSource = params.get('source') || 'webhook';
      this.pendingCreatePreset = params.get('preset') || '';
    }
  }

  private async loadProjectAndAgents(): Promise<void> {
    if (!this.projectId) {
      this.error = 'No project ID found in URL';
      this.loading = false;
      return;
    }

    this.loading = true;
    this.error = null;
    try {
      const projRes = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`);
      if (!projRes.ok) {
        throw new Error(await extractApiError(projRes, `HTTP ${projRes.status}`));
      }
      if (!this.isConnected) return;
      this.project = (await projRes.json()) as ProjectSummary;
      dispatchPageTitle(this, 'Triggers', this.project.name || this.projectId);

      // Non-blocking best-effort fetch of project agents for action/binding pickers.
      void this.loadAgents();
    } catch (err) {
      if (!this.isConnected) return;
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    } finally {
      this.loading = false;
    }
  }

  private async loadAgents(): Promise<void> {
    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/agents?limit=200`
      );
      if (!res.ok) return;
      const data = (await res.json()) as {
        agents?: Array<{ id: string; name: string; status?: string }>;
      };
      if (!this.isConnected) return;
      this.agents = Array.isArray(data.agents)
        ? data.agents.map((a) =>
            a.status !== undefined
              ? { id: a.id, name: a.name, status: a.status }
              : { id: a.id, name: a.name }
          )
        : [];
    } catch {
      // Non-blocking: fall back to free-text agent input if unavailable.
    }
  }

  private handleNavigateToCreate(opts: { sourceKind: string; presetId?: string }): void {
    this.activeTab = 'rules';
    this.pendingCreateSource = opts.sourceKind;
    this.pendingCreatePreset = opts.presetId ?? '';
    const tabGroup = this.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    tabGroup?.show?.('rules');
    const ruleList = this.shadowRoot?.querySelector(
      'scion-trigger-list'
    ) as ScionTriggerList | null;
    ruleList?.openCreateDialog(opts.sourceKind, opts.presetId ?? '');
  }

  private handleInspectEvent(eventId: string): void {
    this.activeTab = 'events';
    const tabGroup = this.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    tabGroup?.show?.('events');
    const eventList = this.shadowRoot?.querySelector(
      'scion-trigger-event-list'
    ) as ScionTriggerEventList | null;
    void eventList?.openEventDetail(eventId);
  }

  private handleTriggersRefresh(): void {
    const ruleList = this.shadowRoot?.querySelector(
      'scion-trigger-list'
    ) as ScionTriggerList | null;
    void ruleList?.loadTriggers();
  }

  private projectNamed(allowPending: boolean): Named | null {
    if (!this.projectId) return null;
    if (this.project) {
      return {
        id: this.projectId,
        label: projectDisplayName(this.project, this.projectId),
      };
    }
    const cached = projectLabelNow(this.projectId);
    return allowPending ? cached : { id: cached.id, label: cached.label };
  }

  private ancestorCrumbs(allowPending: boolean) {
    const project = this.projectNamed(allowPending);
    return project ? projectTrail(project) : [projectsRoot()];
  }

  override render() {
    if (!this.enabled) {
      return html`<scion-page-404></scion-page-404>`;
    }

    if (this.loading) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(true)}></scion-breadcrumb>
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading triggers...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(false)}></scion-breadcrumb>
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadProjectAndAgents()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    const sourceCards = getRegisteredSourceCards();

    return html`
      <scion-breadcrumb
        .crumbs=${[...this.ancestorCrumbs(false), current('Triggers')]}
      ></scion-breadcrumb>

      <div class="header">
        <sl-icon name="lightning-charge"></sl-icon>
        <h1>Triggers</h1>
      </div>
      <p class="subtitle">
        Manage webhook endpoints, event-driven trigger rules, incoming events, and execution history
        for ${this.project?.name || this.projectId}.
      </p>

      ${sourceCards.length > 0
        ? html`
            <div class="sources-header" data-sources-header>
              ${sourceCards.map(
                ({ sourceKind, entry }) => html`
                  <div data-source-card=${sourceKind}>
                    ${entry.render({
                      projectId: this.projectId,
                      onNavigateToCreate: (opts) => this.handleNavigateToCreate(opts),
                    })}
                  </div>
                `
              )}
            </div>
          `
        : nothing}

      <sl-tab-group
        @sl-tab-show=${(e: CustomEvent<{ name: string }>) => {
          if (e.target !== e.currentTarget) return;
          const name = e.detail?.name as TriggersTab | undefined;
          if (name && VALID_TABS.has(name)) {
            this.activeTab = name;
          }
        }}
      >
        <sl-tab slot="nav" panel="endpoints" ?active=${this.activeTab === 'endpoints'}>
          Endpoints
        </sl-tab>
        <sl-tab slot="nav" panel="rules" ?active=${this.activeTab === 'rules'}> Rules </sl-tab>
        <sl-tab slot="nav" panel="events" ?active=${this.activeTab === 'events'}> Events </sl-tab>
        <sl-tab slot="nav" panel="history" ?active=${this.activeTab === 'history'}>
          History
        </sl-tab>

        <sl-tab-panel name="endpoints" ?active=${this.activeTab === 'endpoints'}>
          <div class="tab-panel-body">
            <scion-webhook-endpoint-list
              .projectId=${this.projectId}
              .agents=${this.agents}
              .triggers=${this.triggers}
              @endpoints-loaded=${(e: CustomEvent<{ endpoints: WebhookEndpoint[] }>) => {
                this.endpoints = e.detail.endpoints;
              }}
              @triggers-refresh-requested=${() => this.handleTriggersRefresh()}
            ></scion-webhook-endpoint-list>
          </div>
        </sl-tab-panel>

        <sl-tab-panel name="rules" ?active=${this.activeTab === 'rules'}>
          <div class="tab-panel-body">
            <scion-trigger-list
              .projectId=${this.projectId}
              .endpoints=${this.endpoints}
              .agents=${this.agents}
              .pendingCreateSource=${this.pendingCreateSource}
              .pendingCreatePreset=${this.pendingCreatePreset}
              @triggers-loaded=${(e: CustomEvent<{ triggers: TriggerDefinition[] }>) => {
                this.triggers = e.detail.triggers;
              }}
            ></scion-trigger-list>
          </div>
        </sl-tab-panel>

        <sl-tab-panel name="events" ?active=${this.activeTab === 'events'}>
          <div class="tab-panel-body">
            <scion-trigger-event-list
              .projectId=${this.projectId}
              .endpoints=${this.endpoints}
            ></scion-trigger-event-list>
          </div>
        </sl-tab-panel>

        <sl-tab-panel name="history" ?active=${this.activeTab === 'history'}>
          <div class="tab-panel-body">
            <scion-trigger-fire-list
              .projectId=${this.projectId}
              .triggers=${this.triggers}
              @inspect-event=${(e: CustomEvent<{ eventId: string }>) => {
                this.handleInspectEvent(e.detail.eventId);
              }}
            ></scion-trigger-fire-list>
          </div>
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-triggers': ScionPageProjectTriggers;
  }
}
