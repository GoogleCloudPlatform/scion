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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

// Mounting <scion-header> also mounts its tray child
// (<scion-notification-tray>), which fetches on connect —
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
  formatBadgeCount,
  projectIdFromDashboardPath,
  projectIdFromChatSpacePath,
  slugFromChatPath,
  type ScionHeader,
} from './header.js';
import { CHAT_UNREAD_COUNT_EVENT, chatUnread } from '../../client/chat-unread.js';
import { resetServerFlagStateForTests, setServerFlags } from '../../utils/feature-flags.js';
import { TOUCH_PRIMARY_QUERY } from '../../utils/input-modality.js';
import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import {
  GRAPH_PALETTE_AVAILABILITY_EVENT,
  GRAPH_PALETTE_OPEN_REQUEST_EVENT,
  setGraphPaletteAvailable,
} from '../../client/graph-palette-events.js';
import type { User } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import { setViewModeShortcutsEnabled } from '../../client/view-mode-shortcuts.js';

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
  // Only checks whether matchMedia is already a mock; it is never called here.
  // eslint-disable-next-line @typescript-eslint/unbound-method
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

  it('is absent on every terminal route: Jump to agent lives in the terminal list footer', async () => {
    // The terminal view's palette opener is a labelled button pinned to
    // the bottom of its Open terminals column (TerminalWorkspaceRoot), so
    // the header must not duplicate it on /terminals, a per-agent route,
    // or the multi-pane URL form (whose currentPath keeps its query).
    for (const currentPath of [
      '/terminals',
      '/terminals/00000000-0000-0000-0000-000000000001',
      '/terminals?lv=1&lp=two-columns&s0=&s1=',
    ]) {
      const el = await mountHeader({ currentPath });
      expect(paletteButton(el)).toBeNull();
      el.remove();
    }
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

  it('dispatches GRAPH_PALETTE_OPEN_REQUEST_EVENT, not the chat event, after focusing the button', async () => {
    setGraphPaletteAvailable(graphOwner, true);
    const el = await mountHeader({ currentPath: '/agents' });
    const button = paletteButton(el) as HTMLElement;
    const focusSpy = vi.spyOn(button, 'focus');
    const received: string[] = [];
    let graphEvent: CustomEvent | undefined;
    el.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, () => received.push('chat'));
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

    // The terminal view opens its palette from its own list footer.
    el = await mountHeader({ currentPath: '/terminals' });
    expect(paletteButton(el)).toBeNull();
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
      return Promise.resolve(new Response(JSON.stringify(body), { status: 200 }));
    });
  }

  /** The notification count shown on the wide-layout trigger badge. */
  function badges(el: ScionHeader): string[] {
    return [...(el.shadowRoot?.querySelectorAll('.wide-right .trigger-badge') ?? [])].map(
      (b) => b.textContent?.trim() ?? ''
    );
  }

  async function switchUser(el: ScionHeader, u: User): Promise<void> {
    el.user = u;
    await el.updateComplete;
  }

  /** Gives late tray fetches and timers time to run before re-checking. */
  async function afterLateUpdates(): Promise<void> {
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
    await vi.waitFor(() => expect(badges(el)).toEqual(['2']), { timeout: 3000 });

    unread = 0;
    await switchUser(el, { id: 'u2', email: 'u2@example.com', name: 'U2' });
    expect(badges(el)).toEqual([]);
    await afterLateUpdates();
    expect(badges(el)).toEqual([]);

    unread = 1;
    await switchUser(el, { id: 'u3', email: 'u3@example.com', name: 'U3' });
    await vi.waitFor(() => expect(badges(el)).toEqual(['1']), { timeout: 3000 });
  });

  it('keeps the counts when the same user is handed over as a new object', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2']), { timeout: 3000 });

    await switchUser(el, { id: 'u1', email: 'u1@example.com', name: 'U1' });
    expect(badges(el)).toEqual(['2']);
    await afterLateUpdates();
    expect(badges(el)).toEqual(['2']);
  });

  it('clears the counts after sign-out and signing back in', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2']), { timeout: 3000 });

    unread = 0;
    el.user = null;
    await el.updateComplete;

    await switchUser(el, { id: 'u1', email: 'u1@example.com', name: 'U1' });
    expect(badges(el)).toEqual([]);
    await afterLateUpdates();
    expect(badges(el)).toEqual([]);
  });

  it('never renders the next user with the previous user counts', async () => {
    unread = 2;
    serveUnread();
    const el = await mountHeader({ user: { id: 'u1', email: 'u1@example.com', name: 'U1' } });
    await vi.waitFor(() => expect(badges(el)).toEqual(['2']), { timeout: 3000 });

    // The counts are private; read them through a structural view.
    const counts = el as unknown as { notificationCount: number };
    const renders: { id: string | undefined; notif: number }[] = [];
    const originalRender = el.render.bind(el);
    el.render = (): ReturnType<typeof originalRender> => {
      renders.push({ id: el.user?.id, notif: counts.notificationCount });
      return originalRender();
    };

    unread = 0;
    await switchUser(el, { id: 'u2', email: 'u2@example.com', name: 'U2' });

    const u2Renders = renders.filter((r) => r.id === 'u2');
    expect(u2Renders.length).toBeGreaterThan(0);
    for (const r of u2Renders) {
      expect(r).toEqual({ id: 'u2', notif: 0 });
    }
  });
});

// ---------------------------------------------------------------------------
// Tray badge counts follow the trays' count events, however slow a fetch is.
// ---------------------------------------------------------------------------

describe('tray badge counts: slow fetches', () => {
  interface Held {
    url: string;
    method: string;
    release: (count: number) => void;
  }

  /** Requests the trays made that have not been answered yet. */
  let held: Held[] = [];

  function itemsOf(count: number): unknown[] {
    return Array.from({ length: count }, (_, i) => ({
      id: `item-${i}`,
      status: 'COMPLETED',
      message: `item ${i}`,
      sender: 'agent:helper',
      msg: `item ${i}`,
      type: 'instruction',
      agentId: 'agent-1',
      createdAt: new Date().toISOString(),
    }));
  }

  function holdRequests(): void {
    vi.mocked(apiFetch).mockImplementation(
      (url: string, init?: RequestInit) =>
        new Promise<Response>((resolve) => {
          held.push({
            url,
            method: init?.method ?? 'GET',
            release: (count) => {
              let body: unknown = {};
              if (url.startsWith('/api/v1/notifications')) body = itemsOf(count);
              resolve(new Response(JSON.stringify(body), { status: 200 }));
            },
          });
        })
    );
  }

  /** Takes every held list request, leaving action requests in place. */
  function takeLists(): Held[] {
    const lists = held.filter((h) => h.method === 'GET');
    held = held.filter((h) => h.method !== 'GET');
    return lists;
  }

  /** Answers every held list request with count items. */
  async function releaseLists(count: number): Promise<void> {
    const lists = takeLists();
    expect(lists.length).toBeGreaterThan(0);
    for (const h of lists) h.release(count);
    await settle();
  }

  /** Lets response chains and the resulting Lit updates run. */
  async function settle(): Promise<void> {
    for (let i = 0; i < 10; i++) await vi.advanceTimersByTimeAsync(0);
  }

  function badges(el: ScionHeader): string[] {
    return [...(el.shadowRoot?.querySelectorAll('.wide-right .trigger-badge') ?? [])].map(
      (b) => b.textContent?.trim() ?? ''
    );
  }

  /** The tray's private actions, read through a structural view. */
  interface TrayActions {
    ackOne(id: string): Promise<void>;
  }

  function tray(el: ScionHeader, tag: string): TrayActions {
    return el.shadowRoot?.querySelector(tag) as unknown as TrayActions;
  }

  beforeEach(() => {
    held = [];
    holdRequests();
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'],
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.mocked(apiFetch).mockImplementation(() =>
      Promise.resolve(new Response('{}', { status: 200 }))
    );
  });

  async function mount(id: string): Promise<ScionHeader> {
    const el = await mountHeader({ user: { id, email: `${id}@example.com`, name: id } });
    await settle();
    return el;
  }

  it('shows the badges when a slow first fetch resolves', async () => {
    const el = await mount('u1');
    await vi.advanceTimersByTimeAsync(600);
    expect(badges(el)).toEqual([]);

    await releaseLists(2);
    expect(badges(el)).toEqual(['2']);
  });

  it('shows the next user counts when a slow fetch after a user switch resolves', async () => {
    const el = await mount('u1');
    await releaseLists(2);
    expect(badges(el)).toEqual(['2']);

    el.user = { id: 'u2', email: 'u2@example.com', name: 'u2' };
    await el.updateComplete;
    await settle();
    expect(badges(el)).toEqual([]);

    await vi.advanceTimersByTimeAsync(600);
    expect(badges(el)).toEqual([]);

    await releaseLists(1);
    expect(badges(el)).toEqual(['1']);
  });

  it('ignores a response for the previous user', async () => {
    const el = await mount('u1');
    const stale = takeLists();
    expect(stale.length).toBeGreaterThan(0);

    el.user = { id: 'u2', email: 'u2@example.com', name: 'u2' };
    await el.updateComplete;
    await settle();

    for (const h of stale) h.release(3);
    await settle();
    expect(badges(el)).toEqual([]);

    await releaseLists(1);
    expect(badges(el)).toEqual(['1']);

    // A late response for the previous user changes no badge either.
    el.user = { id: 'u3', email: 'u3@example.com', name: 'u3' };
    await el.updateComplete;
    await settle();
    const staleU3 = takeLists();
    el.user = { id: 'u2', email: 'u2@example.com', name: 'u2' };
    await el.updateComplete;
    await settle();
    await releaseLists(1);
    expect(badges(el)).toEqual(['1']);
    for (const h of staleU3) h.release(4);
    await settle();
    expect(badges(el)).toEqual(['1']);
  });

  it('updates the badge when an item is acknowledged', async () => {
    const el = await mount('u1');
    await releaseLists(2);
    expect(badges(el)).toEqual(['2']);

    const ack = tray(el, 'scion-notification-tray').ackOne('item-0');
    await settle();
    for (const h of held.splice(0)) h.release(0);
    await ack;
    await settle();
    expect(badges(el)).toEqual(['1']);
    const notifBadge = el.shadowRoot?.querySelector(
      '.wide-right sl-icon-button[name="bell"] + .trigger-badge'
    );
    expect(notifBadge?.textContent?.trim()).toBe('1');
  });

  it('keeps updating the badges after the header is detached and attached again', async () => {
    const el = await mount('u1');
    await releaseLists(1);
    expect(badges(el)).toEqual(['1']);

    el.remove();
    await settle();
    document.body.appendChild(el);
    await el.updateComplete;
    await settle();
    // The trays fetch again when they reconnect.
    await releaseLists(1);
    expect(badges(el)).toEqual(['1']);

    stateManager.dispatchEvent(new CustomEvent('notification-created'));
    await settle();
    await releaseLists(4);
    expect(badges(el)).toEqual(['4']);
    el.remove();
  });
});

// ---------------------------------------------------------------------------
// Chat unread badge on the chat mode selector, and no envelope.
// ---------------------------------------------------------------------------

describe('chat unread badge', () => {
  function setChatUnread(count: number): void {
    window.dispatchEvent(new CustomEvent(CHAT_UNREAD_COUNT_EVENT, { detail: { count } }));
  }

  /** Badge text on the segmented chat buttons (wide and medium tiers). */
  function segmentBadges(el: ScionHeader): string[] {
    return [
      ...(el.shadowRoot?.querySelectorAll(
        '.mode-switch button[data-mode="chat"] .chat-unread-badge'
      ) ?? []),
    ].map((b) => b.textContent?.trim() ?? '');
  }

  function dropdownTriggerBadge(el: ScionHeader): Element | null | undefined {
    return el.shadowRoot?.querySelector('.compact-mode-dropdown .mode-trigger .chat-unread-badge');
  }

  function dropdownChatCount(el: ScionHeader): Element | null | undefined {
    return el.shadowRoot?.querySelector(
      '.compact-mode-dropdown sl-menu-item[value="chat"] .chat-count'
    );
  }

  beforeEach(() => {
    setServerFlags({ 'web.native_chat': true });
  });

  afterEach(() => {
    setChatUnread(0);
    resetServerFlagStateForTests();
  });

  it('shows the unread conversation count on every chat selector', async () => {
    const el = await mountHeader({ currentPath: '/' });
    setChatUnread(3);
    await el.updateComplete;

    // The wide and the medium (icon-only) segmented controls.
    expect(segmentBadges(el)).toEqual(['3', '3']);
    // The narrow tier: on the dropdown trigger and on its Chat item.
    expect(dropdownTriggerBadge(el)?.textContent?.trim()).toBe('3');
    expect(dropdownChatCount(el)?.textContent?.trim()).toBe('3');
    expect(dropdownChatCount(el)?.getAttribute('slot')).toBe('suffix');
  });

  it('keeps the badge out of the Chat label', async () => {
    const el = await mountHeader({ currentPath: '/' });
    setChatUnread(2);
    await el.updateComplete;

    const button = el.shadowRoot?.querySelector('.wide-center button[data-mode="chat"]');
    const badge = button?.querySelector('.chat-unread-badge');
    // The badge sits in the icon wrapper, not in the label.
    expect(badge?.parentElement?.classList.contains('mode-icon')).toBe(true);
    expect(button?.querySelector('.mode-label')?.textContent?.trim()).toBe('Chat');
  });

  it('is hidden at zero', async () => {
    const el = await mountHeader({ currentPath: '/' });
    setChatUnread(4);
    await el.updateComplete;
    setChatUnread(0);
    await el.updateComplete;

    expect(segmentBadges(el)).toEqual([]);
    expect(dropdownTriggerBadge(el)).toBeNull();
    expect(dropdownChatCount(el)).toBeNull();
  });

  it('caps the count at 99+', async () => {
    expect(formatBadgeCount(99)).toBe('99');
    expect(formatBadgeCount(100)).toBe('99+');

    const el = await mountHeader({ currentPath: '/' });
    setChatUnread(250);
    await el.updateComplete;
    expect(segmentBadges(el)).toEqual(['99+', '99+']);
    expect(dropdownChatCount(el)?.textContent?.trim()).toBe('99+');
  });

  it('names the count in the accessible label', async () => {
    const el = await mountHeader({ currentPath: '/' });
    setChatUnread(1);
    await el.updateComplete;
    const button = el.shadowRoot?.querySelector('.wide-center button[data-mode="chat"]');
    expect(button?.getAttribute('aria-label')).toBe('Chat, 1 unread conversation');
    expect(button?.querySelector('.chat-unread-badge')?.getAttribute('aria-hidden')).toBe('true');

    setChatUnread(5);
    await el.updateComplete;
    expect(button?.getAttribute('aria-label')).toBe('Chat, 5 unread conversations');
    expect(
      el.shadowRoot
        ?.querySelector('.compact-mode-dropdown .mode-trigger')
        ?.getAttribute('aria-label')
    ).toBe('Switch view mode, 5 unread conversations');
  });

  it('shows no badge when signed out', async () => {
    const el = await mountHeader({ user: null, currentPath: '/' });
    setChatUnread(3);
    await el.updateComplete;
    expect(segmentBadges(el)).toEqual([]);
    expect(dropdownTriggerBadge(el)).toBeNull();
  });

  it('stops following the count once disconnected', async () => {
    const el = await mountHeader({ currentPath: '/' });
    el.remove();
    setChatUnread(6);
    // Read the private state directly: reconnecting would re-seed it from
    // the counter and hide whether the listener was removed.
    expect((el as unknown as { chatUnreadCount: number }).chatUnreadCount).toBe(0);
  });

  it('shows a count the counter already holds when it mounts late', async () => {
    const count = vi.spyOn(chatUnread, 'count', 'get').mockReturnValue(7);
    try {
      const el = await mountHeader({ currentPath: '/' });
      expect(segmentBadges(el)).toEqual(['7', '7']);
    } finally {
      count.mockRestore();
    }
  });

  it('re-reads the counter when it reconnects', async () => {
    const el = await mountHeader({ currentPath: '/' });
    el.remove();
    const count = vi.spyOn(chatUnread, 'count', 'get').mockReturnValue(2);
    try {
      document.body.appendChild(el);
      await el.updateComplete;
      expect(segmentBadges(el)).toEqual(['2', '2']);
    } finally {
      count.mockRestore();
    }
  });
});

describe('envelope removed', () => {
  it('renders no Messages button, menu item or inbox tray', async () => {
    const el = await mountHeader({ currentPath: '/' });
    const root = el.shadowRoot;
    expect(root?.querySelector('sl-icon-button[name="envelope"]')).toBeNull();
    expect(root?.querySelector('sl-menu-item[value="messages"]')).toBeNull();
    expect(root?.querySelector('scion-inbox-tray')).toBeNull();
    // The bell stays.
    expect(root?.querySelector('sl-icon-button[name="bell"]')).not.toBeNull();
    expect(root?.querySelector('sl-menu-item[value="notifications"]')).not.toBeNull();
  });
});

describe('mode switch: remembered chat path', () => {
  async function modeSwitchTarget(el: ScionHeader, mode: 'dashboard' | 'chat'): Promise<string> {
    let target = '';
    el.addEventListener('nav-click', ((e: CustomEvent<{ path: string }>) => {
      target = e.detail.path;
    }) as EventListener);
    await (el as unknown as { handleModeSwitch(m: string): Promise<void> }).handleModeSwitch(mode);
    return target;
  }

  it('returns to the thread the shell last recorded', async () => {
    const el = await mountHeader({ currentPath: '/chat/alpha/topic-1' });
    el.currentPath = '/chat/alpha/topic-2';
    await el.updateComplete;
    el.currentPath = '/';
    await el.updateComplete;
    expect(await modeSwitchTarget(el, 'chat')).toBe('/chat/alpha/topic-2');
  });

  it('drops a message jump fragment so the return does not replay the jump', async () => {
    const el = await mountHeader({ currentPath: '/chat/alpha/topic-3#msg-m1' });
    el.currentPath = '/terminals';
    await el.updateComplete;
    expect(await modeSwitchTarget(el, 'chat')).toBe('/chat/alpha/topic-3');
  });
});

// ---------------------------------------------------------------------------
// View-mode keyboard shortcuts: Cmd+1/2/3 on macOS, Ctrl+1/2/3 elsewhere.
// ---------------------------------------------------------------------------

describe('view mode shortcuts', () => {
  const originalPlatform = Object.getOwnPropertyDescriptor(window.navigator, 'platform');
  let navTargets: string[] = [];
  const onNav = ((e: CustomEvent<{ path: string }>): void => {
    navTargets.push(e.detail.path);
  }) as EventListener;

  function setPlatform(platform: string): void {
    Object.defineProperty(window.navigator, 'platform', { value: platform, configurable: true });
  }

  beforeEach(() => {
    navTargets = [];
    document.addEventListener('nav-click', onNav);
    setPlatform('Linux x86_64');
  });

  afterEach(() => {
    document.removeEventListener('nav-click', onNav);
    if (originalPlatform) {
      Object.defineProperty(window.navigator, 'platform', originalPlatform);
    }
    localStorage.clear();
    delete (window as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__;
  });

  /** Dispatches a keydown on `target` the way a browser would: bubbling and composed. */
  function press(target: EventTarget, init: KeyboardEventInit): KeyboardEvent {
    const e = new KeyboardEvent('keydown', {
      bubbles: true,
      composed: true,
      cancelable: true,
      ...init,
    });
    target.dispatchEvent(e);
    return e;
  }

  /** A focused text input with a keydown spy, standing in for a composer or terminal textarea. */
  function focusedInput(): { input: HTMLInputElement; seen: ReturnType<typeof vi.fn> } {
    const input = document.createElement('input');
    const seen = vi.fn();
    input.addEventListener('keydown', seen);
    document.body.appendChild(input);
    input.focus();
    return { input, seen };
  }

  async function settled(): Promise<void> {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }

  it('Ctrl+1, Ctrl+2 and Ctrl+3 switch to Dashboard, Chat and Terminal', async () => {
    await mountHeader({ currentPath: '/projects/p1' });

    press(document.body, { code: 'Digit2', key: '2', ctrlKey: true });
    await settled();
    press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    press(document.body, { code: 'Digit1', key: '1', ctrlKey: true });
    await settled();

    expect(navTargets).toHaveLength(3);
    expect(navTargets[0]).toMatch(/^\/chat/);
    expect(navTargets[1]).toBe('/terminals');
    expect(navTargets[2]).not.toMatch(/^\/(chat|terminals)/);
  });

  it('uses Cmd on macOS and leaves Ctrl+digit alone there', async () => {
    setPlatform('MacIntel');
    await mountHeader({ currentPath: '/projects/p1' });

    const ctrl = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    expect(ctrl.defaultPrevented).toBe(false);
    expect(navTargets).toEqual([]);

    const cmd = press(document.body, { code: 'Digit3', key: '3', metaKey: true });
    await settled();
    expect(cmd.defaultPrevented).toBe(true);
    expect(navTargets).toEqual(['/terminals']);
  });

  it('switches with focus in a text input, and the input never receives the key', async () => {
    await mountHeader({ currentPath: '/projects/p1' });
    const { input, seen } = focusedInput();

    const e = press(input, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();

    expect(navTargets).toEqual(['/terminals']);
    expect(e.defaultPrevented).toBe(true);
    expect(seen).not.toHaveBeenCalled();
    expect(input.value).toBe('');
  });

  it('switches with focus inside a shadow root, as in a terminal pane', async () => {
    await mountHeader({ currentPath: '/projects/p1' });
    const host = document.createElement('div');
    const textarea = document.createElement('textarea');
    const seen = vi.fn();
    textarea.addEventListener('keydown', seen);
    host.attachShadow({ mode: 'open' }).appendChild(textarea);
    document.body.appendChild(host);

    const e = press(textarea, { code: 'Digit2', key: '2', ctrlKey: true });
    await settled();

    expect(navTargets).toHaveLength(1);
    expect(navTargets[0]).toMatch(/^\/chat/);
    expect(e.defaultPrevented).toBe(true);
    expect(seen).not.toHaveBeenCalled();
  });

  it('leaves unrelated keys untouched', async () => {
    await mountHeader({ currentPath: '/projects/p1' });
    const { input, seen } = focusedInput();

    const unrelated: KeyboardEventInit[] = [
      { code: 'Digit1', key: '1' },
      { code: 'Digit4', key: '4', ctrlKey: true },
      { code: 'Digit1', key: '!', ctrlKey: true, shiftKey: true },
      { code: 'Digit1', key: '1', ctrlKey: true, altKey: true },
      { code: 'Digit1', key: '1', metaKey: true },
      { code: 'KeyK', key: 'k', ctrlKey: true },
      { code: 'Digit1', key: '1', ctrlKey: true, isComposing: true },
    ];
    for (const init of unrelated) {
      const e = press(input, init);
      expect(e.defaultPrevented, JSON.stringify(init)).toBe(false);
    }
    await settled();

    expect(seen).toHaveBeenCalledTimes(unrelated.length);
    expect(navTargets).toEqual([]);

    // Control: the real chord from the same input is handled.
    press(input, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    expect(navTargets).toEqual(['/terminals']);
  });

  it('switches once for a held chord and keeps repeats from the browser', async () => {
    await mountHeader({ currentPath: '/projects/p1' });

    press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    const repeat = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true, repeat: true });
    await settled();

    expect(repeat.defaultPrevented).toBe(true);
    expect(navTargets).toEqual(['/terminals']);
  });

  it('passes the key through when its mode is not offered', async () => {
    window.__SCION_FEATURES__ = { 'web.native_chat': false, 'web.terminal_workspace': true };
    await mountHeader({ currentPath: '/projects/p1' });
    const { input, seen } = focusedInput();

    const e = press(input, { code: 'Digit2', key: '2', ctrlKey: true });
    await settled();

    expect(e.defaultPrevented).toBe(false);
    expect(seen).toHaveBeenCalledTimes(1);
    expect(navTargets).toEqual([]);

    // Control: an offered mode still switches.
    press(input, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    expect(navTargets).toEqual(['/terminals']);
  });

  it('leaves the keys to the header that is on screen', async () => {
    const hiddenWrapper = document.createElement('div');
    hiddenWrapper.hidden = true;
    document.body.appendChild(hiddenWrapper);
    const hiddenHeader = document.createElement('scion-header');
    hiddenHeader.user = TEST_USER;
    hiddenHeader.currentPath = '/projects/p1';
    hiddenWrapper.appendChild(hiddenHeader);
    await hiddenHeader.updateComplete;

    const e = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    expect(e.defaultPrevented).toBe(false);
    expect(navTargets).toEqual([]);

    await mountHeader({ currentPath: '/terminals' });
    press(document.body, { code: 'Digit2', key: '2', ctrlKey: true });
    await settled();
    expect(navTargets).toHaveLength(1);
  });

  it('stops listening once the header is removed', async () => {
    const el = await mountHeader({ currentPath: '/projects/p1' });
    press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();
    expect(navTargets).toEqual(['/terminals']);
    navTargets = [];
    el.remove();

    const e = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
    await settled();

    expect(e.defaultPrevented).toBe(false);
    expect(navTargets).toEqual([]);
  });

  describe('with the setting off', () => {
    it('does nothing and the key reaches the input and the browser', async () => {
      setViewModeShortcutsEnabled(false);
      await mountHeader({ currentPath: '/projects/p1' });
      const { input, seen } = focusedInput();

      const e = press(input, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();

      expect(e.defaultPrevented).toBe(false);
      expect(seen).toHaveBeenCalledTimes(1);
      expect(navTargets).toEqual([]);

      // Control: turning it back on restores the shortcut.
      setViewModeShortcutsEnabled(true);
      press(input, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(navTargets).toEqual(['/terminals']);
    });

    it('takes effect in a mounted header without a reload, both ways', async () => {
      await mountHeader({ currentPath: '/projects/p1' });

      setViewModeShortcutsEnabled(false);
      const off = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(off.defaultPrevented).toBe(false);
      expect(navTargets).toEqual([]);

      setViewModeShortcutsEnabled(true);
      const on = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(on.defaultPrevented).toBe(true);
      expect(navTargets).toEqual(['/terminals']);
    });
  });

  describe('with the setting changed in another tab', () => {
    /** Writes the store directly and fires the storage event another tab's write would. */
    function writeFromOtherTab(value: string | null): void {
      if (value === null) {
        localStorage.removeItem('scion-view-mode-shortcuts');
      } else {
        localStorage.setItem('scion-view-mode-shortcuts', value);
      }
      window.dispatchEvent(
        new StorageEvent('storage', {
          key: 'scion-view-mode-shortcuts',
          newValue: value,
          storageArea: localStorage,
        })
      );
    }

    it('updates a mounted header both ways without a remount', async () => {
      await mountHeader({ currentPath: '/projects/p1' });

      writeFromOtherTab('false');
      const off = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(off.defaultPrevented).toBe(false);
      expect(navTargets).toEqual([]);

      writeFromOtherTab(null);
      const on = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(on.defaultPrevented).toBe(true);
      expect(navTargets).toEqual(['/terminals']);
    });

    it('updates the hints in a mounted header', async () => {
      const el = await mountHeader({ currentPath: '/projects/p1' });
      const hints = (): number => el.shadowRoot?.querySelectorAll('.mode-shortcut').length ?? 0;
      expect(hints()).toBe(3);

      writeFromOtherTab('false');
      await el.updateComplete;
      expect(hints()).toBe(0);
    });

    it('stops listening for storage events once the header is removed', async () => {
      const el = await mountHeader({ currentPath: '/projects/p1' });
      const removeSpy = vi.spyOn(window, 'removeEventListener');
      try {
        el.remove();
        expect(removeSpy.mock.calls.some(([type]) => type === 'storage')).toBe(true);
      } finally {
        removeSpy.mockRestore();
      }
    });
  });

  describe('with a modal dialog', () => {
    function dialog(open: boolean): HTMLElement {
      const el = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
      el.open = open;
      return el;
    }

    it('leaves the key to an open dialog and does not switch', async () => {
      await mountHeader({ currentPath: '/projects/p1' });
      const dlg = dialog(true);
      const input = document.createElement('input');
      const seen = vi.fn();
      input.addEventListener('keydown', seen);
      dlg.appendChild(input);
      document.body.appendChild(dlg);

      const e = press(input, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();

      expect(e.defaultPrevented).toBe(false);
      expect(seen).toHaveBeenCalledTimes(1);
      expect(navTargets).toEqual([]);
    });

    it('sees an open dialog inside a shadow root', async () => {
      await mountHeader({ currentPath: '/projects/p1' });
      const host = document.createElement('div');
      host.attachShadow({ mode: 'open' }).appendChild(dialog(true));
      document.body.appendChild(host);

      const e = press(document.body, { code: 'Digit2', key: '2', ctrlKey: true });
      await settled();

      expect(e.defaultPrevented).toBe(false);
      expect(navTargets).toEqual([]);
    });

    it('switches when the dialog is closed', async () => {
      await mountHeader({ currentPath: '/projects/p1' });
      document.body.appendChild(dialog(false));

      const e = press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();

      expect(e.defaultPrevented).toBe(true);
      expect(navTargets).toEqual(['/terminals']);
    });
  });

  describe('hints', () => {
    function modeTooltips(el: ScionHeader): string[] {
      // The switch renders once per layout tier; the first is representative.
      const sw = el.shadowRoot?.querySelector('.mode-switch');
      return Array.from(sw?.querySelectorAll('sl-tooltip') ?? []).map(
        (t) => t.getAttribute('content') ?? ''
      );
    }

    function modeButtons(el: ScionHeader): Array<string | null> {
      const sw = el.shadowRoot?.querySelector('.mode-switch');
      return Array.from(sw?.querySelectorAll('button') ?? []).map((b) =>
        b.getAttribute('aria-keyshortcuts')
      );
    }

    function menuHints(el: ScionHeader): string[] {
      const menu = el.shadowRoot?.querySelector('sl-menu');
      return Array.from(menu?.querySelectorAll('.mode-shortcut') ?? []).map(
        (k) => k.textContent ?? ''
      );
    }

    it('shows Ctrl+N in the mode tooltips, aria-keyshortcuts and menu off macOS', async () => {
      const el = await mountHeader({ currentPath: '/projects/p1' });

      expect(modeTooltips(el)).toEqual([
        'Dashboard · Ctrl+1',
        'Chat · Ctrl+2',
        'Terminals (0) · Ctrl+3',
      ]);
      expect(modeButtons(el)).toEqual(['Control+1', 'Control+2', 'Control+3']);
      expect(menuHints(el)).toEqual(['Ctrl+1', 'Ctrl+2', 'Ctrl+3']);
    });

    it('shows the command symbol on macOS', async () => {
      setPlatform('MacIntel');
      const el = await mountHeader({ currentPath: '/projects/p1' });

      expect(modeTooltips(el)).toEqual(['Dashboard · ⌘1', 'Chat · ⌘2', 'Terminals (0) · ⌘3']);
      expect(modeButtons(el)).toEqual(['Meta+1', 'Meta+2', 'Meta+3']);
      expect(menuHints(el)).toEqual(['⌘1', '⌘2', '⌘3']);
    });

    it('drops the hints when the setting is off', async () => {
      setViewModeShortcutsEnabled(false);
      const el = await mountHeader({ currentPath: '/projects/p1' });

      expect(modeTooltips(el)).toEqual(['Dashboard', 'Chat', 'Terminals (0)']);
      expect(modeButtons(el)).toEqual([null, null, null]);
      expect(menuHints(el)).toEqual([]);

      // Control: turning it on brings the hints back without a remount.
      setViewModeShortcutsEnabled(true);
      await el.updateComplete;
      expect(menuHints(el)).toEqual(['Ctrl+1', 'Ctrl+2', 'Ctrl+3']);
    });

    it('drops the hints on a touch-primary device', async () => {
      stubTouchPrimary(true);
      const el = await mountHeader({ currentPath: '/projects/p1' });

      expect(modeTooltips(el)).toEqual(['Dashboard', 'Chat', 'Terminals (0)']);
      expect(menuHints(el)).toEqual([]);

      // Control: the shortcut itself still works for a hardware keyboard.
      press(document.body, { code: 'Digit3', key: '3', ctrlKey: true });
      await settled();
      expect(navTargets).toEqual(['/terminals']);
    });
  });
});
