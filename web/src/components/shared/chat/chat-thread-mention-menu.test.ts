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
 * Context menu on highlighted `@agent` mentions inside chat messages
 * (ptone/scion#8419): right-click, touch long-press, text-selection
 * preservation, keyboard accessibility, and parity with the agent actions
 * on a message from that agent.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { agentGraphHref, terminalHref } from '../../../client/open-terminal.js';
import type { ChatAgentMember } from './chat-members.js';
import { LONG_PRESS_MS } from './long-press.js';

const navigateToMock = vi.fn();
const fakeStateManager = new (class extends EventTarget {
  currentScope = null;
  getAgent(): undefined {
    return undefined;
  }
})();
const apiFetch = vi.fn();

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  get navigateTo(): typeof navigateToMock {
    return navigateToMock;
  },
  get stateManager(): typeof fakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string): Promise<string> => Promise.resolve(fallback),
}));

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

const AGENT_ACTION_LABELS = new Set([
  'Make this agent thread default',
  'Open terminal',
  'Open in graph',
]);

const LEAD_AGENT: ChatAgentMember = {
  id: 'agent-lead',
  kind: 'agent',
  displayName: 'Lead Coordinator',
  slug: 'lead',
  canAttach: false,
  projectId: 'proj-lead',
};

const CODER_AGENT: ChatAgentMember = {
  id: 'agent-coder',
  kind: 'agent',
  displayName: 'Code Writer',
  slug: 'coder',
  canAttach: true,
  projectId: 'proj-coder',
};

let sheet = false;
let touch = false;

beforeEach(() => {
  sheet = false;
  touch = false;
  apiFetch.mockReset();
  navigateToMock.mockReset();
  window.getSelection()?.removeAllRanges();
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query: string) =>
      ({
        matches: query.includes('max-width')
          ? sheet
          : query.includes('hover: none')
            ? touch
            : false,
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
      }) as unknown as MediaQueryList
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
  window.getSelection()?.removeAllRanges();
  document.body.innerHTML = '';
});

interface MountOptions {
  messages: Array<Record<string, unknown>>;
  agentMembers?: ChatAgentMember[];
  members?: Array<{
    id: string;
    name: string;
    email: string;
    kind: 'user' | 'agent';
    slug?: string;
  }>;
  isDM?: boolean;
  projectId?: string;
  defaultAgent?: string;
}

async function mountThread(opts: MountOptions): Promise<ScionChatThread> {
  apiFetch.mockImplementation((path: unknown, init?: RequestInit) => {
    const p = String(path);
    if (init?.method === 'PATCH' && p.includes('/api/v1/chat/topics/')) {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({}),
      } as unknown as Response);
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ messages: opts.messages }),
    } as unknown as Response);
  });

  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = opts.isDM ? 'dm:agent:agent-coder:user:user-me' : 'topic-1';
  el.currentUserId = 'user-me';
  el.isDM = opts.isDM ?? false;
  el.projectId = opts.projectId ?? 'proj-thread';
  el.defaultAgent = opts.defaultAgent ?? '';
  el.agentMembers = opts.agentMembers ?? [LEAD_AGENT, CODER_AGENT];
  el.members = opts.members ?? [
    { id: 'user-alice', name: 'Alice Smith', email: 'alice@example.com', kind: 'user' },
    { id: 'agent-lead', name: 'Lead Coordinator', email: '', kind: 'agent', slug: 'lead' },
    { id: 'agent-coder', name: 'Code Writer', email: '', kind: 'agent', slug: 'coder' },
  ];
  document.body.appendChild(el);

  await vi.waitFor(() => {
    expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(opts.messages.length);
  });
  for (const m of opts.messages) {
    const id = String(m.id);
    if (m.msg && !m.plain) {
      await vi.waitFor(() => {
        expect(messageBody(el, id)).not.toBeNull();
      });
    }
  }
  return el;
}

function messageHost(el: ScionChatThread, id: string): HTMLElement {
  return el.shadowRoot!.querySelector<HTMLElement>(`#msg-${id}`)!;
}

function messageBody(el: ScionChatThread, id: string): HTMLElement | null {
  return messageHost(el, id)?.shadowRoot?.querySelector<HTMLElement>('.md-content') ?? null;
}

function mentionSpan(el: ScionChatThread, msgId: string, slug: string): HTMLElement {
  const span = messageBody(el, msgId)?.querySelector<HTMLElement>(
    `.mention[data-mention="${slug}"]`
  );
  if (!span) throw new Error(`mention @${slug} not found in msg-${msgId}`);
  return span;
}

async function rightClick(
  el: ScionChatThread,
  target: Element,
  init: MouseEventInit = {},
  betweenPressAndMenu?: () => void
): Promise<MouseEvent> {
  const clientX = init.clientX ?? 40;
  const clientY = init.clientY ?? 60;
  // Keyboard-synthesized contextmenu at (0, 0) has no preceding pointer press.
  if (!(clientX === 0 && clientY === 0)) {
    target.dispatchEvent(
      new PointerEvent('pointerdown', {
        bubbles: true,
        composed: true,
        cancelable: true,
        button: 2,
        buttons: 2,
        pointerType: 'mouse',
        isPrimary: true,
        clientX,
        clientY,
      })
    );
  }
  betweenPressAndMenu?.();
  const e = new MouseEvent('contextmenu', {
    bubbles: true,
    composed: true,
    cancelable: true,
    button: 2,
    clientX,
    clientY,
    ...init,
  });
  target.dispatchEvent(e);
  await el.updateComplete;
  await el.updateComplete;
  return e;
}

function popupItems(el: ScionChatThread): HTMLElement[] {
  return [...el.shadowRoot!.querySelectorAll<HTMLElement>('.context-menu-item')];
}

function itemLabels(items: HTMLElement[]): string[] {
  return items.map((i) => i.textContent?.replace(/\s+/g, ' ').trim() ?? '');
}

function itemSignature(items: HTMLElement[]): Array<{ label: string; icon: string | null }> {
  return items.map((i) => ({
    label: i.textContent?.replace(/\s+/g, ' ').trim() ?? '',
    icon: i.querySelector('sl-icon')?.getAttribute('name') ?? null,
  }));
}

describe('scion-chat-thread highlighted @agent mention context menu (ptone/scion#8419)', () => {
  it('opens the same agent menu items on a highlighted @agent mention as on a message from that agent', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm-user',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Please check with @coder on this.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
        {
          id: 'm-coder',
          sender: 'agent:coder',
          senderId: 'agent-coder',
          projectId: 'proj-coder',
          msg: 'On it.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:01:00Z',
        },
      ],
    });

    // 1. Open menu on the message from agent:coder and record its agent items.
    await rightClick(el, messageHost(el, 'm-coder'));
    const fromMessageAgentItems = itemSignature(popupItems(el)).filter((item) =>
      AGENT_ACTION_LABELS.has(item.label)
    );
    expect(fromMessageAgentItems).toEqual([
      { label: 'Make this agent thread default', icon: 'robot' },
      { label: 'Open terminal', icon: 'terminal' },
      { label: 'Open in graph', icon: 'diagram-3' },
    ]);

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    // 2. Right-click @coder inside Alice's message: opens the agent menu for coder,
    // matching the agent items from coder's message.
    const mention = mentionSpan(el, 'm-user', 'coder');
    const ev = await rightClick(el, mention);
    expect(ev.defaultPrevented).toBe(true);

    const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]');
    expect(menu).not.toBeNull();
    expect(menu?.getAttribute('aria-label')).toBe('Agent actions, Code Writer');
    expect(itemSignature(popupItems(el))).toEqual(fromMessageAgentItems);
  });

  it('targets the mentioned agent rather than the message author when another agent mentions @coder', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm-lead',
          sender: 'agent:lead',
          senderId: 'agent-lead',
          projectId: 'proj-lead',
          msg: 'Handing off to @coder now.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });

    // Lead has canAttach: false, whereas Coder has canAttach: true and projectId: 'proj-coder'.
    await rightClick(el, mentionSpan(el, 'm-lead', 'coder'));
    expect(itemLabels(popupItems(el))).toEqual([
      'Make this agent thread default',
      'Open terminal',
      'Open in graph',
    ]);

    const navClick = vi.fn();
    document.addEventListener('nav-click', navClick);
    try {
      popupItems(el)
        .find((i) => i.textContent?.includes('Open terminal'))!
        .click();
    } finally {
      document.removeEventListener('nav-click', navClick);
    }
    expect(navClick).toHaveBeenCalledTimes(1);
    expect((navClick.mock.calls[0][0] as CustomEvent<{ path: string }>).detail.path).toBe(
      terminalHref('agent-coder')
    );

    // Reopen and verify "Open in graph" and "Make this agent thread default" target coder.
    await rightClick(el, mentionSpan(el, 'm-lead', 'coder'));
    popupItems(el)
      .find((i) => i.textContent?.includes('Open in graph'))!
      .click();
    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-coder', 'agent-coder'));

    const defaultChanged = vi.fn();
    el.addEventListener('default-agent-changed', defaultChanged);
    await rightClick(el, mentionSpan(el, 'm-lead', 'coder'));
    popupItems(el)
      .find((i) => i.textContent?.includes('Make this agent thread default'))!
      .click();
    await vi.waitFor(() => expect(defaultChanged).toHaveBeenCalledTimes(1));
    expect((defaultChanged.mock.calls[0][0] as CustomEvent).detail).toEqual({
      defaultAgent: 'coder',
    });

    // Once coder is the thread default, "Make this agent thread default" hides on @coder too.
    await rightClick(el, mentionSpan(el, 'm-lead', 'coder'));
    expect(itemLabels(popupItems(el))).toEqual(['Open terminal', 'Open in graph']);
  });

  it('resolves a highlighted mention written using the hyphenated displayName slug or mixed case', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Thanks @code-writer and @Coder!',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });

    await rightClick(el, mentionSpan(el, 'm1', 'code-writer'));
    expect(itemLabels(popupItems(el))).toEqual([
      'Make this agent thread default',
      'Open terminal',
      'Open in graph',
    ]);

    await rightClick(el, mentionSpan(el, 'm1', 'Coder'));
    expect(itemLabels(popupItems(el))).toEqual([
      'Make this agent thread default',
      'Open terminal',
      'Open in graph',
    ]);
  });

  it('preserves the browser context menu when an existing selection covers the clicked target', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Select `config.yaml` or @coder text here.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
        {
          id: 'm2',
          sender: 'agent:lead',
          senderId: 'agent-lead',
          projectId: 'proj-lead',
          msg: 'Follow up with @lead.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:01:00Z',
        },
      ],
    });
    const mention = mentionSpan(el, 'm1', 'coder');
    const codeEl = messageBody(el, 'm1')!.querySelector('code')!;
    const leadMention = mentionSpan(el, 'm2', 'lead');
    const sel = window.getSelection()!;

    // 1. Real DOM non-collapsed Range covering @coder suppresses both mention and message menus.
    const mentionRange = document.createRange();
    mentionRange.selectNodeContents(mention);
    sel.removeAllRanges();
    sel.addRange(mentionRange);

    const domSelectedEvent = await rightClick(el, mention);
    expect(domSelectedEvent.defaultPrevented).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();
    sel.removeAllRanges();

    // 2. Real DOM Range covering inline message text suppresses the message menu on that text.
    const codeRange = document.createRange();
    codeRange.selectNodeContents(codeEl);
    sel.addRange(codeRange);

    const codeSelectedEvent = await rightClick(el, codeEl);
    expect(codeSelectedEvent.defaultPrevented).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();
    sel.removeAllRanges();

    // 3. Shadow-DOM composed range (Chromium/WebKit/Gecko getComposedRanges where
    // window.getSelection().getRangeAt(0) is collapsed on document.body).
    const collapsedLightRange = document.createRange();
    collapsedLightRange.setStart(document.body, 0);
    collapsedLightRange.setEnd(document.body, 0);
    const mentionText = mention.firstChild!;
    const fakeComposed = {
      collapsed: false,
      startContainer: mentionText,
      startOffset: 0,
      endContainer: mentionText,
      endOffset: mentionText.textContent!.length,
    } as StaticRange;
    const spy = vi.spyOn(window, 'getSelection').mockReturnValue({
      type: 'Range',
      isCollapsed: true,
      rangeCount: 1,
      getRangeAt: () => collapsedLightRange,
      getComposedRanges: () => [fakeComposed],
      removeAllRanges: () => {},
    } as unknown as Selection);
    try {
      const shadowSelectedEvent = await rightClick(el, mention);
      expect(shadowSelectedEvent.defaultPrevented).toBe(false);
      expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();

      // The same composed range in m1 does NOT suppress @lead in m2.
      const otherMentionEvent = await rightClick(el, leadMention);
      expect(otherMentionEvent.defaultPrevented).toBe(true);
      expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
        'Agent actions, Lead Coordinator'
      );
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
      await el.updateComplete;
    } finally {
      spy.mockRestore();
    }

    // 4. Left-button drag-selection followed by right-click still preserves the browser menu.
    mention.dispatchEvent(
      new PointerEvent('pointerdown', {
        bubbles: true,
        composed: true,
        button: 0,
        buttons: 1,
        pointerType: 'mouse',
        isPrimary: true,
      })
    );
    const dragRange = document.createRange();
    dragRange.selectNodeContents(mention);
    sel.removeAllRanges();
    sel.addRange(dragRange);
    const afterDragRightClick = new MouseEvent('contextmenu', {
      bubbles: true,
      composed: true,
      cancelable: true,
      button: 2,
      clientX: 40,
      clientY: 60,
    });
    mention.dispatchEvent(afterDragRightClick);
    await el.updateComplete;
    expect(afterDragRightClick.defaultPrevented).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();
    sel.removeAllRanges();

    // 5. Once selection is cleared, right-click opens the agent menu again.
    const normalEvent = await rightClick(el, mention);
    expect(normalEvent.defaultPrevented).toBe(true);
    expect(itemLabels(popupItems(el))).toContain('Open terminal');
  });

  it('opens mention and message menus when a leftover selection is outside the clicked element', async () => {
    const outsideNote = document.createElement('p');
    outsideNote.textContent = 'Header text outside thread';
    document.body.appendChild(outsideNote);

    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'First message with `selected-token` and @coder.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
        {
          id: 'm2',
          sender: 'agent:coder',
          senderId: 'agent-coder',
          projectId: 'proj-coder',
          msg: 'Second message mentioning @lead.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:01:00Z',
        },
      ],
    });

    const sel = window.getSelection()!;
    const m1Code = messageBody(el, 'm1')!.querySelector('code')!;
    const m1Mention = mentionSpan(el, 'm1', 'coder');
    const m2Content = messageBody(el, 'm2')!;
    const m2Mention = mentionSpan(el, 'm2', 'lead');

    // 1. Selection in m1 does not block right-clicking @lead in m2 or m2's message menu.
    const m1Range = document.createRange();
    m1Range.selectNodeContents(m1Code);
    sel.removeAllRanges();
    sel.addRange(m1Range);

    const m2MentionEv = await rightClick(el, m2Mention);
    expect(m2MentionEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Agent actions, Lead Coordinator'
    );

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    const m2MsgEv = await rightClick(el, m2Content);
    expect(m2MsgEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Message actions, Code Writer'
    );

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    // 2. Selection on `selected-token` in m1 does not block right-clicking sibling @coder in m1.
    const siblingMentionEv = await rightClick(el, m1Mention);
    expect(siblingMentionEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Agent actions, Code Writer'
    );

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    // 3. Selection in light DOM outside the thread does not block either menu.
    const outsideRange = document.createRange();
    outsideRange.selectNodeContents(outsideNote);
    sel.removeAllRanges();
    sel.addRange(outsideRange);

    const lightMentionEv = await rightClick(el, m1Mention);
    expect(lightMentionEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Agent actions, Code Writer'
    );
  });

  it('opens mention and message menus when macOS selects the word between pointerdown and contextmenu', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm-user',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Right-click @coder on macOS.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
        {
          id: 'm-coder',
          sender: 'agent:coder',
          senderId: 'agent-coder',
          projectId: 'proj-coder',
          msg: 'Plain paragraph text on macOS.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:01:00Z',
        },
      ],
    });

    const sel = window.getSelection()!;
    const mention = mentionSpan(el, 'm-user', 'coder');
    const coderContent = messageBody(el, 'm-coder')!;

    // 1. Right-click @coder: nothing selected at pointerdown, browser selects @coder before contextmenu.
    const mentionEv = await rightClick(el, mention, {}, () => {
      const wordRange = document.createRange();
      wordRange.selectNodeContents(mention);
      sel.removeAllRanges();
      sel.addRange(wordRange);
    });
    expect(mentionEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Agent actions, Code Writer'
    );

    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    // 2. Right-click message text while the macOS auto-selected @coder is still selected in m-user,
    // and macOS then selects the word in m-coder before contextmenu.
    const msgEv = await rightClick(el, coderContent, {}, () => {
      const wordRange = document.createRange();
      wordRange.selectNodeContents(coderContent);
      sel.removeAllRanges();
      sel.addRange(wordRange);
    });
    expect(msgEv.defaultPrevented).toBe(true);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Message actions, Code Writer'
    );
  });

  it('is keyboard accessible: mention is focusable, ContextMenu and Shift+F10 open the menu, Escape restores focus', async () => {
    for (const [key, shiftKey] of [
      ['ContextMenu', false],
      ['F10', true],
    ] as const) {
      const el = await mountThread({
        messages: [
          {
            id: 'm1',
            sender: 'user:alice@example.com',
            senderId: 'user-alice',
            msg: 'Keyboard @coder test.',
            type: 'chat',
            createdAt: '2026-01-01T00:00:00Z',
          },
        ],
      });

      const msgHost = messageHost(el, 'm1');
      const mention = mentionSpan(el, 'm1', 'coder');
      expect(mention.getAttribute('tabindex')).toBe('0');
      expect(mention.getAttribute('role')).toBe('button');

      mention.focus();
      expect(msgHost.shadowRoot!.activeElement).toBe(mention);

      const kd = new KeyboardEvent('keydown', {
        key,
        shiftKey,
        bubbles: true,
        cancelable: true,
      });
      mention.dispatchEvent(kd);
      await el.updateComplete;
      await el.updateComplete;

      expect(kd.defaultPrevented).toBe(true);

      const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
      expect(menu).not.toBeNull();
      const items = popupItems(el);
      expect(el.shadowRoot!.activeElement).toBe(items[0]);

      // Escape closes the menu and restores focus to the mention span.
      menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      await el.updateComplete;

      expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();
      expect(msgHost.shadowRoot!.activeElement).toBe(mention);

      document.body.innerHTML = '';
    }
  });

  it('opens the mobile action sheet on touch long-press without triggering mention-click', async () => {
    vi.useFakeTimers();
    sheet = true;
    touch = true;

    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Touch @coder here.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });
    const mention = mentionSpan(el, 'm1', 'coder');
    const mentionClicks: string[] = [];
    const onMentionClick = (e: Event): void => {
      mentionClicks.push((e as CustomEvent<{ slug: string }>).detail.slug);
    };
    document.addEventListener('mention-click', onMentionClick);

    try {
      mention.dispatchEvent(
        new PointerEvent('pointerdown', {
          bubbles: true,
          composed: true,
          pointerId: 1,
          pointerType: 'touch',
          isPrimary: true,
          clientX: 30,
          clientY: 40,
        })
      );
      vi.advanceTimersByTime(LONG_PRESS_MS + 10);
      await el.updateComplete;

      const actionSheet = el.shadowRoot!.querySelector('scion-action-sheet') as HTMLElement & {
        open: boolean;
        heading: string;
        items: Array<{ id: string; label: string }>;
      };
      expect(actionSheet.open).toBe(true);
      expect(actionSheet.heading).toBe('Code Writer');
      expect(actionSheet.items.map((i) => i.label)).toEqual([
        'Make this agent thread default',
        'Open terminal',
        'Open in graph',
      ]);

      // Lifting finger + trailing click is swallowed so DM navigation does not fire.
      window.dispatchEvent(
        new PointerEvent('pointerup', {
          bubbles: true,
          pointerId: 1,
          pointerType: 'touch',
          isPrimary: true,
        })
      );
      mention.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
      expect(mentionClicks).toEqual([]);
    } finally {
      document.removeEventListener('mention-click', onMentionClick);
    }
  });

  it('handles Android touch long-press contextmenu (including native word selection during hold) without double-opening', async () => {
    vi.useFakeTimers();
    sheet = true;
    touch = true;

    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'user:alice@example.com',
          senderId: 'user-alice',
          msg: 'Android touch @coder here.',
          type: 'chat',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });
    const mention = mentionSpan(el, 'm1', 'coder');
    const sel = window.getSelection()!;
    const mentionClicks: string[] = [];
    const onMentionClick = (e: Event): void => {
      mentionClicks.push((e as CustomEvent<{ slug: string }>).detail.slug);
    };
    document.addEventListener('mention-click', onMentionClick);

    try {
      mention.dispatchEvent(
        new PointerEvent('pointerdown', {
          bubbles: true,
          composed: true,
          pointerId: 7,
          pointerType: 'touch',
          isPrimary: true,
          clientX: 30,
          clientY: 40,
        })
      );
      // Android selects the word under the finger during the hold before firing contextmenu.
      const range = document.createRange();
      range.selectNodeContents(mention);
      sel.removeAllRanges();
      sel.addRange(range);

      const touchCtx = new PointerEvent('contextmenu', {
        bubbles: true,
        composed: true,
        cancelable: true,
        pointerId: 7,
        pointerType: 'touch',
        clientX: 30,
        clientY: 40,
      });
      mention.dispatchEvent(touchCtx);
      await el.updateComplete;

      expect(touchCtx.defaultPrevented).toBe(true);
      const actionSheet = el.shadowRoot!.querySelector('scion-action-sheet') as HTMLElement & {
        open: boolean;
        heading: string;
      };
      expect(actionSheet.open).toBe(true);
      expect(actionSheet.heading).toBe('Code Writer');

      // Advancing past LONG_PRESS_MS does not fire a second open after contextMenu() won.
      vi.advanceTimersByTime(LONG_PRESS_MS + 50);
      await el.updateComplete;
      expect(actionSheet.open).toBe(true);

      window.dispatchEvent(
        new PointerEvent('pointerup', {
          bubbles: true,
          pointerId: 7,
          pointerType: 'touch',
          isPrimary: true,
        })
      );
      mention.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
      expect(mentionClicks).toEqual([]);
    } finally {
      document.removeEventListener('mention-click', onMentionClick);
    }
  });

  it('keeps non-agent user mentions and unknown mentions on their current behavior', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'agent:coder',
          senderId: 'agent-coder',
          projectId: 'proj-coder',
          msg: 'Ping @alice-smith and @nobody about this.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });

    const aliceMention = mentionSpan(el, 'm1', 'alice-smith');
    // Shift+F10 on a human mention does not open an agent menu.
    const kd = new KeyboardEvent('keydown', {
      key: 'F10',
      shiftKey: true,
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    aliceMention.dispatchEvent(kd);
    await el.updateComplete;
    expect(kd.defaultPrevented).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="menu"]')).toBeNull();

    // Right-clicking @alice-smith falls through to the message context menu (not an agent-only mention menu for Alice).
    await rightClick(el, aliceMention);
    const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
    expect(menu.getAttribute('aria-label')).toMatch(/^Message actions/);
    expect(itemLabels(popupItems(el))).toContain('Reply');
  });

  it('switches cleanly between the mention agent menu and the message menu without rebuilding rows', async () => {
    const el = await mountThread({
      messages: [
        {
          id: 'm1',
          sender: 'agent:lead',
          senderId: 'agent-lead',
          projectId: 'proj-lead',
          msg: 'Coordinate with @coder.',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    });

    const renderRowsSpy = vi.spyOn(
      el as unknown as { renderMessages: () => unknown },
      'renderMessages'
    );
    const mention = mentionSpan(el, 'm1', 'coder');

    // Keyboard-synthesized contextmenu at (0, 0) also opens the mention menu anchored to mention.
    mention.focus();
    await rightClick(el, mention, { clientX: 0, clientY: 0 });
    expect(mention.hasAttribute('aria-expanded')).toBe(false);
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Agent actions, Code Writer'
    );

    // Right-clicking the message bubble outside the mention switches to the message menu.
    await rightClick(el, messageHost(el, 'm1'));
    expect(el.shadowRoot!.querySelector('[role="menu"]')?.getAttribute('aria-label')).toBe(
      'Message actions, Lead Coordinator'
    );
    expect(renderRowsSpy).not.toHaveBeenCalled();
  });
});
