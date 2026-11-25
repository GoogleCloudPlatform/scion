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
 * The conversation header's open-decisions button (ptone/scion#8594):
 * an always-visible `signpost-split` button (no numeric badge) that opens this
 * conversation's open decision prompts, oldest first, split into "Waiting on
 * you" and "Waiting on others".
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import {
  ConversationOpenDecisionsController,
  type ConversationDecisionCounts,
  type ConversationOpenDecisionsSnapshot,
  type ConversationOpenPromptItem,
} from '../../../client/conversation-open-decisions.js';
import { formatInstantWithZone, formatRelative } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { shouldUseMenuSheet } from './context-menu.js';
import { withFocusMove } from '../focus-moved.js';

/** Room kept between the desktop panel and the viewport edges, px. */
const PANEL_MARGIN_PX = 8;

/** The desktop panel's height cap: min(26rem, 70vh). */
const PANEL_MAX_REM = 26;

/** The smallest height the desktop panel is squeezed to, px. */
const PANEL_MIN_PX = 120;

/** `detail` of the `open-decision-open` event. */
export interface OpenDecisionOpenDetail {
  messageId: string;
}

let panelSeq = 0;

@customElement('scion-chat-open-decisions')
export class ScionChatOpenDecisions extends LitElement {
  /** The conversation whose open decisions are listed. */
  @property() conversationKey = '';

  /** Project slug (or ID fallback) for "Open in Decisions" links. */
  @property() projectSlug = '';

  @state() private open = false;
  @state() private sheet = false;
  @state() private hiddenBy404 = false;
  @state() private counts: ConversationDecisionCounts = { waitingOnYou: 0, waitingOnOthers: 0 };
  @state() private prompts: ConversationOpenPromptItem[] = [];
  @state() private truncated = false;
  @state() private loading = false;
  @state() private error = '';
  @state() private othersExpanded = false;
  @state() private liveAnnouncement = '';

  private prevServerTotal = 0;
  private prevLoadedOnce = false;
  private lastServerSeq = 0;
  private readonly seenServerPromptIds = new Set<string>();
  private announcementSeq = 0;
  private announcementClearTimer: ReturnType<typeof setTimeout> | null = null;

  private readonly panelId = `open-decisions-panel-${++panelSeq}`;
  private readonly headingId = `${this.panelId}-heading`;
  readonly _zone = new DisplayZoneController(this);
  private readonly controller = new ConversationOpenDecisionsController({
    onChange: (snap: ConversationOpenDecisionsSnapshot) => this.applySnapshot(snap),
  });

  static override styles = css`
    :host {
      position: relative;
      display: inline-flex;
    }

    :host([data-hidden]) {
      display: none;
    }

    .trigger {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      border: none;
      background: none;
      padding: var(--sl-spacing-x-small, 0.5rem);
      margin: 0;
      border-radius: var(--sl-border-radius-medium, 0.25rem);
      font-size: inherit;
      line-height: 1;
      color: var(--sl-color-neutral-600, #475569);
      cursor: pointer;
      transition: var(--sl-transition-x-fast, 100ms) color;
      -webkit-appearance: none;
    }

    .trigger.waiting-on-you {
      color: var(--scion-primary, #3b82f6);
    }

    .trigger:hover {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .trigger:active {
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .trigger:focus {
      outline: none;
    }

    .trigger:focus-visible {
      outline: var(--sl-focus-ring, solid 3px rgba(59, 130, 246, 0.4));
      outline-offset: var(--sl-focus-ring-offset, 1px);
    }

    @media (prefers-reduced-motion: reduce) {
      .trigger {
        transition: none;
      }
    }

    .panel {
      position: absolute;
      top: calc(100% + 0.25rem);
      right: 0;
      z-index: 50;
      display: flex;
      flex-direction: column;
      box-sizing: border-box;
      width: min(24rem, calc(100vw - 1rem));
      max-height: min(26rem, 70vh);
      background: var(--scion-surface, #fff);
      color: var(--scion-text, #1e293b);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: var(--sl-shadow-large, 0 8px 24px rgba(0, 0, 0, 0.15));
      font-weight: 400;
      text-align: left;
    }

    .panel:focus {
      outline: none;
    }

    /* ---- Narrow screens: modal bottom sheet ---- */
    dialog.sheet {
      position: fixed;
      inset: auto 0 0 0;
      margin: 0;
      width: 100%;
      max-width: 100%;
      max-height: 80vh;
      max-height: 80dvh;
      box-sizing: border-box;
      padding: 0 env(safe-area-inset-right, 0px) env(safe-area-inset-bottom, 0px)
        env(safe-area-inset-left, 0px);
      border: none;
      border-radius: calc(var(--scion-radius-xl) * 0.875) calc(var(--scion-radius-xl) * 0.875) 0 0;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      box-shadow: 0 -4px 24px rgba(0, 0, 0, 0.18);
      overflow: hidden;
      flex-direction: column;
      font-weight: 400;
      text-align: left;
      touch-action: pan-y pinch-zoom;
    }

    dialog.sheet[open] {
      display: flex;
      animation: sheet-in 0.18s ease-out;
    }

    dialog.sheet::backdrop {
      background: rgba(15, 23, 42, 0.45);
    }

    @keyframes sheet-in {
      from {
        transform: translateY(100%);
      }
      to {
        transform: translateY(0);
      }
    }

    @media (prefers-reduced-motion: reduce) {
      dialog.sheet[open] {
        animation: none;
      }
    }

    .grab {
      flex: none;
      width: 2.25rem;
      height: 0.25rem;
      border-radius: var(--scion-radius-full, 9999px);
      background: var(--scion-border, #e2e8f0);
      margin: 0.4rem auto 0;
    }

    .panel-header {
      flex: none;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.5rem 0.375rem 0.75rem;
    }

    .sheet .panel-header {
      padding: 0.5rem 0.25rem 0.5rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .close {
      flex: none;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 44px;
      height: 44px;
      border: none;
      background: none;
      border-radius: calc(var(--scion-radius) * 0.75);
      color: var(--scion-text-muted, #64748b);
      font-size: 1.125rem;
      cursor: pointer;
    }

    .close:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    .sheet .scroll {
      padding: 0.25rem 0.5rem 0.75rem;
    }

    .sheet .entry {
      min-height: 52px;
      padding: 0.5rem 0.55rem;
    }

    .sheet button.section-header {
      min-height: 44px;
    }

    .sheet .status sl-button::part(base) {
      min-height: 44px;
    }

    .panel-title {
      flex: 1 1 auto;
      min-width: 0;
      margin: 0;
      font-weight: 600;
      font-size: var(--chat-fs-base, 0.875rem);
    }

    .panel-count {
      font-weight: 400;
      color: var(--scion-text-muted, #64748b);
      margin-left: 0.2rem;
    }

    .sort-label {
      flex: none;
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
    }

    .scroll {
      flex: 1 1 auto;
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
      touch-action: pan-y pinch-zoom;
      padding: 0 0.5rem 0.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      gap: 0.35rem;
      font-size: var(--chat-fs-xs, 0.75rem);
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--scion-text-muted, #64748b);
      padding: 0.45rem 0.5rem 0.2rem;
    }

    .section-count {
      font-weight: 700;
      color: var(--scion-text-secondary, #475569);
    }

    button.section-header {
      cursor: pointer;
      border: none;
      background: none;
      width: 100%;
      text-align: left;
      border-radius: calc(var(--scion-radius) * 0.75);
      min-height: 2rem;
    }

    button.section-header:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    button.section-header:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    ul {
      list-style: none;
      margin: 0;
      padding: 0;
    }

    .entry {
      display: block;
      min-height: 44px;
      box-sizing: border-box;
      padding: 0.375rem 0.5rem;
      border-radius: calc(var(--scion-radius) * 0.75);
      color: inherit;
      text-decoration: none;
    }

    a.entry:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    a.entry:focus {
      outline: none;
    }

    a.entry:focus-visible {
      background: var(--scion-bg-subtle, #f1f5f9);
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: -2px;
    }

    .entry-meta {
      display: flex;
      gap: 0.3rem;
      align-items: center;
      min-width: 0;
      font-size: var(--chat-fs-xs, 0.75rem);
      line-height: 1.35;
      color: var(--scion-text-muted, #64748b);
    }

    .entry-asker {
      color: var(--scion-text-secondary, #475569);
      font-weight: 600;
      min-width: 0;
      overflow: hidden;
      white-space: nowrap;
      text-overflow: ellipsis;
    }

    .entry-age {
      flex: none;
    }

    .chip {
      flex: none;
      font-size: 0.66rem;
      font-weight: 600;
      line-height: 1;
      padding: 0.16rem 0.38rem;
      border-radius: var(--scion-radius-full, 9999px);
      border: 1px solid currentColor;
      color: var(--scion-text-secondary, #475569);
      white-space: nowrap;
    }

    .chip.you {
      color: var(--scion-primary, #3b82f6);
    }

    .entry-question {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      font-size: var(--chat-fs-sm, 0.8125rem);
      line-height: 1.35;
      overflow-wrap: anywhere;
    }

    .entry.others .entry-question {
      color: var(--scion-text-secondary, #475569);
    }

    .entry-reason {
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
      display: flex;
      gap: 0.25rem;
      align-items: center;
      flex-wrap: wrap;
      margin-top: 0.1rem;
    }

    .entry.gone {
      cursor: default;
    }

    .entry.gone .entry-question {
      color: var(--scion-text-muted, #64748b);
    }

    .entry.gone a,
    .panel-footer a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
    }

    .entry.gone a:hover,
    .panel-footer a:hover {
      text-decoration: underline;
    }

    .panel-footer {
      flex: none;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.45rem 0.75rem;
      font-size: var(--chat-fs-xs, 0.75rem);
      color: var(--scion-text-muted, #64748b);
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 0.5rem;
    }

    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      margin: -1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }

    .status {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.25rem 0.5rem;
      padding: 0.75rem 0.5rem;
      font-size: var(--chat-fs-sm, 0.8125rem);
      color: var(--scion-text-muted, #64748b);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.conversationKey) {
      this.controller.setConversation(this.conversationKey);
    }
    this.controller.connect();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopListening();
    this.clearAnnouncementTimer();
    this.announcementSeq++;
    this.controller.disconnect();
  }

  protected override willUpdate(changed: PropertyValues): void {
    if (changed.has('conversationKey') && changed.get('conversationKey') !== undefined) {
      this.closePanel(false);
      this.othersExpanded = false;
      this.clearAnnouncementTimer();
      this.announcementSeq++;
      this.liveAnnouncement = '';
      this.prevServerTotal = 0;
      this.prevLoadedOnce = false;
      this.lastServerSeq = 0;
      this.seenServerPromptIds.clear();
      this.controller.setConversation(this.conversationKey);
    }
    if (changed.has('hiddenBy404')) {
      this.toggleAttribute('data-hidden', this.hiddenBy404);
    }
  }

  private clearAnnouncementTimer(): void {
    if (this.announcementClearTimer !== null) {
      clearTimeout(this.announcementClearTimer);
      this.announcementClearTimer = null;
    }
  }

  private announceNewDecision(): void {
    this.clearAnnouncementTimer();
    const seq = ++this.announcementSeq;
    // Clear first so consecutive announcements always produce a real DOM text
    // mutation that assistive technologies re-announce.
    this.liveAnnouncement = '';
    void this.updateComplete.then(() => {
      if (seq !== this.announcementSeq || this.open) return;
      this.liveAnnouncement = 'New decision waiting';
      this.announcementClearTimer = setTimeout(() => {
        this.announcementClearTimer = null;
        if (seq === this.announcementSeq) {
          this.liveAnnouncement = '';
        }
      }, 1500);
    });
  }

  private applySnapshot(snap: ConversationOpenDecisionsSnapshot): void {
    const nextTotal = snap.counts.waitingOnYou + snap.counts.waitingOnOthers;
    if (snap.loadedOnce && snap.serverSnapshotSeq !== this.lastServerSeq) {
      this.lastServerSeq = snap.serverSnapshotSeq;
      const hasNewPromptId = snap.prompts.some(
        (p) => !this.seenServerPromptIds.has(p.promptMessageId)
      );
      const truncatedIncrease = snap.truncated && nextTotal > this.prevServerTotal;
      if (this.prevLoadedOnce && !this.open && (hasNewPromptId || truncatedIncrease)) {
        this.announceNewDecision();
      }
      for (const p of snap.prompts) {
        this.seenServerPromptIds.add(p.promptMessageId);
      }
      this.prevServerTotal = nextTotal;
      this.prevLoadedOnce = true;
    }
    this.counts = snap.counts;
    this.prompts = snap.prompts;
    this.truncated = snap.truncated;
    this.loading = snap.loading;
    this.error = snap.error;
    this.hiddenBy404 = snap.hiddenBy404;
    if (snap.hiddenBy404 && this.open) {
      this.closePanel(false);
    }
  }

  protected override updated(changed: PropertyValues): void {
    super.updated(changed);
    if (changed.has('open') || changed.has('sheet')) this.syncSheet();
    if (this.open && !this.sheet) this.fitPanel();
  }

  private syncSheet(): void {
    const dialog = this.renderRoot.querySelector<HTMLDialogElement>('dialog.sheet');
    if (!dialog) return;
    if (this.open && this.sheet && !dialog.open) {
      withFocusMove(() => dialog.showModal());
      const scroll = dialog.querySelector('.scroll');
      if (scroll) scroll.scrollTop = 0;
    }
  }

  /** Whether the panel is open (for the page and tests). */
  get isOpen(): boolean {
    return this.open;
  }

  /** Put focus back on the toolbar button. */
  focusTrigger(): void {
    const trigger = this.renderRoot.querySelector<HTMLElement>('.trigger');
    if (trigger) withFocusMove(() => trigger.focus());
  }

  private get totalCount(): number {
    return this.counts.waitingOnYou + this.counts.waitingOnOthers;
  }

  private get accessibleButtonLabel(): string {
    const total = this.totalCount;
    if (total === 0) return 'Open decisions in this conversation: none';
    if (this.counts.waitingOnYou > 0) {
      return `Open decisions in this conversation: ${total}, ${this.counts.waitingOnYou} waiting on you`;
    }
    return `Open decisions in this conversation: ${total}, none waiting on you`;
  }

  private toggle(): void {
    if (this.open) {
      this.closePanel(true);
      return;
    }
    this.sheet = shouldUseMenuSheet();
    this.open = true;
    this.clearAnnouncementTimer();
    this.announcementSeq++;
    this.liveAnnouncement = '';
    if (!this.sheet) document.addEventListener('pointerdown', this.handleOutsidePointer, true);
    window.addEventListener('resize', this.handleResize);
    window.addEventListener('popstate', this.handlePopState);
    void this.controller.refetchNow();
    if (!this.sheet) {
      void this.updateComplete.then(() => {
        this.renderRoot.querySelector<HTMLElement>('.panel')?.focus();
      });
    }
  }

  private closePanel(restoreFocus: boolean): void {
    if (!this.open) return;
    this.open = false;
    this.stopListening();
    if (restoreFocus) {
      void this.updateComplete.then(() => {
        this.focusTrigger();
      });
    }
  }

  private stopListening(): void {
    document.removeEventListener('pointerdown', this.handleOutsidePointer, true);
    window.removeEventListener('resize', this.handleResize);
    window.removeEventListener('popstate', this.handlePopState);
  }

  private fitPanel(): void {
    const panel = this.renderRoot.querySelector<HTMLElement>('.panel');
    if (!panel) return;
    const vw = document.documentElement.clientWidth || window.innerWidth;
    const vh = window.visualViewport?.height ?? window.innerHeight;
    const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    panel.style.transform = '';
    const rect = panel.getBoundingClientRect();
    const room = vh - rect.top - PANEL_MARGIN_PX;
    const cap = Math.min(PANEL_MAX_REM * rem, vh * 0.7);
    panel.style.maxHeight = `${Math.max(Math.min(cap, room), PANEL_MIN_PX)}px`;
    let shift = 0;
    if (rect.left < PANEL_MARGIN_PX) shift = PANEL_MARGIN_PX - rect.left;
    else if (rect.right > vw - PANEL_MARGIN_PX) shift = vw - PANEL_MARGIN_PX - rect.right;
    if (shift !== 0) panel.style.transform = `translateX(${Math.round(shift)}px)`;
  }

  private handleOutsidePointer = (e: PointerEvent): void => {
    if (!e.composedPath().includes(this)) this.closePanel(false);
  };

  private handleFocusOut = (e: FocusEvent): void => {
    if (!this.open || this.sheet) return;
    const next = e.relatedTarget as Node | null;
    if (!next) return;
    if (this.contains(next) || this.renderRoot.contains(next)) return;
    this.closePanel(false);
  };

  private handleResize = (): void => {
    if (!this.open) return;
    if (shouldUseMenuSheet() !== this.sheet) {
      this.closePanel(false);
      return;
    }
    if (!this.sheet) this.fitPanel();
  };

  private handleSheetCancel = (e: Event): void => {
    e.preventDefault();
    this.closePanel(true);
  };

  private handleSheetClose = (): void => {
    if (this.open && this.sheet) this.closePanel(true);
  };

  private handleSheetClick = (e: MouseEvent): void => {
    e.stopPropagation();
    const dialog = e.currentTarget as HTMLDialogElement;
    if (e.target !== dialog) return;
    const r = dialog.getBoundingClientRect();
    const inside =
      e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
    if (!inside) this.closePanel(true);
  };

  private stopTouch = {
    handleEvent: (e: TouchEvent): void => e.stopPropagation(),
    passive: true,
  };

  private handlePopState = (): void => {
    this.closePanel(false);
  };

  private handleKeydown = (e: KeyboardEvent): void => {
    if (e.key === 'Escape' && this.open) {
      e.stopPropagation();
      e.preventDefault();
      this.closePanel(true);
    }
  };

  private openEntry(e: MouseEvent, item: ConversationOpenPromptItem): void {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    this.closePanel(false);
    this.dispatchEvent(
      new CustomEvent<OpenDecisionOpenDetail>('open-decision-open', {
        detail: { messageId: item.promptMessageId },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderChip(item: ConversationOpenPromptItem): TemplateResult | typeof nothing {
    const cls = item.waitingOnYou ? 'chip you' : 'chip';
    if (item.answerMode === 'set' && item.totalCount > 1) {
      return html`<span class=${cls}>${item.openCount} of ${item.totalCount} open</span>`;
    }
    if (item.awaitingConfirmation) {
      return html`<span class=${cls}>Confirm</span>`;
    }
    return nothing;
  }

  private get decisionsPageUrl(): string {
    const conv = encodeURIComponent(this.conversationKey);
    if (this.projectSlug) {
      return `/projects/${encodeURIComponent(this.projectSlug)}/decisions?conversation=${conv}`;
    }
    return `/profile/decisions?conversation=${conv}`;
  }

  private reasonIcon(reason: string): string {
    const lower = reason.toLowerCase();
    if (lower.includes('read-only')) return 'eye';
    if (lower.includes('confirm')) return 'person';
    return 'slash-circle';
  }

  private renderEntry(item: ConversationOpenPromptItem): TemplateResult {
    const age = formatRelative(item.createdAt, { style: 'short' });
    const abs = formatInstantWithZone(item.createdAt);
    const meta = html`<span class="entry-meta">
      <span class="entry-asker">${item.askerAgentName}</span>
      <span aria-hidden="true">·</span>
      <time class="entry-age" datetime=${item.createdAt} title=${abs}>${age}</time>
      ${this.renderChip(item)}
    </span>`;
    const question = html`<span class="entry-question">${item.question}</span>`;

    if (!item.promptAvailable) {
      const cls = item.waitingOnYou ? 'entry gone' : 'entry others gone';
      return html`<li>
        <div class=${cls} data-message-id=${item.promptMessageId}>
          ${meta} ${question}
          <span class="entry-reason">
            <sl-icon name="clock-history" aria-hidden="true"></sl-icon>
            <span>Message no longer in chat ·</span>
            <a href=${this.decisionsPageUrl} @click=${(): void => this.closePanel(false)}
              >Open in Decisions</a
            >
          </span>
        </div>
      </li>`;
    }

    const cls = item.waitingOnYou ? 'entry' : 'entry others';
    return html`<li>
      <a
        class=${cls}
        href="#msg-${encodeURIComponent(item.promptMessageId)}"
        data-message-id=${item.promptMessageId}
        @click=${(e: MouseEvent): void => this.openEntry(e, item)}
      >
        ${meta} ${question}
        ${!item.waitingOnYou && item.disabledReason
          ? html`<span class="entry-reason">
              <sl-icon name=${this.reasonIcon(item.disabledReason)} aria-hidden="true"></sl-icon>
              <span>${item.disabledReason}</span>
            </span>`
          : nothing}
      </a>
    </li>`;
  }

  private renderBody(): TemplateResult {
    const hasRows = this.prompts.length > 0;
    if (!hasRows && this.loading) {
      return html`<div class="status" role="status">Loading open decisions…</div>`;
    }
    if (!hasRows && this.error) {
      return html`<div class="status" role="alert">
        <span>${this.error}</span>
        <sl-button
          size="small"
          variant="text"
          @click=${(): void => void this.controller.refetchNow()}
          >Retry</sl-button
        >
      </div>`;
    }
    if (!hasRows) {
      return html`<div class="status" role="status">No open decisions in this conversation.</div>`;
    }
    const waitingYou = this.prompts.filter((p) => p.waitingOnYou);
    const waitingOthers = this.prompts.filter((p) => !p.waitingOnYou);
    const showOthersList = waitingYou.length === 0 || this.othersExpanded;

    return html`
      ${waitingYou.length > 0
        ? html`
            <div class="section-header">
              Waiting on you <span class="section-count">${this.counts.waitingOnYou}</span>
            </div>
            <ul aria-label="Decisions waiting on you">
              ${repeat(
                waitingYou,
                (p) => p.promptMessageId,
                (p) => this.renderEntry(p)
              )}
            </ul>
          `
        : nothing}
      ${this.counts.waitingOnOthers > 0
        ? waitingOthers.length > 0
          ? html`
              <button
                type="button"
                class="section-header"
                aria-expanded=${showOthersList ? 'true' : 'false'}
                @click=${(): void => {
                  this.othersExpanded = !showOthersList;
                }}
              >
                <sl-icon
                  name=${showOthersList ? 'chevron-down' : 'chevron-right'}
                  aria-hidden="true"
                ></sl-icon>
                Waiting on others <span class="section-count">${this.counts.waitingOnOthers}</span>
              </button>
              ${showOthersList
                ? html`<ul aria-label="Decisions waiting on others">
                    ${repeat(
                      waitingOthers,
                      (p) => p.promptMessageId,
                      (p) => this.renderEntry(p)
                    )}
                  </ul>`
                : nothing}
            `
          : html`
              <div class="section-header">
                Waiting on others <span class="section-count">${this.counts.waitingOnOthers}</span>
              </div>
            `
        : nothing}
    `;
  }

  private renderHeader(total: number): TemplateResult {
    return html`<div class="panel-header">
      <h2 class="panel-title" id=${this.headingId}>
        Open decisions${total > 0 ? html`<span class="panel-count">${total}</span>` : nothing}
      </h2>
      ${total > 0 ? html`<span class="sort-label">Oldest first</span>` : nothing}
      ${this.sheet
        ? html`<button
            type="button"
            class="close"
            aria-label="Close open decisions"
            @click=${(): void => this.closePanel(true)}
          >
            <sl-icon name="x-lg" aria-hidden="true"></sl-icon>
          </button>`
        : nothing}
    </div>`;
  }

  private renderFooter(total: number): TemplateResult | typeof nothing {
    if (!this.truncated) return nothing;
    return html`<div class="panel-footer">
      <span>Showing the oldest ${this.prompts.length} of ${total}</span>
      <a href=${this.decisionsPageUrl} @click=${(): void => this.closePanel(false)}
        >Open all in Decisions</a
      >
    </div>`;
  }

  private renderPanel(total: number): TemplateResult | typeof nothing {
    if (!this.open) return nothing;
    if (this.sheet) {
      return html`<dialog
        class="sheet"
        id=${this.panelId}
        aria-labelledby=${this.headingId}
        @cancel=${this.handleSheetCancel}
        @close=${this.handleSheetClose}
        @click=${this.handleSheetClick}
        @touchstart=${this.stopTouch}
        @touchmove=${this.stopTouch}
        @touchend=${this.stopTouch}
        @touchcancel=${this.stopTouch}
      >
        <div class="grab" aria-hidden="true"></div>
        ${this.renderHeader(total)}
        <div class="scroll">${this.renderBody()}</div>
        ${this.renderFooter(total)}
      </dialog>`;
    }
    return html`<div
      class="panel"
      id=${this.panelId}
      role="dialog"
      aria-modal="false"
      aria-labelledby=${this.headingId}
      tabindex="-1"
      @touchstart=${this.stopTouch}
      @touchmove=${this.stopTouch}
      @touchend=${this.stopTouch}
      @touchcancel=${this.stopTouch}
    >
      ${this.renderHeader(total)}
      <div class="scroll">${this.renderBody()}</div>
      ${this.renderFooter(total)}
    </div>`;
  }

  override render(): TemplateResult | typeof nothing {
    if (this.hiddenBy404) return nothing;
    const total = this.totalCount;
    const waitingOnYou = this.counts.waitingOnYou > 0;
    return html`
      <div
        class="host-wrap"
        @focusout=${this.handleFocusOut}
        @keydown=${this.handleKeydown}
        style="display: contents"
      >
        <sl-tooltip content="Open decisions" ?disabled=${this.open}>
          <button
            type="button"
            class=${waitingOnYou ? 'trigger waiting-on-you' : 'trigger'}
            aria-label=${this.accessibleButtonLabel}
            aria-haspopup="dialog"
            aria-expanded=${this.open ? 'true' : 'false'}
            aria-controls=${this.open ? this.panelId : nothing}
            @click=${(): void => this.toggle()}
          >
            <sl-icon name="signpost-split" aria-hidden="true"></sl-icon>
          </button>
        </sl-tooltip>
        ${this.renderPanel(total)}
        <span class="sr-only" aria-live="polite" role="status">${this.liveAnnouncement}</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-open-decisions': ScionChatOpenDecisions;
  }
}
