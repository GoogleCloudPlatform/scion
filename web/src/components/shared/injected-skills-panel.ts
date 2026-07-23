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
 * Injected Skills Panel Component
 *
 * Shared component for managing injected skills across project, user, and hub
 * scopes. Accepts a scope and scopeId prop and calls the appropriate API
 * endpoints. Hub system entries render as read-only with a lock badge.
 *
 * Scopes:
 *   project → GET/POST/PUT/DELETE /api/v1/projects/{scopeId}/injected-skills[/{id}]
 *   user    → GET/POST/PUT/DELETE /api/v1/users/me/injected-skills[/{id}]
 *   hub     → GET/PUT /api/v1/hub/settings/injected-skills
 *             (system entries are always read-only; user_defined entries editable)
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { Skill } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

export type InjectedSkillsScope = 'project' | 'user' | 'hub';

/** Normalized internal row — covers both SkillInjectionEntry and SkillReference shapes. */
interface SkillRow {
  /** Entry ID — present for project/user scopes (from SkillInjectionEntry); empty for hub. */
  id: string;
  /** Canonical skill URI (field name differs: skillUri vs uri). */
  uri: string;
  /** Alias override, if any. */
  as: string;
  /** Whether a resolution failure is non-fatal. */
  optional: boolean;
  /** Position for drag-based reorder (project/user only). */
  sortOrder: number;
  /** Enriched skill name (if URI resolves to skill bank). */
  skillName: string;
  /** Enriched skill slug (if URI resolves to skill bank). */
  skillSlug: string;
  /** True for hub system entries — cannot be edited or removed. */
  readonly: boolean;
}

@customElement('scion-injected-skills-panel')
export class ScionInjectedSkillsPanel extends LitElement {
  /** Which scope this panel manages. */
  @property() scope: InjectedSkillsScope = 'project';

  /** Project or user UUID; empty for hub scope. */
  @property() scopeId = '';

  /** When true, the entire panel is read-only (used for system hub entries). */
  @property({ type: Boolean }) readonly = false;

  @state() private loading = true;
  @state() private rows: SkillRow[] = [];
  @state() private error: string | null = null;

  // Add dialog
  @state() private dialogOpen = false;
  @state() private dialogMode: 'search' | 'uri' = 'search';
  @state() private dialogSkillQuery = '';
  @state() private dialogSkillResults: Skill[] = [];
  @state() private dialogSkillSearching = false;
  @state() private dialogSelectedSkill: Skill | null = null;
  @state() private dialogUri = '';
  @state() private dialogAs = '';
  @state() private dialogOptional = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Delete — tracked by index to avoid key collisions on hub rows (all have id='')
  @state() private _deletingIndex: number | null = null;

  // Drag
  @state() private dragSourceIndex: number | null = null;
  @state() private dragOverIndex: number | null = null;

  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private _searchAbortController: AbortController | null = null;
  private _loadAbortController: AbortController | null = null;

  static override styles = [
    resourceStyles,
    css`
      .panel-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 1rem;
        gap: 1rem;
      }

      .panel-header-info p {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        margin: 0;
      }

      .drag-handle {
        cursor: grab;
        color: var(--scion-text-muted, #64748b);
        font-size: 1rem;
        display: flex;
        align-items: center;
        padding: 0 0.25rem;
      }

      .drag-handle:active {
        cursor: grabbing;
      }

      tr.drag-over td {
        background: var(--sl-color-primary-50, #eff6ff);
      }

      tr.dragging {
        opacity: 0.4;
      }

      .system-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        border: 1px solid var(--scion-border, #e2e8f0);
      }

      .system-badge sl-icon {
        font-size: 0.625rem;
      }

      .skill-name {
        font-weight: 600;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .skill-uri {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 300px;
      }

      .skill-info {
        display: flex;
        flex-direction: column;
      }

      .mode-toggle {
        display: flex;
        gap: 0.5rem;
        margin-bottom: 0.75rem;
      }

      .mode-toggle sl-button[variant='primary'] {
        font-weight: 600;
      }

      .search-results {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        max-height: 200px;
        overflow-y: auto;
        margin-top: 0.5rem;
      }

      .search-result-item {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        cursor: pointer;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        font-size: 0.875rem;
      }

      .search-result-item:last-child {
        border-bottom: none;
      }

      .search-result-item:hover,
      .search-result-item.selected {
        background: var(--sl-color-primary-50, #eff6ff);
      }

      .search-result-item .skill-slug {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .no-results {
        padding: 0.75rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    // Lit applies element properties/attributes AFTER connectedCallback via its
    // update cycle. For project-scoped panels, scopeId may still be '' here,
    // which would produce a malformed URL (/api/v1/projects//injected-skills).
    // Guard: only load now if scope is set and (for project) scopeId is non-empty.
    // updated() re-triggers load() once both values are available.
    if (this.scope && (this.scope !== 'project' || this.scopeId)) {
      void this.load();
    }
  }

  override updated(changedProperties: PropertyValues): void {
    if (changedProperties.has('scopeId') || changedProperties.has('scope')) {
      if (this.scope && (this.scope !== 'project' || this.scopeId)) {
        void this.load();
      } else {
        // Clear stale rows when scope/scopeId becomes invalid so that a panel
        // reused with a new (empty) scopeId does not show data from the
        // previous scope. Abort any in-flight load first so it cannot overwrite
        // the cleared state with stale data from the previous scope.
        this._loadAbortController?.abort();
        this._loadAbortController = null;
        this.loading = false;
        this.rows = [];
        this.error = null;
      }
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    this.cancelSearch();
    this._loadAbortController?.abort();
    this._loadAbortController = null;
  }

  /** Abort any in-flight skill search and clear the controller reference. */
  private cancelSearch(): void {
    this._searchAbortController?.abort();
    this._searchAbortController = null;
  }

  // ── API helpers ──────────────────────────────────────────────────────────

  private get apiBase(): string {
    switch (this.scope) {
      case 'project':
        return `/api/v1/projects/${this.scopeId}/injected-skills`;
      case 'user':
        return `/api/v1/users/me/injected-skills`;
      case 'hub':
        return `/api/v1/hub/settings/injected-skills`;
    }
  }

  async load(): Promise<void> {
    // Cancel any in-flight load and start fresh — same pattern as _searchAbortController.
    // This handles rapid scope/scopeId changes: the stale request is aborted immediately
    // and only the latest load() commits rows to state.
    this._loadAbortController?.abort();
    this._loadAbortController = new AbortController();
    const { signal } = this._loadAbortController;

    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch(this.apiBase, { signal });
      if (signal.aborted) return; // Aborted after fetch completed — discard
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      if (this.scope === 'hub') {
        const data = (await res.json()) as {
          system?: Array<{ uri: string; as?: string; optional?: boolean }>;
          user_defined?: Array<{ uri: string; as?: string; optional?: boolean }>;
        };
        if (signal.aborted) return; // Aborted while parsing — discard
        const systemRows: SkillRow[] = (data.system || []).map((s, i) => ({
          id: '',
          uri: s.uri,
          as: s.as || '',
          optional: s.optional ?? false,
          sortOrder: i,
          skillName: '',
          skillSlug: '',
          readonly: true,
        }));
        const userRows: SkillRow[] = (data.user_defined || []).map((s, i) => ({
          id: '',
          uri: s.uri,
          as: s.as || '',
          optional: s.optional ?? false,
          sortOrder: i,
          skillName: '',
          skillSlug: '',
          readonly: false,
        }));
        this.rows = [...systemRows, ...userRows];
      } else {
        const data = (await res.json()) as {
          entries?: Array<{
            id: string;
            skillUri: string;
            skillAs?: string;
            optional?: boolean;
            sortOrder?: number;
            skillName?: string;
            skillSlug?: string;
          }>;
        };
        if (signal.aborted) return; // Aborted while parsing — discard
        this.rows = (data.entries || []).map((e) => ({
          id: e.id,
          uri: e.skillUri,
          as: e.skillAs || '',
          optional: e.optional ?? false,
          sortOrder: e.sortOrder ?? 0,
          skillName: e.skillName || '',
          skillSlug: e.skillSlug || '',
          readonly: false,
        }));
      }
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') {
        return; // Silently abort — caller will call load() again with the correct scope
      }
      this.error = err instanceof Error ? err.message : 'Failed to load injected skills';
    } finally {
      if (!signal.aborted) {
        this.loading = false;
      }
    }
  }

  private async addEntry(uri: string, skillAs: string, optional: boolean): Promise<void> {
    if (this.scope === 'hub') {
      // For hub: append to user_defined and PUT the full user_defined list
      const userDefined = this.rows
        .filter((r) => !r.readonly)
        .map((r) => this.rowToSkillRef(r));
      userDefined.push(this.buildSkillRef(uri, skillAs, optional));
      await this.putHubUserDefined(userDefined);
    } else {
      const res = await apiFetch(this.apiBase, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ skillUri: uri, skillAs: skillAs || undefined, optional }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to add skill (HTTP ${res.status})`));
      }
    }
    await this.load();
  }

  private async deleteEntry(row: SkillRow, rowIndex: number): Promise<void> {
    if (this.scope === 'hub') {
      // For hub: remove from user_defined and PUT.
      // Filter by index (not URI) so duplicate URIs don't cause silent double-deletion.
      const userDefined = this.rows
        .filter((r, i) => !r.readonly && i !== rowIndex)
        .map((r) => this.rowToSkillRef(r));
      await this.putHubUserDefined(userDefined);
    } else {
      const res = await apiFetch(`${this.apiBase}/${encodeURIComponent(row.id)}`, {
        method: 'DELETE',
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to delete skill (HTTP ${res.status})`));
      }
    }
    await this.load();
  }

  private async reorder(newOrder: SkillRow[]): Promise<void> {
    if (this.scope === 'hub') {
      const userDefined = newOrder
        .filter((r) => !r.readonly)
        .map((r) => this.rowToSkillRef(r));
      await this.putHubUserDefined(userDefined);
    } else {
      const entries = newOrder.map((r, i) => ({
        id: r.id,
        skillUri: r.uri,
        skillAs: r.as || undefined,
        optional: r.optional,
        sortOrder: i + 1,
      }));
      const res = await apiFetch(this.apiBase, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ entries }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to reorder skills (HTTP ${res.status})`));
      }
    }
    await this.load();
  }

  /** Convert a SkillRow to the SkillReference wire format for hub PUT. */
  private rowToSkillRef(r: SkillRow): { uri: string; as?: string; optional?: boolean } {
    return this.buildSkillRef(r.uri, r.as, r.optional);
  }

  /** Build a SkillReference object, omitting undefined optional fields. */
  private buildSkillRef(
    uri: string,
    as: string,
    optional: boolean
  ): { uri: string; as?: string; optional?: boolean } {
    const ref: { uri: string; as?: string; optional?: boolean } = { uri };
    if (as) ref.as = as;
    if (optional) ref.optional = true;
    return ref;
  }

  // Note: hub-scope skill injection uses a PUT-whole-list API (no per-item DELETE endpoint).
  // Concurrent deletes can cause a lost-update race: if two deletes are in flight simultaneously,
  // the second PUT will overwrite the first. This is an architectural limitation of the hub API
  // and should be addressed by adding a per-item DELETE endpoint in a future change.
  private async putHubUserDefined(
    userDefined: Array<{ uri: string; as?: string; optional?: boolean }>
  ): Promise<void> {
    const res = await apiFetch(this.apiBase, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user_defined: userDefined }),
    });
    if (!res.ok) {
      throw new Error(await extractApiError(res, `Failed to update hub skills (HTTP ${res.status})`));
    }
  }

  // ── Dialog ────────────────────────────────────────────────────────────────

  private openDialog(): void {
    this.dialogMode = 'search';
    this.dialogSkillQuery = '';
    this.dialogSkillResults = [];
    this.dialogSkillSearching = false;
    this.dialogSelectedSkill = null;
    this.dialogUri = '';
    this.dialogAs = '';
    this.dialogOptional = false;
    this.dialogLoading = false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
    // Abort any in-flight search so it doesn't update state after close.
    this.cancelSearch();
  }

  private handleSearchInput(query: string): void {
    this.dialogSkillQuery = query;
    this.dialogSelectedSkill = null;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    if (!query.trim()) {
      this.cancelSearch();
      this.dialogSkillSearching = false;
      this.dialogSkillResults = [];
      return;
    }
    this.dialogSkillSearching = true;
    this.searchTimer = setTimeout(() => void this.searchSkills(query), 300);
  }

  private async searchSkills(query: string): Promise<void> {
    this._searchAbortController?.abort();
    this._searchAbortController = new AbortController();
    const { signal } = this._searchAbortController;
    try {
      const res = await apiFetch(
        `/api/v1/skills?q=${encodeURIComponent(query)}&status=active&limit=20`,
        { signal }
      );
      if (res.ok) {
        const data = (await res.json()) as { skills?: Skill[] } | Skill[];
        this.dialogSkillResults = Array.isArray(data) ? data : (data as { skills?: Skill[] }).skills || [];
      }
    } catch (err) {
      if (err instanceof Error && err.name === 'AbortError') return; // Stale request — discard
      // Non-critical — just show no results
    } finally {
      // Only clear the spinner if this request was not aborted — a newer in-flight
      // request (or closeDialog) may have aborted it and its spinner is still needed.
      if (!signal.aborted) {
        this.dialogSkillSearching = false;
      }
    }
  }

  private async handleAddSkill(e: Event): Promise<void> {
    e.preventDefault();

    let uri = '';
    if (this.dialogMode === 'search') {
      if (!this.dialogSelectedSkill) {
        this.dialogError = 'Please select a skill from the search results';
        return;
      }
      // Build a skill bank URI from the skill's slug
      uri = `skill://${this.dialogSelectedSkill.slug}`;
    } else {
      uri = this.dialogUri.trim();
      if (!uri) {
        this.dialogError = 'Skill URI is required';
        return;
      }
    }

    this.dialogLoading = true;
    this.dialogError = null;
    try {
      await this.addEntry(uri, this.dialogAs.trim(), this.dialogOptional);
      this.closeDialog();
    } catch (err) {
      this.dialogError = err instanceof Error ? err.message : 'Failed to add skill';
    } finally {
      this.dialogLoading = false;
    }
  }

  // ── Drag & drop ──────────────────────────────────────────────────────────

  private handleDragStart(index: number, e: DragEvent): void {
    this.dragSourceIndex = index;
    if (e.dataTransfer) {
      e.dataTransfer.effectAllowed = 'move';
    }
  }

  private handleDragOver(index: number, e: DragEvent): void {
    const targetRow = this.rows[index];
    if (targetRow?.readonly) {
      // Readonly rows reject drops — no highlight, no cursor change, no position update.
      // Do NOT call e.preventDefault() so the browser keeps its default "no-drop" cursor.
      return;
    }
    e.preventDefault();
    if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
    this.dragOverIndex = index;
  }

  private handleDragLeave(): void {
    this.dragOverIndex = null;
  }

  private handleDrop(targetIndex: number, e: DragEvent): void {
    e.preventDefault();
    this.dragOverIndex = null;
    if (this.dragSourceIndex === null || this.dragSourceIndex === targetIndex) {
      this.dragSourceIndex = null;
      return;
    }
    const sourceIndex = this.dragSourceIndex;
    const newOrder = [...this.rows];
    const [moved] = newOrder.splice(sourceIndex, 1);
    // For drag-down (source < target), removing the source shifts all elements
    // after it up by 1. targetIndex now points one slot past the intended drop
    // position (the element AFTER the intended target). Subtract 1 to correct.
    // For drag-up (source > target), indices are unaffected — no adjustment.
    const insertAt = sourceIndex < targetIndex ? targetIndex - 1 : targetIndex;
    newOrder.splice(insertAt, 0, moved);
    this.dragSourceIndex = null;
    // Optimistic update
    this.rows = newOrder;
    void this.reorder(newOrder).catch((err) => {
      console.error('Reorder failed:', err);
      // Revert optimistic update; if reload also fails, surface an error message.
      void this.load().catch((reloadErr) => {
        console.error('Failed to reload after reorder error:', reloadErr);
        this.error = 'Failed to reload — please refresh';
      });
    });
  }

  private handleDragEnd(): void {
    this.dragSourceIndex = null;
    this.dragOverIndex = null;
  }

  // ── Rendering ────────────────────────────────────────────────────────────

  override render() {
    const canEdit = !this.readonly;

    return html`
      <div class="panel-header">
        <div class="panel-header-info">
          <p>${this.renderDescription()}</p>
        </div>
        ${canEdit
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openDialog}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add Skill
              </sl-button>
            `
          : nothing}
      </div>

      ${this.loading
        ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading skills…</div>`
        : this.error
          ? html`
              <div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.load()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>
            `
          : this.rows.length === 0
            ? this.renderEmpty()
            : this.renderTable()}

      ${this.renderDialog()}
    `;
  }

  private renderDescription(): string {
    switch (this.scope) {
      case 'project':
        return 'Skills automatically injected into every agent in this project.';
      case 'user':
        return 'Skills automatically injected into every agent you own, across all projects.';
      case 'hub':
        return 'Skills automatically injected into all agents on this hub.';
    }
  }

  private renderEmpty() {
    const canEdit = !this.readonly;
    return html`
      <div class="empty-state">
        <sl-icon name="puzzle"></sl-icon>
        <h3>No Injected Skills</h3>
        <p>${this.renderDescription()}</p>
        ${canEdit
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openDialog}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Add Skill
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  private renderTable() {
    const canEdit = !this.readonly;
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              ${canEdit ? html`<th style="width: 2rem;"></th>` : nothing}
              <th>Skill</th>
              <th>Alias (as)</th>
              <th>Optional</th>
              ${canEdit ? html`<th class="actions-cell"></th>` : nothing}
            </tr>
          </thead>
          <tbody>
            ${this.rows.map((row, index) => this.renderRow(row, index, canEdit))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(row: SkillRow, index: number, canEdit: boolean) {
    const isDeleting = this._deletingIndex === index;
    const isDragging = this.dragSourceIndex === index;
    const isDragOver = this.dragOverIndex === index;
    const rowReadonly = row.readonly;

    // A row can be dragged only if it's not readonly and we have a skill with an id
    const draggable = canEdit && !rowReadonly;

    return html`
      <tr
        class=${[isDragging ? 'dragging' : '', isDragOver ? 'drag-over' : ''].join(' ')}
        draggable=${draggable ? 'true' : 'false'}
        @dragstart=${draggable ? (e: DragEvent) => this.handleDragStart(index, e) : nothing}
        @dragover=${canEdit ? (e: DragEvent) => this.handleDragOver(index, e) : nothing}
        @dragleave=${canEdit ? () => this.handleDragLeave() : nothing}
        @drop=${(e: DragEvent) => this.handleDrop(index, e)}
        @dragend=${draggable ? () => this.handleDragEnd() : nothing}
      >
        ${canEdit
          ? html`
              <td style="width: 2rem; padding: 0.5rem;">
                ${rowReadonly
                  ? nothing
                  : html`<span class="drag-handle" title="Drag to reorder">⠿</span>`}
              </td>
            `
          : nothing}
        <td>
          <div class="skill-info">
            ${row.skillName
              ? html`<span class="skill-name">${row.skillName}</span>`
              : nothing}
            <span class="skill-uri">${row.uri}</span>
            ${row.skillSlug
              ? html`<span class="skill-uri">/${row.skillSlug}</span>`
              : nothing}
          </div>
          ${rowReadonly
            ? html`
                <span class="system-badge" style="margin-top: 0.25rem; display: inline-flex;">
                  <sl-icon name="lock"></sl-icon>
                  System
                </span>
              `
            : nothing}
        </td>
        <td>
          ${row.as
            ? html`<span class="key-cell">${row.as}</span>`
            : html`<span style="color: var(--scion-text-muted, #64748b); font-size: 0.8125rem;">—</span>`}
        </td>
        <td>
          ${row.optional
            ? html`<sl-icon name="check-circle" style="color: var(--sl-color-success-600, #16a34a);"></sl-icon>`
            : html`<sl-icon name="x-circle" style="color: var(--scion-text-muted, #64748b);"></sl-icon>`}
        </td>
        ${canEdit
          ? html`
              <td class="actions-cell">
                ${rowReadonly
                  ? nothing
                  : html`
                      <sl-icon-button
                        name="trash"
                        label="Remove"
                        ?disabled=${isDeleting}
                        @click=${() => this.handleDeleteRow(row, index)}
                        style="color: var(--sl-color-danger-600, #dc2626);"
                      ></sl-icon-button>
                    `}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  private async handleDeleteRow(row: SkillRow, rowIndex: number): Promise<void> {
    const label = row.skillName || row.uri;
    if (!confirm(`Remove skill "${label}" from this ${this.scope === 'hub' ? 'hub' : this.scope === 'project' ? 'project' : 'profile'}?`)) {
      return;
    }
    // Guard against stale rowIndex: a concurrent drag-reorder between the click
    // and the confirm() call could shift row positions. Re-find the row by its
    // stable identity (URI + id) before committing the delete.
    let resolvedIndex = rowIndex;
    const currentAtIndex = this.rows[rowIndex];
    if (!currentAtIndex || currentAtIndex.uri !== row.uri || currentAtIndex.id !== row.id) {
      const found = this.rows.findIndex((r) => r.uri === row.uri && r.id === row.id);
      if (found === -1) return; // Row no longer present — cancel delete
      resolvedIndex = found;
    }
    this._deletingIndex = resolvedIndex;
    try {
      await this.deleteEntry(row, resolvedIndex);
    } catch (err) {
      console.error('Failed to delete skill:', err);
      alert(err instanceof Error ? err.message : 'Failed to remove skill');
    } finally {
      this._deletingIndex = null;
    }
  }

  private renderDialog() {
    const isSearchMode = this.dialogMode === 'search';

    return html`
      <sl-dialog
        label="Add Injected Skill"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
        style="--width: 560px;"
      >
        <div class="dialog-form">
          <div class="mode-toggle">
            <sl-button
              size="small"
              variant=${isSearchMode ? 'primary' : 'default'}
              @click=${() => {
                this.dialogMode = 'search';
                this.dialogError = null;
              }}
            >
              <sl-icon slot="prefix" name="search"></sl-icon>
              Skill Bank
            </sl-button>
            <sl-button
              size="small"
              variant=${!isSearchMode ? 'primary' : 'default'}
              @click=${() => {
                this.dialogMode = 'uri';
                this.dialogError = null;
              }}
            >
              <sl-icon slot="prefix" name="link-45deg"></sl-icon>
              External URI
            </sl-button>
          </div>

          ${isSearchMode
            ? html`
                <sl-input
                  label="Search skills"
                  placeholder="Type to search…"
                  clearable
                  .value=${this.dialogSkillQuery}
                  @sl-input=${(e: Event) =>
                    this.handleSearchInput((e.target as HTMLInputElement).value)}
                >
                  ${this.dialogSkillSearching
                    ? html`<sl-spinner slot="suffix" style="font-size: 1rem;"></sl-spinner>`
                    : nothing}
                </sl-input>

                ${this.dialogSkillResults.length > 0
                  ? html`
                      <div class="search-results">
                        ${this.dialogSkillResults.map(
                          (skill) => html`
                            <div
                              class="search-result-item ${this.dialogSelectedSkill?.id === skill.id ? 'selected' : ''}"
                              @click=${() => {
                                this.dialogSelectedSkill = skill;
                              }}
                            >
                              <sl-icon name="puzzle" style="color: var(--scion-primary, #3b82f6);"></sl-icon>
                              <div>
                                <div>${skill.name}</div>
                                <div class="skill-slug">${skill.slug}</div>
                              </div>
                              ${this.dialogSelectedSkill?.id === skill.id
                                ? html`<sl-icon name="check-circle-fill" style="margin-left: auto; color: var(--scion-primary, #3b82f6);"></sl-icon>`
                                : nothing}
                            </div>
                          `
                        )}
                      </div>
                    `
                  : this.dialogSkillQuery && !this.dialogSkillSearching
                    ? html`<div class="no-results">No skills found matching "${this.dialogSkillQuery}"</div>`
                    : nothing}

                ${this.dialogSelectedSkill
                  ? html`
                      <div class="dialog-hint">
                        <sl-icon name="check-circle"></sl-icon>
                        Selected: <strong>${this.dialogSelectedSkill.name}</strong>
                        (${this.dialogSelectedSkill.slug})
                      </div>
                    `
                  : nothing}
              `
            : html`
                <sl-input
                  label="Skill URI"
                  placeholder="e.g. skill://my-skill or gs://bucket/path/skill.yaml"
                  help-text="Skill bank URI (skill://slug), GCS path, or GitHub URL."
                  .value=${this.dialogUri}
                  @sl-input=${(e: Event) => {
                    this.dialogUri = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `}

          <sl-input
            label="Alias (as)"
            placeholder="Optional — override the skill's default name"
            help-text="If set, the skill will be installed under this name instead of its default."
            .value=${this.dialogAs}
            @sl-input=${(e: Event) => {
              this.dialogAs = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <label class="checkbox-label">
            <input
              type="checkbox"
              .checked=${this.dialogOptional}
              @change=${(e: Event) => {
                this.dialogOptional = (e.target as HTMLInputElement).checked;
              }}
            />
            <span class="checkbox-text">
              <span>Optional</span>
              <span class="checkbox-description">
                If checked, a resolution failure for this skill will log a warning but not fail
                agent provisioning.
              </span>
            </span>
          </label>

          ${this.dialogError
            ? html`<div class="dialog-error">${this.dialogError}</div>`
            : nothing}
        </div>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeDialog}
          ?disabled=${this.dialogLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          ?disabled=${this.dialogLoading}
          @click=${this.handleAddSkill}
        >
          Add Skill
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-injected-skills-panel': ScionInjectedSkillsPanel;
  }
}
