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
 * Shared "back" link for detail pages (ptone/scion#4177): an arrow and a
 * label, linking to the page the user came from (usually the list page).
 *
 * Usage, in the shared detail header:
 *
 *   <scion-detail-header heading=${name}>
 *     <scion-back-link slot="back" href="/brokers">Back to Brokers</scion-back-link>
 *     ...
 *   </scion-detail-header>
 *
 * or on its own, above an error or loading state that has no header.
 *
 * It renders a real anchor, so the app's router handles the click (it looks
 * for anchors along the composed path) and modifier-clicks open a new tab.
 * The label is the anchor's slotted text, which gives it its accessible name;
 * the arrow is decorative.
 *
 * Style: muted text that turns primary on hover, the style most detail pages
 * already used.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

@customElement('scion-back-link')
export class ScionBackLink extends LitElement {
  /** Link target. */
  @property() href = '';

  /**
   * Truncate a long label with an ellipsis (ptone/scion#8731). Pair with
   * `tooltip` so the full plain-text name remains readable on hover/focus.
   */
  @property({ type: Boolean, reflect: true }) truncate = false;

  /** Optional plain-text tooltip shown on hover/focus (e.g. full project name). */
  @property() tooltip = '';

  static override styles = css`
    :host {
      display: inline-flex;
      max-width: 100%;
      min-width: 0;
      margin-bottom: 1rem;
    }

    sl-tooltip {
      max-width: 100%;
      min-width: 0;
    }

    a {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      max-width: 100%;
      min-width: 0;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
    }

    a:hover {
      color: var(--scion-primary, #3b82f6);
    }

    sl-icon {
      flex-shrink: 0;
    }

    .label {
      min-width: 0;
    }

    :host([truncate]) .label {
      max-width: 24ch;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
  `;

  override render() {
    const link = html`
      <a href=${this.href} part="link">
        <sl-icon name="arrow-left" aria-hidden="true"></sl-icon>
        <span class="label" part="label"><slot></slot></span>
      </a>
    `;
    const tip = this.tooltip.trim();
    if (!tip) return link;
    return html`<sl-tooltip content=${tip}>${link}</sl-tooltip>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-back-link': ScionBackLink;
  }
}
