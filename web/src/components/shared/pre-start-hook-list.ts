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
 * Pre-Start Hook List Component
 *
 * Self-contained list + CRUD component for pre-start hooks. Pre-start hooks are
 * not file-based resources, so `scion-resource-list` does not apply; this
 * component follows the `scion-env-var-list` / `scion-injected-skills-panel`
 * patterns instead (own loading state, inline expand, inline create/edit form).
 *
 * The same component serves both scopes — only `apiBasePath` differs:
 *   project → /api/v1/projects/{projectId}/pre-start-hooks
 *   hub     → /api/v1/pre-start-hooks
 *
 * Exactly one hook is active per scope. Creating a new hook (or activating an
 * archived one) automatically archives the previous active hook, so there is no
 * standalone "archive" action — the hub API does not expose one.
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PreStartHook, PreStartHookSummary } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

/** Maximum script size accepted by the hub API (64 KB). */
const SCRIPT_MAX_BYTES = 64 * 1024;

/** Default script body pre-filled into the create form. */
const DEFAULT_SCRIPT =
  '#!/bin/sh\nset -eu\n\n# Runs inside the agent container before the harness starts.\n';

/** Mirrors api.Slugify closely enough for client-side preview. */
function slugify(text: string): string {
  return text
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');
}

/** Byte length of a UTF-8 string (script limits are byte-based, not char-based). */
function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

@customElement('scion-pre-start-hook-list')
export class ScionPreStartHookList extends LitElement {
  /**
   * API base path — `/api/v1/projects/{id}` for project scope,
   * `/api/v1` for hub scope.
   */
  @property({ type: String }) apiBasePath = '';

  /** When true, no create/edit/activate/delete affordances are shown. */
  @property({ type: Boolean }) readonly = false;

  /**
   * Hub-level fallback hook shown as an inherited indicator. Only relevant at
   * project scope. The banner is displayed when this is set and the project has
   * no active hook of its own; it disappears once a project hook is activated.
   * The parent page fetches this — the component never calls the hub endpoint.
   */
  @property({ type: Object }) inheritedHook: PreStartHookSummary | null = null;

  @state() private loading = true;
  @state() private hooks: PreStartHook[] = [];
  @state() private error: string | null = null;

  /** Row whose script is expanded inline. */
  @state() private expandedId: string | null = null;

  /** Row with an action (activate/delete) in flight. */
  @state() private busyId: string | null = null;

  // Inline create/edit form state.
  @state() private formOpen = false;
  @state() private formMode: 'create' | 'edit' = 'create';
  @state() private formHookId: string | null = null;
  @state() private formName = '';
  @state() private formSlug = '';
  @state() private formDescription = '';
  @state() private formScript = '';
  @state() private formSaving = false;
  @state() private formError: string | null = null;

  /** True once the user edits the slug by hand — stops name-derived updates. */
  private slugTouched = false;

  /** apiBasePath the current list was loaded from (guards redundant fetches). */
  private loadedPath: string | null = null;

  static override styles = [
    resourceStyles,
    css`
      .list-header {
        display: flex;
        justify-content: flex-end;
        margin-bottom: 1rem;
      }

      .inherited-banner {
        display: flex;
        align-items: flex-start;
        gap: 0.625rem;
        padding: 0.75rem 1rem;
        margin-bottom: 1rem;
        border: 1px dashed var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      .inherited-banner sl-icon {
        flex-shrink: 0;
        margin-top: 0.125rem;
        font-size: 1rem;
      }

      .inherited-banner .inherited-title {
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .inherited-banner .inherited-help {
        margin-top: 0.125rem;
      }

      .hook-name {
        font-weight: 600;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .hook-description {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
        max-width: 22rem;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }

      .badge.status-active {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .badge.status-archived {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        border: 1px solid var(--scion-border, #e2e8f0);
      }

      .script-row td {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .script-preview {
        margin: 0;
        padding: 0.75rem;
        max-height: 20rem;
        overflow: auto;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        line-height: 1.5;
        white-space: pre;
        color: var(--scion-text, #1e293b);
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
      }

      .inline-form {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        background: var(--scion-surface, #ffffff);
        padding: 1.25rem;
        margin-bottom: 1rem;
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .inline-form h3 {
        font-size: 1rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0;
      }

      .form-row {
        display: grid;
        grid-template-columns: 1fr 1fr;
        gap: 1rem;
      }

      .script-field sl-textarea::part(textarea) {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
        line-height: 1.5;
      }

      .script-meta {
        display: flex;
        justify-content: space-between;
        align-items: center;
        gap: 0.5rem;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
      }

      .script-meta.over-limit {
        color: var(--sl-color-danger-600, #dc2626);
        font-weight: 600;
      }

      .form-actions {
        display: flex;
        justify-content: flex-end;
        gap: 0.5rem;
      }

      @media (max-width: 768px) {
        .form-row {
          grid-template-columns: 1fr;
        }
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.apiBasePath) {
      void this.load();
    }
  }

  override willUpdate(changed: PropertyValues): void {
    // The base path is supplied by the parent page and can change (e.g. when the
    // project id resolves after the first render). Reload when it does. Doing
    // this in willUpdate keeps the resulting state change inside the same
    // update cycle instead of scheduling an extra one.
    if (changed.has('apiBasePath') && this.apiBasePath && this.apiBasePath !== this.loadedPath) {
      void this.load();
    }
  }

  /** Collection endpoint for the current scope. */
  private get hooksUrl(): string {
    return `${this.apiBasePath}/pre-start-hooks`;
  }

  /** True when the current scope already has an active hook. */
  private get hasActiveHook(): boolean {
    return this.hooks.some((h) => h.status === 'active');
  }

  /** Reloads the hook list. Public so parent pages can refresh it. */
  async load(): Promise<void> {
    if (!this.apiBasePath) return;
    const path = this.apiBasePath;
    this.loadedPath = path;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(`${path}/pre-start-hooks`);
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }
      const data = (await response.json()) as { hooks?: PreStartHook[] } | PreStartHook[];
      const hooks = Array.isArray(data) ? data : data.hooks || [];
      // Ignore a response for a base path we have since navigated away from.
      if (this.loadedPath !== path) return;
      this.hooks = hooks;
    } catch (err) {
      console.error('Failed to load pre-start hooks:', err);
      if (this.loadedPath !== path) return;
      this.error = err instanceof Error ? err.message : 'Failed to load pre-start hooks';
    } finally {
      if (this.loadedPath === path) {
        this.loading = false;
      }
    }
  }

  // ── Form handling ────────────────────────────────────────────────────

  private openCreateForm(): void {
    this.formMode = 'create';
    this.formHookId = null;
    this.formName = '';
    this.formSlug = '';
    this.formDescription = '';
    this.formScript = DEFAULT_SCRIPT;
    this.formError = null;
    this.slugTouched = false;
    this.formOpen = true;
  }

  private openEditForm(hook: PreStartHook): void {
    this.formMode = 'edit';
    this.formHookId = hook.id;
    this.formName = hook.name;
    this.formSlug = hook.slug;
    this.formDescription = hook.description || '';
    this.formScript = hook.script || '';
    this.formError = null;
    this.slugTouched = true;
    this.formOpen = true;
  }

  private closeForm(): void {
    this.formOpen = false;
    this.formError = null;
  }

  private onNameInput(e: Event): void {
    this.formName = (e.target as HTMLInputElement).value;
    // The slug is derived from the name until the user edits it directly.
    if (this.formMode === 'create' && !this.slugTouched) {
      this.formSlug = slugify(this.formName);
    }
  }

  private onSlugInput(e: Event): void {
    this.slugTouched = true;
    this.formSlug = (e.target as HTMLInputElement).value;
  }

  private async handleSave(e?: Event): Promise<void> {
    e?.preventDefault();

    const name = this.formName.trim();
    if (!name) {
      this.formError = 'Name is required';
      return;
    }
    if (!this.formScript.trim()) {
      this.formError = 'Script is required';
      return;
    }
    // Mirror the hub-side 64 KB limit so the user gets immediate feedback.
    if (byteLength(this.formScript) > SCRIPT_MAX_BYTES) {
      this.formError = 'Script exceeds the 64 KB size limit';
      return;
    }

    this.formSaving = true;
    this.formError = null;

    try {
      const isCreate = this.formMode === 'create';
      const url = isCreate
        ? this.hooksUrl
        : `${this.hooksUrl}/${encodeURIComponent(this.formHookId!)}`;
      // Slug is immutable after creation — the update endpoint ignores it.
      const body = isCreate
        ? {
            name,
            slug: this.formSlug.trim() || undefined,
            description: this.formDescription.trim() || undefined,
            script: this.formScript,
          }
        : {
            name,
            description: this.formDescription.trim(),
            script: this.formScript,
          };

      const response = await apiFetch(url, {
        method: isCreate ? 'POST' : 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      this.closeForm();
      await this.load();
    } catch (err) {
      console.error('Failed to save pre-start hook:', err);
      this.formError = err instanceof Error ? err.message : 'Failed to save pre-start hook';
    } finally {
      this.formSaving = false;
    }
  }

  // ── Row actions ──────────────────────────────────────────────────────

  private async handleActivate(hook: PreStartHook): Promise<void> {
    this.busyId = hook.id;
    try {
      const response = await apiFetch(`${this.hooksUrl}/${encodeURIComponent(hook.id)}/activate`, {
        method: 'POST',
      });
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `Failed to activate (HTTP ${response.status})`)
        );
      }
      await this.load();
    } catch (err) {
      console.error('Failed to activate pre-start hook:', err);
      alert(err instanceof Error ? err.message : 'Failed to activate pre-start hook');
    } finally {
      this.busyId = null;
    }
  }

  private async handleDelete(hook: PreStartHook): Promise<void> {
    // Deletion is not retroactive: the script is baked into each agent's
    // applied configuration at creation time, so existing agents keep running
    // it on every restart until they are recreated.
    const message =
      `Delete pre-start hook "${hook.name}"? This cannot be undone. ` +
      'Existing agents that inherited this hook will continue to run it until recreated.';
    if (!confirm(message)) {
      return;
    }
    this.busyId = hook.id;
    try {
      const response = await apiFetch(`${this.hooksUrl}/${encodeURIComponent(hook.id)}`, {
        method: 'DELETE',
      });
      if (!response.ok && response.status !== 204) {
        throw new Error(
          await extractApiError(response, `Failed to delete (HTTP ${response.status})`)
        );
      }
      if (this.expandedId === hook.id) this.expandedId = null;
      await this.load();
    } catch (err) {
      console.error('Failed to delete pre-start hook:', err);
      alert(err instanceof Error ? err.message : 'Failed to delete pre-start hook');
    } finally {
      this.busyId = null;
    }
  }

  /**
   * A hook can be deleted when it is archived, or when it is the only hook in
   * the scope (the hub rejects deleting an active hook while others exist).
   */
  private canDelete(hook: PreStartHook): boolean {
    return hook.status !== 'active' || this.hooks.length === 1;
  }

  private formatDate(value: string | undefined): string {
    if (!value) return '—';
    const date = new Date(value);
    if (isNaN(date.getTime())) return value;
    return date.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  // ── Rendering ────────────────────────────────────────────────────────

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading pre-start hooks...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading pre-start hooks.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.load()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      ${this.renderInheritedBanner()}
      ${!this.readonly && !this.formOpen
        ? html`
            <div class="list-header">
              <sl-button variant="primary" size="small" @click=${this.openCreateForm}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create Hook
              </sl-button>
            </div>
          `
        : nothing}
      ${this.formOpen ? this.renderForm() : nothing}
      ${this.hooks.length === 0 ? this.renderEmpty() : this.renderTable()}
    `;
  }

  private renderInheritedBanner() {
    if (!this.inheritedHook || this.hasActiveHook) return nothing;
    return html`
      <div class="inherited-banner">
        <sl-icon name="diagram-3"></sl-icon>
        <div>
          <div class="inherited-title">Inherited from hub: ${this.inheritedHook.name}</div>
          <div class="inherited-help">
            This hub-default script runs before agents in this project start, because no project
            hook is active. Activating a project hook overrides it.
          </div>
        </div>
      </div>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="terminal"></sl-icon>
        <h3>No Pre-Start Hooks</h3>
        <p>
          Pre-start hooks are shell scripts staged into the agent container and run before the
          harness starts.
        </p>
        ${!this.readonly && !this.formOpen
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openCreateForm}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create Hook
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Slug</th>
              <th>Status</th>
              <th class="hide-mobile">Created</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.hooks.map((hook) => this.renderRow(hook))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(hook: PreStartHook) {
    const busy = this.busyId === hook.id;
    const expanded = this.expandedId === hook.id;
    const isActive = hook.status === 'active';

    return html`
      <tr>
        <td>
          <div class="hook-name">${hook.name}</div>
          ${hook.description
            ? html`<div class="hook-description">${hook.description}</div>`
            : nothing}
        </td>
        <td class="key-cell">${hook.slug}</td>
        <td>
          <span class="badge ${isActive ? 'status-active' : 'status-archived'}">
            ${isActive ? 'active' : hook.status || 'archived'}
          </span>
        </td>
        <td class="hide-mobile"><span class="meta-text">${this.formatDate(hook.created)}</span></td>
        <td class="actions-cell">
          <sl-icon-button
            name=${expanded ? 'chevron-up' : 'code-slash'}
            label=${expanded ? 'Hide script' : 'View script'}
            @click=${() => {
              this.expandedId = expanded ? null : hook.id;
            }}
          ></sl-icon-button>
          ${this.readonly
            ? nothing
            : html`
                <sl-icon-button
                  name="pencil"
                  label="Edit"
                  ?disabled=${busy}
                  @click=${() => this.openEditForm(hook)}
                ></sl-icon-button>
                ${isActive
                  ? nothing
                  : html`
                      <sl-icon-button
                        name="play-circle"
                        label="Activate"
                        ?disabled=${busy}
                        @click=${() => this.handleActivate(hook)}
                      ></sl-icon-button>
                    `}
                ${this.canDelete(hook)
                  ? html`
                      <sl-icon-button
                        name="trash"
                        label="Delete"
                        ?disabled=${busy}
                        @click=${() => this.handleDelete(hook)}
                      ></sl-icon-button>
                    `
                  : nothing}
              `}
        </td>
      </tr>
      ${expanded
        ? html`
            <tr class="script-row">
              <td colspan="5"><pre class="script-preview">${hook.script || ''}</pre></td>
            </tr>
          `
        : nothing}
    `;
  }

  private renderForm() {
    const isCreate = this.formMode === 'create';
    const scriptBytes = byteLength(this.formScript);
    const overLimit = scriptBytes > SCRIPT_MAX_BYTES;

    return html`
      <form class="inline-form" @submit=${this.handleSave}>
        <h3>${isCreate ? 'Create Pre-Start Hook' : 'Edit Pre-Start Hook'}</h3>

        <div class="form-row">
          <sl-input
            label="Name"
            placeholder="e.g. Install build tools"
            value=${this.formName}
            @sl-input=${this.onNameInput}
            required
          ></sl-input>
          <sl-input
            label="Slug"
            placeholder="auto-derived from name"
            value=${this.formSlug}
            ?disabled=${!isCreate}
            help-text=${isCreate
              ? 'Auto-derived from the name; edit to override.'
              : 'Slug cannot be changed.'}
            @sl-input=${this.onSlugInput}
          ></sl-input>
        </div>

        <sl-input
          label="Description"
          placeholder="Optional description"
          value=${this.formDescription}
          @sl-input=${(e: Event) => {
            this.formDescription = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>

        <div class="script-field">
          <sl-textarea
            label="Script"
            rows="10"
            resize="vertical"
            spellcheck="false"
            value=${this.formScript}
            @sl-input=${(e: Event) => {
              this.formScript = (e.target as HTMLTextAreaElement).value;
            }}
            required
          ></sl-textarea>
          <div class="script-meta ${overLimit ? 'over-limit' : ''}">
            <span>Runs inside the agent container before the harness starts.</span>
            <span
              >${scriptBytes.toLocaleString()} / ${SCRIPT_MAX_BYTES.toLocaleString()} bytes</span
            >
          </div>
        </div>

        ${isCreate
          ? html`
              <div class="dialog-hint">
                <sl-icon name="info-circle"></sl-icon>
                <span>Creating a hook makes it active and archives the current active hook.</span>
              </div>
            `
          : nothing}
        ${this.formError ? html`<div class="dialog-error">${this.formError}</div>` : nothing}

        <div class="form-actions">
          <sl-button variant="default" ?disabled=${this.formSaving} @click=${this.closeForm}>
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            ?loading=${this.formSaving}
            ?disabled=${this.formSaving || overLimit}
            @click=${this.handleSave}
          >
            ${isCreate ? 'Create' : 'Save'}
          </sl-button>
        </div>
      </form>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-pre-start-hook-list': ScionPreStartHookList;
  }
}
