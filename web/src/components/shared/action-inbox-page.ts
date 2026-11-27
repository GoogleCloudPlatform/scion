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
 * `<scion-action-inbox-page>`: reusable message-action inbox layout component
 * (design §5.5, AC-12).
 *
 * Kind-agnostic: renders the Inbox tab strip, heading/descriptor, Open and
 * Resolved tabs, project/agent/search/sort filter bar, card list delegated to
 * `config.renderCard`, Load more pagination, and empty/loading/error states.
 */

import { LitElement, css, html, nothing, type PropertyValues, type TemplateResult } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import type { PageData } from '../../shared/types.js';
import type { ActionInboxSort, ActionInboxTab } from '../../client/action-inbox.js';
import {
  ActionInboxPageController,
  type ActionInboxKindConfig,
} from './action-inbox-controller.js';
import { inboxTabStripStyles, renderInboxTabStrip } from './inbox-tabs.js';
import '../pages/not-found.js';

@customElement('scion-action-inbox-page')
export class ScionActionInboxPage<TCard = unknown> extends LitElement {
  @property({ attribute: false })
  config: ActionInboxKindConfig<TCard> | null = null;

  @property({ type: Object })
  pageData: PageData | null = null;

  readonly ctrl = new ActionInboxPageController<TCard>(this);

  static override styles = [
    inboxTabStripStyles,
    css`
      :host {
        display: block;
        max-width: 1100px;
        margin: 0 auto;
        padding: 1.5rem 1.25rem;
      }

      .page-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        gap: 1rem;
        margin-bottom: 0.75rem;
      }

      .page-heading {
        min-width: 0;
      }

      .page-title {
        margin: 0;
        font-size: 1.5rem;
        font-weight: 600;
        color: var(--scion-text);
      }

      .page-descriptor {
        margin: 0.25rem 0 0;
        font-size: 0.875rem;
        font-style: italic;
        color: var(--scion-text-muted);
      }

      .sub-tabs {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        margin-bottom: 1rem;
      }

      .sub-tab-btn {
        display: inline-flex;
        align-items: center;
        gap: 0.4rem;
        padding: 0.45rem 0.85rem;
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        background: var(--scion-surface);
        color: var(--scion-text-secondary);
        font: inherit;
        font-size: 0.875rem;
        font-weight: 500;
        cursor: pointer;
        transition:
          background var(--scion-transition-fast, 150ms ease),
          border-color var(--scion-transition-fast, 150ms ease),
          color var(--scion-transition-fast, 150ms ease);
      }

      .sub-tab-btn:hover {
        background: var(--scion-bg-subtle);
        color: var(--scion-text);
      }

      .sub-tab-btn.active {
        background: var(--scion-primary);
        border-color: var(--scion-primary);
        color: var(--scion-primary-text);
      }

      .sub-tab-count {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        min-width: 1.25rem;
        padding: 0 0.35rem;
        border-radius: var(--scion-radius-full);
        font-size: 0.75rem;
        font-weight: 600;
        line-height: 1.25rem;
        background: var(--scion-bg-subtle);
        color: var(--scion-text);
      }

      .sub-tab-btn.active .sub-tab-count {
        background: color-mix(in srgb, var(--scion-primary-text) 22%, transparent);
        color: var(--scion-primary-text);
      }

      .filter-bar {
        display: flex;
        flex-wrap: wrap;
        align-items: flex-end;
        gap: 0.65rem;
        margin-bottom: 1rem;
        padding: 0.75rem;
        background: var(--scion-surface);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
      }

      .filter-field {
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
        min-width: 150px;
      }

      .filter-field.search-field {
        flex: 1 1 220px;
      }

      .filter-label {
        font-size: 0.75rem;
        font-weight: 500;
        color: var(--scion-text-muted);
      }

      .filter-select,
      .filter-input {
        height: 2.125rem;
        padding: 0 0.6rem;
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius-sm);
        background: var(--scion-bg);
        color: var(--scion-text);
        font: inherit;
        font-size: 0.875rem;
      }

      .filter-select:focus-visible,
      .filter-input:focus-visible {
        outline: 2px solid var(--scion-primary);
        outline-offset: 1px;
      }

      .notice-alert {
        display: block;
        margin-bottom: 1rem;
      }

      .card-list {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .state-panel {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        gap: 0.6rem;
        padding: 2.5rem 1.25rem;
        text-align: center;
        background: var(--scion-surface);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        color: var(--scion-text-muted);
      }

      .state-panel sl-icon {
        font-size: 1.75rem;
        color: var(--scion-text-muted);
      }

      .state-panel p {
        margin: 0;
        font-size: 0.9375rem;
      }

      .load-more-row {
        display: flex;
        justify-content: center;
        margin-top: 1.25rem;
      }

      @media (max-width: 480px) {
        :host {
          padding: 1rem 0.75rem;
        }
        .filter-field {
          flex: 1 1 100%;
        }
      }
    `,
  ];

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('config')) {
      this.ctrl.setConfig(this.config);
    }
    if (changed.has('pageData')) {
      this.ctrl.setUserId(this.pageData?.user?.id ?? '');
    }
  }

  private renderTabButton(tab: ActionInboxTab, label: string, count?: number): TemplateResult {
    const active = this.ctrl.tab === tab;
    return html`
      <button
        type="button"
        role="tab"
        aria-selected=${active ? 'true' : 'false'}
        class="sub-tab-btn ${active ? 'active' : ''}"
        data-action-tab=${tab}
        @click=${(): void => this.ctrl.setTab(tab)}
      >
        <span>${label}</span>
        ${typeof count === 'number' && count > 0
          ? html`<span class="sub-tab-count" data-action-tab-count=${tab}>${count}</span>`
          : nothing}
      </button>
    `;
  }

  private renderFilterBar(): TemplateResult {
    const projects = this.ctrl.facets.projects;
    const agents = this.ctrl.facets.agents;
    return html`
      <div class="filter-bar" data-action-filters>
        <div class="filter-field">
          <label class="filter-label" for="action-inbox-project">Project</label>
          <select
            id="action-inbox-project"
            class="filter-select"
            data-filter-project
            .value=${this.ctrl.project}
            @change=${(e: Event): void =>
              this.ctrl.setProject((e.target as HTMLSelectElement).value)}
          >
            <option value="">All projects</option>
            ${projects.map(
              (p) => html`
                <option value=${p.id} ?selected=${this.ctrl.project === p.id}>
                  ${p.name || p.id} (${p.count})
                </option>
              `
            )}
          </select>
        </div>

        <div class="filter-field">
          <label class="filter-label" for="action-inbox-agent">Agent</label>
          <select
            id="action-inbox-agent"
            class="filter-select"
            data-filter-agent
            .value=${this.ctrl.agent}
            @change=${(e: Event): void => this.ctrl.setAgent((e.target as HTMLSelectElement).value)}
          >
            <option value="">All agents</option>
            ${agents.map(
              (a) => html`
                <option value=${a.id} ?selected=${this.ctrl.agent === a.id}>
                  ${a.name || a.id} (${a.count})
                </option>
              `
            )}
          </select>
        </div>

        <div class="filter-field search-field">
          <label class="filter-label" for="action-inbox-search">Search</label>
          <input
            id="action-inbox-search"
            class="filter-input"
            type="search"
            placeholder="Filter by key, description, or agent…"
            data-filter-search
            .value=${this.ctrl.q}
            @input=${(e: Event): void => this.ctrl.setQuery((e.target as HTMLInputElement).value)}
          />
        </div>

        <div class="filter-field">
          <label class="filter-label" for="action-inbox-sort">Sort</label>
          <select
            id="action-inbox-sort"
            class="filter-select"
            data-filter-sort
            .value=${this.ctrl.sort}
            @change=${(e: Event): void =>
              this.ctrl.setSort((e.target as HTMLSelectElement).value as ActionInboxSort)}
          >
            <option value="recent" ?selected=${this.ctrl.sort === 'recent'}>Most recent</option>
            <option value="project" ?selected=${this.ctrl.sort === 'project'}>Project</option>
          </select>
        </div>

        ${this.ctrl.hasActiveFilters
          ? html`
              <sl-button
                size="small"
                variant="default"
                data-filter-clear
                @click=${(): void => this.ctrl.clearFilters()}
              >
                Clear filters
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  override render(): TemplateResult {
    const config = this.config;
    if (!config || !config.isEnabled() || this.ctrl.unavailable404) {
      return html`<scion-page-404></scion-page-404>`;
    }

    const onResolved = (): void => this.ctrl.refresh();

    return html`
      <div class="page-header">
        <div class="page-heading">
          <h1 class="page-title">${config.title}</h1>
          <p class="page-descriptor" data-action-descriptor>${config.descriptor}</p>
        </div>
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.ctrl.loading}
          @click=${(): void => this.ctrl.refresh()}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${renderInboxTabStrip(config.tabId)}

      <div class="sub-tabs" role="tablist" aria-label="Request status">
        ${this.renderTabButton('open', 'Open', this.ctrl.counts.open)}
        ${this.renderTabButton('resolved', 'Resolved')}
      </div>

      ${this.renderFilterBar()}
      ${this.ctrl.truncatedProjects || this.ctrl.capped
        ? html`
            <sl-alert variant="warning" open class="notice-alert" data-action-truncated>
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.ctrl.truncatedProjects
                ? 'Showing results from your first 200 projects. Narrow by project or agent to find specific requests.'
                : 'Showing the most recent items up to the scan cap. Narrow your filters to see older items.'}
            </sl-alert>
          `
        : nothing}
      ${this.ctrl.loading && this.ctrl.cards.length === 0
        ? html`
            <div class="state-panel" role="status" data-action-loading>
              <sl-spinner></sl-spinner>
              <p>Loading…</p>
            </div>
          `
        : this.ctrl.error && this.ctrl.cards.length === 0
          ? html`
              <sl-alert variant="danger" open class="notice-alert" data-action-error>
                <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                ${this.ctrl.error}
                ${this.ctrl.errorStatus !== 403
                  ? html`
                      <sl-button
                        size="small"
                        variant="default"
                        style="margin-left: 0.75rem;"
                        @click=${(): void => this.ctrl.refresh()}
                      >
                        Retry
                      </sl-button>
                    `
                  : nothing}
              </sl-alert>
            `
          : this.ctrl.cards.length === 0
            ? html`
                <div class="state-panel" data-action-empty>
                  <sl-icon
                    name=${this.ctrl.tab === 'open' ? 'check-circle' : 'clock-history'}
                    aria-hidden="true"
                  ></sl-icon>
                  <p>
                    ${this.ctrl.hasActiveFilters
                      ? 'No matching requests'
                      : this.ctrl.tab === 'open'
                        ? 'Nothing Waiting'
                        : 'No resolved requests yet'}
                  </p>
                  ${this.ctrl.hasActiveFilters
                    ? html`
                        <sl-button
                          size="small"
                          variant="default"
                          @click=${(): void => this.ctrl.clearFilters()}
                        >
                          Clear filters
                        </sl-button>
                      `
                    : nothing}
                </div>
              `
            : html`
                ${this.ctrl.error
                  ? html`
                      <sl-alert variant="danger" open class="notice-alert" data-action-error>
                        <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                        ${this.ctrl.error}
                      </sl-alert>
                    `
                  : nothing}
                <div class="card-list" data-action-cards>
                  ${repeat(
                    this.ctrl.cards,
                    (card) => config.getCardId(card),
                    (card) => config.renderCard(card, { onResolved })
                  )}
                </div>
                ${this.ctrl.nextCursor
                  ? html`
                      <div class="load-more-row">
                        <sl-button
                          size="small"
                          variant="default"
                          ?loading=${this.ctrl.loadingMore}
                          data-action-load-more
                          @click=${(): void => void this.ctrl.loadMore()}
                        >
                          Load more
                        </sl-button>
                      </div>
                    `
                  : nothing}
              `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-action-inbox-page': ScionActionInboxPage;
  }
}
