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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { render, type TemplateResult } from 'lit';
import { apiFetch } from '../../client/api.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => ({
  navigateTo: vi.fn(),
  stateManager: new EventTarget(),
}));

vi.mock('../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));

let ScionPageChat: any;

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

/** Drive one touch gesture through the page's swipe handlers. */
function swipe(el: any, opts: { dx: number; dy?: number; durationMs?: number }): void {
  const dy = opts.dy ?? 0;
  const start = 200;
  const now = Date.now();
  vi.setSystemTime(now);

  el.handleTouchStart({ touches: [{ clientX: start, clientY: 100 }] });
  el.handleTouchMove({ touches: [{ clientX: start + opts.dx, clientY: 100 + dy }] });
  vi.setSystemTime(now + (opts.durationMs ?? 100));
  el.handleTouchEnd({ changedTouches: [{ clientX: start + opts.dx, clientY: 100 + dy }] });
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
    const back = renderToFragment(el.renderMobileBackButton('center')).querySelector('.mobile-back');

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
  beforeAll(() => {
    (window as any).innerWidth = 400;
  });

  afterEach(() => {
    (window as any).innerWidth = 400;
  });

  it('swipes right from the conversation to the rail, and back left', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();

    swipe(el, { dx: 120 });
    expect(el.mobilePanel).toBe('left');

    swipe(el, { dx: -120 });
    expect(el.mobilePanel).toBe('center');
  });

  it('swipes left from the conversation to the members panel, and back right', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();

    swipe(el, { dx: -120 });
    expect(el.mobilePanel).toBe('right');

    swipe(el, { dx: 120 });
    expect(el.mobilePanel).toBe('center');
  });

  it('does not run past the outermost panels', () => {
    vi.useFakeTimers();
    const el = createPage();
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
    swipe(flick, { dx: 60, durationMs: 150 });
    expect(flick.mobilePanel).toBe('left');

    const slow = createPageOnConversation();
    swipe(slow, { dx: 60, durationMs: 900 });
    expect(slow.mobilePanel).toBe('center');
  });

  it('ignores a mostly vertical drag — that is the message list scrolling', () => {
    vi.useFakeTimers();
    const el = createPageOnConversation();

    swipe(el, { dx: 120, dy: 200 });

    expect(el.mobilePanel).toBe('center');
  });

  it('ignores swipes on desktop viewports', () => {
    vi.useFakeTimers();
    (window as any).innerWidth = 1400;
    const el = createPageOnConversation();

    swipe(el, { dx: 200 });

    expect(el.mobilePanel).toBe('center');
  });
});
