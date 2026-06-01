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
 * Hub Resources page component
 *
 * Displays hub-scoped resources (environment variables, secrets) and the
 * global file-based resources (templates, harness configs). Structured to
 * mirror the project settings Resources section for consistency.
 */

import { LitElement, html, css } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import '../shared/env-var-list.js';
import '../shared/secret-list.js';
import '../shared/resource-list.js';
import '../shared/resource-import.js';

@customElement('scion-page-settings')
export class ScionPageSettings extends LitElement {
  @state()
  private activeTab = 'env-vars';

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section > p {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .tab-intro {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(base) {
      background: transparent;
    }

    sl-tab-panel::part(base) {
      padding: 1.5rem 0 0 0;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    // Deep-link a specific tab via ?tab= (e.g. ?tab=templates), used by the
    // resource detail pages' "back" links.
    if (typeof window !== 'undefined') {
      const tab = new URLSearchParams(window.location.search).get('tab');
      if (tab) {
        this.activeTab = tab;
      }
    }
  }

  /** Refresh a resource list (by element id) after an import. */
  private refreshList(id: string): void {
    const list = this.shadowRoot?.querySelector(`#${id}`) as
      | import('../shared/resource-list.js').ScionResourceList
      | null;
    void list?.load();
  }

  override render() {
    return html`
      <div class="header">
        <sl-icon name="gear"></sl-icon>
        <h1>Hub Resources</h1>
      </div>

      <div class="section">
        <h2>Resources</h2>
        <p>Hub-scoped resources available to all projects and agents.</p>

        <sl-tab-group
          @sl-tab-show=${(e: CustomEvent) => {
            this.activeTab = (e.detail as { name: string }).name;
          }}
        >
          <sl-tab slot="nav" panel="env-vars" ?active=${this.activeTab === 'env-vars'}
            >Environment Variables</sl-tab
          >
          <sl-tab slot="nav" panel="secrets" ?active=${this.activeTab === 'secrets'}>Secrets</sl-tab>
          <sl-tab slot="nav" panel="templates" ?active=${this.activeTab === 'templates'}
            >Templates</sl-tab
          >
          <sl-tab slot="nav" panel="harness-configs" ?active=${this.activeTab === 'harness-configs'}
            >Harness Configs</sl-tab
          >

          <sl-tab-panel name="env-vars">
            <scion-env-var-list scope="hub" apiBasePath="/api/v1" compact></scion-env-var-list>
          </sl-tab-panel>

          <sl-tab-panel name="secrets">
            <scion-secret-list scope="hub" apiBasePath="/api/v1" compact></scion-secret-list>
          </sl-tab-panel>

          <sl-tab-panel name="templates">
            <p class="tab-intro">Global agent templates. Open one to browse and edit its files.</p>
            <scion-resource-import
              kind="template"
              scope="global"
              canImport
              @resource-imported=${() => this.refreshList('templates-list')}
            ></scion-resource-import>
            <scion-resource-list
              id="templates-list"
              kind="template"
              scope="global"
              detailBasePath="/settings"
            ></scion-resource-list>
          </sl-tab-panel>

          <sl-tab-panel name="harness-configs">
            <p class="tab-intro">
              Global harness configurations. Open one to browse and edit its files.
            </p>
            <scion-resource-import
              kind="harness-config"
              scope="global"
              canImport
              @resource-imported=${() => this.refreshList('harness-configs-list')}
            ></scion-resource-import>
            <scion-resource-list
              id="harness-configs-list"
              kind="harness-config"
              scope="global"
              detailBasePath="/settings"
            ></scion-resource-list>
          </sl-tab-panel>
        </sl-tab-group>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-settings': ScionPageSettings;
  }
}
