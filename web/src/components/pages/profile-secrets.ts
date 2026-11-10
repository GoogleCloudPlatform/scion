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
 * Profile Secrets Management page
 *
 * Thin wrapper around the shared <scion-secret-list> component,
 * providing the page header and user scope configuration.
 */

import { LitElement, html, css } from 'lit';
import { customElement } from 'lit/decorators.js';

import '../shared/secret-list.js';

@customElement('scion-page-profile-secrets')
export class ScionPageProfileSecrets extends LitElement {
  static override styles = css`
    :host {
      display: block;
    }

    .page-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .page-header-info h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .page-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }
  `;

  override render() {
    return html`
      <div class="page-header">
        <div class="page-header-info">
          <h1>Secrets</h1>
          <p>
            Manage encrypted secrets for your agents. Saved values cannot be retrieved. Secrets your
            agents save wait for your approval before they are delivered; you can reveal a pending
            value before you approve it.
          </p>
        </div>
      </div>

      <scion-secret-list scope="user" apiBasePath="/api/v1"></scion-secret-list>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-secrets': ScionPageProfileSecrets;
  }
}
