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
 * Browser notifications for chat mentions and DMs.
 *
 * This is the *only* place a chat browser notification is created. The
 * notification tray also fires browser notifications, but it explicitly skips
 * chat statuses (see notification-tray.ts) — otherwise a single mention would
 * pop twice: once from the SSE event here, once when the tray re-fetches in
 * response to the same event.
 *
 * Everything needed comes off the SSE payload, so no fetch is involved and
 * the popup appears at message speed.
 */

import { stateManager } from './state.js';
import { chatConversationPath } from './chat-routes.js';
import { canShowPushNotification } from './push-preference.js';

/** Notification statuses this dispatcher owns. */
export const MENTION_STATUS = 'MENTION';
export const DM_RECEIVED_STATUS = 'DM_RECEIVED';

const CHAT_STATUSES: ReadonlySet<string> = new Set([MENTION_STATUS, DM_RECEIVED_STATUS]);

/** True for the notification statuses produced by chat. */
export function isChatNotificationStatus(status: string | undefined): boolean {
  return !!status && CHAT_STATUSES.has(status);
}

/** The chat fields of a `user.<id>.notification` SSE payload. */
export interface ChatNotificationPayload {
  id?: string;
  status?: string;
  message?: string;
  projectId?: string;
  subscriberId?: string;
  senderId?: string;
  senderName?: string;
  conversationKey?: string;
  conversationName?: string;
  preview?: string;
}

/**
 * Composes the popup title from structured fields.
 *
 * Deliberately not parsed out of `message`: that string is a formatted
 * sentence whose shape is the server's business.
 */
export function chatNotificationTitle(n: ChatNotificationPayload): string {
  const sender = n.senderName?.trim() || 'Someone';
  if (n.status === DM_RECEIVED_STATUS) {
    return `${sender} sent you a message`;
  }
  const thread = n.conversationName?.trim();
  return thread ? `${sender} mentioned you in #${thread}` : `${sender} mentioned you`;
}

/**
 * The popup body: the message text alone, since the title already carries
 * sender and place. Falls back to the formatted message for events from a hub
 * old enough not to send `preview`.
 */
export function chatNotificationBody(n: ChatNotificationPayload): string {
  return n.preview?.trim() || n.message?.trim() || '';
}

/**
 * Collapsing tag. One popup per conversation: ten messages in a busy thread
 * replace each other rather than burying the desktop.
 */
export function chatNotificationTag(n: ChatNotificationPayload): string {
  return `scion-chat:${n.conversationKey ?? n.id ?? ''}`;
}

/** Reason a notification was not shown. `null` means it was shown. */
export type SuppressionReason =
  | 'not-chat'
  | 'not-for-me'
  | 'own-message'
  | 'conversation-visible'
  | 'push-disabled';

/**
 * Owns the mention/DM popups for the lifetime of the page.
 *
 * A single instance is exported below; it is started by the client entry
 * point once the signed-in user is known, because a notification addressed to
 * somebody else must never be shown, and "somebody else" is only detectable
 * with an identity to compare against.
 */
export class ChatNotificationDispatcher {
  private userId = '';
  private activeConversationKey = '';
  private listening = false;
  private readonly boundHandler = this.onNotificationEvent.bind(this);

  /** Test seam: the constructor used to build popups. */
  constructor(
    private readonly notify: (
      title: string,
      options: NotificationOptions
    ) => Notification | null = (title, options) => new window.Notification(title, options),
    private readonly navigate: (path: string) => void = (path) => {
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path }, bubbles: true, composed: true })
      );
    }
  ) {}

  /** Begins dispatching for the signed-in user. Idempotent. */
  start(userId: string): void {
    this.userId = userId;
    if (this.listening) return;
    stateManager.addEventListener('notification-created', this.boundHandler);
    this.listening = true;
  }

  stop(): void {
    if (!this.listening) return;
    stateManager.removeEventListener('notification-created', this.boundHandler);
    this.listening = false;
  }

  /**
   * Records the conversation the user is looking at, so messages arriving in
   * it do not pop a notification about something already on screen. Chat calls
   * this on every conversation change and clears it on unmount.
   */
  setActiveConversation(conversationKey: string | null | undefined): void {
    this.activeConversationKey = conversationKey ?? '';
  }

  private onNotificationEvent(e: Event): void {
    const detail = (e as CustomEvent<{ data?: unknown }>).detail;
    this.handle((detail?.data ?? {}) as ChatNotificationPayload);
  }

  /**
   * Applies the suppression rules and shows the popup.
   * Returns the reason it was suppressed, or null if it was shown.
   */
  handle(n: ChatNotificationPayload): SuppressionReason | null {
    if (!isChatNotificationStatus(n.status)) return 'not-chat';

    // The subject already scopes delivery to this user, but a stale SSE
    // connection from a previous session on the same tab would deliver
    // somebody else's notification. Cheap second gate.
    if (!this.userId || n.subscriberId !== this.userId) return 'not-for-me';

    // Your own message, echoed to your other tabs.
    if (n.senderId && n.senderId === this.userId) return 'own-message';

    // Already looking at it. Only when the tab actually has focus — a
    // background tab left open on a conversation is not "watching" it.
    if (
      n.conversationKey &&
      n.conversationKey === this.activeConversationKey &&
      document.hasFocus()
    ) {
      return 'conversation-visible';
    }

    // Checked last so the rules above are observable in tests without
    // granting notification permission.
    if (!canShowPushNotification()) return 'push-disabled';

    const path = chatConversationPath({
      conversationKey: n.conversationKey ?? '',
      ...(n.projectId ? { projectId: n.projectId } : {}),
    });

    const popup = this.notify(chatNotificationTitle(n), {
      body: chatNotificationBody(n),
      tag: chatNotificationTag(n),
      icon: '/scion-notification-icon.png',
    });

    if (popup && path) {
      popup.onclick = (): void => {
        window.focus();
        this.navigate(path);
        popup.close();
      };
    }

    return null;
  }
}

/** The page-wide dispatcher. */
export const chatNotifications = new ChatNotificationDispatcher();
