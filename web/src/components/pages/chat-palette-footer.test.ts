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
 * The chat page's quick-switcher button, pinned below the spaces rail at
 * every width and below the members column in the phone layout: where it
 * renders, its accessible name and keyboard-shortcut attributes per input
 * modality, the focus-then-dispatch order of its click, and the style
 * contract (pinned footer, safe-area inset, 44px tap target). Geometry and
 * the real open path through shadow DOM live in e2e/chat-palette and
 * e2e/chat-mobile (Chromium).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../client/chat-palette-events.js';
import { FakeEventSource } from '../../client/__fixtures__/agent-store-harness.js';
import { TOUCH_PRIMARY_QUERY } from '../../utils/input-modality.js';
import { elementStyleRules } from './__fixtures__/css-rules.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const platform = vi.hoisted(() => ({ mac: false }));
vi.mock('../../utils/platform.js', () => ({ isMacPlatform: () => platform.mac }));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{"agents":[]}', { status: 200 }))),
  };
});

const MOBILE = '@media (max-width: 768px)';
const TAP_TARGET = '@media (max-width: 768px), (hover: none) and (pointer: coarse)';
const TOUCH = '@media (hover: none) and (pointer: coarse)';

let rules: Map<string, string>;

beforeAll(async () => {
  await import('./chat.js');
  rules = elementStyleRules('scion-page-chat');
  // See chat-palette-shortcut.test.ts: a connected page lazily imports
  // these in the background; warming them keeps that import from still
  // being in flight when the environment tears down.
  await Promise.all([
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-members.js'),
  ]);
});

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  document.body.innerHTML = '';
  platform.mac = false;
  vi.unstubAllGlobals();
});

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

async function mountPage(opts: { mobile?: boolean } = {}): Promise<any> {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  document.body.appendChild(el);
  // connectedCallback reads the layout from the media query; set it after.
  el.isMobileLayout = opts.mobile ?? false;
  await el.updateComplete;
  return el;
}

function railButton(page: any): HTMLButtonElement | null {
  return page.shadowRoot.querySelector('.v2-rail > .palette-footer > .palette-footer-btn');
}

function membersButton(page: any): HTMLButtonElement | null {
  return page.shadowRoot.querySelector('.v2-members > .palette-footer > .palette-footer-btn');
}

/** The last value a rule body gives `prop`, as the cascade would apply it. */
function decl(body: string | undefined, prop: string): string | undefined {
  if (!body) return undefined;
  let value: string | undefined;
  for (const part of body.split(';')) {
    const i = part.indexOf(':');
    if (i < 0) continue;
    if (part.slice(0, i).trim() === prop) value = part.slice(i + 1).trim();
  }
  return value;
}

describe('palette footer: where it renders', () => {
  it('wide layout: one button, at the bottom of the spaces rail, none in the members column', async () => {
    stubTouchPrimary(false);
    const page = await mountPage();

    expect(railButton(page)).not.toBeNull();
    // The footer is the rail's last child, below the list.
    expect(page.shadowRoot.querySelector('.v2-rail')?.lastElementChild?.className).toBe(
      'palette-footer'
    );
    expect(membersButton(page)).toBeNull();
    expect(page.shadowRoot.querySelectorAll('.palette-footer-btn')).toHaveLength(1);
  });

  it('phone layout: a button at the bottom of both the spaces and the members columns', async () => {
    stubTouchPrimary(true);
    const page = await mountPage({ mobile: true });

    expect(railButton(page)).not.toBeNull();
    expect(membersButton(page)).not.toBeNull();
    expect(page.shadowRoot.querySelector('.v2-members')?.lastElementChild?.className).toBe(
      'palette-footer'
    );
  });

  it('the members button follows the layout when it changes', async () => {
    stubTouchPrimary(false);
    const page = await mountPage({ mobile: true });
    expect(membersButton(page)).not.toBeNull();

    page.isMobileLayout = false;
    await page.updateComplete;

    expect(membersButton(page)).toBeNull();
    expect(railButton(page)).not.toBeNull();
  });
});

describe('palette footer: accessible name and shortcut attributes', () => {
  it('desktop (non-Mac): "Open quick switcher", Ctrl+K hint, title and Control+K', async () => {
    stubTouchPrimary(false);
    const btn = railButton(await mountPage());

    expect(btn?.getAttribute('type')).toBe('button');
    expect(btn?.getAttribute('aria-label')).toBe('Open quick switcher');
    expect(btn?.getAttribute('aria-haspopup')).toBe('dialog');
    expect(btn?.getAttribute('aria-keyshortcuts')).toBe('Control+K');
    expect(btn?.getAttribute('title')).toBe('Quick switcher (Ctrl+K)');
    expect(btn?.querySelector('.palette-footer-label')?.textContent?.trim()).toBe('Quick switcher');
    expect(btn?.querySelector('.palette-footer-shortcut')?.textContent?.trim()).toBe('Ctrl+K');
    expect(btn?.querySelector('sl-icon')?.getAttribute('name')).toBe('compass');
  });

  it('desktop (Mac): ⌘K hint and title, Meta+K', async () => {
    platform.mac = true;
    stubTouchPrimary(false);
    const btn = railButton(await mountPage());

    expect(btn?.getAttribute('aria-keyshortcuts')).toBe('Meta+K');
    expect(btn?.getAttribute('title')).toBe('Quick switcher (⌘K)');
    expect(btn?.querySelector('.palette-footer-shortcut')?.textContent?.trim()).toBe('⌘K');
  });

  it('touch: same accessible name, no title and no aria-keyshortcuts, on both buttons', async () => {
    stubTouchPrimary(true);
    const page = await mountPage({ mobile: true });

    for (const btn of [railButton(page), membersButton(page)]) {
      expect(btn?.getAttribute('aria-label')).toBe('Open quick switcher');
      expect(btn?.getAttribute('aria-haspopup')).toBe('dialog');
      expect(btn?.hasAttribute('aria-keyshortcuts')).toBe(false);
      expect(btn?.hasAttribute('title')).toBe(false);
    }
  });
});

describe('palette footer: click', () => {
  it('focuses the button, then dispatches CHAT_PALETTE_OPEN_REQUEST_EVENT with bubbles and composed set', async () => {
    // Order matters: _openPalette captures the deep active element as the
    // invoker synchronously. On Safari, which does not focus a clicked
    // button on its own, a dispatch-then-focus order would capture what
    // was focused before the tap (possibly the composer), and closing the
    // palette would pop the on-screen keyboard back open.
    stubTouchPrimary(false);
    const btn = railButton(await mountPage())!;
    const order: string[] = [];
    let received: CustomEvent | undefined;
    vi.spyOn(btn, 'focus').mockImplementation((opts?: FocusOptions) => {
      order.push(`focus:${JSON.stringify(opts)}`);
    });
    btn.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, (e) => {
      order.push('dispatch');
      received = e as CustomEvent;
    });

    btn.click();

    expect(order).toEqual(['focus:{"preventScroll":true}', 'dispatch']);
    expect(received!.bubbles).toBe(true);
    expect(received!.composed).toBe(true);
  });

  it('the members button sends the same request', async () => {
    stubTouchPrimary(true);
    const btn = membersButton(await mountPage({ mobile: true }))!;
    const received: string[] = [];
    btn.addEventListener(CHAT_PALETTE_OPEN_REQUEST_EVENT, () => received.push('open'));

    btn.click();

    expect(received).toEqual(['open']);
  });
});

describe('palette footer: style contract', () => {
  it('is pinned below a list that alone takes the remaining height', () => {
    expect(decl(rules.get('.palette-footer'), 'flex')).toBe('0 0 auto');
    for (const sel of [
      '.v2-rail scion-chat-space-rail',
      '.v2-rail .loading-rail',
      '.v2-members scion-chat-members',
    ]) {
      expect(decl(rules.get(sel), 'flex'), sel).toBe('1');
      expect(decl(rules.get(sel), 'min-height'), sel).toBe('0');
    }
  });

  it('takes the bottom safe-area inset, and the lists above it drop theirs', () => {
    expect(decl(rules.get('.palette-footer'), 'padding-bottom')).toContain(
      'env(safe-area-inset-bottom'
    );
    expect(decl(rules.get('.v2-rail'), '--chat-list-inset-bottom')).toBe('0px');
    expect(decl(rules.get(`${MOBILE} .v2-panels .v2-members`), '--chat-list-inset-bottom')).toBe(
      '0px'
    );
    // Side by side the members column has no footer, so its list keeps the inset.
    expect(decl(rules.get('.v2-members'), '--chat-list-inset-bottom')).toBeUndefined();
  });

  it('is full width with a 44px tap target on phones and touch devices', () => {
    expect(decl(rules.get('.palette-footer-btn'), 'width')).toBe('100%');
    expect(decl(rules.get(`${TAP_TARGET} .palette-footer-btn`), 'min-height')).toBe('44px');
  });

  it('hides the shortcut hint on touch-primary devices', () => {
    expect(decl(rules.get(`${TOUCH} .palette-footer-shortcut`), 'display')).toBe('none');
  });
});
