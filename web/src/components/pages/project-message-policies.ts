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
 * Project Message Policies page (`/projects/:id/message-policies`, U1 read-only).
 *
 * Gated ONLY on `hub.message_policies`: renders `<scion-page-404>` and makes
 * zero API calls while the flag is off. `hub.system_one` off hides only
 * System One columns and fields.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { can, canAny, type Capabilities, type PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { paginateAll } from '../../client/paginate-all.js';
import { current, projectTrail, projectsRoot, type Named } from '../../client/crumbs.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { projectDisplayName, projectLabelNow } from '../../client/project-label.js';
import {
  FEATURE_FLAGS_CHANGED_EVENT,
  messagePoliciesEnabled,
  systemOneEnabled,
} from '../../utils/feature-flags.js';
import '../shared/breadcrumb.js';
import './not-found.js';
import '../message-policies/policy-list.js';
import '../message-policies/policy-outcomes.js';
import type { ScionMessagePolicyOutcomes } from '../message-policies/policy-outcomes.js';
import type { MessagePolicy, ProjectAgentSummary } from '../message-policies/types.js';

interface ProjectSummary {
  id: string;
  name: string;
  slug?: string;
  _capabilities?: Capabilities;
}

type MessagePoliciesTab = 'policies' | 'outcomes';

const VALID_TABS = new Set<MessagePoliciesTab>(['policies', 'outcomes']);

@customElement('scion-page-project-message-policies')
export class ScionPageProjectMessagePolicies extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state() private enabled = false;
  @state() private systemOneOn = false;
  @state() private project: ProjectSummary | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private forbidden = false;

  @state() private activeTab: MessagePoliciesTab = 'policies';
  @state() private outcomePolicyFilter = '';
  @state() private policies: MessagePolicy[] = [];
  @state() private agents: ProjectAgentSummary[] = [];

  private boundOnFeatureFlagsChanged = (): void => {
    const nextEnabled = messagePoliciesEnabled();
    const nextSystemOne = systemOneEnabled();
    const wasEnabled = this.enabled;
    this.enabled = nextEnabled;
    this.systemOneOn = nextSystemOne;
    if (nextEnabled && !wasEnabled) {
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
      border: 1px solid var(--scion-error-border);
      border-radius: var(--scion-radius-lg);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--scion-status-danger);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted);
      margin: 0 auto 1rem auto;
      max-width: 520px;
    }

    .error-details {
      font-family: var(--scion-font-mono);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius);
      color: var(--scion-error-text);
      margin-bottom: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    this.enabled = messagePoliciesEnabled();
    this.systemOneOn = systemOneEnabled();
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
    void this.loadProject();
  }

  private extractRouteParams(): void {
    if (!this.projectId) {
      const path = this.pageData?.path || window.location.pathname;
      const match = path.match(/\/projects\/([^/]+)\/message-policies/);
      if (match?.[1]) {
        try {
          this.projectId = decodeURIComponent(match[1]);
        } catch {
          this.error = 'Invalid project ID in URL';
        }
      }
    }

    const params = new URLSearchParams(window.location.search);
    const tabParam = params.get('tab') as MessagePoliciesTab | null;
    if (tabParam && VALID_TABS.has(tabParam)) {
      this.activeTab = tabParam;
    }
    const policyParam = params.get('policy');
    if (policyParam) {
      this.outcomePolicyFilter = policyParam;
    }
  }

  private async loadProject(): Promise<void> {
    if (!this.projectId) {
      this.error = 'No project ID found in URL';
      this.loading = false;
      return;
    }

    this.loading = true;
    this.error = null;
    this.forbidden = false;

    try {
      const projRes = await apiFetch(`/api/v1/projects/${encodeURIComponent(this.projectId)}`);
      if (!this.isConnected) return;
      if (projRes.status === 403) {
        this.forbidden = true;
        return;
      }
      if (!projRes.ok) {
        throw new Error(await extractApiError(projRes, `HTTP ${projRes.status}`));
      }

      const proj = (await projRes.json()) as ProjectSummary;
      if (!this.isConnected) return;
      this.project = proj;

      if (
        proj._capabilities &&
        !canAny(proj._capabilities, 'read_message_policies', 'manage_message_policies')
      ) {
        this.forbidden = true;
        return;
      }

      dispatchPageTitle(this, 'Message Policies', proj.name || this.projectId);
      void this.loadAgents();
    } catch (err) {
      if (!this.isConnected) return;
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    } finally {
      if (this.isConnected) {
        this.loading = false;
      }
    }
  }

  private async loadAgents(): Promise<void> {
    try {
      const items = await paginateAll<ProjectAgentSummary>({
        path: `/api/v1/projects/${encodeURIComponent(this.projectId)}/agents`,
        pageSize: 100,
        parsePage: (raw) => {
          const data = raw as {
            agents?: Array<{ id: string; name: string }>;
            nextCursor?: string;
          };
          const pageItems = Array.isArray(data.agents)
            ? data.agents.map((a) => ({ id: a.id, name: a.name }))
            : [];
          return data.nextCursor
            ? { items: pageItems, nextCursor: data.nextCursor }
            : { items: pageItems };
        },
      });
      if (!this.isConnected) return;
      this.agents = items;
    } catch {
      // Non-blocking: fall back to raw agent IDs if agent list is unavailable.
    }
  }

  private get canManagePolicies(): boolean {
    return can(this.project?._capabilities, 'manage_message_policies');
  }

  private handleViewPolicyOutcomes(policyId: string): void {
    this.outcomePolicyFilter = policyId;
    this.activeTab = 'outcomes';
    const tabGroup = this.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    tabGroup?.show?.('outcomes');
    const outcomesEl = this.shadowRoot?.querySelector(
      'scion-message-policy-outcomes'
    ) as ScionMessagePolicyOutcomes | null;
    outcomesEl?.filterByPolicy(policyId);
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
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.loading) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(true)}></scion-breadcrumb>
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading message policies...</p>
        </div>
      `;
    }

    if (this.forbidden) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(false)}></scion-breadcrumb>
        <div class="error-state" role="alert" data-page-forbidden>
          <sl-icon name="shield-x"></sl-icon>
          <h2>Access Denied</h2>
          <p>
            You do not have permission to view message policies for this project (<code
              >project.read_message_policies</code
            >
            required).
          </p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <scion-breadcrumb .crumbs=${this.ancestorCrumbs(false)}></scion-breadcrumb>
        <div class="error-state" data-page-error>
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadProject()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <scion-breadcrumb
        .crumbs=${[...this.ancestorCrumbs(false), current('Message Policies')]}
      ></scion-breadcrumb>

      <div class="header">
        <sl-icon name="shield-check"></sl-icon>
        <h1>Message Policies</h1>
      </div>
      <p class="subtitle">
        Outbound agent message admission policies and content-free policy outcomes for
        ${this.project?.name || this.projectId}.
      </p>

      <sl-tab-group
        @sl-tab-show=${(e: CustomEvent<{ name: string }>) => {
          if (e.target !== e.currentTarget) return;
          const name = e.detail?.name as MessagePoliciesTab | undefined;
          if (name && VALID_TABS.has(name)) {
            this.activeTab = name;
          }
        }}
      >
        <sl-tab slot="nav" panel="policies" ?active=${this.activeTab === 'policies'}>
          Policies
        </sl-tab>
        <sl-tab slot="nav" panel="outcomes" ?active=${this.activeTab === 'outcomes'}>
          Policy outcomes
        </sl-tab>

        <sl-tab-panel name="policies" ?active=${this.activeTab === 'policies'}>
          <div class="tab-panel-body">
            <scion-message-policy-list
              .projectId=${this.projectId}
              .systemOneEnabled=${this.systemOneOn}
              .canManage=${this.canManagePolicies}
              .agents=${this.agents}
              @policies-loaded=${(
                e: CustomEvent<{ policies: MessagePolicy[]; filtered?: boolean }>
              ) => {
                if (!e.detail.filtered || this.policies.length === 0) {
                  this.policies = e.detail.policies;
                }
              }}
              @view-policy-outcomes=${(e: CustomEvent<{ policyId: string }>) => {
                this.handleViewPolicyOutcomes(e.detail.policyId);
              }}
            ></scion-message-policy-list>
          </div>
        </sl-tab-panel>

        <sl-tab-panel name="outcomes" ?active=${this.activeTab === 'outcomes'}>
          <div class="tab-panel-body">
            <scion-message-policy-outcomes
              .projectId=${this.projectId}
              .systemOneEnabled=${this.systemOneOn}
              .policies=${this.policies}
              .agents=${this.agents}
              .initialPolicyId=${this.outcomePolicyFilter}
            ></scion-message-policy-outcomes>
          </div>
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-message-policies': ScionPageProjectMessagePolicies;
  }
}
