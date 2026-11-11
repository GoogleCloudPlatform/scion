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
 * Read-only "Runtime target" card of a flat Runtime Broker in the Brokers
 * area (broker UI Option A, slice A1). It shows the stored target, the
 * defaults its instance reported and, for a Kubernetes target, the
 * reported placement. Every row comes from shared/runtime-target.ts, which
 * names each published field explicitly and never walks the object.
 * Nothing here is editable from the Hub. A legacy broker has no card.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { RuntimeBroker } from '../../shared/types.js';
import {
  PRESENTATION_CAPTION,
  placementRows,
  presentationRows,
  runtimeTargetRows,
  type DisplayRow,
} from '../../shared/runtime-target.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { brokerAreaStyles } from './broker-area-styles.js';

@customElement('scion-broker-runtime-target-card')
export class ScionBrokerRuntimeTargetCard extends LitElement {
  /** Re-renders absolute times when the display timezone changes. */
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  broker: RuntimeBroker | null = null;

  static override styles = brokerAreaStyles;

  private renderRows(rows: DisplayRow[], cls: string) {
    return html`
      <dl class="runtime-target-rows ${cls}">
        ${rows.map(
          (r) => html`
            <dt>${r.label}</dt>
            <dd>${r.value}</dd>
          `
        )}
      </dl>
    `;
  }

  override render() {
    const broker = this.broker;
    const target = broker?.runtimeTarget;
    if (!broker || !target) return nothing;
    const presentation = broker.runtimeTargetPresentation;
    const observedAt = broker.runtimeTargetPresentationObservedAt;
    const observed = (observedAt && formatInstantWithZone(observedAt)) || '—';
    const placement = placementRows(target, presentation);
    return html`
      <div class="section runtime-target-card">
        <div class="section-header runtime-target-header">
          <sl-icon name="hdd-rack"></sl-icon>
          <h2>Runtime target</h2>
        </div>
        ${this.renderRows(runtimeTargetRows(target, presentation), 'target-rows')}
        <h3>Reported defaults</h3>
        ${presentation
          ? html`
              ${this.renderRows(presentationRows(presentation), 'presentation-rows')}
              ${placement.length > 0
                ? html`
                    <h3>Placement</h3>
                    ${this.renderRows(placement, 'placement-rows')}
                  `
                : nothing}
              <p class="presentation-caption">
                Reported by the instance at ${observed}. ${PRESENTATION_CAPTION}
              </p>
            `
          : html`<p class="presentation-caption">No defaults reported yet.</p>`}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-runtime-target-card': ScionBrokerRuntimeTargetCard;
  }
}
