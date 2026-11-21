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
 * Preview rows are passed via `.messages`; server-truncated bodies are fetched
 * on demand when the user opens the full-message dialog.
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../../client/api.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';
import { formatChatDate, renderDateDivider, chatDateDividerStyles } from './chat-date-divider.js';
import { formatInstant, formatInstantWithZone } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import type { Message } from '../../../shared/types.js';

@customElement('scion-chat-interagent-marker')
export class ScionChatInteragentMarker extends LitElement {
  /**
   * Re-renders this marker when the effective display zone changes
   * (review R2-1).
   */
  readonly _zone = new DisplayZoneController(this);

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

  /** DM conversation key used to load a truncated message's full body on demand. */
  @property({ type: String, attribute: 'conversation-key' })
  conversationKey = '';
  /** Whether this marker is hidden (eye toggle off). */
  @property({ type: Boolean, reflect: true })
  override hidden = false;

  /** Message currently shown in the full-content popover, if any. */
  @state()
  private expandedMessage: Message | null = null;

  /**
   * Rendered body of the expanded message popover (an `.ia-full-body`
   * element), or null to show the text as is.
   */
  @state()
  private expandedContent: HTMLElement | null = null;

  /** True while the full body for a server-truncated message is loading. */
  @state()
  private loadingFullMessage = false;

  /** True when on-demand full-message fetch failed. */
  @state()
  private fullMessageLoadFailed = false;

  /** Set of message IDs whose `.ia-body` is truncated by the line clamp. */
  @state()
  private truncatedIds: ReadonlySet<string> = new Set();

  /** Per-component cache of fetched full message bodies keyed by message ID. */
  private readonly fullMessageCache = new Map<string, string>();

  /** Sequence token used to discard out-of-date full-message responses. */
  private previewRequestSeq = 0;

  static override styles = [
    chatDateDividerStyles,
    css`
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
        background: color-mix(in srgb, var(--scion-neutral-400, #94a3b8) 10%, transparent);
        border: 1px solid var(--scion-border, rgba(148, 163, 184, 0.2));
        border-radius: 9999px;
        cursor: pointer;
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #64748b);
        transition: background 0.15s;
        user-select: none;
        text-align: center;
      }

      .marker-pill:hover {
        background: color-mix(in srgb, var(--scion-neutral-400, #94a3b8) 18%, transparent);
      }

      /* Expanded state — bordered container, centered to match collapsed pill */
      .marker-expanded {
        max-width: min(70%, 600px); /* match .bubble max-width in chat-message.ts */
        margin-left: auto; /* centered */
        margin-right: auto;
        margin-top: 0.25rem;
        margin-bottom: 0.25rem;
        padding: 0.5rem 1rem;
        background: color-mix(in srgb, var(--scion-neutral-400, #94a3b8) 5%, transparent);
        border: 2px solid var(--scion-border, rgba(148, 163, 184, 0.2));
        border-radius: var(--scion-radius);
        cursor: pointer;
        user-select: none;
      }

      .marker-expanded:hover {
        background: color-mix(in srgb, var(--scion-neutral-400, #94a3b8) 10%, transparent);
      }

      .ia-msg {
        display: block;
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #64748b);
        line-height: 1.4;
        padding: 0.25rem 0;
      }

      .ia-msg-header {
        display: flex;
        align-items: baseline;
        gap: 0.25rem;
        white-space: nowrap;
      }

      .ia-time {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #94a3b8);
        margin-right: 0.25rem;
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
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
      }

      .ia-body {
        margin-top: 0.125rem;
        margin-left: 0.5rem;
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
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #94a3b8);
      }

      .ia-expand sl-icon-button::part(base):hover {
        color: var(--scion-primary-600, #2563eb);
      }

      .ia-cross-project {
        font-size: 0.6875rem;
        font-weight: 500;
        color: var(--sl-color-success-700, #15803d);
        background: var(--sl-color-success-50, #f0fdf4);
        padding: 0 0.25rem;
        border-radius: calc(var(--scion-radius-sm) * 0.75);
        flex-shrink: 0;
        white-space: nowrap;
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
        font-size: var(--chat-fs-base);
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
        font-size: var(--chat-fs-lg);
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
        border-radius: calc(var(--scion-radius) * 0.75);
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
        font-size: var(--chat-fs-lg);
        line-height: 1.6;
        color: var(--scion-text, #1e293b);
      }

      .ia-full-preview .ia-full-status,
      .ia-full-preview .ia-full-error {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        margin-top: 0.75rem;
        font-size: var(--chat-fs-base);
        color: var(--scion-text-muted, #64748b);
      }
    `,
  ];

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('conversationKey')) {
      const prevKey = changed.get('conversationKey') as string | undefined;
      if (prevKey !== undefined && prevKey !== this.conversationKey) {
        this.fullMessageCache.clear();
      }
    }
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

  /**
   * Format a sender/recipient like "agent:slug" to just "slug".
   * For cross-project messages, prefixes the participant's truncated project ID.
   */
  private formatParticipant(value: string, projectId?: string, crossProject = false): string {
    let name = value;
    if (name.startsWith('agent:')) name = name.slice(6);
    else if (name.startsWith('user:')) name = name.slice(5);

    if (crossProject && projectId) {
      const shortProject = projectId.slice(0, 8);
      return `${shortProject} / ${name}`;
    }
    return name;
  }

  /**
   * True when a message crossed project boundaries: its sender and recipient
   * provenance stamps name two different, non-empty projects.
   */
  private isCrossProject(msg: Message): boolean {
    return (
      !!msg.senderProjectId &&
      !!msg.recipientProjectId &&
      msg.senderProjectId !== msg.recipientProjectId
    );
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
      if (
        next.size !== this.truncatedIds.size ||
        [...next].some((id) => !this.truncatedIds.has(id))
      ) {
        this.truncatedIds = next;
      }
    });
  }

  private async renderMarkdownBody(text: string): Promise<HTMLElement | null> {
    try {
      const renderer = await getMarkdownRenderer();
      const fragment = renderer.renderFragment(text);
      if (fragment.hasChildNodes()) {
        const content = document.createElement('div');
        content.className = 'ia-full-body';
        content.append(fragment);
        return content;
      }
    } catch {
      // Fall back to plain text rendering.
    }
    return null;
  }

  /** Open the full-content popover for a message. */
  private async openMessagePreview(msg: Message, e: Event): Promise<void> {
    e.stopPropagation(); // Don't toggle the marker collapse.
    const seq = ++this.previewRequestSeq;
    const cached = this.fullMessageCache.get(msg.id);

    if (!msg.msgTruncated || cached !== undefined) {
      const body = cached ?? msg.msg;
      const content = await this.renderMarkdownBody(body);
      if (seq !== this.previewRequestSeq) return;
      this.loadingFullMessage = false;
      this.fullMessageLoadFailed = false;
      this.expandedContent = content;
      this.expandedMessage =
        cached !== undefined ? { ...msg, msg: cached, msgTruncated: false } : msg;
      return;
    }

    // Open the dialog immediately with the preview and a spinner while loading.
    this.expandedContent = null;
    this.expandedMessage = msg;
    this.loadingFullMessage = true;
    this.fullMessageLoadFailed = false;

    if (!this.conversationKey) {
      this.loadingFullMessage = false;
      this.fullMessageLoadFailed = true;
      return;
    }

    try {
      const params = new URLSearchParams({ messageId: msg.id });
      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/interagent?${params.toString()}`,
        { suppressAccessDeniedToast: true }
      );
      if (seq !== this.previewRequestSeq) return;
      if (!res.ok) {
        throw new Error(`HTTP ${res.status}`);
      }
      const data = (await res.json()) as { messages?: Message[] };
      if (seq !== this.previewRequestSeq) return;
      const full = data?.messages?.[0];
      if (!full || typeof full.msg !== 'string') {
        throw new Error('Missing full message');
      }
      this.fullMessageCache.set(msg.id, full.msg);
      const content = await this.renderMarkdownBody(full.msg);
      if (seq !== this.previewRequestSeq) return;
      this.expandedContent = content;
      this.expandedMessage = { ...msg, msg: full.msg, msgTruncated: false };
      this.loadingFullMessage = false;
    } catch {
      if (seq !== this.previewRequestSeq) return;
      const previewContent = await this.renderMarkdownBody(msg.msg);
      if (seq !== this.previewRequestSeq) return;
      this.expandedContent = previewContent;
      this.loadingFullMessage = false;
      this.fullMessageLoadFailed = true;
    }
  }

  /** Close the message popover. */
  private closeMessagePreview(e: Event): void {
    if (e.target === e.currentTarget) {
      this.previewRequestSeq++;
      this.expandedMessage = null;
      this.expandedContent = null;
      this.loadingFullMessage = false;
      this.fullMessageLoadFailed = false;
    }
  }

  /** Toggle expanded state — click anywhere on the pill. */
  private toggle(): void {
    this.expanded = !this.expanded;
  }

  /** Compact time-only label for the two-line expanded row, e.g. "14:15". */
  private formatTime(iso: string): string {
    return formatInstant(iso, 'time');
  }

  /** Fuller date+time label for the full-content dialog, e.g. "Sep 23, 14:15". */
  private formatDateTime(iso: string): string {
    return formatInstant(iso, 'datetime');
  }

  /** Full instant plus zone label for the `.ia-time` tooltip (review R1-3, AC4). */
  private formatTimeTitle(iso: string): string {
    return formatInstantWithZone(iso);
  }

  /** Render a single expanded inter-agent message row (header + body). */
  private renderInteragentMessage(m: Message): TemplateResult {
    const crossProject = this.isCrossProject(m);
    return html`
      <div class="ia-msg">
        <div class="ia-msg-header">
          <span class="ia-time" title=${this.formatTimeTitle(m.createdAt)}
            >${this.formatTime(m.createdAt)}</span
          >
          <span class="ia-sender"
            >${this.formatParticipant(m.sender, m.senderProjectId, crossProject)}</span
          >
          <span class="ia-arrow">&rarr;</span>
          <span class="ia-recipient"
            >${this.formatParticipant(m.recipient, m.recipientProjectId, crossProject)}</span
          >
          ${crossProject
            ? html`<span class="ia-cross-project">
                <sl-icon name="globe" style="font-size:0.5625rem"></sl-icon>
                cross-project
              </span>`
            : nothing}
          ${this.truncatedIds.has(m.id) || Boolean(m.msgTruncated)
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
        <span class="ia-body" data-msg-id=${m.id}>${m.msg}</span>
      </div>
    `;
  }

  /** Render the full-content dialog for a single message. */
  private renderMessagePreview() {
    const msg = this.expandedMessage;
    if (!msg) return nothing;
    const crossProject = this.isCrossProject(msg);
    return html`
      <sl-dialog
        class="ia-full-preview"
        open
        label="Agent Message"
        @sl-after-hide=${(e: Event) => this.closeMessagePreview(e)}
      >
        <div class="ia-full-header">
          <span class="ia-time" title=${this.formatTimeTitle(msg.createdAt)}
            >${this.formatDateTime(msg.createdAt)}</span
          >
          <span class="ia-sender"
            >${this.formatParticipant(msg.sender, msg.senderProjectId, crossProject)}</span
          >
          <span class="ia-arrow">&rarr;</span>
          <span class="ia-recipient"
            >${this.formatParticipant(msg.recipient, msg.recipientProjectId, crossProject)}</span
          >
          ${crossProject
            ? html`<span class="ia-cross-project">
                <sl-icon name="globe" style="font-size:0.5625rem"></sl-icon> cross-project
              </span>`
            : nothing}
        </div>
        ${this.expandedContent ?? html`<div class="ia-full-body-plain">${msg.msg}</div>`}
        ${this.loadingFullMessage
          ? html`<div class="ia-full-status" role="status">
              <sl-spinner></sl-spinner>
              <span>Loading full message&hellip;</span>
            </div>`
          : nothing}
        ${this.fullMessageLoadFailed
          ? html`<div class="ia-full-error" role="status">Couldn't load full message</div>`
          : nothing}
      </sl-dialog>
    `;
  }

  override render() {
    if (this.expanded) {
      const rows: TemplateResult[] = [];
      let lastDate = '';
      let sawFirstDate = false;
      for (let i = 0; i < this.messages.length; i++) {
        const m = this.messages[i];
        const dateStr = formatChatDate(m.createdAt);
        // A marker now normally holds a single calendar day of messages (the
        // main timeline splits runs by day before handing them here), and the
        // main timeline has already rendered the shared separator for that
        // day directly above the marker — so the first day here must not get
        // its own. This loop stays defensive: if it's ever given a multi-day
        // span, later days still fall back to the same separator the main
        // timeline uses, rather than a bespoke style.
        if (dateStr) {
          if (!sawFirstDate) {
            lastDate = dateStr;
            sawFirstDate = true;
          } else if (dateStr !== lastDate) {
            lastDate = dateStr;
            rows.push(renderDateDivider(dateStr));
          }
        }
        rows.push(this.renderInteragentMessage(m));
      }
      return html`
        <sl-tooltip content="Click to collapse">
          <div class="marker-expanded" @click=${this.toggle}>${rows}</div>
        </sl-tooltip>
        ${this.renderMessagePreview()}
      `;
    }

    const crossCount = this.messages.filter((m) => this.isCrossProject(m)).length;
    return html`
      <sl-tooltip content="Click to expand">
        <div class="marker-pill" @click=${this.toggle}>
          ${this.messageCount} agent-agent
          message${this.messageCount !== 1 ? 's' : ''}${crossCount > 0
            ? html` <span class="ia-cross-project">
                <sl-icon name="globe" style="font-size:0.5625rem"></sl-icon> ${crossCount}
                cross-project
              </span>`
            : nothing}
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
