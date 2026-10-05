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
 * Tests for the project-context helpers used by the dashboard ↔ chat mode
 * switch in scion-header. These are pure functions — no DOM needed.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

// Mounting <scion-header> also mounts its tray children
// (<scion-inbox-tray>/<scion-notification-tray>), which fetch on connect —
// stubbed here so the DOM-mounting tests below don't make real network
// calls. Mocked by resolved path, so this also covers the trays' own import
// of the same module.
vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

import {
  projectIdFromDashboardPath,
  projectIdFromChatSpacePath,
  slugFromChatPath,
  isMacPlatform,
  type ScionHeader,
} from './header.js';
import { TOUCH_PRIMARY_QUERY } from '../../utils/input-modality.js';
import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import { TERMINAL_PALETTE_OPEN_REQUEST_EVENT } from '../../client/terminal-palette-events.js';
import {
  GRAPH_PALETTE_AVAILABILITY_EVENT,
  GRAPH_PALETTE_OPEN_REQUEST_EVENT,
  setGraphPaletteAvailable,
} from '../../client/graph-palette-events.js';
import type { User } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';

describe('projectIdFromDashboardPath', () => {
  it('extracts the project ID from /projects/:id', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/settings', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/settings')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/schedules', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/schedules')).toBe('abc-123');
  });

  it('extracts the project ID from /projects/:id/metrics', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123/metrics')).toBe('abc-123');
  });

  it('returns null for /projects/new (creation form)', () => {
    expect(projectIdFromDashboardPath('/projects/new')).toBeNull();
  });

  it('returns null for /projects (list page)', () => {
    expect(projectIdFromDashboardPath('/projects')).toBeNull();
  });

  it('returns null for the root path', () => {
    expect(projectIdFromDashboardPath('/')).toBeNull();
  });

  it('returns null for unrelated paths', () => {
    expect(projectIdFromDashboardPath('/agents/foo')).toBeNull();
    expect(projectIdFromDashboardPath('/chat')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(projectIdFromDashboardPath('/projects/abc-123?foo=bar')).toBe('abc-123');
    expect(projectIdFromDashboardPath('/projects/abc-123#hash')).toBe('abc-123');
  });
});

describe('projectIdFromChatSpacePath', () => {
  it('extracts project ID from /chat/space/:id', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42')).toBe('proj-42');
  });

  it('extracts project ID from /chat/space/:id/thread/:tid', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42/thread/topic-7')).toBe('proj-42');
  });

  it('returns null for /chat/:slug paths', () => {
    expect(projectIdFromChatSpacePath('/chat/my-project')).toBeNull();
  });

  it('returns null for /chat/dm paths', () => {
    expect(projectIdFromChatSpacePath('/chat/dm/abc')).toBeNull();
  });

  it('returns null for bare /chat', () => {
    expect(projectIdFromChatSpacePath('/chat')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(projectIdFromChatSpacePath('/chat/space/proj-42?thread=topic-7')).toBe('proj-42');
    expect(projectIdFromChatSpacePath('/chat/space/proj-42#hash')).toBe('proj-42');
  });
});

describe('slugFromChatPath', () => {
  it('extracts the slug from /chat/:slug', () => {
    expect(slugFromChatPath('/chat/my-project')).toBe('my-project');
  });

  it('extracts the slug from /chat/:slug/:threadId', () => {
    expect(slugFromChatPath('/chat/my-project/thread-1')).toBe('my-project');
  });

  it('returns null for /chat/space/:id (handled by projectIdFromChatSpacePath)', () => {
    expect(slugFromChatPath('/chat/space/proj-42')).toBeNull();
  });

  it('returns null for /chat/dm/:key (DMs have no project context)', () => {
    expect(slugFromChatPath('/chat/dm/abc')).toBeNull();
  });

  it('returns null for bare /chat', () => {
    expect(slugFromChatPath('/chat')).toBeNull();
  });

  it('returns null for non-chat paths', () => {
    expect(slugFromChatPath('/')).toBeNull();
    expect(slugFromChatPath('/projects/abc')).toBeNull();
  });

  it('ignores query parameters and hash fragments', () => {
    expect(slugFromChatPath('/chat/my-project?foo=bar')).toBe('my-project');
    expect(slugFromChatPath('/chat/my-project#hash')).toBe('my-project');
  });
});

describe('isMacPlatform', () => {
  const originalPlatform = Object.getOwnPropertyDescriptor(window.navigator, 'platform');
  const originalUAData = Object.getOwnPropertyDescriptor(window.navigator, 'userAgentData');

  afterEach(() => {
    if (originalPlatform) {
      Object.defineProperty(window.navigator, 'platform', originalPlatform);
    }
    if (originalUAData) {
      Object.defineProperty(window.navigator, 'userAgentData', originalUAData);
    } else {
      delete (window.navigator as unknown as Record<string, unknown>).userAgentData;
    }
  });

  function setPlatform(platform: string): void {
    Object.defineProperty(window.navigator, 'platform', { value: platform, configurable: true });
  }

  function setUserAgentDataPlatform(platform: string | undefined): void {
    Object.defineProperty(window.navigator, 'userAgentData', {
      value: platform === undefined ? undefined : { platform },
      configurable: true,
    });
  }

  it('without Client Hints, falls back to the navigator.platform regex', () => {
    setUserAgentDataPlatform(undefined);
    setPlatform('MacIntel');
    expect(isMacPlatform()).toBe(true);

    setPlatform('Linux x86_64');
    expect(isMacPlatform()).toBe(false);
  });

  it('prefers navigator.userAgentData.platform over navigator.platform when both are present', () => {
    // navigator.platform disagrees on purpose, to prove Client Hints wins.
    setPlatform('Linux x86_64');
    setUserAgentDataPlatform('macOS');
    expect(isMacPlatform()).toBe(true);

    setPlatform('MacIntel');
    setUserAgentDataPlatform('Windows');
    expect(isMacPlatform()).toBe(false);
  });

  it('returns false, not throw, when navigator is unavailable', () => {
    vi.stubGlobal('navigator', undefined);
    expect(isMacPlatform()).toBe(false);
    vi.unstubAllGlobals();
  });
});

// ---------------------------------------------------------------------------
// The header palette button: render conditions, click dispatch, and the
// touch-modality-dependent tooltip/aria-keyshortcuts affordances.
// ---------------------------------------------------------------------------

const TEST_USER: User = { id: 'u1', email: 'u1@example.com', name: 'User One' };

/** Stubs `window.matchMedia` so `TouchPrimaryController` sees a fixed touch/desktop result. Must be called before the header is connected (the controller reads it in `hostConnected`). */
function stubTouchPrimary(isTouch: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: query === TOUCH_PRIMARY_QUERY && isTouch,
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    }))
  );
}

async function mountHeader(
  overrides: Partial<{ user: User | null; currentPath: string }> = {}
): Promise<ScionHeader> {
  if (!vi.isMockFunction(window.matchMedia)) stubTouchPrimary(false);
  const el = document.createElement('scion-header');
  el.user = 'user' in overrides ? overrides.user! : TEST_USER;
  el.currentPath = overrides.currentPath ?? '/chat';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function paletteButton(el: ScionHeader): Element | null | undefined {
  return el.shadowRoot?.querySelector('.palette-button');
}

function paletteTooltip(el: ScionHeader): Element | null | undefined {
  return paletteButton(el)?.closest('sl-tooltip');
}

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
});

describe('palette button: render conditions (chat route x user)', () => {
  it('renders when signed in, on a chat route', async () => {
    const el = await mountHeader();
    expect(paletteButton(el)).not.toBeNull();
  });

  it('renders on a DM route and a thread route, not just bare /chat', async () => {
    let el = await mountHeader({ currentPath: '/chat/dm/some-key' });
    expect(paletteButton(el)).not.toBeNull();
    el.remove();

    el = await mountHeader({ currentPath: '/chat/my-project/thread-123' });
    expect(paletteButton(el)).not.toBeNull();
  });

  it('is absent on a non-chat route', async () => {
    const el = await mountHeader({ currentPath: '/projects/abc' });
    expect(paletteButton(el)).toBeNull();
  });

  it('is absent when no user is signed in', async () => {
    const el = await mountHeader({ user: null });
    expect(paletteButton(el)).toBeNull();
  });

  it('renders on /terminals and a per-agent terminal route, labeled "Open Jump to agent"', async () => {
    let el = await mountHeader({ currentPath: '/terminals' });
    let button = paletteButton(el);
    expect(button).not.toBeNull();
    expect(button?.getAttribute('aria-label')).toBe('Open Jump to agent');
    el.remove();

    el = await mountHeader({ currentPath: '/terminals/00000000-0000-0000-0000-000000000001' });
    button = paletteButton(el);
    expect(button).not.toBeNull();
    expect(button?.getAttribute('aria-label')).toBe('Open Jump to agent');
  });

  it('renders on the multi-pane URL form (/terminals?lv=1&...), not just the bare path', async () => {
    // The router's currentPath still carries the query string here — a
    // bare `=== '/terminals'` check would never match, since the real
    // query-string suffix is still attached.
    const el = await mountHeader({
      currentPath: '/terminals?lv=1&lp=two-columns&s0=&s1=',
    });
    expect(paletteButton(el)).not.toBeNull();
  });

  it('labels the chat route button "Quick switcher", not "Jump to agent"', async () => {
    const el = await mountHeader();
    expect(paletteButton(el)?.getAttribute('aria-label')).toBe('Open quick switcher');
  });
});

describe('palette button: graph views', () => {
  const graphOwner = {};

  afterEach(() => {
    setGraphPaletteAvailable(graphOwner, false);
  });

  it('is absent on a dashboard route while no graph offers the palette', async () => {
    const el = await mountHeader({ currentPath: '/agents' });
    expect(paletteButton(el)).toBeNull();
  });

  it('renders as "Jump to agent" while a graph offers the palette, and goes when it stops', async () => {
    setGraphPaletteAvailable(graphOwner, true);
    const el = await mountHeader({ currentPath: '/agents' });
    const button = paletteButton(el);
    expect(button?.getAttribute('aria-label')).toBe('Open Jump to agent');
    expect(button?.getAttribute('aria-label')).not.toBe('Jump to agent');
    expect(paletteTooltip(el)?.getAttribute('content')).toMatch(/^Jump to agent \((⌘K|Ctrl\+K)\)$/);

    setGraphPaletteAvailable(graphOwner, false);
    await el.updateComplete;
    expect(paletteButton(el)).toBeNull();
  });

  it('appears when a graph starts offering the palette after the header mounted', async () => {
    const el = await mountHeader({ currentPath: '/projects/abc' });
    expect(paletteButton(el)).toBeNull();

    setGraphPaletteAvailable(graphOwner, true);
    await el.updateComplete;

    expect(paletteButton(el)?.getAttribute('aria-label')).toBe('Open Jump to agent');
  });

  it('dispatches GRAPH_PALETTE_OPEN_REQUEST_EVENT, not the chat or terminal event, after focusing the button', async () => {
    setGraphPaletteAvailable(graphOwner, true);
    const el = await mountHeader({ currentPath: '/agents' });
    const button = paletteButton(el) as HTMLElement;
    const focusSpy = vi.spyOn(button, 'focus');
    const received: string[] = [];
    let graphEvent: CustomEvent | undefined;
    el.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, () => received.push('chat'));
    el.addEventListener(TERMINAL_PALETTE_OPEN_REQUEST_EVENT, () => received.push('terminal'));
    el.addEventListener(GRAPH_PALETTE_OPEN_REQUEST_EVENT, (e) => {
      received.push('graph');
      graphEvent = e as CustomEvent;
    });

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    expect(received).toEqual(['graph']);
    expect(graphEvent!.bubbles).toBe(true);
    expect(graphEvent!.composed).toBe(true);
  });

  it('leaves chat and terminal routes to their own palettes', async () => {
    setGraphPaletteAvailable(graphOwner, true);
    let el = await mountHeader({ currentPath: '/chat' });
    expect(paletteButton(el)?.getAttribute('aria-label')).toBe('Open quick switcher');
    el.remove();

    el = await mountHeader({ currentPath: '/terminals' });
    const received: string[] = [];
    el.addEventListener(TERMINAL_PALETTE_OPEN_REQUEST_EVENT, () => received.push('terminal'));
    el.addEventListener(GRAPH_PALETTE_OPEN_REQUEST_EVENT, () => received.push('graph'));
    (paletteButton(el) as HTMLElement).click();
    expect(received).toEqual(['terminal']);
  });

  it('is absent when no user is signed in', async () => {
    setGraphPaletteAvailable(graphOwner, true);
    const el = await mountHeader({ user: null, currentPath: '/agents' });
    expect(paletteButton(el)).toBeNull();
  });

  it('stops following availability once disconnected', async () => {
    const el = await mountHeader({ currentPath: '/agents' });
    const added = vi.spyOn(window, 'addEventListener');
    const removed = vi.spyOn(window, 'removeEventListener');
    el.remove();
    document.body.append(el);
    await el.updateComplete;
    const handler = added.mock.calls.find(
      ([type]) => type === GRAPH_PALETTE_AVAILABILITY_EVENT
    )?.[1];
    expect(handler).toBeDefined();
    expect(removed).toHaveBeenCalledWith(GRAPH_PALETTE_AVAILABILITY_EVENT, handler);
  });
});

describe('palette button: click dispatch', () => {
  it('focuses the button, then dispatches CHAT_PALETTE_OPEN_REQUEST_EVENT with bubbles and composed set', async () => {
    const el = await mountHeader();
    const button = paletteButton(el) as HTMLElement;
    expect(button).not.toBeNull();
    const focusSpy = vi.spyOn(button, 'focus');
    let received: CustomEvent | undefined;
    el.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, (e) => {
      received = e as CustomEvent;
    });

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    expect(received).toBeDefined();
    expect(received!.bubbles).toBe(true);
    expect(received!.composed).toBe(true);
  });

  it('dispatches TERMINAL_PALETTE_OPEN_REQUEST_EVENT (not the chat event) on /terminals', async () => {
    const el = await mountHeader({ currentPath: '/terminals' });
    const button = paletteButton(el) as HTMLElement;
    expect(button).not.toBeNull();
    let chatReceived = false;
    let terminalReceived: CustomEvent | undefined;
    el.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, () => {
      chatReceived = true;
    });
    el.addEventListener(TERMINAL_PALETTE_OPEN_REQUEST_EVENT, (e) => {
      terminalReceived = e as CustomEvent;
    });

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(chatReceived).toBe(false);
    expect(terminalReceived).toBeDefined();
    expect(terminalReceived!.bubbles).toBe(true);
    expect(terminalReceived!.composed).toBe(true);
  });

  it('calls focus() before dispatching the event, not after', async () => {
    // Order matters, not just that both happen: chat.ts's _openPalette
    // captures the deep active element synchronously, before its own first
    // await. On Safari, which does not focus a clicked button on its own, a
    // dispatch-then-focus ordering would still capture whatever was focused
    // *before* the click — possibly the composer — defeating the reason
    // this call exists at all. A real click/focus round-trip can't
    // distinguish the two orderings in Chromium (it focuses on click
    // regardless), so this records call order directly instead.
    const el = await mountHeader();
    const button = paletteButton(el) as HTMLElement;
    const order: string[] = [];
    vi.spyOn(button, 'focus').mockImplementation(() => {
      order.push('focus');
    });
    el.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, () => {
      order.push('dispatch');
    });

    button.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));

    expect(order).toEqual(['focus', 'dispatch']);
  });
});

describe('palette button: tooltip and aria-keyshortcuts follow the mocked modality', () => {
  const originalPlatform = Object.getOwnPropertyDescriptor(window.navigator, 'platform');

  afterEach(() => {
    if (originalPlatform) {
      Object.defineProperty(window.navigator, 'platform', originalPlatform);
    }
  });

  function setPlatform(platform: string): void {
    Object.defineProperty(window.navigator, 'platform', { value: platform, configurable: true });
  }

  it('on touch: tooltip is disabled and the button has no aria-keyshortcuts', async () => {
    stubTouchPrimary(true);
    const el = await mountHeader();

    expect(paletteTooltip(el)?.hasAttribute('disabled')).toBe(true);
    expect(paletteButton(el)?.hasAttribute('aria-keyshortcuts')).toBe(false);
  });

  it('on desktop (non-Mac): tooltip is enabled and reads "(Ctrl+K)"; aria-keyshortcuts is "Control+K"', async () => {
    setPlatform('Linux x86_64');
    stubTouchPrimary(false);
    const el = await mountHeader();

    const tooltip = paletteTooltip(el);
    expect(tooltip?.hasAttribute('disabled')).toBe(false);
    expect(tooltip?.getAttribute('content')).toBe('Quick switcher (Ctrl+K)');
    expect(paletteButton(el)?.getAttribute('aria-keyshortcuts')).toBe('Control+K');
  });

  it('on desktop (Mac): tooltip reads "(⌘K)"; aria-keyshortcuts is "Meta+K"', async () => {
    setPlatform('MacIntel');
    stubTouchPrimary(false);
    const el = await mountHeader();

    const tooltip = paletteTooltip(el);
    expect(tooltip?.getAttribute('content')).toBe('Quick switcher (⌘K)');
    expect(paletteButton(el)?.getAttribute('aria-keyshortcuts')).toBe('Meta+K');
  });

  it('on /terminals: tooltip reads "Jump to agent" with the platform shortcut', async () => {
    stubTouchPrimary(false);
    setPlatform('MacIntel');
    let el = await mountHeader({ currentPath: '/terminals' });
    expect(paletteTooltip(el)?.getAttribute('content')).toBe('Jump to agent (⌘K)');
    el.remove();

    setPlatform('Linux x86_64');
    el = await mountHeader({ currentPath: '/terminals' });
    expect(paletteTooltip(el)?.getAttribute('content')).toBe('Jump to agent (Ctrl+K)');
  });

  it('the button always has aria-haspopup="dialog", on both touch and desktop', async () => {
    stubTouchPrimary(true);
    let el = await mountHeader();
    expect(paletteButton(el)?.getAttribute('aria-haspopup')).toBe('dialog');
    el.remove();

    stubTouchPrimary(false);
    el = await mountHeader();
    expect(paletteButton(el)?.getAttribute('aria-haspopup')).toBe('dialog');
  });
});

// ---------------------------------------------------------------------------
// Tray badge counts follow the signed-in user.
// ---------------------------------------------------------------------------

describe('tray badge counts: user switch', () => {
  /** How many unread items each tray list request returns. */
  let unread = 0;

  function serveUnread(): void {
    vi.mocked(apiFetch).mockImplementation((url: string) => {
      const items = Array.from({ length: unread }, (_, i) => ({
        id: `item-${i}`,
        status: 'COMPLETED',
        message: `item ${i}`,
        sender: 'agent:helper',
        msg: `item ${i}`,
        type: 'instruction',
        agentId: 'agent-1',
        createdAt: new Date().toISOString(),
      }));
      let body: unknown = {};
      if (url.startsWith('/api/v1/notifications')) body = items;
      else if (url.startsWith('/api/v1/messages')) body = { items };
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });
  }

  /** The inbox and notification counts shown on the wide-layout trigger badges. */
  function badges(el: ScionHeader): string[] {
    return [...(el.shadowRoot?.querySelectorAll('.wide-right .trigger-badge') ?? [])].map(
      (b) => b.textContent?.trim() ?? ''
    );
  }

  async function switchUser(el: ScionHeader, u: User): Promise<void> {
    el.user = u;
    await el.updateComplete;
  }

  /** Waits past the header's delayed count sync. */
  async function afterCountSync(): Promise<void> {
    await new Promise((r) => setTimeout(r, 700));
  }

  afterEach(() => {
    vi.mocked(apiFetch).mockImplementation(() =>
      Promise.resolve(new Response('{}', { status: 200 }))
    );
  });

  it('shows the next user counts after a user switch', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2', '2']), { timeout: 3000 });

    unread = 0;
    await switchUser(el, { id: 'u2', email: 'u2@example.com', name: 'U2' });
    expect(badges(el)).toEqual([]);
    await afterCountSync();
    expect(badges(el)).toEqual([]);

    unread = 1;
    await switchUser(el, { id: 'u3', email: 'u3@example.com', name: 'U3' });
    await vi.waitFor(() => expect(badges(el)).toEqual(['1', '1']), { timeout: 3000 });
  });

  it('keeps the counts when the same user is handed over as a new object', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2', '2']), { timeout: 3000 });

    await switchUser(el, { id: 'u1', email: 'u1@example.com', name: 'U1' });
    expect(badges(el)).toEqual(['2', '2']);
    await afterCountSync();
    expect(badges(el)).toEqual(['2', '2']);
  });

  it('clears the counts after sign-out and signing back in', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2', '2']), { timeout: 3000 });

    unread = 0;
    el.user = null;
    await el.updateComplete;

    await switchUser(el, { id: 'u1', email: 'u1@example.com', name: 'U1' });
    expect(badges(el)).toEqual([]);
    await afterCountSync();
    expect(badges(el)).toEqual([]);
  });

  it('never renders the next user with the previous user counts', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2', '2']), { timeout: 3000 });

    // The counts are private; read them through a structural view.
    const counts = el as unknown as { inboxCount: number; notificationCount: number };
    const renders: { id: string | undefined; inbox: number; notif: number }[] = [];
    const originalRender = el.render.bind(el);
    el.render = (): ReturnType<typeof originalRender> => {
      renders.push({ id: el.user?.id, inbox: counts.inboxCount, notif: counts.notificationCount });
      return originalRender();
    };

    unread = 0;
    await switchUser(el, { id: 'u2', email: 'u2@example.com', name: 'U2' });

    const u2Renders = renders.filter((r) => r.id === 'u2');
    expect(u2Renders.length).toBeGreaterThan(0);
    for (const r of u2Renders) {
      expect(r).toEqual({ id: 'u2', inbox: 0, notif: 0 });
    }
  });
});
