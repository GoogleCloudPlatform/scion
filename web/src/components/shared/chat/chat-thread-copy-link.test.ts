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
 * The message menu's link items: "Copy link to msg" (a link to the message)
 * on every message, and "Copy link address" only when the menu is raised on
 * an http, https or mailto link in the message.
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
const { showToast } = await import('../../../utils/toast.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

const DOCS = 'https://example.com/docs?q=1';
const MESSAGES = [
  { id: 'm1', sender: 'agent:bot', msg: 'plain words only' },
  { id: 'm2', sender: 'agent:bot', msg: `see [the docs](${DOCS}) here` },
].map((m, i) => ({ ...m, createdAt: `2026-01-01T00:0${i}:00Z` }));

let sheet = false;
let touch = false;
let writeText: ReturnType<typeof vi.fn>;
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');

beforeEach(() => {
  vi.mocked(showToast).mockClear();
  sheet = false;
  touch = false;
  apiFetch.mockReset();
  apiFetch.mockResolvedValue({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ messages: MESSAGES }),
  } as unknown as Response);
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
  writeText = vi.fn().mockResolvedValue(undefined);
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
});

afterEach(() => {
  vi.restoreAllMocks();
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
  document.body.innerHTML = '';
});

async function mount(): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = 'topic-1';
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(body(el, 'm2').querySelector(`a[href="${DOCS}"]`)).not.toBeNull();
  });
  return el;
}

/** The rendered markdown of a message, inside its own shadow root. */
function body(el: ScionChatThread, id: string): HTMLElement {
  const msg = el.shadowRoot?.querySelector(`#msg-${id}`);
  const content = msg?.shadowRoot?.querySelector<HTMLElement>('.md-content');
  if (!content) throw new Error(`message ${id} has not rendered`);
  return content;
}

/** Right-click `target` (composed, so it crosses the message's shadow root). */
async function rightClick(el: ScionChatThread, target: Element): Promise<void> {
  target.dispatchEvent(
    new MouseEvent('contextmenu', {
      bubbles: true,
      composed: true,
      cancelable: true,
      clientX: 20,
      clientY: 20,
    })
  );
  await el.updateComplete;
  await el.updateComplete;
}

function popupRows(el: ScionChatThread): HTMLElement[] {
  return [...el.shadowRoot!.querySelectorAll<HTMLElement>('.context-menu-item')];
}

function labels(rows: HTMLElement[]): string[] {
  return rows.map((r) => r.textContent?.trim() ?? '');
}

/** A pointer event of `type` at `target`, composed so it crosses shadow roots. */
function press(target: Element, type = 'pointerdown'): void {
  target.dispatchEvent(new Event(type, { bubbles: true, composed: true }));
}

/** The message host element itself. */
function host(el: ScionChatThread, id: string): HTMLElement {
  return el.shadowRoot!.querySelector<HTMLElement>(`#msg-${id}`)!;
}

/** A contextmenu raised as a PointerEvent of `pointerType`, at a real position. */
async function pointerMenu(
  el: ScionChatThread,
  target: Element,
  pointerType: string
): Promise<void> {
  target.dispatchEvent(
    new PointerEvent('contextmenu', {
      bubbles: true,
      composed: true,
      cancelable: true,
      clientX: 20,
      clientY: 20,
      pointerType,
    })
  );
  await el.updateComplete;
  await el.updateComplete;
}

function closeMenu(): void {
  document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
}

describe('message menu link items', () => {
  it('names the message link "Copy link to msg" and offers no address off a link', async () => {
    const el = await mount();
    await rightClick(el, body(el, 'm1'));
    const shown = labels(popupRows(el));
    expect(shown).toContain('Copy link to msg');
    expect(shown).not.toContain('Copy link');
    expect(shown).not.toContain('Copy link address');
  });

  it('offers no address for the text around a link in the same message', async () => {
    const el = await mount();
    await rightClick(el, body(el, 'm2'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('adds "Copy link address" after "Copy link to msg" on a link and copies its href', async () => {
    const el = await mount();
    const link = body(el, 'm2').querySelector(`a[href="${DOCS}"]`)!;
    await rightClick(el, link);
    const rows = popupRows(el);
    const shown = labels(rows);
    expect(shown.indexOf('Copy link address')).toBe(shown.indexOf('Copy link to msg') + 1);

    rows[shown.indexOf('Copy link address')].click();
    expect(writeText).toHaveBeenCalledWith(DOCS);
    await el.updateComplete;
    expect(popupRows(el)).toHaveLength(0);
  });

  it('shows the failure toast, without throwing, when the Clipboard API is absent', async () => {
    const el = await mount();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    const internals = el as unknown as {
      copyLinkAddressAction(href: string): { run: () => void };
    };
    expect(() => internals.copyLinkAddressAction(DOCS).run()).not.toThrow();

    await rightClick(el, body(el, 'm2').querySelector(`a[href="${DOCS}"]`)!);
    const rows = popupRows(el);
    rows[labels(rows).indexOf('Copy link address')].click();
    await el.updateComplete;
    expect(popupRows(el)).toHaveLength(0);
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger'));
    expect(showToast).not.toHaveBeenCalledWith('Copied', 'success');
  });

  it('still copies the message link from a menu raised on a link', async () => {
    const el = await mount();
    await rightClick(el, body(el, 'm2').querySelector('a')!);
    const rows = popupRows(el);
    rows[labels(rows).indexOf('Copy link to msg')].click();
    expect(writeText).toHaveBeenCalledWith(
      `${window.location.origin}${window.location.pathname}#msg-m2`
    );
  });

  it('forgets the link when the menu closes and is reopened off a link', async () => {
    const el = await mount();
    await rightClick(el, body(el, 'm2').querySelector('a')!);
    expect(labels(popupRows(el))).toContain('Copy link address');
    closeMenu();
    await el.updateComplete;
    await rightClick(el, body(el, 'm1'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('offers no address for a javascript: link', async () => {
    const el = await mount();
    const bad = document.createElement('a');
    bad.setAttribute('href', 'javascript:void(0)');
    bad.textContent = 'bad';
    body(el, 'm1').appendChild(bad);
    await rightClick(el, bad);
    const shown = labels(popupRows(el));
    expect(shown).toContain('Copy link to msg');
    expect(shown).not.toContain('Copy link address');
  });

  it('offers the same items in the mobile sheet', async () => {
    sheet = true;
    const el = await mount();
    await rightClick(el, body(el, 'm2').querySelector('a')!);
    const actionSheet = el.shadowRoot!.querySelector('scion-action-sheet') as HTMLElement & {
      items: Array<{ id: string; label: string }>;
    };
    const items = actionSheet.items.map((i) => i.label);
    expect(items.indexOf('Copy link address')).toBe(items.indexOf('Copy link to msg') + 1);
    actionSheet.dispatchEvent(
      new CustomEvent('action-sheet-select', { detail: { id: 'copy-link-address' } })
    );
    expect(writeText).toHaveBeenCalledWith(DOCS);
  });
});

describe('message menu link items: a press on a link, then another open path', () => {
  function docsLink(el: ScionChatThread): Element {
    return body(el, 'm2').querySelector(`a[href="${DOCS}"]`)!;
  }

  it('offers the pressed link when the menu opens on the message host', async () => {
    const el = await mount();
    press(docsLink(el));
    press(docsLink(el), 'pointerup');
    await rightClick(el, host(el, 'm2'));
    const rows = popupRows(el);
    const shown = labels(rows);
    expect(shown.indexOf('Copy link address')).toBe(shown.indexOf('Copy link to msg') + 1);
    rows[shown.indexOf('Copy link address')].click();
    expect(writeText).toHaveBeenCalledWith(DOCS);
  });

  it('offers the pressed link when a touch long-press arrives as a tap', async () => {
    touch = true;
    sheet = true;
    const el = await mount();
    press(docsLink(el));
    press(docsLink(el), 'pointercancel');
    host(el, 'm2').dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true, clientX: 20, clientY: 20 })
    );
    await el.updateComplete;
    const actionSheet = el.shadowRoot!.querySelector('scion-action-sheet') as HTMLElement & {
      items: Array<{ id: string; label: string }>;
    };
    expect(actionSheet.items.map((i) => i.label)).toContain('Copy link address');
  });

  it.each(['touch', ''])(
    'offers the pressed link on the tap path for a PointerEvent click (pointerType %j)',
    async (pointerType) => {
      touch = true;
      sheet = true;
      const el = await mount();
      press(docsLink(el));
      press(docsLink(el), 'pointerup');
      host(el, 'm2').dispatchEvent(
        new PointerEvent('click', {
          bubbles: true,
          composed: true,
          clientX: 20,
          clientY: 20,
          pointerType,
        })
      );
      await el.updateComplete;
      const actionSheet = el.shadowRoot!.querySelector('scion-action-sheet') as HTMLElement & {
        items: Array<{ id: string; label: string }>;
      };
      expect(actionSheet.items.map((i) => i.label)).toContain('Copy link address');
    }
  );

  it('forgets a press older than the window', async () => {
    const el = await mount();
    const now = Date.now();
    const clock = vi.spyOn(Date, 'now').mockReturnValue(now);
    press(docsLink(el));
    clock.mockReturnValue(now + 1500);
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('forgets the link once a later press lands elsewhere', async () => {
    const el = await mount();
    press(docsLink(el));
    press(body(el, 'm2'));
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('does not carry a pressed link into a menu on another message', async () => {
    const el = await mount();
    press(docsLink(el));
    await rightClick(el, host(el, 'm1'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('uses the pressed link once: the next menu on plain text has no address', async () => {
    const el = await mount();
    press(docsLink(el));
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).toContain('Copy link address');
    closeMenu();
    await el.updateComplete;
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('restarts the window at the end of the press', async () => {
    const el = await mount();
    const now = Date.now();
    const clock = vi.spyOn(Date, 'now').mockReturnValue(now);
    press(docsLink(el));
    clock.mockReturnValue(now + 900);
    press(docsLink(el), 'pointerup');
    clock.mockReturnValue(now + 1800);
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).toContain('Copy link address');
  });

  it('forgets a press just past the window (1001 ms, no pointerup)', async () => {
    const el = await mount();
    const now = Date.now();
    const clock = vi.spyOn(Date, 'now').mockReturnValue(now);
    press(docsLink(el));
    clock.mockReturnValue(now + 1001);
    await rightClick(el, host(el, 'm2'));
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('ignores a pressed link for a keyboard PointerEvent menu (empty pointerType)', async () => {
    const el = await mount();
    press(docsLink(el));
    await pointerMenu(el, host(el, 'm2'), '');
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });

  it('uses a pressed link for a touch contextmenu (Android long-press)', async () => {
    const el = await mount();
    press(docsLink(el));
    await pointerMenu(el, host(el, 'm2'), 'touch');
    expect(labels(popupRows(el))).toContain('Copy link address');
  });

  it('ignores a pressed link for a menu the keyboard raised', async () => {
    const el = await mount();
    press(docsLink(el));
    host(el, 'm2').dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, composed: true, cancelable: true })
    );
    await el.updateComplete;
    await el.updateComplete;
    expect(labels(popupRows(el))).not.toContain('Copy link address');
  });
});

describe('message menu copy items: feedback toasts', () => {
  const MSG_LINK = `${window.location.origin}${window.location.pathname}#msg-m2`;
  const ITEMS: Array<[string, string]> = [
    ['Copy text', `see [the docs](${DOCS}) here`],
    ['Copy link to msg', MSG_LINK],
    ['Copy link address', DOCS],
  ];

  async function clickItem(el: ScionChatThread, label: string): Promise<void> {
    await rightClick(el, body(el, 'm2').querySelector(`a[href="${DOCS}"]`)!);
    const rows = popupRows(el);
    const index = labels(rows).indexOf(label);
    expect(index).toBeGreaterThanOrEqual(0);
    expect(() => rows[index].click()).not.toThrow();
    await el.updateComplete;
    expect(popupRows(el)).toHaveLength(0);
  }

  it.each(ITEMS)('"%s" copies and shows "Copied"', async (label, text) => {
    const el = await mount();
    await clickItem(el, label);
    expect(writeText).toHaveBeenCalledWith(text);
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledWith('Copied', 'success'));
    expect(showToast).toHaveBeenCalledTimes(1);
  });

  it.each(ITEMS)('"%s" shows "Could not copy" when the Clipboard API is absent', async (label) => {
    const el = await mount();
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
    await clickItem(el, label);
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger'));
    expect(showToast).toHaveBeenCalledTimes(1);
  });

  it.each(ITEMS)('"%s" shows "Could not copy" when the write is rejected', async (label) => {
    const el = await mount();
    writeText.mockRejectedValue(new Error('denied'));
    await clickItem(el, label);
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger'));
    expect(showToast).toHaveBeenCalledTimes(1);
  });
});
