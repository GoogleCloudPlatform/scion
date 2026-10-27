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

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

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
