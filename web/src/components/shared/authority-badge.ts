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
 * "Orphaned" / "Revoked" badge for an agent's authority state
 * (ptone/scion#2143), with a tooltip naming the cause and the remediation.
 * Renders nothing for any other state.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { AgentAuthority } from '../../shared/types.js';
import {
  authorityBadgeLabel,
  authorityTooltip,
  isAuthorityFlagged,
} from '../../shared/agent-authority.js';

@customElement('scion-authority-badge')
export class ScionAuthorityBadge extends LitElement {
  @property({ attribute: false })
  authority: AgentAuthority | null | undefined = null;

  @property({ type: String })
  size: 'small' | 'medium' = 'medium';

  static override styles = css`
    :host {
      display: inline-flex;
    }

    sl-badge::part(base) {
      gap: 0.25rem;
    }

    .small sl-badge::part(base) {
      font-size: 0.75rem;
    }
  `;

  override render() {
    const a = this.authority;
    if (!isAuthorityFlagged(a)) return nothing;
    const tip = authorityTooltip(a);
    return html`<span class=${this.size} title=${tip}>
      <sl-tooltip content=${tip} hoist>
        <sl-badge variant=${a.state === 'revoked' ? 'danger' : 'warning'} pill aria-label=${tip}>
          <sl-icon name="exclamation-triangle"></sl-icon>
          ${authorityBadgeLabel(a)}
        </sl-badge>
      </sl-tooltip>
    </span>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-authority-badge': ScionAuthorityBadge;
  }
}
