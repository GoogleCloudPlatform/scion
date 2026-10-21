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
 * Brokers area detail page (broker UI Option A, slice A1).
 *
 * Rendered for /brokers/{id} while the hub.flat_runtime_brokers experiment
 * is on (client/broker-routes.ts); the existing detail page renders
 * otherwise. Tabs (?tab=):
 *
 * - overview: runtime target card, capacity, capabilities, legacy profiles;
 * - agents: the projects this broker provides for, with their agents;
 * - health: shown only to a user holding hub.health.read, exactly the set
 *   the health summary endpoint authorizes; also hidden on a 401/403;
 * - configure: shown when the broker's capabilities allow update or delete.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { can } from '../../shared/types.js';
import type { Agent, BrokerSettingsResponse, PageData, RuntimeBroker } from '../../shared/types.js';
import type { HealthSummaryBroker } from './health-broker-table.js';
import { isFlatBroker } from '../../shared/runtime-target.js';
import { hostIdentityWarning } from '../../shared/broker-host-identity.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { hasAnyPermission } from '../../lib/admin-permissions.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import { replaceSearch } from '../../client/navigation.js';
import { stateManager } from '../../client/state.js';
import { brokerTypeBadgeStyles } from '../shared/resource-styles.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';
import { HEALTH_READ_PERMISSION } from '../../shared/broker-area-filters.js';
import type { BrokerAreaProject } from './broker-area-agents.js';
import '../shared/status-badge.js';
import '../shared/detail-header.js';
import './broker-area-overview.js';
import './broker-area-agents.js';
import './broker-area-health.js';
import './broker-area-configure.js';

/** Parallel project reads for the Default-for-project marker. */
const DEFAULTS_CONCURRENCY = 4;

export type BrokerAreaTab = 'overview' | 'agents' | 'health' | 'configure';

/** Parses ?tab=; anything unknown is "overview". */
export function parseBrokerAreaTab(value: string | null | undefined): BrokerAreaTab {
  switch (value) {
    case 'agents':
    case 'health':
    case 'configure':
      return value;
    default:
      return 'overview';
  }
}

@customElement('scion-page-broker-area-detail')
export class ScionPageBrokerAreaDetail extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  brokerId = '';

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private broker: RuntimeBroker | null = null;
  @state() private projects: BrokerAreaProject[] = [];
  @state() private agents: Agent[] = [];
  @state() private settings: BrokerSettingsResponse | null = null;
  @state() private defaultFor: ReadonlySet<string> = new Set();
  @state() private tab: BrokerAreaTab = 'overview';

  /** Whether the Health tab is shown (hub.health.read and a readable summary). */
  @state() private healthAvailable = false;
  @state() private healthRow: HealthSummaryBroker | null = null;
  @state() private healthTruncated = false;

  private boundOnBrokersUpdated = this.onBrokersUpdated.bind(this);

  static override styles = [brokerTypeBadgeStyles, brokerAreaStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.brokerId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/brokers\/([^/]+)/);
      if (match) this.brokerId = match[1];
    }
    if (typeof window !== 'undefined') {
      this.tab = parseBrokerAreaTab(new URLSearchParams(window.location.search).get('tab'));
    }
    void this.loadData();
    void this.loadHealth();
    if (this.brokerId) {
      stateManager.setScope({ type: 'broker-detail', brokerId: this.brokerId });
    }
    stateManager.addEventListener('brokers-updated', this.boundOnBrokersUpdated);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('brokers-updated', this.boundOnBrokersUpdated);
  }

  private onBrokersUpdated(): void {
    const updated = stateManager.getBroker(this.brokerId);
    if (updated && this.broker) {
      this.broker = { ...this.broker, ...updated };
    }
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const id = this.brokerId;
      const [brokerRes, projectsRes, agentsRes, settingsRes] = await Promise.all([
        apiFetch(`/api/v1/runtime-brokers/${id}`),
        apiFetch(`/api/v1/runtime-brokers/${id}/projects`),
        apiFetch(`/api/v1/agents?runtimeBrokerId=${id}`),
        apiFetch(`/api/v1/runtime-brokers/${id}/settings`),
      ]);
      if (!brokerRes.ok) {
        throw new Error(
          await extractApiError(brokerRes, `HTTP ${brokerRes.status}: ${brokerRes.statusText}`)
        );
      }
      this.broker = (await brokerRes.json()) as RuntimeBroker;
      dispatchPageTitle(this, this.broker.name || id, 'Brokers');

      if (projectsRes.ok) {
        const data = (await projectsRes.json()) as { projects?: BrokerAreaProject[] };
        this.projects = data.projects || [];
      }
      if (agentsRes.ok) {
        const data = (await agentsRes.json()) as { agents?: Agent[] } | Agent[];
        this.agents = Array.isArray(data) ? data : data.agents || [];
      }
      if (settingsRes.ok) {
        this.settings = (await settingsRes.json()) as BrokerSettingsResponse;
      }
      stateManager.seedBrokers([this.broker]);
      void this.loadDefaults();
    } catch (err) {
      console.error('Failed to load broker:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load broker';
    } finally {
      this.loading = false;
    }
  }

  /**
   * Marks the projects whose default Runtime Broker is this broker: one GET per
   * project, at most DEFAULTS_CONCURRENCY at a time (the providers listing does not
   * carry the default; adding it there is ptone/scion#5413).
   */
  private async loadDefaults(): Promise<void> {
    const queue = this.projects.map((p) => p.projectId);
    const ids = new Set<string>();
    const worker = async (): Promise<void> => {
      for (let id = queue.shift(); id !== undefined; id = queue.shift()) {
        try {
          const res = await fetch(`/api/v1/projects/${id}`, { credentials: 'include' });
          if (!res.ok) continue;
          const project = (await res.json()) as { defaultRuntimeBrokerId?: string };
          if (project.defaultRuntimeBrokerId === this.brokerId) ids.add(id);
        } catch {
          // A project that cannot be read simply gets no marker.
        }
      }
    };
    await Promise.all(Array.from({ length: DEFAULTS_CONCURRENCY }, () => worker()));
    this.defaultFor = ids;
  }

  /**
   * Loads this broker's health row, only for a user holding
   * hub.health.read. A 401/403 hides the Health tab.
   */
  private async loadHealth(): Promise<void> {
    const status = await loadAdminStatus(stateManager.getCurrentUserId());
    const canReadHealth = hasAnyPermission(status, [HEALTH_READ_PERMISSION]);
    if (!canReadHealth) return;
    try {
      const res = await fetch('/api/v1/admin/health/summary', { credentials: 'include' });
      // A 401/403, or any other failure, leaves the Health tab hidden.
      if (!res.ok) return;
      const body = (await res.json()) as {
        runtime_brokers?: { items?: HealthSummaryBroker[]; truncated?: boolean };
      };
      const list = body.runtime_brokers;
      this.healthRow = list?.items?.find((b) => b.id === this.brokerId) ?? null;
      this.healthTruncated = !!list?.truncated;
      this.healthAvailable = true;
    } catch (err) {
      console.error('Failed to load broker health:', err);
    }
  }

  private get configureVisible(): boolean {
    const caps = this.broker?._capabilities;
    return can(caps, 'update') || can(caps, 'delete');
  }

  private onTabShow(e: CustomEvent<{ name: string }>): void {
    this.tab = parseBrokerAreaTab(e.detail.name);
    const params = new URLSearchParams(window.location.search);
    if (this.tab === 'overview') {
      params.delete('tab');
    } else {
      params.set('tab', this.tab);
    }
    replaceSearch(params);
  }

  /** Configure saved and re-read the broker: the fresh record replaces the row. */
  private onBrokerUpdated(e: CustomEvent<RuntimeBroker>): void {
    if (!this.broker) return;
    this.broker = e.detail;
    stateManager.seedBrokers([this.broker]);
    dispatchPageTitle(this, this.broker.name || this.brokerId, 'Brokers');
  }

  private onSettingsUpdated(e: CustomEvent<BrokerSettingsResponse>): void {
    this.settings = e.detail;
  }

  /**
   * A deep link to ?tab=health: the Health tab exists only once the health load
   * resolves, after the first render, so show it then.
   */
  override updated(changed: Map<PropertyKey, unknown>): void {
    if (changed.has('healthAvailable') && this.healthAvailable && this.tab === 'health') {
      const group = this.shadowRoot?.querySelector('sl-tab-group') as
        | (HTMLElement & { show?: (panel: string) => void })
        | null;
      group?.show?.('health');
    }
  }

  private getStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'online':
        return 'success';
      case 'degraded':
        return 'warning';
      case 'error':
        return 'danger';
      default:
        return 'neutral';
    }
  }

  private renderBrokerTypeBadge(broker: RuntimeBroker) {
    const brokerType = broker.labels?.['scion.io/broker-type'];
    if (!brokerType) return nothing;
    const label = brokerType.charAt(0).toUpperCase() + brokerType.slice(1);
    const cssClass = brokerType === 'hosted' ? 'broker-type-badge hosted' : 'broker-type-badge';
    return html`<span class=${cssClass}>${label}</span>`;
  }

  /**
   * The host identity warning (mismatch or pinned_absent), shown above the
   * tabs whatever tab is open; account names only for callers allowed to
   * update (hostIdentityWarning).
   */
  private renderHostIdentityAlert(broker: RuntimeBroker) {
    const warning = hostIdentityWarning(broker);
    if (!warning) return nothing;
    const where = can(broker._capabilities, 'update') ? ' Change it on the Configure tab.' : '';
    return html`
      <sl-alert variant="warning" open class="host-identity-alert">
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        <strong>${warning.message}</strong>
        ${warning.configured ? html`<div>Configured: ${warning.configured}</div>` : nothing}
        ${warning.observed ? html`<div>Host reports: ${warning.observed}</div>` : nothing}
        <div>The configured value is used.${where}</div>
      </sl-alert>
    `;
  }

  override render() {
    if (this.loading) {
      return html`<div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading broker...</p>
      </div>`;
    }
    const broker = this.broker;
    if (this.error || !broker) {
      return html`
        <scion-back-link href="/brokers">Back to Brokers</scion-back-link>
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Broker</h2>
          <div class="error-details">${this.error || 'Broker not found'}</div>
          <sl-button variant="primary" @click=${() => this.loadData()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }
    const flat = isFlatBroker(broker);
    const subtitle = [broker.version ? `v${broker.version}` : '', broker.endpoint ?? '']
      .filter((s) => s)
      .join(' · ');
    const tab =
      (this.tab === 'health' && !this.healthAvailable) ||
      (this.tab === 'configure' && !this.configureVisible)
        ? 'overview'
        : this.tab;
    return html`
      <scion-detail-header heading=${broker.name}>
        <scion-back-link slot="back" href="/brokers">Back to Brokers</scion-back-link>
        <sl-icon slot="icon" name="hdd-rack"></sl-icon>
        <span class="kind-badge ${flat ? 'flat' : ''}">${flat ? 'Flat' : 'Legacy'}</span>
        ${broker.hostedByThisServer
          ? html`<sl-icon
              class="hosted-marker"
              name="house"
              label="Runs in this server's process"
              title="Runs in this server's process"
            ></sl-icon>`
          : nothing}
        ${this.renderBrokerTypeBadge(broker)}
        <scion-status-badge
          status=${this.getStatusVariant(broker.status)}
          label="Connection: ${broker.status}"
          size="small"
        ></scion-status-badge>
        ${subtitle ? html`<div slot="meta" class="header-subtitle">${subtitle}</div>` : nothing}
      </scion-detail-header>

      ${this.renderHostIdentityAlert(broker)}

      <sl-tab-group @sl-tab-show=${this.onTabShow}>
        <sl-tab slot="nav" panel="overview" ?active=${tab === 'overview'}>Overview</sl-tab>
        <sl-tab slot="nav" panel="agents" ?active=${tab === 'agents'}>Agents & projects</sl-tab>
        ${this.healthAvailable
          ? html`<sl-tab slot="nav" panel="health" ?active=${tab === 'health'}>
              <sl-icon name="heart-pulse"></sl-icon> Health
            </sl-tab>`
          : nothing}
        ${this.configureVisible
          ? html`<sl-tab slot="nav" panel="configure" ?active=${tab === 'configure'}>
              <sl-icon name="sliders"></sl-icon> Configure
            </sl-tab>`
          : nothing}

        <sl-tab-panel name="overview">
          <scion-broker-area-overview
            .broker=${broker}
            .settings=${this.settings}
          ></scion-broker-area-overview>
        </sl-tab-panel>
        <sl-tab-panel name="agents">
          <scion-broker-area-agents
            .projects=${this.projects}
            .agents=${this.agents}
            .defaultFor=${this.defaultFor}
          ></scion-broker-area-agents>
        </sl-tab-panel>
        ${this.healthAvailable
          ? html`<sl-tab-panel name="health">
              <scion-broker-area-health
                .row=${this.healthRow}
                ?truncated=${this.healthTruncated}
              ></scion-broker-area-health>
            </sl-tab-panel>`
          : nothing}
        ${this.configureVisible
          ? html`<sl-tab-panel name="configure">
              <scion-broker-area-configure
                .broker=${broker}
                .settings=${this.settings}
                .projectCount=${this.projects.length}
                .agentCount=${this.agents.length}
                @broker-updated=${this.onBrokerUpdated}
                @broker-settings-updated=${this.onSettingsUpdated}
              ></scion-broker-area-configure>
            </sl-tab-panel>`
          : nothing}
      </sl-tab-group>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-broker-area-detail': ScionPageBrokerAreaDetail;
  }
}
