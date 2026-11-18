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

import type { DecisionAnsweredDetail } from '../../client/decision-prompts.js';
import type {
  ProjectDecisionCounts,
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import './chat/chat-decisions.js';
import {
  avatarColor,
  avatarInitial,
  conversationDisplayLabel,
  conversationHref,
  formatDecisionLogWhen,
  promptAnswerability,
  replyHrefForEntry,
  surfaceDisplay,
  type WaitingOnOthersItem,
} from './decisions-format.js';

export const decisionsPageSharedStyles: CSSResult = css`
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
    border-radius: 0.25rem;
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
    --sl-panel-background-color: var(--scion-surface, #ffffff);
    --sl-panel-border-color: var(--scion-border, #e2e8f0);
    --sl-color-neutral-700: var(--scion-text, #1e293b);
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

export function renderPromptCard(
  p: ProjectDecisionPromptView,
  opts: {
    currentUserId: string;
    defaultProjectId?: string | undefined;
    projectBadgeName?: string | undefined;
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

  return html`
    <div
      class="pcard"
      data-prompt-card=${p.promptMessageId}
      @decision-answered=${(e: CustomEvent<DecisionAnsweredDetail>): void => opts.onAnswered(p, e)}
      @scroll-to-message=${(e: CustomEvent<{ messageId?: string }>): void => opts.onJump(p, e)}
      @decision-jump=${(e: CustomEvent<{ messageId?: string }>): void => opts.onJump(p, e)}
    >
      <div class="pcard-head">
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
        <span>
          in
          ${convHref
            ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
        </span>
        <span class="muted" title=${formatInstantWithZone(p.createdAt)}
          >· ${formatRelative(p.createdAt, { style: 'short' })}</span
        >
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

export function renderProvenanceBadge(e: ProjectDecisionLogEntryView): TemplateResult {
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

export function renderMadeRow(
  e: ProjectDecisionLogEntryView,
  opts: {
    currentUserId: string;
    showProjectColumn: boolean;
    defaultProjectId?: string | undefined;
    resolveProjectName?: ((ref: ProjectDecisionProjectRef | undefined) => string) | undefined;
  }
): TemplateResult {
  const isFollow = e.outcome === 'confirm' || e.outcome === 'override';
  const surf = surfaceDisplay(e.surface);
  const projectId = opts.defaultProjectId || e.project?.id || '';
  const projectName = opts.resolveProjectName ? opts.resolveProjectName(e.project) : '';
  const projectHref =
    opts.showProjectColumn && projectId
      ? `/projects/${encodeURIComponent(projectId)}/decisions`
      : null;
  const whereLabel = conversationDisplayLabel(e.chatKey, e.conversationLabel);
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, projectId);
  const replyHref = replyHrefForEntry(e, projectId);

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
      <td><span class="key">${e.decision.key || e.decision.ref}</span> v${e.decision.version}</td>
      <td class="q">
        <span class="q-text" title=${e.decision.question}>${e.decision.question}</span>
      </td>
      <td><b>${e.label || e.choiceId}</b></td>
      <td data-answered-by>${renderAnsweredBy(e, opts.currentUserId)}</td>
      <td data-provenance>
        ${renderProvenanceBadge(e)}
        ${e.provenance === 'agent_interpreted' && replyHref
          ? html` <a class="linkish" data-reply-link href=${replyHref}>reply</a>`
          : e.provenance === 'direct' && e.outcome === 'override' && e.previousChoiceLabel
            ? html` <span class="muted">from ${e.previousChoiceLabel}</span>`
            : e.provenance === 'direct' && e.outcome === 'answer' && e.matchedDefault
              ? html` <span class="muted">recommended</span>`
              : nothing}
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
  }
): TemplateResult {
  const surf = surfaceDisplay(e.surface);
  const proj = e.project?.id ? e.project : opts.fallbackProject;
  const projectId = opts.defaultProjectId || proj?.id || '';
  const projectName = opts.resolveProjectName ? opts.resolveProjectName(proj) : '';
  const whereLabel = conversationDisplayLabel(e.chatKey, e.conversationLabel);
  const convHref = conversationHref(e.chatKey, e.conversationAvailable, projectId);
  const replyHref = replyHrefForEntry(e, projectId);

  return html`
    <div class="item" data-mobile-answer=${e.answerId}>
      <div class="top">
        <span class="key">${e.decision.key || e.decision.ref}</span>
        <b>${e.label || e.choiceId}</b>
        ${renderProvenanceBadge(e)}
      </div>
      <div>${e.decision.question}</div>
      <div class="meta">
        ${opts.includeProjectInMeta && projectName
          ? html`${projectName} · `
          : nothing}${renderAnsweredBy(e, opts.currentUserId)}
        ${e.outcome === 'override' && e.previousChoiceLabel
          ? html` · from ${e.previousChoiceLabel}`
          : nothing}
        · ${surf.label} · ${e.decision.askerAgentName} ·
        ${convHref
          ? html`<a class="linkish" href=${convHref}>${whereLabel}</a>`
          : html`<span class="muted">${whereLabel}</span>`}
        ·
        <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
          >${formatDecisionLogWhen(e.at)}</time
        >
        ${replyHref ? html` · <a class="linkish" href=${replyHref}>reply</a>` : nothing}
      </div>
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
          })
      )}
    </div>
  `;
}
