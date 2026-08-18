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
 * Inline collapsed inter-agent message marker.
 *
 * Renders a compact pill in the DM timeline showing that inter-agent
 * messages occurred between two user-facing DM messages. Collapsed by
 * default, it shows "(n agent-agent messages)". On expand it displays the
 * individual messages in a compact sender->recipient format.
 *
 * Messages are passed directly via the `.messages` property (no lazy loading).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import type { Message } from '../../../shared/types.js';

@customElement('scion-chat-interagent-marker')
export class ScionChatInteragentMarker extends LitElement {
  /** Number of messages in this group. */
  @property({ type: Number })
  messageCount = 0;

  /** The messages to display when expanded (passed directly from parent). */
  @property({ type: Array })
  messages: Message[] = [];

  /** Whether this marker is expanded (shows individual messages). */
  @property({ type: Boolean, reflect: true })
  expanded = false;

  /** Externally-controlled global expand/collapse override. */
  @property({ type: Boolean, attribute: 'global-expanded' })
  globalExpanded = false;

  /** Whether this marker is hidden (eye toggle off). */
  @property({ type: Boolean, reflect: true })
  override hidden = false;

  /** Message currently shown in the full-content popover, if any. */
  @state()
  private expandedMessage: Message | null = null;

  /** Rendered HTML for the expanded message popover. */
  @state()
  private expandedHtml = '';

  /** Set of message IDs whose `.ia-body` is truncated by the line clamp. */
  @state()
  private truncatedIds: ReadonlySet<string> = new Set();

  static override styles = css`
    :host {
      display: block;
    }

    :host([hidden]) {
      display: none;
    }

    /* Collapsed pill — centered compact badge */
    .marker-pill {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      max-width: 40%;
      margin-left: auto;
      margin-right: auto;
      padding: 0.375rem 1rem;
      margin-top: 0.25rem;
      margin-bottom: 0.25rem;
      background: rgba(148, 163, 184, 0.1);
      border: 1px solid var(--scion-border, rgba(148, 163, 184, 0.2));
      border-radius: 9999px;
      cursor: pointer;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      transition: background 0.15s;
      user-select: none;
      text-align: center;
    }

    .marker-pill:hover {
      background: rgba(148, 163, 184, 0.18);
    }

    /* Expanded state — bordered container, centered to match collapsed pill */
    .marker-expanded {
      max-width: min(70%, 600px);  /* match .bubble max-width in chat-message.ts */
      margin-left: auto;   /* centered */
      margin-right: auto;
      margin-top: 0.25rem;
      margin-bottom: 0.25rem;
      padding: 0.5rem 1rem;
      background: rgba(148, 163, 184, 0.05);
      border: 2px solid var(--scion-border, rgba(148, 163, 184, 0.2));
      border-radius: 0.5rem;
      cursor: pointer;
      user-select: none;
    }

    .marker-expanded:hover {
      background: rgba(148, 163, 184, 0.1);
    }

    .ia-msg {
      display: flex;
      align-items: baseline;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.4;
      padding: 0.125rem 0;
    }

    .ia-sender {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .ia-arrow {
      color: var(--scion-text-muted, #94a3b8);
      flex-shrink: 0;
    }

    .ia-recipient {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .ia-body {
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
    }

    /* Expand icon button — shown only on truncated messages */
    .ia-expand {
      flex-shrink: 0;
      margin-left: auto;
    }

    .ia-expand sl-icon-button::part(base) {
      padding: 0.125rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .ia-expand sl-icon-button::part(base):hover {
      color: var(--scion-primary-600, #2563eb);
    }

    /* Full message popover dialog */
    .ia-full-preview::part(panel) {
      width: 90vw;
      max-width: 700px;
    }

    .ia-full-preview::part(body) {
      padding: 1rem;
    }

    .ia-full-preview .ia-full-header {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.75rem;
      padding-bottom: 0.5rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .ia-full-preview .ia-full-header .ia-sender,
    .ia-full-preview .ia-full-header .ia-recipient {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .ia-full-preview .ia-full-body {
      font-size: 0.875rem;
      line-height: 1.6;
      color: var(--scion-text, #1e293b);
      overflow-wrap: break-word;
    }

    .ia-full-preview .ia-full-body p {
      margin: 0 0 0.5em;
    }

    .ia-full-preview .ia-full-body p:last-child {
      margin-bottom: 0;
    }

    .ia-full-preview .ia-full-body pre {
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      margin: 0.5em 0;
    }

    .ia-full-preview .ia-full-body code {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.8125em;
    }

    .ia-full-preview .ia-full-body-plain {
      white-space: pre-wrap;
      font-size: 0.875rem;
      line-height: 1.6;
      color: var(--scion-text, #1e293b);
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    // React to global expand/collapse toggle changes.
    if (changed.has('globalExpanded')) {
      const wasGlobal = changed.get('globalExpanded') as boolean | undefined;
      if (wasGlobal !== undefined && wasGlobal !== this.globalExpanded) {
        this.expanded = this.globalExpanded;
      }
    }
    // Detect which message bodies are truncated by the CSS line clamp.
    // Only run when expanded or messages actually change to avoid layout thrashing.
    if (this.expanded && (changed.has('expanded') || changed.has('messages'))) {
      this.detectTruncation();
    }
  }

  /** Format a sender/recipient like "agent:slug" to just "slug". */
  private formatParticipant(value: string): string {
    if (value.startsWith('agent:')) return value.slice(6);
    if (value.startsWith('user:')) return value.slice(5);
    return value;
  }

  /**
   * After render, check each `.ia-body` element to see if its content is
   * clipped by the 2-line CSS clamp. Uses `requestAnimationFrame` to ensure
   * layout has been computed.
   */
  private detectTruncation(): void {
    requestAnimationFrame(() => {
      const bodies = this.shadowRoot?.querySelectorAll<HTMLElement>('.ia-body[data-msg-id]');
      if (!bodies) return;
      const next = new Set<string>();
      bodies.forEach((el) => {
        if (el.scrollHeight > el.clientHeight + 1) {
          const id = el.dataset.msgId;
          if (id) next.add(id);
        }
      });
      // Only update state if the set actually changed to avoid re-render loops.
      if (next.size !== this.truncatedIds.size || [...next].some((id) => !this.truncatedIds.has(id))) {
        this.truncatedIds = next;
      }
    });
  }

  /** Open the full-content popover for a message. */
  private async openMessagePreview(msg: Message, e: Event): Promise<void> {
    e.stopPropagation(); // Don't toggle the marker collapse.
    // Await markdown rendering first, then set both expandedHtml and
    // expandedMessage in the same microtask so Lit batches into one render,
    // avoiding a content flash from plain text to rendered HTML.
    let htmlContent = '';
    try {
      const renderer = await getMarkdownRenderer();
      htmlContent = renderer.render(msg.msg);
    } catch {
      htmlContent = '';
    }
    this.expandedHtml = htmlContent;
    this.expandedMessage = msg;
  }

  /** Close the message popover. */
  private closeMessagePreview(e: Event): void {
    if (e.target === e.currentTarget) {
      this.expandedMessage = null;
      this.expandedHtml = '';
    }
  }

  /** Toggle expanded state — click anywhere on the pill. */
  private toggle(): void {
    this.expanded = !this.expanded;
  }

  /** Render the full-content dialog for a single message. */
  private renderMessagePreview() {
    const msg = this.expandedMessage;
    if (!msg) return nothing;
    return html`
      <sl-dialog
        class="ia-full-preview"
        open
        label="Agent Message"
        @sl-after-hide=${(e: Event) => this.closeMessagePreview(e)}
      >
        <div class="ia-full-header">
          <span class="ia-sender">${this.formatParticipant(msg.sender)}</span>
          <span class="ia-arrow">&rarr;</span>
          <span class="ia-recipient">${this.formatParticipant(msg.recipient)}</span>
        </div>
        ${this.expandedHtml
          ? html`<div class="ia-full-body" .innerHTML=${this.expandedHtml}></div>`
          : html`<div class="ia-full-body-plain">${msg.msg}</div>`}
      </sl-dialog>
    `;
  }

  override render() {
    if (this.expanded) {
      return html`
        <sl-tooltip content="Click to collapse">
          <div class="marker-expanded" @click=${this.toggle}>
            ${this.messages.length > 0
              ? this.messages.map(
                  (m) => html`
                    <div class="ia-msg">
                      <span class="ia-sender">${this.formatParticipant(m.sender)}</span>
                      <span class="ia-arrow">&rarr;</span>
                      <span class="ia-recipient">${this.formatParticipant(m.recipient)}</span>:
                      <span class="ia-body" data-msg-id=${m.id}>${m.msg}</span>
                      ${this.truncatedIds.has(m.id)
                        ? html`
                            <span class="ia-expand">
                              <sl-icon-button
                                name="arrows-angle-expand"
                                label="Expand message"
                                @click=${(e: Event) => this.openMessagePreview(m, e)}
                              ></sl-icon-button>
                            </span>
                          `
                        : nothing}
                    </div>
                  `
                )
              : nothing}
          </div>
        </sl-tooltip>
        ${this.renderMessagePreview()}
      `;
    }

    return html`
      <sl-tooltip content="Click to expand">
        <div class="marker-pill" @click=${this.toggle}>
          ${this.messageCount} agent-agent message${this.messageCount !== 1 ? 's' : ''}
        </div>
      </sl-tooltip>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-interagent-marker': ScionChatInteragentMarker;
  }
}
