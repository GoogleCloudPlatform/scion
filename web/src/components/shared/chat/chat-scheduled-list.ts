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
 * The current user's scheduled messages in one conversation, shown at the
 * bottom of the thread as dimmed bubbles with a banner: the send time and a
 * Cancel button while pending; when delivery failed, the reason with Send
 * now (missed or interrupted only), Copy to composer and Dismiss. Only the
 * sender ever receives these (GET and the user-scoped SSE subject). When a
 * message is sent its bubble goes away; the real message arrives on the
 * ordinary chat stream.
 *
 * Cancel and Copy to composer hand the text to the composer with a
 * `chat-scheduled-restore` event (see ScheduledRestoreDetail).
 */

import { LitElement, html, css, nothing } from 'lit';
import type { PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';
import { stateManager } from '../../../client/state.js';
import {
  applyScheduledUpdate,
  cancelScheduledMessage,
  dismissScheduledMessage,
  listScheduledMessages,
  scheduledFailureText,
  scheduledSendNowAllowed,
  sendNowScheduledMessage,
  sortScheduled,
  type ScheduledMessage,
  type ScheduledMessageEvent,
} from '../../../client/chat-scheduled.js';
import { formatInstantWithZone, formatRelative } from '../../../utils/time.js';
import { DisplayZoneController } from '../../../utils/display-zone-controller.js';
import { showToast } from '../../../utils/toast.js';

/** How often relative times ("in 14 hours") are refreshed. */
const RELATIVE_REFRESH_MS = 30_000;

/**
 * `detail` of the `chat-scheduled-restore` event: put `text` into the
 * composer. A cancelled message sets `onlyIfEmpty`, so an unsent draft is
 * never replaced; Copy to composer does not.
 */
export interface ScheduledRestoreDetail {
  text: string;
  onlyIfEmpty: boolean;
}

@customElement('scion-chat-scheduled-list')
export class ScionChatScheduledList extends LitElement {
  /** The conversation whose scheduled messages are shown. */
  @property({ type: String })
  conversationKey = '';

  /** Whether scheduled send is available here (experiment on, a topic). */
  @property({ type: Boolean })
  enabled = false;

  @state() private messages: ScheduledMessage[] = [];
  /** Messages with a Cancel, Send now or Dismiss request in flight. */
  @state() private busy = new Set<string>();

  readonly _zone = new DisplayZoneController(this);
  private refreshTimer: ReturnType<typeof setInterval> | null = null;
  private loadGeneration = 0;
  /**
   * Changes applied while a load is in flight, replayed onto its result so
   * a GET that started earlier cannot overwrite them. Null when idle.
   */
  private updatesDuringLoad: ScheduledMessage[] | null = null;

  static override styles = css`
    :host {
      display: block;
    }
    :host([hidden]) {
      display: none;
    }
    .list {
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 0.5rem;
      padding: 0.25rem 1rem 0.5rem;
      max-height: 40vh;
      overflow-y: auto;
    }
    .item {
      max-width: min(80%, 40rem);
      display: flex;
      flex-direction: column;
      align-items: stretch;
    }
    .bubble {
      opacity: 0.6;
      background: var(--scion-primary-50, #eff6ff);
      border: 1px dashed var(--scion-border, #cbd5e1);
      border-radius: 0.75rem 0.75rem 0 0;
      padding: 0.5rem 0.75rem;
      white-space: pre-wrap;
      overflow-wrap: anywhere;
      font-size: var(--chat-fs-md, 0.875rem);
      color: var(--scion-text, #1e293b);
    }
    .banner {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-top: none;
      border-radius: 0 0 0.75rem 0.75rem;
      font-size: var(--chat-fs-sm, 0.75rem);
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-surface, #ffffff);
    }
    .banner.failed {
      color: var(--sl-color-danger-700, #b91c1c);
      background: var(--sl-color-danger-50, #fef2f2);
      border-color: var(--sl-color-danger-200, #fecaca);
    }
    .banner .when {
      flex: 1;
    }
    .actions {
      display: flex;
      gap: 0.75rem;
      padding: 0.25rem 0.75rem 0;
      font-size: var(--chat-fs-sm, 0.75rem);
    }
    .cancel-btn,
    .action-btn {
      background: none;
      border: none;
      padding: 0;
      font: inherit;
      color: var(--scion-primary, #2563eb);
      cursor: pointer;
      text-decoration: underline;
    }
    .actions .action-btn {
      color: var(--sl-color-danger-700, #b91c1c);
    }
    .cancel-btn[disabled],
    .action-btn[disabled] {
      cursor: default;
      opacity: 0.5;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.addEventListener('chat-scheduled-updated', this.handleScheduledEvent);
    stateManager.addEventListener('connected', this.handleReconnect);
    this.refreshTimer = setInterval(() => this.requestUpdate(), RELATIVE_REFRESH_MS);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('chat-scheduled-updated', this.handleScheduledEvent);
    stateManager.removeEventListener('connected', this.handleReconnect);
    if (this.refreshTimer) clearInterval(this.refreshTimer);
    this.refreshTimer = null;
  }

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('conversationKey') || changed.has('enabled')) {
      this.messages = [];
      void this.load();
    }
  }

  /** Show a message this session just scheduled (before its SSE event). */
  add(message: ScheduledMessage): void {
    if (message.conversationKey !== this.conversationKey) return;
    this.applyUpdate(message);
  }

  /** Apply one change now, and again onto an in-flight load's result. */
  private applyUpdate(message: ScheduledMessage): void {
    this.updatesDuringLoad?.push(message);
    this.messages = applyScheduledUpdate(this.messages, message);
  }

  /** The messages currently shown (for tests). */
  get items(): readonly ScheduledMessage[] {
    return this.messages;
  }

  private async load(): Promise<void> {
    const generation = ++this.loadGeneration;
    const key = this.conversationKey;
    this.updatesDuringLoad = null;
    if (!this.enabled || !key) return;
    this.updatesDuringLoad = [];
    try {
      const list = await listScheduledMessages(key);
      if (generation !== this.loadGeneration) return;
      let merged = sortScheduled(list);
      for (const update of this.updatesDuringLoad ?? []) {
        merged = applyScheduledUpdate(merged, update);
      }
      this.messages = merged;
    } catch {
      // Best effort: the thread works without the list; a reconnect retries.
    } finally {
      if (generation === this.loadGeneration) this.updatesDuringLoad = null;
    }
  }

  private readonly handleReconnect = (): void => {
    void this.load();
  };

  private readonly handleScheduledEvent = (e: Event): void => {
    if (!this.enabled) return;
    const detail = (e as CustomEvent<{ data?: ScheduledMessageEvent }>).detail;
    const m = detail?.data?.scheduledMessage;
    if (!m || m.conversationKey !== this.conversationKey) return;
    this.applyUpdate(m);
  };

  /**
   * Run one row action, at most one at a time per message. On failure the
   * hub's message is shown and the list reloaded, since the row may have
   * changed under it (for example sent while being cancelled).
   */
  private async runAction(
    m: ScheduledMessage,
    fallback: string,
    action: () => Promise<void>
  ): Promise<void> {
    if (this.busy.has(m.id)) return;
    this.busy = new Set([...this.busy, m.id]);
    try {
      await action();
    } catch (err) {
      showToast(err instanceof Error ? err.message : fallback, 'danger');
      void this.load();
    } finally {
      const next = new Set(this.busy);
      next.delete(m.id);
      this.busy = next;
    }
  }

  private cancel(m: ScheduledMessage): Promise<void> {
    return this.runAction(m, 'Failed to cancel message', async () => {
      await cancelScheduledMessage(this.conversationKey, m.id);
      this.applyUpdate({ ...m, status: 'cancelled' });
      // Edit = cancel, change, schedule again: the text goes back into an
      // empty composer.
      this.restoreText(m.content, true);
    });
  }

  private sendNow(m: ScheduledMessage): Promise<void> {
    return this.runAction(m, 'Failed to send message', async () => {
      this.applyUpdate(await sendNowScheduledMessage(this.conversationKey, m.id));
    });
  }

  private dismiss(m: ScheduledMessage): Promise<void> {
    return this.runAction(m, 'Failed to dismiss message', async () => {
      await dismissScheduledMessage(this.conversationKey, m.id);
      this.applyUpdate({ ...m, status: 'cancelled' });
    });
  }

  private restoreText(text: string, onlyIfEmpty: boolean): void {
    this.dispatchEvent(
      new CustomEvent<ScheduledRestoreDetail>('chat-scheduled-restore', {
        detail: { text, onlyIfEmpty },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    if (!this.enabled || this.messages.length === 0) return nothing;
    return html`
      <div class="list" role="list" aria-label="Scheduled messages">
        ${repeat(
          this.messages,
          (m) => m.id,
          (m) => this.renderItem(m)
        )}
      </div>
    `;
  }

  private renderItem(m: ScheduledMessage) {
    return html`
      <div class="item" role="listitem" data-id=${m.id} data-status=${m.status}>
        <div class="bubble">${m.content}</div>
        ${this.renderBanner(m)}
      </div>
    `;
  }

  private renderBanner(m: ScheduledMessage) {
    const busy = this.busy.has(m.id);
    if (m.status === 'failed') {
      return html`<div class="banner failed" role="status">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <span class="when">${scheduledFailureText(m.failureReason)}</span>
        </div>
        <div class="actions">
          ${scheduledSendNowAllowed(m)
            ? html`<button
                class="action-btn send-now-btn"
                type="button"
                ?disabled=${busy}
                @click=${() => void this.sendNow(m)}
              >
                Send now
              </button>`
            : nothing}
          <button
            class="action-btn copy-btn"
            type="button"
            @click=${() => this.restoreText(m.content, false)}
          >
            Copy to composer
          </button>
          <button
            class="action-btn dismiss-btn"
            type="button"
            ?disabled=${busy}
            @click=${() => void this.dismiss(m)}
          >
            Dismiss
          </button>
        </div>`;
    }
    if (m.status === 'sending') {
      return html`<div class="banner" role="status">
        <sl-icon name="clock"></sl-icon>
        <span class="when">Sending…</span>
      </div>`;
    }
    return html`<div class="banner">
      <sl-icon name="clock"></sl-icon>
      <span class="when"
        >Scheduled for ${formatInstantWithZone(m.fireAt)} (${formatRelative(m.fireAt)})</span
      >
      <button
        class="cancel-btn"
        type="button"
        ?disabled=${busy}
        @click=${() => void this.cancel(m)}
      >
        Cancel
      </button>
    </div>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-scheduled-list': ScionChatScheduledList;
  }
}
