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
 * Shared styles and Lit renderers for `<scion-page-project-decisions>` and
 * `<scion-page-profile-decisions>` (Design §4.3 R7, ptone/scion#7996).
 *
 * Renders into the host page's own shadow root so both surfaces share one
 * component tree, styling, and accessibility structure without adding an extra
 * shadow boundary.
 */

import { css, html, nothing, type CSSResult, type TemplateResult } from 'lit';
import { repeat } from 'lit/directives/repeat.js';

import {
  dismissAvailableAtMsForDecisions,
  isOtherAnswer,
  type DecisionAnsweredDetail,
} from '../../client/decision-prompts.js';
import type {
  ProjectDecisionCounts,
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import { formatInstant, formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { dismissDialogStyles } from './chat/chat-decisions.js';
import {
  MAX_BULK_DISMISS_SELECTION,
  MAX_DISMISS_REASON_RUNES,
  avatarColor,
  avatarInitial,
  clampDismissReason,
  conversationDisplayLabel,
  conversationHref,
  formatDecisionLogWhen,
  promptAnswerability,
  promptWaitingOnViewerRows,
  replyHrefForEntry,
  surfaceDisplay,
  type WaitingOnOthersItem,
} from './decisions-format.js';

export const decisionsPageSharedStyles: CSSResult = css`
  ${dismissDialogStyles}

  :host {
    display: block;
    --chat-fs-sm: 0.875rem;
    --chat-fs-base: 0.9375rem;
    --chat-fs-lg: 1.0625rem;
  }

  .ph {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    flex-wrap: wrap;
  }

  .ph h1 {
    margin: 0;
    font-size: 1.5rem;
    font-weight: 700;
    color: var(--scion-text, #1e293b);
  }

  .sub {
    color: var(--scion-text-secondary, #475569);
    font-size: 0.875rem;
    margin: 0.25rem 0 1rem;
  }

  sl-tab sl-badge::part(base) {
    font-size: 0.68rem;
    margin-left: 0.35rem;
  }

  sl-tab-panel::part(base) {
    padding: 0.75rem 0 0;
  }

  .loading-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    padding: 4rem 2rem;
    color: var(--scion-text-muted, #64748b);
  }

  .loading-state sl-spinner {
    font-size: 2rem;
    margin-bottom: 1rem;
  }

  .error-state {
    text-align: center;
    padding: 3rem 2rem;
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--sl-color-danger-200, #fecaca);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  .error-state sl-icon {
    font-size: 2.5rem;
    color: var(--sl-color-danger-500, #ef4444);
    margin-bottom: 0.75rem;
  }

  .error-state h2 {
    font-size: 1.2rem;
    font-weight: 600;
    color: var(--scion-text, #1e293b);
    margin: 0 0 0.5rem;
  }

  .error-details {
    font-family: var(--scion-font-mono, monospace);
    font-size: 0.85rem;
    background: var(--scion-bg-subtle, #f1f5f9);
    padding: 0.65rem 0.9rem;
    border-radius: var(--scion-radius, 0.5rem);
    color: var(--sl-color-danger-700, #b91c1c);
    margin-bottom: 1rem;
  }

  /* ---- Prompt cards (Mocks P3-2, R-1, R-2) ---- */
  .pcard {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    margin: 0.75rem 0;
  }

  .pcard.sel {
    border-color: var(--scion-primary, #3b82f6);
    box-shadow: 0 0 0 1px var(--scion-primary, #3b82f6);
  }

  .pcard-head sl-checkbox.card-select {
    margin-right: 0.1rem;
  }

  .pcard-head sl-checkbox.card-select::part(label) {
    margin-inline-start: 0;
  }

  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
    clip-path: inset(50%);
    white-space: nowrap;
    border: 0;
  }

  .select-all-row {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.35rem 0.75rem;
    margin: 0.35rem 0;
    font-size: 0.875rem;
    color: var(--scion-text-secondary, #475569);
  }

  .selbar {
    position: sticky;
    top: 0;
    z-index: 2;
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.5rem 0.75rem;
    margin: 0.5rem 0;
    background: var(--scion-primary-50, rgba(59, 130, 246, 0.12));
    border: 1px solid var(--scion-primary, #3b82f6);
    border-radius: var(--scion-radius-lg, 0.75rem);
    font-size: 0.875rem;
    color: var(--scion-text, #1e293b);
    flex-wrap: wrap;
  }

  .selbar .spacer {
    flex: 1;
  }

  .bulk-dismiss-list {
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
    max-height: 14rem;
    overflow-y: auto;
    padding: 0.45rem 0.55rem;
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius, 0.5rem);
    background: var(--scion-bg-subtle, #f8fafc);
  }

  .bulk-dismiss-item {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text, #1e293b);
    overflow-wrap: anywhere;
  }

  .bulk-dismiss-item + .bulk-dismiss-item {
    border-top: 1px solid var(--scion-border, #e2e8f0);
    padding-top: 0.45rem;
  }

  .bulk-dismiss-item-meta {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    flex-wrap: wrap;
    font-size: 0.8125rem;
    color: var(--scion-text-secondary, #475569);
  }

  .bulk-dismiss-item-meta b {
    color: var(--scion-text, #1e293b);
  }

  .bulk-dismiss-item-q {
    display: flex;
    align-items: baseline;
    gap: 0.4rem;
  }

  .bulk-dismiss-progress {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }

  .bulk-dismiss-progress-label {
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary, #475569);
  }

  .pcard-head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.55rem 0.75rem;
    font-size: var(--chat-fs-sm, 0.875rem);
    color: var(--scion-text-secondary, #475569);
    flex-wrap: wrap;
  }

  .pcard-head sl-badge::part(base) {
    font-size: 0.72rem;
    background: var(--scion-badge-neutral-bg, #f1f5f9);
    color: var(--scion-badge-neutral-text, #334155);
    border-color: var(--scion-border, #e2e8f0);
  }

  .pcard-head .avatar {
    width: 1.5rem;
    height: 1.5rem;
    border-radius: 50%;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 0.72rem;
    font-weight: 700;
    color: #ffffff;
    flex-shrink: 0;
  }

  .pcard-head b {
    color: var(--scion-text, #1e293b);
  }

  .pcard-head .right {
    margin-left: auto;
    display: inline-flex;
    gap: 0.4rem;
    align-items: center;
  }

  .pcard scion-chat-decisions {
    margin: 0 0.75rem 0.75rem;
  }

  .linkish {
    color: var(--scion-primary, #3b82f6);
    text-decoration: none;
    font-weight: 500;
    cursor: pointer;
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
  }

  .linkish:hover {
    text-decoration: underline;
  }

  .purged-note {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
    color: var(--scion-text-muted, #64748b);
  }

  .muted {
    color: var(--scion-text-muted, #64748b);
  }

  .key {
    font: 600 0.75rem/1 var(--scion-font-mono, monospace);
    background: var(--scion-badge-neutral-bg, #f1f5f9);
    color: var(--scion-badge-neutral-text, #334155);
    padding: 0.2rem 0.4rem;
    border-radius: var(--scion-radius-sm);
    white-space: nowrap;
  }

  .badge {
    display: inline-flex;
    align-items: center;
    gap: 0.2rem;
    font-size: 0.68rem;
    font-weight: 600;
    line-height: 1;
    padding: 0.2rem 0.45rem;
    border-radius: 999px;
    background: var(--scion-badge-neutral-bg, #f1f5f9);
    color: var(--scion-badge-neutral-text, #334155);
    white-space: nowrap;
  }

  .badge.interp {
    background: var(--scion-badge-warning-bg, #fef3c7);
    color: var(--scion-badge-warning-text, #92400e);
  }

  .badge.conf {
    background: var(--scion-badge-success-bg, #dcfce7);
    color: var(--scion-badge-success-text, #166534);
  }

  .badge.over {
    background: var(--scion-badge-primary-bg, #dbeafe);
    color: var(--scion-badge-primary-text, #1e40af);
  }

  .badge.dec {
    background: var(--scion-badge-danger-bg, #fee2e2);
    color: var(--scion-badge-danger-text, #991b1b);
  }

  .dismiss-reason {
    font-size: 0.78rem;
    margin-top: 0.15rem;
    overflow-wrap: anywhere;
  }

  .badge sl-icon {
    font-size: 0.8rem;
  }

  /* ---- Waiting on others (Mocks P3-2, R-1) ---- */
  sl-details.others {
    margin-top: 1rem;
  }

  sl-details.others::part(base) {
    background: var(--scion-surface, #ffffff);
    border-color: var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
  }

  sl-details.others::part(header) {
    font-weight: 600;
    color: var(--scion-text, #1e293b);
  }

  /* ---- Stacked list (.mlist: Waiting on others, Made groups & Mobile Made) ---- */
  .mlist .item {
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius, 0.5rem);
    padding: 0.55rem 0.65rem;
    margin: 0.5rem 0;
    font-size: 0.85rem;
    color: var(--scion-text, #1e293b);
  }

  .mlist .item .top {
    display: flex;
    gap: 0.4rem;
    align-items: center;
    flex-wrap: wrap;
    margin-bottom: 0.15rem;
  }

  .mlist .item .meta {
    color: var(--scion-text-muted, #64748b);
    font-size: 0.78rem;
    margin-top: 0.25rem;
  }

  /* ---- Made validation, timeout alert & table (Mocks P3-3, F-1, F-5, R-3) ---- */
  sl-alert[data-search-timeout] {
    display: block;
    margin: 0.75rem 0;
  }

  .date-range-error {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.55rem 0.75rem;
    margin: 0 0 0.75rem;
    font-size: 0.85rem;
    color: var(--sl-color-danger-700, #b91c1c);
    background: var(--sl-color-danger-50, #fef2f2);
    border: 1px solid var(--sl-color-danger-200, #fecaca);
    border-radius: var(--scion-radius, 0.5rem);
  }

  .date-range-error sl-icon {
    color: var(--sl-color-danger-600, #dc2626);
    flex-shrink: 0;
  }

  table.log {
    width: 100%;
    border-collapse: collapse;
    background: var(--scion-surface, #ffffff);
    border: 1px solid var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    overflow: hidden;
    font-size: 0.85rem;
    color: var(--scion-text, #1e293b);
  }

  table.log th {
    text-align: left;
    font-weight: 600;
    font-size: 0.75rem;
    color: var(--scion-text-secondary, #475569);
    background: var(--scion-bg-subtle, #f1f5f9);
    padding: 0.5rem 0.6rem;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
    white-space: nowrap;
  }

  table.log td {
    padding: 0.5rem 0.6rem;
    border-bottom: 1px solid var(--scion-border, #e2e8f0);
    vertical-align: top;
  }

  table.log tbody tr:last-child td {
    border-bottom: none;
  }

  table.log tr.follow td {
    background: var(--scion-bg, #f8fafc);
  }

  table.log td.q {
    max-width: 18rem;
    font-weight: 400;
  }

  table.log td.q .q-text {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
    overflow-wrap: anywhere;
  }

  table.log td.ans {
    max-width: 22rem;
  }

  table.log td.ans .ans-clamp {
    display: -webkit-box;
    -webkit-line-clamp: 2;
    -webkit-box-orient: vertical;
    overflow: hidden;
    overflow-wrap: anywhere;
  }

  .mlist .item .ans-other {
    overflow-wrap: anywhere;
  }

  .surf {
    display: inline-flex;
    gap: 0.3rem;
    align-items: center;
    color: var(--scion-text-secondary, #475569);
    font-size: 0.8rem;
    white-space: nowrap;
  }

  .made-mobile-list {
    display: none;
  }

  .more {
    display: flex;
    justify-content: center;
    margin: 0.85rem 0;
  }

  .more sl-button::part(base) {
    background: var(--scion-surface, #ffffff);
    border-color: var(--scion-border, #e2e8f0);
    color: var(--scion-text, #1e293b);
  }

  .more sl-button::part(base):hover {
    background: var(--scion-bg-subtle, #f1f5f9);
    border-color: var(--scion-border, #e2e8f0);
    color: var(--scion-text, #1e293b);
  }

  /* ---- Empty states (Mock P3-6) ---- */
  .empty {
    text-align: center;
    padding: 2.5rem 1rem;
    color: var(--scion-text-secondary, #475569);
    background: var(--scion-surface, #ffffff);
    border: 1px dashed var(--scion-border, #e2e8f0);
    border-radius: var(--scion-radius-lg, 0.75rem);
    margin: 0.5rem 0;
  }

  .empty sl-icon {
    font-size: 2rem;
    color: var(--scion-text-muted, #64748b);
  }

  .empty p {
    margin: 0.4rem 0 0;
  }

  .empty p b {
    color: var(--scion-text, #1e293b);
  }

  .dismiss-tip-wrap {
    display: inline-flex;
  }

  /* ---- Made detail panel: offered choices (ptone/scion#8443) ---- */
  .made-disc {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 1.5rem;
    height: 1.5rem;
    margin-inline-end: 0.25rem;
    padding: 0;
    border: 1px solid transparent;
    border-radius: var(--scion-radius-sm);
    background: none;
    color: var(--scion-text-secondary, #475569);
    cursor: pointer;
    vertical-align: middle;
  }

  .made-disc:hover {
    background: var(--scion-bg-subtle, #f1f5f9);
  }

  .made-disc:focus-visible {
    outline: 2px solid var(--scion-primary, #3b82f6);
    outline-offset: 1px;
  }

  .rec-marker {
    font-size: 0.78rem;
    overflow-wrap: anywhere;
  }

  .badge.await {
    background: var(--scion-badge-warning-bg, #fef3c7);
    color: var(--scion-badge-warning-text, #92400e);
  }

  table.log tr.detail-row td {
    background: var(--scion-bg, #f8fafc);
    padding-top: 0.25rem;
  }

  .made-detail {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    font-size: 0.85rem;
    color: var(--scion-text, #1e293b);
  }

  .mlist .item .made-detail {
    margin-top: 0.45rem;
    padding-top: 0.45rem;
    border-top: 1px solid var(--scion-border, #e2e8f0);
  }

  .made-choices {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }

  .made-choice {
    display: grid;
    grid-template-columns: 1.1rem 1fr;
    gap: 0.4rem;
    align-items: start;
    overflow-wrap: anywhere;
  }

  .made-choice .pick {
    color: var(--scion-primary, #3b82f6);
    padding-top: 0.1rem;
  }

  .made-choice .desc {
    display: block;
    color: var(--scion-text-secondary, #475569);
    font-size: 0.8rem;
  }

  .made-choice .other-text {
    display: block;
    overflow-wrap: anywhere;
  }

  .badge.rec {
    margin-inline-start: 0.35rem;
    vertical-align: middle;
  }

  .made-detail-notes {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem 0.9rem;
    align-items: center;
    color: var(--scion-text-secondary, #475569);
    font-size: 0.8rem;
  }

  .made-detail-notes .dismiss-reason {
    margin-top: 0;
  }

  @media (max-width: 640px) {
    .ph h1 {
      font-size: 1.2rem;
    }

    table.log {
      display: none;
    }

    .made-mobile-list {
      display: block;
    }
  }
`;

export function renderDecisionsLoadingState(label = 'Loading decisions...'): TemplateResult {
  return html`
    <div class="loading-state">
      <sl-spinner></sl-spinner>
      <p>${label}</p>
    </div>
  `;
}

export function renderDecisionsErrorState(opts: {
  heading: string;
  message: string;
  retryAttr: 'data-retry-pending' | 'data-retry-made';
  showPrefixIcon?: boolean | undefined;
  onRetry: () => void;
}): TemplateResult {
  return html`
    <div class="error-state">
      <sl-icon name="exclamation-triangle"></sl-icon>
      <h2>${opts.heading}</h2>
      <div class="error-details">${opts.message}</div>
      ${opts.retryAttr === 'data-retry-pending'
        ? html`
            <sl-button variant="primary" data-retry-pending @click=${opts.onRetry}>
              ${opts.showPrefixIcon
                ? html`<sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>`
                : nothing}
              Retry
            </sl-button>
          `
        : html`
            <sl-button variant="primary" data-retry-made @click=${opts.onRetry}> Retry </sl-button>
          `}
    </div>
  `;
}

export function renderWaitingTabBadge(opts: {
  activeTab: 'pending' | 'made';
  isPendingFiltered: boolean;
  counts: ProjectDecisionCounts;
  filteredCounts: ProjectDecisionCounts | null;
}): TemplateResult | typeof nothing {
  if (
    opts.activeTab === 'pending' &&
    opts.isPendingFiltered &&
    opts.filteredCounts !== null &&
    (opts.counts.waitingOnYou > 0 || opts.filteredCounts.waitingOnYou > 0)
  ) {
    return html`<sl-badge variant="primary" pill data-waiting-badge
      >${opts.filteredCounts.waitingOnYou} of ${opts.counts.waitingOnYou}</sl-badge
    >`;
  }
  if (opts.counts.waitingOnYou > 0) {
    return html`<sl-badge variant="primary" pill data-waiting-badge
      >${opts.counts.waitingOnYou}</sl-badge
    >`;
  }
  return nothing;
}

export function renderSearchTimeoutAlert(scope: 'project' | 'profile'): TemplateResult {
  const hint =
    scope === 'profile'
      ? 'Search took too long; narrow it with a project, conversation, agent, or date filter.'
      : 'Search took too long; narrow it with a conversation, agent, or date filter.';
  return html`
    <sl-alert variant="warning" open data-search-timeout>
      <sl-icon slot="icon" name="hourglass-split"></sl-icon>
      ${hint}
    </sl-alert>
  `;
}

export function renderNoMatchState(tab: 'pending' | 'made', onClear: () => void): TemplateResult {
  return html`
    <div class="empty" data-no-match=${tab}>
      <sl-icon name="search"></sl-icon>
      <p><b>No decisions match these filters</b></p>
      <p>
        <sl-button size="small" variant="text" data-clear-filters @click=${onClear}>
          Clear filters
        </sl-button>
      </p>
    </div>
  `;
}

export function renderPendingEmptyState(opts: {
  scope: 'project' | 'profile';
  seeMadeHref: string;
  onSeeMade: () => void;
}): TemplateResult {
  const body =
    opts.scope === 'profile'
      ? 'When an agent asks you to choose across your projects, it shows up here.'
      : 'When an agent asks you to choose, it shows up here.';
  return html`
    <div class="empty" data-empty="pending">
      <sl-icon name="check2-circle"></sl-icon>
      <p><b>Nothing is waiting on you</b></p>
      <p>
        ${body}
        <a
          class="linkish"
          href=${opts.seeMadeHref}
          data-see-made
          @click=${(e: Event): void => {
            e.preventDefault();
            opts.onSeeMade();
          }}
          >See decisions made</a
        >
      </p>
    </div>
  `;
}

export function renderMadeEmptyState(scope: 'project' | 'profile'): TemplateResult {
  const body =
    scope === 'profile'
      ? "Answers to agents' decisions across your projects appear here."
      : "Answers to agents' decisions in this project's conversations appear here.";
  return html`
    <div class="empty" data-empty="made">
      <sl-icon name="journal-text"></sl-icon>
      <p><b>No decisions recorded yet</b></p>
      <p>${body}</p>
    </div>
  `;
}

export function renderDateRangeError(): TemplateResult {
  return html`
    <div class="date-range-error" role="alert" data-date-range-error>
      <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
      <span>Start date must be on or before end date.</span>
    </div>
  `;
}

export function renderLoadMoreFooter(opts: {
  tab: 'pending' | 'made';
  nextCursor: string;
  loadingMore: boolean;
  showEndFilteredNotice?: boolean | undefined;
  onLoadMore: () => void;
}): TemplateResult | typeof nothing {
  if (opts.nextCursor) {
    return html`
      <div class="more">
        <sl-button
          size="small"
          data-load-more=${opts.tab}
          ?loading=${opts.loadingMore}
          ?disabled=${opts.loadingMore}
          @click=${opts.onLoadMore}
        >
          Load more
        </sl-button>
      </div>
    `;
  }
  if (opts.showEndFilteredNotice) {
    return html`
      <div class="more">
        <span class="muted" style="font-size: 0.8rem">End of matching decisions</span>
      </div>
    `;
  }
  return nothing;
}

export function renderPendingSelectionHeader(opts: {
  selectedCount: number;
  selectableCount: number;
  submitting: boolean;
  onToggleAll: (checked: boolean) => void;
  onClear: () => void;
  onDismiss: () => void;
}): TemplateResult {
  const cap = Math.min(opts.selectableCount, MAX_BULK_DISMISS_SELECTION);
  const allSelected = cap > 0 && opts.selectedCount >= cap;
  const indeterminate = opts.selectedCount > 0 && !allSelected;
  const selectAllDisabled = cap === 0 || opts.submitting;

  const onSelectAllChange = (e: Event): void => {
    const box = e.currentTarget as HTMLElement & { checked?: boolean };
    const nextChecked = Boolean(box.checked) && !allSelected;
    opts.onToggleAll(nextChecked);
  };

  if (opts.selectedCount === 0) {
    return html`
      <div class="select-all-row">
        <sl-checkbox
          data-select-all-prompts
          ?checked=${false}
          .checked=${false}
          ?indeterminate=${false}
          .indeterminate=${false}
          ?disabled=${selectAllDisabled}
          @sl-change=${onSelectAllChange}
          >Select all on this page</sl-checkbox
        >
      </div>
    `;
  }

  return html`
    <div class="selbar" data-selection-bar>
      <sl-checkbox
        data-select-all-prompts
        ?checked=${allSelected}
        .checked=${allSelected}
        ?indeterminate=${indeterminate}
        .indeterminate=${indeterminate}
        ?disabled=${selectAllDisabled}
        @sl-change=${onSelectAllChange}
        >Select all on this page</sl-checkbox
      >
      <b data-selected-count
        >${opts.selectedCount === 1 ? '1 card selected' : `${opts.selectedCount} cards selected`}</b
      >
      <span class="spacer"></span>
      <sl-button
        size="small"
        variant="text"
        data-selection-clear
        ?disabled=${opts.submitting}
        @click=${opts.onClear}
        >Clear</sl-button
      >
      <sl-button
        size="small"
        variant="danger"
        outline
        data-selection-dismiss
        ?disabled=${opts.submitting}
        @click=${opts.onDismiss}
      >
        <sl-icon slot="prefix" name="x-circle" aria-hidden="true"></sl-icon>
        Dismiss…
      </sl-button>
    </div>
  `;
}

export function renderPromptCard(
  p: ProjectDecisionPromptView,
  opts: {
    currentUserId: string;
    defaultProjectId?: string | undefined;
    projectBadgeName?: string | undefined;
    askerGone?: boolean | undefined;
    dismissRemainingMin?: number | undefined;
    dismissAvailableAtMs?: number | undefined;
    selected?: boolean | undefined;
    canSelect?: boolean | undefined;
    selectionCapReached?: boolean | undefined;
    onToggleSelect?: ((p: ProjectDecisionPromptView, checked: boolean) => void) | undefined;
    onDismissTooRecent?:
      | ((
          p: ProjectDecisionPromptView,
          e: CustomEvent<{ messageId: string; availableAt: string }>
        ) => void)
      | undefined;
    onAnswered: (p: ProjectDecisionPromptView, e: CustomEvent<DecisionAnsweredDetail>) => void;
    onJump: (p: ProjectDecisionPromptView, e: CustomEvent<{ messageId?: string }>) => void;
  }
): TemplateResult {
  const projectId = opts.defaultProjectId || p.project?.id || '';
  const whereLabel = conversationDisplayLabel(p.chatKey, p.conversationLabel);
  const convHref = conversationHref(p.chatKey, p.conversationAvailable, projectId);
  const permalink =
    p.promptAvailable && convHref
      ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
      : null;
  const mode = p.answerMode === 'set' || p.answerMode === 'single' ? p.answerMode : undefined;
  const remainingMin = opts.dismissRemainingMin ?? 0;
  const availMs =
    opts.dismissAvailableAtMs ?? dismissAvailableAtMsForDecisions(p.decisions, p.createdAt);
  const isSelected = Boolean(opts.selected);
  const canDismissAnyRow =
    opts.canSelect ?? promptWaitingOnViewerRows(p, opts.currentUserId).length > 0;
  const capReached = Boolean(opts.selectionCapReached) && !isSelected;
  const checkboxDisabled = !canDismissAnyRow || remainingMin > 0 || capReached;
  const checkboxTooltip =
    canDismissAnyRow && remainingMin > 0
      ? `Dismiss available in ${remainingMin} min`
      : capReached
        ? `Select up to ${MAX_BULK_DISMISS_SELECTION} cards at once`
        : '';

  const renderSelectCheckbox = (): TemplateResult => {
    const box = html`
      <sl-checkbox
        class="card-select"
        data-select-prompt=${p.promptMessageId}
        ?checked=${isSelected}
        .checked=${isSelected}
        ?disabled=${checkboxDisabled}
        @sl-change=${(e: Event): void => {
          const target = e.currentTarget as HTMLElement & { checked?: boolean };
          if (checkboxDisabled) {
            target.checked = isSelected;
            return;
          }
          opts.onToggleSelect?.(p, Boolean(target.checked));
        }}
        ><span class="sr-only">Select decision card from ${p.askerAgentName}</span></sl-checkbox
      >
    `;
    if (checkboxTooltip) {
      return html`
        <sl-tooltip data-select-prompt-tooltip=${p.promptMessageId} content=${checkboxTooltip}>
          <span class="dismiss-tip-wrap">${box}</span>
        </sl-tooltip>
      `;
    }
    return box;
  };

  return html`
    <div
      class="pcard ${isSelected ? 'sel' : ''}"
      data-prompt-card=${p.promptMessageId}
      @decision-answered=${(e: CustomEvent<DecisionAnsweredDetail>): void => opts.onAnswered(p, e)}
      @decision-dismiss-too-recent=${(
        e: CustomEvent<{ messageId: string; availableAt: string }>
      ): void => opts.onDismissTooRecent?.(p, e)}
      @scroll-to-message=${(e: CustomEvent<{ messageId?: string }>): void => opts.onJump(p, e)}
      @decision-jump=${(e: CustomEvent<{ messageId?: string }>): void => opts.onJump(p, e)}
    >
      <div class="pcard-head">
        ${opts.onToggleSelect ? renderSelectCheckbox() : nothing}
        ${opts.projectBadgeName
          ? html`<sl-badge variant="neutral" data-card-project>${opts.projectBadgeName}</sl-badge>`
          : nothing}
        <span
          class="avatar"
          aria-hidden="true"
          style="background: ${avatarColor(p.askerAgentId || p.askerAgentName)}"
          >${avatarInitial(p.askerAgentName)}</span
        >
        <b>${p.askerAgentName}</b>
        ${opts.askerGone ? html`<span class="muted" data-asker-deleted>(deleted)</span>` : nothing}
        <span>
          in
          ${convHref
            ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
        </span>
        <span class="muted" title=${formatInstantWithZone(p.createdAt)}
          >· ${formatRelative(p.createdAt, { style: 'short' })}</span
        >
        ${canDismissAnyRow && remainingMin > 0 && availMs > 0
          ? html`<span class="muted" data-dismiss-available-at
              >${`· dismiss available at ${formatInstant(new Date(availMs).toISOString(), 'time')}`}</span
            >`
          : nothing}
        <span class="right">
          ${permalink
            ? html`<a class="linkish" data-open-in-chat href=${permalink}
                ><sl-icon name="chat-left-text" aria-hidden="true"></sl-icon> Open in chat</a
              >`
            : !p.promptAvailable
              ? html`<span class="purged-note" data-purged-note
                  ><sl-icon name="archive" aria-hidden="true"></sl-icon>
                  <span>Original message no longer available</span></span
                >`
              : nothing}
        </span>
      </div>
      <scion-chat-decisions
        .messageId=${p.promptMessageId}
        .decisions=${p.decisions}
        .currentUserId=${opts.currentUserId}
        .answerMode=${mode}
        .answerability=${promptAnswerability(p)}
        .createdAt=${p.createdAt}
        .conversationLabel=${whereLabel}
        .permalinkHref=${permalink ?? ''}
        .waitingReasons=${p.waitingReasons}
        .askerGone=${Boolean(opts.askerGone)}
        surface="decisions_page"
      ></scion-chat-decisions>
    </div>
  `;
}

export function renderWaitingOnOthersItem(
  item: WaitingOnOthersItem,
  opts: {
    defaultProjectId?: string | undefined;
    resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
  } = {}
): TemplateResult {
  const { prompt: p, decision: d, reason } = item;
  const projectId = opts.defaultProjectId || p.project?.id || '';
  const projectName = opts.resolveProjectName ? opts.resolveProjectName(p.project) : '';
  const whereLabel = conversationDisplayLabel(p.chatKey, p.conversationLabel);
  const convHref = conversationHref(p.chatKey, p.conversationAvailable, projectId);
  const permalink =
    p.promptAvailable && convHref
      ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
      : null;

  let reasonBadge: TemplateResult;
  if (reason === 'other_user_confirm') {
    const owner = d.decision?.answeredByName || 'Another user';
    reasonBadge = html`<span class="badge interp"
      ><sl-icon name="robot" aria-hidden="true"></sl-icon>${owner} to confirm</span
    >`;
  } else if (reason === 'read_only') {
    reasonBadge = html`<span class="badge">read-only for you</span>`;
  } else if (reason === 'message_denied') {
    reasonBadge = html`<span class="badge">messaging restricted</span>`;
  } else {
    reasonBadge = html`<span class="badge">not answerable by you</span>`;
  }

  return html`
    <div class="item" data-others-row=${d.rowId}>
      <div class="top">
        ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
        <b>${d.question}</b>
        ${reasonBadge}
      </div>
      <div class="meta">
        ${projectName ? html`${projectName} · ` : nothing}${p.askerAgentName} · ${whereLabel} ·
        ${formatRelative(p.createdAt, { style: 'short' })}
        ${permalink
          ? html` · <a class="linkish" href=${permalink}>Open in chat</a>`
          : !p.promptAvailable
            ? html` · Original message no longer available`
            : nothing}
      </div>
    </div>
  `;
}

export function renderWaitingOnOthersSection(opts: {
  items: readonly WaitingOnOthersItem[];
  counts: ProjectDecisionCounts;
  filteredCounts: ProjectDecisionCounts | null;
  isFiltered: boolean;
  hasWaitingPrompts: boolean;
  defaultProjectId?: string | undefined;
  resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
}): TemplateResult | typeof nothing {
  const showOthersSection =
    opts.items.length > 0 ||
    (opts.isFiltered &&
      opts.filteredCounts !== null &&
      opts.hasWaitingPrompts &&
      opts.counts.waitingOnOthers > 0);
  if (!showOthersSection) return nothing;

  const othersSummary =
    opts.isFiltered && opts.filteredCounts !== null
      ? `Waiting on others (${opts.filteredCounts.waitingOnOthers} of ${opts.counts.waitingOnOthers})`
      : `Waiting on others (${Math.max(opts.counts.waitingOnOthers, opts.items.length)})`;

  return html`
    <sl-details class="others" data-waiting-others summary=${othersSummary}>
      <div class="mlist">
        ${repeat(
          opts.items,
          (item) => item.decision.rowId,
          (item) =>
            renderWaitingOnOthersItem(item, {
              defaultProjectId: opts.defaultProjectId,
              resolveProjectName: opts.resolveProjectName,
            })
        )}
      </div>
    </sl-details>
  `;
}

export function renderAnsweredBy(
  e: ProjectDecisionLogEntryView,
  currentUserId: string
): TemplateResult | string {
  if (currentUserId && e.answeredByUserId === currentUserId) {
    return 'you';
  }
  if (!e.answeredByUserId) {
    return html`${e.answeredByName || 'Unknown'} <span class="muted">(deleted user)</span>`;
  }
  return e.answeredByName || 'Unknown';
}

function isDismissLogEntry(e: ProjectDecisionLogEntryView): boolean {
  return e.outcome === 'dismiss' || e.choiceId === '$dismissed';
}

export function renderProvenanceBadge(e: ProjectDecisionLogEntryView): TemplateResult {
  if (isDismissLogEntry(e)) {
    return html`<span class="badge dec"
      ><sl-icon name="x-circle" aria-hidden="true"></sl-icon>dismissed</span
    >`;
  }
  if (e.provenance === 'agent_interpreted') {
    return html`<span class="badge interp"
      ><sl-icon name="robot" aria-hidden="true"></sl-icon>interpreted</span
    >`;
  }
  if (e.provenance === 'direct' && e.outcome === 'confirm') {
    return html`<span class="badge conf"
      ><sl-icon name="patch-check" aria-hidden="true"></sl-icon>confirmed</span
    >`;
  }
  if (e.provenance === 'direct' && e.outcome === 'override') {
    return html`<span class="badge over"
      ><sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>changed</span
    >`;
  }
  return html`<span class="badge">direct</span>`;
}

/** Expand state and toggle for the Made detail panels, owned by the page. */
export interface MadeDisclosure {
  expanded: ReadonlySet<string>;
  onToggle: (answerId: string) => void;
}

/** Label of the decision's recommended choice, or '' when none is known. */
function recommendedLabel(e: ProjectDecisionLogEntryView): string {
  const id = e.decision.defaultChoiceId;
  if (!id) return '';
  return e.decision.choices?.find((c) => c.id === id)?.label || id;
}

/**
 * Collapsed-row recommendation marker (design 4.3): `recommended` when the
 * answer matched the recommendation, `recommended: X` when one existed and
 * was not picked, nothing for dismissals or without a recommendation.
 */
export function renderRecommendationMarker(
  e: ProjectDecisionLogEntryView
): TemplateResult | typeof nothing {
  if (isDismissLogEntry(e)) return nothing;
  if (e.matchedDefault) {
    return html`<span class="muted rec-marker" data-rec-marker="matched">recommended</span>`;
  }
  const label = recommendedLabel(e);
  if (!label) return nothing;
  return html`<span class="muted rec-marker" data-rec-marker="unpicked"
    >recommended: ${label}</span
  >`;
}

function renderAwaitingBadge(e: ProjectDecisionLogEntryView): TemplateResult | typeof nothing {
  return e.awaitingConfirmation
    ? html`<span class="badge await" data-awaiting-confirmation
        ><sl-icon name="hourglass-split" aria-hidden="true"></sl-icon>awaiting confirmation</span
      >`
    : nothing;
}

function answerText(a: {
  choiceId: string;
  label: string;
  otherText?: string | undefined;
}): string {
  const label = a.label || a.choiceId;
  return isOtherAnswer(a) && a.otherText ? `${label}: “${a.otherText}”` : label;
}

function renderDisclosureButton(
  e: ProjectDecisionLogEntryView,
  panelId: string,
  d: MadeDisclosure
): TemplateResult {
  const open = d.expanded.has(e.answerId);
  const name = `${e.decision.key || e.decision.ref} v${e.decision.version}`;
  return html`<button
    type="button"
    class="made-disc"
    data-made-toggle=${e.answerId}
    aria-expanded=${open ? 'true' : 'false'}
    aria-controls=${panelId}
    aria-label=${`${open ? 'Hide' : 'Show'} choices for ${name}`}
    @click=${(): void => d.onToggle(e.answerId)}
  >
    <sl-icon name=${open ? 'chevron-down' : 'chevron-right'} aria-hidden="true"></sl-icon>
  </button>`;
}

/**
 * The Made detail panel (design 4.3): every offered choice in order with its
 * description, the recommended tag and the pick; the Other row; the dismissal;
 * the folded interpreted answer; "superseded by vN"; and "Open in chat" when
 * the prompt message still exists. Every agent-supplied string renders as
 * plain text.
 */
export function renderMadeDetailPanel(
  e: ProjectDecisionLogEntryView,
  opts: { id: string; projectId: string }
): TemplateResult {
  const isDismiss = isDismissLogEntry(e);
  const pickedId = isDismiss ? '' : e.choiceId;
  const choices = e.decision.choices ?? [];
  const defaultId = e.decision.defaultChoiceId ?? '';
  const other = e.decision.other;
  const otherPicked = !isDismiss && isOtherAnswer(e);
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, opts.projectId);
  const permalink =
    e.decision.promptAvailable && convHref && e.decision.promptMessageId
      ? `${convHref}#msg-${encodeURIComponent(e.decision.promptMessageId)}`
      : null;
  const interp = e.interpretedAnswer;
  const replacedBy = e.decision.replacedBy;

  const pickMark = (picked: boolean): TemplateResult =>
    html`<span class="pick"
      >${picked
        ? html`<sl-icon name="check-circle-fill" aria-hidden="true"></sl-icon
            ><span class="sr-only">Picked: </span>`
        : nothing}</span
    >`;

  return html`
    <div class="made-detail" id=${opts.id} data-made-detail=${e.answerId}>
      ${choices.length > 0 || other
        ? html`<ul class="made-choices" aria-label="Offered choices">
            ${choices.map((c) => {
              const picked = c.id === pickedId;
              return html`<li
                class="made-choice"
                data-detail-choice=${c.id}
                ?data-picked=${picked}
                ?data-recommended=${c.id === defaultId}
              >
                ${pickMark(picked)}
                <span>
                  <span class=${picked ? 'lbl picked' : 'lbl'}
                    >${picked ? html`<b>${c.label}</b>` : c.label}</span
                  >${c.id === defaultId
                    ? html`<span class="badge rec" data-detail-recommended>recommended</span>`
                    : nothing}
                  ${c.description
                    ? html`<span class="desc" data-detail-desc>${c.description}</span>`
                    : nothing}
                </span>
              </li>`;
            })}
            ${other
              ? html`<li
                  class="made-choice"
                  data-detail-choice="$other"
                  ?data-picked=${otherPicked}
                >
                  ${pickMark(otherPicked)}
                  <span>
                    <span class="lbl"
                      >${otherPicked ? html`<b>${other.label}</b>` : other.label}</span
                    >
                    ${otherPicked && e.otherText
                      ? html`<span class="other-text" data-detail-other-text
                          >“${e.otherText}”</span
                        >`
                      : nothing}
                    ${other.description
                      ? html`<span class="desc" data-detail-desc>${other.description}</span>`
                      : nothing}
                  </span>
                </li>`
              : nothing}
          </ul>`
        : html`<span class="muted" data-detail-no-choices
            >Offered choices are not available.</span
          >`}
      <div class="made-detail-notes">
        ${isDismiss
          ? html`<span data-detail-dismissed
              ><b>Dismissed</b>${e.dismissReason
                ? html`:
                    <span class="dismiss-reason" data-detail-dismiss-reason
                      >${e.dismissReason}</span
                    >`
                : nothing}</span
            >`
          : nothing}
        ${interp
          ? html`<span data-detail-interpreted>Was interpreted as ${answerText(interp)}</span>`
          : nothing}
        ${replacedBy
          ? html`<span data-detail-superseded>Superseded by v${replacedBy.version}</span>`
          : nothing}
        ${permalink
          ? html`<a class="linkish" data-detail-open-in-chat href=${permalink}
              ><sl-icon name="chat-left-text" aria-hidden="true"></sl-icon> Open in chat</a
            >`
          : nothing}
      </div>
    </div>
  `;
}

export function renderMadeRow(
  e: ProjectDecisionLogEntryView,
  opts: {
    currentUserId: string;
    showProjectColumn: boolean;
    defaultProjectId?: string | undefined;
    resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
    disclosure?: MadeDisclosure | undefined;
  }
): TemplateResult {
  const isDismiss = isDismissLogEntry(e);
  const isFollow = e.outcome === 'confirm' || e.outcome === 'override';
  const panelId = `made-detail-${e.answerId}`;
  const expanded = Boolean(opts.disclosure?.expanded.has(e.answerId));
  const columnCount = opts.showProjectColumn ? 9 : 8;
  const surf = surfaceDisplay(e.surface);
  const projectId = opts.defaultProjectId || e.project?.id || '';
  const projectName = opts.resolveProjectName ? opts.resolveProjectName(e.project) : '';
  const projectHref =
    opts.showProjectColumn && projectId
      ? `/projects/${encodeURIComponent(projectId)}/decisions`
      : null;
  const whereLabel = conversationDisplayLabel(e.chatKey, e.conversationLabel);
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, projectId);
  const replyHref = isDismiss ? null : replyHrefForEntry(e, projectId);
  const answerLabel = e.label || e.choiceId;
  const otherText = !isDismiss && isOtherAnswer(e) ? e.otherText : undefined;
  const prevText =
    !isDismiss && e.provenance === 'direct' && e.outcome === 'override' && e.previousChoiceLabel
      ? `from ${e.previousChoiceLabel}${e.previousOtherText ? ` “${e.previousOtherText}”` : ''}`
      : '';
  const dismissReason =
    isDismiss && e.dismissReason
      ? html`<div class="dismiss-reason muted" data-dismiss-reason>${e.dismissReason}</div>`
      : nothing;

  return html`
    <tr class=${isFollow ? 'follow' : ''} data-answer-row=${e.answerId}>
      <td>
        <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
          >${formatDecisionLogWhen(e.at)}</time
        >
      </td>
      ${opts.showProjectColumn
        ? html`
            <td data-project-cell>
              ${projectHref
                ? html`<a class="linkish" data-project-link href=${projectHref}>${projectName}</a>`
                : projectName}
            </td>
          `
        : nothing}
      <td data-decision-cell>
        ${opts.disclosure ? renderDisclosureButton(e, panelId, opts.disclosure) : nothing}<span
          class="key"
          >${e.decision.key || e.decision.ref}</span
        >
        v${e.decision.version}
      </td>
      <td class="q">
        <span class="q-text" title=${e.decision.question}>${e.decision.question}</span>
      </td>
      <td data-answer-cell class=${otherText ? 'ans' : nothing}>
        ${isDismiss
          ? html`<b>Dismissed</b>${dismissReason}`
          : otherText
            ? html`<div class="ans-clamp" title=${`${answerLabel}: “${otherText}”`}>
                <b>${answerLabel}:</b> “${otherText}”
              </div>`
            : html`<b>${answerLabel}</b>`}
      </td>
      <td data-answered-by>${renderAnsweredBy(e, opts.currentUserId)}</td>
      <td data-provenance>
        ${renderProvenanceBadge(e)} ${renderAwaitingBadge(e)}
        ${!isDismiss && e.provenance === 'agent_interpreted' && replyHref
          ? html` <a class="linkish" data-reply-link href=${replyHref}>reply</a>`
          : prevText
            ? html` <span class="muted">${prevText}</span>`
            : nothing}
        ${renderRecommendationMarker(e)}
      </td>
      <td data-surface>
        <span class="surf"
          >${surf.icon
            ? html`<sl-icon name=${surf.icon} aria-hidden="true"></sl-icon>`
            : nothing}${surf.label}</span
        >
      </td>
      <td data-agent-where>
        ${e.decision.askerAgentName} ·
        ${convHref
          ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
          : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
      </td>
    </tr>
    ${expanded
      ? html`<tr class="detail-row" data-detail-row=${e.answerId}>
          <td colspan=${columnCount}>${renderMadeDetailPanel(e, { id: panelId, projectId })}</td>
        </tr>`
      : nothing}
  `;
}

export function renderMadeItem(
  e: ProjectDecisionLogEntryView,
  opts: {
    currentUserId: string;
    includeProjectInMeta: boolean;
    defaultProjectId?: string | undefined;
    fallbackProject?: ProjectDecisionProjectRef | undefined;
    resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
    disclosure?: MadeDisclosure | undefined;
  }
): TemplateResult {
  const isDismiss = isDismissLogEntry(e);
  const surf = surfaceDisplay(e.surface);
  const proj = e.project?.id ? e.project : opts.fallbackProject;
  const panelId = `made-detail-m-${e.answerId}`;
  const expanded = Boolean(opts.disclosure?.expanded.has(e.answerId));
  const projectId = opts.defaultProjectId || proj?.id || '';
  const projectName = opts.resolveProjectName ? opts.resolveProjectName(proj) : '';
  const whereLabel = conversationDisplayLabel(e.chatKey, e.conversationLabel);
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, projectId);
  const replyHref = isDismiss ? null : replyHrefForEntry(e, projectId);
  const answerLabel = e.label || e.choiceId;
  const otherText = !isDismiss && isOtherAnswer(e) ? e.otherText : undefined;
  const prevText =
    !isDismiss && e.outcome === 'override' && e.previousChoiceLabel
      ? `from ${e.previousChoiceLabel}${e.previousOtherText ? ` “${e.previousOtherText}”` : ''}`
      : '';

  return html`
    <div class="item" data-mobile-answer=${e.answerId}>
      <div class="top">
        ${opts.disclosure ? renderDisclosureButton(e, panelId, opts.disclosure) : nothing}<span
          class="key"
          >${e.decision.key || e.decision.ref}</span
        >
        ${isDismiss
          ? html`<b>Dismissed</b>`
          : otherText
            ? html`<b>${answerLabel}:</b> <span class="ans-other">“${otherText}”</span>`
            : html`<b>${answerLabel}</b>`}
        ${renderProvenanceBadge(e)} ${renderAwaitingBadge(e)} ${renderRecommendationMarker(e)}
      </div>
      <div>${e.decision.question}</div>
      ${isDismiss && e.dismissReason
        ? html`<div class="dismiss-reason muted" data-dismiss-reason>${e.dismissReason}</div>`
        : nothing}
      <div class="meta">
        ${opts.includeProjectInMeta && projectName
          ? html`${projectName} · `
          : nothing}${renderAnsweredBy(e, opts.currentUserId)}
        ${prevText ? html` · ${prevText}` : nothing} · ${surf.label} · ${e.decision.askerAgentName}
        ·
        ${convHref
          ? html`<a class="linkish" href=${convHref}>${whereLabel}</a>`
          : html`<span class="muted">${whereLabel}</span>`}
        ·
        <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
          >${formatDecisionLogWhen(e.at)}</time
        >
        ${replyHref ? html` · <a class="linkish" href=${replyHref}>reply</a>` : nothing}
      </div>
      ${expanded ? renderMadeDetailPanel(e, { id: panelId, projectId }) : nothing}
    </div>
  `;
}

export function renderMadeTableAndMobileList(
  entries: readonly ProjectDecisionLogEntryView[],
  opts: {
    currentUserId: string;
    showProjectColumn: boolean;
    defaultProjectId?: string | undefined;
    resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
    disclosure?: MadeDisclosure | undefined;
  }
): TemplateResult {
  return html`
    <table class="log" data-made-table>
      <thead>
        <tr>
          <th>When</th>
          ${opts.showProjectColumn ? html`<th>Project</th>` : nothing}
          <th>Decision</th>
          <th>Question</th>
          <th>Answer</th>
          <th>Answered by</th>
          <th>Provenance</th>
          <th>Surface</th>
          <th>Agent · where</th>
        </tr>
      </thead>
      <tbody>
        ${repeat(
          entries,
          (e) => e.answerId,
          (e) =>
            renderMadeRow(e, {
              currentUserId: opts.currentUserId,
              showProjectColumn: opts.showProjectColumn,
              defaultProjectId: opts.defaultProjectId,
              resolveProjectName: opts.resolveProjectName,
              disclosure: opts.disclosure,
            })
        )}
      </tbody>
    </table>

    <div class="mlist made-mobile-list" data-made-mobile-list>
      ${repeat(
        entries,
        (e) => e.answerId,
        (e) =>
          renderMadeItem(e, {
            currentUserId: opts.currentUserId,
            includeProjectInMeta: opts.showProjectColumn,
            defaultProjectId: opts.defaultProjectId,
            resolveProjectName: opts.resolveProjectName,
            disclosure: opts.disclosure,
          })
      )}
    </div>
  `;
}

export function renderBulkDismissDialog(opts: {
  prompts: readonly ProjectDecisionPromptView[];
  currentUserId: string;
  reasonDraft: string;
  submitting: boolean;
  completedCount: number;
  goneAgentIds: ReadonlySet<string>;
  onReasonInput: (value: string) => void;
  onCancel: () => void;
  onConfirm: () => void;
}): TemplateResult | typeof nothing {
  const prompts = opts.prompts;
  if (prompts.length === 0) return nothing;

  const count = prompts.length;
  const dialogLabel = count === 1 ? 'Dismiss 1 card' : `Dismiss ${count} cards`;
  const confirmLabel = count === 1 ? 'Dismiss 1 card' : `Dismiss ${count} cards`;
  const onTextareaInput = (e: Event): void => {
    const raw = (e.target as HTMLElement & { value?: string })?.value ?? '';
    const clamped = clampDismissReason(raw);
    if (clamped !== raw && e.target && 'value' in e.target) {
      (e.target as { value: string }).value = clamped;
    }
    opts.onReasonInput(clamped);
  };

  let goneCount = 0;
  const distinctAgentNames = new Set<string>();
  for (const p of prompts) {
    if (p.askerAgentName) distinctAgentNames.add(p.askerAgentName);
    if (p.askerAgentId && opts.goneAgentIds.has(p.askerAgentId)) {
      goneCount++;
    }
  }
  const allGone = goneCount === count;
  const someGone = goneCount > 0 && !allGone;
  const singleAsker = distinctAgentNames.size === 1 ? [...distinctAgentNames][0] : '';

  const progressPct = count > 0 ? Math.round((opts.completedCount / count) * 100) : 0;

  return html`
    <sl-dialog
      class="dismiss-dialog"
      data-bulk-dismiss-dialog
      label=${dialogLabel}
      open
      @sl-request-close=${(e: Event): void => {
        if (opts.submitting) {
          e.preventDefault();
          return;
        }
        opts.onCancel();
      }}
    >
      <div class="dismiss-dialog-body">
        <div class="dismiss-dialog-note">
          Dismissed decisions close for everyone and cannot be answered. This cannot be undone.
        </div>
        <div class="bulk-dismiss-list" data-bulk-dismiss-list>
          ${prompts.map((p) => {
            const waitingRows = promptWaitingOnViewerRows(p, opts.currentUserId);
            const firstRow = waitingRows[0] ?? p.decisions[0];
            const extraCount = Math.max(0, waitingRows.length - 1);
            const whereLabel = conversationDisplayLabel(p.chatKey, p.conversationLabel);
            const isGone = Boolean(p.askerAgentId && opts.goneAgentIds.has(p.askerAgentId));
            return html`
              <div class="bulk-dismiss-item" data-bulk-dismiss-item=${p.promptMessageId}>
                <div class="bulk-dismiss-item-meta">
                  <b>${p.askerAgentName || 'Agent'}</b>
                  ${isGone ? html`<span class="muted">(deleted)</span>` : nothing}
                  <span>·</span>
                  <span>${whereLabel}</span>
                  ${extraCount > 0
                    ? html`<span class="muted">· +${extraCount} more</span>`
                    : nothing}
                </div>
                ${firstRow
                  ? html`
                      <div class="bulk-dismiss-item-q">
                        ${firstRow.key ? html`<span class="key">${firstRow.key}</span>` : nothing}
                        <span>${firstRow.question}</span>
                      </div>
                    `
                  : nothing}
              </div>
            `;
          })}
        </div>
        <sl-textarea
          size="small"
          rows="2"
          label="Reason (optional)"
          help-text=${allGone
            ? 'Plain text, up to 500 characters. Shown on each card.'
            : 'Plain text, up to 500 characters. Sent to live asking agents and shown on each card.'}
          placeholder="Why these no longer apply"
          data-bulk-dismiss-reason
          ?disabled=${opts.submitting}
          .value=${opts.reasonDraft}
          @sl-input=${onTextareaInput}
          @input=${onTextareaInput}
        >
          <span slot="help-text" class="dismiss-reason-help">
            <span
              >${allGone
                ? 'Plain text, up to 500 characters. Shown on each card.'
                : 'Plain text, up to 500 characters. Sent to live asking agents and shown on each card.'}</span
            >
            <span class="dismiss-reason-count" data-bulk-dismiss-reason-count
              >${[...opts.reasonDraft].length}/${MAX_DISMISS_REASON_RUNES}</span
            >
          </span>
        </sl-textarea>
        ${allGone
          ? html`<div class="dismiss-tell" data-bulk-dismiss-tell="gone">
              <sl-icon name="person-x" aria-hidden="true"></sl-icon>
              <span
                >${singleAsker
                  ? `${singleAsker} is gone; nothing will be sent.`
                  : 'Asking agents are gone; nothing will be sent.'}</span
              >
            </div>`
          : someGone
            ? html`<div class="dismiss-tell" data-bulk-dismiss-tell="mixed">
                <sl-icon name="send" aria-hidden="true"></sl-icon>
                <span
                  >Live asking agents will be told; deleted agents are gone and nothing will be sent
                  to them.</span
                >
              </div>`
            : html`<div class="dismiss-tell" data-bulk-dismiss-tell="live">
                <sl-icon name="send" aria-hidden="true"></sl-icon>
                <span
                  >${singleAsker
                    ? `${singleAsker} will be told if reachable.`
                    : 'Live asking agents will be told in their conversations.'}</span
                >
              </div>`}
        ${opts.submitting
          ? html`
              <div
                class="bulk-dismiss-progress"
                data-bulk-dismiss-progress
                role="status"
                aria-live="polite"
              >
                <div class="bulk-dismiss-progress-label">
                  Dismissing ${opts.completedCount} of ${count} ${count === 1 ? 'card' : 'cards'}…
                </div>
                <sl-progress-bar value=${progressPct}></sl-progress-bar>
              </div>
            `
          : nothing}
      </div>
      <div slot="footer" class="dismiss-dialog-footer">
        <sl-button
          size="small"
          data-bulk-dismiss-cancel
          ?disabled=${opts.submitting}
          @click=${opts.onCancel}
        >
          Cancel
        </sl-button>
        <sl-button
          size="small"
          variant="danger"
          outline
          data-bulk-dismiss-confirm
          ?loading=${opts.submitting}
          ?disabled=${opts.submitting}
          @click=${opts.onConfirm}
        >
          ${confirmLabel}
        </sl-button>
      </div>
    </sl-dialog>
  `;
}
