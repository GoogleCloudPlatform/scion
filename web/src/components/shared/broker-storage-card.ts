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
 * Read-only "Project storage" card of a broker in the Brokers area. It shows
 * the storage layout the broker reported and, for users who can read the
 * health summary, how that layout differs from the hub's. Storage comes from
 * the broker's settings.yaml or its deployment tooling and is not edited from
 * this page, so nothing here is editable.
 */

import { LitElement, html, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

import type { BrokerWorkspaceStorage } from '../../shared/types.js';
import { mismatchLines, storageRows } from '../../shared/broker-storage.js';
import { brokerAreaStyles } from './broker-area-styles.js';

/** The note on where project storage is configured. */
export const STORAGE_SOURCE_NOTE =
  "As reported by the broker. Project storage is set in the broker's settings.yaml " +
  '(server.project_storage) or its deployment tooling; a change needs a restart.';

@customElement('scion-broker-storage-card')
export class ScionBrokerStorageCard extends LitElement {
  /** The broker's reported layout; undefined when it reported none. */
  @property({ type: Object })
  storage: BrokerWorkspaceStorage | null | undefined = undefined;

  /**
   * The hub's layout comparison lines; null when the comparison is not
   * available (for example no health summary access, or no summary row for
   * this broker).
   */
  @property({ type: Array })
  mismatch: string[] | null = null;

  static override styles = brokerAreaStyles;

  private renderMismatch(): TemplateResult | typeof nothing {
    const lines = mismatchLines(this.mismatch);
    if (lines.length === 0) return nothing;
    return html`
      <sl-alert variant="warning" open class="storage-mismatch-alert">
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        <strong>Storage layout differs from the hub</strong>
        <div>Shared-mode agent starts on this broker are refused until they match.</div>
        <ul class="storage-mismatch">
          ${lines.map((l) => html`<li>${l}</li>`)}
        </ul>
      </sl-alert>
    `;
  }

  override render(): TemplateResult {
    const rows = storageRows(this.storage);
    return html`
      <div class="section runtime-target-card storage-card">
        <div class="section-header runtime-target-header">
          <sl-icon name="hdd-rack"></sl-icon>
          <h2>Project storage</h2>
        </div>
        ${this.renderMismatch()}
        ${rows.length > 0
          ? html`
              <dl class="runtime-target-rows storage-rows">
                ${rows.map(
                  (r) => html`
                    <dt>${r.label}</dt>
                    <dd>${r.value}</dd>
                  `
                )}
              </dl>
            `
          : html`<p class="tab-note storage-none">No storage layout reported (older broker).</p>`}
        ${this.mismatch === null
          ? html`<p class="presentation-caption storage-compare-note">
              The comparison with the hub's layout is not available.
            </p>`
          : nothing}
        <p class="presentation-caption storage-source-note">${STORAGE_SOURCE_NOTE}</p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-broker-storage-card': ScionBrokerStorageCard;
  }
}
