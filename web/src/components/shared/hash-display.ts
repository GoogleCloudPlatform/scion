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
 * Hash Display component
 *
 * Renders a long hash (e.g. `sha256:ba81f11e...`) in a fixed-width
 * monospace box that visually truncates with an ellipsis while keeping
 * the full text in the DOM (so browser find / cmd-f still matches it).
 * A copy-to-clipboard icon button sits alongside it.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { copyToClipboard, showCopyFailedToast } from '../../utils/clipboard.js';

@customElement('scion-hash-display')
export class ScionHashDisplay extends LitElement {
  /** Full hash string (e.g. "sha256:ba81f11e..."). */
  @property({ type: String })
  hash = '';

  /**
   * CSS max-width applied to the visible hash text. Anything beyond this
   * is clipped with an ellipsis. The full text remains in the DOM so
   * browser find still matches the truncated portion.
   */
  @property({ type: String, attribute: 'max-width' })
  maxWidth = '14ch';

  @state()
  private copied = false;

  static override styles = css`
    :host {
      display: inline-flex;
      align-items: baseline;
      gap: 0.25rem;
      max-width: 100%;
      min-width: 0;
      vertical-align: middle;
      font-family: var(--scion-font-mono, var(--sl-font-mono, monospace));
      font-size: inherit;
      color: inherit;
    }

    .hash-text {
      display: inline-block;
      max-width: var(--scion-hash-max-width, 14ch);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      vertical-align: bottom;
      min-width: 0;
    }

    .copy-btn {
      cursor: pointer;
      background: transparent;
      border: 0;
      padding: 0;
      margin: 0;
      color: var(--scion-text-muted, var(--sl-color-neutral-500, #64748b));
      flex-shrink: 0;
      line-height: 1;
      display: inline-flex;
      align-items: center;
      transition: color var(--scion-transition-fast, 150ms ease);
    }

    .copy-btn:hover {
      color: var(--scion-primary, var(--sl-color-primary-600, #3b82f6));
    }

    .copy-btn sl-icon {
      font-size: 0.95em;
    }

    .copy-btn.copied {
      color: var(--sl-color-success-600, #16a34a);
    }
  `;

  private async copy(e: Event): Promise<void> {
    e.stopPropagation();
    if (!this.hash) return;
    if (!(await copyToClipboard(this.hash))) {
      // The full hash remains selectable in the DOM via the title tooltip.
      showCopyFailedToast();
      return;
    }
    this.copied = true;
    setTimeout(() => {
      this.copied = false;
    }, 1500);
  }

  override render(): unknown {
    if (!this.hash) return nothing;
    const styleAttr = `--scion-hash-max-width: ${this.maxWidth}`;
    return html`
      <span class="hash-text" style=${styleAttr} title=${this.hash}>${this.hash}</span>
      <sl-tooltip content=${this.copied ? 'Copied!' : 'Copy to clipboard'}>
        <button
          type="button"
          class="copy-btn ${this.copied ? 'copied' : ''}"
          @click=${(e: Event): Promise<void> => this.copy(e)}
          aria-label="Copy hash to clipboard"
        >
          <sl-icon name=${this.copied ? 'clipboard-check' : 'clipboard'}></sl-icon>
        </button>
      </sl-tooltip>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-hash-display': ScionHashDisplay;
  }
}
