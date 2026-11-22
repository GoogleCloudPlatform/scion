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
 * `<scion-chat-decisions>`: the card a decision-prompt message renders under
 * web.decisions (ptone/scion#7673, design §4.1–§4.3).
 *
 * Single mode: one click records the answer through
 * `POST /api/v1/decisions/{rowId}/answer`; Enter on the focused card picks
 * the recommended choice. The card applies the 200's view at once and emits
 * `decision-answered` so the thread holds the same view; other tabs get it
 * over `chat.decision.updated`.
 *
 * Set mode (`answerMode=set`): clicks only select. Recommended choices are
 * outlined and nothing is selected until the viewer clicks, unless their
 * `preselectRecommendedChoices` profile setting is on, which starts each
 * recommended choice selected (ptone/scion#7842). One Submit answers sends
 * every actionable row through
 * `POST /api/v1/decision-prompts/{messageId}/answers`. A 409
 * `answer_set_changed` takes the fresh views and names the changed rows
 * inline; rows answered elsewhere lock as their updates arrive.
 *
 * Replaced rows link to their new version (`decision-jump`); an
 * agent-interpreted answer renders read-only.
 *
 * Answerability (design §4.5): the thread passes `answerability` for the
 * prompt's asker. A blocked card disables every choice (`aria-disabled`),
 * and a set's Submit, and shows the reason inline; a 403 on a click or a
 * set submit blocks it the same way and emits `decision-answer-denied`. A
 * gone asker stays answerable with a note.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import { formatInstant } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { TouchPrimaryController } from '../../../utils/input-modality.js';
import { showToast } from '../../../utils/toast.js';
import { copyWithToast } from '../../../utils/clipboard.js';
import {
  MAX_DISMISS_REASON_RUNES,
  dismissResultToast,
  isOnlyTooRecentDismissError,
} from '../decisions-format.js';
import { focusElement } from '../focus-moved.js';
import {
  ANSWERABLE,
  DISMISS_MIN_AGE_MS,
  OTHER_CHOICE_ID,
  actionableReason,
  answerDecision,
  answerDecisionSet,
  answerabilityFromDenial,
  dismissAvailableAtMsForDecisions,
  dismissDecisionPrompt,
  fetchAskerAnswerability,
  isClosedUnanswered,
  isOpenDecision,
  isOtherAnswer,
  newerDecisionView,
} from '../../../client/decision-prompts.js';
import {
  MessageActionStateController,
  dismissDialogStyles,
  isWholeSetDismissed as isWholeSetDismissedShared,
  isWholeSetWithdrawn as isWholeSetWithdrawnShared,
  messageActionBannerStyles,
  messageActionRowStatusClass,
} from './message-action-state.js';
export { dismissDialogStyles };
import {
  DecisionPreferencesController,
  preselectRecommendedChoices,
} from '../../../client/decision-preferences.js';
import type {
  AskerAnswerability,
  DecisionAnswerDeniedDetail,
  DecisionAnsweredDetail,
  DecisionAnswerSurface,
  SetAnswer,
} from '../../../client/decision-prompts.js';
import type {
  DecisionAnswerMode,
  ChoiceOption,
  DecisionView,
  OtherOption,
} from '../../../shared/types.js';

let hintSeq = 0;

@customElement('scion-chat-decisions')
export class ScionChatDecisions extends LitElement {
  readonly _zone = new DisplayZoneController(this);
  readonly _decisionPrefs = new DecisionPreferencesController(this);
  private readonly touchPrimary = new TouchPrimaryController(this);
  private readonly actionState = new MessageActionStateController(this, {
    kind: 'decision',
    dismissMinAgeMs: DISMISS_MIN_AGE_MS,
    getCreatedAt: () => this.createdAt,
    getDismissAvailableAtMs: () => dismissAvailableAtMsForDecisions(this.views, this.createdAt),
    clockNow: () => this.clockNow(),
    hasDismissableRows: () => this.views.some((d) => this.rowWaitsOnViewer(d)),
    getDismissableRowIds: () =>
      this.views.filter((d) => this.rowWaitsOnViewer(d)).map((d) => d.rowId),
    jumpEventName: 'decision-jump',
  });

  /** The decision-prompt message id. */
  @property()
  messageId = '';

  /** The prompt's decisions, as the thread holds them. */
  @property({ type: Array })
  decisions: DecisionView[] = [];

  /** The signed-in user's id: their own answer reads "Answered by you". */
  @property()
  currentUserId = '';

  /** The prompt's answer mode; two or more decisions are a set either way. */
  @property()
  answerMode: DecisionAnswerMode | undefined = undefined;

  /** Whether the viewer can answer this prompt's asker (design §4.5). */
  @property({ attribute: false })
  answerability: AskerAnswerability = ANSWERABLE;

  /** Surface recorded on answers submitted from this card (`native` or `decisions_page`). */
  @property({ type: String })
  surface: DecisionAnswerSurface = 'native';

  /** ISO timestamp when this prompt version was created (for the 5-minute dismissal threshold). */
  @property()
  createdAt = '';

  /** Display label of the conversation (for example `#release` or `your DM`), shown in the dismiss dialog. */
  @property()
  conversationLabel = '';

  /** Relative permalink for Copy link on `decisions_page` (omitted when the prompt message was purged). */
  @property()
  permalinkHref = '';

  /** Per-row non-actionable reasons from the Decisions page API (`read_only`, `message_denied`, `other_user_confirm`). */
  @property({ attribute: false })
  waitingReasons: Readonly<Record<string, string>> | undefined = undefined;

  /** Hint from parent facets that the asking agent was deleted. */
  @property({ type: Boolean })
  askerGone = false;

  /** Injectable clock for deterministic dismissal-age tests. */
  clockNow: () => number = () => Date.now();

  /**
   * What this card's own 403 said, authoritative over `answerability`
   * until the card is gone.
   */
  @state()
  private denied: AskerAnswerability | null = null;

  /**
   * Views this card received from its own answer POST, newer than
   * `decisions` until the thread passes them back.
   */
  @state()
  private local = new Map<string, DecisionView>();

  /** The row and choice being sent, while the POST is in flight. */
  @state()
  private submitting: { rowId: string; choiceId: string } | null = null;

  /** Single mode: a non-interpreted choice staged in the inline change bar (Mock P2-6). */
  @state()
  private pendingChange: { rowId: string; choiceId: string } | null = null;

  /** Single mode: whether the inline free-form Other editor is open. */
  @state()
  private otherOpen = false;

  /** Single mode: draft text in the inline free-form Other editor (memory only). */
  @state()
  private otherDraft = '';

  /** Single mode: row/response key whose interpreted Other text seeded `otherDraft`. */
  @state()
  private interpretedOtherSeedKey = '';

  /** Rendered question/description markdown, keyed by source text. */
  @state()
  private rendered = new Map<string, DocumentFragment>();

  /** Set mode: choices the viewer clicked, by row (local until submitted). */
  @state()
  private picked = new Map<string, string>();

  /** Set mode: per-row draft text in the inline free-form Other editor (memory only). */
  @state()
  private setOtherDrafts = new Map<string, string>();

  /** Set mode: the Submit answers POST is in flight. */
  @state()
  private setSubmitting = false;

  /** Set mode: rows a 409 answer_set_changed named, shown until the next submit. */
  @state()
  private changedAlert: { key: string; phrase: string }[] | null = null;

  /**
   * Set mode: who answered rows that were actionable here, while this card
   * was open and not by the viewer (design §4.3, concurrent answer).
   */
  @state()
  private answeredElsewhere = new Map<string, string>();

  private readonly hintId = `decision-hint-${++hintSeq}`;
  private readonly submitHintId = `decision-submit-hint-${hintSeq}`;
  private readonly reasonId = `decision-reason-${hintSeq}`;

  static override styles = css`
    ${dismissDialogStyles}
    ${messageActionBannerStyles}

    :host {
      display: block;
      margin-top: 0.5rem;
      font-size: var(--chat-fs-base, 0.9375rem);
      line-height: 1.5;
    }

    .card {
      background: var(--scion-surface, #fff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .card:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: 2px;
    }

    .card-head {
      display: flex;
      flex-wrap: nowrap;
      align-items: flex-start;
      gap: 0.375rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg, #f8fafc);
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
    }

    .card-head-lead {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.375rem 0.5rem;
      flex: 1 1 auto;
      min-width: 0;
    }

    .card-head sl-icon {
      flex-shrink: 0;
      font-size: 1rem;
      color: var(--scion-primary, #3b82f6);
    }

    .title {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .key {
      font: 600 0.75rem/1 var(--scion-font-mono, monospace);
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      padding: 0.2rem 0.4rem;
      border-radius: var(--scion-radius-sm);
      white-space: nowrap;
    }

    .row {
      padding: 0.65rem 0.75rem;
    }

    .row + .row {
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .q {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      line-height: 1.4;
      overflow-wrap: anywhere;
    }

    .q p {
      margin: 0 0 0.25rem;
    }

    .q p:last-child {
      margin-bottom: 0;
    }

    /* The first block is the question; what follows is detail (mock s3). */
    .q > div > * + * {
      font-weight: 400;
      color: var(--scion-text-secondary, #475569);
    }

    .q :is(h1, h2, h3, h4, h5, h6) {
      font-size: 1em;
      margin: 0.25rem 0;
    }

    .q table {
      display: block;
      overflow-x: auto;
      max-width: 100%;
    }

    .q code,
    .desc code {
      font-size: 0.85em;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0 0.25rem;
      border-radius: calc(var(--scion-radius-sm) * 0.8);
    }

    .choices {
      display: flex;
      flex-wrap: wrap;
      gap: 0.4rem;
      margin-top: 0.5rem;
    }

    .choices.stack {
      flex-direction: column;
      flex-wrap: nowrap;
    }

    .card sl-button {
      -webkit-touch-callout: none;
      -webkit-user-select: none;
      user-select: none;
      touch-action: manipulation;
    }

    .card sl-button[data-readonly]::part(base) {
      cursor: default;
    }

    /*
     * A set card's plain choices are native buttons (renderSetButton) drawn
     * as a small sl-button using Shoelace's own rules and tokens, so both
     * themes match the single card's sl-buttons.
     */
    .sb {
      appearance: none;
      margin: 0;
      display: inline-flex;
      align-items: stretch;
      justify-content: center;
      border-style: solid;
      border-width: var(--sl-input-border-width, 1px);
      border-radius: var(--sl-input-border-radius-small, 0.1875rem);
      min-height: var(--sl-input-height-small, 1.875rem);
      padding: 0;
      font-family: var(--sl-input-font-family, inherit);
      font-size: var(--sl-button-font-size-small, 0.75rem);
      font-weight: var(--sl-font-weight-semibold, 600);
      line-height: calc(
        var(--sl-input-height-small, 1.875rem) - var(--sl-input-border-width, 1px) * 2
      );
      white-space: nowrap;
      vertical-align: middle;
      -webkit-touch-callout: none;
      user-select: none;
      -webkit-user-select: none;
      touch-action: manipulation;
      cursor: pointer;
      transition:
        var(--sl-transition-x-fast, 100ms) background-color,
        var(--sl-transition-x-fast, 100ms) color,
        var(--sl-transition-x-fast, 100ms) border,
        var(--sl-transition-x-fast, 100ms) box-shadow;
    }

    .sb[data-readonly] {
      cursor: default;
    }

    .sb:focus {
      outline: none;
    }

    .sb:focus-visible {
      outline: var(--sl-focus-ring, 2px solid #3b82f6);
      outline-offset: var(--sl-focus-ring-offset, 1px);
    }

    .sb:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }

    .sb:disabled * {
      pointer-events: none;
    }

    .sb-prefix {
      flex: 0 0 auto;
      display: flex;
      align-items: center;
      pointer-events: none;
    }

    .sb-label {
      display: inline-block;
      padding: 0 var(--sl-spacing-small, 0.75rem);
    }

    .sb.has-prefix {
      padding-inline-start: var(--sl-spacing-x-small, 0.5rem);
    }

    .sb.has-prefix .sb-label {
      padding-inline-start: var(--sl-spacing-x-small, 0.5rem);
    }

    .sb-default {
      background-color: var(--sl-color-neutral-0, #fff);
      border-color: var(--sl-input-border-color, #cbd5e1);
      color: var(--scion-text, #1e293b);
    }

    @media (hover: hover) {
      .sb-default:hover:not(:disabled):not([data-readonly]) {
        background-color: var(--sl-color-primary-50);
        border-color: var(--sl-color-primary-300);
        color: var(--sl-color-primary-700);
      }
    }

    .sb-default:active:not(:disabled):not([data-readonly]) {
      background-color: var(--sl-color-primary-100);
      border-color: var(--sl-color-primary-400);
      color: var(--sl-color-primary-700);
    }

    .sb-filled {
      background-color: var(--sl-color-primary-600, #2563eb);
      border-color: var(--sl-color-primary-600, #2563eb);
      color: var(--sl-color-neutral-0, #fff);
    }

    @media (hover: hover) {
      .sb-filled:hover:not(:disabled):not([data-readonly]) {
        background-color: var(--sl-color-primary-500);
        border-color: var(--sl-color-primary-500);
      }
    }

    .sb-filled:active:not(:disabled):not([data-readonly]) {
      background-color: var(--sl-color-primary-600);
      border-color: var(--sl-color-primary-600);
    }

    .sb-outline {
      background: none;
      border-color: var(--sl-color-primary-600, #2563eb);
      color: var(--sl-color-primary-600, #2563eb);
    }

    @media (hover: hover) {
      .sb-outline:hover:not(:disabled):not([data-readonly]) {
        background-color: var(--sl-color-primary-600);
        color: var(--sl-color-neutral-0, #fff);
      }
    }

    .sb-outline:active:not(:disabled):not([data-readonly]) {
      border-color: var(--sl-color-primary-700);
      background-color: var(--sl-color-primary-700);
      color: var(--sl-color-neutral-0, #fff);
    }

    .sb-subtle {
      background: none;
      border-color: var(--sl-input-border-color, #cbd5e1);
      color: var(--scion-text, #1e293b);
    }

    @media (hover: hover) {
      .sb-subtle:hover:not(:disabled):not([data-readonly]) {
        background-color: var(--sl-color-primary-50);
        border-color: var(--sl-color-primary-300);
        color: var(--sl-color-primary-700);
      }
    }

    .sb-subtle:active:not(:disabled):not([data-readonly]) {
      background-color: var(--sl-color-primary-100);
      border-color: var(--sl-color-primary-400);
      color: var(--sl-color-primary-700);
    }

    .opt {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      text-align: left;
      width: 100%;
      box-sizing: border-box;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #fff);
      padding: 0.45rem 0.6rem;
      font: inherit;
      color: var(--scion-text, #1e293b);
      -webkit-touch-callout: none;
      -webkit-user-select: none;
      user-select: none;
      touch-action: manipulation;
      cursor: pointer;
    }

    .opt[data-readonly] {
      cursor: default;
    }

    @media (hover: hover) {
      .opt:hover:not([aria-disabled='true']):not(:disabled):not(.chosen):not(.sel) {
        border-color: var(--scion-border-hover, #cbd5e1);
        background: var(--scion-bg, #f8fafc);
      }
    }

    .opt:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: 1px;
    }

    .opt .mark {
      width: 1.1rem;
      flex-shrink: 0;
      margin-top: 0.15rem;
    }

    .opt .lbl {
      font-weight: 600;
    }

    .opt .desc {
      display: block;
      color: var(--scion-text-secondary, #475569);
      font-size: var(--chat-fs-sm, 0.875rem);
      font-weight: 400;
    }

    .opt.recd {
      border-color: var(--scion-primary, #3b82f6);
    }

    .opt.recd .lbl {
      color: var(--scion-primary, #3b82f6);
    }

    .opt[aria-disabled='true'] {
      opacity: 0.55;
      cursor: not-allowed;
    }

    .opt.chosen {
      border-color: var(--sl-color-primary-600, #2563eb);
      background: var(--sl-color-primary-600, #2563eb);
      color: var(--sl-color-neutral-0, #fff);
      opacity: 1;
    }

    .opt.chosen .lbl,
    .opt.chosen .desc {
      color: var(--sl-color-neutral-0, #fff);
    }

    .row-foot {
      margin-top: 0.45rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted, #64748b);
      display: flex;
      gap: 0.35rem;
      align-items: center;
      flex-wrap: wrap;
    }

    .row-foot sl-icon {
      font-size: 0.95rem;
    }

    .row-foot .ok {
      color: var(--scion-status-success, #16a34a);
    }

    .row-foot b {
      color: var(--scion-text-secondary, #475569);
    }

    .version {
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .row-top {
      display: flex;
      gap: 0.5rem;
      align-items: baseline;
    }

    .row-top .q {
      min-width: 0;
    }

    @media (max-width: 600px) {
      .row-top {
        flex-wrap: wrap;
        align-items: center;
        gap: 0.25rem 0.5rem;
      }

      .row-top .key,
      .card-head-lead .key {
        box-sizing: border-box;
        max-width: 100%;
        white-space: normal;
        overflow-wrap: anywhere;
        line-height: 1.25;
      }

      .row-top .q {
        order: 1;
        flex: 1 0 100%;
      }
    }

    .row.replaced .q,
    .row.replaced .choices,
    .row.dismissed .q,
    .row.dismissed .choices,
    .row.dismissed .row-top .key,
    .row.withdrawn .q,
    .row.withdrawn .choices,
    .row.withdrawn .row-top .key,
    .row.closed .q,
    .row.closed .choices,
    .row.closed .row-top .key {
      opacity: 0.55;
    }

    .card.withdrawn-all .card-head .title,
    .card.dismissed-all .card-head .title {
      color: var(--scion-text-secondary, #475569);
    }

    .card-foot.card-foot-wd {
      display: block;
    }

    .tag-wd {
      font-size: 0.7rem;
      font-weight: 600;
      line-height: 1;
      padding: 0.15rem 0.45rem;
      border-radius: 999px;
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      white-space: nowrap;
    }

    .card-head .menu {
      margin-left: auto;
      flex-shrink: 0;
      display: inline-flex;
      align-items: center;
    }

    .card-head .spacer ~ .menu {
      margin-left: 0;
    }

    .card-head .card-menu sl-icon-button::part(base) {
      color: var(--scion-text-secondary, #475569);
      padding: 0.125rem;
      font-size: 1rem;
    }

    .card-head .card-menu sl-icon {
      color: currentColor;
      font-size: 0.95rem;
    }

    sl-dropdown.card-menu sl-menu {
      --sl-panel-background-color: var(--scion-surface, #fff);
      --sl-panel-border-color: var(--scion-border, #e2e8f0);
      --sl-color-neutral-700: var(--scion-text, #1e293b);
      --sl-color-neutral-100: var(--scion-bg-subtle, #f1f5f9);
      --sl-color-neutral-1000: var(--scion-text, #1e293b);
      min-width: 11rem;
    }

    sl-dropdown.card-menu sl-menu-item::part(base) {
      font-size: var(--chat-fs-sm, 0.875rem);
    }

    .menu-item-stack {
      display: inline-flex;
      flex-direction: column;
      line-height: 1.25;
    }

    .menu-sub {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .dismiss-dialog-title {
      display: inline-flex;
      align-items: center;
      gap: 0.45rem;
    }

    .dismiss-dialog-title sl-icon {
      color: var(--scion-status-danger, #dc2626);
      font-size: 1.05rem;
      flex-shrink: 0;
    }

    .dismiss-row-list {
      display: flex;
      flex-direction: column;
      gap: 0.45rem;
    }

    .dismiss-single-row {
      display: flex;
      align-items: baseline;
      gap: 0.45rem;
      font-size: var(--chat-fs-base, 0.9375rem);
      color: var(--scion-text, #1e293b);
      overflow-wrap: anywhere;
    }

    .dismiss-pick {
      display: flex;
      align-items: flex-start;
      overflow-wrap: anywhere;
    }

    .dismiss-pick sl-checkbox::part(base) {
      align-items: flex-start;
    }

    .dismiss-pick sl-checkbox::part(label) {
      color: var(--scion-text, #1e293b);
      font-size: var(--chat-fs-base, 0.9375rem);
      line-height: 1.4;
    }

    .dismiss-pick.off {
      opacity: 0.55;
    }

    .dismiss-pick-text {
      display: inline-block;
    }

    .dismiss-why {
      display: block;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.1rem;
    }

    .row-foot a.jump {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
    }

    .row-foot a.jump:hover {
      text-decoration: underline;
    }

    .row-foot a.jump:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: 1px;
      border-radius: calc(var(--scion-radius-sm) * 0.8);
    }

    .card-foot {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.55rem 0.75rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg, #f8fafc);
      flex-wrap: wrap;
    }

    .counter {
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
    }

    .counter b {
      color: var(--scion-text, #1e293b);
    }

    .counter.missing b {
      color: var(--scion-status-warning, #d97706);
    }

    .counter .ok {
      color: var(--scion-status-success, #16a34a);
      vertical-align: -0.125em;
    }

    .spacer {
      flex: 1;
    }

    .card sl-alert.changed {
      display: block;
      margin: 0.6rem 0.75rem 0;
    }

    .note {
      display: flex;
      gap: 0.35rem;
      align-items: flex-start;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0.75rem;
      border-top: 1px dashed var(--scion-border, #e2e8f0);
    }

    .note sl-icon {
      flex-shrink: 0;
      margin-top: 0.2rem;
    }

    .note b {
      color: var(--scion-text-secondary, #475569);
    }

    .reason {
      margin-top: 0.5rem;
      display: flex;
      gap: 0.4rem;
      align-items: flex-start;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.4rem 0.55rem;
    }

    .set > .reason {
      margin: 0.5rem 0.75rem;
    }

    .reason sl-icon {
      flex-shrink: 0;
      margin-top: 0.2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .reason b {
      color: var(--scion-text, #1e293b);
    }

    .opt.chosen .mark {
      color: var(--sl-color-neutral-0, #fff);
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

    .card-head .badge sl-icon {
      font-size: 0.8rem;
      color: currentColor;
    }

    .chip {
      font-size: 0.7rem;
      font-weight: 600;
      line-height: 1;
      padding: 0.15rem 0.45rem;
      border-radius: 999px;
      margin-left: auto;
      white-space: nowrap;
    }

    .chip.confirm {
      background: var(--scion-badge-success-bg, #dcfce7);
      color: var(--scion-badge-success-text, #166534);
    }

    .chip.change {
      background: var(--scion-badge-primary-bg, #dbeafe);
      color: var(--scion-badge-primary-text, #1e40af);
    }

    .chip.interp {
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-badge-warning-text, #92400e);
    }

    .confirm-row {
      margin-top: 0.5rem;
      display: flex;
      align-items: center;
      gap: 0.6rem;
      flex-wrap: wrap;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
    }

    .confirmbar {
      margin-top: 0.5rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text, #1e293b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.45rem 0.6rem;
    }

    .confirmbar > sl-icon {
      flex-shrink: 0;
      color: var(--scion-primary, #3b82f6);
    }

    .row-foot .muted {
      color: var(--scion-text-muted, #64748b);
    }

    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.35rem;
    }

    @media (max-width: 768px), (hover: none) and (pointer: coarse) {
      .hint {
        display: none;
      }
    }

    .other-btn::part(base) {
      border-style: dashed;
    }

    .sb-other {
      border-style: dashed;
    }

    .editor-foot {
      display: flex;
      align-items: baseline;
      gap: 0.5rem;
    }

    .set-other-help {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .opt.other-opt {
      border-style: dashed;
    }

    .opt.other-opt .mark,
    .opt.sel .mark {
      color: var(--scion-primary, #3b82f6);
    }

    .opt.sel {
      flex-direction: column;
      cursor: default;
      border-color: var(--scion-primary, #3b82f6);
      background: var(--scion-primary-50, #eff6ff);
      box-shadow: inset 0 0 0 1px var(--scion-primary, #3b82f6);
      user-select: auto;
      -webkit-user-select: auto;
    }

    .opt.sel .opt-head {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      width: 100%;
    }

    .editor {
      margin-top: 0.55rem;
      display: flex;
      flex-direction: column;
      gap: 0.4rem;
      padding: 0.55rem 0.6rem;
      border: 1px solid var(--scion-primary, #3b82f6);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-bg, #f8fafc);
      box-sizing: border-box;
    }

    .editor.inset {
      width: 100%;
      margin-top: 0.2rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #fff);
    }

    .editor-head {
      display: flex;
      align-items: baseline;
      gap: 0.5rem;
    }

    .editor .lab {
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
    }

    .editor .cnt {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-left: auto;
      font-variant-numeric: tabular-nums;
      white-space: nowrap;
    }

    .editor .cnt.warn {
      color: var(--scion-status-warning, #d97706);
    }

    .editor .cnt.over {
      color: var(--scion-status-danger, #dc2626);
    }

    .editor .ctl {
      display: flex;
      gap: 0.4rem;
      align-items: center;
      flex-wrap: wrap;
    }

    .editor .ctl sl-input {
      flex: 1 1 16rem;
      min-width: 0;
    }

    .editor sl-input::part(form-control-label) {
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

    .editor sl-input::part(base) {
      background: var(--scion-surface, #fff);
      border-color: var(--scion-border, #e2e8f0);
    }

    .editor sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    .editor sl-input::part(input)::placeholder {
      color: var(--scion-text-muted, #64748b);
    }

    .otext {
      margin-top: 0.45rem;
      padding: 0.3rem 0.6rem;
      border-left: 3px solid var(--scion-primary, #3b82f6);
      color: var(--scion-text, #1e293b);
      font-size: var(--chat-fs-base, 0.9375rem);
      overflow-wrap: anywhere;
    }

    @media (max-width: 480px) {
      .editor .ctl sl-input {
        flex-basis: 100%;
      }

      .editor .ctl {
        justify-content: flex-end;
      }
    }

    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }
  `;

  /** The view shown for a row: the newer of the thread's and this card's own. */
  private viewOf(d: DecisionView): DecisionView {
    const own = this.local.get(d.rowId);
    return own ? newerDecisionView(d, own) : d;
  }

  private get views(): DecisionView[] {
    return this.decisions.map((d) => this.viewOf(d));
  }

  override willUpdate(changed: Map<string, unknown>): void {
    if (changed.has('decisions') || changed.has('currentUserId')) {
      if (changed.has('decisions')) {
        this.noteAnsweredElsewhere(changed.get('decisions') as DecisionView[] | undefined);
        // Drop own views the thread has caught up with.
        if (this.local.size > 0) {
          const next = new Map(this.local);
          for (const d of this.decisions) {
            const own = next.get(d.rowId);
            if (own && newerDecisionView(own, d) === d) next.delete(d.rowId);
          }
          if (next.size !== this.local.size) this.local = next;
        }
        void this.renderMarkdown();
      }
      if (this.pendingChange) {
        const row = this.views.find((d) => d.rowId === this.pendingChange?.rowId);
        if (
          !row ||
          !this.rowActionable(row) ||
          row.decision?.provenance !== 'agent_interpreted' ||
          row.decision.choiceId === this.pendingChange.choiceId
        ) {
          this.pendingChange = null;
        }
      }
      if (this.picked.size > 0) {
        const byRow = new Map(this.views.map((d) => [d.rowId, d]));
        let next: Map<string, string> | null = null;
        for (const rowId of this.picked.keys()) {
          const row = byRow.get(rowId);
          if (!row || !this.rowActionable(row)) {
            next ??= new Map(this.picked);
            next.delete(rowId);
          }
        }
        if (next) this.picked = next;
      }
      if (this.setOtherDrafts.size > 0) {
        const byRow = new Map(this.views.map((d) => [d.rowId, d]));
        let next: Map<string, string> | null = null;
        for (const rowId of this.setOtherDrafts.keys()) {
          const row = byRow.get(rowId);
          if (!row || !isOpenDecision(row) || !this.rowActionable(row) || !row.other) {
            next ??= new Map(this.setOtherDrafts);
            next.delete(rowId);
          }
        }
        if (next) this.setOtherDrafts = next;
      }
    }
    if (
      changed.has('decisions') ||
      changed.has('currentUserId') ||
      changed.has('answerability') ||
      changed.has('waitingReasons')
    ) {
      this.actionState.reconcileOpenDismissDialog();
    }
    if (
      this.isConnected &&
      (changed.has('createdAt') ||
        changed.has('decisions') ||
        changed.has('currentUserId') ||
        changed.has('answerability') ||
        changed.has('waitingReasons'))
    ) {
      this.actionState.scheduleDismissUnlockTimer();
    }
    if (
      !this.isSet &&
      (changed.has('decisions') || changed.has('currentUserId') || changed.has('answerability'))
    ) {
      const row = this.views[0];
      if (row && this.isActionableInterpretedOther(row)) {
        const seedKey = `${row.rowId}:${row.decision?.responseMessageId ?? ''}`;
        if (this.interpretedOtherSeedKey !== seedKey) {
          this.interpretedOtherSeedKey = seedKey;
          this.otherDraft = row.decision?.otherText ?? '';
          this.otherOpen = false;
        }
      } else {
        if (this.interpretedOtherSeedKey !== '') {
          this.interpretedOtherSeedKey = '';
          this.otherDraft = '';
          this.otherOpen = false;
        } else if (this.otherOpen || this.otherDraft) {
          if (!row || !isOpenDecision(row) || !this.rowActionable(row) || !row.other) {
            this.otherOpen = false;
            this.otherDraft = '';
          } else if (this.answerBlocked) {
            this.otherOpen = false;
          }
        }
      }
    }
  }

  /**
   * Records rows that were actionable in `prior` and are now answered by
   * someone other than the viewer, while the viewer was choosing.
   */
  private noteAnsweredElsewhere(prior: DecisionView[] | undefined): void {
    if (!prior || !this.isSet || this.setSubmitting) return;
    const before = new Map(prior.map((d) => [d.rowId, this.viewOf(d)]));
    let next: Map<string, string> | null = null;
    for (const d of this.decisions) {
      const was = before.get(d.rowId);
      if (!was || !this.rowActionable(was) || d.status !== 'decided' || !d.decision) continue;
      if (d.decision.answeredByUserId && d.decision.answeredByUserId === this.currentUserId) {
        continue;
      }
      next ??= new Map(this.answeredElsewhere);
      next.set(d.rowId, this.answeredBy(d));
    }
    if (next) this.answeredElsewhere = next;
  }

  /** Renders every question and description not yet rendered. */
  private async renderMarkdown(): Promise<void> {
    const sources: string[] = [];
    for (const d of this.decisions) {
      if (d.question && !this.rendered.has(d.question)) sources.push(d.question);
      for (const c of d.choices ?? []) {
        if (c.description && !this.rendered.has(c.description)) sources.push(c.description);
      }
      if (d.other?.description && !this.rendered.has(d.other.description)) {
        sources.push(d.other.description);
      }
    }
    if (sources.length === 0) return;
    try {
      const renderer = await getMarkdownRenderer();
      const next = new Map(this.rendered);
      // No image from elsewhere: agent-written markdown must not make every
      // viewer's browser fetch from a host the agent chose.
      for (const src of sources) {
        next.set(src, renderer.renderFragment(src, { sameOriginImagesOnly: true }));
      }
      this.rendered = next;
    } catch {
      // Plain text stays shown.
    }
  }

  /**
   * Rendered markdown for `src`, as fresh nodes. Inline (a choice
   * description, which sits inside the choice's button) unwraps a lone
   * paragraph and turns links into their text and images into their alt
   * text: a link inside the button would record the answer when clicked,
   * and nested interactive content is unreachable by keyboard.
   */
  private markdown(src: string, inline: boolean): Node | string {
    const frag = this.rendered.get(src);
    if (!frag) return src;
    const copy = frag.cloneNode(true) as DocumentFragment;
    if (inline) {
      for (const a of Array.from(copy.querySelectorAll('a'))) {
        a.replaceWith(...Array.from(a.childNodes));
      }
      for (const img of Array.from(copy.querySelectorAll('img'))) {
        img.replaceWith(img.getAttribute('alt') ?? '');
      }
      const elements = Array.from(copy.children);
      if (elements.length === 1 && elements[0]?.tagName === 'P') {
        const span = document.createElement('span');
        span.append(...Array.from(elements[0].childNodes));
        return span;
      }
    }
    const wrap = document.createElement('div');
    wrap.append(copy);
    return wrap;
  }

  private answeredBy(d: DecisionView): string {
    const a = d.decision;
    if (!a) return '';
    if (a.answeredByUserId && a.answeredByUserId === this.currentUserId) return 'you';
    return a.answeredByName || a.answeredByEmail || 'someone';
  }

  private dismissedBy(d: DecisionView): string {
    const dm = d.dismissed;
    if (!dm) return 'someone';
    if (dm.byUserId && dm.byUserId === this.currentUserId) return 'you';
    return dm.byName || 'someone';
  }

  private withdrawnBy(d: DecisionView): string {
    return d.withdrawn?.byName || d.askerAgentName || 'the agent';
  }

  private rowStatusClass(d: DecisionView): string {
    return messageActionRowStatusClass(d.status, isClosedUnanswered(d));
  }

  private isWholeSetWithdrawn(views: readonly DecisionView[]): boolean {
    return isWholeSetWithdrawnShared(views, (d) => this.withdrawnBy(d));
  }

  private isWholeSetDismissed(views: readonly DecisionView[]): boolean {
    return isWholeSetDismissedShared(views, (d) => this.dismissedBy(d));
  }

  /** Whether `d` is open and waiting on the signed-in viewer to answer or dismiss. */
  private rowWaitsOnViewer(d: DecisionView): boolean {
    return !this.answerBlocked && !this.waitingReasons?.[d.rowId] && this.rowActionable(d);
  }

  private get isDismissAskerGone(): boolean {
    return (
      this.effectiveAnswerability.state === 'gone' ||
      this.askerGone ||
      this.actionState.dismissProbedGone
    );
  }

  private openDismissDialog(): void {
    if (!this.actionState.openDismissDialog()) return;

    const first = this.views[0];
    if (
      !this.isDismissAskerGone &&
      first?.askerAgentId &&
      (this.surface === 'decisions_page' || this.effectiveAnswerability.state === 'unknown')
    ) {
      this.actionState.probeAskerGone(async () => {
        const ans = await fetchAskerAnswerability(first.askerAgentId, first.askerAgentName);
        return ans.state === 'gone';
      });
    }
  }

  private async confirmDismiss(): Promise<void> {
    if (
      !this.actionState.dismissDialogOpen ||
      this.actionState.dismissSubmitting ||
      !this.messageId
    )
      return;
    const views = this.views;
    const targetIds = this.isSet
      ? views
          .filter(
            (d) =>
              isOpenDecision(d) &&
              this.rowWaitsOnViewer(d) &&
              this.actionState.dismissRemainingMin === 0 &&
              this.actionState.dismissCheckedIds.has(d.rowId)
          )
          .map((d) => d.rowId)
      : views[0] && this.rowWaitsOnViewer(views[0]) && this.actionState.dismissRemainingMin === 0
        ? [views[0].rowId]
        : [];
    if (targetIds.length === 0) return;

    const askerName = views[0]?.askerAgentName || '';
    this.actionState.setDismissSubmitting(true);
    try {
      const res = await dismissDecisionPrompt(this.messageId, {
        decisionIds: targetIds,
        reason: this.actionState.dismissReasonDraft,
        surface: this.surface,
        idempotencyKey: crypto.randomUUID(),
      });
      if (!this.isConnected) return;
      if (res.ok) {
        const updated = [...res.dismissed, ...res.alreadyDismissed];
        this.actionState.closeDismissDialogState();
        this.applyOwnViews(updated, res.responseMessageId);
        const toast = dismissResultToast(
          res.deliveryStatus,
          res.dismissed.length,
          res.alreadyDismissed.length,
          askerName
        );
        showToast(toast.message, toast.variant);
        return;
      }
      if (isOnlyTooRecentDismissError(res)) {
        if (res.availableAt) {
          this.actionState.setDismissAvailableAtOverride(res.availableAt);
          this.dispatchEvent(
            new CustomEvent<{ messageId: string; availableAt: string }>(
              'decision-dismiss-too-recent',
              {
                detail: { messageId: this.messageId, availableAt: res.availableAt },
                bubbles: true,
                composed: true,
              }
            )
          );
        }
        this.actionState.closeDismissDialogState();
        if (res.decisions) {
          this.applyOwnViews(res.decisions);
        }
        this.actionState.scheduleDismissUnlockTimer();
        showToast(
          res.code === 'dismiss_changed'
            ? 'Decisions must be at least 5 minutes old before they can be dismissed.'
            : res.message,
          'warning'
        );
        return;
      }
      if (res.code === 'dismiss_changed') {
        this.actionState.closeDismissDialogState();
        if (res.decisions) {
          this.applyOwnViews(res.decisions);
        }
        showToast('Nothing dismissed: this card changed.', 'warning');
        return;
      }
      if (res.status === 403 && views[0]) {
        this.actionState.closeDismissDialogState();
        this.applyDenied(views[0], res.code, res.reason);
        return;
      }
      showToast(res.message, 'danger');
    } finally {
      this.actionState.setDismissSubmitting(false);
    }
  }

  private get copyLinkUrl(): string | null {
    if (this.surface === 'decisions_page') {
      return this.permalinkHref ? `${window.location.origin}${this.permalinkHref}` : null;
    }
    if (!this.messageId) return null;
    return `${window.location.origin}${window.location.pathname}#msg-${encodeURIComponent(this.messageId)}`;
  }

  private renderCardMenu(views: readonly DecisionView[]): TemplateResult | typeof nothing {
    if (!views.some((d) => this.rowWaitsOnViewer(d))) return nothing;
    const remMin = this.actionState.dismissRemainingMin;
    const linkUrl = this.copyLinkUrl;
    return html`<span class="menu">
      <sl-dropdown class="card-menu" placement="bottom-end" hoist data-card-menu>
        <sl-icon-button
          slot="trigger"
          name="three-dots"
          label="More"
          data-card-menu-trigger
        ></sl-icon-button>
        <sl-menu>
          ${remMin > 0
            ? html`<sl-menu-item value="dismiss" disabled data-dismiss-menu-item>
                <sl-icon slot="prefix" name="x-circle" aria-hidden="true"></sl-icon>
                <sl-tooltip
                  hoist
                  data-dismiss-tooltip
                  content=${`Dismiss available in ${remMin} min`}
                >
                  <span class="menu-item-stack">
                    <span>Dismiss…</span>
                    <span class="menu-sub">Available in ${remMin} min</span>
                  </span>
                </sl-tooltip>
              </sl-menu-item>`
            : html`<sl-menu-item
                value="dismiss"
                data-dismiss-menu-item
                @click=${(): void => this.openDismissDialog()}
              >
                <sl-icon slot="prefix" name="x-circle" aria-hidden="true"></sl-icon>
                Dismiss…
              </sl-menu-item>`}
          ${linkUrl
            ? html`<sl-menu-item
                value="copy-link"
                data-copy-link-menu-item
                @click=${(): void => void copyWithToast(linkUrl)}
              >
                <sl-icon slot="prefix" name="link-45deg" aria-hidden="true"></sl-icon>
                Copy link
              </sl-menu-item>`
            : nothing}
        </sl-menu>
      </sl-dropdown>
    </span>`;
  }

  private whyRowDisabledInDismissDialog(d: DecisionView): string {
    if (this.actionState.dismissRemainingMin > 0 && this.rowWaitsOnViewer(d)) {
      const availMs = this.actionState.dismissAvailableAtMsEffective;
      return availMs > 0
        ? `Available at ${formatInstant(new Date(availMs).toISOString(), 'time')}`
        : 'Too recent to dismiss';
    }
    const wReason = this.waitingReasons?.[d.rowId];
    const actReason = actionableReason(d, this.currentUserId);
    if (actReason === 'not_recorded_answerer' || wReason === 'other_user_confirm') {
      return `Waiting on ${this.ownerLabel(d)} to confirm ${this.interpreterName(d)}'s reading`;
    }
    if (wReason === 'read_only') return 'Read-only for you';
    if (wReason === 'message_denied') return 'Messaging restricted for you';
    return 'Waiting on someone else';
  }

  private renderDismissDialog(views: readonly DecisionView[]): TemplateResult | typeof nothing {
    if (!this.actionState.dismissDialogOpen || views.length === 0) return nothing;
    const isSet = this.isSet;
    const openRows = views.filter((d) => isOpenDecision(d));
    const checkedCount = isSet
      ? openRows.filter(
          (d) =>
            this.rowWaitsOnViewer(d) &&
            this.actionState.dismissRemainingMin === 0 &&
            this.actionState.dismissCheckedIds.has(d.rowId)
        ).length
      : views[0] && this.rowWaitsOnViewer(views[0]) && this.actionState.dismissRemainingMin === 0
        ? 1
        : 0;
    const dialogLabel = isSet ? 'Dismiss decisions' : 'Dismiss decision';
    const confirmLabel = !isSet
      ? 'Dismiss'
      : checkedCount === 1
        ? 'Dismiss 1 decision'
        : checkedCount > 1
          ? `Dismiss ${checkedCount} decisions`
          : 'Dismiss';
    const askerName = views[0]?.askerAgentName || 'The agent';
    const gone = this.isDismissAskerGone;
    const where = this.conversationLabel.trim();
    const helpText = gone
      ? 'Plain text, up to 500 characters. Shown on the card.'
      : `Plain text, up to 500 characters. Sent to ${askerName} and shown on the card.`;
    const reasonLen = [...this.actionState.dismissReasonDraft].length;
    const singleRow = views[0];

    return html`
      <sl-dialog
        class="dismiss-dialog"
        data-dismiss-dialog
        label=${dialogLabel}
        open
        @sl-request-close=${(e: Event): void => {
          if (this.actionState.dismissSubmitting) {
            e.preventDefault();
            return;
          }
          this.actionState.closeDismissDialog();
        }}
      >
        <span slot="label" class="dismiss-dialog-title">
          <sl-icon name="x-circle" aria-hidden="true"></sl-icon>
          <span>${dialogLabel}</span>
        </span>
        <div class="dismiss-dialog-body">
          ${isSet
            ? html`
                <div class="dismiss-dialog-note">
                  Dismissed decisions close for everyone and cannot be answered. This cannot be
                  undone.
                </div>
                <div class="dismiss-row-list">
                  ${openRows.map((d) => {
                    const pickable =
                      this.rowWaitsOnViewer(d) && this.actionState.dismissRemainingMin === 0;
                    if (pickable) {
                      const checked = this.actionState.dismissCheckedIds.has(d.rowId);
                      return html`
                        <div class="dismiss-pick" data-dismiss-row=${d.rowId}>
                          <sl-checkbox
                            data-dismiss-check=${d.rowId}
                            ?checked=${checked}
                            ?disabled=${this.actionState.dismissSubmitting}
                            @sl-change=${(e: Event): void =>
                              this.actionState.onDismissRowToggle(d.rowId, e)}
                          >
                            <span class="dismiss-pick-text">
                              ${d.key
                                ? html`<span class="key">${d.key}</span> `
                                : nothing}${this.markdown(d.question, true)}
                              ${d.status === 'decided' &&
                              d.decision?.provenance === 'agent_interpreted'
                                ? html`<span class="dismiss-why"
                                    >Interpreted as “${d.decision.label}” from your reply</span
                                  >`
                                : nothing}
                            </span>
                          </sl-checkbox>
                        </div>
                      `;
                    }
                    return html`
                      <div class="dismiss-pick off" data-dismiss-row=${d.rowId}>
                        <sl-checkbox disabled>
                          <span class="dismiss-pick-text">
                            ${d.key
                              ? html`<span class="key">${d.key}</span> `
                              : nothing}${this.markdown(d.question, true)}
                            <span class="dismiss-why"
                              >${this.whyRowDisabledInDismissDialog(d)}</span
                            >
                          </span>
                        </sl-checkbox>
                      </div>
                    `;
                  })}
                </div>
              `
            : html`
                ${singleRow
                  ? html`<div class="dismiss-single-row" data-dismiss-row=${singleRow.rowId}>
                      ${singleRow.key ? html`<span class="key">${singleRow.key}</span>` : nothing}
                      <span>${this.markdown(singleRow.question, true)}</span>
                    </div>`
                  : nothing}
                <div class="dismiss-dialog-note">
                  A dismissed decision closes for everyone and cannot be answered. This cannot be
                  undone.
                </div>
              `}
          <sl-textarea
            size="small"
            rows="2"
            label="Reason (optional)"
            help-text=${helpText}
            placeholder="Why it no longer applies"
            data-dismiss-reason
            ?disabled=${this.actionState.dismissSubmitting}
            .value=${this.actionState.dismissReasonDraft}
            @sl-input=${this.actionState.onDismissReasonInput}
            @input=${this.actionState.onDismissReasonInput}
          >
            <span slot="help-text" class="dismiss-reason-help">
              <span>${helpText}</span>
              <span class="dismiss-reason-count" data-dismiss-reason-count
                >${reasonLen}/${MAX_DISMISS_REASON_RUNES}</span
              >
            </span>
          </sl-textarea>
          ${gone
            ? html`<div class="dismiss-tell" data-dismiss-tell="gone">
                <sl-icon name="person-x" aria-hidden="true"></sl-icon>
                <span>${askerName} is gone; nothing will be sent.</span>
              </div>`
            : html`<div class="dismiss-tell" data-dismiss-tell="live">
                <sl-icon name="send" aria-hidden="true"></sl-icon>
                <span
                  >${where
                    ? `${askerName} will be told in ${where}.`
                    : `${askerName} will be told.`}</span
                >
              </div>`}
        </div>
        <div slot="footer" class="dismiss-dialog-footer">
          <sl-button
            size="small"
            data-dismiss-cancel
            ?disabled=${this.actionState.dismissSubmitting}
            @click=${this.actionState.closeDismissDialog}
          >
            Cancel
          </sl-button>
          <sl-button
            size="small"
            variant="danger"
            outline
            data-dismiss-confirm
            ?loading=${this.actionState.dismissSubmitting}
            ?disabled=${this.actionState.dismissSubmitting || checkedCount === 0}
            @click=${(): void => void this.confirmDismiss()}
          >
            ${confirmLabel}
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private ownerLabel(d: DecisionView): string {
    const a = d.decision;
    return a?.answeredByName || a?.answeredByEmail || 'the recorded answerer';
  }

  private notOwnerTooltip(d: DecisionView): string {
    return `Only ${this.ownerLabel(d)} can confirm or change this answer`;
  }

  /** Whether `d` can be answered or confirmed/changed by the current viewer. */
  private rowActionable(d: DecisionView): boolean {
    return actionableReason(d, this.currentUserId) === '';
  }

  /** The viewer's answerability: this card's own 403 wins over the thread's. */
  get effectiveAnswerability(): AskerAnswerability {
    return this.denied ?? this.answerability ?? ANSWERABLE;
  }

  /**
   * Whether the viewer cannot answer at all: every choice, and a set's
   * Submit, is disabled and `renderAnswerBlocked()` says why.
   */
  get answerBlocked(): boolean {
    return this.effectiveAnswerability.state === 'blocked';
  }

  private actionable(d: DecisionView): boolean {
    return this.rowActionable(d) && !this.submitting && !this.answerBlocked;
  }

  private isActionableInterpreted(d: DecisionView): boolean {
    return (
      d.status === 'decided' &&
      d.decision?.provenance === 'agent_interpreted' &&
      this.rowActionable(d) &&
      !this.answerBlocked
    );
  }

  private isActionableInterpretedOther(d: DecisionView): boolean {
    return this.isActionableInterpreted(d) && isOtherAnswer(d.decision);
  }

  /** Enter on the focused card (not on a control inside it) picks the recommended choice on pending cards only. */
  private onCardKeydown(e: KeyboardEvent, d: DecisionView): void {
    if (e.key !== 'Enter' || e.target !== e.currentTarget || this.otherOpen) return;
    if (d.status !== 'pending' || !d.defaultChoiceId || !this.actionable(d)) return;
    e.preventDefault();
    void this.choose(d, d.defaultChoiceId);
  }

  private onSingleChoiceClick(d: DecisionView, choiceId: string): void {
    if (!this.actionable(d)) return;
    if (d.status === 'decided' && d.decision?.provenance === 'agent_interpreted') {
      this.otherOpen = false;
      if (!isOtherAnswer(d.decision)) {
        this.otherDraft = '';
      }
      if (choiceId === d.decision.choiceId) {
        if (this.pendingChange?.rowId === d.rowId) {
          this.pendingChange = null;
        }
        return;
      }
      this.pendingChange = { rowId: d.rowId, choiceId };
      return;
    }
    this.otherOpen = false;
    this.otherDraft = '';
    void this.choose(d, choiceId);
  }

  private async openOther(d: DecisionView): Promise<void> {
    const canOpen =
      this.actionable(d) &&
      (d.status === 'pending' || this.isActionableInterpreted(d)) &&
      (!!d.other || isOtherAnswer(d.decision));
    if (!canOpen) return;
    this.pendingChange = null;
    if (!this.isActionableInterpretedOther(d)) {
      this.otherOpen = true;
    }
    await this.updateComplete;
    const input = this.shadowRoot?.querySelector<
      HTMLElement & { updateComplete?: Promise<unknown> }
    >('.editor sl-input');
    if (input?.updateComplete) {
      await input.updateComplete;
    }
    focusElement(input);
  }

  private async cancelOther(d?: DecisionView): Promise<void> {
    if (d && this.isActionableInterpretedOther(d)) {
      this.otherDraft = d.decision?.otherText ?? '';
    } else {
      this.otherOpen = false;
      this.otherDraft = '';
    }
    await this.updateComplete;
    const btn = this.shadowRoot?.querySelector<HTMLElement>(`[data-choice="${OTHER_CHOICE_ID}"]`);
    focusElement(btn);
  }

  private async submitOther(d: DecisionView): Promise<void> {
    const canSubmit =
      this.actionable(d) &&
      (d.status === 'pending' || this.isActionableInterpreted(d)) &&
      (!!d.other || isOtherAnswer(d.decision));
    if (!canSubmit) return;
    const trimmed = this.otherDraft.trim();
    if (!trimmed || [...trimmed].length > 200) return;
    await this.choose(d, OTHER_CHOICE_ID, trimmed);
  }

  private async choose(d: DecisionView, choiceId: string, otherText?: string): Promise<void> {
    if (!this.actionable(d)) return;
    this.submitting = { rowId: d.rowId, choiceId };
    try {
      const res = await answerDecision(
        d.rowId,
        choiceId,
        crypto.randomUUID(),
        this.surface,
        otherText
      );
      if (res.ok) {
        this.pendingChange = null;
        this.otherOpen = false;
        this.otherDraft = '';
        this.applyOwn(res.decision, res.responseMessageId);
        return;
      }
      if (res.decision) {
        // 409 (`already_answered`, `replaced`, `withdrawn`) or Hub-shaped 403
        // (`not_recorded_answerer`, where `writeNotRecordedAnswerer` includes
        // `details.decision`): refresh the card to the authoritative view (read-only
        // for non-owner, not blocked) and warn.
        this.pendingChange = null;
        this.otherOpen = false;
        this.otherDraft = '';
        this.applyOwn(res.decision);
        if (res.code === 'already_answered') {
          const who = this.answeredBy(res.decision);
          showToast(`Already answered by ${who}.`, 'warning');
        } else if (res.code === 'replaced') {
          const v = res.replacedBy?.version ?? res.decision.replacedBy?.version;
          showToast(v ? `Replaced by v${v}.` : 'This decision was replaced.', 'warning');
        } else if (res.code === 'dismissed') {
          const who = this.dismissedBy(res.decision);
          showToast(
            who === 'you' ? 'You dismissed this decision.' : `${who} dismissed this decision.`,
            'warning'
          );
        } else if (res.code === 'withdrawn') {
          const who = this.withdrawnBy(res.decision);
          showToast(`No longer needed: ${who} withdrew this decision`, 'warning');
        } else {
          showToast(res.message, 'warning');
        }
        return;
      }
      if (res.status === 403) {
        this.otherOpen = false;
        this.applyDenied(d, res.code, res.reason);
        return;
      }
      showToast(res.message, 'danger');
    } finally {
      this.submitting = null;
    }
  }

  /**
   * A 403 from an answer endpoint, single or set (design §4.5 step 3):
   * nothing was recorded. Blocks the card with the reason, toasts it, and
   * tells the thread so the asker's other prompts (or the whole read-only
   * thread) show it too.
   */
  private applyDenied(d: DecisionView, code: string, reason: string | undefined): void {
    const answerability = answerabilityFromDenial(code, reason, d.askerAgentName);
    this.denied = answerability;
    showToast(blockedText(answerability), 'warning');
    const detail: DecisionAnswerDeniedDetail = {
      messageId: this.messageId,
      askerAgentId: d.askerAgentId,
      code,
      ...(reason ? { reason } : {}),
      answerability,
    };
    this.dispatchEvent(
      new CustomEvent<DecisionAnswerDeniedDetail>('decision-answer-denied', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  private applyOwn(view: DecisionView, responseMessageId?: string): void {
    const next = new Map(this.local);
    next.set(view.rowId, view);
    this.local = next;
    const detail: DecisionAnsweredDetail = {
      messageId: this.messageId,
      decisions: [view],
      ...(responseMessageId ? { responseMessageId } : {}),
    };
    this.dispatchEvent(
      new CustomEvent<DecisionAnsweredDetail>('decision-answered', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  /** Whether this card is a set (design §4.3): answerMode=set, or 2+ decisions. */
  private get isSet(): boolean {
    return this.answerMode === 'set' || this.decisions.length > 1;
  }

  override render(): TemplateResult | typeof nothing {
    if (this.isSet) return this.renderSet();
    // Single mode (design §4.1): the card holds one decision.
    const d = this.views[0];
    if (!d) return nothing;
    const blocked = this.answerBlocked && this.rowActionable(d);
    const pendingWithDefault =
      d.status === 'pending' && !!d.defaultChoiceId && !blocked && !this.otherOpen;
    const showEnterHint = pendingWithDefault && !this.touchPrimary.isTouch;
    return html`
      <div
        class="card"
        role="group"
        tabindex="0"
        aria-label=${plainText(d.question)}
        aria-disabled=${blocked ? 'true' : nothing}
        aria-describedby=${showEnterHint ? this.hintId : blocked ? this.reasonId : nothing}
        @keydown=${(e: KeyboardEvent) => this.onCardKeydown(e, d)}
        @contextmenu=${this.onCardContextMenu}
      >
        <div class="card-head">
          <span class="card-head-lead">
            <sl-icon name="signpost-split"></sl-icon>
            <span class="title">Decision</span>
            ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
            ${d.version > 1 || d.status === 'replaced' || !!d.replacedBy
              ? html`<span class="version">v${d.version}</span>`
              : nothing}
          </span>
          ${this.renderSingleBadge(d)} ${this.renderCardMenu([d])}
        </div>
        ${this.renderRow(d, showEnterHint)} ${this.renderAskerGoneNote()}
      </div>
      ${this.renderDismissDialog([d])}
    `;
  }

  private renderSingleBadge(d: DecisionView): TemplateResult | typeof nothing {
    if (d.status !== 'decided' || !d.decision) return nothing;
    if (d.decision.provenance === 'agent_interpreted') {
      return html`<span class="spacer"></span
        ><span class="badge interp"
          ><sl-icon name="robot" aria-hidden="true"></sl-icon>interpreted</span
        >`;
    }
    if (d.decision.provenance === 'direct' && d.decision.outcome === 'confirm') {
      return html`<span class="spacer"></span
        ><span class="badge conf"
          ><sl-icon name="patch-check" aria-hidden="true"></sl-icon>confirmed</span
        >`;
    }
    if (d.decision.provenance === 'direct' && d.decision.outcome === 'override') {
      return html`<span class="spacer"></span
        ><span class="badge over"
          ><sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>changed</span
        >`;
    }
    return nothing;
  }

  private renderRow(d: DecisionView, showHint: boolean): TemplateResult {
    const interpreted = d.status === 'decided' && d.decision?.provenance === 'agent_interpreted';
    const interpretedOther = interpreted && isOtherAnswer(d.decision);
    const chosenOther = d.status === 'decided' && isOtherAnswer(d.decision) && !interpreted;
    const otherOpt: OtherOption | undefined =
      d.other ??
      (chosenOther || interpretedOther ? { label: d.decision?.label || 'Other' } : undefined);
    const stacked = (d.choices ?? []).some((c) => !!c.description) || !!otherOpt?.description;
    const stagingFixedFromInterpOther = interpretedOther && this.pendingChange?.rowId === d.rowId;
    const showInterpretedOtherEditor =
      this.isActionableInterpretedOther(d) && !stagingFixedFromInterpOther;
    const showPendingOrFixedToOtherEditor =
      this.otherOpen &&
      !this.answerBlocked &&
      !!otherOpt &&
      (d.status === 'pending' || (this.isActionableInterpreted(d) && !interpretedOther));
    const editorOpen = showInterpretedOtherEditor || showPendingOrFixedToOtherEditor;
    const showStaticOtherText =
      (chosenOther || (interpretedOther && !showInterpretedOtherEditor)) && !!d.decision?.otherText;
    return html`
      <div class="row${this.rowStatusClass(d)}" data-row=${d.rowId} data-status=${d.status}>
        <div class="q">${this.markdown(d.question, false)}</div>
        <div class="choices${stacked ? ' stack' : ''}">
          ${(d.choices ?? []).map((c) =>
            stacked ? this.renderOptRow(d, c) : this.renderButton(d, c)
          )}
          ${otherOpt
            ? stacked
              ? this.renderOtherOptRow(d, otherOpt, editorOpen, chosenOther, interpretedOther)
              : this.renderOtherButton(d, otherOpt, editorOpen, chosenOther, interpretedOther)
            : nothing}
        </div>
        ${!stacked && editorOpen && otherOpt ? this.renderOtherEditor(d, otherOpt) : nothing}
        ${showStaticOtherText ? html`<div class="otext">“${d.decision!.otherText}”</div>` : nothing}
        ${this.renderSingleActionsAndFoot(d)}
        ${showHint && !this.submitting
          ? html`<div class="hint" id=${this.hintId}>
              ${this.isSingleChoiceOnly(d)
                ? 'Press Enter to confirm.'
                : 'Press Enter to choose the recommended answer (outlined).'}
            </div>`
          : nothing}
      </div>
    `;
  }

  private renderOtherButton(
    d: DecisionView,
    other: OtherOption,
    editorOpen: boolean,
    chosenOther: boolean,
    interpretedOther: boolean
  ): TemplateResult {
    if (chosenOther) {
      const label = d.decision?.label || other.label;
      return html`<sl-button
        size="small"
        data-choice=${OTHER_CHOICE_ID}
        variant="primary"
        data-readonly
        title=${label}
        ><sl-icon slot="prefix" name="check-circle-fill"></sl-icon>${label}</sl-button
      >`;
    }
    const loading =
      this.submitting?.rowId === d.rowId && this.submitting.choiceId === OTHER_CHOICE_ID;
    if (interpretedOther) {
      const label = d.decision?.label || other.label;
      const reason = actionableReason(d, this.currentUserId);
      const notOwner = reason === 'not_recorded_answerer';
      if (notOwner) {
        const tip = this.notOwnerTooltip(d);
        return html`<sl-tooltip content=${tip}
          ><sl-button
            size="small"
            data-choice=${OTHER_CHOICE_ID}
            variant="primary"
            disabled
            aria-disabled="true"
            title=${tip}
            ><sl-icon slot="prefix" name="robot"></sl-icon>${label}</sl-button
          ></sl-tooltip
        >`;
      }
      const disabled = (!loading && !!this.submitting) || this.answerBlocked;
      const stagingFixed = this.pendingChange?.rowId === d.rowId;
      return html`<sl-button
        size="small"
        data-choice=${OTHER_CHOICE_ID}
        variant=${stagingFixed ? 'default' : 'primary'}
        ?outline=${!stagingFixed || this.answerBlocked}
        ?loading=${loading}
        ?disabled=${disabled}
        aria-disabled=${disabled ? 'true' : nothing}
        title=${label}
        @click=${() => {
          if (!disabled) void this.openOther(d);
        }}
        ><sl-icon
          slot="prefix"
          name=${stagingFixed || this.answerBlocked ? 'robot' : 'pencil'}
        ></sl-icon
        >${label}</sl-button
      >`;
    }
    const canOpen =
      (d.status === 'pending' || this.isActionableInterpreted(d)) && this.actionable(d);
    const disabled = !canOpen && !loading;
    return html`<sl-button
      size="small"
      class=${editorOpen ? '' : 'other-btn'}
      data-choice=${OTHER_CHOICE_ID}
      variant=${editorOpen ? 'primary' : 'default'}
      ?outline=${editorOpen}
      ?loading=${loading}
      ?disabled=${disabled}
      aria-disabled=${disabled ? 'true' : nothing}
      title=${other.label}
      @click=${() => {
        if (canOpen) void this.openOther(d);
      }}
      ><sl-icon slot="prefix" name="pencil"></sl-icon>${other.label}…</sl-button
    >`;
  }

  private renderOtherOptRow(
    d: DecisionView,
    other: OtherOption,
    editorOpen: boolean,
    chosenOther: boolean,
    interpretedOther: boolean
  ): TemplateResult {
    const loading =
      this.submitting?.rowId === d.rowId && this.submitting.choiceId === OTHER_CHOICE_ID;
    if (editorOpen) {
      return html`<div class="opt sel" data-choice=${OTHER_CHOICE_ID}>
        <div class="opt-head">
          <span class="mark"
            >${loading
              ? html`<sl-spinner></sl-spinner>`
              : html`<sl-icon name="pencil"></sl-icon>`}</span
          >
          <span>
            <span class="lbl"
              >${interpretedOther ? d.decision?.label || other.label : `${other.label}…`}</span
            >
            ${other.description
              ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
              : nothing}
          </span>
        </div>
        <div class="editor inset">${this.renderOtherEditorInner(d, other, true)}</div>
      </div>`;
    }
    if (chosenOther) {
      const label = d.decision?.label || other.label;
      return html`<button
        type="button"
        class="opt chosen"
        data-choice=${OTHER_CHOICE_ID}
        data-readonly
        aria-disabled="false"
        title=${label}
      >
        <span class="mark"><sl-icon name="check-circle-fill"></sl-icon></span>
        <span>
          <span class="lbl">${label}</span>
          ${other.description
            ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
            : nothing}
        </span>
      </button>`;
    }
    if (interpretedOther) {
      const label = d.decision?.label || other.label;
      const reason = actionableReason(d, this.currentUserId);
      const notOwner = reason === 'not_recorded_answerer';
      const disabled = notOwner || (!loading && !!this.submitting) || this.answerBlocked;
      const tip = notOwner ? this.notOwnerTooltip(d) : label;
      const cls = notOwner ? 'opt chosen' : 'opt recd';
      const btn = html`<button
        type="button"
        class=${cls}
        data-choice=${OTHER_CHOICE_ID}
        ?disabled=${notOwner}
        aria-disabled=${disabled || loading ? 'true' : 'false'}
        aria-busy=${loading ? 'true' : nothing}
        title=${tip}
        @click=${() => {
          if (!disabled) void this.openOther(d);
        }}
      >
        <span class="mark"><sl-icon name="robot"></sl-icon></span>
        <span>
          <span class="lbl">${label}</span>
          ${other.description
            ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
            : nothing}
        </span>
      </button>`;
      return notOwner ? html`<sl-tooltip content=${tip}>${btn}</sl-tooltip>` : btn;
    }
    const canOpen =
      (d.status === 'pending' || this.isActionableInterpreted(d)) && this.actionable(d);
    const disabled = !canOpen && !loading;
    const notOwner = actionableReason(d, this.currentUserId) === 'not_recorded_answerer';
    return html`<button
      type="button"
      class="opt other-opt"
      data-choice=${OTHER_CHOICE_ID}
      ?disabled=${disabled && notOwner}
      aria-disabled=${disabled || loading ? 'true' : 'false'}
      aria-busy=${loading ? 'true' : nothing}
      title=${other.label}
      @click=${() => {
        if (canOpen) void this.openOther(d);
      }}
    >
      <span class="mark"
        >${loading
          ? html`<sl-spinner></sl-spinner>`
          : html`<sl-icon name="pencil"></sl-icon>`}</span
      >
      <span>
        <span class="lbl">${other.label}…</span>
        ${other.description
          ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
          : nothing}
      </span>
    </button>`;
  }

  private renderOtherEditor(d: DecisionView, other: OtherOption): TemplateResult {
    return html`<div class="editor">${this.renderOtherEditorInner(d, other, false)}</div>`;
  }

  private renderOtherEditorInner(
    d: DecisionView,
    other: OtherOption,
    inset: boolean
  ): TemplateResult {
    const trimmed = this.otherDraft.trim();
    const trimmedLen = [...trimmed].length;
    const showCounter = trimmedLen >= 160;
    const warnCounter = trimmedLen >= 180 && trimmedLen <= 200;
    const overCounter = trimmedLen > 200;
    const announceCounter = warnCounter || overCounter;
    const loading =
      this.submitting?.rowId === d.rowId && this.submitting.choiceId === OTHER_CHOICE_ID;
    const canSend = trimmedLen > 0 && trimmedLen <= 200 && !this.submitting;
    const isInterpreted = d.status === 'decided' && d.decision?.provenance === 'agent_interpreted';
    const counter = showCounter
      ? html`<span
          class="cnt${overCounter ? ' over' : warnCounter ? ' warn' : ''}"
          role=${announceCounter ? 'status' : nothing}
          aria-live=${announceCounter ? 'polite' : nothing}
          >${trimmedLen} / 200</span
        >`
      : nothing;
    return html`
      ${(!inset && !isInterpreted) || showCounter
        ? html`<div class="editor-head">
            ${!inset && !isInterpreted ? html`<span class="lab">${other.label}</span>` : nothing}
            ${counter}
          </div>`
        : nothing}
      <div class="ctl">
        <sl-input
          size="small"
          label=${plainText(d.question)}
          placeholder=${other.placeholder || 'Type your answer'}
          .value=${this.otherDraft}
          ?disabled=${!!this.submitting}
          @sl-input=${(e: Event) => {
            this.otherDraft = (e.target as HTMLInputElement).value ?? '';
          }}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' && !e.isComposing) {
              e.preventDefault();
              e.stopPropagation();
              void this.submitOther(d);
            } else if (e.key === 'Escape') {
              e.preventDefault();
              e.stopPropagation();
              void this.cancelOther(d);
            }
          }}
        ></sl-input>
        ${isInterpreted
          ? nothing
          : html`<sl-button
                size="small"
                variant="primary"
                data-action="send-other"
                ?loading=${loading}
                ?disabled=${!canSend}
                aria-disabled=${!canSend ? 'true' : nothing}
                @click=${() => void this.submitOther(d)}
                >Send</sl-button
              >
              <sl-button
                size="small"
                variant="text"
                data-action="cancel-other"
                ?disabled=${!!this.submitting}
                @click=${() => void this.cancelOther(d)}
                >Cancel</sl-button
              >`}
      </div>
    `;
  }

  /** True when a decision has exactly one fixed choice and no Other option (confirmation/approval). */
  private isSingleChoiceOnly(d: DecisionView): boolean {
    return (d.choices?.length ?? 0) === 1 && !d.other;
  }

  private singleChoiceAppearance(
    d: DecisionView,
    c: ChoiceOption
  ): {
    variant: 'primary' | 'default';
    outlined: boolean;
    icon: string;
    loading: boolean;
    disabled: boolean;
    nativeDisabled: boolean;
    pressed?: boolean;
    recommended: boolean;
    title: string;
    notOwnerTip?: string;
    clickable: boolean;
  } {
    const loading = this.submitting?.rowId === d.rowId && this.submitting.choiceId === c.id;
    const recommended = d.status === 'pending' && d.defaultChoiceId === c.id;
    const reason = actionableReason(d, this.currentUserId);
    const notOwner = reason === 'not_recorded_answerer';
    const title = notOwner
      ? this.notOwnerTooltip(d)
      : recommended
        ? `${c.label} (recommended)`
        : c.label;

    if (d.status === 'decided' && d.decision?.provenance === 'agent_interpreted') {
      const isInterpreted = d.decision.choiceId === c.id;
      if (notOwner) {
        return {
          variant: isInterpreted ? 'primary' : 'default',
          outlined: false,
          icon: isInterpreted ? 'robot' : '',
          loading: false,
          disabled: true,
          nativeDisabled: true,
          recommended: false,
          title,
          ...(isInterpreted ? { notOwnerTip: title } : {}),
          clickable: false,
        };
      }
      if (reason === '') {
        const disabled = (!loading && !!this.submitting) || this.answerBlocked;
        const changingTo =
          !this.answerBlocked && this.pendingChange?.rowId === d.rowId
            ? this.pendingChange.choiceId
            : undefined;
        if (changingTo !== undefined) {
          const isTarget = changingTo === c.id;
          return {
            variant: isTarget ? 'primary' : 'default',
            outlined: !isTarget && isInterpreted,
            icon: isTarget ? 'check-lg' : isInterpreted ? 'robot' : '',
            loading,
            disabled,
            nativeDisabled: false,
            ...(isTarget ? { pressed: true } : {}),
            recommended: false,
            title,
            clickable: !disabled && !isTarget,
          };
        }
        return {
          variant: isInterpreted && !this.answerBlocked ? 'primary' : 'default',
          outlined: isInterpreted && this.answerBlocked,
          icon: isInterpreted ? 'robot' : '',
          loading,
          disabled,
          nativeDisabled: false,
          recommended: false,
          title,
          clickable: !disabled && !isInterpreted,
        };
      }
    }

    const chosen = d.status === 'decided' && d.decision?.choiceId === c.id;
    const disabled = !chosen && !loading && !this.actionable(d);
    const singleChoicePending = d.status === 'pending' && this.isSingleChoiceOnly(d);
    const outlined = recommended && !chosen && !singleChoicePending;
    return {
      variant: chosen || outlined || singleChoicePending ? 'primary' : 'default',
      outlined,
      icon: chosen ? 'check-circle-fill' : '',
      loading,
      disabled,
      nativeDisabled: disabled && notOwner,
      recommended,
      title,
      clickable: !chosen && !disabled,
    };
  }

  /**
   * Suppress touch long-press `contextmenu` on choice controls so reading a
   * multi-line option with a resting finger neither triggers native selection
   * nor bubbles to `scion-chat-message`'s context sheet. Desktop right-clicks,
   * keyboard ContextMenu, and external links still pass through.
   */
  private onCardContextMenu = (e: MouseEvent): void => {
    const fromKeyboard =
      (e.clientX === 0 && e.clientY === 0) ||
      (typeof PointerEvent !== 'undefined' && e instanceof PointerEvent && e.pointerType === '');
    if (fromKeyboard) return;
    const isTouch =
      (typeof PointerEvent !== 'undefined' &&
        e instanceof PointerEvent &&
        e.pointerType !== 'mouse') ||
      (typeof window.matchMedia === 'function' && window.matchMedia('(hover: none)').matches);
    if (!isTouch) return;
    for (const node of e.composedPath()) {
      if (node === e.currentTarget) break;
      if (node instanceof HTMLAnchorElement && /^(https?:|mailto:)/i.test(node.href)) return;
      if (
        node instanceof HTMLInputElement ||
        (node instanceof HTMLElement && node.tagName === 'SL-INPUT')
      ) {
        return;
      }
    }
    e.preventDefault();
    e.stopPropagation();
  };

  private renderButton(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.singleChoiceAppearance(d, c);
    const btn = html`<sl-button
      size="small"
      data-choice=${c.id}
      variant=${s.variant}
      ?outline=${s.outlined}
      ?loading=${s.loading}
      ?disabled=${s.disabled}
      ?data-readonly=${!s.clickable && !s.disabled && !s.loading}
      aria-disabled=${s.disabled ? 'true' : nothing}
      aria-pressed=${s.pressed ? 'true' : nothing}
      title=${s.title}
      @click=${() => {
        if (s.clickable) this.onSingleChoiceClick(d, c.id);
      }}
      >${s.icon
        ? html`<sl-icon slot="prefix" name=${s.icon}></sl-icon>`
        : nothing}${c.label}${s.recommended
        ? html`<span class="sr-only"> (recommended)</span>`
        : nothing}</sl-button
    >`;
    return s.notOwnerTip ? html`<sl-tooltip content=${s.notOwnerTip}>${btn}</sl-tooltip>` : btn;
  }

  private renderOptRow(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.singleChoiceAppearance(d, c);
    const singleChoicePending = d.status === 'pending' && this.isSingleChoiceOnly(d);
    const filled = s.variant === 'primary' && !s.outlined && !singleChoicePending;
    const cls = ['opt', s.outlined || singleChoicePending ? 'recd' : '', filled ? 'chosen' : '']
      .filter(Boolean)
      .join(' ');
    const btn = html`<button
      type="button"
      class=${cls}
      data-choice=${c.id}
      ?disabled=${s.nativeDisabled}
      ?data-readonly=${!s.clickable && !s.disabled && !s.loading}
      aria-disabled=${s.disabled || s.loading ? 'true' : 'false'}
      aria-pressed=${s.pressed ? 'true' : nothing}
      aria-busy=${s.loading ? 'true' : nothing}
      title=${s.title}
      @click=${() => {
        if (s.clickable) this.onSingleChoiceClick(d, c.id);
      }}
    >
      <span class="mark"
        >${s.icon
          ? html`<sl-icon name=${s.icon}></sl-icon>`
          : s.loading
            ? html`<sl-spinner></sl-spinner>`
            : nothing}</span
      >
      <span>
        <span class="lbl">${c.label}</span>${s.recommended
          ? html`<span class="sr-only"> (recommended)</span>`
          : nothing}
        ${c.description
          ? html`<span class="desc">${this.markdown(c.description, true)}</span>`
          : nothing}
      </span>
    </button>`;
    return s.notOwnerTip ? html`<sl-tooltip content=${s.notOwnerTip}>${btn}</sl-tooltip>` : btn;
  }

  /**
   * Why the viewer cannot answer (design §4.3 "Pending, blocked"): visible
   * text, not a tooltip, with `lock` (or `eye` for a read-only thread).
   * Set mode renders it once under the rows.
   */
  renderAnswerBlocked(): TemplateResult | typeof nothing {
    const a = this.effectiveAnswerability;
    if (a.state !== 'blocked') return nothing;
    return html`<div
      class="reason"
      id=${this.reasonId}
      data-reason=${a.readOnly ? 'read-only' : 'denied'}
    >
      <sl-icon name=${a.readOnly ? 'eye' : 'lock'} aria-hidden="true"></sl-icon>
      <span
        >${a.askerName ? html`You can't answer <b>${a.askerName}</b>: ` : nothing}${a.reason ??
        ''}</span
      >
    </div>`;
  }

  /**
   * The asker no longer exists (design §4.3 "Asker gone"): still
   * answerable, the hub records the answer without delivering it. Single
   * and set cards.
   */
  renderAskerGoneNote(): TemplateResult | typeof nothing {
    if (this.effectiveAnswerability.state !== 'gone') return nothing;
    const views = this.views;
    if (!views.some((d) => this.rowActionable(d))) return nothing;
    const name = views[0]?.askerAgentName || 'The agent';
    return html`<div class="note" role="note" data-note="asker-gone">
      <sl-icon name="info-circle" aria-hidden="true"></sl-icon>
      <span
        ><b>${name}</b> is no longer running. Your answer will be recorded but not delivered.</span
      >
    </div>`;
  }

  private renderSingleActionsAndFoot(d: DecisionView): TemplateResult | typeof nothing {
    if (this.submitting?.rowId === d.rowId) {
      return this.actionState.renderSubmittingBanner('Sending…');
    }
    if (
      d.status === 'decided' &&
      d.decision?.provenance === 'agent_interpreted' &&
      this.rowActionable(d)
    ) {
      if (this.answerBlocked) {
        return html`${this.renderTerminalFoot(d)}${this.renderAnswerBlocked()}`;
      }
      const origIsOther = isOtherAnswer(d.decision);
      const origDisplay = html`<b>${d.decision.label}</b>${origIsOther && d.decision.otherText
          ? html` “${d.decision.otherText}”`
          : nothing}`;
      if (this.pendingChange?.rowId === d.rowId) {
        const target = d.choices.find((c) => c.id === this.pendingChange?.choiceId);
        if (target) {
          return html`<div class="confirmbar" role="region" aria-label="Confirm answer change">
            <sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>
            <span role="status" aria-live="polite"
              >Change your answer from ${origDisplay} to <b>${target.label}</b>?
              ${d.askerAgentName || 'The agent'} will be told.</span
            >
            <span class="spacer"></span>
            <sl-button
              size="small"
              data-action="cancel-change"
              @click=${() => {
                this.pendingChange = null;
              }}
              >Cancel</sl-button
            >
            <sl-button
              size="small"
              variant="primary"
              data-action="confirm-change"
              @click=${() => {
                const cid = target.id;
                this.pendingChange = null;
                void this.choose(d, cid);
              }}
              >Change answer</sl-button
            >
          </div>`;
        }
      }
      if (!origIsOther && this.otherOpen && d.other) {
        const trimmed = this.otherDraft.trim();
        const trimmedLen = [...trimmed].length;
        const canSubmit = trimmedLen > 0 && trimmedLen <= 200 && !this.submitting;
        return html`${this.renderTerminalFoot(d)}
          <div class="confirmbar" role="region" aria-label="Confirm answer change">
            <sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>
            <span role="status" aria-live="polite"
              >Change your answer from ${origDisplay} to <b>${d.other.label}</b>${trimmed
                ? html` “${trimmed}”`
                : nothing}?
              ${trimmed
                ? `${d.askerAgentName || 'The agent'} will be told.`
                : 'Type your answer above.'}</span
            >
            <span class="spacer"></span>
            <sl-button
              size="small"
              data-action="cancel-change"
              @click=${() => void this.cancelOther(d)}
              >Cancel</sl-button
            >
            <sl-button
              size="small"
              variant="primary"
              data-action="confirm-change"
              ?disabled=${!canSubmit}
              aria-disabled=${!canSubmit ? 'true' : nothing}
              @click=${() => void this.submitOther(d)}
              >Change answer</sl-button
            >
          </div>`;
      }
      if (origIsOther) {
        const origText = d.decision.otherText ?? '';
        const trimmed = this.otherDraft.trim();
        const trimmedLen = [...trimmed].length;
        const otherLabel = d.other?.label || d.decision.label;
        if (trimmed !== origText) {
          const canSubmit = trimmedLen > 0 && trimmedLen <= 200 && !this.submitting;
          return html`${this.renderTerminalFoot(d)}
            <div class="confirmbar" role="region" aria-label="Confirm answer change">
              <sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>
              <span role="status" aria-live="polite"
                >Change your answer from ${origDisplay} to <b>${otherLabel}</b>${trimmed
                  ? html` “${trimmed}”`
                  : nothing}?
                ${trimmed
                  ? `${d.askerAgentName || 'The agent'} will be told.`
                  : 'Type your answer above.'}</span
              >
              <span class="spacer"></span>
              <sl-button
                size="small"
                data-action="cancel-change"
                @click=${() => {
                  this.otherDraft = origText;
                }}
                >Cancel</sl-button
              >
              <sl-button
                size="small"
                variant="primary"
                data-action="confirm-change"
                ?disabled=${!canSubmit}
                aria-disabled=${!canSubmit ? 'true' : nothing}
                @click=${() => void this.submitOther(d)}
                >Change answer</sl-button
              >
            </div>`;
        }
        return html`${this.renderTerminalFoot(d)}
          <div class="confirm-row">
            <sl-button
              size="small"
              variant="success"
              outline
              data-action="confirm"
              @click=${() => void this.choose(d, OTHER_CHOICE_ID, origText)}
              ><sl-icon slot="prefix" name="patch-check"></sl-icon>Confirm
              “${d.decision.label}”</sl-button
            >
            <span>or edit your answer or pick another choice to change it</span>
          </div>`;
      }
      return html`${this.renderTerminalFoot(d)}
        <div class="confirm-row">
          <sl-button
            size="small"
            variant="success"
            outline
            data-action="confirm"
            @click=${() => void this.choose(d, d.decision!.choiceId)}
            ><sl-icon slot="prefix" name="patch-check"></sl-icon>Confirm
            “${d.decision.label}”</sl-button
          >
          ${this.isSingleChoiceOnly(d)
            ? nothing
            : html`<span>or pick another answer to change it</span>`}
        </div>`;
    }
    const terminal = this.renderTerminalFoot(d);
    if (terminal) return terminal;
    if (this.answerBlocked) return this.renderAnswerBlocked();
    return html`<div class="row-foot">
      <sl-icon name="hourglass-split"></sl-icon>Awaiting your answer
    </div>`;
  }

  /** The footer of a decided or replaced row (single and set), else null. */
  private renderTerminalFoot(d: DecisionView, inSet = false): TemplateResult | null {
    if (d.status === 'decided' && d.decision) {
      if (d.decision.provenance === 'agent_interpreted') {
        if (
          inSet &&
          this.rowActionable(d) &&
          !this.answerBlocked &&
          this.selectionOf(d) !== d.decision.choiceId
        ) {
          return html`<div class="row-foot interpreted" role="status">
            <sl-icon name="robot"></sl-icon>
            <span
              >Interpreted as <i>${d.decision.label}</i> by <b>${this.interpreterName(d)}</b> from
              your reply</span
            >
            ${this.renderViewReply(d.decision.sourceMessageId)}
          </div>`;
        }
        const who = this.answeredBy(d);
        return html`<div class="row-foot interpreted" role="status">
          <sl-icon name="robot"></sl-icon>
          <span
            >Interpreted by <b>${this.interpreterName(d)}</b> from
            ${who === 'you' ? html`your` : html`<b>${who}</b>'s`} reply</span
          >
          ${this.renderAt(d.decision.at)} ${this.renderViewReply(d.decision.sourceMessageId)}
        </div>`;
      }
      if (d.decision.provenance === 'direct' && d.decision.outcome === 'confirm') {
        const who = this.answeredBy(d);
        return html`<div class="row-foot" role="status">
          <sl-icon class="ok" name="check-circle-fill"></sl-icon>
          <span>Answered by <b>${who}</b> (confirmed)</span>
          ${this.renderAt(d.decision.at)}
          <span aria-hidden="true">·</span>
          <span class="muted"
            >first interpreted by ${this.interpreterName(d)} from
            ${who === 'you' ? 'your' : 'their'} reply</span
          >
        </div>`;
      }
      if (d.decision.provenance === 'direct' && d.decision.outcome === 'override') {
        const prev = d.decision.previousChoiceLabel;
        const prevOther = d.decision.previousOtherText;
        return html`<div class="row-foot" role="status">
          <sl-icon class="ok" name="check-circle-fill"></sl-icon>
          <span>Answered by <b>${this.answeredBy(d)}</b></span>
          ${this.renderAt(d.decision.at)}
          <span aria-hidden="true">·</span>
          <span
            >changed from the interpreted
            answer${prev
              ? html` <i>${prev}</i>${prevOther ? html` “${prevOther}”` : nothing}`
              : nothing}</span
          >
        </div>`;
      }
      return html`<div class="row-foot" role="status">
        <sl-icon class="ok" name="check-circle-fill"></sl-icon>
        <span>Answered by <b>${this.answeredBy(d)}</b></span>
        ${this.renderAt(d.decision.at)}
      </div>`;
    }
    if (d.status === 'replaced') {
      return this.actionState.renderReplacedBanner(d.replacedBy);
    }
    if (d.status === 'dismissed') {
      return this.renderDismissedFoot(d);
    }
    if (d.status === 'withdrawn') {
      return this.renderWithdrawnFoot(d);
    }
    if (isClosedUnanswered(d)) {
      return this.actionState.renderClosedBanner();
    }
    return null;
  }

  private renderWithdrawnFoot(d: DecisionView, inCardFoot = false): TemplateResult {
    return this.actionState.renderWithdrawnBanner(
      {
        by: this.withdrawnBy(d),
        at: d.withdrawn?.at,
        reason: d.withdrawn?.reason,
        replacedBy: d.replacedBy,
      },
      { inCardFoot }
    );
  }

  private renderDismissedFoot(d: DecisionView, inCardFoot = false): TemplateResult {
    return this.actionState.renderDismissedBanner(
      {
        by: this.dismissedBy(d),
        at: d.dismissed?.at,
        reason: d.dismissed?.reason,
        replacedBy: d.replacedBy,
      },
      { inCardFoot }
    );
  }

  private renderViewReply(sourceMessageId: string | undefined): TemplateResult | typeof nothing {
    if (!sourceMessageId) return nothing;
    return html`<span aria-hidden="true">·</span>
      <a
        class="jump"
        data-action="view-reply"
        href="#msg-${sourceMessageId}"
        @click=${(e: Event) => this.viewReply(e, sourceMessageId)}
        >View reply</a
      >`;
  }

  private viewReply(e: Event, messageId: string): void {
    e.preventDefault();
    this.dispatchEvent(
      new CustomEvent<{ messageId: string }>('scroll-to-message', {
        detail: { messageId },
        bubbles: true,
        composed: true,
      })
    );
  }

  /** " · time" for an instant, or nothing. */
  private renderAt(at: string | undefined): TemplateResult | typeof nothing {
    return this.actionState.renderAt(at);
  }

  /** The agent that interpreted an answer: the asker unless the view names another. */
  private interpreterName(d: DecisionView): string {
    const id = d.decision?.interpretedByAgentId;
    if (!id || id === d.askerAgentId) return d.askerAgentName || 'the agent';
    return id;
  }

  // ---- Set mode (design §4.3) ----

  /**
   * The row's selection: the viewer's click, else their own interpreted
   * choice (which beats `defaultChoiceId`, Mock P2-4), else, with the
   * viewer's preselect setting on, the recommended choice on a pending row.
   */
  private selectionOf(d: DecisionView): string | undefined {
    const picked = this.picked.get(d.rowId);
    if (picked === OTHER_CHOICE_ID && d.other) return OTHER_CHOICE_ID;
    if (picked && d.choices.some((c) => c.id === picked)) return picked;
    if (
      d.status === 'decided' &&
      d.decision?.provenance === 'agent_interpreted' &&
      this.rowActionable(d)
    ) {
      if (isOtherAnswer(d.decision) && d.other) return OTHER_CHOICE_ID;
      if (d.choices.some((c) => c.id === d.decision!.choiceId)) {
        return d.decision.choiceId;
      }
    }
    if (
      preselectRecommendedChoices() &&
      d.status === 'pending' &&
      d.defaultChoiceId &&
      d.defaultChoiceId !== OTHER_CHOICE_ID &&
      d.choices.some((c) => c.id === d.defaultChoiceId)
    ) {
      return d.defaultChoiceId;
    }
    return undefined;
  }

  private setRowOtherDraft(d: DecisionView): string {
    const draft = this.setOtherDrafts.get(d.rowId);
    if (draft !== undefined) return draft;
    if (
      d.status === 'decided' &&
      d.decision?.provenance === 'agent_interpreted' &&
      isOtherAnswer(d.decision) &&
      d.decision.otherText
    ) {
      return d.decision.otherText;
    }
    return '';
  }

  private setRowReady(d: DecisionView): boolean {
    const sel = this.selectionOf(d);
    if (!sel) return false;
    if (sel === OTHER_CHOICE_ID) {
      const len = [...this.setRowOtherDraft(d).trim()].length;
      return len > 0 && len <= 200;
    }
    return true;
  }

  private select(d: DecisionView, choiceId: string): void {
    if (!this.rowActionable(d) || this.setSubmitting || this.answerBlocked) return;
    if (choiceId === OTHER_CHOICE_ID && !d.other) return;
    // Compare with the click, not the effective selection: a click on a
    // preselected recommended choice is recorded, so it survives the
    // preselect setting being turned off while the card is open.
    if (this.picked.get(d.rowId) === choiceId) return;
    const next = new Map(this.picked);
    next.set(d.rowId, choiceId);
    this.picked = next;
    if (choiceId === OTHER_CHOICE_ID) {
      void this.focusSetOtherInput(d.rowId);
    }
  }

  private async focusSetOtherInput(rowId: string): Promise<void> {
    await this.updateComplete;
    const rows = Array.from(this.shadowRoot?.querySelectorAll<HTMLElement>('.row') ?? []);
    const row = rows.find((el) => el.dataset.row === rowId);
    const input = row?.querySelector<HTMLElement & { updateComplete?: Promise<unknown> }>(
      '.editor sl-input'
    );
    if (input?.updateComplete) {
      await input.updateComplete;
    }
    focusElement(input);
  }

  /** Sends every actionable row's selection as one set. */
  private async submitSet(): Promise<void> {
    if (this.setSubmitting || this.answerBlocked) return;
    const actionable = this.views.filter((d) => this.rowActionable(d));
    const answers: SetAnswer[] = [];
    for (const d of actionable) {
      const choiceId = this.selectionOf(d);
      if (!choiceId) return;
      if (choiceId === OTHER_CHOICE_ID) {
        const trimmed = this.setRowOtherDraft(d).trim();
        if (!trimmed || [...trimmed].length > 200) return;
        answers.push({ rowId: d.rowId, choiceId, otherText: trimmed });
      } else {
        answers.push({ rowId: d.rowId, choiceId });
      }
    }
    if (answers.length === 0) return;
    this.changedAlert = null;
    this.setSubmitting = true;
    try {
      const res = await answerDecisionSet(
        this.messageId,
        answers,
        crypto.randomUUID(),
        this.surface
      );
      if (res.ok) {
        this.picked = new Map();
        this.setOtherDrafts = new Map();
        this.applyOwnViews(res.decisions, res.responseMessageId);
        return;
      }
      if ((res.code === 'answer_set_changed' || res.status === 409) && res.decisions) {
        this.applySetChanged(res.decisions, res.reasons ?? {});
        return;
      }
      if (res.status === 403 && actionable[0]) {
        // Same authoritative handling as a single click; local selections
        // stay, nothing was recorded.
        this.applyDenied(actionable[0], res.code, res.reason);
        return;
      }
      showToast(res.message, 'danger');
    } finally {
      this.setSubmitting = false;
    }
  }

  /**
   * 409 answer_set_changed: hold the fresh views, keep selections on rows
   * still actionable, keep Other drafts on rows still open, and name the
   * changed rows inline.
   */
  private applySetChanged(fresh: DecisionView[], reasons: Record<string, string>): void {
    const byRow = new Map(fresh.map((d) => [d.rowId, d]));
    const kept = new Map<string, string>();
    for (const [rowId, choiceId] of this.picked) {
      const row = byRow.get(rowId);
      if (row && this.rowActionable(row)) kept.set(rowId, choiceId);
    }
    this.picked = kept;
    const keptDrafts = new Map<string, string>();
    for (const [rowId, draft] of this.setOtherDrafts) {
      const row = byRow.get(rowId);
      if (row && isOpenDecision(row) && this.rowActionable(row) && row.other) {
        keptDrafts.set(rowId, draft);
      }
    }
    this.setOtherDrafts = keptDrafts;
    const changed: { key: string; phrase: string }[] = [];
    for (const d of fresh) {
      const reason = reasons[d.rowId];
      if (reason) changed.push({ key: d.key || d.ref, phrase: this.changePhrase(reason, d) });
    }
    this.changedAlert = changed;
    this.applyOwnViews(fresh);
  }

  /** The 409 reason of a row as a phrase after its key. */
  private changePhrase(reason: string, d: DecisionView): string {
    switch (reason) {
      case 'replaced':
        return 'was replaced';
      case 'withdrawn':
        return 'is no longer needed';
      case 'dismissed':
        return 'was dismissed';
      case 'decided':
      case 'not_recorded_answerer':
        return d.decision
          ? `was answered by ${this.answeredBy(d)}`
          : 'was answered by someone else';
      case 'missing':
        return 'still needs an answer';
      default:
        return 'is no longer needed';
    }
  }

  /** Holds views from this card's own set POST and hands them to the thread. */
  private applyOwnViews(views: DecisionView[], responseMessageId?: string): void {
    const next = new Map(this.local);
    for (const v of views) next.set(v.rowId, v);
    this.local = next;
    const detail: DecisionAnsweredDetail = {
      messageId: this.messageId,
      decisions: views,
      ...(responseMessageId ? { responseMessageId } : {}),
    };
    this.dispatchEvent(
      new CustomEvent<DecisionAnsweredDetail>('decision-answered', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderSet(): TemplateResult | typeof nothing {
    const views = this.views;
    if (views.length === 0) return nothing;
    const asker = views[0]?.askerAgentName;
    const n = views.length;
    const label = `${n} decisions${asker ? ` from ${asker}` : ''}`;
    const blocked = this.answerBlocked && views.some((d) => this.rowActionable(d));
    const wholeWithdrawn = this.isWholeSetWithdrawn(views);
    const wholeDismissed = this.isWholeSetDismissed(views);
    return html`
      <div
        class="card set${wholeWithdrawn ? ' withdrawn-all' : ''}${wholeDismissed
          ? ' dismissed-all'
          : ''}"
        role="group"
        aria-label=${label}
        aria-disabled=${blocked ? 'true' : nothing}
        aria-describedby=${blocked ? this.reasonId : nothing}
        @contextmenu=${this.onCardContextMenu}
      >
        <div class="card-head">
          <span class="card-head-lead">
            <sl-icon name="list-check"></sl-icon>
            <span class="title">${n} decisions</span>
            ${wholeDismissed ? html`<span class="tag-wd">Dismissed</span>` : nothing}
          </span>
          ${this.renderCardMenu(views)}
        </div>
        ${this.changedAlert?.length
          ? html`<sl-alert variant="warning" open class="changed">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              This changed while you were choosing:
              ${this.changedAlert.map(
                (c, i) => html`${i > 0 ? ', ' : ''}<b>${c.key}</b> ${c.phrase}`
              )}.
              ${views.some((d) => this.rowActionable(d))
                ? 'Your other selections are kept — review and submit again.'
                : 'Your selections were not sent.'}
            </sl-alert>`
          : nothing}
        ${views.map((d) => this.renderSetRow(d, wholeWithdrawn || wholeDismissed))}
        ${blocked ? this.renderAnswerBlocked() : nothing} ${this.renderAskerGoneNote()}
        ${this.renderSetFoot(views)}
      </div>
      ${this.renderDismissDialog(views)}
    `;
  }

  private renderSetRow(d: DecisionView, omitFoot = false): TemplateResult {
    const chosenOther = d.status === 'decided' && isOtherAnswer(d.decision);
    const otherOpt: OtherOption | undefined =
      d.other ?? (chosenOther ? { label: d.decision?.label || 'Other' } : undefined);
    const stacked = (d.choices ?? []).some((c) => !!c.description) || !!otherOpt?.description;
    const otherSelected =
      this.rowActionable(d) && !this.answerBlocked && this.selectionOf(d) === OTHER_CHOICE_ID;
    const notOwner = actionableReason(d, this.currentUserId) === 'not_recorded_answerer';
    return html`
      <div
        class="row${this.rowStatusClass(d)}${notOwner ? ' readonly' : ''}"
        role="group"
        aria-label=${plainText(d.question)}
        data-row=${d.rowId}
        data-status=${d.status}
      >
        <div class="row-top">
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
          <div class="q">${this.markdown(d.question, false)}</div>
          ${this.renderSetRowChip(d)}
        </div>
        <div class="choices${stacked ? ' stack' : ''}">
          ${(d.choices ?? []).map((c) =>
            stacked ? this.renderSetOptRow(d, c) : this.renderSetButton(d, c)
          )}
          ${otherOpt
            ? stacked
              ? this.renderSetOtherOptRow(d, otherOpt, otherSelected, chosenOther)
              : this.renderSetOtherButton(d, otherOpt, otherSelected, chosenOther)
            : nothing}
        </div>
        ${!stacked && otherSelected && otherOpt
          ? this.renderSetOtherEditor(d, otherOpt, false)
          : nothing}
        ${chosenOther && !otherSelected && d.decision?.otherText
          ? html`<div class="otext">“${d.decision.otherText}”</div>`
          : nothing}
        ${omitFoot ? nothing : (this.renderTerminalFoot(d, true) ?? nothing)}
      </div>
    `;
  }

  private renderSetOtherButton(
    d: DecisionView,
    other: OtherOption,
    otherSelected: boolean,
    chosenOther: boolean
  ): TemplateResult {
    const actionable = this.rowActionable(d);
    const notOwner = actionableReason(d, this.currentUserId) === 'not_recorded_answerer';
    if (chosenOther && !actionable) {
      const label = d.decision?.label || other.label;
      const isInterp = d.decision?.provenance === 'agent_interpreted';
      const icon = isInterp ? 'robot' : 'check-circle-fill';
      const btn = html`<button
        type="button"
        class="sb sb-filled has-prefix"
        data-choice=${OTHER_CHOICE_ID}
        data-look="filled"
        ?disabled=${notOwner}
        ?data-readonly=${!notOwner}
        aria-disabled=${notOwner ? 'true' : nothing}
        title=${notOwner ? this.notOwnerTooltip(d) : label}
      >
        <span class="sb-prefix"><sl-icon name=${icon} aria-hidden="true"></sl-icon></span
        ><span class="sb-label">${label}</span>
      </button>`;
      return notOwner
        ? html`<sl-tooltip content=${this.notOwnerTooltip(d)}>${btn}</sl-tooltip>`
        : btn;
    }
    const disabled = !actionable || this.setSubmitting || this.answerBlocked;
    const look = otherSelected ? 'filled' : 'default';
    return html`<button
      type="button"
      class="sb sb-${look}${otherSelected ? '' : ' sb-other'} has-prefix"
      data-choice=${OTHER_CHOICE_ID}
      data-look=${look}
      ?disabled=${disabled}
      ?data-readonly=${!actionable && !disabled}
      aria-disabled=${disabled ? 'true' : nothing}
      aria-pressed=${actionable ? String(otherSelected) : nothing}
      title=${other.label}
      @click=${() => this.select(d, OTHER_CHOICE_ID)}
    >
      <span class="sb-prefix"><sl-icon name="pencil" aria-hidden="true"></sl-icon></span
      ><span class="sb-label">${other.label}…</span>
    </button>`;
  }

  private renderSetOtherOptRow(
    d: DecisionView,
    other: OtherOption,
    otherSelected: boolean,
    chosenOther: boolean
  ): TemplateResult {
    const actionable = this.rowActionable(d);
    const notOwner = actionableReason(d, this.currentUserId) === 'not_recorded_answerer';
    if (otherSelected) {
      return html`<div class="opt sel" data-choice=${OTHER_CHOICE_ID}>
        <div class="opt-head">
          <span class="mark"><sl-icon name="pencil"></sl-icon></span>
          <span>
            <span class="lbl">${other.label}…</span>
            ${other.description
              ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
              : nothing}
          </span>
        </div>
        ${this.renderSetOtherEditor(d, other, true)}
      </div>`;
    }
    if (chosenOther && !actionable) {
      const label = d.decision?.label || other.label;
      const isInterp = d.decision?.provenance === 'agent_interpreted';
      const icon = isInterp ? 'robot' : 'check-circle-fill';
      const btn = html`<button
        type="button"
        class="opt chosen"
        data-choice=${OTHER_CHOICE_ID}
        ?disabled=${notOwner}
        ?data-readonly=${!notOwner}
        aria-disabled=${notOwner ? 'true' : 'false'}
        title=${notOwner ? this.notOwnerTooltip(d) : label}
      >
        <span class="mark"><sl-icon name=${icon}></sl-icon></span>
        <span>
          <span class="lbl">${label}</span>
          ${other.description
            ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
            : nothing}
        </span>
      </button>`;
      return notOwner
        ? html`<sl-tooltip content=${this.notOwnerTooltip(d)}>${btn}</sl-tooltip>`
        : btn;
    }
    const disabled = !actionable || this.setSubmitting || this.answerBlocked;
    return html`<button
      type="button"
      class="opt other-opt"
      data-choice=${OTHER_CHOICE_ID}
      ?disabled=${disabled && notOwner}
      aria-pressed=${actionable ? 'false' : nothing}
      aria-disabled=${disabled ? 'true' : 'false'}
      title=${other.label}
      @click=${() => {
        if (!disabled) this.select(d, OTHER_CHOICE_ID);
      }}
    >
      <span class="mark"><sl-icon name="pencil"></sl-icon></span>
      <span>
        <span class="lbl">${other.label}…</span>
        ${other.description
          ? html`<span class="desc">${this.markdown(other.description, true)}</span>`
          : nothing}
      </span>
    </button>`;
  }

  private renderSetOtherEditor(
    d: DecisionView,
    other: OtherOption,
    inset: boolean
  ): TemplateResult {
    const draft = this.setRowOtherDraft(d);
    const trimmed = draft.trim();
    const trimmedLen = [...trimmed].length;
    const showCounter = trimmedLen >= 160;
    const warnCounter = trimmedLen >= 180 && trimmedLen <= 200;
    const overCounter = trimmedLen > 200;
    const announceCounter = warnCounter || overCounter;
    const showHelp = trimmedLen === 0;
    const counter = showCounter
      ? html`<span
          class="cnt${overCounter ? ' over' : warnCounter ? ' warn' : ''}"
          role=${announceCounter ? 'status' : nothing}
          aria-live=${announceCounter ? 'polite' : nothing}
          >${trimmedLen} / 200</span
        >`
      : nothing;
    return html`<div class="editor${inset ? ' inset' : ''}">
      <div class="ctl">
        <sl-input
          size="small"
          label=${plainText(d.question)}
          placeholder=${other.placeholder || 'Type your answer'}
          .value=${draft}
          ?disabled=${this.setSubmitting}
          @sl-input=${(e: Event) => {
            const val = (e.target as HTMLInputElement).value ?? '';
            const next = new Map(this.setOtherDrafts);
            next.set(d.rowId, val);
            this.setOtherDrafts = next;
          }}
        ></sl-input>
      </div>
      ${showHelp || showCounter
        ? html`<div class="editor-foot">
            ${showHelp
              ? html`<span class="set-other-help"
                  >Type your answer to include it in the submit.</span
                >`
              : nothing}
            ${counter}
          </div>`
        : nothing}
    </div>`;
  }

  private renderSetRowChip(d: DecisionView): TemplateResult | typeof nothing {
    if (d.status !== 'decided' || d.decision?.provenance !== 'agent_interpreted') return nothing;
    if (this.rowActionable(d)) {
      if (this.answerBlocked) return nothing;
      return this.selectionOf(d) === d.decision.choiceId
        ? html`<span class="chip confirm">will confirm</span>`
        : html`<span class="chip change">will change</span>`;
    }
    return html`<span class="chip interp">${this.ownerLabel(d)}'s answer</span>`;
  }

  /**
   * A set choice's look (design §4.2 + Phase 2 Mocks P2-4/P2-5):
   * - pending row: selected choice filled (`check-lg`), recommended choice
   *   outlined while not selected;
   * - viewer's own interpreted row: confirming (`selected === choiceId`) is
   *   filled (`robot`), changing is filled (`check-lg`) on the new choice and
   *   subtle-outlined (`robot`) on the original interpreted choice;
   * - another user's interpreted row: disabled, interpreted choice filled
   *   (`robot`) with tooltip;
   * - direct decided / replaced row: chosen answer filled (`check-circle-fill`).
   */
  private setChoiceAppearance(
    d: DecisionView,
    c: ChoiceOption
  ): {
    look: 'filled' | 'outline' | 'subtle' | 'default';
    icon: string;
    selected: boolean;
    recommended: boolean;
    disabled: boolean;
    nativeDisabled: boolean;
    actionable: boolean;
    title: string;
    notOwnerTip?: string;
  } {
    const reason = actionableReason(d, this.currentUserId);
    const actionable = reason === '';
    const notOwner = reason === 'not_recorded_answerer';
    const pending = d.status === 'pending';
    const recommended = pending && d.defaultChoiceId === c.id;
    const title = notOwner
      ? this.notOwnerTooltip(d)
      : recommended
        ? `${c.label} (recommended)`
        : c.label;

    if (d.status === 'decided' && d.decision?.provenance === 'agent_interpreted') {
      const isInterpreted = d.decision.choiceId === c.id;
      if (!actionable) {
        return {
          look: isInterpreted ? 'filled' : 'default',
          icon: isInterpreted ? 'robot' : '',
          selected: false,
          recommended: false,
          disabled: true,
          nativeDisabled: notOwner,
          actionable: false,
          title,
          ...(isInterpreted && notOwner ? { notOwnerTip: title } : {}),
        };
      }
      const selected = !this.answerBlocked && this.selectionOf(d) === c.id;
      const disabled = this.setSubmitting || this.answerBlocked;
      if (selected && isInterpreted) {
        return {
          look: 'filled',
          icon: 'robot',
          selected: true,
          recommended: false,
          disabled,
          nativeDisabled: false,
          actionable: true,
          title,
        };
      }
      if (selected) {
        return {
          look: 'filled',
          icon: 'check-lg',
          selected: true,
          recommended: false,
          disabled,
          nativeDisabled: false,
          actionable: true,
          title,
        };
      }
      if (isInterpreted) {
        return {
          look: 'subtle',
          icon: 'robot',
          selected: false,
          recommended: false,
          disabled,
          nativeDisabled: false,
          actionable: true,
          title,
        };
      }
      return {
        look: 'default',
        icon: '',
        selected: false,
        recommended: false,
        disabled,
        nativeDisabled: false,
        actionable: true,
        title,
      };
    }

    const chosen = d.status === 'decided' && d.decision?.choiceId === c.id;
    // A blocked card shows nothing selected (mock s10): the recommended
    // choice stays outlined. Picks are kept for an in-session unblock.
    const selected = pending && !this.answerBlocked && this.selectionOf(d) === c.id;
    const disabled = !chosen && (!pending || this.setSubmitting || this.answerBlocked);
    const filled = chosen || selected;
    const outlined = !filled && recommended;
    return {
      look: filled ? 'filled' : outlined ? 'outline' : 'default',
      icon: chosen ? 'check-circle-fill' : selected ? 'check-lg' : '',
      selected,
      recommended,
      disabled,
      nativeDisabled: disabled && notOwner,
      actionable,
      title,
    };
  }

  /**
   * A plain set choice: a native toggle button, so its `aria-pressed`
   * reaches assistive technology (Shoelace's `sl-button` does not forward
   * it from the host to its inner button). Styled as a small `sl-button`
   * (`.sb` rules): filled when selected or chosen, outlined when
   * recommended and not selected.
   */
  private renderSetButton(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.setChoiceAppearance(d, c);
    const btn = html`<button
      type="button"
      class="sb sb-${s.look}${s.icon ? ' has-prefix' : ''}"
      data-choice=${c.id}
      data-look=${s.look}
      ?disabled=${s.disabled}
      ?data-readonly=${!s.actionable && !s.disabled}
      aria-disabled=${s.disabled ? 'true' : nothing}
      aria-pressed=${s.actionable ? String(s.selected) : nothing}
      title=${s.title}
      @click=${() => this.select(d, c.id)}
    >
      ${s.icon
        ? html`<span class="sb-prefix"><sl-icon name=${s.icon} aria-hidden="true"></sl-icon></span>`
        : nothing}<span class="sb-label"
        >${c.label}${s.recommended
          ? html`<span class="sr-only"> (recommended)</span>`
          : nothing}</span
      >
    </button>`;
    return s.notOwnerTip ? html`<sl-tooltip content=${s.notOwnerTip}>${btn}</sl-tooltip>` : btn;
  }

  private renderSetOptRow(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.setChoiceAppearance(d, c);
    const filled = s.look === 'filled';
    const outlined = s.look === 'outline' || s.look === 'subtle';
    const cls = ['opt', outlined ? 'recd' : '', filled ? 'chosen' : ''].filter(Boolean).join(' ');
    const btn = html`<button
      type="button"
      class=${cls}
      data-choice=${c.id}
      ?disabled=${s.nativeDisabled}
      ?data-readonly=${!s.actionable && !s.disabled}
      aria-pressed=${s.actionable ? String(s.selected) : nothing}
      aria-disabled=${s.disabled ? 'true' : 'false'}
      title=${s.title}
      @click=${() => {
        if (!s.disabled) this.select(d, c.id);
      }}
    >
      <span class="mark">${s.icon ? html`<sl-icon name=${s.icon}></sl-icon>` : nothing}</span>
      <span>
        <span class="lbl">${c.label}</span>${s.recommended
          ? html`<span class="sr-only"> (recommended)</span>`
          : nothing}
        ${c.description
          ? html`<span class="desc">${this.markdown(c.description, true)}</span>`
          : nothing}
      </span>
    </button>`;
    return s.notOwnerTip ? html`<sl-tooltip content=${s.notOwnerTip}>${btn}</sl-tooltip>` : btn;
  }

  /** "1 replaced", "2 answered by Bob": the non-actionable rows a set submit leaves out. */
  private excludedSummary(
    views: DecisionView[],
    opts?: { omitNotRecordedAnswerer?: boolean }
  ): string[] {
    const parts: string[] = [];
    const decided = views.filter(
      (d) =>
        d.status === 'decided' &&
        !this.rowActionable(d) &&
        (!opts?.omitNotRecordedAnswerer ||
          actionableReason(d, this.currentUserId) !== 'not_recorded_answerer')
    );
    if (decided.length > 0) {
      const names = new Set(decided.map((d) => this.answeredBy(d)));
      const [only] = names;
      parts.push(
        names.size === 1 && only
          ? `${decided.length} answered by ${only}`
          : `${decided.length} answered`
      );
    }
    const replaced = views.filter((d) => d.status === 'replaced').length;
    if (replaced > 0) parts.push(`${replaced} replaced`);
    const withdrawn = views.filter((d) => d.status === 'withdrawn').length;
    if (withdrawn > 0) parts.push(`${withdrawn} no longer needed`);
    const dismissed = views.filter((d) => d.status === 'dismissed').length;
    if (dismissed > 0) parts.push(`${dismissed} dismissed`);
    const otherClosed = views.filter(
      (d) =>
        d.status !== 'replaced' &&
        d.status !== 'withdrawn' &&
        d.status !== 'dismissed' &&
        isClosedUnanswered(d)
    ).length;
    if (otherClosed > 0) parts.push(`${otherClosed} closed`);
    return parts;
  }

  private renderSetCounterContent(
    views: DecisionView[],
    actionable: DecisionView[],
    remaining: number
  ): TemplateResult {
    if (remaining > 0) {
      const excluded = this.excludedSummary(views);
      return html`<b>${remaining} remaining</b>${excluded.map((p) => ` · ${p}`)}`;
    }
    const interpreted = actionable.filter((d) => d.decision?.provenance === 'agent_interpreted');
    if (interpreted.length === 0) {
      const excluded = this.excludedSummary(views);
      return html`<b>0 remaining</b>${excluded.map((p) => ` · ${p}`)}`;
    }
    let confirms = 0;
    let changes = 0;
    for (const d of interpreted) {
      if (this.selectionOf(d) === d.decision?.choiceId) confirms++;
      else changes++;
    }
    const suffixes: string[] = [];
    if (confirms > 0) suffixes.push(`${confirms} ${confirms === 1 ? 'confirm' : 'confirms'}`);
    if (changes > 0) suffixes.push(`${changes} ${changes === 1 ? 'change' : 'changes'}`);
    const excluded = this.excludedSummary(views, { omitNotRecordedAnswerer: true });
    const m = actionable.length;
    return html`<b>${m} of ${m}</b> ready${suffixes.map((s) => ` · ${s}`)}${excluded.map(
        (p) => ` · ${p}`
      )}`;
  }

  private setRowOverLimit(d: DecisionView): boolean {
    return (
      this.selectionOf(d) === OTHER_CHOICE_ID && [...this.setRowOtherDraft(d).trim()].length > 200
    );
  }

  private renderSetFoot(views: DecisionView[]): TemplateResult | typeof nothing {
    const actionable = views.filter((d) => this.rowActionable(d));
    if (actionable.length === 0) return this.renderSetDoneFoot(views);
    // Blocked (mock s10): the reason line is the footer; no counter or Submit.
    if (this.answerBlocked) return nothing;
    const missing = actionable.filter((d) => !this.setRowReady(d));
    const remaining = missing.length;
    const missingKeys = missing.map((d) => d.key || d.ref).join(', ');
    const onlyOverLimit = remaining > 0 && missing.every((d) => this.setRowOverLimit(d));
    const hasOtherMissing = missing.some((d) => !!d.other);
    const hint =
      remaining > 0
        ? onlyOverLimit
          ? `Shorten the answer for ${missingKeys}`
          : hasOtherMissing
            ? `Choose or type an answer for ${missingKeys} first`
            : `Choose an answer for ${missingKeys} first`
        : '';
    const disabled = remaining > 0;
    const submit = html`<sl-button
      class="submit"
      size="small"
      variant="primary"
      ?disabled=${disabled}
      aria-disabled=${disabled ? 'true' : nothing}
      ?loading=${this.setSubmitting}
      aria-describedby=${disabled ? this.submitHintId : nothing}
      @click=${() => void this.submitSet()}
      >Submit answers</sl-button
    >`;
    return html`<div class="card-foot">
      <span class="counter${remaining > 0 ? ' missing' : ''}" role="status"
        >${this.renderSetCounterContent(views, actionable, remaining)}</span
      >
      <span class="spacer"></span>
      ${disabled
        ? html`<span class="sr-only" id=${this.submitHintId}>${hint}</span>
            <sl-tooltip content=${hint}>${submit}</sl-tooltip>`
        : submit}
    </div>`;
  }

  /**
   * The footer once no row is actionable: "Submitted by X · time" when one
   * set submit answered every decided row, else what happened to the rows;
   * plus the concurrent-answer note when the viewer's choices were not sent
   * (not to a viewer who could not answer: there were no choices to send).
   */
  private renderSetDoneFoot(views: DecisionView[]): TemplateResult {
    const firstRow = views[0];
    if (firstRow && this.isWholeSetWithdrawn(views)) {
      return html`<div class="card-foot card-foot-wd">
        ${this.renderWithdrawnFoot(firstRow, true)}
      </div>`;
    }
    if (firstRow && this.isWholeSetDismissed(views)) {
      return html`<div class="card-foot card-foot-wd">
        ${this.renderDismissedFoot(firstRow, true)}
      </div>`;
    }
    const decided = views.filter((d) => d.status === 'decided' && d.decision);
    const batch = decided[0]?.decision?.batchId;
    const oneBatch = !!batch && decided.every((d) => d.decision?.batchId === batch);
    const first = decided[0];
    const others = [...new Set(this.answeredElsewhere.values())];
    return html`<div class="card-foot">
        <span class="counter" role="status"
          >${oneBatch && first
            ? html`<sl-icon class="ok" name="check-circle-fill"></sl-icon> Submitted by
                <b>${this.answeredBy(first)}</b> ${this.renderAt(first.decision?.at)}`
            : this.excludedSummary(views).join(' · ')}</span
        >
      </div>
      ${others.length > 0 && !this.answerBlocked
        ? html`<div class="note" role="status">
            ${joinNames(others)} answered while you were choosing; your selections were not sent.
          </div>`
        : nothing}`;
  }
}

/** The reason line of a blocked answerability, as plain text. */
function blockedText(a: AskerAnswerability): string {
  const reason = a.reason ?? '';
  return a.askerName ? `You can't answer ${a.askerName}: ${reason}` : reason;
}

/** "Bob", "Bob and Carol", "Bob, Carol and Dan". */
function joinNames(names: string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

/** Markdown source reduced to text for an accessible name. */
function plainText(md: string): string {
  return md
    .replace(/`([^`]*)`/g, '$1')
    .replace(/[*_~#>]/g, '')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\s+/g, ' ')
    .trim();
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-decisions': ScionChatDecisions;
  }
}
