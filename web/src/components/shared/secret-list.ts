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
 * Shared Secret List Component
 *
 * Full CRUD component for secrets. Used by both the profile secrets page
 * (scope=user) and the project configuration page (scope=project).
 *
 * In non-compact mode (profile page), renders a table with an add button.
 * In compact mode (project page), wraps in a section with header/description.
 *
 * In user scope the list also carries the user's pending secrets (secrets an
 * agent saved, waiting for the owner's approval) with a state badge and
 * Approve, Reject and Reveal actions. A pending value is shown only after
 * an explicit Reveal and is dropped when the reveal dialog closes.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { Secret, SecretType, ResourceScope, InjectionMode } from '../../shared/types.js';
import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import {
  PENDING_EXISTING_CHANGED_CODE,
  PENDING_SECRETS_API,
  decodeBase64Text,
  dispatchPendingSecretsChanged,
  entryStateLabel,
  pendingCount,
  pendingEffectText,
  pendingOverridesText,
  pendingReplacesText,
  pendingWasReplaced,
  pendingWriterText,
  secretEntries,
  valueLengthText,
  type PendingSecret,
  type SecretEntry,
  type SecretListResponse,
} from '../../client/pending-secrets.js';
import { resourceStyles } from './resource-styles.js';
import { showToast } from '../../utils/toast.js';
import { copyToClipboard, showCopyFailedToast } from '../../utils/clipboard.js';
import { showConfirm } from './confirm-dialog.js';
import { formatInstant, formatRelative } from '../../utils/time.js';

/** What the user sees when a pending secret expired or was already decided. */
const PENDING_GONE_MESSAGE =
  'This pending secret has expired or was already approved or rejected. The list was refreshed.';

/** What the user sees when an approval is refused because the existing secret changed. */
const PENDING_EXISTING_CHANGED_MESSAGE =
  'Your existing secret changed since the agent saved this one. ' +
  'Reject this pending secret and have the agent save it again.';

@customElement('scion-secret-list')
export class ScionSecretList extends LitElement {
  @property() scope: ResourceScope = 'user';
  @property() scopeId = '';
  @property() apiBasePath = '/api/v1';
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private entries: SecretEntry[] = [];
  @state() private error: string | null = null;

  // Create/Update dialog
  @state() private dialogOpen = false;
  @state() private dialogMode: 'create' | 'update' | 'edit-settings' = 'create';
  @state() private dialogKey = '';
  @state() private dialogValue = '';
  @state() private dialogDescription = '';
  @state() private dialogType: SecretType = 'environment';
  @state() private dialogTarget = '';
  @state() private dialogInjectionMode: InjectionMode = 'as_needed';
  @state() private dialogAllowProgeny = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Delete
  @state() private deletingKey: string | null = null;

  // Copy-to-clipboard feedback
  @state() private copiedSecretKey: string | null = null;

  // Pending secrets: the id of the one whose request is in flight.
  @state() private pendingBusyId: string | null = null;

  // Approve dialog
  @state() private approveEntry: SecretEntry | null = null;
  @state() private approveAllowProgeny = false;
  @state() private approveError: string | null = null;

  // Reveal dialog. The value lives only here, only while the dialog is
  // open; closing the dialog clears it.
  @state() private revealPending: PendingSecret | null = null;
  @state() private revealText: string | null = null;
  @state() private revealIsBase64 = false;
  @state() private revealError: string | null = null;

  static override styles = [
    resourceStyles,
    css`
      sl-textarea.secret-value::part(textarea) {
        -webkit-text-security: disc;
      }

      .state-badge {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.75rem;
        font-weight: 500;
        white-space: nowrap;
      }

      .state-badge.approved {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .state-badge.pending {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #a16207);
      }

      .state-expiry {
        display: block;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
      }

      tr.pending-row td {
        border-bottom: none;
      }

      tr.pending-detail-row td {
        padding-top: 0;
      }

      .pending-details {
        margin: 0;
        padding: 0.5rem 0.75rem;
        list-style: none;
        font-size: 0.8125rem;
        color: var(--scion-text, #1e293b);
        background: var(--scion-bg-subtle, #f8fafc);
        border-radius: var(--scion-radius, 0.5rem);
        display: flex;
        flex-direction: column;
        gap: 0.25rem;
        overflow-wrap: anywhere;
      }

      .pending-details .muted {
        color: var(--scion-text-muted, #64748b);
      }

      .pending-actions {
        display: inline-flex;
        gap: 0.25rem;
        flex-wrap: wrap;
        justify-content: flex-end;
      }

      .reveal-value {
        margin: 0;
        padding: 0.75rem;
        max-height: 16rem;
        overflow: auto;
        white-space: pre-wrap;
        overflow-wrap: anywhere;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSecrets();
  }

  private async loadSecrets(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const url =
        this.scope === 'user'
          ? `${this.apiBasePath}/secrets?scope=user&includePending=true`
          : this.scope !== 'project'
            ? `${this.apiBasePath}/secrets?scope=${this.scope}`
            : `${this.apiBasePath}/secrets`;
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      const data = (await response.json()) as SecretListResponse | Secret[];
      this.entries = secretEntries(data);
      if (this.scope === 'user') dispatchPendingSecretsChanged(pendingCount(this.entries));
    } catch (err) {
      console.error('Failed to load secrets:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load secrets';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogMode = 'create';
    this.dialogKey = '';
    this.dialogValue = '';
    this.dialogDescription = '';
    this.dialogType = 'environment';
    this.dialogTarget = '';
    this.dialogInjectionMode = 'as_needed';
    this.dialogAllowProgeny = false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private openUpdateDialog(secret: Secret): void {
    this.dialogMode = 'update';
    this.dialogKey = secret.key;
    this.dialogValue = '';
    this.dialogDescription = secret.description || '';
    this.dialogType = secret.type;
    this.dialogTarget = secret.target || '';
    this.dialogInjectionMode = secret.injectionMode || 'as_needed';
    this.dialogAllowProgeny = secret.allowProgeny || false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private openEditSettingsDialog(secret: Secret): void {
    this.dialogMode = 'edit-settings';
    this.dialogKey = secret.key;
    this.dialogValue = '';
    this.dialogDescription = secret.description || '';
    this.dialogType = secret.type;
    this.dialogTarget = secret.target || '';
    this.dialogInjectionMode = secret.injectionMode || 'as_needed';
    this.dialogAllowProgeny = secret.allowProgeny || false;
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
  }

  private async handleSave(e: Event): Promise<void> {
    e.preventDefault();

    const key = this.dialogKey.trim();
    if (!key) {
      this.dialogError = 'Key is required';
      return;
    }

    const isEditSettings = this.dialogMode === 'edit-settings';

    if (!isEditSettings && !this.dialogValue) {
      this.dialogError = 'Value is required';
      return;
    }

    this.dialogLoading = true;
    this.dialogError = null;

    try {
      let body: Record<string, unknown>;
      let method: string;

      if (isEditSettings) {
        // PATCH: metadata-only update, no value
        body = {
          description: this.dialogDescription,
          type: this.dialogType,
          target: this.dialogTarget || undefined,
          injectionMode: this.dialogInjectionMode,
          allowProgeny: this.scope === 'user' ? this.dialogAllowProgeny : undefined,
        };
        method = 'PATCH';
      } else {
        // PUT: full update including value
        body = {
          value: btoa(
            Array.from(new TextEncoder().encode(this.dialogValue), (b) =>
              String.fromCharCode(b)
            ).join('')
          ),
          scope: this.scope,
          description: this.dialogDescription || undefined,
          type: this.dialogType,
          target: this.dialogTarget || undefined,
          injectionMode: this.dialogInjectionMode,
          allowProgeny: this.scope === 'user' ? this.dialogAllowProgeny : undefined,
        };
        method = 'PUT';
      }

      if (this.scope === 'project') {
        body.scopeId = this.scopeId;
      }

      const url =
        isEditSettings && this.scope !== 'project'
          ? `${this.apiBasePath}/secrets/${encodeURIComponent(key)}?scope=${this.scope}`
          : `${this.apiBasePath}/secrets/${encodeURIComponent(key)}`;

      const response = await apiFetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      this.closeDialog();
      await this.loadSecrets();
    } catch (err) {
      console.error('Failed to save secret:', err);
      this.dialogError = err instanceof Error ? err.message : 'Failed to save';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleDelete(secret: Secret, event?: MouseEvent): Promise<void> {
    if (
      !event?.altKey &&
      !(await showConfirm(`Delete secret "${secret.key}"? This cannot be undone.`))
    ) {
      return;
    }

    this.deletingKey = secret.key;

    try {
      const deleteUrl =
        this.scope !== 'project'
          ? `${this.apiBasePath}/secrets/${encodeURIComponent(secret.key)}?scope=${this.scope}`
          : `${this.apiBasePath}/secrets/${encodeURIComponent(secret.key)}`;
      const response = await apiFetch(deleteUrl, { method: 'DELETE' });

      if (!response.ok && response.status !== 204) {
        throw new Error(
          await extractApiError(response, `Failed to delete (HTTP ${response.status})`)
        );
      }

      await this.loadSecrets();
    } catch (err) {
      console.error('Failed to delete secret:', err);
      showToast(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.deletingKey = null;
    }
  }

  private async copySecretRef(secret: Secret): Promise<void> {
    if (!secret.secretRef) return;
    if (!(await copyToClipboard(secret.secretRef))) {
      showCopyFailedToast();
      return;
    }
    this.copiedSecretKey = secret.key;
    setTimeout(() => {
      this.copiedSecretKey = null;
    }, 1500);
  }

  // ── Pending secrets ──────────────────────────────────────────────────

  private pendingUrl(p: PendingSecret, action: 'approve' | 'reject' | 'reveal'): string {
    return `${PENDING_SECRETS_API}/${encodeURIComponent(p.id)}/${action}`;
  }

  /** A pending secret that is gone (404): tell the user and refresh. */
  private async handlePendingGone(): Promise<void> {
    showToast(PENDING_GONE_MESSAGE, 'warning');
    await this.loadSecrets();
  }

  private openApproveDialog(entry: SecretEntry): void {
    if (!entry.pending) return;
    this.approveEntry = entry;
    this.approveAllowProgeny = entry.pending.allowProgeny;
    this.approveError = null;
  }

  private closeApproveDialog(): void {
    this.approveEntry = null;
    this.approveError = null;
  }

  private async handleApprove(): Promise<void> {
    const pending = this.approveEntry?.pending;
    if (!pending) return;
    this.pendingBusyId = pending.id;
    this.approveError = null;
    // The owner may only turn progeny access off, never on.
    const body = pending.allowProgeny && !this.approveAllowProgeny ? { allowProgeny: false } : {};
    try {
      const res = await apiFetch(this.pendingUrl(pending, 'approve'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
        suppressAccessDeniedToast: true,
      });
      if (res.status === 404) {
        this.closeApproveDialog();
        await this.handlePendingGone();
        return;
      }
      if (!res.ok) {
        const info = await parseApiError(res, `Failed to approve (HTTP ${res.status})`);
        this.approveError =
          info.code === PENDING_EXISTING_CHANGED_CODE
            ? PENDING_EXISTING_CHANGED_MESSAGE
            : info.message;
        return;
      }
      this.closeApproveDialog();
      showToast(`Secret "${pending.key}" approved`, 'success');
      await this.loadSecrets();
    } catch (err) {
      this.approveError = err instanceof Error ? err.message : 'Failed to approve';
    } finally {
      this.pendingBusyId = null;
    }
  }

  private async handleReject(pending: PendingSecret, event?: MouseEvent): Promise<void> {
    if (
      !event?.altKey &&
      !(await showConfirm(
        `Reject pending secret "${pending.key}"? The value the agent saved is discarded.`
      ))
    ) {
      return;
    }
    this.pendingBusyId = pending.id;
    try {
      const res = await apiFetch(this.pendingUrl(pending, 'reject'), { method: 'POST' });
      if (res.status === 404) {
        await this.handlePendingGone();
        return;
      }
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to reject (HTTP ${res.status})`));
      }
      showToast(`Pending secret "${pending.key}" rejected`, 'success');
      await this.loadSecrets();
    } catch (err) {
      showToast(err instanceof Error ? err.message : 'Failed to reject');
    } finally {
      this.pendingBusyId = null;
    }
  }

  /**
   * Reveals a pending value after the user clicked Reveal. The value is
   * held in component state only while the dialog is open and is never
   * logged or stored.
   */
  private async handleReveal(pending: PendingSecret): Promise<void> {
    this.clearReveal();
    this.revealPending = pending;
    this.pendingBusyId = pending.id;
    try {
      const res = await apiFetch(this.pendingUrl(pending, 'reveal'), { method: 'POST' });
      if (res.status === 404) {
        this.closeReveal();
        await this.handlePendingGone();
        return;
      }
      if (!res.ok) {
        this.revealError = await extractApiError(res, `Failed to reveal (HTTP ${res.status})`);
        return;
      }
      const data = (await res.json()) as { value?: unknown };
      // The dialog may have closed while the request ran.
      if (this.revealPending?.id !== pending.id) return;
      const b64 = typeof data.value === 'string' ? data.value : '';
      const text = decodeBase64Text(b64);
      this.revealIsBase64 = text === null;
      this.revealText = text ?? b64;
    } catch {
      this.revealError = 'Failed to reveal';
    } finally {
      this.pendingBusyId = null;
    }
  }

  private clearReveal(): void {
    this.revealText = null;
    this.revealIsBase64 = false;
    this.revealError = null;
  }

  private closeReveal(): void {
    this.clearReveal();
    this.revealPending = null;
  }

  // ── Rendering ────────────────────────────────────────────────────────

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderFull();
  }

  private renderFull() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading secrets...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading your secrets.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadSecrets()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Secret
        </sl-button>
      </div>
      ${this.entries.length === 0 ? this.renderEmpty() : this.renderTable()} ${this.renderDialog()}
      ${this.renderApproveDialog()} ${this.renderRevealDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Secrets</h2>
            <p>
              Manage encrypted secrets for
              ${this.scope === 'hub' ? 'all agents on this hub' : 'agents in this project'}. Values
              are write-only and cannot be retrieved after saving.
            </p>
          </div>
          <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Secret
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading secrets...</div>`
          : this.error
            ? html`<div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.loadSecrets()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>`
            : this.entries.length === 0
              ? this.renderEmpty()
              : this.renderTable()}
      </div>
      ${this.renderDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Key</th>
              ${this.scope === 'user' ? html`<th>State</th>` : nothing}
              <th>Type</th>
              <th>Inject</th>
              ${this.scope === 'user' ? html`<th>Progeny</th>` : nothing}
              <th class="hide-mobile">Description</th>
              <th>Version</th>
              <th class="hide-mobile">Updated</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.entries.map((entry) => this.renderEntry(entry))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderEntry(entry: SecretEntry): TemplateResult {
    const columns = this.scope === 'user' ? 9 : 7;
    return html`
      ${this.renderRow(entry)}
      ${entry.pending
        ? html`<tr class="pending-detail-row">
            <td colspan=${columns}>${this.renderPendingDetails(entry)}</td>
          </tr>`
        : nothing}
    `;
  }

  private renderRow(entry: SecretEntry) {
    const secret = entry.approved;
    const pending = entry.pending;
    const isDeleting = this.deletingKey === entry.key;
    const type = secret?.type ?? pending?.type ?? 'environment';
    const injectionMode = secret ? secret.injectionMode : pending?.injectionMode;
    const allowProgeny = secret ? secret.allowProgeny : pending?.allowProgeny;
    const description = secret ? secret.description : pending?.description;

    return html`
      <tr class=${pending ? 'pending-row' : ''}>
        <td class="key-cell">
          <div class="key-info">
            <div class="key-icon">
              <sl-icon name="shield-lock"></sl-icon>
            </div>
            ${entry.key}
          </div>
        </td>
        ${this.scope === 'user' ? html`<td>${this.renderState(entry)}</td>` : nothing}
        <td>
          <span class="type-badge ${type}">${type}</span>
        </td>
        <td>
          ${injectionMode === 'as_needed'
            ? html`<span class="badge inject-as-needed">as needed</span>`
            : html`<span class="badge inject-always">always</span>`}
        </td>
        ${this.scope === 'user'
          ? html`<td>
              ${allowProgeny
                ? html`<sl-icon name="check-lg" title="Progeny can access"></sl-icon>`
                : '—'}
            </td>`
          : nothing}
        <td class="description-cell hide-mobile">${description || '—'}</td>
        <td>${secret ? this.renderVersion(secret) : '—'}</td>
        <td class="hide-mobile">
          <span class="meta-text"
            >${formatRelative(secret ? secret.updated : (pending?.updated ?? ''))}</span
          >
        </td>
        <td class="actions-cell">
          ${pending ? this.renderPendingActions(pending) : nothing}
          ${secret
            ? html`
                <sl-icon-button
                  name="pencil"
                  label="Edit settings"
                  ?disabled=${isDeleting}
                  @click=${() => this.openEditSettingsDialog(secret)}
                ></sl-icon-button>
                <sl-icon-button
                  name="arrow-clockwise"
                  label="Update value"
                  ?disabled=${isDeleting}
                  @click=${() => this.openUpdateDialog(secret)}
                ></sl-icon-button>
                <sl-icon-button
                  name="trash"
                  label="Delete"
                  ?disabled=${isDeleting}
                  @click=${(e: MouseEvent) => this.handleDelete(secret, e)}
                ></sl-icon-button>
              `
            : nothing}
        </td>
      </tr>
    `;
  }

  private renderVersion(secret: Secret): TemplateResult {
    return secret.secretRef
      ? html`<sl-tooltip
          content=${this.copiedSecretKey === secret.key ? 'Copied!' : secret.secretRef}
          hoist
        >
          <span
            class="version-badge version-badge-copyable"
            @click=${() => this.copySecretRef(secret)}
            title=""
            >v${secret.version}</span
          >
        </sl-tooltip>`
      : html`<span class="version-badge">v${secret.version}</span>`;
  }

  private renderState(entry: SecretEntry): TemplateResult {
    const label = entryStateLabel(entry);
    if (!entry.pending) {
      return html`<span class="state-badge approved">${label}</span>`;
    }
    return html`
      <span class="state-badge pending">${label}</span>
      <span class="state-expiry">expires ${formatInstant(entry.pending.expiresAt)}</span>
    `;
  }

  private renderPendingActions(pending: PendingSecret): TemplateResult {
    const busy = this.pendingBusyId !== null;
    const entry = this.entries.find((e) => e.pending?.id === pending.id);
    return html`
      <span class="pending-actions">
        <sl-button
          size="small"
          variant="primary"
          ?disabled=${busy}
          @click=${(): void => {
            if (entry) this.openApproveDialog(entry);
          }}
        >
          Approve
        </sl-button>
        <sl-button
          size="small"
          variant="default"
          ?disabled=${busy}
          @click=${(e: MouseEvent): void => void this.handleReject(pending, e)}
        >
          Reject
        </sl-button>
        <sl-button
          size="small"
          variant="text"
          ?disabled=${busy}
          @click=${(): void => void this.handleReveal(pending)}
        >
          Reveal
        </sl-button>
      </span>
    `;
  }

  /**
   * What the user sees about a pending secret: never its value. With
   * `allowProgeny` the effect line follows the approve dialog's switch.
   */
  private renderPendingDetails(
    entry: SecretEntry,
    allowProgeny?: boolean
  ): TemplateResult | typeof nothing {
    const p = entry.pending;
    if (!p) return nothing;
    const overrides = pendingOverridesText(p);
    const replaces = pendingReplacesText(p, entry.approved, (iso) => formatInstant(iso));
    return html`
      <ul class="pending-details">
        <li>
          <span class="muted">Name:</span> ${p.key} &middot;
          <span class="muted">Type:</span> ${p.type}
          ${p.target ? html`&middot; <span class="muted">Target:</span> ${p.target}` : nothing}
          &middot; <span class="muted">Value length:</span> ${valueLengthText(p.valueLen)}
        </li>
        <li>${pendingWriterText(p)}</li>
        <li>
          Written at
          ${formatInstant(p.created)}${pendingWasReplaced(p)
            ? html`, replaced at ${formatInstant(p.updated)}`
            : nothing},
          expires at ${formatInstant(p.expiresAt)}
        </li>
        <li>${pendingEffectText(p, allowProgeny ?? p.allowProgeny)}</li>
        ${overrides ? html`<li>${overrides}</li>` : nothing}
        ${replaces ? html`<li>${replaces}</li>` : nothing}
      </ul>
    `;
  }

  private renderApproveDialog(): TemplateResult {
    const entry = this.approveEntry;
    const p = entry?.pending;
    const busy = !!p && this.pendingBusyId === p.id;
    return html`
      <sl-dialog
        class="approve-dialog"
        label=${p ? `Approve secret "${p.key}"` : 'Approve secret'}
        ?open=${!!p}
        @sl-request-close=${this.closeApproveDialog}
      >
        ${entry && p
          ? html`
              ${this.renderPendingDetails(entry, this.approveAllowProgeny)}
              ${p.allowProgeny
                ? html`
                    <sl-checkbox
                      class="approve-progeny"
                      .checked=${this.approveAllowProgeny}
                      @sl-change=${(e: Event): void => {
                        // Untick only: approval can turn progeny access off,
                        // never on, so the box is disabled once unticked.
                        this.approveAllowProgeny =
                          this.approveAllowProgeny && (e.target as HTMLInputElement).checked;
                      }}
                      ?disabled=${!this.approveAllowProgeny}
                    >
                      Also deliver to agents your agents create
                    </sl-checkbox>
                  `
                : nothing}
              ${this.approveError
                ? html`<div class="dialog-error">${this.approveError}</div>`
                : nothing}
            `
          : nothing}
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${busy}
          @click=${this.closeApproveDialog}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${busy}
          ?disabled=${busy}
          @click=${this.handleApprove}
        >
          Approve
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderRevealDialog(): TemplateResult {
    const p = this.revealPending;
    const loading = !!p && this.pendingBusyId === p.id;
    return html`
      <sl-dialog
        class="reveal-dialog"
        label=${p ? `Value of pending secret "${p.key}"` : 'Pending secret value'}
        ?open=${!!p}
        @sl-request-close=${this.closeReveal}
      >
        ${loading ? html`<sl-spinner></sl-spinner>` : nothing}
        ${this.revealError ? html`<div class="dialog-error">${this.revealError}</div>` : nothing}
        ${this.revealText !== null
          ? html`
              ${this.revealIsBase64
                ? html`<div class="dialog-hint">
                    <sl-icon name="info-circle"></sl-icon>
                    The value is not text; it is shown base64-encoded.
                  </div>`
                : nothing}
              <pre class="reveal-value">${this.revealText}</pre>
            `
          : nothing}
        <sl-button slot="footer" variant="primary" @click=${this.closeReveal}>Close</sl-button>
      </sl-dialog>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="shield-lock"></sl-icon>
        <h3>No Secrets</h3>
        <p>
          Add encrypted secrets that will be securely injected into
          ${this.compact
            ? this.scope === 'hub'
              ? 'all agents on this hub'
              : 'agents in this project'
            : 'your agents'}.
        </p>
        <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Secret
        </sl-button>
      </div>
    `;
  }

  private renderDialog() {
    const isCreate = this.dialogMode === 'create';
    const isEditSettings = this.dialogMode === 'edit-settings';
    const title = isEditSettings
      ? 'Edit Secret Settings'
      : isCreate
        ? 'Add Secret'
        : 'Update Secret';

    return html`
      <sl-dialog label=${title} ?open=${this.dialogOpen} @sl-request-close=${this.closeDialog}>
        <form class="dialog-form" @submit=${this.handleSave}>
          <sl-input
            label=${this.dialogType === 'file' ? 'Name' : 'Key'}
            placeholder=${this.dialogType === 'file' ? 'e.g. ssh_deploy_key' : 'e.g. GITHUB_TOKEN'}
            value=${this.dialogKey}
            ?disabled=${!isCreate}
            @sl-input=${(e: Event) => {
              this.dialogKey = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          ${isEditSettings
            ? nothing
            : this.dialogType === 'file'
              ? html`
                  <sl-textarea
                    class="secret-value"
                    label="Value"
                    placeholder="Paste file contents (e.g. private key)"
                    value=${this.dialogValue}
                    rows="6"
                    resize="vertical"
                    @sl-input=${(e: Event) => {
                      this.dialogValue = (e.target as HTMLTextAreaElement).value;
                    }}
                    required
                  ></sl-textarea>
                `
              : html`
                  <sl-input
                    label="Value"
                    placeholder="Secret value"
                    value=${this.dialogValue}
                    type="password"
                    @sl-input=${(e: Event) => {
                      this.dialogValue = (e.target as HTMLInputElement).value;
                    }}
                    required
                  ></sl-input>
                `}
          ${isEditSettings
            ? nothing
            : html`<div class="dialog-hint">
                <sl-icon name="info-circle"></sl-icon>
                Secret values are encrypted and can never be retrieved after saving.
              </div>`}

          <sl-select
            label="Type"
            value=${this.dialogType}
            @sl-change=${(e: Event) => {
              this.dialogType = (e.target as HTMLSelectElement).value as SecretType;
            }}
          >
            <sl-option value="environment">Environment Variable</sl-option>
            <sl-option value="variable">Runtime Variable</sl-option>
            <sl-option value="file">File</sl-option>
          </sl-select>

          ${this.dialogType === 'file'
            ? html`
                <sl-input
                  label="Target Path"
                  placeholder="e.g. /home/agent/.ssh/id_rsa"
                  value=${this.dialogTarget}
                  @sl-input=${(e: Event) => {
                    this.dialogTarget = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              `
            : nothing}

          <sl-textarea
            label="Description"
            placeholder="Optional description"
            value=${this.dialogDescription}
            rows="2"
            resize="none"
            @sl-input=${(e: Event) => {
              this.dialogDescription = (e.target as HTMLTextAreaElement).value;
            }}
          ></sl-textarea>

          <div class="radio-field">
            <span class="radio-field-label">Inject</span>
            <sl-radio-group
              value=${this.dialogInjectionMode}
              @sl-change=${(e: Event) => {
                this.dialogInjectionMode = (e.target as HTMLInputElement).value as InjectionMode;
              }}
            >
              <sl-radio-button value="always">Always</sl-radio-button>
              <sl-radio-button value="as_needed">As needed</sl-radio-button>
            </sl-radio-group>
            <span class="radio-field-help">
              "As needed" injects only when the agent configuration requests this value.
            </span>
          </div>

          ${this.scope === 'user'
            ? html`
                <sl-switch
                  ?checked=${this.dialogAllowProgeny}
                  @sl-change=${(e: Event) => {
                    this.dialogAllowProgeny = (e.target as HTMLInputElement).checked;
                  }}
                >
                  Allow agent progeny to access
                </sl-switch>
                <span class="radio-field-help">
                  When enabled, agents spawned by your agents (and their descendants) will also
                  receive this secret.
                </span>
              `
            : nothing}
          ${this.dialogError ? html`<div class="dialog-error">${this.dialogError}</div>` : nothing}
        </form>

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
          @click=${this.handleSave}
        >
          ${isCreate ? 'Create' : isEditSettings ? 'Save' : 'Update'}
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-secret-list': ScionSecretList;
  }
}
