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
 * Collapsible Decisions Filter Panel (`<scion-decisions-filter-panel>`, Design D13, ptone/scion#7993).
 *
 * - Collapsed by default; open/closed preference is persisted per user in
 *   `localStorage` under `scion:decisions:filters-open` (never in the URL).
 * - Summary row displays a funnel icon, "Search and filters", removable `sl-tag`
 *   chips for every active filter on the current tab (search included as `"text"`),
 *   active filter count, and a Clear button (or `/` shortcut hint when collapsed with 0 active).
 * - Expanded body shows the search input (`sl-input[type="search"]`) and filter controls
 *   (Conversation & Agent `<scion-facet-combobox>` on both tabs; Answered by, Provenance,
 *   From, and To on `tab="made"`).
 * - Stacks controls into a single column at `<= 760px` (one component serves desktop and mobile).
 * - Accepts `scope: 'project' | 'profile'` and leading/trailing slots + `extraTags` so P5
 *   (`ptone/scion#7988`) can add the profile Project combobox and sort radio group cleanly.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  MAX_DECISIONS_SEARCH_RUNES,
  normalizeDecisionsSearchQuery,
  type MadeDecisionsProvenanceFilter,
  type ProjectDecisionActorView,
  type ProjectDecisionConversationView,
} from '../../client/project-decisions.js';
import type { FacetChangeDetail, FacetComboboxOption } from './facet-combobox.js';
import './facet-combobox.js';

/** `localStorage` key storing expanded/collapsed preference for Decisions filter panels (D13). */
export const DECISIONS_FILTERS_OPEN_STORAGE_KEY = 'scion:decisions:filters-open';

export type DecisionsFilterScope = 'project' | 'profile';

export type DecisionsFilterKey =
  | 'q'
  | 'conversation'
  | 'agent'
  | 'user'
  | 'provenance'
  | 'from'
  | 'to'
  | 'project';

/** Removable summary chip descriptor. */
export interface DecisionsFilterTag {
  key: DecisionsFilterKey;
  label: string;
}

/** Patch emitted when one or more structured filters change or a tag is removed. */
export interface DecisionsFilterPatch {
  q?: string;
  conversation?: string;
  agent?: string;
  user?: string;
  provenance?: MadeDecisionsProvenanceFilter;
  from?: string;
  to?: string;
  project?: string;
}

/** Detail payload of `decisions-search-input`. */
export interface DecisionsSearchInputDetail {
  value: string;
  immediate: boolean;
}

function readStoredOpenPreference(): boolean {
  try {
    const raw = window.localStorage.getItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY);
    return raw === 'true' || raw === '1';
  } catch {
    return false;
  }
}

function writeStoredOpenPreference(open: boolean): void {
  try {
    window.localStorage.setItem(DECISIONS_FILTERS_OPEN_STORAGE_KEY, open ? 'true' : 'false');
  } catch {
    // Ignore storage quota / privacy mode errors.
  }
}

function provenanceTagWord(value: string): string {
  switch (value) {
    case 'direct':
      return 'direct';
    case 'interpreted':
    case 'agent_interpreted':
      return 'interpreted';
    case 'confirmed':
      return 'confirmed';
    case 'overridden':
      return 'changed';
    default:
      return value;
  }
}

@customElement('scion-decisions-filter-panel')
export class ScionDecisionsFilterPanel extends LitElement {
  /** Surface hosting the panel (`'project'` in P2; `'profile'` reserved for P5). */
  @property({ type: String }) scope: DecisionsFilterScope = 'project';

  /**
   * Whether the connected Hub supports P1+ search and conversation/pending filters (Design 5.3).
   * When `false` (older Hub without `conversations` facet), hides the search input and
   * Conversation combobox (plus the Agent combobox on `tab="pending"`), while keeping Phase 3
   * `Made` filters, tags, and `Clear` working.
   */
  @property({ type: Boolean, attribute: 'supports-search' }) supportsSearch = true;

  /** Active tab (`'pending'` shows shared controls; `'made'` adds answer log controls). */
  @property({ type: String }) tab: 'pending' | 'made' = 'pending';

  /** Committed search query (`q` in URL / API, shown as `"q"` tag when >= 2 runes). */
  @property({ type: String }) q = '';

  /** Live text inside the search input (may differ from `q` during 300 ms debounce). */
  @property({ attribute: false }) searchText: string | undefined = undefined;

  /** Selected conversation `chatKey`. */
  @property({ type: String }) conversation = '';

  /** Selected asking agent UUID. */
  @property({ type: String }) agent = '';

  /** Selected answerer user ID (`tab="made"`). */
  @property({ type: String }) user = '';

  /** Selected provenance filter (`tab="made"`). */
  @property({ type: String }) provenance: MadeDecisionsProvenanceFilter = '';

  /** Selected start date `YYYY-MM-DD` (`tab="made"`). */
  @property({ type: String }) from = '';

  /** Selected end date `YYYY-MM-DD` (`tab="made"`). */
  @property({ type: String }) to = '';

  /** Authenticated viewer's user ID (labels self as "You" / "you"). */
  @property({ type: String }) currentUserId = '';

  /** Conversation facet options from the Hub response. */
  @property({ attribute: false }) conversations: ProjectDecisionConversationView[] = [];

  /** Asking-agent facet options from the Hub response. */
  @property({ attribute: false }) agents: ProjectDecisionActorView[] = [];

  /** Recorded-answerer facet options from the Hub response. */
  @property({ attribute: false }) answerers: ProjectDecisionActorView[] = [];

  /** Optional extra tags (reserved for `scope="profile"` in P5). */
  @property({ attribute: false }) extraTags: DecisionsFilterTag[] = [];

  @state() private open = false;

  static override styles = css`
    :host {
      display: block;
      margin: 0.75rem 0;
    }

    .fpanel {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .sum {
      display: flex;
      align-items: center;
      gap: 0.45rem;
      padding: 0.4rem 0.65rem;
      flex-wrap: wrap;
      font-size: 0.85rem;
      color: var(--scion-text-secondary, #475569);
      min-height: 2.25rem;
      cursor: pointer;
    }

    .sum-toggle {
      display: inline-flex;
      align-items: center;
      gap: 0.4rem;
      background: transparent;
      border: none;
      padding: 0.15rem 0.2rem;
      margin: 0;
      font: inherit;
      color: inherit;
      cursor: pointer;
      border-radius: var(--scion-radius-sm, 0.25rem);
    }

    .sum-toggle:focus-visible {
      outline: 2px solid var(--scion-primary, #2563eb);
      outline-offset: 2px;
    }

    .chev {
      font-size: 0.8rem;
      color: var(--scion-text-muted, #64748b);
    }

    .lbl {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .kbd {
      font: 0.72rem var(--scion-font-mono, ui-monospace, SFMono-Regular, Menlo, monospace);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.25rem;
      padding: 0.05rem 0.35rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.3;
    }

    .muted {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8rem;
      white-space: nowrap;
    }

    .end {
      margin-left: auto;
      display: flex;
      gap: 0.35rem;
      align-items: center;
    }

    .body {
      border-top: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.65rem;
      display: flex;
      flex-direction: column;
      gap: 0.55rem;
    }

    .body[hidden] {
      display: none;
    }

    .search {
      width: 100%;
    }

    .empty-filters-note {
      margin: 0;
      font-size: 0.82rem;
      color: var(--scion-text-muted, #64748b);
    }

    sl-tag::part(base) {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-color: var(--scion-border, #e2e8f0);
      color: var(--scion-text, #1e293b);
    }

    sl-tag::part(remove-button) {
      color: var(--scion-text-muted, #64748b);
    }

    sl-input::part(form-control-label),
    sl-select::part(form-control-label) {
      color: var(--scion-text, #1e293b);
    }

    sl-input::part(base),
    sl-select::part(combobox) {
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    sl-input::part(input),
    sl-select::part(display-input) {
      color: var(--scion-text, #1e293b);
    }

    sl-input::part(input)::placeholder,
    sl-select::part(display-input)::placeholder {
      color: var(--scion-text-muted, #64748b);
    }

    sl-input::part(prefix),
    sl-input::part(clear-button),
    sl-select::part(expand-icon),
    sl-select::part(clear-button) {
      color: var(--scion-text-muted, #64748b);
    }

    sl-select::part(listbox) {
      background: var(--scion-surface, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
    }

    sl-option::part(base) {
      color: var(--scion-text, #1e293b);
    }

    .row2 {
      display: flex;
      gap: 0.5rem;
      flex-wrap: wrap;
      align-items: flex-end;
    }

    .row2 > scion-facet-combobox,
    .row2 > sl-select {
      flex: 1 1 11rem;
      min-width: 10rem;
    }

    .date-pair {
      display: flex;
      gap: 0.5rem;
      flex: 1 1 18rem;
    }

    .date-pair > sl-input {
      flex: 1 1 8.5rem;
      min-width: 8.5rem;
    }

    @media (max-width: 760px) {
      .row2 {
        flex-direction: column;
        align-items: stretch;
      }

      .row2 > scion-facet-combobox,
      .row2 > sl-select,
      .date-pair {
        flex: 1 1 auto;
        width: 100%;
        min-width: 0;
      }

      .date-pair > sl-input {
        flex: 1 1 0;
        min-width: 0;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.open = readStoredOpenPreference();
  }

  /** Whether the panel body is currently expanded. */
  get isOpen(): boolean {
    return this.open;
  }

  /** Expands the panel (persisting the preference) and focuses the search input (`/` shortcut, D13). */
  async expandAndFocusSearch(): Promise<void> {
    if (!this.supportsSearch) return;
    if (!this.open) {
      this.setOpen(true);
    }
    await this.updateComplete;
    const slInput = this.shadowRoot?.querySelector<
      HTMLElement & { focus?: () => void; shadowRoot?: ShadowRoot | null }
    >('sl-input[data-filter="q"]');
    if (!slInput) return;
    slInput.focus?.();
    const inner = slInput.shadowRoot?.querySelector<HTMLInputElement>('input');
    inner?.focus();
  }

  setOpen(next: boolean): void {
    this.open = next;
    writeStoredOpenPreference(next);
  }

  private get conversationOptions(): FacetComboboxOption[] {
    return this.conversations.map((c) => ({
      value: c.key,
      label: c.label || c.key,
      ...(c.hint ? { hint: c.hint } : {}),
    }));
  }

  private get agentOptions(): FacetComboboxOption[] {
    return this.agents.map((a) => ({
      value: a.id,
      label: a.name || a.id,
      ...(a.hint ? { hint: a.hint } : {}),
    }));
  }

  private get activeTags(): DecisionsFilterTag[] {
    const tags: DecisionsFilterTag[] = [...this.extraTags];

    if (this.supportsSearch) {
      const normalizedQ = normalizeDecisionsSearchQuery(this.q);
      if (normalizedQ) {
        tags.push({ key: 'q', label: `"${normalizedQ}"` });
      }

      if (this.conversation) {
        const convLabel =
          this.conversations.find((c) => c.key === this.conversation)?.label ?? this.conversation;
        tags.push({ key: 'conversation', label: `Conversation: ${convLabel}` });
      }
    }

    if ((this.supportsSearch || this.tab === 'made') && this.agent) {
      const actor = this.agents.find((a) => a.id === this.agent);
      const duplicateName =
        actor !== undefined && this.agents.filter((a) => a.name === actor.name).length > 1;
      const agentLabel = actor
        ? duplicateName && actor.hint
          ? `${actor.name} (${actor.hint})`
          : actor.name
        : this.agent;
      tags.push({ key: 'agent', label: `Agent: ${agentLabel}` });
    }

    if (this.tab === 'made') {
      if (this.user) {
        const userLabel =
          this.currentUserId && this.user === this.currentUserId
            ? 'you'
            : (this.answerers.find((u) => u.id === this.user)?.name ?? this.user);
        tags.push({ key: 'user', label: `Answered by: ${userLabel}` });
      }
      if (this.provenance) {
        tags.push({
          key: 'provenance',
          label: `Provenance: ${provenanceTagWord(this.provenance)}`,
        });
      }
      if (this.from) {
        tags.push({ key: 'from', label: `From: ${this.from}` });
      }
      if (this.to) {
        tags.push({ key: 'to', label: `To: ${this.to}` });
      }
    }

    return tags;
  }

  private selectValueOf(e: Event): string {
    const raw = (e.currentTarget as { value?: string | string[] } | null)?.value;
    const str = Array.isArray(raw) ? (raw[0] ?? '') : (raw ?? '');
    return str === 'all' ? '' : str.trim();
  }

  private handleSummaryBarClick(e: MouseEvent): void {
    const path = e.composedPath();
    for (const node of path) {
      if (!(node instanceof HTMLElement)) continue;
      if (node === e.currentTarget) break;
      const tag = node.tagName.toLowerCase();
      if (tag === 'button' || tag === 'sl-button' || tag === 'sl-tag') {
        return;
      }
    }
    this.setOpen(!this.open);
  }

  private handleSearchInput(e: Event): void {
    const val = (e.currentTarget as { value?: string } | null)?.value ?? '';
    this.dispatchEvent(
      new CustomEvent<DecisionsSearchInputDetail>('decisions-search-input', {
        detail: { value: val, immediate: val.trim() === '' },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleSearchClear(): void {
    this.dispatchEvent(
      new CustomEvent<DecisionsSearchInputDetail>('decisions-search-input', {
        detail: { value: '', immediate: true },
        bubbles: true,
        composed: true,
      })
    );
  }

  private removeTag(key: DecisionsFilterKey): void {
    switch (key) {
      case 'q':
        this.emitPatch({ q: '' });
        break;
      case 'conversation':
        this.emitPatch({ conversation: '' });
        break;
      case 'agent':
        this.emitPatch({ agent: '' });
        break;
      case 'user':
        this.emitPatch({ user: '' });
        break;
      case 'provenance':
        this.emitPatch({ provenance: '' });
        break;
      case 'from':
        this.emitPatch({ from: '' });
        break;
      case 'to':
        this.emitPatch({ to: '' });
        break;
      case 'project':
        this.emitPatch({ project: '' });
        break;
    }
  }

  private emitPatch(patch: DecisionsFilterPatch): void {
    this.dispatchEvent(
      new CustomEvent<DecisionsFilterPatch>('decisions-filter-change', {
        detail: patch,
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitClear(): void {
    this.dispatchEvent(
      new CustomEvent('decisions-filters-clear', {
        bubbles: true,
        composed: true,
      })
    );
  }

  override render(): TemplateResult {
    const tags = this.activeTags;
    const activeCount = tags.length;
    const displayedSearch = this.searchText ?? this.q;

    return html`
      <div class="fpanel" data-decisions-filter-panel data-scope=${this.scope}>
        <div class="sum" @click=${(e: MouseEvent): void => this.handleSummaryBarClick(e)}>
          <button
            type="button"
            class="sum-toggle"
            data-filter-panel-toggle
            aria-expanded=${this.open ? 'true' : 'false'}
            aria-controls="decisions-filter-body"
            @click=${(e: MouseEvent): void => {
              e.stopPropagation();
              this.setOpen(!this.open);
            }}
          >
            <sl-icon
              class="chev"
              name=${this.open ? 'chevron-down' : 'chevron-right'}
              aria-hidden="true"
            ></sl-icon>
            <sl-icon name="funnel" aria-hidden="true"></sl-icon>
            <span class="lbl">${this.supportsSearch ? 'Search and filters' : 'Filters'}</span>
          </button>

          ${tags.map(
            (tag) => html`
              <sl-tag
                size="small"
                removable
                data-filter-tag=${tag.key}
                @sl-remove=${(e: Event): void => {
                  e.stopPropagation();
                  this.removeTag(tag.key);
                }}
              >
                ${tag.label}
              </sl-tag>
            `
          )}

          <span class="end">
            ${activeCount > 0
              ? html`
                  <span class="muted" data-active-filter-count>${activeCount} active</span>
                  <sl-button
                    size="small"
                    variant="text"
                    data-clear-filters
                    @click=${(e: Event): void => {
                      e.stopPropagation();
                      this.emitClear();
                    }}
                  >
                    Clear
                  </sl-button>
                `
              : this.supportsSearch && !this.open
                ? html`<span class="kbd" aria-hidden="true">/</span>`
                : nothing}
          </span>
        </div>

        <div id="decisions-filter-body" class="body" ?hidden=${!this.open}>
          ${this.supportsSearch
            ? html`
                <sl-input
                  class="search"
                  size="small"
                  type="search"
                  clearable
                  maxlength=${MAX_DECISIONS_SEARCH_RUNES}
                  data-filter="q"
                  aria-label="Search decisions"
                  placeholder="Search questions, answers, keys, agents, conversations"
                  .value=${displayedSearch}
                  @sl-input=${(e: Event): void => this.handleSearchInput(e)}
                  @sl-clear=${(): void => this.handleSearchClear()}
                >
                  <sl-icon slot="prefix" name="search"></sl-icon>
                </sl-input>
              `
            : nothing}
          ${this.scope === 'project' && !this.supportsSearch && this.tab === 'pending'
            ? html`
                <p class="empty-filters-note" data-no-pending-filters>
                  No filters are available on this tab for this Hub version.
                </p>
              `
            : html`
                <div class="row2">
                  <slot name="leading-controls"></slot>

                  ${this.supportsSearch
                    ? html`
                        <scion-facet-combobox
                          data-filter="conversation"
                          label="Conversation"
                          placeholder="Any"
                          footer-text="↑↓ Enter Esc"
                          .options=${this.conversationOptions}
                          .value=${this.conversation}
                          @facet-change=${(e: CustomEvent<FacetChangeDetail>): void => {
                            this.emitPatch({ conversation: e.detail.value });
                          }}
                        ></scion-facet-combobox>
                      `
                    : nothing}
                  ${this.supportsSearch || this.tab === 'made'
                    ? html`
                        <scion-facet-combobox
                          data-filter="agent"
                          label="Agent"
                          placeholder="Any"
                          footer-text="Only agents that asked decisions here · ↑↓ Enter Esc"
                          .options=${this.agentOptions}
                          .value=${this.agent}
                          @facet-change=${(e: CustomEvent<FacetChangeDetail>): void => {
                            this.emitPatch({ agent: e.detail.value });
                          }}
                        ></scion-facet-combobox>
                      `
                    : nothing}
                  ${this.tab === 'made'
                    ? html`
                        <sl-select
                          size="small"
                          label="Answered by"
                          data-filter="user"
                          .value=${this.user || 'all'}
                          @sl-change=${(e: Event): void => {
                            this.emitPatch({ user: this.selectValueOf(e) });
                          }}
                        >
                          <sl-option value="all">Anyone</sl-option>
                          ${this.currentUserId
                            ? html`<sl-option value=${this.currentUserId}>You</sl-option>`
                            : nothing}
                          ${this.answerers
                            .filter((u) => u.id && u.id !== this.currentUserId)
                            .map((u) => html`<sl-option value=${u.id}>${u.name}</sl-option>`)}
                        </sl-select>

                        <sl-select
                          size="small"
                          label="Provenance"
                          data-filter="provenance"
                          .value=${this.provenance || 'all'}
                          @sl-change=${(e: Event): void => {
                            this.emitPatch({
                              provenance: this.selectValueOf(e) as MadeDecisionsProvenanceFilter,
                            });
                          }}
                        >
                          <sl-option value="all">Any</sl-option>
                          <sl-option value="direct">Direct</sl-option>
                          <sl-option value="interpreted">Interpreted</sl-option>
                          <sl-option value="confirmed">Confirmed</sl-option>
                          <sl-option value="overridden">Changed</sl-option>
                        </sl-select>

                        <div class="date-pair">
                          <sl-input
                            size="small"
                            type="date"
                            label="From"
                            data-filter="from"
                            .value=${this.from}
                            @sl-change=${(e: Event): void => {
                              const val = (
                                (e.currentTarget as { value?: string } | null)?.value ?? ''
                              ).trim();
                              this.emitPatch({ from: val });
                            }}
                          ></sl-input>

                          <sl-input
                            size="small"
                            type="date"
                            label="To"
                            data-filter="to"
                            .value=${this.to}
                            @sl-change=${(e: Event): void => {
                              const val = (
                                (e.currentTarget as { value?: string } | null)?.value ?? ''
                              ).trim();
                              this.emitPatch({ to: val });
                            }}
                          ></sl-input>
                        </div>
                      `
                    : nothing}

                  <slot name="trailing-controls"></slot>
                </div>
              `}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-decisions-filter-panel': ScionDecisionsFilterPanel;
  }
}
