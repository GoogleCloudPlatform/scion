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
 * Shared header for resource detail pages (ptone/scion#3856): an icon, the
 * resource name as the page h1, badges beside the name, a meta line under
 * it, and the page actions on the right.
 *
 * The layout wraps rather than overflowing, a pattern that started on the
 * agent page (miller79/scion#147):
 * - a long name breaks inside the h1, even with no spaces (hostnames, ids);
 * - the badges follow the name and drop onto the next line when it does not
 *   fit, instead of being clipped or pushed off the page;
 * - the icon keeps its size and sits on the first line of the name;
 * - the actions drop below the title when the row is too narrow for both,
 *   and wrap among themselves on a phone.
 *
 * Usage:
 *
 *   <scion-detail-header heading=${name}>
 *     <scion-back-link slot="back" href="/brokers">Back to Brokers</scion-back-link>
 *     <sl-icon slot="icon" name="hdd-rack"></sl-icon>
 *     <scion-status-badge ...></scion-status-badge>
 *     <div slot="meta">...</div>
 *     <div slot="actions" class="header-actions">...</div>
 *   </scion-detail-header>
 *
 * The back slot sits above the title row and takes one or more
 * scion-back-link elements (ptone/scion#4177); they share one row and wrap.
 * Leave it empty on pages with no back link.
 *
 * Unslotted children are the badges, laid out after the name. Slotted
 * content stays in the page's DOM, so the page styles its own
 * badges, meta line and buttons. Give the actions slot a single wrapper
 * element; this component lays it out as a wrapping flex row.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import './back-link.js';

@customElement('scion-detail-header')
export class ScionDetailHeader extends LitElement {
  /** Resource name, rendered as the page h1. */
  @property() heading = '';

  static override styles = css`
    :host {
      display: block;
      margin-bottom: 1.5rem;
    }

    /* Back links share a row and wrap. Each link carries its own 1rem
       bottom margin, so an empty slot takes no space. */
    .back {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      column-gap: 1rem;
    }

    .header {
      display: flex;
      flex-wrap: wrap;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
    }

    /* The title takes the free space, but once it would get narrower than
       16rem the actions wrap onto the next line instead. min-width:0 lets a
       long name shrink the column rather than widen the page. */
    .header-info {
      flex: 1 1 16rem;
      min-width: 0;
    }

    .header-title {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
    }

    /* The icon keeps its size while the name absorbs the shrinking. */
    ::slotted([slot='icon']) {
      flex-shrink: 0;
    }

    /* Centre the icon on the first line of the name: (1.95rem h1 line box
       - 1.5rem icon) / 2. */
    ::slotted(sl-icon[slot='icon']) {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
      margin-top: 0.225rem;
    }

    /* A long name wraps on its own line; the badges then follow on the next
       line instead of floating beside a multi-line name. Stretching to the
       row height keeps a one-line name centred on an icon taller than the
       line (the group page's tile). */
    .header-title-text {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      align-self: stretch;
      gap: 0.5rem 0.75rem;
      min-width: 0;
    }

    /* overflow-wrap:anywhere breaks a long name with no spaces or hyphens,
       which would otherwise overflow with nowhere to break. */
    h1 {
      font-size: 1.5rem;
      font-weight: 700;
      line-height: 1.3;
      color: var(--scion-text, #1e293b);
      margin: 0;
      min-width: 0;
      overflow-wrap: anywhere;
    }

    ::slotted([slot='actions']) {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.5rem;
      min-width: 0;
    }
  `;

  override render() {
    return html`
      <div class="back" part="back"><slot name="back"></slot></div>
      <div class="header" part="header">
        <div class="header-info">
          <div class="header-title">
            <slot name="icon"></slot>
            <div class="header-title-text">
              <h1 part="heading">${this.heading}</h1>
              <slot></slot>
            </div>
          </div>
          <slot name="meta"></slot>
        </div>
        <slot name="actions"></slot>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-detail-header': ScionDetailHeader;
  }
}
