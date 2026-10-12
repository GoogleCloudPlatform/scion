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
 * The service-account picker's options and help text with each account's
 * mapping state (ptone/scion#4391), shared by every picker that labels
 * accounts so they read the same. Pass the accounts in picker order
 * (orderByAssignStatus). An account without a mapping state renders as it
 * did before the hub reported one.
 */

import { html, css, nothing, type TemplateResult } from 'lit';

import { assignStatusLabel } from '../../shared/gcp-sa-assign-status.js';
import type { GCPServiceAccount, GCPServiceAccountAssignStatus } from '../../shared/types.js';

/** One account's option: email, display name, hub scope, and its mapping state. */
export function renderServiceAccountOption(sa: GCPServiceAccount): TemplateResult {
  const status = assignStatusLabel(sa.assignStatus);
  return html`<sl-option
    value=${sa.id}
    data-assign-state=${sa.assignStatus?.state ?? nothing}
    title=${sa.assignStatus?.message || nothing}
  >
    ${sa.email}${sa.displayName ? ` (${sa.displayName})` : ''}${sa.scope === 'hub'
      ? ' (Hub)'
      : ''}${status ? html` <span class="assign-status">— ${status}</span>` : nothing}
  </sl-option>`;
}

/**
 * The selected account's hub message, for the select's help-text slot; a
 * warning when the account is not mapped on the target. Nothing when the
 * hub gave no message.
 */
export function renderAssignStatusHelp(
  status: GCPServiceAccountAssignStatus | undefined
): TemplateResult | typeof nothing {
  if (!status?.message) return nothing;
  return html`<div
    slot="help-text"
    class=${status.state === 'not_mapped'
      ? 'assign-status-message warning'
      : 'assign-status-message'}
    data-testid="gcp-sa-assign-status"
    data-assign-state=${status.state}
  >
    ${status.message}
  </div>`;
}

/** Styles for the option label and the help text. */
export const assignStatusStyles = css`
  .assign-status {
    color: var(--sl-color-neutral-600);
  }
  .assign-status-message {
    font-size: var(--sl-font-size-small);
    color: var(--sl-color-neutral-600);
  }
  .assign-status-message.warning {
    color: var(--sl-color-warning-700, #b45309);
  }
`;
