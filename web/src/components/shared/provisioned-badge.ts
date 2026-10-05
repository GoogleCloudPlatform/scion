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
 * "Provisioned, not started" badge (ptone/scion#2929): `scion create`
 * provisions an agent without starting it, which otherwise reads as a
 * stuck start. Renders nothing unless the agent is provision-only.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { Agent } from '../../shared/types.js';
import {
  PROVISIONED_ONLY_LABEL,
  isProvisionedOnly,
  provisionedStartHint,
} from '../../shared/agent-state-display.js';

@customElement('scion-provisioned-badge')
export class ScionProvisionedBadge extends LitElement {
  @property({ attribute: false })
  agent: Pick<Agent, 'name' | 'phase' | 'provisionedOnly'> | null = null;

  @property({ type: String })
  size: 'small' | 'medium' = 'medium';

  static override styles = css`
    :host {
      display: inline-flex;
      min-width: 0;
    }

    :host([hidden]) {
      display: none;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.625rem;
      border-radius: 9999px;
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #475569);
    }

    .badge.small {
      font-size: 0.8125rem;
      padding: 0.125rem 0.5rem;
      gap: 0.25rem;
    }
  `;

  private get shown(): boolean {
    return this.agent !== null && isProvisionedOnly(this.agent);
  }

  protected override willUpdate(): void {
    // Take no space (and no flex gap) when there is nothing to show.
    this.toggleAttribute('hidden', !this.shown);
  }

  override render(): TemplateResult | typeof nothing {
    if (!this.shown) return nothing;
    const hint = provisionedStartHint(this.agent!.name);
    return html`
      <span
        class="badge ${this.size}"
        title=${hint}
        aria-label="${PROVISIONED_ONLY_LABEL}. ${hint}"
      >
        <sl-icon name="info-circle"></sl-icon>
        <span>${PROVISIONED_ONLY_LABEL}</span>
      </span>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-provisioned-badge': ScionProvisionedBadge;
  }
}
