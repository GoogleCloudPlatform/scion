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
 * GCP Service Account List Component
 *
 * CRUD component for managing GCP service accounts in a scope: a Scion project,
 * or the hub. Follows the same patterns as scion-secret-list.
 *
 * IT USED TO BE PINNED TO A PROJECT (a single `projectId` property, nested URLs
 * hard-coded at five call sites). Hub-scoped accounts are parentless, so that
 * shape could not render them at all without borrowing an unrelated project's
 * id. Every URL now comes from ../../shared/gcp-service-account-urls.js, which
 * is the only place in the web client that decides which address an account
 * has.
 *
 * WHAT DOES NOT CHANGE WITH SCOPE: which buttons appear. Delete and re-verify
 * are rendered from each row's `_capabilities`, computed per account by the
 * Hub, and never from the row being visible. That distinction is load-bearing
 * for hub-scoped accounts specifically, because EVERY logged-in user can see
 * them (hub-member-read-all grants read on "*" at hub scope) while almost none
 * can delete them -- so "visible" and "manageable" are the same set for project
 * accounts and wildly different sets here. A list that rendered Delete from
 * existence would give most of the hub a button that 403s.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type {
  GCPServiceAccount,
  GCPVerificationStatus,
  Capabilities,
  GCPMintQuotaInfo,
} from '../../shared/types.js';
import { can } from '../../shared/types.js';
import type { GCPSAListScope } from '../../shared/gcp-service-account-urls.js';
import {
  saCreateUrl,
  saDetailPath,
  saListUrl,
  saMintUrl,
  saRef,
  saVerifyUrl,
} from '../../shared/gcp-service-account-urls.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';
import { showToast } from '../../utils/toast.js';
import { showConfirm } from './confirm-dialog.js';

/** Detail payload for the `sa-list-changed` CustomEvent. */
export interface SAListChangedDetail {
  action: 'registered' | 'verified' | 'minted' | 'deleted';
  account: GCPServiceAccount;
}

@customElement('scion-gcp-service-account-list')
export class ScionGCPServiceAccountList extends LitElement {
  /** 'project' for a Scion project's own accounts, 'hub' for hub-scoped ones. */
  @property() scope: GCPSAListScope = 'project';

  /** The Scion project id at project scope. Unused, and refused, at hub scope. */
  @property() scopeId = '';

  @property({ type: Boolean }) compact = false;

  @state() private accounts: GCPServiceAccount[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private listCapabilities: Capabilities | undefined;

  // Register dialog state (BYOSA — bring your own service account)
  @state() private dialogOpen = false;
  @state() private dialogEmail = '';
  @state() private dialogProjectId = '';
  @state() private dialogDisplayName = '';
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Action state
  @state() private verifyingId: string | null = null;
  @state() private deletingId: string | null = null;

  // Mint dialog state
  @state() private mintDialogOpen = false;
  @state() private mintAccountId = '';
  @state() private mintDisplayName = '';
  @state() private mintDescription = '';
  @state() private mintDialogLoading = false;
  @state() private mintDialogError: string | null = null;
  @state() private mintAllowSelfActAs = true;

  // Quota info
  @state() private mintQuota: GCPMintQuotaInfo | null = null;

  // Copy-to-clipboard state
  @state() private copiedEmail: string | null = null;

  // Verify-failed dialog state
  @state() private verifyFailedOpen = false;
  @state() private verifyFailedHubEmail = '';
  @state() private verifyFailedTargetEmail = '';

  static override styles = [
    resourceStyles,
    css`
      .status-cell-inline {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
      }

      .verify-failed-content {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .verify-failed-content p {
        margin: 0;
        line-height: 1.5;
      }

      .verify-failed-content code {
        background: var(--sl-color-neutral-100, #f1f5f9);
        padding: 0.125rem 0.375rem;
        border-radius: 0.25rem;
        font-size: 0.875em;
        word-break: break-all;
      }

      .managed-badge {
        display: inline-flex;
        align-items: center;
        font-size: 0.6875rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
        font-weight: 500;
        white-space: nowrap;
      }

      .quota-info {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .quota-info .quota-warning {
        color: var(--sl-color-warning-700, #a16207);
      }

      .list-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        flex-wrap: wrap;
      }

      .verify-failed-content .gcloud-command {
        background: var(--sl-color-neutral-100, #f1f5f9);
        padding: 0.75rem 1rem;
        border-radius: 0.375rem;
        font-family: monospace;
        font-size: 0.8125rem;
        line-height: 1.6;
        overflow-x: auto;
        white-space: pre-wrap;
        word-break: break-all;
      }
    `,
  ];

  /** The scope the currently displayed rows were loaded for. */
  private loadedKey: string | null = null;

  /**
   * THE LOAD IS KEYED ON THE SCOPE, NOT ON A LIFECYCLE MOMENT.
   *
   * This component used to load once from connectedCallback, which was
   * sufficient while it was pinned to one project for its whole life. A
   * component that can be re-pointed -- another project, or the hub -- must
   * re-load when that happens, and the failure if it does not is the bad kind:
   * the previous scope's accounts stay on screen and look like an answer.
   *
   * Keying on scope rather than reacting to the changed-properties map also
   * makes the FIRST update free: Lit reports every initially-set property as
   * changed, so a naive `changed.has('scope')` fires a second, identical
   * request on mount.
   *
   * THE KEY CHECK IS NOT AN OPTIMISATION. Measured by deleting it: loadAccounts
   * sets @state, @state schedules an update, updated() calls this again --
   * unbounded, and the test run hangs rather than failing. Anything that
   * re-enters this method must keep an early return that does not depend on
   * loadAccounts having finished.
   */
  private maybeLoad(): void {
    const key = `${this.scope}:${this.scopeId}`;
    if (key === this.loadedKey) return;
    this.loadedKey = key;
    void this.loadAccounts();
  }

  override firstUpdated(): void {
    this.maybeLoad();
  }

  override updated(): void {
    this.maybeLoad();
  }

  /**
   * THERE IS NO CREATE AFFORDANCE AT HUB SCOPE, AND THAT IS NOT A CAPABILITY
   * DECISION.
   *
   * The Hub refuses hub-scoped registration outright: POST to the flat
   * collection with scope=hub answers 400 invalid_request, and it does so
   * before consulting any policy, because the enabling change is held (#19).
   * So a hub admin's `create` capability at hub scope is TRUE and the operation
   * still fails -- capability answers "may you", not "is it implemented".
   *
   * Rendering the button from the capability alone would therefore produce the
   * one thing this feature is under instruction to avoid: an affordance that
   * cannot work. It is suppressed here rather than in the template so the rule
   * has a name and a single place to be deleted from when the hold lifts.
   *
   * WHAT MUST NOT HAPPEN when it does lift: this returning true while
   * saCreateUrl still points somewhere that succeeds by registering the wrong
   * thing. The URL is already correct -- it addresses the refusal -- which is
   * why the two live apart.
   */
  private canCreateHere(): boolean {
    if (this.scope === 'hub') return false;
    return can(this.listCapabilities, 'create');
  }

  /**
   * Mint is a per-project operation against the Hub's own GCP project, with a
   * per-project quota; the flat route has no mint endpoint at all. At hub scope
   * there is nowhere to send it, which saMintUrl says by returning null.
   */
  private canMintHere(): boolean {
    if (this.scope !== 'project') return false;
    return can(this.listCapabilities, 'mint');
  }

  /**
   * Dispatches a bubbling `sa-list-changed` event so parent components can
   * react to registration, verification, minting, or deletion of a service
   * account without a full page reload.
   */
  private dispatchSAChange(detail: SAListChangedDetail): void {
    this.dispatchEvent(
      new CustomEvent('sa-list-changed', {
        bubbles: true,
        composed: true,
        detail,
      })
    );
  }

  private async loadAccounts(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(saListUrl(this.scope, this.scopeId));

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      const data = (await response.json()) as
        | {
            items?: GCPServiceAccount[];
            _capabilities?: Capabilities;
            mint_quota?: GCPMintQuotaInfo;
          }
        | GCPServiceAccount[];

      if (Array.isArray(data)) {
        this.accounts = data;
      } else {
        this.accounts = data.items || [];
        this.listCapabilities = data._capabilities;
        this.mintQuota = data.mint_quota || null;
      }
    } catch (err) {
      console.error('Failed to load GCP service accounts:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load service accounts';
    } finally {
      this.loading = false;
    }
  }

  private openMintDialog(): void {
    this.mintAccountId = '';
    this.mintDisplayName = '';
    this.mintDescription = '';
    this.mintAllowSelfActAs = true;
    this.mintDialogError = null;
    this.mintDialogOpen = true;
  }

  private closeMintDialog(): void {
    this.mintDialogOpen = false;
  }

  private async handleMint(e: Event): Promise<void> {
    e.preventDefault();

    this.mintDialogLoading = true;
    this.mintDialogError = null;

    try {
      const body: Record<string, unknown> = {};
      if (this.mintAccountId.trim()) body.account_id = this.mintAccountId.trim();
      if (this.mintDisplayName.trim()) body.display_name = this.mintDisplayName.trim();
      if (this.mintDescription.trim()) body.description = this.mintDescription.trim();
      if (!this.mintAllowSelfActAs) body.allow_self_act_as = false;

      // Non-null asserted through a guard rather than a `!`: openMintDialog is
      // unreachable at hub scope (renderMintAffordance returns nothing there),
      // and mint has no hub address to fall back on.
      const url = saMintUrl(this.scope, this.scopeId);
      if (!url) {
        throw new Error('Minting is only available for a project');
      }

      const response = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      // Parse the minted SA from the response before any other consumption.
      const mintedSA = (await response.json().catch(() => null)) as GCPServiceAccount | null;

      this.closeMintDialog();
      await this.loadAccounts();

      if (mintedSA?.id) {
        this.dispatchSAChange({ action: 'minted', account: mintedSA });
      }
    } catch (err) {
      console.error('Failed to mint service account:', err);
      this.mintDialogError = err instanceof Error ? err.message : 'Failed to mint service account';
    } finally {
      this.mintDialogLoading = false;
    }
  }

  private isMintDisabled(): boolean {
    if (!this.mintQuota) return false;
    const { project_cap, project_minted, global_cap, global_minted } = this.mintQuota;
    if (project_cap > 0 && project_minted >= project_cap) return true;
    if (global_cap > 0 && global_minted >= global_cap) return true;
    return false;
  }

  private openAddDialog(): void {
    this.dialogEmail = '';
    this.dialogProjectId = '';
    this.dialogDisplayName = '';
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
  }

  private async handleAdd(e: Event): Promise<void> {
    e.preventDefault();

    const email = this.dialogEmail.trim();
    if (!email) {
      this.dialogError = 'Service account email is required';
      return;
    }

    const projectId = this.dialogProjectId.trim() || this.extractProjectFromEmail(email);

    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const body: Record<string, unknown> = {
        email,
        projectId,
      };
      if (this.dialogDisplayName.trim()) {
        body.displayName = this.dialogDisplayName.trim();
      }

      // saCreateUrl at hub scope addresses the Hub's own refusal, not some
      // project's collection. No affordance reaches this line at hub scope
      // today; if one ever does, it must fail the way the server says it
      // fails rather than quietly registering a project-scoped account.
      const response = await apiFetch(saCreateUrl(this.scope, this.scopeId), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      // Parse the created SA (and verification metadata) before any other
      // consumption — response.json() can only be read once.
      const data = (await response.json().catch(() => ({}))) as GCPServiceAccount & {
        verificationFailed?: boolean;
        verificationDetails?: { hubServiceAccountEmail?: string; targetEmail?: string };
      };

      this.closeDialog();
      await this.loadAccounts();

      if (data.id) {
        this.dispatchSAChange({ action: 'registered', account: data });
      }

      if (data.verificationFailed) {
        this.verifyFailedHubEmail = data.verificationDetails?.hubServiceAccountEmail || '';
        this.verifyFailedTargetEmail = data.verificationDetails?.targetEmail || email;
        this.verifyFailedOpen = true;
      }
    } catch (err) {
      console.error('Failed to register service account:', err);
      this.dialogError = err instanceof Error ? err.message : 'Failed to register service account';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleVerify(account: GCPServiceAccount): Promise<void> {
    this.verifyingId = account.id;

    try {
      const response = await apiFetch(saVerifyUrl(account), { method: 'POST' });

      if (!response.ok) {
        const errorData = (await response.json().catch(() => ({}))) as {
          error?: {
            message?: string;
            details?: { hubServiceAccountEmail?: string; targetEmail?: string };
          };
        };

        const details = errorData?.error?.details;
        if (details?.hubServiceAccountEmail) {
          this.verifyFailedHubEmail = details.hubServiceAccountEmail;
          this.verifyFailedTargetEmail = details.targetEmail || account.email;
          this.verifyFailedOpen = true;
        } else {
          this.verifyFailedHubEmail = '';
          this.verifyFailedTargetEmail = account.email;
          this.verifyFailedOpen = true;
        }

        await this.loadAccounts();
        return;
      }

      // Parse the updated SA from the verify response before reloading.
      const updatedSA = (await response.json().catch(() => null)) as GCPServiceAccount | null;

      await this.loadAccounts();

      if (updatedSA?.id) {
        this.dispatchSAChange({ action: 'verified', account: updatedSA });
      }
    } catch (err) {
      console.error('Failed to verify service account:', err);
      this.verifyFailedHubEmail = '';
      this.verifyFailedTargetEmail = account.email;
      this.verifyFailedOpen = true;
    } finally {
      this.verifyingId = null;
    }
  }

  private closeVerifyFailedDialog(): void {
    this.verifyFailedOpen = false;
  }

  private async handleDelete(account: GCPServiceAccount, event?: MouseEvent): Promise<void> {
    if (
      !event?.altKey &&
      !(await showConfirm(`Delete service account "${account.email}"? This cannot be undone.`))
    ) {
      return;
    }

    this.deletingId = account.id;

    try {
      const response = await apiFetch(saRef(account), { method: 'DELETE' });

      if (!response.ok && response.status !== 204) {
        throw new Error(
          await extractApiError(response, `Failed to delete (HTTP ${response.status})`)
        );
      }

      await this.loadAccounts();
      this.dispatchSAChange({ action: 'deleted', account });
    } catch (err) {
      console.error('Failed to delete service account:', err);
      showToast(err instanceof Error ? err.message : 'Failed to delete');
    } finally {
      this.deletingId = null;
    }
  }

  private getVerificationStatus(account: GCPServiceAccount): GCPVerificationStatus {
    if (account.verificationStatus) return account.verificationStatus;
    return account.verified ? 'verified' : 'unverified';
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
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
          <p>Loading service accounts...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load</h2>
          <p>There was a problem loading GCP service accounts.</p>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadAccounts()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    const canCreate = this.canCreateHere();
    const canMint = this.canMintHere();
    const showHeader = canCreate || canMint;

    return html`
      ${showHeader
        ? html`
            <div class="list-header">
              ${canCreate
                ? html`<sl-button variant="primary" @click=${this.openAddDialog}>
                    <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                    Register Existing
                  </sl-button>`
                : ''}
              ${canMint
                ? html`<sl-button
                    variant="default"
                    @click=${this.openMintDialog}
                    ?disabled=${this.isMintDisabled()}
                  >
                    <sl-icon slot="prefix" name="shield-check"></sl-icon>
                    Mint Service Account
                  </sl-button>`
                : ''}
              ${this.renderQuotaInfo()}
            </div>
          `
        : ''}
      ${this.accounts.length === 0 ? this.renderEmpty() : this.renderTable()} ${this.renderDialog()}
      ${this.renderMintDialog()} ${this.renderVerifyFailedDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>GCP Service Accounts</h2>
            <p>Manage GCP service accounts for agent identity assignment in this project.</p>
          </div>
          ${this.canCreateHere()
            ? html`
                <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  Register Existing
                </sl-button>
              `
            : ''}
          ${this.canMintHere()
            ? html`
                <sl-button
                  variant="default"
                  size="small"
                  @click=${this.openMintDialog}
                  ?disabled=${this.isMintDisabled()}
                >
                  <sl-icon slot="prefix" name="shield-check"></sl-icon>
                  Mint
                </sl-button>
              `
            : ''}
        </div>
        ${this.renderQuotaInfo()}
        ${this.loading
          ? html`<div class="section-loading">
              <sl-spinner></sl-spinner> Loading service accounts...
            </div>`
          : this.error
            ? html`<div class="section-error">
                <span>${this.error}</span>
                <sl-button size="small" @click=${() => this.loadAccounts()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>`
            : this.accounts.length === 0
              ? this.renderEmpty()
              : this.renderTable()}
      </div>
      ${this.renderDialog()} ${this.renderMintDialog()} ${this.renderVerifyFailedDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Email</th>
              <th class="hide-mobile">GCP Project</th>
              <th class="hide-mobile">Name</th>
              <th>Status</th>
              <th class="actions-cell"></th>
            </tr>
          </thead>
          <tbody>
            ${this.accounts.map((account) => this.renderRow(account))}
          </tbody>
        </table>
      </div>
    `;
  }

  private async copyEmail(email: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(email);
      this.copiedEmail = email;
      setTimeout(() => {
        this.copiedEmail = null;
      }, 1500);
    } catch {
      // Clipboard unavailable
    }
  }

  private renderRow(account: GCPServiceAccount) {
    const isDeleting = this.deletingId === account.id;
    const isVerifying = this.verifyingId === account.id;

    return html`
      <tr>
        <td class="key-cell">
          <div class="key-info">
            <div
              class="key-icon"
              style="background: var(--sl-color-primary-100, #dbeafe); color: var(--sl-color-primary-600, #2563eb);"
            >
              <sl-icon name="shield-lock"></sl-icon>
            </div>
            ${this.renderEmail(account)}
            ${account.managed ? html`<span class="managed-badge">Hub-minted</span>` : ''}
            <sl-tooltip content=${this.copiedEmail === account.email ? 'Copied!' : 'Copy email'}>
              <sl-icon-button
                name=${this.copiedEmail === account.email ? 'clipboard-check' : 'clipboard'}
                label="Copy email"
                style="font-size: 0.875rem;"
                @click=${() => this.copyEmail(account.email)}
              ></sl-icon-button>
            </sl-tooltip>
          </div>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${account.projectId}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${account.displayName || '\u2014'}</span>
        </td>
        <td>${this.renderStatus(account, isVerifying, isDeleting)}</td>
        <td class="actions-cell">
          ${can(account._capabilities, 'delete')
            ? html`
                <sl-icon-button
                  name="trash"
                  label="Delete"
                  ?disabled=${isDeleting || isVerifying}
                  @click=${(e: MouseEvent) => this.handleDelete(account, e)}
                ></sl-icon-button>
              `
            : ''}
        </td>
      </tr>
    `;
  }

  /**
   * The email links to a detail page only where one exists. saDetailPath
   * returns null for project-scoped accounts, which are managed from their
   * project's settings tab -- so this renders a link for exactly the accounts
   * that have somewhere to go, rather than for every row with a dead
   * destination for most of them.
   */
  private renderEmail(account: GCPServiceAccount) {
    const href = saDetailPath(account);
    return href ? html`<a href=${href}>${account.email}</a>` : html`${account.email}`;
  }

  private renderStatus(account: GCPServiceAccount, isVerifying: boolean, isDeleting: boolean) {
    const status = this.getVerificationStatus(account);

    const badge =
      status === 'verified'
        ? html`<sl-badge variant="success">
            Verified
            ${account.verifiedAt
              ? html`<sl-tooltip content="Verified ${this.formatRelativeTime(account.verifiedAt)}"
                  ><span>✓</span></sl-tooltip
                >`
              : ''}
          </sl-badge>`
        : status === 'failed'
          ? html`<sl-tooltip
              content=${account.verificationError ||
              'Hub service account lacks serviceAccountTokenCreator role on this SA.'}
            >
              <sl-badge variant="danger">Failed</sl-badge>
            </sl-tooltip>`
          : html`<sl-badge variant="warning">Unverified</sl-badge>`;

    const canVerify = can(account._capabilities, 'verify');

    return html`
      <div class="status-cell-inline">
        ${badge}
        ${canVerify
          ? html`
              <sl-icon-button
                name="arrow-clockwise"
                label="Re-check verification"
                style="font-size: 0.875rem;"
                ?disabled=${isVerifying || isDeleting}
                @click=${() => this.handleVerify(account)}
              ></sl-icon-button>
            `
          : ''}
        ${isVerifying ? html`<sl-spinner style="font-size: 0.75rem;"></sl-spinner>` : ''}
      </div>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="shield-lock"></sl-icon>
        <h3>No GCP Service Accounts</h3>
        <p>Register an existing GCP service account, or mint a new one in the Hub's project.</p>
        ${this.canCreateHere()
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Register Existing
              </sl-button>
            `
          : ''}
        ${this.canMintHere()
          ? html`
              <sl-button
                variant="default"
                size="small"
                @click=${this.openMintDialog}
                ?disabled=${this.isMintDisabled()}
              >
                <sl-icon slot="prefix" name="shield-check"></sl-icon>
                Mint Service Account
              </sl-button>
            `
          : ''}
      </div>
    `;
  }

  private renderDialog() {
    return html`
      <sl-dialog
        label="Register GCP Service Account"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleAdd}>
          <sl-input
            label="Service Account Email"
            placeholder="e.g. agent-worker@project.iam.gserviceaccount.com"
            value=${this.dialogEmail}
            @sl-input=${(e: Event) => {
              this.dialogEmail = (e.target as HTMLInputElement).value;
              const inferred = this.extractProjectFromEmail(this.dialogEmail);
              if (inferred) {
                this.dialogProjectId = inferred;
              }
            }}
            required
          ></sl-input>

          <sl-input
            label="GCP Project ID"
            placeholder="e.g. my-project-123"
            value=${this.dialogProjectId}
            help-text=${this.extractProjectFromEmail(this.dialogEmail)
              ? 'Auto-detected from service account email'
              : ''}
            @sl-input=${(e: Event) => {
              this.dialogProjectId = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <sl-input
            label="Display Name"
            placeholder="Optional human-friendly label"
            value=${this.dialogDisplayName}
            @sl-input=${(e: Event) => {
              this.dialogDisplayName = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <div class="dialog-hint">
            <sl-icon name="info-circle"></sl-icon>
            The Hub will automatically attempt to verify impersonation access after registration.
          </div>

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
          @click=${this.handleAdd}
        >
          Register
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderQuotaInfo() {
    if (!this.mintQuota) return nothing;
    const { project_minted, project_cap, global_minted, global_cap } = this.mintQuota;

    const parts: string[] = [];
    if (project_cap > 0) {
      parts.push(`Project: ${project_minted}/${project_cap}`);
    }
    if (global_cap > 0) {
      parts.push(`Global: ${global_minted}/${global_cap}`);
    }
    if (parts.length === 0) return nothing;

    const atLimit = this.isMintDisabled();
    return html`
      <span class="quota-info ${atLimit ? 'quota-warning' : ''}">
        Minted: ${parts.join(' · ')}${atLimit ? ' (limit reached)' : ''}
      </span>
    `;
  }

  private renderMintDialog() {
    return html`
      <sl-dialog
        label="Mint GCP Service Account"
        ?open=${this.mintDialogOpen}
        @sl-request-close=${this.closeMintDialog}
      >
        <form class="dialog-form" @submit=${this.handleMint}>
          <sl-input
            label="Account ID"
            placeholder="Optional (e.g. my-pipeline → scion-my-pipeline)"
            help-text="Leave empty for auto-generated ID. Will be prefixed with scion-."
            value=${this.mintAccountId}
            @sl-input=${(e: Event) => {
              this.mintAccountId = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <sl-input
            label="Display Name"
            placeholder="Optional human-friendly label"
            value=${this.mintDisplayName}
            @sl-input=${(e: Event) => {
              this.mintDisplayName = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <sl-input
            label="Description"
            placeholder="Optional description"
            value=${this.mintDescription}
            @sl-input=${(e: Event) => {
              this.mintDescription = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <sl-checkbox
            ?checked=${this.mintAllowSelfActAs}
            @sl-change=${(e: Event) => {
              this.mintAllowSelfActAs = (e.target as HTMLInputElement).checked;
            }}
          >
            Allow this service account to act as itself
            <div slot="help-text">
              Enables using this SA as a project default, allowing agents to create sub-agents that
              run as this same identity. Recommended for most use cases.
            </div>
          </sl-checkbox>

          <div class="dialog-hint">
            <sl-icon name="info-circle"></sl-icon>
            The Hub will create a new service account in its own GCP project. The SA is
            automatically verified for impersonation by the Hub.
          </div>

          ${this.mintDialogError
            ? html`<div class="dialog-error">${this.mintDialogError}</div>`
            : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeMintDialog}
          ?disabled=${this.mintDialogLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.mintDialogLoading}
          ?disabled=${this.mintDialogLoading}
          @click=${this.handleMint}
        >
          Mint
        </sl-button>
      </sl-dialog>
    `;
  }

  private extractProjectFromEmail(email: string): string {
    const match = email.match(/@(.+)\.iam\.gserviceaccount\.com$/);
    return match ? match[1] : '';
  }

  private renderVerifyFailedDialog() {
    const targetProject = this.extractProjectFromEmail(this.verifyFailedTargetEmail);
    const projectFlag = targetProject ? ` \\\n  --project="${targetProject}"` : '';
    const gcloudCmd = this.verifyFailedHubEmail
      ? `gcloud iam service-accounts add-iam-policy-binding \\
  ${this.verifyFailedTargetEmail} \\
  --member="serviceAccount:${this.verifyFailedHubEmail}" \\
  --role="roles/iam.serviceAccountTokenCreator"${projectFlag}`
      : '';

    return html`
      <sl-dialog
        label="Verification Failed"
        ?open=${this.verifyFailedOpen}
        @sl-request-close=${this.closeVerifyFailedDialog}
      >
        <div class="verify-failed-content">
          <p>
            The Hub could not impersonate the service account
            <code>${this.verifyFailedTargetEmail}</code>.
          </p>

          ${this.verifyFailedHubEmail
            ? html`
                <p>
                  The Hub's service account
                  <code>${this.verifyFailedHubEmail}</code> needs the
                  <strong>Service Account Token Creator</strong> role
                  (<code>roles/iam.serviceAccountTokenCreator</code>) on the target service account.
                </p>
                <p>Run the following command to grant access:</p>
                <div class="gcloud-command">${gcloudCmd}</div>
              `
            : html`
                <p>
                  Ensure the Hub's service account has the
                  <strong>Service Account Token Creator</strong> role
                  (<code>roles/iam.serviceAccountTokenCreator</code>) on this service account.
                </p>
              `}

          <p>
            <strong>Note:</strong> This service account will not be usable for agent identity
            assignment until verification succeeds.
          </p>
          <p>After granting the role, click the refresh icon to re-check verification.</p>
          <p>
            <strong>Note:</strong> GCP IAM permission changes may take several minutes to propagate.
          </p>
        </div>

        <sl-button slot="footer" variant="primary" @click=${this.closeVerifyFailedDialog}>
          OK
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-gcp-service-account-list': ScionGCPServiceAccountList;
  }
}
