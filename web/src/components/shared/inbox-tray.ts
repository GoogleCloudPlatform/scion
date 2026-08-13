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
 * Inbox Tray Component
 *
 * Envelope icon + popover panel that polls for unread messages from agents
 * and lets the user mark them as read individually or in bulk.
 * Receives real-time updates via the shared stateManager SSE connection.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import type { User, Message } from '../../shared/types.js';

const POLL_INTERVAL_MS = 5 * 60_000; // 5 minutes — fallback only; SSE delivers in real-time

@customElement('scion-inbox-tray')
export class ScionInboxTray extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @state() private messages: Message[] = [];
  @state() private open = false;

  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private boundOnClickOutside = this.onClickOutside.bind(this);
  private boundOnUserMessage = this.onUserMessageEvent.bind(this);

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.user) {
      void this.fetchMessages();
      this.startPolling();
      this.listenForMessages();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopPolling();
    this.stopListeningForMessages();
    document.removeEventListener('click', this.boundOnClickOutside, true);
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('user')) {
      if (this.user) {
        void this.fetchMessages();
        this.startPolling();
        this.listenForMessages();
      } else {
        this.stopPolling();
        this.stopListeningForMessages();
        this.messages = [];
      }
    }
  }

  // ---------------------------------------------------------------------------
  // Polling
  // ---------------------------------------------------------------------------

  private startPolling(): void {
    this.stopPolling();
    this.pollTimer = setInterval(() => void this.fetchMessages(), POLL_INTERVAL_MS);
  }

  private stopPolling(): void {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
  }

  // ---------------------------------------------------------------------------
  // Real-time delivery (via shared stateManager SSE connection)
  // ---------------------------------------------------------------------------

  private listenForMessages(): void {
    stateManager.addEventListener('user-message-created', this.boundOnUserMessage);
  }

  private stopListeningForMessages(): void {
    stateManager.removeEventListener('user-message-created', this.boundOnUserMessage);
  }

  private onUserMessageEvent(): void {
    void this.fetchMessages();
  }

  // ---------------------------------------------------------------------------
  // API
  // ---------------------------------------------------------------------------

  private async fetchMessages(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/messages?unread=true');
      if (!res.ok) return;
      const data = (await res.json()) as { items?: Message[] } | null;
      this.messages = data?.items ?? [];
    } catch {
      // Silently ignore network errors during polling
    }
  }

  private async markOne(id: string): Promise<void> {
    try {
      await apiFetch(`/api/v1/messages/${id}/read`, { method: 'POST' });
      this.messages = this.messages.filter((m) => m.id !== id);
    } catch {
      // Ignore
    }
  }

  private async markAll(): Promise<void> {
    try {
      await apiFetch('/api/v1/messages/read-all', { method: 'POST' });
      this.messages = [];
    } catch {
      // Ignore
    }
  }

  // ---------------------------------------------------------------------------
  // Panel toggle & click-outside
  // ---------------------------------------------------------------------------

  private toggle(): void {
    this.open = !this.open;
    if (this.open) {
      requestAnimationFrame(() => {
        document.addEventListener('click', this.boundOnClickOutside, true);
      });
      void this.fetchMessages();
    } else {
      document.removeEventListener('click', this.boundOnClickOutside, true);
    }
  }

  private onClickOutside(e: Event): void {
    const path = e.composedPath();
    if (!path.includes(this)) {
      this.open = false;
      document.removeEventListener('click', this.boundOnClickOutside, true);
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private typeIcon(type: string): string {
    switch (type) {
      case 'input-needed':
        return 'exclamation-circle-fill';
      case 'state-change':
        return 'info-circle-fill';
      default:
        return 'chat-dots';
    }
  }

  private typeClass(type: string): string {
    switch (type) {
      case 'input-needed':
        return 'type-warning';
      case 'state-change':
        return 'type-info';
      default:
        return 'type-default';
    }
  }

  private relativeTime(iso: string): string {
    const diff = Date.now() - new Date(iso).getTime();
    const seconds = Math.floor(diff / 1000);
    if (seconds < 60) return 'just now';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  }

  private agentLabel(msg: Message): string {
    // sender is like "agent:code-reviewer" — strip the prefix for display
    const sender = msg.sender || '';
    if (sender.startsWith('agent:')) return sender.slice(6);
    return sender || 'agent';
  }

  // ---------------------------------------------------------------------------
  // Styles
  // ---------------------------------------------------------------------------

  static override styles = css`
    :host {
      position: relative;
      display: inline-flex;
      align-items: center;
    }

    /* Envelope button */
    .inbox-btn {
      position: relative;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2.25rem;
      height: 2.25rem;
      border: none;
      border-radius: 0.5rem;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      transition:
        background 0.15s ease,
        color 0.15s ease;
    }

    .inbox-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .inbox-btn sl-icon {
      font-size: 1.25rem;
    }

    /* Badge */
    .badge {
      position: absolute;
      top: 2px;
      right: 2px;
      min-width: 16px;
      height: 16px;
      padding: 0 4px;
      border-radius: 8px;
      background: var(--scion-primary, #3b82f6);
      color: #fff;
      font-size: 0.625rem;
      font-weight: 700;
      line-height: 16px;
      text-align: center;
      pointer-events: none;
    }

    .badge.pulse {
      animation: badge-pulse 2s ease-in-out infinite;
    }

    @keyframes badge-pulse {
      0%,
      100% {
        transform: scale(1);
        opacity: 1;
      }
      50% {
        transform: scale(1.15);
        opacity: 0.85;
      }
    }

    /* Panel */
    .panel {
      position: absolute;
      top: calc(100% + 0.5rem);
      right: 0;
      width: 360px;
      max-height: 480px;
      display: flex;
      flex-direction: column;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.75rem;
      box-shadow:
        0 10px 15px -3px rgba(0, 0, 0, 0.1),
        0 4px 6px -4px rgba(0, 0, 0, 0.1);
      z-index: 1000;
      overflow: hidden;
    }

    .panel-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .panel-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .mark-all-btn {
      border: none;
      background: transparent;
      color: var(--scion-primary, #3b82f6);
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      border-radius: 0.25rem;
      transition: background 0.15s ease;
    }

    .mark-all-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .panel-list {
      flex: 1;
      overflow-y: auto;
      overscroll-behavior: contain;
    }

    /* Message item */
    .msg-item {
      display: flex;
      gap: 0.625rem;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      transition: background 0.1s ease;
    }

    .msg-item:last-child {
      border-bottom: none;
    }

    .msg-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .msg-icon {
      flex-shrink: 0;
      display: flex;
      align-items: flex-start;
      padding-top: 2px;
    }

    .msg-icon sl-icon {
      font-size: 1rem;
    }

    .type-warning sl-icon {
      color: var(--scion-warning, #f59e0b);
    }

    .type-info sl-icon {
      color: var(--scion-text-muted, #64748b);
    }

    .type-default sl-icon {
      color: var(--scion-primary, #3b82f6);
    }

    .msg-body {
      flex: 1;
      min-width: 0;
    }

    .msg-from {
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.125rem;
    }

    .msg-text {
      font-size: 0.8125rem;
      line-height: 1.4;
      color: var(--scion-text, #1e293b);
      word-break: break-word;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
    }

    .msg-meta {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.25rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .msg-type-badge {
      display: inline-block;
      padding: 0.0625rem 0.3125rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      font-size: 0.625rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
    }

    .mark-read-link {
      border: none;
      background: transparent;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
      cursor: pointer;
      padding: 0;
      transition: color 0.15s ease;
    }

    .mark-read-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    /* Empty state */
    .empty-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 2.5rem 1rem;
      gap: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2rem;
      opacity: 0.4;
    }

    .empty-state span {
      font-size: 0.8125rem;
    }

    /* Mobile */
    @media (max-width: 640px) {
      .panel {
        position: fixed;
        top: auto;
        bottom: 0;
        left: 0;
        right: 0;
        width: 100%;
        max-height: 70vh;
        border-radius: 0.75rem 0.75rem 0 0;
      }
    }
  `;

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    const count = this.messages.length;

    return html`
      <button
        class="inbox-btn"
        @click=${(): void => this.toggle()}
        aria-label="Inbox"
        aria-haspopup="true"
        aria-expanded=${this.open}
      >
        <sl-icon name="envelope"></sl-icon>
        ${count > 0
          ? html`<span class="badge pulse">${count > 99 ? '99+' : count}</span>`
          : nothing}
      </button>
      ${this.open ? this.renderPanel() : nothing}
    `;
  }

  private renderPanel() {
    const count = this.messages.length;
    return html`
      <div class="panel" role="dialog" aria-label="Inbox">
        <div class="panel-header">
          <h3 class="panel-title">Inbox</h3>
          ${count > 0
            ? html`<button class="mark-all-btn" @click=${(): void => void this.markAll()}>
                Mark all read
              </button>`
            : nothing}
        </div>
        <div class="panel-list">
          ${count > 0 ? this.messages.map((m) => this.renderItem(m)) : this.renderEmpty()}
        </div>
      </div>
    `;
  }

  private renderItem(msg: Message) {
    return html`
      <div class="msg-item">
        <div class="msg-icon ${this.typeClass(msg.type)}">
          <sl-icon name=${this.typeIcon(msg.type)}></sl-icon>
        </div>
        <div class="msg-body">
          <div class="msg-from">${this.agentLabel(msg)}</div>
          <div class="msg-text">${msg.msg}</div>
          <div class="msg-meta">
            <span>${this.relativeTime(msg.createdAt)}</span>
            ${msg.type ? html`<span class="msg-type-badge">${msg.type}</span>` : nothing}
            <button class="mark-read-link" @click=${(): void => void this.markOne(msg.id)}>
              Mark read
            </button>
          </div>
        </div>
      </div>
    `;
  }

  private renderEmpty() {
    return html`
      <div class="empty-state">
        <sl-icon name="envelope"></sl-icon>
        <span>No unread messages</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-inbox-tray': ScionInboxTray;
  }
}
