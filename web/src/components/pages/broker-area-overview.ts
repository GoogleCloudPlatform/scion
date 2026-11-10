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
 * Overview tab of the Brokers area detail page (broker UI Option A, slice
 * A1): the runtime target card of a flat broker, a read view of its
 * capacity, its capabilities and, for a legacy broker, its profiles.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type {
  BrokerCapabilities,
  BrokerProfile,
  BrokerSettingsResponse,
  RuntimeBroker,
} from '../../shared/types.js';
import { effectiveLimitText, limitSourceLabel } from '../../shared/broker-capacity.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';
import '../shared/broker-runtime-target-card.js';

@customElement('scion-broker-area-overview')
export class ScionBrokerAreaOverview extends LitElement {
  @property({ type: Object })
  broker: RuntimeBroker | null = null;

  @property({ type: Object })
  settings: BrokerSettingsResponse | null = null;

  static override styles = brokerAreaStyles;

  private renderCapacity() {
    const effective = this.settings?.effective?.maxAgents;
    if (!effective) return nothing;
    return html`
      <div class="section capacity-section">
        <div class="section-header">
          <h2>Capacity</h2>
        </div>
        <p class="tab-note">
          Max concurrent agents: ${effectiveLimitText(effective.value)} (from
          ${limitSourceLabel(effective.source)})
          ${effective.count === undefined || effective.count === null
            ? nothing
            : html`· ${effective.count} counted now`}
        </p>
      </div>
    `;
  }

  private renderCapabilities(capabilities: BrokerCapabilities) {
    return html`
      <div class="section">
        <div class="section-header">
          <h2>Capabilities</h2>
        </div>
        <div class="capabilities-row">
          <span class="capability-tag ${capabilities.webPty ? 'enabled' : ''}">WebPTY</span>
          <span class="capability-tag ${capabilities.sync ? 'enabled' : ''}">Sync</span>
          <span class="capability-tag ${capabilities.attach ? 'enabled' : ''}">Attach</span>
        </div>
      </div>
    `;
  }

  private renderProfiles(profiles: BrokerProfile[]) {
    return html`
      <div class="section profiles-section">
        <div class="section-header">
          <h2>Profiles (legacy)</h2>
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

  override render() {
    const broker = this.broker;
    if (!broker) return nothing;
    return html`
      <scion-broker-runtime-target-card .broker=${broker}></scion-broker-runtime-target-card>
      ${this.renderCapacity()}
      ${broker.capabilities ? this.renderCapabilities(broker.capabilities) : nothing}
      ${!broker.runtimeTarget && broker.profiles && broker.profiles.length > 0
        ? this.renderProfiles(broker.profiles)
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-area-overview': ScionBrokerAreaOverview;
  }
}
