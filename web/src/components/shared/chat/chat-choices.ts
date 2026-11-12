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
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import { formatInstant, formatInstantWithZone } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { showToast } from '../../../utils/toast.js';
import { answerDecision, newerDecisionView } from '../../../client/chat-choices.js';
import type { ChoiceAnsweredDetail } from '../../../client/chat-choices.js';
import type { ChoiceOption, DecisionView } from '../../../shared/types.js';

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

  private readonly hintId = `choice-hint-${++hintSeq}`;

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

  private actionable(d: DecisionView): boolean {
    return d.status === 'pending' && !this.submitting;
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
      showToast(res.message, 'danger');
    } finally {
      this.submitting = null;
    }
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

  override render(): TemplateResult | typeof nothing {
    // Single mode (design §4.1): the card holds one decision.
    const d = this.views[0];
    if (!d) return nothing;
    const pendingWithDefault = d.status === 'pending' && !!d.defaultChoiceId;
    return html`
      <div
        class="card"
        role="group"
        tabindex="0"
        aria-label=${plainText(d.question)}
        aria-describedby=${pendingWithDefault ? this.hintId : nothing}
        @keydown=${(e: KeyboardEvent) => this.onCardKeydown(e, d)}
      >
        <div class="card-head">
          <sl-icon name="signpost-split"></sl-icon>
          <span class="title">Decision</span>
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
        </div>
        ${this.renderRow(d, pendingWithDefault)}
      </div>
    `;
  }

  private renderRow(d: DecisionView, showHint: boolean): TemplateResult {
    const stacked = (d.choices ?? []).some((c) => !!c.description);
    return html`
      <div class="row" data-row=${d.rowId} data-status=${d.status}>
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

  private renderFoot(d: DecisionView): TemplateResult {
    if (this.submitting?.rowId === d.rowId) {
      return html`<div class="row-foot" role="status">Sending…</div>`;
    }
    if (d.status === 'decided' && d.decision) {
      const at = d.decision.at;
      return html`<div class="row-foot" role="status">
        <sl-icon class="ok" name="check-circle-fill"></sl-icon>
        <span>Answered by <b>${this.answeredBy(d)}</b></span>
        ${at
          ? html`<span aria-hidden="true">·</span>
              <time datetime=${at} title=${formatInstantWithZone(at)}
                >${formatInstant(at, 'time')}</time
              >`
          : nothing}
      </div>`;
    }
    if (d.status === 'replaced') {
      const v = d.replacedBy?.version;
      return html`<div class="row-foot">${v ? `Replaced by v${v}` : 'Replaced'}</div>`;
    }
    return html`<div class="row-foot">
      <sl-icon name="hourglass-split"></sl-icon>Awaiting your answer
    </div>`;
  }
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
