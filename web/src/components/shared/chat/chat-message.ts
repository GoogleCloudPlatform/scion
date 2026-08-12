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
 * Chat message bubble component.
 *
 * Renders one message in the chat thread. Handles:
 * - User messages right-aligned, agent messages left-aligned
 * - Agent avatar (colour + initials hashed from slug)
 * - Markdown content via the shared utility (or preformatted for plain:true)
 * - Code blocks: monospace, horizontal scroll, copy button (no syntax highlighting)
 * - Attachments: chip showing basename, full path on hover, NOT clickable
 * - Badges: urgent, broadcasted, channel provenance
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';

@customElement('scion-chat-message')
export class ScionChatMessage extends LitElement {
  /** The message body text. */
  @property()
  body = '';

  /** Sender display name. */
  @property()
  sender = '';

  /** Whether this is a message from the agent (left-aligned) or user (right-aligned). */
  @property({ type: Boolean })
  fromAgent = false;

  /** Whether the message is plain text (no markdown rendering). */
  @property({ type: Boolean })
  plain = false;

  /** Agent slug for avatar generation. */
  @property()
  agentSlug = '';

  /** Timestamp string. */
  @property()
  timestamp = '';

  /** Whether to show the sender header (false when grouped with previous message). */
  @property({ type: Boolean })
  showHeader = true;

  /** Whether this message is marked urgent. */
  @property({ type: Boolean })
  urgent = false;

  /** Whether this message was broadcasted. */
  @property({ type: Boolean })
  broadcasted = false;

  /** Channel provenance (e.g. "discord", "telegram"). */
  @property()
  channel = '';

  /** Visibility level: "normal", "verbose", or "full". */
  @property()
  visibility = 'normal';

  /** Message type (e.g. "assistant-reply", "state-change"). */
  @property()
  messageType = '';

  /** Dispatch state: "pending", "dispatched", or "failed". */
  @property()
  dispatchState = '';

  /** Reason for dispatch failure. */
  @property()
  dispatchFailureReason = '';

  /** File attachment paths. */
  @property({ type: Array })
  attachments: string[] = [];

  @state()
  private renderedHtml = '';

  private renderTaskId = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .message-wrapper {
      display: flex;
      gap: 0.5rem;
      padding: 0.125rem 1rem;
    }

    .message-wrapper.from-user {
      flex-direction: row-reverse;
    }

    /* Avatar */
    .avatar {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 0.75rem;
      font-weight: 700;
      color: #fff;
      flex-shrink: 0;
      text-transform: uppercase;
      margin-top: 0.125rem;
    }

    .avatar-spacer {
      width: 2rem;
      flex-shrink: 0;
    }

    /* Bubble */
    .bubble {
      max-width: min(70%, 600px);
      min-width: 3rem;
    }

    .bubble-header {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      margin-bottom: 0.125rem;
    }

    .sender-name {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .msg-time {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .bubble-content {
      padding: 0.5rem 0.75rem;
      border-radius: 0.75rem;
      line-height: 1.5;
      font-size: 0.875rem;
      word-break: break-word;
    }

    .from-agent .bubble-content {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
      border-top-left-radius: 0.25rem;
    }

    .from-user .bubble-content {
      background: var(--scion-primary-50, #eff6ff);
      color: var(--scion-text, #1e293b);
      border-top-right-radius: 0.25rem;
    }

    .from-user .bubble-header {
      flex-direction: row-reverse;
    }

    /* Pre-formatted (plain) text */
    .plain-text {
      white-space: pre-wrap;
      font-family: inherit;
    }

    /* Markdown content styles */
    .md-content {
      overflow-wrap: break-word;
    }

    .md-content p {
      margin: 0 0 0.5em;
    }

    .md-content p:last-child {
      margin-bottom: 0;
    }

    .md-content h1,
    .md-content h2,
    .md-content h3,
    .md-content h4 {
      margin: 0.75em 0 0.25em;
      font-weight: 600;
      line-height: 1.3;
    }

    .md-content h1:first-child,
    .md-content h2:first-child,
    .md-content h3:first-child {
      margin-top: 0;
    }

    .md-content h1 { font-size: 1.25rem; }
    .md-content h2 { font-size: 1.125rem; }
    .md-content h3 { font-size: 1rem; }

    .md-content a {
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
    }

    .md-content a:hover {
      text-decoration: underline;
    }

    .md-content code {
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      font-size: 0.8125em;
      background: var(--scion-surface, #ffffff);
      padding: 0.1em 0.3em;
      border-radius: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .md-content pre {
      position: relative;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      padding: 0.75rem;
      overflow-x: auto;
      margin: 0.5em 0;
    }

    .md-content pre code {
      background: none;
      border: none;
      padding: 0;
      font-size: 0.8125rem;
    }

    .copy-btn {
      position: absolute;
      top: 0.375rem;
      right: 0.375rem;
      padding: 0.125rem 0.5rem;
      font-size: 0.6875rem;
      font-family: inherit;
      line-height: 1.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.25rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      opacity: 0.6;
      transition: opacity 0.15s;
    }

    .copy-btn:hover {
      opacity: 1;
    }

    .md-content ul,
    .md-content ol {
      margin: 0.25em 0 0.5em;
      padding-left: 1.25em;
    }

    .md-content li {
      margin-bottom: 0.125em;
    }

    .md-content blockquote {
      border-left: 3px solid var(--scion-border, #e2e8f0);
      margin: 0.5em 0;
      padding: 0.25em 0.75em;
      color: var(--scion-text-muted, #64748b);
    }

    .md-content blockquote p:last-child {
      margin-bottom: 0;
    }

    .md-content table {
      border-collapse: collapse;
      width: 100%;
      margin: 0.5em 0;
      font-size: 0.8125rem;
    }

    .md-content th,
    .md-content td {
      border: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.375em 0.5em;
      text-align: left;
    }

    .md-content th {
      background: var(--scion-bg-subtle, #f8fafc);
      font-weight: 600;
    }

    /* Badges row */
    .badges {
      display: flex;
      gap: 0.25rem;
      margin-top: 0.25rem;
    }

    .badge {
      display: inline-block;
      padding: 0 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.625rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      line-height: 1.25rem;
    }

    .badge-urgent {
      background: var(--scion-danger-50, #fef2f2);
      color: var(--scion-danger-700, #b91c1c);
    }

    .badge-broadcast {
      background: var(--scion-warning-50, #fffbeb);
      color: var(--scion-warning-700, #b45309);
    }

    .badge-channel {
      background: var(--scion-neutral-100, #f1f5f9);
      color: var(--scion-neutral-600, #475569);
    }

    /* Attachments */
    .attachments {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.375rem;
    }

    .attachment-chip {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      cursor: default;
    }

    .attachment-chip sl-icon {
      font-size: 0.75rem;
    }

    /* Verbose (recessed) rendering — no bubble, muted text, small label */
    .message-wrapper.verbose .bubble-content {
      background: none;
      padding: 0.25rem 0.75rem;
      border-radius: 0;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      font-style: italic;
    }

    .verbose-label {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.625rem;
      font-weight: 500;
      color: var(--scion-text-muted, #94a3b8);
      text-transform: uppercase;
      letter-spacing: 0.04em;
      margin-bottom: 0.125rem;
    }

    .verbose-label sl-icon {
      font-size: 0.6875rem;
    }

    /* Full/trace rendering — collapsed details block */
    .trace-block {
      padding: 0.125rem 1rem;
    }

    .trace-block details {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.375rem;
      background: var(--scion-bg-subtle, #f8fafc);
      max-width: min(80%, 700px);
    }

    .trace-block summary {
      padding: 0.375rem 0.75rem;
      font-size: 0.6875rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      user-select: none;
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .trace-block summary sl-icon {
      font-size: 0.75rem;
    }

    .trace-content {
      padding: 0.5rem 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      border-top: 1px solid var(--scion-border, #e2e8f0);
      white-space: pre-wrap;
      font-family: var(--scion-font-mono, 'SF Mono', 'Fira Code', monospace);
      max-height: 300px;
      overflow-y: auto;
    }

    /* Delivery state indicators */
    .delivery-state {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      margin-top: 0.125rem;
      font-size: 0.625rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .delivery-state sl-icon {
      font-size: 0.6875rem;
    }

    .delivery-state.pending sl-icon {
      color: var(--scion-text-muted, #94a3b8);
    }

    .delivery-state.dispatched sl-icon {
      color: var(--scion-success-500, #22c55e);
    }

    .delivery-state.failed {
      color: var(--scion-danger-600, #dc2626);
    }

    .delivery-state.failed sl-icon {
      color: var(--scion-danger-600, #dc2626);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.renderContent();
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('body') || changed.has('plain')) {
      void this.renderContent();
    }
    if (changed.has('renderedHtml')) {
      this.injectCopyButtons();
    }
  }

  /** Inject copy buttons on all code blocks inside rendered markdown. */
  private injectCopyButtons(): void {
    this.shadowRoot?.querySelectorAll('.md-content pre').forEach((pre) => {
      if (pre.querySelector('.copy-btn')) return;
      const btn = document.createElement('button');
      btn.className = 'copy-btn';
      btn.textContent = 'Copy';
      btn.addEventListener('click', () => {
        const code =
          pre.querySelector('code')?.textContent ?? pre.textContent ?? '';
        navigator.clipboard.writeText(code);
        btn.textContent = 'Copied!';
        setTimeout(() => {
          btn.textContent = 'Copy';
        }, 1500);
      });
      pre.appendChild(btn);
    });
  }

  private async renderContent(): Promise<void> {
    if (!this.body || this.plain) {
      this.renderedHtml = '';
      return;
    }
    const taskId = ++this.renderTaskId;
    try {
      const renderer = await getMarkdownRenderer();
      if (taskId !== this.renderTaskId) return;
      this.renderedHtml = renderer.render(this.body);
    } catch {
      if (taskId !== this.renderTaskId) return;
      this.renderedHtml = '';
    }
  }

  override render() {
    // Full/trace messages render as a collapsed details block.
    if (this.visibility === 'full') {
      return this.renderTraceBlock();
    }

    const dirClass = this.fromAgent ? 'from-agent' : 'from-user';
    const visClass = this.visibility === 'verbose' ? ' verbose' : '';

    return html`
      <div class="message-wrapper ${dirClass}${visClass}">
        ${this.showHeader && this.fromAgent
          ? html`<div class="avatar" style="background: ${this.getAvatarColor()}">${this.getInitials()}</div>`
          : this.fromAgent
            ? html`<div class="avatar-spacer"></div>`
            : nothing}
        <div class="bubble">
          ${this.visibility === 'verbose'
            ? html`
                <span class="verbose-label">
                  <sl-icon name="arrow-return-right"></sl-icon>
                  assistant reply
                </span>
              `
            : nothing}
          ${this.showHeader && this.visibility !== 'verbose'
            ? html`
                <div class="bubble-header">
                  <span class="sender-name">${this.sender}</span>
                  <span class="msg-time">${this.formatTime()}</span>
                </div>
              `
            : nothing}
          <div class="bubble-content">
            ${this.renderBody()}
          </div>
          ${this.renderDeliveryState()}
          ${this.renderBadges()}
          ${this.renderAttachments()}
        </div>
      </div>
    `;
  }

  /** Render a collapsed trace block for full-visibility messages. */
  private renderTraceBlock() {
    return html`
      <div class="trace-block">
        <details>
          <summary>
            <sl-icon name="code-slash"></sl-icon>
            Trace — ${this.sender} at ${this.formatTime()}
          </summary>
          <div class="trace-content">${this.body}</div>
        </details>
      </div>
    `;
  }

  /** Render delivery state indicator for outbound (user-sent) messages. */
  private renderDeliveryState() {
    // Only show on user-sent messages with a dispatch state.
    if (this.fromAgent || !this.dispatchState) return nothing;

    switch (this.dispatchState) {
      case 'pending':
        return html`
          <div class="delivery-state pending">
            <sl-icon name="clock"></sl-icon>
            Sending
          </div>
        `;
      case 'dispatched':
        return html`
          <div class="delivery-state dispatched">
            <sl-icon name="check2"></sl-icon>
            Delivered
          </div>
        `;
      case 'failed':
        return html`
          <sl-tooltip content=${this.dispatchFailureReason || 'Delivery failed'} hoist>
            <div class="delivery-state failed">
              <sl-icon name="exclamation-triangle"></sl-icon>
              Failed
            </div>
          </sl-tooltip>
        `;
      default:
        return nothing;
    }
  }

  private renderBody() {
    if (this.plain || !this.renderedHtml) {
      return html`<div class="plain-text">${this.body}</div>`;
    }
    return html`<div class="md-content" .innerHTML=${this.renderedHtml}></div>`;
  }

  private renderBadges() {
    const hasBadges = this.urgent || this.broadcasted || (this.channel && this.channel !== 'web');
    if (!hasBadges) return nothing;

    return html`
      <div class="badges">
        ${this.urgent ? html`<span class="badge badge-urgent">urgent</span>` : nothing}
        ${this.broadcasted ? html`<span class="badge badge-broadcast">broadcast</span>` : nothing}
        ${this.channel && this.channel !== 'web'
          ? html`<span class="badge badge-channel">via ${this.channel}</span>`
          : nothing}
      </div>
    `;
  }

  private renderAttachments() {
    if (!this.attachments || this.attachments.length === 0) return nothing;

    return html`
      <div class="attachments">
        ${this.attachments.map(
          (path) => html`
            <sl-tooltip content=${path} hoist>
              <span class="attachment-chip">
                <sl-icon name="paperclip"></sl-icon>
                ${this.basename(path)}
              </span>
            </sl-tooltip>
          `
        )}
      </div>
    `;
  }

  private formatTime(): string {
    if (!this.timestamp) return '';
    try {
      const d = new Date(this.timestamp);
      return d.toLocaleTimeString('en', {
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch {
      return '';
    }
  }

  /** Simple deterministic colour from the agent slug. */
  private getAvatarColor(): string {
    const colors = [
      '#3b82f6', '#8b5cf6', '#06b6d4', '#10b981',
      '#f59e0b', '#ef4444', '#ec4899', '#6366f1',
    ];
    let hash = 0;
    const s = this.agentSlug || this.sender || '';
    for (let i = 0; i < s.length; i++) {
      hash = s.charCodeAt(i) + ((hash << 5) - hash);
    }
    return colors[Math.abs(hash) % colors.length];
  }

  /** First two characters of the agent slug or sender name. */
  private getInitials(): string {
    const s = this.agentSlug || this.sender || '';
    return s.substring(0, 2);
  }

  /** Extract the file basename from a path. */
  private basename(path: string): string {
    const parts = path.split('/');
    return parts[parts.length - 1] || path;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-message': ScionChatMessage;
  }
}
