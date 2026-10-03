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
 * Agent list pager (design §6.1, §6.2).
 *
 * "a-b of N", Prev/Next, a page size of 25/50/100 (default 25, persisted),
 * loading and error states, the paged-state "may have changed - Refresh"
 * chip, and the capped-drain total. Used by the project page's grid and
 * list views in every window state; a capped drain renders its total as
 * "X loaded (newest 2,000 checked), more exist" instead of "a-b of N".
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { cappedTotalText } from '../../client/agent-list-window.js';

export type AgentPagerTotal = number | { loaded: number; capped: true };

/** Exported so the host (the single owner of the persisted value) can validate a stored size against the same source of truth. */
export const AGENT_PAGER_PAGE_SIZES = [25, 50, 100] as const;
export type AgentPagerPageSize = (typeof AGENT_PAGER_PAGE_SIZES)[number];

@customElement('scion-agent-pager')
export class ScionAgentPager extends LitElement {
  /** 0-based index of the currently displayed page. */
  @property({ type: Number })
  pageIndex = 0;

  /**
   * Rows before this page, i.e. `a - 1` in "a-b of N" (design §6.1). Not
   * assumed to be `pageIndex * pageSize` — a page can be short (design §5.3
   * step 5a), so the host tracks the real running offset.
   */
  @property({ type: Number })
  rangeStart = 0;

  /** Number of rows actually rendered on the current page. */
  @property({ type: Number })
  rowsOnPage = 0;

  /** Exact total, or `{loaded, capped: true}` for a capped drain (design §4.6). */
  @property({ attribute: false })
  total: AgentPagerTotal = 0;

  @property({ type: Number })
  pageSize: AgentPagerPageSize = 25;

  @property({ type: Boolean })
  hasNext = false;

  @property({ type: Boolean })
  hasPrev = false;

  @property({ type: Boolean })
  loading = false;

  @property({ type: String })
  error: string | null = null;

  /** The zero-cost "may have changed - Refresh" chip (design §6.2). */
  @property({ type: Boolean })
  showChip = false;

  /** The chip's text; the global page's count-only mode says that the counts may have changed. */
  @property({ type: String })
  chipText = 'may have changed · Refresh';

  /**
   * localStorage key to persist a page-size change to; empty disables
   * persistence. This component is otherwise fully controlled: it never
   * reads storage itself on connect, so the host is
   * the single source of truth for `pageSize` — including the value used
   * for the first request's `limit`, which only the host can know about
   * before this component even exists.
   */
  @property({ type: String })
  storageKey = '';

  static override styles = css`
    :host {
      display: block;
    }
    .pager {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.5rem 0;
      flex-wrap: wrap;
    }
    .range {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }
    .capped-banner {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }
    .chip {
      cursor: pointer;
    }
    .chip-disabled {
      cursor: default;
      opacity: 0.6;
      pointer-events: none;
    }
    .error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
    }
  `;

  private setPageSize(size: AgentPagerPageSize): void {
    if (size === this.pageSize) return;
    this.pageSize = size;
    if (this.storageKey) {
      localStorage.setItem(this.storageKey, String(size));
    }
    this.dispatchEvent(new CustomEvent('page-size-change', { detail: { pageSize: size } }));
  }

  private onPrev(): void {
    if (!this.hasPrev || this.loading) return;
    this.dispatchEvent(new Event('prev'));
  }

  private onNext(): void {
    if (!this.hasNext || this.loading) return;
    this.dispatchEvent(new Event('next'));
  }

  private onChipClick(): void {
    // Disabled while loading: a refresh click during an
    // in-flight page-level load would race it the same way Prev/Next would.
    if (this.loading) return;
    this.dispatchEvent(new Event('chip-click'));
  }

  private renderRange() {
    if (typeof this.total === 'object' && this.total.capped) {
      return html`<span class="capped-banner">${cappedTotalText(this.total.loaded)}</span>`;
    }
    if (this.rowsOnPage === 0) {
      return html`<span class="range">0 of ${this.total}</span>`;
    }
    const a = this.rangeStart + 1;
    const b = a + this.rowsOnPage - 1;
    return html`<span class="range">${a}-${b} of ${this.total}</span>`;
  }

  override render() {
    return html`
      <div class="pager">
        ${this.renderRange()}
        <sl-button-group>
          <sl-button
            size="small"
            ?disabled=${!this.hasPrev || this.loading}
            @click=${() => this.onPrev()}
          >
            <sl-icon name="chevron-left"></sl-icon>
            Prev
          </sl-button>
          <sl-button
            size="small"
            ?disabled=${!this.hasNext || this.loading}
            @click=${() => this.onNext()}
          >
            Next
            <sl-icon name="chevron-right"></sl-icon>
          </sl-button>
        </sl-button-group>
        <sl-select
          size="small"
          .value=${String(this.pageSize)}
          style="width: 5.5rem;"
          @sl-change=${(e: Event) =>
            this.setPageSize(
              Number((e.target as HTMLElement & { value: string }).value) as AgentPagerPageSize
            )}
        >
          ${AGENT_PAGER_PAGE_SIZES.map(
            (size) => html`<sl-option value=${String(size)}>${size} / page</sl-option>`
          )}
        </sl-select>
        ${this.loading ? html`<sl-spinner style="font-size: 1rem;"></sl-spinner>` : nothing}
        ${this.error ? html`<span class="error">${this.error}</span>` : nothing}
        ${this.showChip
          ? html`<sl-tag
              class="chip ${this.loading ? 'chip-disabled' : ''}"
              variant="primary"
              pill
              @click=${() => this.onChipClick()}
            >
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              ${this.chipText}
            </sl-tag>`
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-agent-pager': ScionAgentPager;
  }
}
