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
 * `<scion-chat-choice-response>`: the compact line a choice-response
 * message renders under web.chat_choices instead of a bubble
 * (ptone/scion#7673, design §4.6).
 *
 * Single: "**alice** chose **yes** for **D234** *question* · time · Jump".
 * Set: "**alice** answered 3 decisions · time · Show answers ▸", expanding
 * to the answers in position order and "Jump to question". A response the
 * hub could not deliver (`dispatchState=failed`) says "not delivered
 * (agent gone)". Jump links emit `scroll-to-message` for the prompt.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { formatInstant, formatInstantWithZone } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import type { ChoiceResponseSetView, ChoiceResponseView } from '../../../shared/types.js';

/** The provenance of an answer an agent recorded from a user's reply. */
const INTERPRETED = 'agent_interpreted';

@customElement('scion-chat-choice-response')
export class ScionChatChoiceResponse extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  /** A one-click answer's response. */
  @property({ attribute: false })
  response: ChoiceResponseView | undefined = undefined;

  /** A set submit's response; wins over `response` when both are set. */
  @property({ attribute: false })
  responseSet: ChoiceResponseSetView | undefined = undefined;

  /** Who answered, as the line names them ("you" for the viewer). */
  @property()
  answererName = '';

  /** The agent that recorded an interpreted answer. */
  @property()
  agentName = '';

  /** The response message's time. */
  @property()
  timestamp = '';

  /** The response message's dispatch state; "failed" is not delivered. */
  @property()
  dispatchState = '';

  /** Whether the set's answers are shown. */
  @state()
  private open = false;

  static override styles = css`
    :host {
      display: block;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
      line-height: 1.5;
      max-width: 640px;
    }

    .line,
    summary {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.35rem;
    }

    :host([lane='own']) .line,
    :host([lane='own']) summary {
      justify-content: flex-end;
    }

    sl-icon.ok {
      color: var(--scion-status-success, #16a34a);
      flex-shrink: 0;
    }

    sl-icon.robot {
      color: var(--scion-text-muted, #64748b);
      flex-shrink: 0;
    }

    b {
      color: var(--scion-text, #1e293b);
      font-weight: 600;
    }

    .key {
      font: 600 0.75rem/1 var(--scion-font-mono, monospace);
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      padding: 0.2rem 0.4rem;
      border-radius: 0.25rem;
      white-space: nowrap;
    }

    .q {
      font-style: italic;
      overflow-wrap: anywhere;
    }

    time,
    .sep {
      color: var(--scion-text-muted, #64748b);
    }

    .failed {
      color: var(--scion-text-muted, #64748b);
    }

    .link {
      background: none;
      border: 0;
      padding: 0;
      font: inherit;
      font-weight: 500;
      color: var(--scion-primary, #3b82f6);
      cursor: pointer;
    }

    .link:hover {
      text-decoration: underline;
    }

    .link:focus-visible,
    summary:focus-visible {
      outline: 2px solid var(--scion-primary, #3b82f6);
      outline-offset: 2px;
      border-radius: 0.2rem;
    }

    details summary {
      cursor: pointer;
      list-style: none;
    }

    details summary::-webkit-details-marker {
      display: none;
    }

    .answers {
      list-style: none;
      margin: 0.35rem 0 0;
      padding: 0.4rem 0.6rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      background: var(--scion-surface, #fff);
    }

    .answers li {
      display: grid;
      grid-template-columns: 5.5rem 1fr;
      gap: 0.5rem;
      padding: 0.15rem 0;
      align-items: baseline;
    }

    .answers .key {
      justify-self: start;
      overflow: hidden;
      text-overflow: ellipsis;
      max-width: 100%;
    }

    .answers .lbl {
      color: var(--scion-text, #1e293b);
    }
  `;

  private get who(): string {
    return this.answererName || 'Someone';
  }

  private jump(): void {
    const id = this.responseSet?.promptMessageId || this.response?.promptMessageId;
    if (!id) return;
    this.dispatchEvent(
      new CustomEvent<{ messageId: string }>('scroll-to-message', {
        detail: { messageId: id },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render(): TemplateResult | typeof nothing {
    if (this.responseSet) return this.renderSet(this.responseSet);
    if (this.response) return this.renderSingle(this.response);
    return nothing;
  }

  private renderTime(): TemplateResult | typeof nothing {
    const at = this.timestamp;
    if (!at) return nothing;
    return html`<span class="sep" aria-hidden="true">·</span>
      <time datetime=${at} title=${formatInstantWithZone(at)}>${formatInstant(at, 'time')}</time>`;
  }

  private renderNotDelivered(): TemplateResult | typeof nothing {
    if (this.dispatchState !== 'failed') return nothing;
    return html`<span class="sep" aria-hidden="true">·</span>
      <span class="failed" data-state="not-delivered">not delivered (agent gone)</span>`;
  }

  private renderSingle(r: ChoiceResponseView): TemplateResult {
    const interpreted = r.provenance === INTERPRETED;
    const question = r.question?.trim();
    return html`<div class="line" data-kind="single">
      <sl-icon
        class=${interpreted ? 'robot' : 'ok'}
        name=${interpreted ? 'robot' : 'check-circle-fill'}
        aria-hidden="true"
      ></sl-icon>
      <b>${this.who}</b> chose <b>${r.label}</b> for
      ${r.key ? html`<span class="key">${r.key}</span>` : nothing}
      ${question ? html`<span class="q">${question}</span>` : nothing}
      ${interpreted
        ? html`<span class="sep" aria-hidden="true">·</span>
            <span data-state="interpreted"
              >recorded from ${this.who}'s reply by ${this.agentName || 'the agent'}</span
            >`
        : nothing}
      ${this.renderTime()} ${this.renderNotDelivered()}
      <span class="sep" aria-hidden="true">·</span>
      <button type="button" class="link" data-action="jump" @click=${(): void => this.jump()}>
        Jump
      </button>
    </div>`;
  }

  private renderSet(rs: ChoiceResponseSetView): TemplateResult {
    const answers = rs.answers ?? [];
    const n = answers.length;
    const interpreted = answers.some((a) => a.provenance === INTERPRETED);
    return html`<details
      data-kind="set"
      ?open=${this.open}
      @toggle=${(e: Event): void => {
        this.open = (e.currentTarget as HTMLDetailsElement).open;
      }}
    >
      <summary>
        <sl-icon
          class=${interpreted ? 'robot' : 'ok'}
          name=${interpreted ? 'robot' : 'check-circle-fill'}
          aria-hidden="true"
        ></sl-icon>
        <b>${this.who}</b> answered ${n} ${n === 1 ? 'decision' : 'decisions'}
        ${interpreted
          ? html`<span class="sep" aria-hidden="true">·</span>
              <span data-state="interpreted"
                >recorded from ${this.who}'s reply by ${this.agentName || 'the agent'}</span
              >`
          : nothing}
        ${this.renderTime()} ${this.renderNotDelivered()}
        <span class="sep" aria-hidden="true">·</span>
        <span class="link">${this.open ? 'Hide answers ▾' : 'Show answers ▸'}</span>
      </summary>
      <ul class="answers">
        ${answers.map(
          (a) =>
            html`<li data-row=${a.rowId}>
              <span class="key">${a.key}</span>
              <span class="lbl"
                >${a.label}${a.matchedDefault ? html` <i>(recommended)</i>` : nothing}</span
              >
            </li>`
        )}
        <li>
          <span></span>
          <span
            ><button
              type="button"
              class="link"
              data-action="jump"
              @click=${(): void => this.jump()}
            >
              Jump to question
            </button></span
          >
        </li>
      </ul>
    </details>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-choice-response': ScionChatChoiceResponse;
  }
}
