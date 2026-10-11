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
 * Templates for structured settings-save errors (ptone/scion#3923), shared
 * by the admin settings pages that classify errors with
 * shared/settings-save-errors.ts. Pages include `settingsErrorStyles` in
 * their static styles.
 */

import { html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';

import type { SettingsFieldError, SettingsSaveError } from '../../shared/settings-save-errors.js';

export const settingsErrorStyles = css`
  .settings-error-notice {
    padding: 0.75rem 1rem;
    margin-bottom: 1rem;
    border-radius: var(--scion-radius, 0.5rem);
    background: var(--scion-error-bg, #fef2f2);
    border: 1px solid var(--scion-error-border, #fca5a5);
    color: var(--scion-error-text, #991b1b);
    font-size: 0.8125rem;
  }

  .settings-error-notice.conflict {
    background: var(--sl-color-warning-50, #fffbeb);
    border-color: var(--sl-color-warning-300, #fcd34d);
    color: var(--sl-color-warning-800, #92400e);
  }

  .settings-error-notice-title {
    font-weight: 600;
    margin-bottom: 0.25rem;
  }

  .settings-error-notice ul {
    margin: 0.25rem 0 0 0;
    padding-left: 1.25rem;
  }

  .settings-error-notice code {
    font-family: var(--sl-font-mono, monospace);
    font-size: 0.75rem;
  }

  .settings-error-notice-actions {
    margin-top: 0.5rem;
  }

  .field-error {
    display: block;
    margin-top: 0.25rem;
    font-size: 0.75rem;
    color: var(--scion-error-text, #991b1b);
  }
`;

/** Inline messages under one form control. */
export function renderFieldErrorMessages(
  messages: readonly string[],
  field: string
): TemplateResult<1> | typeof nothing {
  if (messages.length === 0) return nothing;
  return html`${messages.map(
    (m) => html`<span class="field-error" role="alert" data-field-error=${field}>${m}</span>`
  )}`;
}

/** Summary of validation entries at the top of a section. */
export function renderSectionErrorSummary(
  errors: readonly SettingsFieldError[],
  section: string
): TemplateResult<1> | typeof nothing {
  if (errors.length === 0) return nothing;
  return html`
    <div class="settings-error-notice" role="alert" data-section-errors=${section}>
      <div class="settings-error-notice-title">
        ${errors.length === 1
          ? 'This section has a validation error. Nothing was saved.'
          : `This section has ${errors.length} validation errors. Nothing was saved.`}
      </div>
      <ul>
        ${errors.map(
          (e) =>
            html`<li>
              ${e.key || e.path ? html`<code>${e.key ?? e.path}</code> ` : nothing}${e.message}
            </li>`
        )}
      </ul>
    </div>
  `;
}

/** Revision conflict notice with a Reload action. */
export function renderConflictNotice(
  err: SettingsSaveError,
  onReload: () => void
): TemplateResult<1> {
  return html`
    <div class="settings-error-notice conflict" role="alert" data-settings-conflict>
      <div class="settings-error-notice-title">Settings changed since you loaded them</div>
      <div>
        Nothing was saved. Reload to see the latest values, then make your changes again. Unsaved
        edits on this page are discarded.
      </div>
      ${err.conflicted.length > 0
        ? html`<div>Changed: ${err.conflicted.map((c) => html` <code>${c.section}</code>`)}</div>`
        : nothing}
      <div class="settings-error-notice-actions">
        <sl-button size="small" variant="warning" data-settings-reload @click=${onReload}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Reload
        </sl-button>
      </div>
    </div>
  `;
}

/** Deployment-managed (Layer-0) rejection notice naming the keys. */
export function renderLayer0Notice(err: SettingsSaveError): TemplateResult<1> {
  return html`
    <div class="settings-error-notice" role="alert" data-settings-layer0>
      <div class="settings-error-notice-title">
        Deployment-managed settings cannot be changed here
      </div>
      <div>
        Nothing was saved.
        ${err.keys.length > 0
          ? html`These keys are managed by deployment configuration (settings.yaml or environment):
            ${err.keys.map((k) => html` <code>${k}</code>`)}`
          : nothing}
      </div>
      ${err.message ? html`<div>${err.message}</div>` : nothing}
    </div>
  `;
}
