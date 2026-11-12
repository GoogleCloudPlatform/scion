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
 * `<scion-chat-choices>`: the card a choice-prompt message renders under
 * web.chat_choices (ptone/scion#7673, design §4.1–§4.3).
 *
 * Single mode: one click records the answer through
 * `POST /api/v1/decisions/{rowId}/answer`; Enter on the focused card picks
 * the recommended choice. The card applies the 200's view at once and emits
 * `choice-answered` so the thread holds the same view; other tabs get it
 * over `chat.choice.updated`.
 *
 * Set mode (`answerMode=set`): clicks only select, rows with a recommended
 * choice start selected, and one Submit answers sends every actionable row
 * through `POST /api/v1/choice-prompts/{messageId}/answers`. A 409
 * `answer_set_changed` takes the fresh views and names the changed rows
 * inline; rows answered elsewhere lock as their updates arrive.
 *
 * Replaced rows link to their new version (`choice-jump`); an
 * agent-interpreted answer renders read-only.
 *
 * Answerability (design §4.5): the thread passes `answerability` for the
 * prompt's asker. A blocked card disables every choice (`aria-disabled`),
 * and a set's Submit, and shows the reason inline; a 403 on a click or a
 * set submit blocks it the same way and emits `choice-answer-denied`. A
 * gone asker stays answerable with a note.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import { formatInstant, formatInstantWithZone } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { showToast } from '../../../utils/toast.js';
import {
  ANSWERABLE,
  answerDecision,
  answerDecisionSet,
  answerabilityFromDenial,
  newerDecisionView,
} from '../../../client/chat-choices.js';
import type {
  AskerAnswerability,
  ChoiceAnswerDeniedDetail,
  ChoiceAnsweredDetail,
  SetAnswer,
} from '../../../client/chat-choices.js';
import type { ChoiceAnswerMode, ChoiceOption, DecisionView } from '../../../shared/types.js';

let hintSeq = 0;

@customElement('scion-chat-choices')
export class ScionChatChoices extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  /** The choice-prompt message id. */
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
  answerMode: ChoiceAnswerMode | undefined = undefined;

  /** Whether the viewer can answer this prompt's asker (design §4.5). */
  @property({ attribute: false })
  answerability: AskerAnswerability = ANSWERABLE;

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

  /** Rendered question/description markdown, keyed by source text. */
  @state()
  private rendered = new Map<string, DocumentFragment>();

  /** Set mode: choices the viewer clicked, by row (local until submitted). */
  @state()
  private picked = new Map<string, string>();

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

  private readonly hintId = `choice-hint-${++hintSeq}`;
  private readonly submitHintId = `choice-submit-hint-${hintSeq}`;
  private readonly reasonId = `choice-reason-${hintSeq}`;

  static override styles = css`
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
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg, #f8fafc);
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
    }

    .card-head sl-icon {
      font-size: 1rem;
      color: var(--scion-primary, #3b82f6);
    }

    .title {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .key {
      font: 600 0.75rem/1 var(--scion-font-mono, monospace);
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      padding: 0.2rem 0.4rem;
      border-radius: 0.25rem;
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
      border-radius: 0.2rem;
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

    /*
     * Shoelace draws a default button's label in neutral-700, which the dark
     * theme maps to a slate barely lighter than the button itself, so an
     * enabled choice read as disabled. Use the theme's text colour.
     */
    .choices sl-button[variant='default'] {
      --sl-color-neutral-700: var(--scion-text, #1e293b);
    }

    .opt {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      text-align: left;
      width: 100%;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #fff);
      padding: 0.45rem 0.6rem;
      font: inherit;
      color: var(--scion-text, #1e293b);
      cursor: pointer;
    }

    .opt:hover:not([aria-disabled='true']) {
      border-color: var(--scion-border-hover, #cbd5e1);
      background: var(--scion-bg, #f8fafc);
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
      border-color: var(--scion-primary, #3b82f6);
      background: var(--scion-primary, #3b82f6);
      color: var(--scion-primary-text, #fff);
      opacity: 1;
    }

    .opt.chosen .lbl,
    .opt.chosen .desc {
      color: var(--scion-primary-text, #fff);
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
    }

    .row-top {
      display: flex;
      gap: 0.5rem;
      align-items: baseline;
    }

    .row-top .q {
      min-width: 0;
    }

    .row.replaced .q,
    .row.replaced .choices {
      opacity: 0.55;
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
      border-radius: 0.2rem;
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

    /*
     * The app theme leaves Shoelace's panel background (neutral-50) and
     * message text (neutral-700) at their light values in dark mode, which
     * drew a white alert inside a dark card. Use the card's own surface and
     * text colours.
     */
    .card sl-alert.changed {
      display: block;
      margin: 0.6rem 0.75rem 0;
      --sl-panel-background-color: var(--scion-surface, #fff);
      --sl-panel-border-color: var(--scion-border, #e2e8f0);
      --sl-color-neutral-700: var(--scion-text, #1e293b);
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
      color: var(--scion-primary-text, #fff);
    }

    .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.35rem;
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
      if (was?.status !== 'pending' || d.status !== 'decided' || !d.decision) continue;
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
    }
    if (sources.length === 0) return;
    try {
      const renderer = await getMarkdownRenderer();
      const next = new Map(this.rendered);
      for (const src of sources) next.set(src, renderer.renderFragment(src));
      this.rendered = next;
    } catch {
      // Plain text stays shown.
    }
  }

  /** Rendered markdown for `src`, as fresh nodes; inline unwraps a lone paragraph. */
  private markdown(src: string, inline: boolean): Node | string {
    const frag = this.rendered.get(src);
    if (!frag) return src;
    const copy = frag.cloneNode(true) as DocumentFragment;
    if (inline) {
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
    return d.status === 'pending' && !this.submitting && !this.answerBlocked;
  }

  /** Enter on the focused card (not on a control inside it) picks the recommended choice. */
  private onCardKeydown(e: KeyboardEvent, d: DecisionView): void {
    if (e.key !== 'Enter' || e.target !== e.currentTarget) return;
    if (!d.defaultChoiceId || !this.actionable(d)) return;
    e.preventDefault();
    void this.choose(d, d.defaultChoiceId);
  }

  private async choose(d: DecisionView, choiceId: string): Promise<void> {
    if (!this.actionable(d)) return;
    this.submitting = { rowId: d.rowId, choiceId };
    try {
      const res = await answerDecision(d.rowId, choiceId, crypto.randomUUID());
      if (res.ok) {
        this.applyOwn(res.decision, res.responseMessageId);
        return;
      }
      if (res.decision) {
        // 409: someone else answered first, or a newer version replaced it.
        this.applyOwn(res.decision);
        if (res.code === 'already_answered') {
          const who = this.answeredBy(res.decision);
          showToast(`Already answered by ${who}.`, 'warning');
        } else if (res.code === 'replaced') {
          const v = res.replacedBy?.version ?? res.decision.replacedBy?.version;
          showToast(v ? `Replaced by v${v}.` : 'This decision was replaced.', 'warning');
        } else {
          showToast(res.message, 'warning');
        }
        return;
      }
      if (res.status === 403) {
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
    const detail: ChoiceAnswerDeniedDetail = {
      messageId: this.messageId,
      askerAgentId: d.askerAgentId,
      code,
      ...(reason ? { reason } : {}),
      answerability,
    };
    this.dispatchEvent(
      new CustomEvent<ChoiceAnswerDeniedDetail>('choice-answer-denied', {
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
    const detail: ChoiceAnsweredDetail = {
      messageId: this.messageId,
      decisions: [view],
      ...(responseMessageId ? { responseMessageId } : {}),
    };
    this.dispatchEvent(
      new CustomEvent<ChoiceAnsweredDetail>('choice-answered', {
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
    const blocked = this.answerBlocked && d.status === 'pending';
    const pendingWithDefault = d.status === 'pending' && !!d.defaultChoiceId && !blocked;
    return html`
      <div
        class="card"
        role="group"
        tabindex="0"
        aria-label=${plainText(d.question)}
        aria-disabled=${blocked ? 'true' : nothing}
        aria-describedby=${pendingWithDefault ? this.hintId : blocked ? this.reasonId : nothing}
        @keydown=${(e: KeyboardEvent) => this.onCardKeydown(e, d)}
      >
        <div class="card-head">
          <sl-icon name="signpost-split"></sl-icon>
          <span class="title">Decision</span>
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
          ${d.version > 1 || d.status === 'replaced'
            ? html`<span class="version">v${d.version}</span>`
            : nothing}
        </div>
        ${this.renderRow(d, pendingWithDefault)} ${this.renderAskerGoneNote()}
      </div>
    `;
  }

  private renderRow(d: DecisionView, showHint: boolean): TemplateResult {
    const stacked = (d.choices ?? []).some((c) => !!c.description);
    return html`
      <div
        class="row${d.status === 'replaced' ? ' replaced' : ''}"
        data-row=${d.rowId}
        data-status=${d.status}
      >
        <div class="q">${this.markdown(d.question, false)}</div>
        <div class="choices${stacked ? ' stack' : ''}">
          ${(d.choices ?? []).map((c) =>
            stacked ? this.renderOptRow(d, c) : this.renderButton(d, c)
          )}
        </div>
        ${this.renderFoot(d)}
        ${showHint && !this.submitting
          ? html`<div class="hint" id=${this.hintId}>
              Press Enter to choose the recommended answer (outlined).
            </div>`
          : nothing}
      </div>
    `;
  }

  private choiceState(
    d: DecisionView,
    c: ChoiceOption
  ): { chosen: boolean; loading: boolean; recommended: boolean; disabled: boolean } {
    const chosen = d.status === 'decided' && d.decision?.choiceId === c.id;
    const loading = this.submitting?.rowId === d.rowId && this.submitting.choiceId === c.id;
    const recommended = d.defaultChoiceId === c.id;
    const disabled = !chosen && !loading && !this.actionable(d);
    return { chosen, loading, recommended, disabled };
  }

  private renderButton(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.choiceState(d, c);
    const outlined = s.recommended && d.status === 'pending';
    const variant = s.chosen || outlined ? 'primary' : 'default';
    return html`<sl-button
      size="small"
      data-choice=${c.id}
      variant=${variant}
      ?outline=${outlined && !s.chosen}
      ?loading=${s.loading}
      ?disabled=${s.disabled}
      aria-disabled=${s.disabled ? 'true' : nothing}
      title=${s.recommended ? `${c.label} (recommended)` : c.label}
      @click=${() => {
        if (!s.chosen) void this.choose(d, c.id);
      }}
      >${s.chosen
        ? html`<sl-icon slot="prefix" name="check-circle-fill"></sl-icon>`
        : nothing}${c.label}${s.recommended
        ? html`<span class="sr-only"> (recommended)</span>`
        : nothing}</sl-button
    >`;
  }

  private renderOptRow(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.choiceState(d, c);
    const cls = [
      'opt',
      s.recommended && d.status === 'pending' ? 'recd' : '',
      s.chosen ? 'chosen' : '',
    ]
      .filter(Boolean)
      .join(' ');
    return html`<button
      type="button"
      class=${cls}
      data-choice=${c.id}
      aria-disabled=${s.disabled || s.loading ? 'true' : 'false'}
      aria-busy=${s.loading ? 'true' : nothing}
      title=${s.recommended ? `${c.label} (recommended)` : c.label}
      @click=${() => {
        if (!s.chosen && !s.disabled) void this.choose(d, c.id);
      }}
    >
      <span class="mark"
        >${s.chosen
          ? html`<sl-icon name="check-circle-fill"></sl-icon>`
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
    if (!views.some((d) => d.status === 'pending')) return nothing;
    const name = views[0]?.askerAgentName || 'The agent';
    return html`<div class="note" role="note" data-note="asker-gone">
      <sl-icon name="info-circle" aria-hidden="true"></sl-icon>
      <span
        ><b>${name}</b> is no longer running. Your answer will be recorded but not delivered.</span
      >
    </div>`;
  }

  private renderFoot(d: DecisionView): TemplateResult | typeof nothing {
    if (this.submitting?.rowId === d.rowId) {
      return html`<div class="row-foot" role="status">Sending…</div>`;
    }
    const terminal = this.renderTerminalFoot(d);
    if (terminal) return terminal;
    if (this.answerBlocked) return this.renderAnswerBlocked();
    return html`<div class="row-foot">
      <sl-icon name="hourglass-split"></sl-icon>Awaiting your answer
    </div>`;
  }

  /** The footer of a decided or replaced row (single and set), else null. */
  private renderTerminalFoot(d: DecisionView): TemplateResult | null {
    if (d.status === 'decided' && d.decision) {
      if (d.decision.provenance === 'agent_interpreted') {
        const who = this.answeredBy(d);
        return html`<div class="row-foot interpreted" role="status">
          <sl-icon name="robot"></sl-icon>
          <span
            >Interpreted by <b>${this.interpreterName(d)}</b> from
            ${who === 'you' ? html`your` : html`<b>${who}</b>'s`} reply</span
          >
          ${this.renderAt(d.decision.at)}
        </div>`;
      }
      return html`<div class="row-foot" role="status">
        <sl-icon class="ok" name="check-circle-fill"></sl-icon>
        <span>Answered by <b>${this.answeredBy(d)}</b></span>
        ${this.renderAt(d.decision.at)}
      </div>`;
    }
    if (d.status === 'replaced') {
      const r = d.replacedBy;
      const label = r?.version ? `v${r.version}` : 'the new version';
      return html`<div class="row-foot">
        <sl-icon name="arrow-repeat"></sl-icon>
        <span>${r?.version ? `Replaced by v${r.version}` : 'Replaced'}</span>
        ${this.renderAt(r?.at)}
        ${r?.messageId
          ? html`<span aria-hidden="true">·</span>
              <a
                class="jump"
                href="#msg-${r.messageId}"
                @click=${(e: Event) => this.jump(e, r.messageId)}
                >Jump to ${label}</a
              >`
          : nothing}
      </div>`;
    }
    return null;
  }

  /** " · time" for an instant, or nothing. */
  private renderAt(at: string | undefined): TemplateResult | typeof nothing {
    if (!at) return nothing;
    return html`<span aria-hidden="true">·</span>
      <time datetime=${at} title=${formatInstantWithZone(at)}>${formatInstant(at, 'time')}</time>`;
  }

  /** The agent that interpreted an answer: the asker unless the view names another. */
  private interpreterName(d: DecisionView): string {
    const id = d.decision?.interpretedByAgentId;
    if (!id || id === d.askerAgentId) return d.askerAgentName || 'the agent';
    return id;
  }

  /** Asks the thread to scroll to a replacement version (design §4.1 `choice-jump`). */
  private jump(e: Event, messageId: string): void {
    e.preventDefault();
    this.dispatchEvent(
      new CustomEvent<{ messageId: string }>('choice-jump', {
        detail: { messageId },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ---- Set mode (design §4.3) ----

  /** The row's selection: the viewer's click, else the recommended choice. */
  private selectionOf(d: DecisionView): string | undefined {
    const picked = this.picked.get(d.rowId);
    if (picked && d.choices.some((c) => c.id === picked)) return picked;
    if (d.defaultChoiceId && d.choices.some((c) => c.id === d.defaultChoiceId)) {
      return d.defaultChoiceId;
    }
    return undefined;
  }

  private select(d: DecisionView, choiceId: string): void {
    if (d.status !== 'pending' || this.setSubmitting || this.answerBlocked) return;
    if (this.selectionOf(d) === choiceId) return;
    const next = new Map(this.picked);
    next.set(d.rowId, choiceId);
    this.picked = next;
  }

  /** Sends every actionable row's selection as one set. */
  private async submitSet(): Promise<void> {
    if (this.setSubmitting || this.answerBlocked) return;
    const actionable = this.views.filter((d) => d.status === 'pending');
    const answers: SetAnswer[] = [];
    for (const d of actionable) {
      const choiceId = this.selectionOf(d);
      if (!choiceId) return;
      answers.push({ rowId: d.rowId, choiceId });
    }
    if (answers.length === 0) return;
    this.changedAlert = null;
    this.setSubmitting = true;
    try {
      const res = await answerDecisionSet(this.messageId, answers, crypto.randomUUID());
      if (res.ok) {
        this.picked = new Map();
        this.applyOwnViews(res.decisions, res.responseMessageId);
        return;
      }
      if (res.code === 'answer_set_changed' && res.decisions) {
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
   * still actionable, and name the changed rows inline.
   */
  private applySetChanged(fresh: DecisionView[], reasons: Record<string, string>): void {
    const byRow = new Map(fresh.map((d) => [d.rowId, d]));
    const kept = new Map<string, string>();
    for (const [rowId, choiceId] of this.picked) {
      if (byRow.get(rowId)?.status === 'pending') kept.set(rowId, choiceId);
    }
    this.picked = kept;
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
    const detail: ChoiceAnsweredDetail = {
      messageId: this.messageId,
      decisions: views,
      ...(responseMessageId ? { responseMessageId } : {}),
    };
    this.dispatchEvent(
      new CustomEvent<ChoiceAnsweredDetail>('choice-answered', {
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
    const blocked = this.answerBlocked && views.some((d) => d.status === 'pending');
    return html`
      <div
        class="card set"
        role="group"
        aria-label=${label}
        aria-disabled=${blocked ? 'true' : nothing}
        aria-describedby=${blocked ? this.reasonId : nothing}
      >
        <div class="card-head">
          <sl-icon name="list-check"></sl-icon>
          <span class="title">${n} decisions</span>
          ${views.map((d) => (d.key ? html`<span class="key">${d.key}</span>` : nothing))}
        </div>
        ${this.changedAlert?.length
          ? html`<sl-alert variant="warning" open class="changed">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              This changed while you were choosing:
              ${this.changedAlert.map(
                (c, i) => html`${i > 0 ? ', ' : ''}<b>${c.key}</b> ${c.phrase}`
              )}.
              ${views.some((d) => d.status === 'pending')
                ? 'Your other selections are kept — review and submit again.'
                : 'Your selections were not sent.'}
            </sl-alert>`
          : nothing}
        ${views.map((d) => this.renderSetRow(d))} ${blocked ? this.renderAnswerBlocked() : nothing}
        ${this.renderAskerGoneNote()} ${this.renderSetFoot(views)}
      </div>
    `;
  }

  private renderSetRow(d: DecisionView): TemplateResult {
    const stacked = (d.choices ?? []).some((c) => !!c.description);
    return html`
      <div
        class="row${d.status === 'replaced' ? ' replaced' : ''}"
        role="group"
        aria-label=${plainText(d.question)}
        data-row=${d.rowId}
        data-status=${d.status}
      >
        <div class="row-top">
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
          <div class="q">${this.markdown(d.question, false)}</div>
        </div>
        <div class="choices${stacked ? ' stack' : ''}">
          ${(d.choices ?? []).map((c) =>
            stacked ? this.renderSetOptRow(d, c) : this.renderSetButton(d, c)
          )}
        </div>
        ${this.renderTerminalFoot(d) ?? nothing}
      </div>
    `;
  }

  /**
   * A set choice's look (design §4.2): a decided row's chosen answer is
   * filled with check-circle-fill; a selected choice filled with check-lg;
   * the recommended choice outlined while not selected.
   */
  private setChoiceState(
    d: DecisionView,
    c: ChoiceOption
  ): { chosen: boolean; selected: boolean; recommended: boolean; disabled: boolean } {
    const pending = d.status === 'pending';
    const chosen = d.status === 'decided' && d.decision?.choiceId === c.id;
    const selected = pending && this.selectionOf(d) === c.id;
    const recommended = d.defaultChoiceId === c.id;
    const disabled = !chosen && (!pending || this.setSubmitting || this.answerBlocked);
    return { chosen, selected, recommended, disabled };
  }

  private renderSetButton(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.setChoiceState(d, c);
    const filled = s.chosen || s.selected;
    const outlined = !filled && s.recommended && d.status === 'pending';
    const icon = s.chosen ? 'check-circle-fill' : s.selected ? 'check-lg' : '';
    return html`<sl-button
      size="small"
      data-choice=${c.id}
      variant=${filled || outlined ? 'primary' : 'default'}
      ?outline=${outlined}
      ?disabled=${s.disabled}
      aria-disabled=${s.disabled ? 'true' : nothing}
      aria-pressed=${d.status === 'pending' ? String(s.selected) : nothing}
      title=${s.recommended ? `${c.label} (recommended)` : c.label}
      @click=${() => this.select(d, c.id)}
      >${icon
        ? html`<sl-icon slot="prefix" name=${icon}></sl-icon>`
        : nothing}${c.label}${s.recommended
        ? html`<span class="sr-only"> (recommended)</span>`
        : nothing}</sl-button
    >`;
  }

  private renderSetOptRow(d: DecisionView, c: ChoiceOption): TemplateResult {
    const s = this.setChoiceState(d, c);
    const filled = s.chosen || s.selected;
    const cls = [
      'opt',
      !filled && s.recommended && d.status === 'pending' ? 'recd' : '',
      filled ? 'chosen' : '',
    ]
      .filter(Boolean)
      .join(' ');
    const icon = s.chosen ? 'check-circle-fill' : s.selected ? 'check-lg' : '';
    return html`<button
      type="button"
      class=${cls}
      data-choice=${c.id}
      aria-pressed=${d.status === 'pending' ? String(s.selected) : nothing}
      aria-disabled=${s.disabled ? 'true' : 'false'}
      title=${s.recommended ? `${c.label} (recommended)` : c.label}
      @click=${() => {
        if (!s.disabled) this.select(d, c.id);
      }}
    >
      <span class="mark">${icon ? html`<sl-icon name=${icon}></sl-icon>` : nothing}</span>
      <span>
        <span class="lbl">${c.label}</span>${s.recommended
          ? html`<span class="sr-only"> (recommended)</span>`
          : nothing}
        ${c.description
          ? html`<span class="desc">${this.markdown(c.description, true)}</span>`
          : nothing}
      </span>
    </button>`;
  }

  /** "1 replaced", "2 answered by Bob": the rows a set submit leaves out. */
  private excludedSummary(views: DecisionView[]): string[] {
    const parts: string[] = [];
    const decided = views.filter((d) => d.status === 'decided');
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
    return parts;
  }

  private renderSetFoot(views: DecisionView[]): TemplateResult {
    const actionable = views.filter((d) => d.status === 'pending');
    if (actionable.length === 0) return this.renderSetDoneFoot(views);
    const missing = actionable.filter((d) => !this.selectionOf(d));
    const remaining = missing.length;
    const excluded = this.excludedSummary(views);
    const missingKeys = missing.map((d) => d.key || d.ref).join(', ');
    const blocked = this.answerBlocked;
    const hint = blocked
      ? blockedText(this.effectiveAnswerability)
      : remaining > 0
        ? `Choose an answer for ${missingKeys} first`
        : '';
    const disabled = blocked || remaining > 0;
    const submit = html`<sl-button
      class="submit"
      size="small"
      variant="primary"
      ?disabled=${disabled}
      aria-disabled=${disabled ? 'true' : nothing}
      ?loading=${this.setSubmitting}
      aria-describedby=${blocked ? this.reasonId : remaining > 0 ? this.submitHintId : nothing}
      @click=${() => void this.submitSet()}
      >Submit answers</sl-button
    >`;
    return html`<div class="card-foot">
      <span class="counter${remaining > 0 ? ' missing' : ''}" role="status"
        ><b>${remaining} remaining</b>${excluded.map((p) => ` · ${p}`)}</span
      >
      <span class="spacer"></span>
      ${blocked
        ? html`<sl-tooltip content=${hint}>${submit}</sl-tooltip>`
        : remaining > 0
          ? html`<span class="sr-only" id=${this.submitHintId}>${hint}</span>
              <sl-tooltip content=${hint}>${submit}</sl-tooltip>`
          : submit}
    </div>`;
  }

  /**
   * The footer once no row is actionable: "Submitted by X · time" when one
   * set submit answered every decided row, else what happened to the rows;
   * plus the concurrent-answer note when the viewer's choices were not sent.
   */
  private renderSetDoneFoot(views: DecisionView[]): TemplateResult {
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
      ${others.length > 0
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
    'scion-chat-choices': ScionChatChoices;
  }
}
