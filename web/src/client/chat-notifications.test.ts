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
 * Tests for chat browser notifications.
 *
 * The suppression rules are the whole feature: a notification system that
 * pops for your own messages, for the conversation you are reading, or for
 * somebody else's mention is worse than none. Each rule gets a test that
 * fails when the rule is removed.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

import {
  ChatNotificationDispatcher,
  chatNotificationTitle,
  chatNotificationBody,
  chatNotificationTag,
  isChatNotificationStatus,
  type ChatNotificationPayload,
} from './chat-notifications.js';
import { CHAT_DM_ROUTE, CHAT_THREAD_ROUTE } from './chat-routes.js';
import { PUSH_STORAGE_KEY } from './push-preference.js';
import { stateManager } from './state.js';

const ME = 'user-me';
const THEM = 'user-them';

/** Popups created during a test. */
let popups: FakeNotification[] = [];

class FakeNotification {
  static permission: NotificationPermission = 'granted';
  static requestPermission = vi.fn(async () => FakeNotification.permission);

  onclick: (() => void) | null = null;
  close = vi.fn();

  constructor(
    public title: string,
    public options: NotificationOptions = {}
  ) {
    popups.push(this);
  }
}

/** A mention addressed to ME, in a project thread. */
function mention(overrides: Partial<ChatNotificationPayload> = {}): ChatNotificationPayload {
  return {
    id: 'notif-1',
    status: 'MENTION',
    message: '@Ada mentioned you in #design-review: ship it',
    projectId: 'proj-1',
    subscriberId: ME,
    senderId: THEM,
    senderName: 'Ada',
    conversationKey: 'topic-1',
    conversationName: 'design-review',
    preview: 'ship it',
    ...overrides,
  };
}

/** A DM addressed to ME. */
function dm(overrides: Partial<ChatNotificationPayload> = {}): ChatNotificationPayload {
  return {
    id: 'notif-2',
    status: 'DM_RECEIVED',
    message: 'Ada sent you a message: lunch?',
    subscriberId: ME,
    senderId: THEM,
    senderName: 'Ada',
    conversationKey: `dm:user:${THEM}:user:${ME}`,
    preview: 'lunch?',
    ...overrides,
  };
}

/** Dispatchers started during a test, torn down afterwards. */
let started: ChatNotificationDispatcher[] = [];

/**
 * A started dispatcher using the real Notification constructor.
 *
 * stateManager is a page-wide singleton, so a dispatcher left listening would
 * keep answering events raised by later tests.
 */
function dispatcher(): ChatNotificationDispatcher {
  const d = new ChatNotificationDispatcher();
  d.start(ME);
  started.push(d);
  return d;
}

beforeEach(() => {
  popups = [];
  started = [];
  FakeNotification.permission = 'granted';
  (window as unknown as { Notification: unknown }).Notification = FakeNotification;
  localStorage.setItem(PUSH_STORAGE_KEY, 'true');
  // happy-dom reports the document as focused; make it explicit so the
  // "actively viewing" rule is exercised under a known condition.
  vi.spyOn(document, 'hasFocus').mockReturnValue(true);
});

afterEach(() => {
  for (const d of started) d.stop();
  started = [];
  vi.restoreAllMocks();
  localStorage.clear();
});

describe('chat notification content', () => {
  it('composes the title from structured fields, not by parsing the message', () => {
    expect(chatNotificationTitle(mention())).toBe('Ada mentioned you in #design-review');
    expect(chatNotificationTitle(dm())).toBe('Ada sent you a message');
  });

  it('titles a mention without a thread name without a stray #', () => {
    const title = chatNotificationTitle(mention({ conversationName: '' }));
    expect(title).toBe('Ada mentioned you');
    expect(title).not.toContain('#');
  });

  it('uses the preview as the body, falling back to the formatted message', () => {
    expect(chatNotificationBody(mention())).toBe('ship it');
    // A hub too old to send `preview` must still produce a readable popup.
    const legacy = mention({ preview: undefined });
    expect(chatNotificationBody(legacy)).toBe(legacy.message);
  });

  it('tags per conversation so a busy thread collapses to one popup', () => {
    const first = chatNotificationTag(mention({ id: 'notif-a' }));
    const second = chatNotificationTag(mention({ id: 'notif-b' }));
    expect(first).toBe(second);
    expect(chatNotificationTag(dm())).not.toBe(first);
  });

  it('claims only the chat statuses', () => {
    expect(isChatNotificationStatus('MENTION')).toBe(true);
    expect(isChatNotificationStatus('DM_RECEIVED')).toBe(true);
    expect(isChatNotificationStatus('COMPLETED')).toBe(false);
    expect(isChatNotificationStatus(undefined)).toBe(false);
  });
});

describe('chat notification dispatch', () => {
  it('shows a popup for a mention addressed to me', () => {
    expect(dispatcher().handle(mention())).toBeNull();

    expect(popups).toHaveLength(1);
    expect(popups[0].title).toBe(chatNotificationTitle(mention()));
    expect(popups[0].options.body).toBe('ship it');
    expect(popups[0].options.tag).toBe(chatNotificationTag(mention()));
  });

  it('dispatches from the SSE event the state manager raises', () => {
    dispatcher();

    stateManager.dispatchEvent(
      new CustomEvent('notification-created', { detail: { data: dm() } })
    );

    expect(popups).toHaveLength(1);
    expect(popups[0].title).toBe('Ada sent you a message');
  });

  it('ignores agent-status notifications — the tray owns those', () => {
    const status = { id: 'n', status: 'COMPLETED', subscriberId: ME, message: 'done' };
    expect(dispatcher().handle(status)).toBe('not-chat');
    expect(popups).toHaveLength(0);
  });

  it('ignores a notification addressed to another user', () => {
    expect(dispatcher().handle(mention({ subscriberId: THEM }))).toBe('not-for-me');
    expect(popups).toHaveLength(0);
  });

  it('ignores my own message echoed back to this tab', () => {
    expect(dispatcher().handle(dm({ senderId: ME }))).toBe('own-message');
    expect(popups).toHaveLength(0);
  });

  it('stays quiet for the conversation on screen', () => {
    const d = dispatcher();
    d.setActiveConversation('topic-1');
    expect(d.handle(mention({ conversationKey: 'topic-1' }))).toBe('conversation-visible');
    expect(popups).toHaveLength(0);

    // A different conversation is not on screen, so it still pops.
    expect(d.handle(mention({ conversationKey: 'topic-2' }))).toBeNull();
    expect(popups).toHaveLength(1);
  });

  it('still notifies for the open conversation when the tab is not focused', () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);
    const d = dispatcher();
    d.setActiveConversation('topic-1');

    expect(d.handle(mention({ conversationKey: 'topic-1' }))).toBeNull();
    expect(popups).toHaveLength(1);
  });

  it('resumes notifying after leaving the conversation', () => {
    const d = dispatcher();
    d.setActiveConversation('topic-1');
    d.setActiveConversation(null);

    expect(d.handle(mention({ conversationKey: 'topic-1' }))).toBeNull();
    expect(popups).toHaveLength(1);
  });

  it('does nothing until a user is known', () => {
    const d = new ChatNotificationDispatcher();
    expect(d.handle(mention())).toBe('not-for-me');
    expect(popups).toHaveLength(0);
  });

  it('respects the master toggle', () => {
    localStorage.setItem(PUSH_STORAGE_KEY, 'false');
    expect(dispatcher().handle(mention())).toBe('push-disabled');
    expect(popups).toHaveLength(0);
  });

  it('respects a denied browser permission', () => {
    FakeNotification.permission = 'denied';
    expect(dispatcher().handle(mention())).toBe('push-disabled');
    expect(popups).toHaveLength(0);
  });

  it('does not throw when the browser has no Notification API', () => {
    delete (window as unknown as { Notification?: unknown }).Notification;
    expect(() => dispatcher().handle(mention())).not.toThrow();
    expect(popups).toHaveLength(0);
  });

  it('stops dispatching after stop()', () => {
    const d = dispatcher();
    d.stop();

    stateManager.dispatchEvent(
      new CustomEvent('notification-created', { detail: { data: mention() } })
    );

    expect(popups).toHaveLength(0);
  });
});

describe('click-to-navigate', () => {
  /** Captures the path the router would receive from a popup click. */
  function clickAndCapturePath(payload: ChatNotificationPayload): string | undefined {
    let path: string | undefined;
    const listener = (e: Event): void => {
      path = (e as CustomEvent<{ path: string }>).detail.path;
    };
    // The same listener the router installs in main.ts setupRouter().
    document.addEventListener('nav-click', listener);
    try {
      dispatcher().handle(payload);
      popups[0]?.onclick?.();
    } finally {
      document.removeEventListener('nav-click', listener);
    }
    return path;
  }

  it('routes a thread mention to a path the router actually matches', () => {
    const path = clickAndCapturePath(mention());

    expect(path).toBeDefined();
    // CHAT_THREAD_ROUTE is the regex registered in the router's route table,
    // imported rather than retyped — a path that satisfies it is a path the
    // router will resolve to the chat page.
    expect(CHAT_THREAD_ROUTE.test(path as string)).toBe(true);
    expect(path).toContain('proj-1');
    expect(path).toContain('topic-1');
  });

  it('routes a DM to a path the router actually matches', () => {
    const payload = dm();
    const path = clickAndCapturePath(payload);

    expect(path).toBeDefined();
    expect(CHAT_DM_ROUTE.test(path as string)).toBe(true);
    // The colons in the DM key are escaped, so the key stays one path segment.
    expect(decodeURIComponent((path as string).replace('/chat/dm/', ''))).toBe(
      payload.conversationKey
    );
  });

  it('focuses the tab and closes the popup on click', () => {
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});
    dispatcher().handle(mention());
    popups[0].onclick?.();

    expect(focus).toHaveBeenCalled();
    expect(popups[0].close).toHaveBeenCalled();
  });

  it('shows a mention with no project without a click target rather than a broken one', () => {
    // A thread key with no project cannot be addressed; the popup is still
    // worth showing, but it must not navigate somewhere that 404s.
    const path = clickAndCapturePath(mention({ projectId: '' }));

    expect(popups).toHaveLength(1);
    expect(path).toBeUndefined();
  });
});
