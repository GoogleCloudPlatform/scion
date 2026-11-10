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
 * Jump arrival in the chat thread: reduced motion, focus on the landed
 * message, and a popup menu opened while a jump is still settling.
 */

import { describe, it, expect, beforeEach, afterEach, onTestFinished, vi } from 'vitest';

const fakeStateManager = new (class extends EventTarget {
  currentScope = null;
  getAgent(): undefined {
    return undefined;
  }
})();
const apiFetch = vi.fn();

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  get navigateTo() {
    return vi.fn();
  },
  get stateManager() {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string) => Promise.resolve(fallback),
}));

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

const KEY = 'topic-1';
const MESSAGES = ['m1', 'm2', 'm3'].map((id, i) => ({
  id,
  sender: 'them@example.com',
  msg: `mock ${id}`,
  createdAt: `2026-01-01T00:0${i}:00Z`,
}));

function history(): Response {
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve({ messages: MESSAGES }),
  } as unknown as Response;
}

interface Internals {
  contextMenuMessage: { id: string } | null;
  contextMenuPosition: { x: number; y: number };
}

let reducedMotion = false;
let scrollIntoView: ReturnType<typeof vi.fn>;
const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;

async function mount(): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = KEY;
  document.body.appendChild(el);
  await vi.waitFor(() => expect(el.shadowRoot?.querySelector('#msg-m2')).not.toBeNull());
  return el;
}

function msgEl(el: ScionChatThread, id: string): HTMLElement {
  return el.shadowRoot!.querySelector<HTMLElement>(`#msg-${id}`)!;
}

/** Give an element a fixed viewport box (happy-dom has no layout). */
function placeAt(node: HTMLElement, top: number, height = 40): void {
  node.getBoundingClientRect = (): DOMRect =>
    ({
      top,
      bottom: top + height,
      left: 0,
      right: 600,
      width: 600,
      height,
      x: 0,
      y: top,
    }) as DOMRect;
}

beforeEach(() => {
  reducedMotion = false;
  apiFetch.mockReset();
  apiFetch.mockResolvedValue(history());
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query: string) =>
      ({
        matches: query.includes('prefers-reduced-motion') ? reducedMotion : false,
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
      }) as unknown as MediaQueryList
  );
  scrollIntoView = vi.fn();
  Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
    configurable: true,
    value: scrollIntoView,
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  if (originalScrollIntoView) {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: originalScrollIntoView,
    });
  } else {
    delete (HTMLElement.prototype as unknown as Record<string, unknown>).scrollIntoView;
  }
  document.body.innerHTML = '';
});

describe('scion-chat-thread jump arrival', () => {
  it('scrolls smoothly by default and instantly under reduced motion, highlighting either way', async () => {
    const el = await mount();
    await el.scrollToMessageById('m2');
    expect(scrollIntoView).toHaveBeenLastCalledWith({ behavior: 'smooth', block: 'center' });
    reducedMotion = true;
    await el.scrollToMessageById('m1');
    expect(scrollIntoView).toHaveBeenLastCalledWith({ behavior: 'auto', block: 'center' });
    expect(msgEl(el, 'm1').classList.contains('permalink-highlight')).toBe(true);
  });

  it('focuses the landed message only when asked, without adding it to the tab order for good', async () => {
    const el = await mount();
    await el.scrollToMessageById('m2');
    expect(el.shadowRoot!.activeElement).not.toBe(msgEl(el, 'm2'));
    expect(msgEl(el, 'm2').hasAttribute('tabindex')).toBe(false);

    await el.scrollToMessageById('m2', true, true);
    const target = msgEl(el, 'm2');
    expect(el.shadowRoot!.activeElement).toBe(target);
    expect(target.getAttribute('tabindex')).toBe('-1');
    target.blur();
    expect(target.hasAttribute('tabindex')).toBe(false);
  });

  it('a newer jump wins: only the latest target is focused', async () => {
    const el = await mount();
    const first = el.scrollToMessageById('m1', true, true);
    const second = el.scrollToMessageById('m3', true, true);
    await Promise.all([first, second]);
    expect(el.shadowRoot!.activeElement).toBe(msgEl(el, 'm3'));
    expect(msgEl(el, 'm1').hasAttribute('tabindex')).toBe(false);
  });
});

describe('scion-chat-thread popup menu while the thread scrolls', () => {
  async function openMenuOn(el: ScionChatThread, id: string): Promise<HTMLElement> {
    const scroller = el.shadowRoot!.querySelector<HTMLElement>('.messages-scroll')!;
    placeAt(scroller, 0, 600);
    placeAt(msgEl(el, id), 300);
    msgEl(el, id).dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 50, clientY: 310 })
    );
    await el.updateComplete;
    expect((el as unknown as Internals).contextMenuMessage?.id).toBe(id);
    return scroller;
  }

  it('moves the menu with its message on a scroll the user did not make (a jump settling)', async () => {
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    placeAt(msgEl(el, 'm2'), 260);
    scroller.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    const internals = el as unknown as Internals;
    expect(internals.contextMenuMessage?.id).toBe('m2');
    expect(internals.contextMenuPosition).toEqual({ x: 50, y: 270 });
  });

  it('closes when the message leaves the view', async () => {
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    placeAt(msgEl(el, 'm2'), -100);
    scroller.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    expect((el as unknown as Internals).contextMenuMessage).toBeNull();
  });

  it('closes on a scroll the user makes: wheel, a scroll key, a scrollbar grab', async () => {
    for (const gesture of ['wheel', 'key', 'scrollbar'] as const) {
      const el = await mount();
      const scroller = await openMenuOn(el, 'm2');
      if (gesture === 'wheel') {
        scroller.dispatchEvent(new WheelEvent('wheel', { bubbles: true, composed: true }));
      } else if (gesture === 'key') {
        document.body.dispatchEvent(
          new KeyboardEvent('keydown', { key: 'PageDown', bubbles: true })
        );
      } else {
        scroller.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true, composed: true }));
      }
      placeAt(msgEl(el, 'm2'), 280);
      scroller.dispatchEvent(new Event('scroll'));
      await el.updateComplete;
      expect((el as unknown as Internals).contextMenuMessage, gesture).toBeNull();
      document.body.innerHTML = '';
    }
  });

  it('a pointer down on a message (the right-click itself) is not a user scroll', async () => {
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    msgEl(el, 'm2').dispatchEvent(
      new PointerEvent('pointerdown', { bubbles: true, composed: true, button: 2 })
    );
    placeAt(msgEl(el, 'm2'), 290);
    scroller.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    expect((el as unknown as Internals).contextMenuMessage?.id).toBe('m2');
  });

  it('arrow keys inside the menu are not a user scroll: a settling scroll still moves it', async () => {
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    await el.updateComplete;
    const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
    const item = menu.querySelector<HTMLElement>('[role^="menuitem"]')!;
    item.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'ArrowDown',
        bubbles: true,
        composed: true,
        cancelable: true,
      })
    );
    placeAt(msgEl(el, 'm2'), 280);
    scroller.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    const internals = el as unknown as Internals;
    expect(internals.contextMenuMessage?.id).toBe('m2');
    expect(internals.contextMenuPosition).toEqual({ x: 50, y: 290 });
  });

  it('keys inside the menu do not cancel the jump settle re-check; the same key outside does', async () => {
    type Watch = {
      watchJumpScrollSettle(el: HTMLElement, id: string, align: ScrollLogicalPosition): void;
      _jumpScrollCleanup: (() => void) | null;
    };
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    const w = el as unknown as Watch;
    w.watchJumpScrollSettle(scroller, 'm2', 'center');
    expect(w._jumpScrollCleanup).not.toBeNull();
    const item = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"] [role^="menuitem"]')!;
    item.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: 'ArrowDown',
        bubbles: true,
        composed: true,
        cancelable: true,
      })
    );
    expect(w._jumpScrollCleanup).not.toBeNull();
    document.body.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true })
    );
    expect(w._jumpScrollCleanup).toBeNull();
  });

  it('stops following once the menu closes', async () => {
    const el = await mount();
    const scroller = await openMenuOn(el, 'm2');
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect((el as unknown as Internals).contextMenuMessage).toBeNull();
    const before = { ...(el as unknown as Internals).contextMenuPosition };
    placeAt(msgEl(el, 'm2'), 100);
    scroller.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    expect((el as unknown as Internals).contextMenuPosition).toEqual(before);
  });
});

describe('scion-chat-thread message menu keyboard contract', () => {
  it.each([
    { stars: false, returnsTo: 'the focused opener' },
    { stars: true, returnsTo: "the message's More actions button" },
  ])(
    'opens as a named menu with focus on its first item; Escape returns focus to $returnsTo (stars $stars)',
    async ({ stars }) => {
      (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
        'web.chat_starred_messages': stars,
      };
      onTestFinished(() => {
        delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
      });
      const el = await mount();
      const opener = document.createElement('button');
      document.body.appendChild(opener);
      opener.focus();
      msgEl(el, 'm2').dispatchEvent(
        new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 5, clientY: 5 })
      );
      await el.updateComplete;
      await el.updateComplete;
      const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
      expect(menu.getAttribute('aria-label')).toMatch(/^Message actions/);
      const items = [...menu.querySelectorAll('[role="menuitem"], [role="menuitemcheckbox"]')];
      expect(items.length).toBeGreaterThan(1);
      expect(el.shadowRoot!.activeElement).toBe(items[0]);
      menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
      expect(el.shadowRoot!.activeElement).toBe(items[1]);
      menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      await el.updateComplete;
      expect((el as unknown as Internals).contextMenuMessage).toBeNull();
      if (stars) {
        const row = msgEl(el, 'm2') as HTMLElement & { actionsButton: HTMLButtonElement | null };
        expect(row.actionsButton).not.toBeNull();
        expect(row.shadowRoot!.activeElement).toBe(row.actionsButton);
      } else {
        expect(document.activeElement).toBe(opener);
      }
    }
  );

  it('a click on the overlay closes without pulling focus back', async () => {
    const el = await mount();
    const opener = document.createElement('button');
    document.body.appendChild(opener);
    opener.focus();
    msgEl(el, 'm2').dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 5, clientY: 5 })
    );
    await el.updateComplete;
    el.shadowRoot!.querySelector<HTMLElement>('.context-menu-overlay')!.click();
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 5));
    expect((el as unknown as Internals).contextMenuMessage).toBeNull();
    expect(document.activeElement).not.toBe(opener);
  });
});

describe('scion-chat-thread message list tab stop (More actions)', () => {
  type Msg = import('./chat-message.js').ScionChatMessage;

  function setFlag(on: boolean): void {
    (window as unknown as { __SCION_FEATURES__?: Record<string, boolean> }).__SCION_FEATURES__ = {
      'web.chat_starred_messages': on,
    };
  }

  afterEach(() => {
    delete (window as unknown as { __SCION_FEATURES__?: unknown }).__SCION_FEATURES__;
  });

  function rows(el: ScionChatThread): Msg[] {
    return [...el.shadowRoot!.querySelectorAll<Msg>('scion-chat-message')];
  }

  async function settled(el: ScionChatThread): Promise<void> {
    await el.updateComplete;
    await Promise.all(rows(el).map((m) => m.updateComplete));
  }

  function tabStops(el: ScionChatThread): string[] {
    return rows(el)
      .filter((m) => m.actionsButton?.getAttribute('tabindex') === '0')
      .map((m) => m.messageId);
  }

  function focusedRow(el: ScionChatThread): string {
    // happy-dom's ShadowRoot.activeElement throws while focus is on a
    // detached node (a closed menu's row): treat that as no row focused.
    const has = (m: Msg): boolean => {
      try {
        return !!m.actionsButton && m.shadowRoot!.activeElement === m.actionsButton;
      } catch {
        return false;
      }
    };
    return rows(el).find(has)?.messageId ?? '';
  }

  function press(m: Msg, key: string, shiftKey = false): void {
    m.actionsButton!.dispatchEvent(
      new KeyboardEvent('keydown', {
        key,
        shiftKey,
        bubbles: true,
        composed: true,
        cancelable: true,
      })
    );
  }

  it('is off while the experiment is off', async () => {
    setFlag(false);
    const el = await mount();
    await settled(el);
    expect(rows(el).every((m) => m.actionsButton === null)).toBe(true);
  });

  it('puts exactly one More actions button in the tab order, named for its message', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    expect(tabStops(el)).toEqual(['m3']);
    const b = rows(el)[2].actionsButton!;
    expect(b.getAttribute('aria-label')).toMatch(/^More actions, message from /);
    expect(b.getAttribute('aria-haspopup')).toBe('menu');
    expect(b.getAttribute('aria-expanded')).toBe('false');
    // Only the active button carries the key help.
    expect(b.getAttribute('aria-describedby')).toBe('actions-help');
    expect(rows(el)[2].shadowRoot!.getElementById('actions-help')?.textContent).toContain(
      'Tab leaves'
    );
    expect(rows(el)[0].actionsButton!.hasAttribute('aria-describedby')).toBe(false);
  });

  it('with no stop yet, a message taller than the view takes it when no message fits inside', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    placeAt(el.shadowRoot!.querySelector<HTMLElement>('.messages-scroll')!, 0, 600);
    placeAt(msgEl(el, 'm1'), -900, 200);
    placeAt(msgEl(el, 'm2'), -100, 900);
    placeAt(msgEl(el, 'm3'), 900);
    const state = el as unknown as { _activeActionsId: string; _activeActionsIndex: number };
    state._activeActionsId = '';
    state._activeActionsIndex = -1;
    el.requestUpdate();
    await settled(el);
    expect(tabStops(el)).toEqual(['m2']);
  });

  it('Up/Down/Home/End move the stop and focus through the rendered messages', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    rows(el)[2].actionsButton!.focus();
    press(rows(el)[2], 'ArrowUp');
    await settled(el);
    await vi.waitFor(() => expect(focusedRow(el)).toBe('m2'));
    expect(tabStops(el)).toEqual(['m2']);
    press(rows(el)[1], 'Home');
    await vi.waitFor(() => expect(focusedRow(el)).toBe('m1'));
    press(rows(el)[0], 'ArrowUp');
    await settled(el);
    expect(focusedRow(el)).toBe('m1');
    press(rows(el)[0], 'End');
    await vi.waitFor(() => expect(focusedRow(el)).toBe('m3'));
    expect(tabStops(el)).toEqual(['m3']);
  });

  it('activating the button opens the menu there; Escape returns focus to it', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    const row = rows(el)[1];
    row.actionsButton!.focus();
    row.actionsButton!.click();
    await settled(el);
    expect((el as unknown as Internals).contextMenuMessage?.id).toBe('m2');
    expect(row.actionsButton!.getAttribute('aria-expanded')).toBe('true');
    expect(tabStops(el)).toEqual(['m2']);
    const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
    expect(el.shadowRoot!.activeElement).toBe(menu.querySelector('[role^="menuitem"]'));
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settled(el);
    expect((el as unknown as Internals).contextMenuMessage).toBeNull();
    expect(row.actionsButton!.getAttribute('aria-expanded')).toBe('false');
    expect(focusedRow(el)).toBe('m2');
  });

  it('Shift+F10 and the ContextMenu key open the menu from the button', async () => {
    setFlag(true);
    for (const [k, shift] of [
      ['F10', true],
      ['ContextMenu', false],
    ] as const) {
      const el = await mount();
      await settled(el);
      press(rows(el)[0], k, shift);
      await settled(el);
      expect((el as unknown as Internals).contextMenuMessage?.id, k).toBe('m1');
      document.body.innerHTML = '';
    }
  });

  it('a right-click makes that message the tab stop and its button the menu opener', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    rows(el)[0].dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 5, clientY: 5 })
    );
    await settled(el);
    expect(tabStops(el)).toEqual(['m1']);
    const menu = el.shadowRoot!.querySelector<HTMLElement>('[role="menu"]')!;
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settled(el);
    expect(focusedRow(el)).toBe('m1');
  });

  it('a live message does not move the stop or focus', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    rows(el)[0].actionsButton!.focus();
    press(rows(el)[0], 'ArrowDown');
    await vi.waitFor(() => expect(focusedRow(el)).toBe('m2'));
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: {
          state: {},
          data: {
            id: 'm4',
            conversationKey: KEY,
            threadId: KEY,
            sender: 'them@example.com',
            msg: 'live',
            createdAt: '2026-01-01T00:09:00Z',
          },
        },
      })
    );
    await new Promise((r) => setTimeout(r, 10));
    await settled(el);
    expect(tabStops(el)).toEqual(['m2']);
    expect(focusedRow(el)).toBe('m2');
  });

  it('removing the focused message hands the stop and focus to the closest one; an unrelated removal moves nothing', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    const internals = el as unknown as { messages: Array<{ id: string }> };
    rows(el)[1].actionsButton!.focus();
    await settled(el);
    // An unrelated row goes.
    internals.messages = internals.messages.filter((m) => m.id !== 'm1');
    await settled(el);
    expect(focusedRow(el)).toBe('m2');
    expect(tabStops(el)).toEqual(['m2']);
    // The focused row goes.
    internals.messages = internals.messages.filter((m) => m.id !== 'm2');
    await settled(el);
    await vi.waitFor(() => expect(focusedRow(el)).toBe('m3'));
    expect(tabStops(el)).toEqual(['m3']);
  });

  it('a jump makes its message the stop, and Shift+F10 on the focused message opens its menu', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    await el.scrollToMessageById('m1', true, true);
    await settled(el);
    expect(tabStops(el)).toEqual(['m1']);
    const host = rows(el)[0];
    expect(el.shadowRoot!.activeElement).toBe(host);
    host.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'F10', shiftKey: true, bubbles: true, cancelable: true })
    );
    await settled(el);
    expect((el as unknown as Internals).contextMenuMessage?.id).toBe('m1');
  });

  it('keys from inside message content are left alone', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    const host = rows(el)[1];
    const inner = document.createElement('a');
    host.shadowRoot!.appendChild(inner);
    const e = new KeyboardEvent('keydown', {
      key: 'F10',
      shiftKey: true,
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    inner.dispatchEvent(e);
    await settled(el);
    expect(e.defaultPrevented).toBe(false);
    expect((el as unknown as Internals).contextMenuMessage).toBeNull();
    const arrow = new KeyboardEvent('keydown', {
      key: 'ArrowUp',
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    inner.dispatchEvent(arrow);
    expect(arrow.defaultPrevented).toBe(false);
  });

  it('a headerless starred row keeps its star in the gutter and the button inside the bubble', async () => {
    setFlag(true);
    const el = await mount();
    await settled(el);
    const row = rows(el)[1];
    row.starred = true;
    await row.updateComplete;
    const bubble = row.shadowRoot!.querySelector('.bubble')!;
    const star = bubble.querySelector('.star-meta .star-indicator');
    expect(star).not.toBeNull();
    expect(star!.closest('.bubble-content')).toBeNull();
    expect(row.actionsButton!.parentElement!.classList.contains('bubble-content')).toBe(true);
    row.starred = false;
    await row.updateComplete;
    expect(bubble.querySelector('.star-meta')).toBeNull();
  });
});
