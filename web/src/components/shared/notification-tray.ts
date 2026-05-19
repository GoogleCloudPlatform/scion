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
 * Notification Tray Component
 *
 * Self-contained bell icon + popover panel that polls for unacknowledged
 * notifications and lets the user acknowledge them individually or in bulk.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import type { User, Notification } from '../../shared/types.js';

const POLL_INTERVAL_MS = 5 * 60_000; // 5 minutes — fallback only; SSE delivers in real-time
const PUSH_STORAGE_KEY = 'scion-push-notifications';

@customElement('scion-notification-tray')
export class ScionNotificationTray extends LitElement {
  @property({ type: Object })
  user: User | null = null;

  @state() private notifications: Notification[] = [];
  @state() private open = false;

  private pollTimer: ReturnType<typeof setInterval> | null = null;
  private boundOnClickOutside = this.onClickOutside.bind(this);
  private boundOnNotification = this.onNotificationEvent.bind(this);

  /** IDs already seen — used to detect genuinely new notifications. */
  private seenIds = new Set<string>();

  /** Suppresses browser push for the initial fetch so existing notifications don't fire. */
  private initialFetchDone = false;

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.user) {
      void this.fetchNotifications();
      this.startPolling();
      this.listenForNotifications();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopPolling();
    this.stopListeningForNotifications();
    document.removeEventListener('click', this.boundOnClickOutside, true);
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('user')) {
      if (this.user) {
        void this.fetchNotifications();
        this.startPolling();
        this.listenForNotifications();
      } else {
        this.stopPolling();
        this.stopListeningForNotifications();
        this.notifications = [];
      }
    }
    this.detectTruncation();
  }

  private detectTruncation(): void {
    const messages = this.shadowRoot?.querySelectorAll('.notif-message');
    if (!messages) return;
    messages.forEach((el) => {
      const badge = el.parentElement?.querySelector('.truncation-badge') as HTMLElement | null;
      if (!badge) return;
      badge.style.display = el.scrollHeight > el.clientHeight ? 'inline-flex' : 'none';
    });
  }

  // ---------------------------------------------------------------------------
  // Polling
  // ---------------------------------------------------------------------------

  private startPolling(): void {
    this.stopPolling();
    this.pollTimer = setInterval(() => void this.fetchNotifications(), POLL_INTERVAL_MS);
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

  private listenForNotifications(): void {
    stateManager.addEventListener('notification-created', this.boundOnNotification);
  }

  private stopListeningForNotifications(): void {
    stateManager.removeEventListener('notification-created', this.boundOnNotification);
  }

  private onNotificationEvent(): void {
    // The stateManager fires notification-created when a notification SSE
    // event arrives. Re-fetch to get the full notification objects.
    void this.fetchNotifications();
  }

  // ---------------------------------------------------------------------------
  // API
  // ---------------------------------------------------------------------------

  private async fetchNotifications(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/notifications?acknowledged=false');
      if (!res.ok) return;
      const data = (await res.json()) as Notification[] | null;
      const incoming = data ?? [];

      // Detect new notifications (IDs not previously seen) and dispatch
      // browser push for them — but only after the first fetch so we don't
      // blast existing unacknowledged notifications on page load.
      if (this.initialFetchDone) {
        for (const n of incoming) {
          if (!this.seenIds.has(n.id)) {
            this.dispatchBrowserNotification(n);
          }
        }
      }

      // Update seen set to current snapshot
      this.seenIds = new Set(incoming.map((n) => n.id));
      this.initialFetchDone = true;
      this.notifications = incoming;
    } catch {
      // Silently ignore network errors during polling
    }
  }

  /**
   * Fires a browser Notification if the user has opted in via profile settings
   * and the browser has granted permission.
   */
  private dispatchBrowserNotification(n: Notification): void {
    if (
      !('Notification' in window) ||
      window.Notification.permission !== 'granted' ||
      localStorage.getItem(PUSH_STORAGE_KEY) !== 'true'
    ) {
      return;
    }

    const title = this.browserNotificationTitle(n.status);
    new window.Notification(title, {
      body: n.message,
      tag: n.id, // deduplicate if the same notification is seen again
      icon: '/scion-notification-icon.png',
    });
  }

  private browserNotificationTitle(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'Agent Completed';
      case 'WAITING_FOR_INPUT':
        return 'Agent Needs Input';
      case 'LIMITS_EXCEEDED':
        return 'Agent Limits Exceeded';
      default:
        return 'Scion Notification';
    }
  }

  private async ackOne(id: string): Promise<void> {
    try {
      await apiFetch(`/api/v1/notifications/${id}/ack`, { method: 'POST' });
      this.notifications = this.notifications.filter((n) => n.id !== id);
    } catch {
      // Ignore
    }
  }

  private async ackAll(): Promise<void> {
    try {
      await apiFetch('/api/v1/notifications/ack-all', { method: 'POST' });
      this.notifications = [];
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
      // Defer listener so the current click doesn't immediately close
      requestAnimationFrame(() => {
        document.addEventListener('click', this.boundOnClickOutside, true);
      });
      // Refresh on open
      void this.fetchNotifications();
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

  private statusIcon(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'check-circle-fill';
      case 'WAITING_FOR_INPUT':
        return 'exclamation-circle-fill';
      case 'LIMITS_EXCEEDED':
        return 'x-circle-fill';
      default:
        return 'info-circle-fill';
    }
  }

  private statusClass(status: string): string {
    switch (status) {
      case 'COMPLETED':
        return 'status-success';
      case 'WAITING_FOR_INPUT':
        return 'status-warning';
      case 'LIMITS_EXCEEDED':
        return 'status-danger';
      default:
        return 'status-info';
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

  // ---------------------------------------------------------------------------
  // Styles
  // ---------------------------------------------------------------------------

  static override styles = css`
    :host {
      position: relative;
      display: inline-flex;
      align-items: center;
    }

    /* Bell button */
    .bell-btn {
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

    .bell-btn:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text, #1e293b);
    }

    .bell-btn sl-icon {
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
      background: var(--scion-danger, #ef4444);
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

    /* Notification item */
    .notif-item {
      display: flex;
      gap: 0.625rem;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      transition: background 0.1s ease;
    }

    .notif-item:last-child {
      border-bottom: none;
    }

    .notif-item:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .notif-icon {
      flex-shrink: 0;
      display: flex;
      align-items: flex-start;
      padding-top: 2px;
    }

    .notif-icon sl-icon {
      font-size: 1rem;
    }

    .status-success sl-icon {
      color: var(--scion-success, #22c55e);
    }

    .status-warning sl-icon {
      color: var(--scion-warning, #f59e0b);
    }

    .status-danger sl-icon {
      color: var(--scion-danger, #ef4444);
    }

    .status-info sl-icon {
      color: var(--scion-text-muted, #64748b);
    }

    .notif-body {
      flex: 1;
      min-width: 0;
    }

    .notif-message {
      font-size: 0.8125rem;
      line-height: 1.4;
      color: var(--scion-text, #1e293b);
      word-break: break-word;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
    }

    .truncation-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      padding: 0 0.375rem;
      margin-top: 0.125rem;
      font-size: 0.6875rem;
      font-weight: 700;
      line-height: 1.25rem;
      border-radius: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      letter-spacing: 0.05em;
    }

    .truncation-badge:hover {
      background: var(--scion-border, #e2e8f0);
    }

    .notif-meta {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-top: 0.25rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .notif-meta a {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
    }

    .notif-meta a:hover {
      text-decoration: underline;
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

    .scope-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.125rem;
      font-size: 0.625rem;
      font-weight: 500;
      padding: 0.0625rem 0.3125rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      vertical-align: middle;
    }

    .scope-indicator sl-icon {
      font-size: 0.5625rem;
    }

    .panel-footer {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 0.5rem 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .manage-link {
      border: none;
      background: transparent;
      color: var(--scion-primary, #3b82f6);
      font-size: 0.75rem;
      font-weight: 500;
      cursor: pointer;
      padding: 0.25rem 0.5rem;
      border-radius: 0.25rem;
      text-decoration: none;
      transition: background 0.15s ease;
    }

    .manage-link:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
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
    const count = this.notifications.length;

    return html`
      <button
        class="bell-btn"
        @click=${(): void => this.toggle()}
        aria-label="Notifications"
        aria-haspopup="true"
        aria-expanded=${this.open}
      >
        <sl-icon name="bell"></sl-icon>
        ${count > 0
          ? html`<span class="badge pulse">${count > 99 ? '99+' : count}</span>`
          : nothing}
      </button>
      ${this.open ? this.renderPanel() : nothing}
    `;
  }

  private renderPanel() {
    const count = this.notifications.length;
    return html`
      <div class="panel" role="dialog" aria-label="Notifications">
        <div class="panel-header">
          <h3 class="panel-title">Notifications</h3>
          ${count > 0
            ? html`<button class="mark-all-btn" @click=${(): void => void this.ackAll()}>
                Mark all read
              </button>`
            : nothing}
        </div>
        <div class="panel-list">
          ${count > 0
            ? this.notifications.map((n) => this.renderItem(n))
            : this.renderEmpty()}
        </div>
        <div class="panel-footer">
          <a
            href="/projects"
            class="manage-link"
            @click=${(e: Event): void => {
              e.preventDefault();
              this.open = false;
              document.removeEventListener('click', this.boundOnClickOutside, true);
              window.history.pushState({}, '', '/projects');
              window.dispatchEvent(new PopStateEvent('popstate'));
            }}
          >Manage subscriptions</a>
        </div>
      </div>
    `;
  }

  private renderItem(n: Notification) {
    return html`
      <div class="notif-item">
        <div class="notif-icon ${this.statusClass(n.status)}">
          <sl-icon name=${this.statusIcon(n.status)}></sl-icon>
        </div>
        <div class="notif-body">
          <div class="notif-message">${n.message}</div>
          <sl-tooltip content=${n.message} hoist>
            <span class="truncation-badge" style="display:none">...</span>
          </sl-tooltip>
          <div class="notif-meta">
            <span>${this.relativeTime(n.createdAt)}</span>
            <a href="/agents/${n.agentId}" @click=${(e: Event): void => this.navigateToAgent(e, n.agentId)}>
              View agent
            </a>
            <button
              class="mark-read-link"
              @click=${(): void => void this.ackOne(n.id)}
            >
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
        <sl-icon name="bell-slash"></sl-icon>
        <span>No notifications</span>
      </div>
    `;
  }

  private navigateToAgent(e: Event, agentId: string): void {
    e.preventDefault();
    this.open = false;
    document.removeEventListener('click', this.boundOnClickOutside, true);
    window.history.pushState({}, '', `/agents/${agentId}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-notification-tray': ScionNotificationTray;
  }
}
