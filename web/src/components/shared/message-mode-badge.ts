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
 * Message Mode Badge Component
 *
 * Displays the messaging authorization scope of an agent as a
 * color-coded pill badge with icon, label, and tooltip.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { getMessageModeDisplay } from '../../shared/message-mode.js';
import type { MessageMode } from '../../shared/types.js';

@customElement('scion-message-mode-badge')
export class ScionMessageModeBadge extends LitElement {
  /**
   * The message mode to display. Defaults to 'project' when unset
   * (migration edge case — see design doc Section 7.1).
   */
  @property()
  mode: MessageMode = 'project';

  /**
   * Size variant
   */
  @property()
  size: 'small' | 'medium' = 'small';

  /**
   * Whether to show the label text alongside the icon
   */
  @property({ type: Boolean })
  showLabel = true;

  /**
   * Whether to wrap the badge in a tooltip showing the mode description
   */
  @property({ type: Boolean })
  showTooltip = true;

  static override styles = css`
    :host {
      display: inline-flex;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-weight: 500;
      white-space: nowrap;
      border: 1px solid;
    }

    /* Size variants */
    .badge.small {
      font-size: 12px;
      gap: 0.25rem;
    }

    .badge.small sl-icon {
      font-size: 14px;
    }

    .badge.medium {
      font-size: 14px;
      padding: 0.1875rem 0.625rem;
    }

    .badge.medium sl-icon {
      font-size: 16px;
    }

    /* Color variants — background tint + border + text */
    .badge.success {
      background: var(--sl-color-success-100);
      border-color: var(--sl-color-success-600);
      color: var(--sl-color-success-600);
    }

    .badge.primary {
      background: var(--sl-color-primary-100);
      border-color: var(--sl-color-primary-600);
      color: var(--sl-color-primary-600);
    }

    .badge.warning {
      background: var(--sl-color-warning-100);
      border-color: var(--sl-color-warning-600);
      color: var(--sl-color-warning-600);
    }

    .badge.danger {
      background: var(--sl-color-danger-100);
      border-color: var(--sl-color-danger-600);
      color: var(--sl-color-danger-600);
    }
  `;

  override render() {
    const effectiveMode = this.mode || 'project';
    const display = getMessageModeDisplay(effectiveMode);

    const badge = html`
      <span class="badge ${display.color} ${this.size}">
        <sl-icon name="${display.icon}"></sl-icon>
        ${this.showLabel ? display.label : ''}
      </span>
    `;

    if (this.showTooltip) {
      return html`
        <sl-tooltip content="${display.description}">
          ${badge}
        </sl-tooltip>
      `;
    }

    return badge;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-message-mode-badge': ScionMessageModeBadge;
  }
}
