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
 * Browser notifications for chat messages.
 *
 * This is the *only* place a chat browser notification is created. The hub
 * no longer writes chat rows into the notification tray, so the tray (agent
 * events only) never pops for chat.
 *
 * A popup is raised for a new message, driven by the chat message SSE event,
 * when all of these hold:
 *
 * - it is not the user's own message;
 * - the user takes part in the conversation: a DM they are in, or a thread
 *   they are a member of (see isKnownThreadMember);
 * - the conversation is not on screen in a focused tab;
 * - the conversation is not muted;
 * - the user turned on chat message alerts (and the browser allows them).
 *
 * The chime follows the same rules except the last: it does not depend on
 * desktop permission, so a background thread is audible with popups off.
 */

import { apiFetch } from './api.js';
import { chatDMsLoad } from './chat-list-cache.js';
import { chatConversationPath } from './chat-routes.js';
import { canShowPushNotification } from './push-preference.js';
import { stateManager } from './state.js';
import { playChimeThrottled } from '../utils/audio.js';

/** The fields of a chat message SSE payload this module reads. */
export interface ChatMessagePayload {
  id?: string;
  projectId?: string;
  /** `user:<email>` or `agent:<slug>`. */
  sender?: string;
  senderId?: string;
  msg?: string;
  /** The conversation key: a thread id, or a `dm:` key. */
  threadId?: string;
  /** Set by the state manager for events on the user's own subject. */
  deliveredToUser?: boolean;
}

/** What the dispatcher knows about a conversation, looked up on demand. */
export interface ConversationInfo {
  muted: boolean;
  /** Thread name, for threads. */
  name?: string;
  /** DM peer's display name, for DMs. */
  peerName?: string;
  /** Thread creator's user id, for threads. */
  createdBy?: string;
}

/** The signed-in user, as far as the dispatcher needs to know them. */
export interface ChatNotificationIdentity {
  id: string;
  email?: string;
  name?: string;
}

/** Reason a notification was not shown. `null` means it was shown. */
export type SuppressionReason =
  | 'not-chat'
  | 'not-for-me'
  | 'own-message'
  | 'duplicate'
  | 'not-member'
  | 'conversation-visible'
  | 'muted'
  | 'push-disabled';

/** How long a looked-up conversation list is reused. */
export const CONVERSATION_INFO_MAX_AGE_MS = 15_000;

/** How many message ids are remembered to drop a repeated event. */
const SEEN_MESSAGE_LIMIT = 200;

/** Longest popup body, in characters. */
const BODY_MAX_CHARS = 200;

/** Whether a conversation key is a DM key. */
export function isDMKey(key: string): boolean {
  return key.startsWith('dm:');
}

/** The user ids a `dm:<kind>:<id>:<kind>:<id>` key names. */
export function dmUserIds(key: string): string[] {
  const parts = key.split(':');
  const ids: string[] = [];
  for (let i = 1; i + 1 < parts.length; i += 2) {
    if (parts[i] === 'user' && parts[i + 1]) ids.push(parts[i + 1]);
  }
  return ids;
}

/** A sender reference (`user:<email>`, `agent:<slug>`) as a display name. */
export function senderDisplayName(sender: string | undefined): string {
  const s = sender?.trim() ?? '';
  const colon = s.indexOf(':');
  const name = colon >= 0 ? s.slice(colon + 1) : s;
  return name.trim() || 'Someone';
}

/**
 * The names an @mention of this user can take, lowercased: the display name,
 * its hyphenated form, the email and the email's local part. Mirrors how
 * the hub resolves a mention to a thread member.
 */
export function mentionNamesFor(identity: ChatNotificationIdentity): string[] {
  const names = new Set<string>();
  const name = identity.name?.trim().toLowerCase();
  if (name) {
    names.add(name);
    names.add(name.split(' ').join('-'));
  }
  const email = identity.email?.trim().toLowerCase();
  if (email) {
    names.add(email);
    const at = email.indexOf('@');
    if (at > 0) names.add(email.slice(0, at));
  }
  return [...names];
}

/** Whether a mention name may continue with this character. */
function isNameChar(ch: string): boolean {
  return /[\w.@+-]/.test(ch);
}

/** Whether `text` @mentions any of `names` (lowercased). */
export function mentionsAny(text: string | undefined, names: readonly string[]): boolean {
  if (!text || names.length === 0) return false;
  const lower = text.toLowerCase();
  for (const name of names) {
    const needle = `@${name}`;
    let from = 0;
    for (;;) {
      const at = lower.indexOf(needle, from);
      if (at < 0) break;
      const next = lower.charAt(at + needle.length);
      // A trailing full stop ends a sentence rather than the name.
      const nextNext = lower.charAt(at + needle.length + 1);
      if (!next || !isNameChar(next) || (next === '.' && !(nextNext && isNameChar(nextNext)))) {
        return true;
      }
      from = at + 1;
    }
  }
  return false;
}

/** The popup title. */
export function chatMessageTitle(n: ChatMessagePayload, info: ConversationInfo | null): string {
  const key = n.threadId ?? '';
  if (isDMKey(key)) {
    const sender = info?.peerName?.trim() || senderDisplayName(n.sender);
    return `${sender} sent you a message`;
  }
  const sender = senderDisplayName(n.sender);
  const thread = info?.name?.trim();
  return thread ? `${sender} in #${thread}` : `${sender} posted in a thread`;
}

/** The popup body: the message text, shortened. */
export function chatMessageBody(n: ChatMessagePayload): string {
  const text = (n.msg ?? '').trim();
  return text.length > BODY_MAX_CHARS ? `${text.slice(0, BODY_MAX_CHARS - 1)}…` : text;
}

/**
 * Collapsing tag. One popup per conversation: ten messages in a busy thread
 * replace each other rather than burying the desktop.
 */
export function chatMessageTag(n: ChatMessagePayload): string {
  return `scion-chat:${n.threadId ?? n.id ?? ''}`;
}

interface CachedList<T> {
  at: number;
  promise: Promise<T>;
}

/** Thread list entry fields this module reads. */
interface ThreadEntry {
  id?: string;
  name?: string;
  muted?: boolean;
  createdBy?: string;
}

/** DM list entry fields this module reads. */
interface DMEntry {
  conversationKey?: string;
  muted?: boolean;
  peerName?: string;
}

/**
 * Looks up conversation info from the thread and DM lists, reusing a list
 * for CONVERSATION_INFO_MAX_AGE_MS. A failed lookup is null: the popup still
 * shows, without a thread name, rather than staying silent on a network blip.
 */
export async function lookupConversationInfo(
  n: ChatMessagePayload,
  threadCache: Map<string, CachedList<ThreadEntry[] | null>>
): Promise<ConversationInfo | null> {
  const key = n.threadId ?? '';
  if (isDMKey(key)) {
    const data = await chatDMsLoad.load({ maxAgeMs: CONVERSATION_INFO_MAX_AGE_MS });
    const dm = ((data?.dms ?? []) as DMEntry[]).find((d) => d.conversationKey === key);
    if (!dm) return null;
    return { muted: !!dm.muted, ...(dm.peerName ? { peerName: dm.peerName } : {}) };
  }
  const projectId = n.projectId ?? '';
  if (!projectId) return null;
  const now = Date.now();
  let cached = threadCache.get(projectId);
  if (!cached || now - cached.at > CONVERSATION_INFO_MAX_AGE_MS) {
    cached = { at: now, promise: fetchThreads(projectId) };
    threadCache.set(projectId, cached);
  }
  const threads = await cached.promise;
  const t = threads?.find((x) => x.id === key);
  if (!t) return null;
  return {
    muted: !!t.muted,
    ...(t.name ? { name: t.name } : {}),
    ...(t.createdBy ? { createdBy: t.createdBy } : {}),
  };
}

async function fetchThreads(projectId: string): Promise<ThreadEntry[] | null> {
  try {
    const res = await apiFetch(`/api/v1/chat/spaces/${encodeURIComponent(projectId)}/threads`);
    if (!res.ok) return null;
    const body = (await res.json()) as { threads?: ThreadEntry[] } | null;
    return body?.threads ?? [];
  } catch {
    return null;
  }
}

/**
 * Owns the chat message popups for the lifetime of the page.
 *
 * A single instance is exported below; it is started by the client entry
 * point once the signed-in user is known, because "your own message" and
 * "a DM you are in" are only decidable with an identity to compare against.
 */
export class ChatNotificationDispatcher {
  private identity: ChatNotificationIdentity | null = null;
  private mentionNames: string[] = [];
  private activeConversationKey = '';
  private listening = false;
  /** Threads this page has seen the user post in or be mentioned in. */
  private readonly memberThreads = new Set<string>();
  private readonly seenMessages = new Set<string>();
  private readonly threadCache = new Map<string, CachedList<ThreadEntry[] | null>>();
  private readonly boundHandler = (e: Event): void => {
    const detail = (e as CustomEvent<{ data?: unknown }>).detail;
    void this.handle((detail?.data ?? {}) as ChatMessagePayload);
  };

  /** Test seams: the popup constructor, navigation and the info lookup. */
  constructor(
    private readonly notify: (
      title: string,
      options: NotificationOptions
    ) => Notification | null = (title, options) => new window.Notification(title, options),
    private readonly navigate: (path: string) => void = (path) => {
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path }, bubbles: true, composed: true })
      );
    },
    private readonly lookup?: (n: ChatMessagePayload) => Promise<ConversationInfo | null>
  ) {}

  /** Begins dispatching for the signed-in user. Idempotent. */
  start(identity: ChatNotificationIdentity | string): void {
    const next = typeof identity === 'string' ? { id: identity } : identity;
    if (this.identity?.id !== next.id) {
      this.memberThreads.clear();
      this.seenMessages.clear();
      this.threadCache.clear();
    }
    this.identity = next;
    this.mentionNames = mentionNamesFor(next);
    if (this.listening) return;
    stateManager.addEventListener('chat-message-received', this.boundHandler);
    this.listening = true;
  }

  stop(): void {
    if (!this.listening) return;
    stateManager.removeEventListener('chat-message-received', this.boundHandler);
    this.listening = false;
  }

  /**
   * Drops looked-up conversation info, so a mute takes effect on the next
   * message rather than when the cached lists expire. Chat calls this after
   * the user mutes or unmutes a conversation.
   */
  invalidateConversationInfo(): void {
    this.threadCache.clear();
    chatDMsLoad.invalidate();
  }

  /**
   * Records the conversation the user is looking at, so messages arriving in
   * it do not pop a notification about something already on screen. Chat calls
   * this on every conversation change and clears it on unmount.
   */
  setActiveConversation(conversationKey: string | null | undefined): void {
    this.activeConversationKey = conversationKey ?? '';
  }

  /**
   * Whether the page already knows the user is a member of a thread,
   * without fetching anything.
   *
   * The thread message event does not say who the thread's members are, so
   * this takes the hub's membership rules where the page can see them: the
   * user posted in the thread (seen on this page) or is @mentioned in it.
   * An event on the user's own subject is addressed to them, and so counts
   * as membership by itself. The remaining rule, the user created the
   * thread, needs the thread list (isThreadCreator).
   *
   * This is an approximation. Once the hub fans thread messages out to
   * member participants on `user.<id>.chat.message`, every member thread
   * message arrives with `deliveredToUser` set and the check is exact.
   */
  private isKnownThreadMember(n: ChatMessagePayload): boolean {
    const key = n.threadId ?? '';
    if (n.deliveredToUser) return true;
    if (this.memberThreads.has(key)) return true;
    if (mentionsAny(n.msg, this.mentionNames)) {
      this.memberThreads.add(key);
      return true;
    }
    return false;
  }

  /** The creator half of the membership rule, which needs the thread list. */
  private isThreadCreator(info: ConversationInfo | null): boolean {
    return !!info?.createdBy && info.createdBy === this.identity?.id;
  }

  /**
   * Records a message that passed every gate, so a second copy of it (the
   * same message on another subject) is dropped. Only messages that passed
   * are recorded: a copy rejected as "not a member" must not stop the
   * user-subject copy, which proves membership, from being shown.
   */
  private rememberMessage(id: string | undefined): void {
    if (!id) return;
    this.seenMessages.add(id);
    if (this.seenMessages.size > SEEN_MESSAGE_LIMIT) {
      // Sets iterate in insertion order: drop the oldest id.
      for (const oldest of this.seenMessages) {
        this.seenMessages.delete(oldest);
        break;
      }
    }
  }

  /**
   * Applies the rules and shows the popup.
   * Resolves to the reason it was suppressed, or null if it was shown.
   */
  async handle(n: ChatMessagePayload): Promise<SuppressionReason | null> {
    const key = n.threadId?.trim() ?? '';
    // Agent-internal channels are not chat conversations.
    if (!key || key.startsWith('agent:')) return 'not-chat';

    const me = this.identity?.id ?? '';
    if (!me) return 'not-for-me';
    const isDM = isDMKey(key);
    // A stale SSE connection from a previous session on the same tab could
    // deliver another user's DM. Cheap second gate.
    if (isDM && !dmUserIds(key).includes(me)) return 'not-for-me';

    // Your own message, echoed to your tabs. Posting makes you a member.
    if (n.senderId && n.senderId === me) {
      if (!isDM) this.memberThreads.add(key);
      return 'own-message';
    }

    // The same message can arrive on more than one subject; only a copy
    // that passed every gate is remembered (see rememberMessage).
    if (n.id && this.seenMessages.has(n.id)) return 'duplicate';

    // Already looking at it. Only when the tab actually has focus — a
    // background tab left open on a conversation is not "watching" it.
    if (key === this.activeConversationKey && document.hasFocus()) {
      return 'conversation-visible';
    }

    // Membership the page can decide on its own comes first. The thread
    // list is fetched only for what is left: the creator rule, and mute.
    const knownMember = isDM || this.isKnownThreadMember(n);
    const info = this.lookup
      ? await this.lookup(n)
      : await lookupConversationInfo(n, this.threadCache);
    if (!knownMember && !this.isThreadCreator(info)) return 'not-member';
    if (info?.muted) return 'muted';

    // Another copy may have passed while the lookup was in flight.
    if (n.id && this.seenMessages.has(n.id)) return 'duplicate';
    this.rememberMessage(n.id);

    // The chime is independent of desktop push permission/opt-in.
    playChimeThrottled(n.projectId ?? '');

    // Checked last so the rules above are observable in tests without
    // granting notification permission.
    if (!canShowPushNotification('chat')) return 'push-disabled';

    const path = chatConversationPath({
      conversationKey: key,
      ...(n.projectId ? { projectId: n.projectId } : {}),
    });

    const popup = this.notify(chatMessageTitle(n, info), {
      body: chatMessageBody(n),
      tag: chatMessageTag(n),
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
