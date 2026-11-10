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
 * Files tab banner for the project storage migration (ptone/scion#5819).
 *
 * The hub sets `migrationNotice` on the workspace file listing while the
 * project still has files in the hub's previous storage location that the
 * one-shot `project-storage-migrate` maintenance operation has not moved.
 * The text comes from the server; this component only presents it.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';

/** The `migrationNotice` field of a workspace file listing. */
export interface WorkspaceMigrationNotice {
  message: string;
  operationKey: string;
}

@customElement('scion-workspace-migration-notice')
export class ScionWorkspaceMigrationNotice extends LitElement {
  @property({ attribute: false })
  notice: WorkspaceMigrationNotice | null = null;

  static override styles = css`
    :host {
      display: block;
    }
    sl-alert {
      margin-bottom: 0.75rem;
    }
    code {
      font-family: var(--scion-font-mono, monospace);
    }
  `;

  override render(): TemplateResult | typeof nothing {
    if (!this.notice?.message) return nothing;
    return html`
      <sl-alert variant="warning" open role="status">
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        ${this.notice.message}
        ${this.notice.operationKey
          ? html` Operation: <code>${this.notice.operationKey}</code>.`
          : nothing}
      </sl-alert>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-workspace-migration-notice': ScionWorkspaceMigrationNotice;
  }
}
