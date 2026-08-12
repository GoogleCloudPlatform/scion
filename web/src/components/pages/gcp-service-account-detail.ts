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
 * GCP Service Account detail page — /settings/service-accounts/{id}
 *
 * View one account, re-run its verification, delete it.
 *
 * WHY THIS PAGE IS HUB-SIDE ONLY, and it is a deliberate asymmetry with
 * template-detail and harness-config-detail, which both have a project-nested
 * twin:
 *
 *   1. The accounts that NEED it are the parentless ones. A hub- or user-scoped
 *      account has no project, so there is no /projects/{id}/... address to give
 *      it; that absence is why the flat by-id API route exists at all.
 *   2. The nested GET (/api/v1/projects/{pid}/gcp-service-accounts/{id})
 *      returns the account WITHOUT `_capabilities`. A project-nested detail page
 *      could therefore only render Delete and Re-verify from the account being
 *      visible — which is precisely the thing this feature is under instruction
 *      not to do, and it is not a hypothetical here: hub-scoped accounts are
 *      readable by every logged-in user and deletable by almost none.
 *
 * So project-scoped accounts stay where their capabilities come from: the
 * project settings tab, whose list route does compute them per row. If a nested
 * detail page is ever wanted, the prerequisite is the nested GET returning
 * capabilities — not this page learning to guess.
 *
 * The flat route also serves USER-scoped accounts, and this page renders one
 * correctly if navigated to; nothing links there yet.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { GCPServiceAccount, GCPVerificationStatus } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { saRef, saVerifyUrl } from '../../shared/gcp-service-account-urls.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';

@customElement('scion-page-gcp-service-account-detail')
export class ScionPageGCPServiceAccountDetail extends LitElement {
  @state() private accountId = '';
  @state() private account: GCPServiceAccount | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private verifying = false;
  @state() private deleting = false;
  @state() private actionError: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .breadcrumb {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 1rem;
    }

    .breadcrumb a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 1.5rem;
    }

    .header h1 {
      font-size: 1.25rem;
      font-weight: 700;
      margin: 0 0 0.25rem 0;
      word-break: break-all;
    }

    .actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .panel {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
    }

    dl {
      display: grid;
      grid-template-columns: minmax(8rem, max-content) 1fr;
      gap: 0.75rem 1.5rem;
      margin: 0;
      font-size: 0.875rem;
    }

    dt {
      color: var(--scion-text-muted, #64748b);
    }

    dd {
      margin: 0;
      word-break: break-all;
    }

    .error-state,
    .loading-state {
      text-align: center;
      padding: 3rem;
      color: var(--sl-color-neutral-500, #64748b);
    }

    .action-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      margin-top: 1rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/settings\/service-accounts\/([^/]+)/);
      if (match) {
        this.accountId = decodeURIComponent(match[1]);
      }
    }
    void this.load();
  }

  private async load(): Promise<void> {
    if (!this.accountId) {
      this.loading = false;
      this.error = 'No service account id in the URL';
      return;
    }

    this.loading = true;
    this.error = null;

    try {
      // The flat address is built here rather than through saRef, because saRef
      // takes an account and this is the request that fetches one. It is the
      // only place in the client that addresses an account by id alone, and it
      // is correct only because this page serves parentless accounts.
      const response = await apiFetch(`/api/v1/gcp-service-accounts/${this.accountId}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.account = (await response.json()) as GCPServiceAccount;
      dispatchPageTitle(
        this,
        this.account.displayName || this.account.email || this.accountId,
        'Service Accounts'
      );
    } catch (err) {
      console.error('Failed to load GCP service account:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load service account';
    } finally {
      this.loading = false;
    }
  }

  private async handleVerify(): Promise<void> {
    if (!this.account) return;
    this.verifying = true;
    this.actionError = null;

    try {
      const response = await apiFetch(saVerifyUrl(this.account), { method: 'POST' });
      if (!response.ok) {
        this.actionError = await extractApiError(
          response,
          `Verification failed (HTTP ${response.status})`
        );
      }
      // Reload either way: a failed verification is PERSISTED by the Hub, so
      // the row's status is part of the answer and not only the error text.
      await this.load();
    } catch (err) {
      this.actionError = err instanceof Error ? err.message : 'Verification failed';
    } finally {
      this.verifying = false;
    }
  }

  private async handleDelete(): Promise<void> {
    if (!this.account) return;
    if (!confirm(`Delete service account "${this.account.email}"? This cannot be undone.`)) {
      return;
    }

    this.deleting = true;
    this.actionError = null;

    try {
      const response = await apiFetch(saRef(this.account), { method: 'DELETE' });
      if (!response.ok && response.status !== 204) {
        this.actionError = await extractApiError(
          response,
          `Failed to delete (HTTP ${response.status})`
        );
        return;
      }
      window.location.href = '/settings?tab=service-accounts';
    } catch (err) {
      this.actionError = err instanceof Error ? err.message : 'Failed to delete';
    } finally {
      this.deleting = false;
    }
  }

  private status(): GCPVerificationStatus {
    if (!this.account) return 'unverified';
    if (this.account.verificationStatus) return this.account.verificationStatus;
    return this.account.verified ? 'verified' : 'unverified';
  }

  override render() {
    if (this.loading) {
      return html`<div class="loading-state"><sl-spinner></sl-spinner></div>`;
    }

    if (this.error || !this.account) {
      return html`
        <div class="breadcrumb">
          <a href="/settings?tab=service-accounts">Hub Resources</a>
        </div>
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <p>${this.error ?? 'Service account not found'}</p>
        </div>
      `;
    }

    const account = this.account;
    const status = this.status();

    return html`
      <div class="breadcrumb">
        <a href="/settings?tab=service-accounts">Hub Resources</a>
        <span>/</span>
        <span>Service Accounts</span>
      </div>

      <div class="header">
        <div>
          <h1>${account.email}</h1>
          ${account.displayName ? html`<div>${account.displayName}</div>` : nothing}
        </div>
        <div class="actions">
          ${can(account._capabilities, 'verify')
            ? html`<sl-button
                size="small"
                ?loading=${this.verifying}
                ?disabled=${this.deleting}
                @click=${this.handleVerify}
              >
                <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                Re-verify
              </sl-button>`
            : nothing}
          ${can(account._capabilities, 'delete')
            ? html`<sl-button
                size="small"
                variant="danger"
                ?loading=${this.deleting}
                ?disabled=${this.verifying}
                @click=${this.handleDelete}
              >
                <sl-icon slot="prefix" name="trash"></sl-icon>
                Delete
              </sl-button>`
            : nothing}
        </div>
      </div>

      <div class="panel">
        <dl>
          <dt>Status</dt>
          <dd>
            ${status === 'verified'
              ? html`<sl-badge variant="success">Verified</sl-badge>`
              : status === 'failed'
                ? html`<sl-badge variant="danger">Failed</sl-badge>`
                : html`<sl-badge variant="warning">Unverified</sl-badge>`}
          </dd>

          <dt>Scope</dt>
          <dd>${account.scope}</dd>

          <!-- The GCP project the service account itself lives in, which is NOT
               the Scion project that owns the registration. A hub-scoped account
               has no owning Scion project at all. -->
          <dt>GCP project</dt>
          <dd>${account.projectId || '—'}</dd>

          <dt>Managed</dt>
          <dd>${account.managed ? 'Minted by this hub' : 'Registered'}</dd>

          ${account.verificationError
            ? html`<dt>Last error</dt>
                <dd>${account.verificationError}</dd>`
            : nothing}
        </dl>

        ${this.actionError ? html`<div class="action-error">${this.actionError}</div>` : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-gcp-service-account-detail': ScionPageGCPServiceAccountDetail;
  }
}
