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
 * Shared Breadcrumb Navigation Component (`ptone/scion#8281`).
 *
 * Renders an accessible native `<nav aria-label="Breadcrumb"><ol>` trail.
 * Lives in lazy route chunks (never in the entry bundle) and uses CSS-only
 * separators with empty alt text so screen readers skip them. Truncated
 * crumbs lazily mount `<sl-tooltip>` on first `pointerenter`/`focusin`
 * (matching `scion-status-badge`) to keep initial DOM counts within budget.
 */

import { LitElement, html, css, nothing, type PropertyValues, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

export interface Crumb {
  /** Visible plain text. Never empty: pages pass an ID fallback, never ''. */
  label: string;
  /**
   * Link target (unprefixed app path). Omit only on the current-page crumb,
   * which must be last.
   */
  href?: string;
  /** Entity name: cap at 24ch with ellipsis; full label shown in a tooltip. */
  truncate?: boolean;
  /** Label is a fallback (raw ID) while a name resolves: muted style. */
  pending?: boolean;
  /**
   * Same-page intercept (e.g. closing an in-page file editor). Called on a
   * plain left click; if it calls `e.preventDefault()`, the router ignores the
   * click. Modifier and middle clicks still open `href` natively.
   */
  onActivate?: (e: MouseEvent) => void;
}

@customElement('scion-breadcrumb')
export class ScionBreadcrumb extends LitElement {
  @property({ attribute: false })
  crumbs: readonly Crumb[] = [];

  @state()
  private expanded = false;

  /** Indices of truncated crumbs whose `<sl-tooltip>` has been lazily mounted. */
  @state()
  private mountedTooltips: ReadonlySet<number> = new Set();

  static override styles = css`
    :host {
      display: block;
      width: 100%;
      min-width: 0;
      margin-bottom: 1rem;
      font-size: 0.875rem;
      container-type: inline-size;
    }

    nav {
      min-width: 0;
    }

    ol {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      list-style: none;
      margin: 0;
      padding: 0;
      min-width: 0;
      row-gap: 0.25rem;
    }

    li {
      display: inline-flex;
      align-items: center;
      min-width: 0;
      max-width: 100%;
    }

    li + li::before {
      content: '/' / '';
      color: var(--scion-text-muted);
      margin-inline: 0.375rem;
      flex-shrink: 0;
      user-select: none;
    }

    sl-tooltip {
      min-width: 0;
      max-width: 100%;
    }

    a {
      display: inline-block;
      min-width: 0;
      max-width: 100%;
      color: var(--scion-text-muted);
      text-decoration: none;
      border-radius: var(--scion-radius-sm);
    }

    a:hover,
    a:focus-visible {
      color: var(--scion-primary);
    }

    a:focus-visible,
    .ellipsis:focus-visible {
      outline: 2px solid var(--scion-primary);
      outline-offset: 2px;
    }

    .current {
      display: inline-block;
      min-width: 0;
      max-width: 100%;
      color: var(--scion-text);
      font-weight: 500;
    }

    .pending {
      color: var(--scion-text-muted);
      opacity: 0.75;
    }

    .truncate {
      max-width: 24ch;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      vertical-align: bottom;
    }

    .ellipsis-item {
      display: none;
    }

    .ellipsis {
      appearance: none;
      background: transparent;
      border: none;
      padding: 0 0.25rem;
      margin: 0;
      font: inherit;
      line-height: 1.2;
      color: var(--scion-text-muted);
      border-radius: var(--scion-radius-sm);
      cursor: pointer;
    }

    .ellipsis:hover,
    .ellipsis:focus-visible {
      color: var(--scion-primary);
    }

    @container (max-width: 36rem) {
      ol:not(.expanded) li.middle {
        display: none;
      }

      ol:not(.expanded) li.ellipsis-item {
        display: inline-flex;
      }
    }
  `;

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('crumbs')) {
      if (this.crumbs.length < 4 && this.expanded) {
        this.expanded = false;
      }
      if (this.mountedTooltips.size > 0) {
        this.mountedTooltips = new Set();
      }
    }
  }

  private handleCrumbClick(crumb: Crumb, e: MouseEvent): void {
    if (!crumb.onActivate) return;
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
      return;
    }
    crumb.onActivate(e);
  }

  private mountTooltip(index: number, restoreFocus: boolean): void {
    if (this.mountedTooltips.has(index)) return;
    const next = new Set(this.mountedTooltips);
    next.add(index);
    this.mountedTooltips = next;
    void this.updateComplete.then(() => {
      const tip = this.shadowRoot?.querySelector<
        HTMLElement & { show?: () => Promise<void> | void }
      >(`sl-tooltip[data-crumb-index="${index}"]`);
      if (restoreFocus) {
        tip?.querySelector<HTMLElement>('a, span')?.focus();
      }
      void tip?.show?.();
    });
  }

  private async handleExpand(): Promise<void> {
    this.expanded = true;
    await this.updateComplete;
    const firstRevealed = this.shadowRoot?.querySelector<HTMLElement>(
      'li.middle a, li.middle [tabindex]'
    );
    firstRevealed?.focus();
  }

  private renderCrumbContent(crumb: Crumb, index: number): TemplateResult {
    const classes = [crumb.truncate ? 'truncate' : '', crumb.pending ? 'pending' : '']
      .filter(Boolean)
      .join(' ');
    const mountTip = Boolean(crumb.truncate && this.mountedTooltips.has(index));
    const deferTip = Boolean(crumb.truncate && !mountTip);

    let inner: TemplateResult;
    if (crumb.href !== undefined) {
      inner = html`<a
        href=${crumb.href}
        part="link"
        class=${classes}
        @click=${(e: MouseEvent) => this.handleCrumbClick(crumb, e)}
        @pointerenter=${deferTip ? () => this.mountTooltip(index, false) : nothing}
        @focusin=${deferTip ? () => this.mountTooltip(index, true) : nothing}
        >${crumb.label}</a
      >`;
    } else {
      if (index !== this.crumbs.length - 1 && import.meta.env?.DEV) {
        console.warn('scion-breadcrumb: crumb without href must be the last item', crumb);
      }
      const currentClasses = ['current', classes].filter(Boolean).join(' ');
      inner = html`<span
        aria-current="page"
        part="current"
        class=${currentClasses}
        @pointerenter=${deferTip ? () => this.mountTooltip(index, false) : nothing}
        >${crumb.label}</span
      >`;
    }

    if (mountTip) {
      return html`<sl-tooltip content=${crumb.label} data-crumb-index=${index} hoist
        >${inner}</sl-tooltip
      >`;
    }
    return inner;
  }

  override render() {
    if (this.crumbs.length === 0) {
      return nothing;
    }

    const canCollapse = this.crumbs.length >= 4;
    const lastMiddleIndex = this.crumbs.length - 3;

    return html`
      <nav aria-label="Breadcrumb" part="nav">
        <ol part="list" class=${this.expanded ? 'expanded' : ''}>
          ${this.crumbs.map((crumb, index) => {
            const isMiddle = canCollapse && index >= 1 && index <= lastMiddleIndex;
            const item = html`<li class=${isMiddle ? 'middle' : ''}>
              ${this.renderCrumbContent(crumb, index)}
            </li>`;

            if (canCollapse && index === 0) {
              return html`${item}
                <li class="ellipsis-item">
                  <button
                    type="button"
                    class="ellipsis"
                    aria-label="Show full path"
                    aria-expanded=${this.expanded ? 'true' : 'false'}
                    @click=${() => void this.handleExpand()}
                  >
                    &hellip;
                  </button>
                </li>`;
            }

            return item;
          })}
        </ol>
      </nav>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-breadcrumb': ScionBreadcrumb;
  }
}
