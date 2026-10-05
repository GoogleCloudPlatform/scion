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
 * Tests for chat message popups.
 *
 * The popup is the only chat alert the hub no longer backs with a tray row,
 * and a popup that fires for your own messages, for a conversation you are
 * not in, for a muted one, or for the one you are reading is worse than
 * none. Each rule gets a test that fails when the rule is removed.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const { playChimeThrottled } = vi.hoisted(() => ({ playChimeThrottled: vi.fn() }));
vi.mock('../utils/audio.js', () => ({ playChimeThrottled }));

const { apiFetch } = vi.hoisted(() => ({ apiFetch: vi.fn() }));
vi.mock('./api.js', () => ({ apiFetch }));

import {
  ChatNotificationDispatcher,
  chatMessageBody,
  chatMessageTag,
  chatMessageTitle,
  dmUserIds,
  lookupConversationInfo,
  mentionNamesFor,
  mentionsAny,
  senderDisplayName,
  type ChatMessagePayload,
  type ConversationInfo,
} from './chat-notifications.js';
import { chatDMsLoad } from './chat-list-cache.js';
import { CHAT_DM_ROUTE, CHAT_THREAD_ROUTE } from './chat-routes.js';
import { PUSH_STORAGE_KEYS } from './push-preference.js';
import { stateManager } from './state.js';

const ME = 'user-me';
const THEM = 'user-them';
const DM_KEY = `dm:user:${THEM}:user:${ME}`;

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

let nextId = 0;

/** A DM from THEM to ME. */
function dm(overrides: Partial<ChatMessagePayload> = {}): ChatMessagePayload {
  return {
    id: `m-${++nextId}`,
    sender: 'user:ada@example.com',
    senderId: THEM,
    msg: 'lunch?',
    threadId: DM_KEY,
    deliveredToUser: true,
    ...overrides,
  };
}

/** A message from THEM in a project thread. */
function thread(overrides: Partial<ChatMessagePayload> = {}): ChatMessagePayload {
  return {
    id: `m-${++nextId}`,
    projectId: 'proj-1',
    sender: 'user:ada@example.com',
    senderId: THEM,
    msg: 'ship it',
    threadId: 'topic-1',
    ...overrides,
  };
}

/** What the injected lookup answers, per conversation key. */
let infos: Record<string, ConversationInfo | null> = {};

/** Dispatchers started during a test, torn down afterwards. */
let started: ChatNotificationDispatcher[] = [];

/**
 * A started dispatcher using the real Notification constructor and a lookup
 * answered from `infos`.
 *
 * stateManager is a page-wide singleton, so a dispatcher left listening would
 * keep answering events raised by later tests.
 */
function dispatcher(): ChatNotificationDispatcher {
  const d = new ChatNotificationDispatcher(undefined, undefined, (n) =>
    Promise.resolve(infos[n.threadId ?? ''] ?? null)
  );
  d.start({ id: ME, email: 'me@example.com', name: 'Grace Hopper' });
  started.push(d);
  return d;
}

beforeEach(() => {
  popups = [];
  started = [];
  infos = {
    [DM_KEY]: { muted: false, peerName: 'Ada' },
    'topic-1': { muted: false, name: 'design-review', createdBy: THEM },
  };
  playChimeThrottled.mockClear();
  apiFetch.mockReset();
  FakeNotification.permission = 'granted';
  (window as unknown as { Notification: unknown }).Notification = FakeNotification;
  localStorage.setItem(PUSH_STORAGE_KEYS.chat, 'true');
  // happy-dom reports the document as focused; make it explicit so the
  // "actively viewing" rule is exercised under a known condition.
  vi.spyOn(document, 'hasFocus').mockReturnValue(true);
});

afterEach(() => {
  for (const d of started) d.stop();
  started = [];
  vi.restoreAllMocks();
  localStorage.clear();
  chatDMsLoad.invalidate();
});

describe('chat message content', () => {
  it('titles a DM with the peer name, falling back to the sender', () => {
    expect(chatMessageTitle(dm(), { muted: false, peerName: 'Ada L' })).toBe(
      'Ada L sent you a message'
    );
    expect(chatMessageTitle(dm(), null)).toBe('ada@example.com sent you a message');
  });

  it('titles a thread message with the thread name, without a stray #', () => {
    expect(chatMessageTitle(thread(), { muted: false, name: 'design' })).toBe(
      'ada@example.com in #design'
    );
    expect(chatMessageTitle(thread({ sender: 'agent:helper' }), null)).toBe(
      'helper posted in a thread'
    );
  });

  it('uses the message as the body, shortened', () => {
    expect(chatMessageBody(thread({ msg: '  hi  ' }))).toBe('hi');
    const long = chatMessageBody(thread({ msg: 'x'.repeat(500) }));
    expect(long.length).toBe(200);
    expect(long.endsWith('…')).toBe(true);
  });

  it('tags per conversation so a busy thread collapses to one popup', () => {
    expect(chatMessageTag(thread({ id: 'a' }))).toBe(chatMessageTag(thread({ id: 'b' })));
    expect(chatMessageTag(thread())).not.toBe(chatMessageTag(dm()));
  });

  it('reads the user ids out of a DM key', () => {
    expect(dmUserIds(`dm:user:a:user:b`)).toEqual(['a', 'b']);
    expect(dmUserIds(`dm:agent:x:user:b`)).toEqual(['b']);
  });

  it('turns sender references into names', () => {
    expect(senderDisplayName('user:ada@example.com')).toBe('ada@example.com');
    expect(senderDisplayName('agent:helper')).toBe('helper');
    expect(senderDisplayName('')).toBe('Someone');
  });
});

describe('mention matching', () => {
  const names = mentionNamesFor({ id: ME, email: 'grace@example.com', name: 'Grace Hopper' });

  it('matches every form the hub resolves', () => {
    expect(names).toEqual(
      expect.arrayContaining(['grace hopper', 'grace-hopper', 'grace@example.com', 'grace'])
    );
    expect(mentionsAny('hey @grace-hopper look', names)).toBe(true);
    expect(mentionsAny('@Grace, look', names)).toBe(true);
    expect(mentionsAny('ask @grace.', names)).toBe(true);
    expect(mentionsAny('cc @grace@example.com', names)).toBe(true);
  });

  it('does not match a longer name or a bare word', () => {
    expect(mentionsAny('@graceful idea', names)).toBe(false);
    expect(mentionsAny('@grace.smith', names)).toBe(false);
    expect(mentionsAny('grace said', names)).toBe(false);
    expect(mentionsAny(undefined, names)).toBe(false);
  });
});

describe('chat message dispatch', () => {
  it('shows a popup for a DM from somebody else', async () => {
    expect(await dispatcher().handle(dm())).toBeNull();
    expect(popups).toHaveLength(1);
    expect(popups[0].title).toBe('Ada sent you a message');
    expect(popups[0].options.body).toBe('lunch?');
    expect(popups[0].options.tag).toBe(`scion-chat:${DM_KEY}`);
  });

  it('dispatches from the chat message event the state manager raises', async () => {
    dispatcher();
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', { detail: { data: dm() } })
    );
    await vi.waitFor(() => expect(popups).toHaveLength(1));
  });

  it('ignores a DM between two other users', async () => {
    const reason = await dispatcher().handle(dm({ threadId: 'dm:user:a:user:b' }));
    expect(reason).toBe('not-for-me');
    expect(popups).toHaveLength(0);
  });

  it('ignores my own message echoed back to this tab', async () => {
    expect(await dispatcher().handle(dm({ senderId: ME }))).toBe('own-message');
    expect(popups).toHaveLength(0);
  });

  it('ignores a message that is not in a chat conversation', async () => {
    const d = dispatcher();
    expect(await d.handle(dm({ threadId: '' }))).toBe('not-chat');
    expect(await d.handle(dm({ threadId: 'agent:abc' }))).toBe('not-chat');
  });

  it('shows one popup for a message delivered twice', async () => {
    const d = dispatcher();
    const m = dm();
    expect(await d.handle(m)).toBeNull();
    expect(await d.handle(m)).toBe('duplicate');
    expect(popups).toHaveLength(1);
  });

  it('stays quiet for a muted conversation', async () => {
    infos[DM_KEY] = { muted: true };
    infos['topic-1'] = { muted: true, createdBy: ME };
    const d = dispatcher();
    expect(await d.handle(dm())).toBe('muted');
    expect(await d.handle(thread())).toBe('muted');
    expect(popups).toHaveLength(0);
  });

  it('stays quiet for the conversation on screen', async () => {
    const d = dispatcher();
    d.setActiveConversation(DM_KEY);
    expect(await d.handle(dm())).toBe('conversation-visible');
    expect(popups).toHaveLength(0);
  });

  it('still notifies for the open conversation when the tab is not focused', async () => {
    vi.spyOn(document, 'hasFocus').mockReturnValue(false);
    const d = dispatcher();
    d.setActiveConversation(DM_KEY);
    expect(await d.handle(dm())).toBeNull();
  });

  it('resumes notifying after leaving the conversation', async () => {
    const d = dispatcher();
    d.setActiveConversation(DM_KEY);
    d.setActiveConversation(null);
    expect(await d.handle(dm())).toBeNull();
  });

  it('does nothing until a user is known', async () => {
    const d = new ChatNotificationDispatcher();
    expect(await d.handle(dm())).toBe('not-for-me');
  });

  it('respects the chat messages toggle', async () => {
    localStorage.setItem(PUSH_STORAGE_KEYS.chat, 'false');
    // The agent toggle does not stand in for it.
    localStorage.setItem(PUSH_STORAGE_KEYS.agent, 'true');
    expect(await dispatcher().handle(dm())).toBe('push-disabled');
    expect(popups).toHaveLength(0);
  });

  it('respects a denied browser permission', async () => {
    FakeNotification.permission = 'denied';
    expect(await dispatcher().handle(dm())).toBe('push-disabled');
  });

  it('does not throw when the browser has no Notification API', async () => {
    delete (window as unknown as { Notification?: unknown }).Notification;
    expect(await dispatcher().handle(dm())).toBe('push-disabled');
  });

  it('stops dispatching after stop()', async () => {
    const d = dispatcher();
    d.stop();
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', { detail: { data: dm() } })
    );
    await new Promise((r) => setTimeout(r, 0));
    expect(popups).toHaveLength(0);
  });
});

describe('thread membership', () => {
  it('stays quiet for a thread the user is not a member of', async () => {
    expect(await dispatcher().handle(thread())).toBe('not-member');
    expect(popups).toHaveLength(0);
  });

  it('notifies for a thread the user created', async () => {
    infos['topic-1'] = { muted: false, name: 'design-review', createdBy: ME };
    expect(await dispatcher().handle(thread())).toBeNull();
    expect(popups[0].title).toBe('ada@example.com in #design-review');
  });

  it('notifies once the user has posted in the thread', async () => {
    const d = dispatcher();
    expect(await d.handle(thread({ senderId: ME }))).toBe('own-message');
    expect(await d.handle(thread())).toBeNull();
  });

  it('notifies when the message mentions the user, and for later messages too', async () => {
    const d = dispatcher();
    expect(await d.handle(thread({ msg: '@grace-hopper take a look' }))).toBeNull();
    expect(await d.handle(thread({ msg: 'and this' }))).toBeNull();
    expect(popups).toHaveLength(2);
  });

  it('treats a thread message on the user subject as addressed to them', async () => {
    expect(await dispatcher().handle(thread({ deliveredToUser: true }))).toBeNull();
  });

  it('forgets membership when another user signs in', async () => {
    const d = dispatcher();
    await d.handle(thread({ senderId: ME }));
    d.start({ id: 'someone-else' });
    expect(await d.handle(thread())).toBe('not-member');
  });
});

describe('dedupe order and lookups', () => {
  /** A started dispatcher whose lookup is a spy answered from `infos`. */
  function spied(): {
    d: ChatNotificationDispatcher;
    lookup: ReturnType<typeof vi.fn>;
  } {
    const lookup = vi.fn((n: ChatMessagePayload) =>
      Promise.resolve(infos[n.threadId ?? ''] ?? null)
    );
    const d = new ChatNotificationDispatcher(undefined, undefined, lookup);
    d.start({ id: ME, email: 'me@example.com', name: 'Grace Hopper' });
    started.push(d);
    return { d, lookup };
  }

  it('a not-member copy does not stop the user-subject copy of the same message', async () => {
    const { d } = spied();
    const projectCopy = thread({ id: 'same' });
    expect(await d.handle(projectCopy)).toBe('not-member');
    expect(await d.handle({ ...projectCopy, deliveredToUser: true })).toBeNull();
    expect(popups).toHaveLength(1);
    // A third copy is now a duplicate.
    expect(await d.handle({ ...projectCopy, deliveredToUser: true })).toBe('duplicate');
  });

  it('a muted or visible copy is not remembered either', async () => {
    const { d } = spied();
    d.setActiveConversation(DM_KEY);
    expect(await d.handle(dm({ id: 'v' }))).toBe('conversation-visible');
    d.setActiveConversation(null);
    expect(await d.handle(dm({ id: 'v' }))).toBeNull();
  });

  it('shows one popup when two copies pass while both are looking up', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const d = new ChatNotificationDispatcher(undefined, undefined, async (n) => {
      await gate;
      return infos[n.threadId ?? ''] ?? null;
    });
    d.start(ME);
    started.push(d);
    const first = d.handle(dm({ id: 'race' }));
    const second = d.handle(dm({ id: 'race', deliveredToUser: true }));
    release();
    expect((await Promise.all([first, second])).sort()).toEqual(['duplicate', null].sort());
    expect(popups).toHaveLength(1);
  });

  it('looks nothing up when an earlier gate decides', async () => {
    const { d, lookup } = spied();
    expect(await d.handle(dm({ threadId: '' }))).toBe('not-chat');
    expect(await d.handle(dm({ threadId: 'dm:user:a:user:b' }))).toBe('not-for-me');
    expect(await d.handle(thread({ senderId: ME }))).toBe('own-message');
    d.setActiveConversation(DM_KEY);
    expect(await d.handle(dm())).toBe('conversation-visible');
    d.setActiveConversation(null);
    const shown = dm();
    expect(await d.handle(shown)).toBeNull();
    lookup.mockClear();
    expect(await d.handle(shown)).toBe('duplicate');
    expect(lookup).not.toHaveBeenCalled();
  });

  it('looks up once for a known member, for the mute check', async () => {
    const { d, lookup } = spied();
    expect(await d.handle(thread({ deliveredToUser: true }))).toBeNull();
    expect(lookup).toHaveBeenCalledTimes(1);
  });
});

describe('identity change', () => {
  it('forgets shown messages, so the next user is told about them', async () => {
    const d = dispatcher();
    const m = thread({ id: 'shared', deliveredToUser: true });
    expect(await d.handle(m)).toBeNull();
    d.start({ id: 'user-other' });
    expect(await d.handle(m)).toBeNull();
    expect(popups).toHaveLength(2);
  });

  it('keeps shown messages when the same user starts again', async () => {
    const d = dispatcher();
    const m = thread({ id: 'kept', deliveredToUser: true });
    await d.handle(m);
    d.start({ id: ME, email: 'me@example.com', name: 'Grace Hopper' });
    expect(await d.handle(m)).toBe('duplicate');
  });

  it('drops the cached thread lists', async () => {
    apiFetch.mockImplementation(() =>
      Promise.resolve(
        new Response(JSON.stringify({ threads: [{ id: 'topic-1', createdBy: ME }] }), {
          status: 200,
        })
      )
    );
    const d = new ChatNotificationDispatcher();
    d.start(ME);
    started.push(d);
    await d.handle(thread());
    expect(apiFetch).toHaveBeenCalledTimes(1);
    d.start('user-other');
    await d.handle(thread());
    expect(apiFetch).toHaveBeenCalledTimes(2);
  });
});

describe('background chime', () => {
  it('chimes for a message that would otherwise pop a notification', async () => {
    await dispatcher().handle(thread({ deliveredToUser: true }));
    expect(playChimeThrottled).toHaveBeenCalledWith('proj-1');
  });

  it('does not chime for my own message, a muted one, or one not for me', async () => {
    infos[DM_KEY] = { muted: true };
    const d = dispatcher();
    await d.handle(dm({ senderId: ME }));
    await d.handle(dm());
    await d.handle(thread());
    expect(playChimeThrottled).not.toHaveBeenCalled();
  });

  it('does not chime for the conversation already on screen', async () => {
    const d = dispatcher();
    d.setActiveConversation(DM_KEY);
    await d.handle(dm());
    expect(playChimeThrottled).not.toHaveBeenCalled();
  });

  it('chimes even when chat alerts are off — the two are independent', async () => {
    localStorage.setItem(PUSH_STORAGE_KEYS.chat, 'false');
    await dispatcher().handle(dm());
    expect(playChimeThrottled).toHaveBeenCalledTimes(1);
  });
});

describe('click-to-navigate', () => {
  /** Captures the path the router would receive from a popup click. */
  async function clickAndCapturePath(payload: ChatMessagePayload): Promise<string | undefined> {
    let path: string | undefined;
    const listener = (e: Event): void => {
      path = (e as CustomEvent<{ path: string }>).detail.path;
    };
    // The same listener the router installs in main.ts setupRouter().
    document.addEventListener('nav-click', listener);
    try {
      await dispatcher().handle(payload);
      popups[0]?.onclick?.();
    } finally {
      document.removeEventListener('nav-click', listener);
    }
    return path;
  }

  it('routes a thread message to a path the router actually matches', async () => {
    const path = await clickAndCapturePath(thread({ deliveredToUser: true }));
    expect(CHAT_THREAD_ROUTE.test(path as string)).toBe(true);
    expect(path).toContain('proj-1');
    expect(path).toContain('topic-1');
  });

  it('routes a DM to a path the router actually matches', async () => {
    const path = await clickAndCapturePath(dm());
    expect(CHAT_DM_ROUTE.test(path as string)).toBe(true);
    // The colons in the DM key are escaped, so the key stays one path segment.
    expect(decodeURIComponent((path as string).replace('/chat/dm/', ''))).toBe(DM_KEY);
  });

  it('focuses the tab and closes the popup on click', async () => {
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});
    await dispatcher().handle(dm());
    popups[0].onclick?.();
    expect(focus).toHaveBeenCalled();
    expect(popups[0].close).toHaveBeenCalled();
  });

  it('shows a thread message with no project without a click target', async () => {
    const path = await clickAndCapturePath(thread({ projectId: '', deliveredToUser: true }));
    expect(popups).toHaveLength(1);
    expect(path).toBeUndefined();
  });
});

describe('conversation info lookup', () => {
  function respond(body: unknown, ok = true): Response {
    return new Response(JSON.stringify(body), { status: ok ? 200 : 500 });
  }

  it('reads mute and the peer name from the DM list', async () => {
    apiFetch.mockResolvedValue(
      respond({ dms: [{ conversationKey: DM_KEY, muted: true, peerName: 'Ada' }] })
    );
    expect(await lookupConversationInfo(dm(), new Map())).toEqual({
      muted: true,
      peerName: 'Ada',
    });
  });

  it('reads mute, name and creator from the thread list, reusing it', async () => {
    apiFetch.mockResolvedValue(
      respond({ threads: [{ id: 'topic-1', name: 'design', muted: false, createdBy: ME }] })
    );
    const cache = new Map();
    expect(await lookupConversationInfo(thread(), cache)).toEqual({
      muted: false,
      name: 'design',
      createdBy: ME,
    });
    await lookupConversationInfo(thread(), cache);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(apiFetch).toHaveBeenCalledWith('/api/v1/chat/spaces/proj-1/threads');
  });

  it('answers null when the list cannot be loaded', async () => {
    apiFetch.mockResolvedValue(respond({}, false));
    expect(await lookupConversationInfo(thread(), new Map())).toBeNull();
  });

  it('a mute takes effect after invalidation, not on cache expiry', async () => {
    apiFetch.mockResolvedValueOnce(respond({ threads: [{ id: 'topic-1', createdBy: ME }] }));
    const d = new ChatNotificationDispatcher();
    d.start(ME);
    started.push(d);
    expect(await d.handle(thread())).toBeNull();

    apiFetch.mockResolvedValueOnce(
      respond({ threads: [{ id: 'topic-1', createdBy: ME, muted: true }] })
    );
    d.invalidateConversationInfo();
    expect(await d.handle(thread())).toBe('muted');
  });
});
