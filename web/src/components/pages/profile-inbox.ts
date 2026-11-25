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
 * Renders the responsive category card grid in the fixed order Decisions,
 * Agents waiting (6b), Approvals, Reviews (6b), Chat. Disabled and unavailable
 * categories are hidden.
 */

import { LitElement, css, html, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { answerDecision } from '../../client/decision-prompts.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  INBOX_CHAT_EXPANDED_PREVIEWS,
  INBOX_PAGE_PREVIEWS,
  INBOX_SUMMARY_API,
  INBOX_SUMMARY_CHANGED_EVENT,
  inboxSummaryStore,
  type InboxApprovalPreview,
  type InboxCategory,
  type InboxCategoryKey,
  type InboxChatFilter,
  type InboxChatPreview,
  type InboxDecisionPreview,
  type InboxSummaryResponse,
} from '../../client/inbox-summary.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import {
  PENDING_SECRETS_API,
  PENDING_SECRETS_CHANGED_EVENT,
} from '../../client/pending-secrets.js';
import {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  dispatchProfileDecisionsCountChanged,
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
import { formatRelative } from '../../utils/time.js';
import { showToast } from '../../utils/toast.js';
import { inboxTabStripStyles, renderInboxTabStrip } from '../shared/inbox-tabs.js';
import './not-found.js';

/** Fixed display order for category cards on `/profile/inbox` (design §6.3). */
const INBOX_CARD_ORDER: readonly InboxCategoryKey[] = [
  'decisions',
  'waiting',
  'approvals',
  'reviews',
  'chat',
];

function parseChatFilterFromSearch(search: string): InboxChatFilter {
  const raw = new URLSearchParams(search).get('chat')?.trim();
  if (raw === 'dms' || raw === 'mentions') return raw;
  return 'all';
}

function createIdempotencyKey(prefix: string): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

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

function formatRelativeAge(iso: string): string {
  if (!iso) return '';
  const ts = Date.parse(iso);
  if (Number.isNaN(ts)) return '';
  return formatRelative(iso, { style: 'narrow' });
}

interface InboxCardOptions<T> {
  key: InboxCategoryKey;
  icon: string;
  title: string;
  category: InboxCategory<T>;
  defaultHref: string;
  countLabel?: string;
  headerExtra?: TemplateResult | typeof nothing;
  onRetry?: () => void;
  renderBody: (category: InboxCategory<T>) => TemplateResult | typeof nothing;
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
  private busyChoiceKey: string | null = null;

  @state()
  private confirmApproval: InboxApprovalPreview | null = null;

  /** Page-local chat filter (`?chat=all|dms|mentions`). */
  @state()
  private chatFilter: InboxChatFilter = 'all';

  /** Whether the chat card is expanded to show up to 10 previews. */
  @state()
  private chatExpanded = false;

  /** Page-local chat category payload when filtered (`chat !== 'all'`) or expanded (`previews=10`). */
  @state()
  private chatOverride: InboxCategory<InboxChatPreview> | null = null;

  /** Filter and preview cap that `chatOverride` was fetched for. */
  @state()
  private chatOverrideMeta: { filter: InboxChatFilter; previews: number } | null = null;

  @state()
  private chatLoading = false;

  @state()
  private chatError = false;

  /** Mirrors `<scion-profile-nav>`'s 404 probe state for the Decisions sub-tab. */
  @state()
  private decisionsNavVisibility: ProfileDecisionVisibility = 'unknown';

  private unmountPage: (() => void) | null = null;
  private chatAbortController: AbortController | null = null;
  private chatFetchSeq = 0;

  private boundOnSummaryChanged = (): void => {
    const prevSummary = this.summary;
    this.syncFromStore();
    if (
      this.summary &&
      this.summary !== prevSummary &&
      (this.chatFilter !== 'all' || this.chatExpanded)
    ) {
      void this.fetchChatView();
    }
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
        align-items: start;
      }

      .inbox-card {
        display: flex;
        flex-direction: column;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .inbox-card--compact .card-body {
        padding: 0.625rem 1rem;
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

      .card-subbar {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.375rem;
        padding: 0.5rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .chat-chip {
        appearance: none;
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.25rem 0.625rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-full, 9999px);
        background: var(--scion-surface, #ffffff);
        color: var(--scion-text-muted, #64748b);
        font-family: inherit;
        font-size: 0.75rem;
        font-weight: 500;
        line-height: 1.25;
        cursor: pointer;
        transition:
          background-color 0.12s ease,
          border-color 0.12s ease,
          color 0.12s ease;
      }

      .chat-chip:hover {
        color: var(--scion-text, #1e293b);
        border-color: var(--scion-primary, #3b82f6);
      }

      .chat-chip--active {
        background: var(--scion-primary, #3b82f6);
        border-color: var(--scion-primary, #3b82f6);
        color: var(--sl-color-neutral-0, #ffffff);
      }

      .chat-chip--active:hover {
        color: var(--sl-color-neutral-0, #ffffff);
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

      .card-loading-inline {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.25rem 0;
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      .card-footer-action {
        margin-top: 0.5rem;
        display: flex;
        justify-content: flex-start;
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

      .preview-row--stacked {
        flex-direction: column;
        align-items: stretch;
        gap: 0.5rem;
      }

      .preview-info {
        min-width: 0;
        flex: 1;
      }

      .preview-title-line {
        display: flex;
        align-items: center;
        gap: 0.375rem;
        min-width: 0;
      }

      .preview-title {
        display: inline-block;
        font-size: 0.875rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        text-decoration: none;
        word-break: break-word;
      }

      .preview-title:hover {
        color: var(--scion-primary, #3b82f6);
        text-decoration: underline;
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

      .preview-note {
        margin-top: 0.25rem;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .preview-actions {
        display: flex;
        align-items: center;
        gap: 0.375rem;
        flex-shrink: 0;
      }

      .decision-choices {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.375rem;
      }

      .decision-drill-link {
        font-size: 0.75rem;
        font-weight: 500;
        color: var(--scion-primary, #3b82f6);
        text-decoration: none;
        padding: 0.125rem 0.25rem;
      }

      .decision-drill-link:hover {
        text-decoration: underline;
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
    this.chatFilter = parseChatFilterFromSearch(window.location.search);
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
    if (this.chatFilter !== 'all' || this.chatExpanded) {
      void this.fetchChatView();
    }
  }

  private stopStore(): void {
    this.abortChatFetch();
    this.unmountPage?.();
    this.unmountPage = null;
  }

  private abortChatFetch(): void {
    if (this.chatAbortController) {
      this.chatAbortController.abort();
      this.chatAbortController = null;
    }
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

  private handleRefreshClick(): void {
    inboxSummaryStore.requestRefresh({ immediate: true, force: true });
    if (this.chatFilter !== 'all' || this.chatExpanded) {
      void this.fetchChatView();
    }
  }

  private selectChatFilter(filter: InboxChatFilter): void {
    if (filter === this.chatFilter && !this.chatError) return;
    this.chatFilter = filter;
    this.syncChatFilterToUrl(filter);
    this.chatOverride = null;
    this.chatOverrideMeta = null;
    if (filter === 'all' && !this.chatExpanded) {
      this.abortChatFetch();
      this.chatLoading = false;
      this.chatError = false;
      return;
    }
    void this.fetchChatView();
  }

  private toggleChatExpanded(expanded: boolean): void {
    if (expanded === this.chatExpanded) return;
    this.chatExpanded = expanded;
    this.chatOverride = null;
    this.chatOverrideMeta = null;
    if (!expanded && this.chatFilter === 'all') {
      this.abortChatFetch();
      this.chatLoading = false;
      this.chatError = false;
      return;
    }
    void this.fetchChatView();
  }

  private syncChatFilterToUrl(filter: InboxChatFilter): void {
    const params = new URLSearchParams(window.location.search);
    if (filter === 'all') {
      params.delete('chat');
    } else {
      params.set('chat', filter);
    }
    replaceSearch(params);
  }

  private async fetchChatView(): Promise<void> {
    if (!this.enabled) return;
    this.abortChatFetch();
    const controller = new AbortController();
    this.chatAbortController = controller;
    const seq = ++this.chatFetchSeq;
    const previews = this.chatExpanded ? INBOX_CHAT_EXPANDED_PREVIEWS : INBOX_PAGE_PREVIEWS;
    const filter = this.chatFilter;

    this.chatLoading = true;
    this.chatError = false;

    try {
      const url = `${INBOX_SUMMARY_API}?previews=${previews}&chat=${encodeURIComponent(filter)}`;
      const res = await apiFetch(url, { signal: controller.signal });
      if (seq !== this.chatFetchSeq || !this.isConnected) return;
      if (!res.ok) {
        this.chatError = true;
        return;
      }
      const data = (await res.json()) as InboxSummaryResponse;
      if (seq !== this.chatFetchSeq || !this.isConnected) return;
      if (data?.categories?.chat) {
        this.chatOverride = data.categories.chat;
        this.chatOverrideMeta = { filter, previews };
      }
    } catch (err) {
      if (seq !== this.chatFetchSeq || !this.isConnected) return;
      if (err instanceof DOMException && err.name === 'AbortError') return;
      this.chatError = true;
    } finally {
      if (seq === this.chatFetchSeq) {
        if (this.isConnected) {
          this.chatLoading = false;
        }
        if (this.chatAbortController === controller) {
          this.chatAbortController = null;
        }
      }
    }
  }

  private async answerDecisionInline(
    item: InboxDecisionPreview,
    choiceId: string,
    choiceLabel: string
  ): Promise<void> {
    const inline = item.inline;
    if (!inline || this.busyIds.has(item.id)) return;
    const choiceKey = `${item.id}:${choiceId}`;
    this.setBusy(item.id, true);
    this.busyChoiceKey = choiceKey;
    try {
      const result = await answerDecision(
        inline.decisionRowId,
        choiceId,
        createIdempotencyKey('inbox-decision'),
        'inbox'
      );
      if (result.ok) {
        const nextCount = inboxSummaryStore.optimisticRemoveDecision(item.id);
        if (nextCount !== null) {
          dispatchProfileDecisionsCountChanged(nextCount);
        }
        showToast(`Answered "${choiceLabel}"`, 'success');
        return;
      }
      if (result.status === 409 && result.code === 'batch_required') {
        showToast('This prompt requires answering all decisions together.', 'neutral');
        inboxSummaryStore.requestRefresh({ immediate: true, force: true });
        navigateTo(item.href || '/profile/decisions?tab=pending');
        return;
      }
      if (
        result.status === 409 &&
        (result.code === 'already_answered' || result.code === 'replaced')
      ) {
        showToast(result.message, 'neutral');
        inboxSummaryStore.requestRefresh({ immediate: true, force: true });
        return;
      }
      showToast(result.message, 'danger');
    } finally {
      this.setBusy(item.id, false);
      if (this.busyChoiceKey === choiceKey) {
        this.busyChoiceKey = null;
      }
    }
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

  private renderInboxCard<T>(opts: InboxCardOptions<T>): TemplateResult | typeof nothing {
    const cat = opts.category;
    if (cat.state === 'disabled' || cat.state === 'unavailable') return nothing;

    const countText = opts.countLabel ?? (cat.capped ? `${cat.count}+` : String(cat.count));
    const isCompact = cat.state === 'ok' && cat.count === 0;
    const onRetry =
      opts.onRetry ??
      ((): void => inboxSummaryStore.requestRefresh({ immediate: true, force: true }));

    return html`
      <section
        class="inbox-card ${isCompact ? 'inbox-card--compact' : ''}"
        data-inbox-card=${opts.key}
      >
        <header class="card-header">
          <div class="card-title-group">
            <sl-icon name=${opts.icon}></sl-icon>
            <h2 class="card-title">${opts.title}</h2>
            ${cat.state === 'ok'
              ? html`<sl-badge variant=${cat.count > 0 ? 'primary' : 'neutral'} pill data-card-count
                  >${countText}</sl-badge
                >`
              : nothing}
          </div>
          <a href=${cat.href || opts.defaultHref} class="card-view-all" data-card-view-all>
            View all
            <sl-icon name="arrow-right"></sl-icon>
          </a>
        </header>
        ${opts.headerExtra ?? nothing}
        <div class="card-body">
          ${cat.state === 'error'
            ? html`
                <div class="card-error" data-card-error>
                  <span>Couldn't load</span>
                  <sl-button size="small" variant="default" @click=${onRetry}>Retry</sl-button>
                </div>
              `
            : cat.count === 0
              ? html`<p class="card-empty" data-card-empty>Nothing waiting</p>`
              : opts.renderBody(cat)}
        </div>
      </section>
    `;
  }

  private renderDecisionsCard(
    cat: InboxCategory<InboxDecisionPreview>
  ): TemplateResult | typeof nothing {
    return this.renderInboxCard({
      key: 'decisions',
      icon: 'signpost-split',
      title: 'Decisions',
      category: cat,
      defaultHref: '/profile/decisions?tab=pending',
      renderBody: (c) => html`
        <ul class="preview-list">
          ${c.previews.map((item) => this.renderDecisionRow(item))}
        </ul>
      `,
    });
  }

  private renderDecisionRow(item: InboxDecisionPreview): TemplateResult {
    const busy = this.busyIds.has(item.id);
    const href = item.href || '/profile/decisions?tab=pending';
    const age = formatRelativeAge(item.createdAt);
    const inline = item.inline;

    return html`
      <li class="preview-row preview-row--stacked" data-decision-row=${item.id}>
        <div class="preview-info">
          <a href=${href} class="preview-title" data-decision-link>
            ${item.question || 'Pending decision'}
          </a>
          <div class="preview-meta">
            ${item.askerAgentName ? html`<span>by ${item.askerAgentName}</span>` : nothing}
            ${item.projectName ? html`<span>· ${item.projectName}</span>` : nothing}
            ${item.conversationLabel ? html`<span>· ${item.conversationLabel}</span>` : nothing}
            ${age ? html`<span>· ${age}</span>` : nothing}
            ${item.decisionCount > 1
              ? html`<sl-badge variant="neutral">${item.decisionCount} decisions</sl-badge>`
              : nothing}
          </div>
          ${!item.canAnswer && item.disabledReason
            ? html`<div class="preview-note" data-decision-disabled-reason>
                ${item.disabledReason}
              </div>`
            : nothing}
        </div>
        ${inline
          ? html`
              <div
                class="decision-choices"
                role="group"
                aria-label=${item.question || 'Pending decision'}
                data-decision-inline=${item.id}
              >
                ${inline.choices.map((choice) => {
                  const key = `${item.id}:${choice.id}`;
                  return html`
                    <sl-button
                      size="small"
                      variant="default"
                      ?loading=${this.busyChoiceKey === key}
                      ?disabled=${busy}
                      data-decision-choice-btn=${choice.id}
                      @click=${(): void =>
                        void this.answerDecisionInline(item, choice.id, choice.label)}
                    >
                      ${choice.label}
                    </sl-button>
                  `;
                })}
                ${inline.allowsOther
                  ? html`
                      <a
                        href=${href}
                        class="decision-drill-link"
                        data-decision-other-link=${item.id}
                      >
                        Other…
                      </a>
                    `
                  : nothing}
              </div>
            `
          : html`
              <div class="decision-choices">
                <a href=${href} class="decision-drill-link" data-decision-open-link=${item.id}>
                  ${item.canAnswer ? 'Review & answer' : 'View in chat'}
                </a>
              </div>
            `}
      </li>
    `;
  }

  private renderApprovalsCard(
    cat: InboxCategory<InboxApprovalPreview>
  ): TemplateResult | typeof nothing {
    return this.renderInboxCard({
      key: 'approvals',
      icon: 'shield-check',
      title: 'Approvals',
      category: cat,
      defaultHref: '/profile/secrets',
      renderBody: (c) => html`
        <ul class="preview-list">
          ${c.previews.map((item) => this.renderApprovalRow(item))}
        </ul>
      `,
    });
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

  private renderChatCard(
    baseCat: InboxCategory<InboxChatPreview>
  ): TemplateResult | typeof nothing {
    const usingPageLocal = this.chatFilter !== 'all' || this.chatExpanded;
    const desiredPreviews = this.chatExpanded ? INBOX_CHAT_EXPANDED_PREVIEWS : INBOX_PAGE_PREVIEWS;
    const hasMatchingOverride =
      this.chatOverride !== null &&
      this.chatOverrideMeta?.filter === this.chatFilter &&
      this.chatOverrideMeta?.previews === desiredPreviews;

    const effectiveCat: InboxCategory<InboxChatPreview> =
      usingPageLocal && this.chatError
        ? { ...baseCat, state: 'error', previews: [] }
        : usingPageLocal
          ? hasMatchingOverride
            ? this.chatOverride!
            : { ...baseCat, previews: [] }
          : baseCat;

    const breakdown = effectiveCat.breakdown ?? baseCat.breakdown;
    const allCount = breakdown?.unreadConversations ?? effectiveCat.count;
    const dmsCount = breakdown?.dms ?? 0;
    const mentionsCount = breakdown?.mentions ?? 0;
    const countLabel = effectiveCat.capped
      ? `${effectiveCat.count}+ unread`
      : `${effectiveCat.count} unread`;

    const activeFilterCount =
      this.chatFilter === 'dms'
        ? dmsCount
        : this.chatFilter === 'mentions'
          ? mentionsCount
          : allCount;

    const chips =
      baseCat.state === 'ok' && baseCat.count > 0
        ? html`
            <div class="card-subbar" role="group" aria-label="Filter unread chat" data-chat-chips>
              ${(
                [
                  { id: 'all', label: `All (${allCount})` },
                  { id: 'dms', label: `DMs (${dmsCount})` },
                  { id: 'mentions', label: `Mentions (${mentionsCount})` },
                ] as const
              ).map((chip) => {
                const active = this.chatFilter === chip.id;
                return html`
                  <button
                    type="button"
                    class="chat-chip ${active ? 'chat-chip--active' : ''}"
                    aria-pressed=${active ? 'true' : 'false'}
                    data-chat-chip=${chip.id}
                    @click=${(): void => this.selectChatFilter(chip.id)}
                  >
                    ${chip.label}
                  </button>
                `;
              })}
            </div>
          `
        : nothing;

    const expandFooter =
      activeFilterCount > INBOX_PAGE_PREVIEWS || this.chatExpanded
        ? html`
            <div class="card-footer-action">
              ${this.chatExpanded
                ? html`
                    <sl-button
                      size="small"
                      variant="text"
                      ?loading=${this.chatLoading}
                      data-chat-collapse-btn
                      @click=${(): void => this.toggleChatExpanded(false)}
                    >
                      Show fewer
                    </sl-button>
                  `
                : html`
                    <sl-button
                      size="small"
                      variant="text"
                      ?loading=${this.chatLoading}
                      data-chat-expand-btn
                      @click=${(): void => this.toggleChatExpanded(true)}
                    >
                      Show more
                    </sl-button>
                  `}
            </div>
          `
        : nothing;

    return this.renderInboxCard({
      key: 'chat',
      icon: 'chat-dots',
      title: 'Chat',
      category: effectiveCat,
      defaultHref: '/chat',
      countLabel,
      headerExtra: chips,
      onRetry: (): void => {
        if (usingPageLocal) {
          void this.fetchChatView();
        }
        inboxSummaryStore.requestRefresh({ immediate: true, force: true });
      },
      renderBody: (c) => {
        if (this.chatLoading && c.previews.length === 0) {
          return html`
            <div class="card-loading-inline" data-chat-loading>
              <sl-spinner></sl-spinner>
              <span>Loading conversations…</span>
            </div>
            ${this.chatExpanded ? expandFooter : nothing}
          `;
        }
        if (c.previews.length === 0) {
          const emptyText =
            this.chatFilter === 'dms'
              ? 'No unread DMs'
              : this.chatFilter === 'mentions'
                ? 'No unread mentions'
                : 'Nothing waiting';
          return html`
            <p class="card-empty" data-chat-filter-empty>${emptyText}</p>
            ${this.chatExpanded ? expandFooter : nothing}
          `;
        }
        return html`
          <ul class="preview-list">
            ${c.previews.map((item) => this.renderChatRow(item))}
          </ul>
          ${expandFooter}
        `;
      },
    });
  }

  private renderChatRow(item: InboxChatPreview): TemplateResult {
    const href = item.href || '/chat';
    const age = formatRelativeAge(item.lastMessageAt || item.createdAt);
    const kindBadge =
      item.kind === 'dm'
        ? html`<sl-badge variant="primary" data-chat-kind="dm">DM</sl-badge>`
        : item.kind === 'mention'
          ? html`<sl-badge variant="warning" data-chat-kind="mention">@mention</sl-badge>`
          : html`<sl-badge variant="neutral" data-chat-kind="thread">Thread</sl-badge>`;

    return html`
      <li class="preview-row" data-chat-row=${item.id}>
        <div class="preview-info">
          <div class="preview-title-line">
            ${kindBadge}
            <a href=${href} class="preview-title" data-chat-link>${item.label}</a>
          </div>
          <div class="preview-meta">
            ${item.projectName ? html`<span>${item.projectName}</span>` : nothing}
            ${item.projectName && age ? html`<span>·</span>` : nothing}
            ${age ? html`<span>${age}</span>` : nothing}
          </div>
        </div>
      </li>
    `;
  }

  private renderCategoryCard(
    key: InboxCategoryKey,
    categories: InboxSummaryResponse['categories']
  ): TemplateResult | typeof nothing {
    switch (key) {
      case 'decisions':
        return categories.decisions ? this.renderDecisionsCard(categories.decisions) : nothing;
      case 'waiting':
        // Rendered in Phase 6b.
        return nothing;
      case 'approvals':
        return categories.approvals ? this.renderApprovalsCard(categories.approvals) : nothing;
      case 'reviews':
        // Rendered in Phase 6b.
        return nothing;
      case 'chat':
        return categories.chat ? this.renderChatCard(categories.chat) : nothing;
    }
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
      ? INBOX_CARD_ORDER.map((k) => categories[k]).filter(
          (c) => c && c.state !== 'disabled' && c.state !== 'unavailable'
        )
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
          @click=${(): void => this.handleRefreshClick()}
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
                  @click=${(): void => this.handleRefreshClick()}
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
                ${categories
                  ? INBOX_CARD_ORDER.map((key) => this.renderCategoryCard(key, categories))
                  : nothing}
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
