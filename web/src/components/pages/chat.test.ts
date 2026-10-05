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
 * Tests for two chat page navigation paths that have no server round-trip:
 *
 *  1. Clicking an @mention opens the DM with that member. The slug the
 *     composer inserts is not always the member's own slug — it can be a
 *     display name lowercased with dashes — so resolution has to fold both.
 *  2. Mobile swipe navigation between the rail / conversation / members
 *     panels, which must ignore vertical scrolling and desktop viewports.
 *
 * Elements are created but never appended, so connectedCallback (and its
 * network calls) never runs.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { render, type TemplateResult } from 'lit';
import { apiFetch } from '../../client/api.js';
import { navigateTo, replaceRoute } from '../../client/main.js';
import { PAGE_TITLE_EVENT } from '../../client/page-title.js';
import { chatDMsLoad, chatSpacesLoad } from '../../client/chat-list-cache.js';
import { FakeEventSource } from '../../client/__fixtures__/agent-store-harness.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  replaceRoute: vi.fn((path: string) => {
    window.history.replaceState(
      window.history.state,
      '',
      path + window.location.search + window.location.hash
    );
    return Promise.resolve();
  }),
  stateManager: new EventTarget(),
}));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

let ScionPageChat: any;

// A connected page retains the agent store's hub list, which opens the
// store's feed; it never connects here.
beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  // Page-wide shared list loads: one test's DM list must not answer the next.
  chatDMsLoad.invalidate();
  chatSpacesLoad.invalidate();
});

/** Let pending promise callbacks (a few macrotask turns of awaits) run. */
async function flush(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
}

describe('chat mention roster stability', () => {
  it('reuses agent props until the member roster or project changes', () => {
    const page = createPage();
    page.v2Conversation = { projectId: 'p1' };
    page.v2Members = [{ id: 'a', kind: 'agent', name: 'Coder', email: '' }];
    const agents = page.getAgentsFromMembers();
    page.v2TypingUserIds = ['someone'];
    expect(page.getAgentsFromMembers()).toBe(agents);
    page.v2Conversation = { projectId: 'p2' };
    expect(page.getAgentsFromMembers()[0].projectId).toBe('p2');
    page.v2Members = [{ id: 'a', kind: 'agent', name: 'Renamed', email: '' }];
    expect(page.getAgentsFromMembers()[0].name).toBe('Renamed');
  });
});

/** A page instance with a signed-in user and a small member roster. */
function createPage(): any {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  el.v2AgentMembers = [
    { id: 'agent-1', kind: 'agent', displayName: 'Coder One', slug: 'coder-one' },
    { id: 'agent-2', kind: 'agent', displayName: 'Review Bot' },
  ];
  el.v2HumanMembers = [
    { id: 'user-1', kind: 'user', displayName: 'Ada Lovelace', email: 'ada@example.com' },
  ];
  return el;
}

/**
 * A page instance parked on the conversation panel — the state mobile reaches
 * once a thread or DM has been opened. The panel default is the rail, so the
 * swipe tests that start mid-track have to say so explicitly.
 */
function createPageOnConversation(): any {
  const el = createPage();
  el.mobilePanel = 'center';
  return el;
}

/** Render a template on its own so header fragments can be queried. */
function renderToFragment(tpl: TemplateResult): HTMLElement {
  const host = document.createElement('div');
  render(tpl, host);
  return host;
}

/**
 * Answer the topic detail endpoint with a thread's metadata. Every other
 * request the route parse fires (members, agents) gets an empty object.
 */
function serveTopic(topic: { name?: string; defaultAgent?: string }): void {
  vi.mocked(apiFetch).mockImplementation((path: string) =>
    Promise.resolve(
      new Response(path.startsWith('/api/v1/chat/topics/') ? JSON.stringify(topic) : '{}', {
        status: 200,
      })
    )
  );
}

/** Fire a mention click at the page as the message component would. */
function clickMention(el: any, slug: string): void {
  el.handleMentionClick(new CustomEvent('mention-click', { detail: { slug } }));
}

/**
 * Drive one touch gesture through the page's swipe handlers. `path` is the
 * touch's composed path (what sits under the finger); the return value says
 * whether the page cancelled the move.
 */
function swipe(
  el: any,
  opts: { dx: number; dy?: number; durationMs?: number; path?: EventTarget[] }
): { moveCancelled: boolean } {
  const dy = opts.dy ?? 0;
  const start = 200;
  const now = Date.now();
  vi.setSystemTime(now);
  let moveCancelled = false;

  el.handleTouchStart({
    touches: [{ clientX: start, clientY: 100 }],
    composedPath: () => opts.path ?? [],
  });
  el.handleTouchMove({
    touches: [{ clientX: start + opts.dx, clientY: 100 + dy }],
    cancelable: true,
    preventDefault: () => {
      moveCancelled = true;
    },
  });
  vi.setSystemTime(now + (opts.durationMs ?? 100));
  el.handleTouchEnd({ changedTouches: [{ clientX: start + opts.dx, clientY: 100 + dy }] });
  return { moveCancelled };
}

/**
 * Drive a two-finger pinch through the page's swipe handlers. The first
 * finger lands alone (on `path`), the second lands `secondAfterMove` moves
 * later (0 = both land together), both move sideways by `dx`, then the
 * second lifts and the first keeps moving before it lifts too. Returns
 * whether any move from the second finger landing on was cancelled (before
 * that, it is still a one-finger drag).
 */
function pinch(
  el: any,
  opts: { dx: number; path?: EventTarget[]; secondAfterMove?: number }
): { moveCancelled: boolean } {
  const now = Date.now();
  vi.setSystemTime(now);
  let moveCancelled = false;
  let pinching = false;
  const path = () => opts.path ?? [];
  const finger = (x: number) => ({ clientX: x, clientY: 100 });
  const move = (touches: unknown[]) =>
    el.handleTouchMove({
      touches,
      cancelable: true,
      preventDefault: () => {
        if (pinching) moveCancelled = true;
      },
    });
  const together = (opts.secondAfterMove ?? 1) === 0;

  pinching = together;
  el.handleTouchStart({
    touches: together ? [finger(200), finger(260)] : [finger(200)],
    composedPath: path,
  });
  if (!together) {
    for (let i = 1; i <= (opts.secondAfterMove ?? 1); i++) {
      move([finger(200 + (opts.dx / 8) * i)]);
    }
    pinching = true;
    el.handleTouchStart({ touches: [finger(200), finger(260)], composedPath: path });
  }
  for (let i = 1; i <= 8; i++) {
    move([finger(200 + opts.dx * (i / 8)), finger(260 - opts.dx * (i / 8))]);
  }
  vi.setSystemTime(now + 100);
  // The second finger lifts; the first is still down and keeps moving.
  el.handleTouchEnd({ touches: [finger(200 + opts.dx)], changedTouches: [finger(260 - opts.dx)] });
  move([finger(200 + opts.dx * 2)]);
  el.handleTouchEnd({ touches: [], changedTouches: [finger(200 + opts.dx * 2)] });
  return { moveCancelled };
}

/** A sideways scroller 500px wider than its box, scrolled to `scrollLeft`. */
function wideScroller(scrollLeft: number): HTMLElement {
  const el = document.createElement('pre');
  el.style.overflowX = 'auto';
  Object.defineProperty(el, 'scrollWidth', { value: 800 });
  Object.defineProperty(el, 'clientWidth', { value: 300 });
  Object.defineProperty(el, 'scrollLeft', { value: scrollLeft });
  document.body.appendChild(el);
  return el;
}

beforeAll(async () => {
  const mod = await import('./chat.js');
  ScionPageChat = mod.ScionPageChat;
  expect(ScionPageChat).toBeDefined();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('chat page — @mention click opens a DM', () => {
  it('resolves an agent by its slug', () => {
    const el = createPage();
    clickMention(el, 'coder-one');

    expect(el.v2Conversation).toMatchObject({
      isDM: true,
      peerId: 'agent-1',
      peerKind: 'agent',
      peerName: 'Coder One',
      conversationKey: 'dm:agent:agent-1:user:user-me',
    });
  });

  it('resolves an agent by its display name when it has no slug', () => {
    const el = createPage();
    clickMention(el, 'review-bot');

    expect(el.v2Conversation).toMatchObject({ peerId: 'agent-2', peerKind: 'agent' });
  });

  it('resolves a human by display name or email prefix', () => {
    const byName = createPage();
    clickMention(byName, 'ada-lovelace');
    expect(byName.v2Conversation).toMatchObject({ peerId: 'user-1', peerKind: 'user' });

    const byEmail = createPage();
    clickMention(byEmail, 'ada');
    expect(byEmail.v2Conversation).toMatchObject({ peerId: 'user-1', peerKind: 'user' });
  });

  it('leaves the view alone for an unknown mention', () => {
    const el = createPage();
    clickMention(el, 'nobody');

    expect(el.v2Conversation).toBeNull();
  });

  it('brings the mobile view back to the conversation panel', () => {
    const el = createPage();
    el.mobilePanel = 'right';
    clickMention(el, 'coder-one');

    expect(el.mobilePanel).toBe('center');
  });
});

describe('chat page — mobile panel default and header navigation', () => {
  it('starts on the space rail so a conversation can be picked', () => {
    expect(createPage().mobilePanel).toBe('left');
  });

  it('returns to the rail when the route clears the conversation', () => {
    const el = createPageOnConversation();
    window.history.replaceState({}, '', '/chat');

    el.parseV2Route();

    expect(el.v2Conversation).toBeNull();
    expect(el.mobilePanel).toBe('left');
  });

  it('opens the conversation panel for a deep-linked DM', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/dm/dm:agent:agent-1:user:user-me');

    el.parseV2Route();

    expect(el.mobilePanel).toBe('center');
  });

  it('opens a DM linked by peer ID on the conversation panel', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/dm/agent-1');

    el.parseV2Route();

    expect(el.v2Conversation).toMatchObject({
      conversationKey: 'dm:agent:agent-1:user:user-me',
      peerId: 'agent-1',
    });
    expect(el.mobilePanel).toBe('center');
  });

  it('leaves the panel alone when a peer-ID DM route is re-parsed while that DM is open', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    el.parseV2Route();
    const opened = el.v2Conversation;
    // The user swipes back to the rail; the URL stays on the DM.
    el.mobilePanel = 'left';

    el.parseV2Route();

    expect(el.mobilePanel).toBe('left');
    expect(el.v2Conversation).toBe(opened);
  });

  it('corrects a peer-ID DM opened before the agents loaded, without moving the panel', () => {
    const el = createPage();
    el.v2AgentMembers = [];
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    el.parseV2Route();
    // With no agents known yet, the peer is taken for a user.
    expect(el.v2Conversation.conversationKey).toBe('dm:user:agent-1:user:user-me');
    el.mobilePanel = 'left';

    // The agents arrive and the rail reload re-parses the same route.
    el.v2AgentMembers = [{ id: 'agent-1', kind: 'agent', displayName: 'Coder One' }];
    el.parseV2Route();

    expect(el.v2Conversation).toMatchObject({
      conversationKey: 'dm:agent:agent-1:user:user-me',
      peerId: 'agent-1',
      peerKind: 'agent',
    });
    expect(el.mobilePanel).toBe('left');
  });

  it('leaves the panel alone when a full-key DM route is re-parsed while that DM is open', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/dm/dm:agent:agent-1:user:user-me');
    el.parseV2Route();
    el.mobilePanel = 'left';

    el.parseV2Route();

    expect(el.mobilePanel).toBe('left');
  });

  it('rewrites a legacy thread URL in place without reopening the open thread', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/space/p1/thread/topic-1#msg-m1');
    el.parseV2Route();
    expect(el.mobilePanel).toBe('center');
    const opened = el.v2Conversation;
    el.mobilePanel = 'left';
    const historyLength = window.history.length;

    // The rail's first load makes the slug known and re-parses the route.
    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');
    el.parseV2Route();

    expect(replaceRoute).toHaveBeenCalledWith('/chat/alpha/topic-1');
    expect(window.location.pathname).toBe('/chat/alpha/topic-1');
    expect(window.location.hash).toBe('#msg-m1');
    expect(window.history.length).toBe(historyLength);
    // Same thread, now carrying the slug the URL names.
    expect(el.v2Conversation).toEqual({ ...opened, projectSlug: 'alpha' });
    expect(el.mobilePanel).toBe('left');
  });

  it('re-titles the thread once the router has caught up with the rewrite', async () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/space/p1/thread/topic-1');
    el.parseV2Route();
    el.v2Conversation = { ...el.v2Conversation, threadName: 'general' };
    const titles: string[][] = [];
    el.addEventListener(PAGE_TITLE_EVENT, (e: Event) =>
      titles.push((e as CustomEvent).detail.segments)
    );

    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');
    el.parseV2Route();
    await Promise.resolve();
    await Promise.resolve();

    expect(titles.at(-1)).toEqual(['#general', 'Chat']);
  });

  it('renders a back button that returns to the rail', () => {
    const el = createPageOnConversation();
    const back = renderToFragment(el.renderMobileBackButton()).querySelector('.mobile-back');

    expect(back?.getAttribute('name')).toBe('chevron-left');
    back?.dispatchEvent(new Event('click'));
    expect(el.mobilePanel).toBe('left');
  });

  it('renders a members button that opens the members panel', () => {
    const el = createPageOnConversation();
    const members = renderToFragment(el.renderMembersButtons()).querySelector('.mobile-members');

    expect(members?.getAttribute('name')).toBe('people');
    members?.dispatchEvent(new Event('click'));
    expect(el.mobilePanel).toBe('right');
  });

  it('keeps the desktop members toggle separate from the mobile one', () => {
    const el = createPageOnConversation();
    el.v2MembersExpanded = true;
    const frag = renderToFragment(el.renderMembersButtons());

    frag.querySelector('.desktop-members sl-icon-button')?.dispatchEvent(new Event('click'));

    expect(el.v2MembersExpanded).toBe(false);
    // The desktop toggle must not move the mobile track.
    expect(el.mobilePanel).toBe('center');
  });

  it('gives the members panel a back button to the conversation', () => {
    const el = createPage();
    el.mobilePanel = 'right';
    const back = renderToFragment(el.renderMobileBackButton('center')).querySelector(
      '.mobile-back'
    );

    back?.dispatchEvent(new Event('click'));

    expect(el.mobilePanel).toBe('center');
  });
});

describe('chat page — deep-linked thread header', () => {
  /** Open /chat/<slug>/<threadId> with the slug already resolved. */
  function deepLinkToThread(el: any): void {
    el._slugToProjectId.set('chat-test', 'proj-1');
    window.history.replaceState({}, '', '/chat/chat-test/topic-1');
    el.parseV2Route();
  }

  it('renders the header before the thread name has resolved', () => {
    serveTopic({});
    const el = createPage();
    deepLinkToThread(el);
    expect(el.v2Conversation.threadName).toBe('');

    const header = renderToFragment(el.renderV2Conversation()).querySelector('.v2-thread-header');

    // No name yet, but the way out of the conversation must still be there.
    expect(header).not.toBeNull();
    expect(header?.querySelector('.mobile-back')).not.toBeNull();
  });

  it('fills the thread name in from the topic endpoint', async () => {
    serveTopic({ name: 'general', defaultAgent: 'coder-one' });
    const el = createPage();

    deepLinkToThread(el);

    await vi.waitFor(() => expect(el.v2Conversation.threadName).toBe('general'));
    expect(el.v2Conversation.defaultAgent).toBe('coder-one');
    const header = renderToFragment(el.renderV2Conversation()).querySelector('.v2-thread-header');
    expect(header?.textContent).toContain('general');
  });

  it('keeps a resolved name across a re-parse of the same route', async () => {
    serveTopic({ name: 'general' });
    const el = createPage();
    deepLinkToThread(el);
    await vi.waitFor(() => expect(el.v2Conversation.threadName).toBe('general'));

    // The rail finishing its load re-parses the route.
    el.parseV2Route();

    expect(el.v2Conversation.threadName).toBe('general');
  });

  it('renders the header for a DM whose peer has not resolved yet', () => {
    const el = createPage();
    window.history.replaceState({}, '', '/chat/dm/dm:agent:agent-9:user:user-me');

    el.parseV2Route();

    const header = renderToFragment(el.renderV2Conversation()).querySelector('.v2-thread-header');
    expect(header).not.toBeNull();
  });
});

describe('chat page — mobile swipe navigation', () => {
  afterEach(() => {
    // The scroller helpers attach their elements to measure them.
    document.body.replaceChildren();
  });

  // The element is never connected (see the file doc comment), so the
  // connectedCallback matchMedia listener that drives `isMobileLayout` in
  // real usage never runs — set it directly here, the same way `mobilePanel`
  // is set directly below, instead of mutating `window.innerWidth`.
  it('swipes right from the conversation to the rail, and back left', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    swipe(el, { dx: 120 });
    expect(el.mobilePanel).toBe('left');

    swipe(el, { dx: -120 });
    expect(el.mobilePanel).toBe('center');
  });

  it('swipes left from the conversation to the members panel, and back right', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    swipe(el, { dx: -120 });
    expect(el.mobilePanel).toBe('right');

    swipe(el, { dx: 120 });
    expect(el.mobilePanel).toBe('center');
  });

  it('does not run past the outermost panels', () => {
    vi.useFakeTimers();
    const el = createPage();
    el.isMobileLayout = true;
    el.mobilePanel = 'left';

    swipe(el, { dx: 120 });
    expect(el.mobilePanel).toBe('left');

    el.mobilePanel = 'right';
    swipe(el, { dx: -120 });
    expect(el.mobilePanel).toBe('right');
  });

  it('accepts a short fast flick but not a short slow drag', () => {
    vi.useFakeTimers();
    const flick = createPageOnConversation();
    flick.isMobileLayout = true;
    swipe(flick, { dx: 60, durationMs: 150 });
    expect(flick.mobilePanel).toBe('left');

    const slow = createPageOnConversation();
    slow.isMobileLayout = true;
    swipe(slow, { dx: 60, durationMs: 900 });
    expect(slow.mobilePanel).toBe('center');
  });

  it('ignores a mostly vertical drag — that is the message list scrolling', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    swipe(el, { dx: 120, dy: 200 });

    expect(el.mobilePanel).toBe('center');
  });

  it('leaves a drag to a sideways scroller that can still scroll that way', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    // At its start: a leftward drag scrolls it, and is not a swipe.
    const left = swipe(el, { dx: -120, path: [wideScroller(0)] });
    expect(el.mobilePanel).toBe('center');
    expect(left.moveCancelled, 'the scroller keeps the pan').toBe(false);

    // Part-way along: the same holds for a rightward drag.
    const right = swipe(el, { dx: 120, path: [wideScroller(200)] });
    expect(el.mobilePanel).toBe('center');
    expect(right.moveCancelled).toBe(false);
  });

  it('swipes panels from a sideways scroller already at its end, and keeps the pan from the browser', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    // At its start a rightward drag has nothing to scroll: it is a panel
    // swipe, and the move is cancelled so the browser cannot claim the pan
    // as a history swipe.
    const atStart = swipe(el, { dx: 120, path: [wideScroller(0)] });
    expect(el.mobilePanel).toBe('left');
    expect(atStart.moveCancelled).toBe(true);

    el.mobilePanel = 'center';
    const atEnd = swipe(el, { dx: -120, path: [wideScroller(500)] });
    expect(el.mobilePanel).toBe('right');
    expect(atEnd.moveCancelled).toBe(true);
  });

  it('never cancels a move with no sideways scroller under the touch', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;
    expect(swipe(el, { dx: 120 }).moveCancelled).toBe(false);
    expect(swipe(el, { dx: -120 }).moveCancelled).toBe(false);
  });

  it('leaves a pinch to the browser, even one starting on a scroller at its end', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = true;

    // The first finger lands alone on a code block at its start and moves
    // right (a move that would be cancelled for one finger), then a second
    // finger lands: the pinch must never be cancelled, nor swipe panels.
    expect(pinch(el, { dx: 120, path: [wideScroller(0)] }).moveCancelled).toBe(false);
    expect(el.mobilePanel).toBe('center');

    // Both fingers landing together, on a scroller at its end.
    expect(
      pinch(el, { dx: -120, path: [wideScroller(500)], secondAfterMove: 0 }).moveCancelled
    ).toBe(false);
    expect(el.mobilePanel).toBe('center');

    // Away from any scroller.
    expect(pinch(el, { dx: 120 }).moveCancelled).toBe(false);
    expect(el.mobilePanel).toBe('center');

    // The next one-finger gesture swipes again.
    expect(swipe(el, { dx: 120, path: [wideScroller(0)] }).moveCancelled).toBe(true);
    expect(el.mobilePanel).toBe('left');
  });

  it('never cancels a move on desktop viewports, even on a scroller at its end', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = false;
    expect(swipe(el, { dx: 120, path: [wideScroller(0)] }).moveCancelled).toBe(false);
    expect(swipe(el, { dx: -120, path: [wideScroller(500)] }).moveCancelled).toBe(false);
    expect(el.mobilePanel).toBe('center');
  });

  it('ignores swipes on desktop viewports', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();
    el.isMobileLayout = false;

    swipe(el, { dx: 200 });

    expect(el.mobilePanel).toBe('center');
  });
});

describe('chat page — DM mute toggle', () => {
  /** A page with an open DM conversation in the given muted state. */
  function pageOnDM(muted: boolean): any {
    const el = createPage();
    el.v2Conversation = {
      conversationKey: 'dm:user-me:user-1',
      projectId: 'proj-1',
      threadName: '',
      peerName: 'Ada Lovelace',
      peerId: 'user-1',
      peerKind: 'user',
      isDM: true,
      muted,
    };
    return el;
  }

  it('PUTs the mute endpoint for the open DM and flips the local state', async () => {
    const el = pageOnDM(false);
    vi.mocked(apiFetch).mockResolvedValue(
      new Response(JSON.stringify({ muted: true }), { status: 200 })
    );

    await el.toggleDMMute();

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/chat/conversations/dm%3Auser-me%3Auser-1/mute',
      expect.objectContaining({ method: 'PUT', body: JSON.stringify({ muted: true }) })
    );
    expect(el.v2Conversation.muted).toBe(true);
  });

  it('unmutes a muted DM', async () => {
    const el = pageOnDM(true);
    vi.mocked(apiFetch).mockResolvedValue(
      new Response(JSON.stringify({ muted: false }), { status: 200 })
    );

    await el.toggleDMMute();

    expect(apiFetch).toHaveBeenCalledWith(
      expect.stringContaining('/mute'),
      expect.objectContaining({ body: JSON.stringify({ muted: false }) })
    );
    expect(el.v2Conversation.muted).toBe(false);
  });

  it('rolls back when the server refuses', async () => {
    const el = pageOnDM(false);
    vi.mocked(apiFetch).mockResolvedValue(new Response('{}', { status: 403 }));

    await el.toggleDMMute();

    expect(el.v2Conversation.muted).toBe(false);
  });

  it('does not reconcile onto a DM the user switched to mid-request', async () => {
    const el = pageOnDM(false);
    // The server disagrees with the optimistic value, so the success path wants
    // to write back — but by the time it resolves the user is reading another DM.
    vi.mocked(apiFetch).mockImplementation(async () => {
      el.v2Conversation = { ...el.v2Conversation, conversationKey: 'dm:user-me:user-2' };
      return new Response(JSON.stringify({ muted: false }), { status: 200 });
    });

    await el.toggleDMMute();

    expect(el.v2Conversation.conversationKey).toBe('dm:user-me:user-2');
    expect(el.v2Conversation.muted).toBe(true);
  });

  it('renders the bell as filled-through only while muted', () => {
    const quiet = renderToFragment(
      pageOnDM(true).renderDMMuteButton(pageOnDM(true).v2Conversation)
    );
    expect(quiet.querySelector('.dm-mute')?.getAttribute('name')).toBe('bell-slash');

    const loud = renderToFragment(
      pageOnDM(false).renderDMMuteButton(pageOnDM(false).v2Conversation)
    );
    expect(loud.querySelector('.dm-mute')?.getAttribute('name')).toBe('bell');
  });
});

describe('chat page — muted DMs raise no unread dot', () => {
  it('ignores an older unread response after a newer refresh clears the dot', async () => {
    const el = createPage();
    el.v2UnreadFromIds = ['agent-1', 'agent-2'];
    let resolveOld!: (response: Response) => void;
    vi.mocked(apiFetch)
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveOld = resolve;
          })
      )
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            dms: [
              { peerId: 'agent-1', hasUnread: false },
              { peerId: 'agent-2', hasUnread: true },
            ],
          })
        )
      );
    const oldRequest = el.loadUnreadDMPeers();
    await el.loadUnreadDMPeers();
    expect(el.v2UnreadFromIds).toEqual(['agent-2']);
    resolveOld(
      new Response(
        JSON.stringify({
          dms: [
            { peerId: 'agent-1', hasUnread: true },
            { peerId: 'agent-2', hasUnread: true },
          ],
        })
      )
    );
    await oldRequest;
    expect(el.v2UnreadFromIds).toEqual(['agent-2']);
  });

  it.each(['dm:agent:agent-1:user:user-me', 'dm:user:user-me:agent:agent-1'])(
    'clears the acknowledged peer, not the selected conversation (%s)',
    (key) => {
      const el = createPage();
      el.v2UnreadFromIds = ['agent-1', 'agent-2'];
      el.v2Conversation = { peerId: 'agent-2' };
      const refresh = vi.spyOn(el, 'loadUnreadDMPeers').mockResolvedValue(undefined);
      el._handleReadStateUpdated(
        new CustomEvent('read-state-updated', {
          detail: { conversationKey: key },
        })
      );
      expect(el.v2UnreadFromIds).toEqual(['agent-2']);
      expect(refresh).toHaveBeenCalledOnce();
    }
  );

  /** Answer GET /api/v1/chat/dms with the given entries. */
  function serveDMs(dms: Array<Record<string, unknown>>): void {
    vi.mocked(apiFetch).mockImplementation((url: string) => {
      if (url.startsWith('/api/v1/chat/dms')) {
        return Promise.resolve(new Response(JSON.stringify({ dms }), { status: 200 }));
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });
  }

  it('leaves a muted DM out of the unread peers', async () => {
    const el = createPage();
    serveDMs([
      { peerId: 'user-1', hasUnread: true, muted: true },
      { peerId: 'user-2', hasUnread: true, muted: false },
    ]);

    await el.loadUnreadDMPeers();

    expect(el.v2UnreadFromIds).toEqual(['user-2']);
  });

  it('keeps unmuted unread DMs when muted is absent from the payload', async () => {
    const el = createPage();
    serveDMs([{ peerId: 'user-1', hasUnread: true }]);

    await el.loadUnreadDMPeers();

    expect(el.v2UnreadFromIds).toEqual(['user-1']);
  });

  it('drops every dot when the only unread DMs are muted', async () => {
    const el = createPage();
    el.v2UnreadFromIds = ['user-1'];
    serveDMs([{ peerId: 'user-1', hasUnread: true, muted: true }]);

    await el.loadUnreadDMPeers();

    expect(el.v2UnreadFromIds).toEqual([]);
  });
});

/**
 * Page-level coverage for mark-unread's SSE unread gate, mute check, and
 * same-tab suppression. These tests exercise `_handleOwnReadStateSSE`,
 * `handleMemberMarkedUnread` and `_handleConversationMarkedUnread` directly,
 * stubbing the rail/thread elements `shadowRoot.querySelector` would
 * otherwise find, rather than mounting the full page (which would fire its
 * own network calls).
 */
describe('chat page — mark-unread page-level handling', () => {
  /** A page with an open DM conversation with the given peer. */
  function pageOnDM(peerId: string, conversationKey: string): any {
    const el = createPage();
    el.v2Conversation = {
      conversationKey,
      projectId: 'proj-1',
      threadName: '',
      peerName: 'Peer',
      peerId,
      peerKind: 'user',
      isDM: true,
    };
    return el;
  }

  /**
   * These pages are never appended to the document (by this file's own
   * design — see the file header — so connectedCallback's network calls
   * never fire), which means Lit never creates a real shadowRoot to query.
   * Replace the accessor with a fake one backed by a selector→element map, so
   * more than one stub (rail and thread) can coexist on the same page — a
   * single-selector version would silently resolve every other selector to
   * null, letting a test assert less than its title claims.
   */
  function stubShadowRoot(el: any, found: Record<string, unknown>): void {
    const fakeShadowRoot = { querySelector: (sel: string) => found[sel] ?? null };
    Object.defineProperty(el, 'shadowRoot', { value: fakeShadowRoot, configurable: true });
  }

  /** Stub the open thread element so suppressOpenThreadAutoAdvance has something to call. */
  function stubThread(el: any): { suppressAutoAdvance: ReturnType<typeof vi.fn> } {
    const thread = { suppressAutoAdvance: vi.fn() };
    stubShadowRoot(el, { 'scion-chat-thread': thread });
    return thread;
  }

  /** Stub the rail element so markThreadUnread calls are observable. */
  function stubRail(el: any): { markThreadUnread: ReturnType<typeof vi.fn> } {
    const rail = { markThreadUnread: vi.fn() };
    stubShadowRoot(el, { 'scion-chat-space-rail': rail });
    return rail;
  }

  /**
   * Stub both the rail and the open thread on the same page — needed for the
   * topic branch of _handleOwnReadStateSSE, which looks up both: the rail to
   * mark the thread unread, and (if it is the open conversation) the thread
   * to suppress its auto-advance.
   */
  function stubRailAndThread(el: any): {
    rail: { markThreadUnread: ReturnType<typeof vi.fn> };
    thread: { suppressAutoAdvance: ReturnType<typeof vi.fn> };
  } {
    const rail = { markThreadUnread: vi.fn() };
    const thread = { suppressAutoAdvance: vi.fn() };
    stubShadowRoot(el, { 'scion-chat-space-rail': rail, 'scion-chat-thread': thread });
    return { rail, thread };
  }

  describe('_handleOwnReadStateSSE unread gate', () => {
    it('a self event without unread:true adds no dot and does not mark the rail thread unread', () => {
      const el = createPage();
      el.v2UnreadFromIds = [];
      const rail = stubRail(el);

      el._handleOwnReadStateSSE(
        new CustomEvent('chat-read-state-updated', {
          detail: { data: { conversationKey: 'topic-1', userId: 'user-me', messageId: 'm1' } },
        })
      );

      expect(el.v2UnreadFromIds).toEqual([]);
      expect(rail.markThreadUnread).not.toHaveBeenCalled();
    });

    it('a self event with unread:true for a topic marks the rail thread unread and suppresses the open thread', () => {
      const el = createPage();
      el.v2Conversation = { conversationKey: 'topic-1' };
      const { rail, thread } = stubRailAndThread(el);

      el._handleOwnReadStateSSE(
        new CustomEvent('chat-read-state-updated', {
          detail: {
            data: { conversationKey: 'topic-1', userId: 'user-me', messageId: '', unread: true },
          },
        })
      );

      expect(rail.markThreadUnread).toHaveBeenCalledWith('topic-1');
      expect(thread.suppressAutoAdvance).toHaveBeenCalledTimes(1);
    });

    it('a self event with unread:true for a DM updates the dot, hasUnread, and suppresses if open', () => {
      const dmKey = 'dm:user:user-me:user:user-1';
      const el = pageOnDM('user-1', dmKey);
      el.v2DMInfoByPeerId = { 'user-1': { key: dmKey, muted: false, hasUnread: false } };
      const thread = stubThread(el);

      el._handleOwnReadStateSSE(
        new CustomEvent('chat-read-state-updated', {
          detail: {
            data: { conversationKey: dmKey, userId: 'user-me', messageId: '', unread: true },
          },
        })
      );

      expect(el.v2UnreadFromIds).toEqual(['user-1']);
      expect(el.v2DMInfoByPeerId['user-1'].hasUnread).toBe(true);
      expect(thread.suppressAutoAdvance).toHaveBeenCalledTimes(1);
    });
  });

  describe('applyDMMarkedUnread mute check and hasUnread state', () => {
    it('a muted peer gets no dot, but hasUnread flips so the members item hides', () => {
      const el = createPage();
      el.v2DMInfoByPeerId = {
        'user-1': { key: 'dm:user:user-me:user:user-1', muted: true, hasUnread: false },
      };
      el.v2UnreadFromIds = [];

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', { detail: { peerId: 'user-1' } })
      );

      expect(el.v2UnreadFromIds).toEqual([]);
      expect(el.v2DMInfoByPeerId['user-1'].hasUnread).toBe(true);
    });

    it('an unmuted peer gets a dot, and hasUnread flips so the members item hides', () => {
      const el = createPage();
      el.v2DMInfoByPeerId = {
        'user-1': { key: 'dm:user:user-me:user:user-1', muted: false, hasUnread: false },
      };
      el.v2UnreadFromIds = [];

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', { detail: { peerId: 'user-1' } })
      );

      expect(el.v2UnreadFromIds).toEqual(['user-1']);
      expect(el.v2DMInfoByPeerId['user-1'].hasUnread).toBe(true);
    });

    it('leaves the info map alone for a peer with no existing entry', () => {
      const el = createPage();
      el.v2DMInfoByPeerId = {};
      el.v2UnreadFromIds = [];

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', { detail: { peerId: 'user-1' } })
      );

      expect(el.v2DMInfoByPeerId).toEqual({});
      // No mute info to check against, so the dot still goes on — a peer
      // loadUnreadDMPeers hasn't captured yet is self-correcting on the next load.
      expect(el.v2UnreadFromIds).toEqual(['user-1']);
    });

    // Pins the `v2UnreadFromIds.includes(peerId)` duplicate guard. Not
    // user-visible (a Set-like list either way), but cheap to pin: the local
    // click and the SSE echo of the same mark-unread both call this, and a
    // duplicate id would be a real (if harmless) bug.
    it('does not duplicate the dot when applied twice for the same peer', () => {
      const el = createPage();
      el.v2DMInfoByPeerId = {
        'user-1': { key: 'dm:user:user-me:user:user-1', muted: false, hasUnread: false },
      };
      el.v2UnreadFromIds = [];

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', { detail: { peerId: 'user-1' } })
      );
      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', { detail: { peerId: 'user-1' } })
      );

      expect(el.v2UnreadFromIds).toEqual(['user-1']);
    });
  });

  describe('same-tab suppression calls', () => {
    it('member-marked-unread for the open conversation suppresses its auto-advance', () => {
      const dmKey = 'dm:user:user-me:user:user-1';
      const el = pageOnDM('user-1', dmKey);
      const thread = stubThread(el);

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', {
          detail: { peerId: 'user-1', conversationKey: dmKey },
        })
      );

      expect(thread.suppressAutoAdvance).toHaveBeenCalledTimes(1);
    });

    it('member-marked-unread for a DM that is not open does not suppress', () => {
      const el = pageOnDM('user-1', 'dm:user:user-me:user:user-1');
      const thread = stubThread(el);

      el.handleMemberMarkedUnread(
        new CustomEvent('member-marked-unread', {
          detail: { peerId: 'user-2', conversationKey: 'dm:user:user-me:user:user-2' },
        })
      );

      expect(thread.suppressAutoAdvance).not.toHaveBeenCalled();
    });

    it('conversation-marked-unread for the open conversation suppresses its auto-advance', () => {
      const el = createPage();
      el.v2Conversation = { conversationKey: 'topic-1' };
      const thread = stubThread(el);

      el._handleConversationMarkedUnread(
        new CustomEvent('conversation-marked-unread', { detail: { conversationKey: 'topic-1' } })
      );

      expect(thread.suppressAutoAdvance).toHaveBeenCalledTimes(1);
    });

    it('conversation-marked-unread for a different conversation does not suppress', () => {
      const el = createPage();
      el.v2Conversation = { conversationKey: 'topic-1' };
      const thread = stubThread(el);

      el._handleConversationMarkedUnread(
        new CustomEvent('conversation-marked-unread', { detail: { conversationKey: 'topic-2' } })
      );

      expect(thread.suppressAutoAdvance).not.toHaveBeenCalled();
    });
  });
});

describe('chat page — promote DM dialog', () => {
  function pageOnAgentDM(): any {
    const el = createPage();
    el.v2Conversation = {
      conversationKey: 'dm:agent:agent-1:user:user-me',
      projectId: 'proj-1',
      projectSlug: '',
      threadName: '',
      peerName: 'Coder One',
      peerId: 'agent-1',
      peerKind: 'agent',
      isDM: true,
    };
    el.promoteDialogOpen = true;
    el.promoteThreadName = 'coder-one';
    return el;
  }

  it('looks up the project slug when the DM does not carry one', () => {
    const el = pageOnAgentDM();
    el._projectIdToSlug.set('proj-1', 'chat-test');

    const dialog = renderToFragment(el.renderPromoteDialog());
    const projectName = dialog.querySelectorAll('strong')[1];

    expect(projectName?.textContent).toBe('chat-test');
  });

  it('uses a readable fallback when the project slug is unavailable', () => {
    const el = pageOnAgentDM();

    const dialog = renderToFragment(el.renderPromoteDialog());
    const projectName = dialog.querySelectorAll('strong')[1];

    expect(projectName?.textContent).toBe('this project');
  });

  it('shows the nested backend error message instead of coercing the error object', async () => {
    const el = pageOnAgentDM();
    const showPromoteToast = vi.spyOn(el, 'showPromoteToast').mockImplementation(() => undefined);
    vi.mocked(apiFetch).mockResolvedValue(
      new Response(
        JSON.stringify({ error: { code: 'PROMOTION_FAILED', message: 'Promotion unavailable' } }),
        { status: 422 }
      )
    );

    await el.executePromote();

    expect(showPromoteToast).toHaveBeenCalledWith('Promotion unavailable', 'danger');
  });

  it('shows a string backend error message', async () => {
    const el = pageOnAgentDM();
    const showPromoteToast = vi.spyOn(el, 'showPromoteToast').mockImplementation(() => undefined);
    vi.mocked(apiFetch).mockResolvedValue(
      new Response(JSON.stringify({ error: 'Promotion unavailable' }), { status: 422 })
    );

    await el.executePromote();

    expect(showPromoteToast).toHaveBeenCalledWith('Promotion unavailable', 'danger');
  });

  it('uses the nested backend error code for conflict guidance', async () => {
    const el = pageOnAgentDM();
    const showPromoteToast = vi.spyOn(el, 'showPromoteToast').mockImplementation(() => undefined);
    vi.mocked(apiFetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          error: { code: 'IN_FLIGHT_MESSAGES', message: 'agent has pending replies' },
        }),
        { status: 409 }
      )
    );

    await el.executePromote();

    expect(showPromoteToast).toHaveBeenCalledWith(
      'Agent is still responding. Try again in a few seconds.',
      'warning'
    );
  });
});

describe('chat page — late route lookups', () => {
  /**
   * Hold the project-by-slug lookup until `release` is called; every other
   * request gets an empty object.
   */
  function holdSlugLookup(): () => void {
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(async (path: string) => {
      if (path.startsWith('/api/v1/projects?slug=')) {
        await held;
        return new Response(JSON.stringify({ items: [{ id: 'p1', slug: 'alpha', name: 'A' }] }), {
          status: 200,
        });
      }
      return new Response('{}', { status: 200 });
    });
    return release;
  }

  it('a late slug lookup leaves the panel alone once the rail has opened the thread', async () => {
    const release = holdSlugLookup();
    const el = createPage();
    window.history.replaceState({}, '', '/chat/alpha/topic-1');
    el.parseV2Route();
    // The rail loads first and opens the thread through the known slug.
    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');
    el.parseV2Route();
    expect(el.mobilePanel).toBe('center');
    el.mobilePanel = 'left';

    release();
    await flush();

    expect(el.mobilePanel).toBe('left');
    expect(el.v2Conversation).toMatchObject({ conversationKey: 'topic-1', projectSlug: 'alpha' });
  });

  it('a late slug lookup does not pull the user back from a thread they opened since', async () => {
    const release = holdSlugLookup();
    const el = createPage();
    window.history.replaceState({}, '', '/chat/alpha/topic-1');
    el.parseV2Route();
    el.navigateToThread({
      conversationKey: 'topic-2',
      projectId: 'p1',
      projectSlug: 'alpha',
      threadName: 'two',
    });

    release();
    await flush();

    expect(el.v2Conversation.conversationKey).toBe('topic-2');
    expect(window.location.pathname).toBe('/chat/alpha/topic-2');
  });

  it('a slug lookup still opens a cold-loaded thread', async () => {
    const release = holdSlugLookup();
    const el = createPage();
    window.history.replaceState({}, '', '/chat/alpha/topic-1');
    el.parseV2Route();

    release();
    await flush();

    expect(el.v2Conversation).toMatchObject({ conversationKey: 'topic-1', projectId: 'p1' });
    expect(el.mobilePanel).toBe('center');
  });
});

describe('chat page — late space lookups', () => {
  /**
   * Hold the project-by-slug lookup and the space's thread list until their
   * releases are called; every other request gets an empty object.
   */
  function holdSpaceLookups(): { slug: () => void; threads: () => void } {
    const releases = { slug: () => {}, threads: () => {} };
    const slugHeld = new Promise<void>((resolve) => {
      releases.slug = resolve;
    });
    const threadsHeld = new Promise<void>((resolve) => {
      releases.threads = resolve;
    });
    vi.mocked(apiFetch).mockImplementation(async (path: string) => {
      if (path.startsWith('/api/v1/projects?slug=')) {
        await slugHeld;
        return new Response(JSON.stringify({ items: [{ id: 'p1', slug: 'alpha', name: 'A' }] }), {
          status: 200,
        });
      }
      if (path === '/api/v1/chat/spaces/p1/threads') {
        await threadsHeld;
        return new Response(
          JSON.stringify({ threads: [{ id: 'general-1', name: 'general', isGeneral: true }] }),
          { status: 200 }
        );
      }
      return new Response('{}', { status: 200 });
    });
    return releases;
  }

  /** A mounted-looking page with a stub rail, on the mobile or desktop layout. */
  function createSpacePage(mobile: boolean): any {
    const el = createPage();
    el.isMobileLayout = mobile;
    const rail = {
      expandSpace: vi.fn(),
      // The real rail loads the list over the same endpoint; going through
      // the mocked apiFetch keeps the held thread request in control.
      threadsFor: vi.fn(async (projectId: string) => {
        const res = await apiFetch(`/api/v1/chat/spaces/${projectId}/threads`);
        return ((await res.json()) as { threads?: unknown[] }).threads ?? [];
      }),
    };
    Object.defineProperty(el, 'isConnected', { get: () => true, configurable: true });
    Object.defineProperty(el, 'shadowRoot', {
      get: () => ({
        querySelector: (sel: string) => (sel === 'scion-chat-space-rail' ? rail : null),
      }),
    });
    el.rail = rail;
    vi.mocked(navigateTo).mockClear();
    return el;
  }

  function openThreadFromRail(el: any): void {
    el.navigateToThread({
      conversationKey: 'topic-2',
      projectId: 'p1',
      projectSlug: 'alpha',
      threadName: 'two',
    });
  }

  it('on mobile, a late slug lookup leaves a thread opened since on screen', async () => {
    const releases = holdSpaceLookups();
    const el = createSpacePage(true);
    window.history.replaceState({}, '', '/chat/alpha');
    el.parseV2Route();
    openThreadFromRail(el);
    expect(el.mobilePanel).toBe('center');

    releases.slug();
    await flush();

    expect(el.mobilePanel).toBe('center');
    expect(el.rail.expandSpace).not.toHaveBeenCalled();
  });

  it('on desktop, a late slug lookup does not move the user off a thread opened since', async () => {
    const releases = holdSpaceLookups();
    releases.threads();
    const el = createSpacePage(false);
    window.history.replaceState({}, '', '/chat/alpha');
    el.parseV2Route();
    openThreadFromRail(el);

    releases.slug();
    await flush();

    expect(navigateTo).not.toHaveBeenCalled();
    expect(el.v2Conversation.conversationKey).toBe('topic-2');
  });

  it('on desktop, a late thread list does not move the user off a thread opened since', async () => {
    const releases = holdSpaceLookups();
    const el = createSpacePage(false);
    el._slugToProjectId.set('alpha', 'p1');
    el._projectIdToSlug.set('p1', 'alpha');
    window.history.replaceState({}, '', '/chat/alpha');
    el.parseV2Route();
    openThreadFromRail(el);

    releases.threads();
    await flush();

    expect(navigateTo).not.toHaveBeenCalled();
    expect(el.v2Conversation.conversationKey).toBe('topic-2');
  });

  it('a late lookup on a page that is no longer mounted does nothing', async () => {
    const releases = holdSpaceLookups();
    releases.threads();
    const el = createSpacePage(false);
    window.history.replaceState({}, '', '/chat/alpha');
    el.parseV2Route();
    Object.defineProperty(el, 'isConnected', { get: () => false });

    releases.slug();
    await flush();

    expect(navigateTo).not.toHaveBeenCalled();
    expect(el.v2Conversation).toBeNull();
  });

  it('a slug lookup still opens a cold-loaded space', async () => {
    const releases = holdSpaceLookups();
    releases.threads();
    const el = createSpacePage(false);
    window.history.replaceState({}, '', '/chat/alpha');
    el.parseV2Route();

    releases.slug();
    await flush();

    expect(navigateTo).toHaveBeenCalledWith('/chat/alpha/general-1');
    expect(el.v2Conversation).toMatchObject({ conversationKey: 'general-1', projectSlug: 'alpha' });
  });
});

describe('chat page — rail reload after a message', () => {
  it('asks the rail for spaces requested after the newest message of the burst', async () => {
    vi.useFakeTimers();
    try {
      const el = createPage();
      const rail = { reload: vi.fn(() => Promise.resolve()) };
      Object.defineProperty(el, 'shadowRoot', {
        get: () => ({
          querySelector: (sel: string) => (sel === 'scion-chat-space-rail' ? rail : null),
        }),
      });
      const first = new CustomEvent('chat-message-received', { detail: {} });
      vi.advanceTimersByTime(5);
      const second = new CustomEvent('chat-message-received', { detail: {} });
      el.handleChatMessage(first);
      el.handleChatMessage(second);

      await vi.advanceTimersByTimeAsync(2000);

      expect(rail.reload).toHaveBeenCalledTimes(1);
      expect(rail.reload).toHaveBeenCalledWith({ startedAfter: second.timeStamp });
      expect(second.timeStamp).toBeGreaterThan(first.timeStamp);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('chat page — late DM peer lookups', () => {
  /**
   * Hold each DM-list request until its own release is called, in order;
   * every other request gets an empty object.
   */
  function holdDMLists(): Array<() => void> {
    const releases: Array<() => void> = [];
    vi.mocked(apiFetch).mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/dms') {
        await new Promise<void>((resolve) => releases.push(resolve));
        return new Response(
          JSON.stringify({
            dms: [
              {
                conversationKey: 'dm:agent:agent-1:user:user-me',
                peerId: 'agent-1',
                peerKind: 'agent',
                peerName: 'Coder One',
              },
              {
                conversationKey: 'dm:agent:agent-2:user:user-me',
                peerId: 'agent-2',
                peerKind: 'agent',
                peerName: 'Review Bot',
              },
            ],
          }),
          { status: 200 }
        );
      }
      return new Response('{}', { status: 200 });
    });
    return releases;
  }

  /** A page whose user ID is unknown, so a peer-ID DM route is resolved over the API. */
  function createPageWithoutUserId(): any {
    const el = createPage();
    el.pageData = {};
    return el;
  }

  it('a second lookup for a DM that is already open leaves the panel alone', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    // The first parse and the rail-loaded re-parse each start a lookup.
    // Back to back they would share one DM-list request; forget it in
    // between so the second answers on its own, as it does once the shared
    // list has aged out.
    el.parseV2Route();
    chatDMsLoad.invalidate();
    el.parseV2Route();
    await flush();
    expect(releases).toHaveLength(2);

    releases[0]();
    await flush();
    expect(el.v2Conversation.conversationKey).toBe('dm:agent:agent-1:user:user-me');
    expect(el.mobilePanel).toBe('center');
    el.mobilePanel = 'left';

    releases[1]();
    await flush();

    expect(el.mobilePanel).toBe('left');
  });

  it('the first parse and the rail-loaded re-parse share one DM-list request and open the DM once', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    const titles: string[] = [];
    el.addEventListener(PAGE_TITLE_EVENT, (e: Event) =>
      titles.push(((e as CustomEvent).detail.segments as string[])[0])
    );
    el.parseV2Route();
    el.parseV2Route();
    await flush();
    expect(releases).toHaveLength(1);

    releases[0]();
    await flush();

    expect(el.v2Conversation.conversationKey).toBe('dm:agent:agent-1:user:user-me');
    expect(el.mobilePanel).toBe('center');
    // Opened by one lookup; the other found it already open and left it.
    expect(titles.filter((t) => t === 'Coder One')).toHaveLength(1);
  });

  it('a lookup that returns after the route moved on opens nothing', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    el.parseV2Route();
    await flush();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();

    releases[0]();
    await flush();

    expect(el.v2Conversation).toBeNull();
    expect(el.mobilePanel).toBe('left');
  });

  it('a DM opened from the members list without a user ID gets its own URL', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    await flush();

    releases[0]();
    await flush();

    expect(window.location.pathname).toBe(
      `/chat/dm/${encodeURIComponent('dm:agent:agent-1:user:user-me')}`
    );
    // The rail's next reload re-parses the route; the DM must stay open.
    el.parseV2Route();
    expect(el.v2Conversation).toMatchObject({ conversationKey: 'dm:agent:agent-1:user:user-me' });
    expect(el.mobilePanel).toBe('center');
  });

  it('a DM opened without a user ID is dropped if the user moves on first', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    await flush();
    el.navigateToThread({
      conversationKey: 'topic-2',
      projectId: 'p1',
      projectSlug: 'alpha',
      threadName: 'two',
    });

    releases[0]();
    await flush();

    expect(el.v2Conversation.conversationKey).toBe('topic-2');
    expect(window.location.pathname).toBe('/chat/alpha/topic-2');
  });

  it('of two DMs opened quickly without a user ID, the last one opened wins', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    el.openDM('agent-2', 'agent', 'Review Bot');
    await flush();
    expect(releases).toHaveLength(2);

    releases[0]();
    await flush();
    releases[1]();
    await flush();

    expect(el.v2Conversation).toMatchObject({ conversationKey: 'dm:agent:agent-2:user:user-me' });
    expect(window.location.pathname).toBe(
      `/chat/dm/${encodeURIComponent('dm:agent:agent-2:user:user-me')}`
    );
  });

  it('of two DMs opened quickly, the last one opened wins even if its answer comes first', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    const historyBefore = window.history.length;
    el.openDM('agent-1', 'agent', 'Coder One');
    el.openDM('agent-2', 'agent', 'Review Bot');
    await flush();
    expect(releases).toHaveLength(2);

    releases[1]();
    await flush();
    releases[0]();
    await flush();

    expect(el.v2Conversation).toMatchObject({ conversationKey: 'dm:agent:agent-2:user:user-me' });
    expect(window.location.pathname).toBe(
      `/chat/dm/${encodeURIComponent('dm:agent:agent-2:user:user-me')}`
    );
    expect(window.history.length).toBe(historyBefore + 1);
  });

  it('a DM opened without a user ID is dropped if the router replaces the page first', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    document.body.appendChild(el);
    // Connecting fires a DM-list request of its own (the unread dots), but
    // only once the lazy rail and members imports resolve, which can take
    // longer than one flush on a slow runner. Those imports resolving is
    // what issues it, so wait for that before taking the baseline.
    await vi.waitFor(() => expect(el.v2SpaceRailLoaded).toBe(true), { timeout: 5000 });
    await flush();
    const pending = releases.length;
    el.openDM('agent-1', 'agent', 'Coder One');
    await flush();
    expect(releases).toHaveLength(pending + 1);
    // The router navigates elsewhere and removes this page.
    window.history.pushState({}, '', '/chat/alpha/topic-2');
    el.remove();
    const historyBefore = window.history.length;

    releases.forEach((release) => release());
    await flush();

    expect(window.location.pathname).toBe('/chat/alpha/topic-2');
    expect(window.history.length).toBe(historyBefore);
  });

  it('a DM opened without a user ID is dropped if the user promotes a thread first', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    el._projectIdToSlug.set('p1', 'alpha');
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    await flush();
    el.navigateToPromotedThread({ id: 'topic-9', projectId: 'p1', name: 'promoted' });

    releases[0]();
    await flush();

    expect(el.v2Conversation).toMatchObject({ conversationKey: 'topic-9', isDM: false });
    expect(window.location.pathname).toBe('/chat/alpha/topic-9');
  });

  it('a DM opened without a user ID is dropped if the user resets the view first', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    await flush();
    el.handleResetView();

    releases[0]();
    await flush();

    expect(el.v2Conversation).toBeNull();
    expect(window.location.pathname).toBe('/chat');
  });

  it('a lookup still opens a cold-loaded DM', async () => {
    const releases = holdDMLists();
    const el = createPageWithoutUserId();
    window.history.replaceState({}, '', '/chat/dm/agent-1');
    el.parseV2Route();
    await flush();

    releases[0]();
    await flush();

    expect(el.v2Conversation).toMatchObject({
      conversationKey: 'dm:agent:agent-1:user:user-me',
      peerName: 'Coder One',
    });
    expect(el.mobilePanel).toBe('center');
  });

  it('a lookup overtaken during the user refresh builds no key and logs no error', async () => {
    // The DM list has no match, so the lookup falls back to fetching the
    // user, and that request is held until released.
    let releaseMe: () => void = () => {};
    vi.mocked(apiFetch).mockImplementation(async (path: string) => {
      if (path === '/api/v1/chat/dms') {
        return new Response(JSON.stringify({ dms: [] }), { status: 200 });
      }
      if (path === '/api/v1/auth/me') {
        await new Promise<void>((resolve) => (releaseMe = resolve));
        // No ID, so a key could not be built had the lookup gone on.
        return new Response('{}', { status: 200 });
      }
      return new Response('{}', { status: 200 });
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const el = createPageWithoutUserId();
    const buildKey = vi.spyOn(el, 'buildDMKey');
    window.history.replaceState({}, '', '/chat');
    el.parseV2Route();
    el.openDM('agent-1', 'agent', 'Coder One');
    // openDM tries the key once itself before it starts the lookup.
    buildKey.mockClear();
    await flush();
    el.navigateToThread({
      conversationKey: 'topic-2',
      projectId: 'p1',
      projectSlug: 'alpha',
      threadName: 'two',
    });

    releaseMe();
    await flush();

    expect(buildKey).not.toHaveBeenCalled();
    expect(errorSpy).not.toHaveBeenCalled();
    expect(el.v2Conversation.conversationKey).toBe('topic-2');
    errorSpy.mockRestore();
  });
});

describe('chat page — startup after the page is removed', () => {
  /**
   * Load initV2's lazy modules up front. Its own imports then come from the
   * module cache, but initV2 still resumes only after an await, so a page
   * removed in the same task as it was connected is removed first.
   */
  async function loadLazyModules(): Promise<void> {
    await Promise.all([
      import('../shared/chat/chat-space-rail.js'),
      import('../shared/chat/chat-members.js'),
    ]);
  }

  /**
   * A page that never renders: these tests are about initV2's side effects,
   * and happy-dom mishandles the members element a disconnected page renders
   * (it calls attribute callbacks on the never-upgraded instance).
   */
  function createUnrenderedPage(): any {
    const el = createPage();
    el.shouldUpdate = () => false;
    return el;
  }

  function dmListLoads(): number {
    return vi.mocked(apiFetch).mock.calls.filter(([path]) => path === '/api/v1/chat/dms').length;
  }

  type IntervalHandle = ReturnType<typeof setInterval>;

  /**
   * Spy on setInterval and clearInterval, so a test can tell which intervals
   * it started and whether each was cleared. Intervals are told apart by
   * handle, not by delay: the presence heartbeat shares the poll's delay.
   */
  function trackIntervals(): { live: () => IntervalHandle[] } {
    const setSpy = vi.spyOn(globalThis, 'setInterval');
    const clearSpy = vi.spyOn(globalThis, 'clearInterval');
    return {
      live: () => {
        const cleared = new Set(clearSpy.mock.calls.map(([h]) => h));
        return setSpy.mock.results
          .map((r) => r.value as IntervalHandle)
          .filter((h) => !cleared.has(h));
      },
    };
  }

  it('a page removed before its lazy imports resolve loads nothing and starts no poll', async () => {
    vi.mocked(apiFetch).mockClear();
    const intervals = trackIntervals();
    await loadLazyModules();
    const el = createUnrenderedPage();
    window.history.replaceState({}, '', '/chat');
    document.body.appendChild(el);
    // The router replaces the page before initV2's imports come back.
    el.remove();

    await flush();

    expect(dmListLoads()).toBe(0);
    expect(el._fallbackPollInterval).toBeNull();
    expect(intervals.live()).toEqual([]);
    expect(el.v2SpaceRailLoaded).toBe(false);
  });

  it('a page removed and connected again before its imports resolve initialises once', async () => {
    vi.mocked(apiFetch).mockClear();
    const intervals = trackIntervals();
    await loadLazyModules();
    const el = createUnrenderedPage();
    window.history.replaceState({}, '', '/chat');
    try {
      document.body.appendChild(el);
      el.remove();
      document.body.appendChild(el);

      await flush();

      expect(el.v2SpaceRailLoaded).toBe(true);
      expect(dmListLoads()).toBe(1);
      // Exactly one live interval, and it is the connected page's poll: the
      // first initV2 started nothing its disconnect could no longer clear.
      expect(el._fallbackPollInterval).not.toBeNull();
      expect(intervals.live()).toEqual([el._fallbackPollInterval]);

      el.remove();
      expect(intervals.live()).toEqual([]);
    } finally {
      el.remove();
    }
  });

  describe('when a lazy import fails', () => {
    const chunkError = new Error('Failed to fetch dynamically imported module');
    let unhandled: unknown[];
    const onUnhandled = (reason: unknown) => unhandled.push(reason);

    beforeEach(async () => {
      // Load the real modules first, so these tests time the same whether
      // or not an earlier test already did: only the mocked import differs.
      await loadLazyModules();
      unhandled = [];
      process.on('unhandledRejection', onUnhandled);
      // A deploy purged the old chunk: the members import rejects.
      vi.doMock('../shared/chat/chat-members.js', () => {
        throw chunkError;
      });
    });

    afterEach(() => {
      vi.doUnmock('../shared/chat/chat-members.js');
      process.off('unhandledRejection', onUnhandled);
    });

    it('logs the error, starts nothing, and flags the rail as failed', async () => {
      vi.mocked(apiFetch).mockClear();
      const intervals = trackIntervals();
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const el = createUnrenderedPage();
      window.history.replaceState({}, '', '/chat');
      try {
        document.body.appendChild(el);

        await flush();

        expect(unhandled).toEqual([]);
        expect(errorSpy).toHaveBeenCalledWith(
          'Chat page failed to load its components:',
          expect.anything()
        );
        expect(el.v2SpaceRailLoaded).toBe(false);
        expect(el.v2SpaceRailLoadFailed).toBe(true);
        expect(dmListLoads()).toBe(0);
        expect(el._fallbackPollInterval).toBeNull();
        expect(intervals.live()).toEqual([]);
      } finally {
        el.remove();
        errorSpy.mockRestore();
      }
    });

    it('on a page removed meanwhile, logs the error and changes nothing', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const el = createUnrenderedPage();
      window.history.replaceState({}, '', '/chat');
      try {
        document.body.appendChild(el);
        el.remove();

        await flush();

        expect(unhandled).toEqual([]);
        expect(errorSpy).toHaveBeenCalledWith(
          'Chat page failed to load its components:',
          expect.anything()
        );
        expect(el.v2SpaceRailLoadFailed).toBe(false);
        expect(el.v2SpaceRailLoaded).toBe(false);
      } finally {
        errorSpy.mockRestore();
      }
    });

    it('a connected page renders a reload message in the rail', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      // Renders the rail's template, not the whole page: happy-dom breaks
      // on the members element once its module is defined (see above).
      const el = createUnrenderedPage();
      window.history.replaceState({}, '', '/chat');
      try {
        document.body.appendChild(el);
        await flush();

        const rail = renderToFragment(el.renderV2Rail());
        const alert = rail.querySelector('[role="alert"]');
        expect(alert).not.toBeNull();
        expect(alert?.textContent).toContain('Reload the page to try again.');
        expect(rail.querySelector('sl-spinner')).toBeNull();
      } finally {
        el.remove();
        errorSpy.mockRestore();
      }
    });

    it('clears the failure once a later startup loads its imports', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const el = createUnrenderedPage();
      window.history.replaceState({}, '', '/chat');
      try {
        document.body.appendChild(el);
        await vi.waitFor(() => expect(el.v2SpaceRailLoadFailed).toBe(true));

        // The page is removed and added again, and this time the chunk loads.
        el.remove();
        vi.doUnmock('../shared/chat/chat-members.js');
        await loadLazyModules();
        document.body.appendChild(el);
        await vi.waitFor(() => expect(el.v2SpaceRailLoaded).toBe(true));

        expect(unhandled).toEqual([]);
        expect(el.v2SpaceRailLoadFailed).toBe(false);
      } finally {
        el.remove();
        errorSpy.mockRestore();
      }
    });
  });
});
