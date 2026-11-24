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
 * Profile Inbox page (`/profile/inbox`, ptone/scion#8962, design §6.3–§6.4).
 *
 * Gated on `web.inbox_dashboard`: when off (or when the hub answers 404),
 * renders `<scion-page-404>`.
 *
 * Phase 1 renders the Approvals card (pending user-scope secrets with inline
 * approve/reject) and hides disabled categories.
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  INBOX_SUMMARY_CHANGED_EVENT,
  inboxSummaryStore,
  type InboxApprovalPreview,
  type InboxCategory,
  type InboxSummaryResponse,
} from '../../client/inbox-summary.js';
import {
  PENDING_SECRETS_API,
  PENDING_SECRETS_CHANGED_EVENT,
} from '../../client/pending-secrets.js';
import {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  fetchProfileDecisionProbe,
  getInitialProfileDecisionVisibility,
  resolveProfileDecisionVisibility,
  type ProfileDecisionVisibility,
} from '../../client/profile-decisions.js';
import { stateManager } from '../../client/state.js';
import {
  FEATURE_FLAGS_CHANGED_EVENT,
  decisionsEnabled,
  isInboxDashboardEnabled,
} from '../../utils/feature-flags.js';
import { showToast } from '../../utils/toast.js';
import { inboxTabStripStyles, renderInboxTabStrip } from '../shared/inbox-tabs.js';
import './not-found.js';

function formatRelativeExpiry(iso: string): string {
  const ms = Date.parse(iso) - Date.now();
  if (Number.isNaN(ms)) return '';
  if (ms <= 0) return 'expired';
  const mins = Math.ceil(ms / 60_000);
  if (mins < 60) return `expires in ${mins}m`;
  const hours = Math.round(mins / 60);
  if (hours < 48) return `expires in ${hours}h`;
  const days = Math.round(hours / 24);
  return `expires in ${days}d`;
}

@customElement('scion-page-profile-inbox')
export class ScionPageProfileInbox extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private enabled = false;

  @state()
  private summary: InboxSummaryResponse | null = null;

  @state()
  private loading = false;

  @state()
  private error: string | null = null;

  @state()
  private unavailable404 = false;

  @state()
  private busyIds = new Set<string>();

  @state()
  private confirmApproval: InboxApprovalPreview | null = null;

  /** Mirrors `<scion-profile-nav>`'s 404 probe state for the Decisions sub-tab. */
  @state()
  private decisionsNavVisibility: ProfileDecisionVisibility = 'unknown';

  private unmountPage: (() => void) | null = null;

  private boundOnSummaryChanged = (): void => {
    this.syncFromStore();
  };

  private boundOnFeatureFlagsChanged = (): void => {
    const next = isInboxDashboardEnabled();
    if (next !== this.enabled) {
      this.enabled = next;
      if (next) {
        this.startStore();
      } else {
        this.stopStore();
      }
    }
    if (this.enabled && decisionsEnabled()) {
      this.syncOrProbeDecisionsAvailability();
    } else {
      this.decisionsNavVisibility = 'unknown';
    }
  };

  private boundOnProfileDecisionsCountChanged = (e: Event): void => {
    const detail = (e as CustomEvent<{ available?: boolean; waitingOnYou?: number }>).detail;
    if (detail?.available === false) {
      this.decisionsNavVisibility = 'hidden';
      return;
    }
    if (typeof detail?.waitingOnYou === 'number') {
      this.decisionsNavVisibility = 'visible';
    }
  };

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
        align-items: center;
        justify-content: space-between;
        gap: 1rem;
        margin-bottom: 0.75rem;
      }

      .page-title {
        margin: 0;
        font-size: 1.5rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .caught-up-banner {
        display: flex;
        align-items: center;
        gap: 0.625rem;
        padding: 0.875rem 1rem;
        margin-bottom: 1rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text-muted, #64748b);
        font-size: 0.9rem;
      }

      .caught-up-banner sl-icon {
        color: var(--sl-color-success-600, #16a34a);
        font-size: 1.15rem;
      }

      .card-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
        gap: 1rem;
      }

      .inbox-card {
        display: flex;
        flex-direction: column;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .card-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .card-title-group {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        min-width: 0;
      }

      .card-title-group sl-icon {
        font-size: 1.1rem;
        color: var(--scion-primary, #3b82f6);
        flex-shrink: 0;
      }

      .card-title {
        margin: 0;
        font-size: 0.95rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .card-view-all {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        font-size: 0.8125rem;
        font-weight: 500;
        color: var(--scion-primary, #3b82f6);
        text-decoration: none;
        white-space: nowrap;
      }

      .card-view-all:hover {
        text-decoration: underline;
      }

      .card-body {
        padding: 0.75rem 1rem;
      }

      .card-empty {
        margin: 0;
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .card-error {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        font-size: 0.875rem;
        color: var(--sl-color-danger-600, #dc2626);
      }

      .preview-list {
        list-style: none;
        margin: 0;
        padding: 0;
        display: flex;
        flex-direction: column;
        gap: 0.625rem;
      }

      .preview-row {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        gap: 0.75rem;
        padding: 0.625rem 0.75rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-sm, 0.375rem);
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .preview-info {
        min-width: 0;
        flex: 1;
      }

      .preview-key {
        display: inline-block;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.85rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        text-decoration: none;
        word-break: break-all;
      }

      .preview-key:hover {
        color: var(--scion-primary, #3b82f6);
        text-decoration: underline;
      }

      .preview-meta {
        margin-top: 0.25rem;
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.375rem;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .preview-actions {
        display: flex;
        align-items: center;
        gap: 0.375rem;
        flex-shrink: 0;
      }

      .loading-state {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        padding: 2rem 0;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.9rem;
      }

      .confirm-details {
        margin: 0.75rem 0 0;
        padding: 0.75rem;
        border-radius: var(--scion-radius-sm, 0.375rem);
        background: var(--scion-bg-subtle, #f1f5f9);
        font-size: 0.875rem;
        display: grid;
        grid-template-columns: auto 1fr;
        gap: 0.375rem 0.75rem;
      }

      .confirm-details dt {
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
      }

      .confirm-details dd {
        margin: 0;
        color: var(--scion-text, #1e293b);
        word-break: break-all;
      }

      .dialog-footer {
        display: flex;
        justify-content: flex-end;
        gap: 0.5rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    this.enabled = isInboxDashboardEnabled();
    window.addEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnSummaryChanged);
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    window.addEventListener(
      PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
      this.boundOnProfileDecisionsCountChanged
    );
    if (this.enabled) {
      this.startStore();
      if (decisionsEnabled()) {
        this.syncOrProbeDecisionsAvailability();
      }
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(INBOX_SUMMARY_CHANGED_EVENT, this.boundOnSummaryChanged);
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFeatureFlagsChanged);
    window.removeEventListener(
      PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
      this.boundOnProfileDecisionsCountChanged
    );
    this.stopStore();
  }

  private syncOrProbeDecisionsAvailability(): void {
    const initial = getInitialProfileDecisionVisibility();
    this.decisionsNavVisibility = initial.visibility;
    if (!initial.needsProbe) {
      return;
    }
    void this.probeDecisionsAvailability();
  }

  private async probeDecisionsAvailability(): Promise<void> {
    if (!decisionsEnabled()) return;
    const probe = await fetchProfileDecisionProbe();
    if (!this.isConnected || !decisionsEnabled()) return;
    this.decisionsNavVisibility = resolveProfileDecisionVisibility(
      this.decisionsNavVisibility,
      probe
    ).visibility;
  }

  private startStore(): void {
    dispatchPageTitle(this, 'Inbox', 'Profile');
    const uid = this.pageData?.user?.id || stateManager.getCurrentUserId() || '';
    if (uid) {
      stateManager.setCurrentUserId(uid);
    }
    if (typeof EventSource !== 'undefined') {
      stateManager.setScope(uid ? { type: 'profile', userId: uid } : { type: 'profile' });
    }
    this.unmountPage?.();
    this.unmountPage = inboxSummaryStore.mountPage(uid || undefined);
    this.syncFromStore();
  }

  private stopStore(): void {
    this.unmountPage?.();
    this.unmountPage = null;
  }

  private syncFromStore(): void {
    this.summary = inboxSummaryStore.summary;
    this.loading = inboxSummaryStore.loading;
    this.error = inboxSummaryStore.error;
    this.unavailable404 = inboxSummaryStore.unavailable404;
  }

  private setBusy(id: string, busy: boolean): void {
    const next = new Set(this.busyIds);
    if (busy) next.add(id);
    else next.delete(id);
    this.busyIds = next;
  }

  private async approveSecret(item: InboxApprovalPreview): Promise<void> {
    if (this.busyIds.has(item.id)) return;
    this.setBusy(item.id, true);
    try {
      const res = await apiFetch(`${PENDING_SECRETS_API}/${encodeURIComponent(item.id)}/approve`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      if (!res.ok) {
        let msg = `Could not approve ${item.key} (${res.status})`;
        try {
          const data = (await res.json()) as { error?: { message?: string } };
          if (data.error?.message) msg = data.error.message;
        } catch {
          // Use status fallback.
        }
        showToast(msg, 'danger');
        return;
      }
      this.confirmApproval = null;
      inboxSummaryStore.optimisticRemoveApproval(item.id);
      window.dispatchEvent(new CustomEvent(PENDING_SECRETS_CHANGED_EVENT, { detail: {} }));
      showToast(`Approved secret ${item.key}`, 'success');
    } catch {
      showToast(`Could not approve ${item.key}`, 'danger');
    } finally {
      this.setBusy(item.id, false);
    }
  }

  private async rejectSecret(item: InboxApprovalPreview): Promise<void> {
    if (this.busyIds.has(item.id)) return;
    this.setBusy(item.id, true);
    try {
      const res = await apiFetch(`${PENDING_SECRETS_API}/${encodeURIComponent(item.id)}/reject`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
      });
      if (!res.ok) {
        let msg = `Could not reject ${item.key} (${res.status})`;
        try {
          const data = (await res.json()) as { error?: { message?: string } };
          if (data.error?.message) msg = data.error.message;
        } catch {
          // Use status fallback.
        }
        showToast(msg, 'danger');
        return;
      }
      inboxSummaryStore.optimisticRemoveApproval(item.id);
      window.dispatchEvent(new CustomEvent(PENDING_SECRETS_CHANGED_EVENT, { detail: {} }));
      showToast(`Rejected secret ${item.key}`, 'neutral');
    } catch {
      showToast(`Could not reject ${item.key}`, 'danger');
    } finally {
      this.setBusy(item.id, false);
    }
  }

  private renderApprovalsCard(
    cat: InboxCategory<InboxApprovalPreview>
  ): TemplateResult | typeof nothing {
    if (cat.state === 'disabled' || cat.state === 'unavailable') return nothing;
    const countLabel = cat.capped ? `${cat.count}+` : String(cat.count);

    return html`
      <section class="inbox-card" data-inbox-card="approvals">
        <header class="card-header">
          <div class="card-title-group">
            <sl-icon name="shield-check"></sl-icon>
            <h2 class="card-title">Approvals</h2>
            ${cat.state === 'ok'
              ? html`<sl-badge variant=${cat.count > 0 ? 'primary' : 'neutral'} pill data-card-count
                  >${countLabel}</sl-badge
                >`
              : nothing}
          </div>
          <a href=${cat.href || '/profile/secrets'} class="card-view-all" data-card-view-all>
            View all
            <sl-icon name="arrow-right"></sl-icon>
          </a>
        </header>
        <div class="card-body">
          ${cat.state === 'error'
            ? html`
                <div class="card-error" data-card-error>
                  <span>Couldn't load</span>
                  <sl-button
                    size="small"
                    variant="default"
                    @click=${(): void => inboxSummaryStore.requestRefresh({ immediate: true })}
                  >
                    Retry
                  </sl-button>
                </div>
              `
            : cat.count === 0
              ? html`<p class="card-empty" data-card-empty>Nothing waiting</p>`
              : html`
                  <ul class="preview-list">
                    ${cat.previews.map((item) => this.renderApprovalRow(item))}
                  </ul>
                `}
        </div>
      </section>
    `;
  }

  private renderApprovalRow(item: InboxApprovalPreview): TemplateResult {
    const busy = this.busyIds.has(item.id);
    const expiry = formatRelativeExpiry(item.expiresAt);
    return html`
      <li class="preview-row" data-approval-row=${item.id}>
        <div class="preview-info">
          <a href=${item.href || '/profile/secrets'} class="preview-key">${item.key}</a>
          <div class="preview-meta">
            ${item.agentSlug ? html`<span>by ${item.agentSlug}</span>` : nothing}
            ${item.projectName ? html`<span>· ${item.projectName}</span>` : nothing}
            ${item.injectionMode
              ? html`<sl-badge variant="neutral">${item.injectionMode}</sl-badge>`
              : nothing}
            ${item.replacesExisting
              ? html`<sl-badge variant="warning">Replaces existing</sl-badge>`
              : nothing}
            ${expiry ? html`<span>· ${expiry}</span>` : nothing}
          </div>
        </div>
        <div class="preview-actions">
          <sl-button
            size="small"
            variant="primary"
            ?loading=${busy}
            ?disabled=${busy}
            data-approve-btn=${item.id}
            @click=${(): void => {
              this.confirmApproval = item;
            }}
          >
            Approve
          </sl-button>
          <sl-button
            size="small"
            variant="default"
            ?disabled=${busy}
            data-reject-btn=${item.id}
            @click=${(): void => void this.rejectSecret(item)}
          >
            Reject
          </sl-button>
        </div>
      </li>
    `;
  }

  private renderConfirmDialog(): TemplateResult {
    const item = this.confirmApproval;
    const busy = item ? this.busyIds.has(item.id) : false;
    return html`
      <sl-dialog
        label="Approve pending secret"
        ?open=${item !== null}
        data-approve-confirm-dialog
        @sl-request-close=${(e: Event): void => {
          if (busy) {
            e.preventDefault();
            return;
          }
          this.confirmApproval = null;
        }}
      >
        ${item
          ? html`
              <p>
                Approve this user-scope secret? To inspect its value first, open
                <a href="/profile/secrets">Profile → Secrets</a>.
              </p>
              <dl class="confirm-details">
                <dt>Key</dt>
                <dd><code>${item.key}</code></dd>
                <dt>Agent</dt>
                <dd>${item.agentSlug || 'Unknown'}</dd>
                <dt>Project</dt>
                <dd>${item.projectName || 'Unknown'}</dd>
                <dt>Injection</dt>
                <dd>${item.injectionMode || 'default'}</dd>
                <dt>Inherited by child agents</dt>
                <dd data-confirm-allow-progeny>${item.allowProgeny ? 'Yes' : 'No'}</dd>
              </dl>
            `
          : nothing}
        <div slot="footer" class="dialog-footer">
          <sl-button
            variant="default"
            ?disabled=${busy}
            @click=${(): void => {
              this.confirmApproval = null;
            }}
          >
            Cancel
          </sl-button>
          <sl-button
            variant="primary"
            ?loading=${busy}
            ?disabled=${busy || !item}
            data-confirm-approve-btn
            @click=${(): void => {
              if (item) void this.approveSecret(item);
            }}
          >
            Approve secret
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  override render(): TemplateResult {
    if (!this.enabled || this.unavailable404) {
      return html`<scion-page-404></scion-page-404>`;
    }

    const categories = this.summary?.categories;
    const enabledCategories = categories
      ? [
          categories.decisions,
          categories.waiting,
          categories.approvals,
          categories.reviews,
          categories.chat,
        ].filter((c) => c && c.state !== 'disabled' && c.state !== 'unavailable')
      : [];
    const allCaughtUp =
      this.summary !== null &&
      enabledCategories.length > 0 &&
      enabledCategories.every((c) => c.state === 'ok' && c.count === 0);

    return html`
      <div class="page-header">
        <h1 class="page-title">Inbox</h1>
        <sl-button
          size="small"
          variant="default"
          ?loading=${this.loading}
          @click=${(): void => inboxSummaryStore.requestRefresh({ immediate: true })}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${renderInboxTabStrip('overview', {
        decisionsVisible: this.decisionsNavVisibility === 'visible',
      })}
      ${this.loading && !this.summary
        ? html`
            <div class="loading-state" data-inbox-loading>
              <sl-spinner></sl-spinner>
              <span>Loading inbox…</span>
            </div>
          `
        : this.error && !this.summary
          ? html`
              <sl-alert variant="danger" open data-inbox-error>
                <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                ${this.error}
                <sl-button
                  size="small"
                  variant="default"
                  style="margin-left: 0.75rem;"
                  @click=${(): void => inboxSummaryStore.requestRefresh({ immediate: true })}
                >
                  Retry
                </sl-button>
              </sl-alert>
            `
          : html`
              ${allCaughtUp
                ? html`
                    <div class="caught-up-banner" data-inbox-caught-up>
                      <sl-icon name="check-circle-fill"></sl-icon>
                      <span>You're all caught up.</span>
                    </div>
                  `
                : nothing}
              <div class="card-grid" data-inbox-grid>
                ${categories?.approvals ? this.renderApprovalsCard(categories.approvals) : nothing}
              </div>
            `}
      ${this.renderConfirmDialog()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-inbox': ScionPageProfileInbox;
  }
}
