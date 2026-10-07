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
 * "Attach artifact" picker for the chat composer (ptone/scion#3224).
 *
 * Lists the artifacts the viewer can read (GET /api/v1/artifacts?mine=1),
 * with search and All / Owned by me / This project filters, and lets the
 * user pick up to the remaining per-message limit. Picking attaches the
 * current version. The hub checks every picked reference again at send.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../../client/api.js';
import {
  artifactListUrl,
  formatArtifactRef,
  type ArtifactListItem,
  type ArtifactListResponse,
} from '../../../client/artifacts.js';
import { formatInstant, formatRelative } from '../../../utils/time.js';

/** An artifact picked in the composer, waiting to be sent. */
export interface PendingArtifact {
  id: string;
  /** Canonical reference sent in the message metadata. */
  ref: string;
  title: string;
  version: number;
}

/** Detail of the `artifact-picker-select` event. */
export interface ArtifactPickerSelectDetail {
  artifacts: PendingArtifact[];
}

type PickerFilter = 'all' | 'owned' | 'project';

/** Delay before a search keystroke reloads the list. */
const SEARCH_DEBOUNCE_MS = 250;

/** Rows fetched per page. */
const PAGE_SIZE = 25;

/** Name lookups run at most this many at a time. */
const NAME_LOOKUP_CONCURRENCY = 6;

@customElement('scion-artifact-picker')
export class ScionArtifactPicker extends LitElement {
  /** Whether the dialog is shown. The host sets it and clears it on close. */
  @property({ type: Boolean })
  open = false;

  /** The conversation's project, for the "This project" filter. */
  @property()
  projectId = '';

  /** How many more artifacts the message may carry. */
  @property({ type: Number })
  remaining = 10;

  /** Artifacts already attached in the composer; shown checked and fixed. */
  @property({ attribute: false })
  attachedIds: readonly string[] = [];

  @state() private query = '';
  @state() private filter: PickerFilter = 'all';
  @state() private items: ArtifactListItem[] = [];
  @state() private nextCursor = '';
  @state() private loading = false;
  @state() private error = '';
  @state() private selected = new Map<string, PendingArtifact>();
  @state() private names = new Map<string, string>();

  private generation = 0;
  private searchTimer: ReturnType<typeof setTimeout> | null = null;

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      this.selected = new Map();
      this.query = '';
      this.filter = 'all';
      void this.load(false);
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) clearTimeout(this.searchTimer);
  }

  private async load(more: boolean): Promise<void> {
    const gen = ++this.generation;
    this.loading = true;
    this.error = '';
    try {
      const res = await apiFetch(
        artifactListUrl({
          q: this.query,
          ownedOnly: this.filter === 'owned',
          ...(more && this.nextCursor ? { cursor: this.nextCursor } : {}),
          limit: PAGE_SIZE,
        })
      );
      if (gen !== this.generation) return;
      if (!res.ok) {
        this.error = await extractApiError(res, 'Could not load artifacts');
        if (!more) this.items = [];
        return;
      }
      const data = (await res.json()) as ArtifactListResponse;
      if (gen !== this.generation) return;
      this.items = more ? [...this.items, ...data.artifacts] : data.artifacts;
      this.nextCursor = data.nextCursor ?? '';
      void this.loadNames(data.artifacts);
    } catch {
      if (gen === this.generation) this.error = 'Could not load artifacts';
    } finally {
      if (gen === this.generation) this.loading = false;
    }
  }

  /** Best-effort display names for agent owners and home projects. */
  private async loadNames(items: ArtifactListItem[]): Promise<void> {
    const wanted = new Set<string>();
    for (const a of items) {
      if (a.ownerKind === 'agent') wanted.add(`agent:${a.ownerRef}`);
      if (a.scopeRef) wanted.add(`project:${a.scopeRef}`);
    }
    const keys = [...wanted].filter((k) => !this.names.has(k));
    const lookup = async (key: string): Promise<void> => {
      const kind = key.slice(0, key.indexOf(':'));
      const id = key.slice(key.indexOf(':') + 1);
      const url =
        kind === 'agent'
          ? `/api/v1/agents/${encodeURIComponent(id)}`
          : `/api/v1/projects/${encodeURIComponent(id)}`;
      try {
        const res = await apiFetch(url, { suppressAccessDeniedToast: true });
        if (!res.ok) return;
        const body = (await res.json()) as { name?: string; slug?: string };
        const name = body.name || body.slug;
        if (name) this.names = new Map(this.names).set(key, name);
      } catch {
        // The row shows a generic label instead.
      }
    };
    for (let i = 0; i < keys.length; i += NAME_LOOKUP_CONCURRENCY) {
      await Promise.all(keys.slice(i, i + NAME_LOOKUP_CONCURRENCY).map(lookup));
    }
  }

  private ownerLabel(a: ArtifactListItem): string {
    if (a.ownerKind === 'agent') {
      const name = this.names.get(`agent:${a.ownerRef}`);
      return name ? `${name} (agent)` : 'Agent';
    }
    return 'User';
  }

  private projectLabel(a: ArtifactListItem): string {
    if (a.scopeRef && a.scopeRef === this.projectId) return 'This project';
    return this.names.get(`project:${a.scopeRef}`) ?? '';
  }

  private onSearch(e: Event): void {
    this.query = (e.target as HTMLInputElement).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.searchTimer = setTimeout(() => {
      this.searchTimer = null;
      void this.load(false);
    }, SEARCH_DEBOUNCE_MS);
  }

  private setFilter(filter: PickerFilter): void {
    if (this.filter === filter) return;
    const reload = filter === 'owned' || this.filter === 'owned';
    this.filter = filter;
    if (reload) void this.load(false);
  }

  private toggle(a: ArtifactListItem): void {
    const next = new Map(this.selected);
    if (next.has(a.id)) {
      next.delete(a.id);
    } else {
      if (next.size >= this.remaining) return;
      next.set(a.id, {
        id: a.id,
        ref: formatArtifactRef(a.id),
        title: a.title,
        version: a.currentSeq,
      });
    }
    this.selected = next;
  }

  private visibleItems(): ArtifactListItem[] {
    if (this.filter !== 'project') return this.items;
    return this.items.filter((a) => a.scopeRef === this.projectId);
  }

  private attach(): void {
    if (this.selected.size === 0) return;
    this.dispatchEvent(
      new CustomEvent<ArtifactPickerSelectDetail>('artifact-picker-select', {
        detail: { artifacts: [...this.selected.values()] },
        bubbles: true,
        composed: true,
      })
    );
    this.close();
  }

  private close(): void {
    this.dispatchEvent(new CustomEvent('artifact-picker-close', { bubbles: true, composed: true }));
  }

  private renderRows() {
    const rows = this.visibleItems();
    if (this.loading && this.items.length === 0) {
      return html`<div class="placeholder"><sl-spinner></sl-spinner></div>`;
    }
    if (this.error) {
      return html`<div class="placeholder error">${this.error}</div>`;
    }
    if (rows.length === 0 && this.nextCursor) {
      // "This project" filters the pages loaded so far (the list has no
      // scope parameter), so more pages may still hold matches.
      return html`<div class="placeholder">No matches in the loaded artifacts.</div>
        ${this.renderLoadMore()}`;
    }
    if (rows.length === 0) {
      return this.query.trim() || this.filter !== 'all'
        ? html`<div class="placeholder">No artifacts match.</div>`
        : html`<div class="placeholder empty">
            <div class="empty-title">No artifacts yet</div>
            Artifacts you can read appear here. Agents publish them as they work.
          </div>`;
    }
    const attached = new Set(this.attachedIds);
    const full = this.selected.size >= this.remaining;
    return html`
      <table>
        <thead>
          <tr>
            <th></th>
            <th>Title</th>
            <th>Owner</th>
            <th>Home project</th>
            <th>Version</th>
            <th>Updated</th>
          </tr>
        </thead>
        <tbody>
          ${rows.map((a) => {
            const isAttached = attached.has(a.id);
            const checked = isAttached || this.selected.has(a.id);
            const disabled = isAttached || (!checked && full);
            return html`
              <tr
                class=${checked ? 'selected' : ''}
                @click=${() => {
                  if (!disabled || this.selected.has(a.id)) this.toggle(a);
                }}
              >
                <td>
                  <sl-checkbox
                    ?checked=${checked}
                    ?disabled=${disabled && !this.selected.has(a.id)}
                    aria-label="Select ${a.title}"
                    @click=${(e: Event) => e.stopPropagation()}
                    @sl-change=${() => this.toggle(a)}
                  ></sl-checkbox>
                </td>
                <td class="title">${a.title}</td>
                <td>${this.ownerLabel(a)}</td>
                <td>${this.projectLabel(a)}</td>
                <td>v${a.currentSeq}</td>
                <td class="muted" title=${formatInstant(a.updatedAt, 'datetime')}>
                  ${formatRelative(a.updatedAt)}
                </td>
              </tr>
            `;
          })}
        </tbody>
      </table>
      ${this.renderLoadMore()}
    `;
  }

  private renderLoadMore() {
    if (!this.nextCursor) return nothing;
    return html`<div class="more">
      <sl-button size="small" ?loading=${this.loading} @click=${() => this.load(true)}>
        Load more
      </sl-button>
    </div>`;
  }

  override render() {
    if (!this.open) return nothing;
    const filters: { id: PickerFilter; label: string }[] = [
      { id: 'all', label: 'All I can read' },
      { id: 'owned', label: 'Owned by me' },
      ...(this.projectId ? [{ id: 'project' as PickerFilter, label: 'This project' }] : []),
    ];
    return html`
      <sl-dialog
        class="artifact-picker"
        open
        label="Attach artifact"
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.close();
        }}
      >
        <sl-input
          class="search"
          placeholder="Search title…"
          clearable
          .value=${this.query}
          @sl-input=${(e: Event) => this.onSearch(e)}
        >
          <sl-icon name="search" slot="prefix"></sl-icon>
        </sl-input>
        <div class="filters" role="group" aria-label="Filter">
          ${filters.map(
            (f) =>
              html`<button
                type="button"
                class="filter ${this.filter === f.id ? 'on' : ''}"
                aria-pressed=${this.filter === f.id ? 'true' : 'false'}
                @click=${() => this.setFilter(f.id)}
              >
                ${f.label}
              </button>`
          )}
        </div>
        <div class="rows">${this.renderRows()}</div>
        <div class="hint">
          Attaches the current version. Recipients see it only if they can read it.
        </div>
        <div slot="footer" class="footer">
          <span class="count"
            >${this.selected.size > 0 ? `${this.selected.size} selected` : ''}</span
          >
          <sl-button size="small" @click=${() => this.close()}>Cancel</sl-button>
          <sl-button
            size="small"
            variant="primary"
            ?disabled=${this.selected.size === 0}
            @click=${() => this.attach()}
          >
            Attach
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  static override styles = css`
    :host {
      display: contents;
    }
    .artifact-picker::part(panel) {
      width: min(92vw, 760px);
    }
    .search {
      margin-bottom: 0.625rem;
    }
    .filters {
      display: flex;
      gap: 0.375rem;
      margin-bottom: 0.625rem;
      flex-wrap: wrap;
    }
    .filter {
      font: inherit;
      font-size: var(--chat-fs-sm, 0.8125rem);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 999px;
      padding: 0.125rem 0.625rem;
      background: transparent;
      color: var(--scion-text-muted, #475569);
      cursor: pointer;
    }
    .filter.on {
      background: var(--sl-color-primary-50, #eff6ff);
      border-color: var(--sl-color-primary-300, #93c5fd);
      color: var(--sl-color-primary-700, #1d4ed8);
    }
    .rows {
      max-height: 50vh;
      overflow: auto;
    }
    table {
      width: 100%;
      border-collapse: collapse;
      font-size: var(--chat-fs-sm, 0.8125rem);
    }
    th {
      text-align: left;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      padding: 0.375rem 0.5rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }
    td {
      padding: 0.5rem;
      border-bottom: 1px solid var(--scion-border-subtle, #f1f5f9);
    }
    tbody tr {
      cursor: pointer;
    }
    tr.selected td {
      background: var(--sl-color-primary-50, #eff6ff);
    }
    td.title {
      font-weight: 600;
    }
    .muted {
      color: var(--scion-text-muted, #64748b);
    }
    .placeholder {
      padding: 2rem 0.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
    }
    .placeholder.error {
      color: var(--sl-color-danger-600, #dc2626);
    }
    .empty-title {
      font-size: 1rem;
      color: var(--scion-text, #0f172a);
      margin-bottom: 0.375rem;
    }
    .more {
      display: flex;
      justify-content: center;
      padding: 0.5rem;
    }
    .hint {
      margin-top: 0.5rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
    .footer {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      width: 100%;
    }
    .footer .count {
      flex: 1;
      text-align: left;
      color: var(--scion-text-muted, #64748b);
      font-size: var(--chat-fs-sm, 0.8125rem);
    }
  `;
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-artifact-picker': ScionArtifactPicker;
  }
}
