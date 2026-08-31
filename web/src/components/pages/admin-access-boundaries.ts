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
 * Admin Access Boundaries inventory page component (stub).
 *
 * Placeholder that will be fleshed out in F2. Provides the route target
 * so the routing shell is complete.
 */

import { LitElement, html, css } from 'lit';
import { customElement } from 'lit/decorators.js';

import { setDocumentTitle } from '../../client/page-title.js';

@customElement('scion-page-admin-access-boundaries')
export class ScionPageAdminAccessBoundaries extends LitElement {
  static override styles = css`
    :host {
      display: block;
    }

    .placeholder {
      text-align: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .placeholder h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .placeholder p {
      margin: 0;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    setDocumentTitle('Access Boundaries');
  }

  override render() {
    return html`
      <div class="placeholder">
        <h1>Access Boundaries</h1>
        <p>Coming soon</p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-access-boundaries': ScionPageAdminAccessBoundaries;
  }
}
