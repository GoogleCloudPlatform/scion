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
 * Profile Templates page
 *
 * Manages user-scoped templates. Composes <scion-resource-list scope="user">
 * for all template operations (list, create, clone, rename, delete,
 * clone-from-global).
 */

import { LitElement, html, css } from 'lit';
import { customElement } from 'lit/decorators.js';

import '../shared/resource-import.js';
import '../shared/resource-list.js';
import type { ScionResourceList } from '../shared/resource-list.js';

@customElement('scion-page-profile-templates')
export class ScionPageProfileTemplates extends LitElement {
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
          <h1>Templates</h1>
          <p>
            Manage your personal agent templates. Use
            <code>scion template sync --template-scope user</code> to upload templates from the CLI.
          </p>
        </div>
      </div>

      <scion-resource-import
        kind="template"
        scope="user"
        canImport
        @resource-changed=${() => this._refreshList()}
      ></scion-resource-import>

      <scion-resource-list
        id="templates-list"
        kind="template"
        scope="user"
        detailBasePath="/profile"
        canClone
        canDelete
        canCreate
        canRename
        cloneFromGlobal
        @resource-changed=${() => this._refreshList()}
      ></scion-resource-list>
    `;
  }

  private _refreshList() {
    const list = this.shadowRoot?.querySelector<ScionResourceList>('#templates-list');
    if (list) void list.load();
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-templates': ScionPageProfileTemplates;
  }
}
