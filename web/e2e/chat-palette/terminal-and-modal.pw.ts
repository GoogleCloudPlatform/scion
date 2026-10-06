// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Chromium, real xterm: hidden mounted chat plus terminal Ctrl+K yields
 * normal PTY control-K and zero palette state/fetch changes; Meta+K does not
 * open the chat palette. An unrelated open dialog prevents activation.
 */

import { test, expect, type Page } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';
import type {} from './fixture.js';

type Frame = { type: string; data?: string };

/** Real xterm/PTY wiring: a routed WebSocket standing in for the Hub's PTY endpoint (mirrors e2e/terminal-pane). */
async function setupTerminal(page: Page) {
  const frames: Frame[] = [];
  await page.addInitScript(() => {
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });
  await page.routeWebSocket('**/pty?*', (socket) => {
    socket.onMessage((message) => frames.push(JSON.parse(String(message)) as Frame));
    // The client only reaches connection:'connected' on its first inbound
    // data frame (client/terminal-sessions.ts: "tmux sends a redraw on
    // attach, so it arrives promptly") — simulate that redraw so tests can
    // wait for a real connected state instead of guessing a timeout.
    socket.send(JSON.stringify({ type: 'data', data: Buffer.from('').toString('base64') }));
  });
  return {
    frames,
    input(): string[] {
      return frames
        .filter((f) => f.type === 'data')
        .map((f) => Buffer.from(f.data!, 'base64').toString());
    },
  };
}

async function gotoHiddenChatWithTerminal(page: Page, query = '') {
  const requests = await setupApiMocks(page);
  const terminal = await setupTerminal(page);
  await page.goto(`/e2e/chat-palette/fixture.html${query}`, { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.evaluate(() => window.chatPaletteFixture.hideChatShowTerminal());
  const helperTextarea = page.locator('.xterm-helper-textarea');
  await helperTextarea.waitFor({ state: 'attached' });
  // Real attach flow: agent fetch -> preflight -> PTY WebSocket. A keystroke
  // sent before this settles can be silently dropped by xterm's onData
  // handler, which is real behavior, not a fixture artifact — so tests that
  // need to observe the outbound byte wait for it, same as e2e/terminal-pane.
  await expect
    .poll(() => page.evaluate(() => window.chatPaletteFixture.terminalConnection()))
    .toBe('connected');
  return { requests, terminal, helperTextarea };
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

/**
 * `document.body.focus()` is a no-op (body is not focusable), so it can
 * never move focus out of a real terminal. This walks *down* through nested
 * shadow roots to the actual deepest focused element (`document.activeElement`
 * only reports the shadow host at each boundary) and blurs that element for
 * real, then hands back where focus landed so the caller can assert it left
 * the terminal.
 */
async function blurDeepActiveElementAndReport(page: Page) {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el) {
      const inner = (el as HTMLElement).shadowRoot?.activeElement ?? null;
      if (!inner) break;
      el = inner;
    }
    (el as HTMLElement | null)?.blur?.();
    const after = document.activeElement;
    return { blurredTag: el?.tagName ?? null, activeAfterBlur: after?.tagName ?? null };
  });
}

test('Ctrl+K in the terminal reaches the PTY and makes zero palette state/fetch changes', async ({
  page,
}) => {
  const { requests, terminal, helperTextarea } = await gotoHiddenChatWithTerminal(page);
  const requestCountBefore = requests.length;

  await helperTextarea.press('Control+k');
  await page.waitForTimeout(150);

  // The PTY got the real control-K byte (0x0b).
  expect(terminal.input().join('')).toContain('\x0b');
  // The (invisible) chat page made no new requests and never opened.
  expect(requests.length).toBe(requestCountBefore);
  const paletteOpen = await page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
  expect(paletteOpen).toBe(false);
});

test('Meta+K in the terminal does not reach the PTY and does not open the chat palette', async ({
  page,
}) => {
  const { terminal, helperTextarea } = await gotoHiddenChatWithTerminal(page);

  await helperTextarea.focus();
  await page.keyboard.press('Meta+k');
  await page.waitForTimeout(150);

  expect(terminal.input().join('')).not.toContain('\x0b');
  await page.evaluate(() => window.chatPaletteFixture.showChatHideTerminal());
  await expect(paletteDialog(page)).toBeHidden();
});

test('returning to chat after a terminal Ctrl+K never reveals a palette', async ({ page }) => {
  const { helperTextarea } = await gotoHiddenChatWithTerminal(page);
  await helperTextarea.focus();
  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);

  await page.evaluate(() => window.chatPaletteFixture.showChatHideTerminal());

  await expect(paletteDialog(page)).toBeHidden();
});

test('an unrelated open sl-dialog prevents the shortcut from activating', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.evaluate(async () => {
    await customElements.whenDefined('sl-dialog');
    const dialog = document.createElement('sl-dialog') as HTMLElement & {
      show?: () => Promise<void>;
      open?: boolean;
      updateComplete?: Promise<unknown>;
    };
    dialog.setAttribute('label', 'Unrelated dialog');
    document.body.appendChild(dialog);
    // Let the dialog's own first update (open=false, matching a real "closed
    // then opened" sequence) finish before flipping `open` — otherwise the
    // connect and the open-write can land in the same update, and Shoelace
    // never treats it as a transition (no sl-show at all). Real callers
    // always hit this naturally, since a `?open=${cond}` binding starts
    // false on the first render.
    await dialog.updateComplete;
    dialog.open = true;
  });
  // The modal guard is a live DOM query run at keydown time (not an
  // event-tracked set — see shared/open-modal.ts's hasOpenModalDescendant),
  // so no wait is strictly required for the guard itself; this margin just lets the
  // dialog's own open-transition and sl-show settle before we probe it.
  await page.waitForTimeout(150);

  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);

  await expect(paletteDialog(page)).toBeHidden();
});

test('a born-open sl-dialog (open set before connecting) blocks Ctrl+K', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.evaluate(async () => {
    await customElements.whenDefined('sl-dialog');
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.setAttribute('label', 'Born-open dialog');
    // Set `open` *before* connecting: Shoelace's `@watch('open')` only fires
    // on a false->true transition, so a dialog created already open (as
    // several real chat dialogs render behind a conditional) never fires
    // sl-show at all. The live modal query must still see it as open.
    dialog.open = true;
    document.body.appendChild(dialog);
  });
  await page.waitForTimeout(150);

  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);

  await expect(paletteDialog(page)).toBeHidden();
});

test('removing a born-open dialog restores Ctrl+K for the palette', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.evaluate(async () => {
    await customElements.whenDefined('sl-dialog');
    const dialog = document.createElement('sl-dialog') as HTMLElement & { open?: boolean };
    dialog.id = 'born-open-dialog';
    dialog.setAttribute('label', 'Born-open dialog');
    dialog.open = true;
    document.body.appendChild(dialog);
  });
  await page.waitForTimeout(150);
  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);
  await expect(paletteDialog(page)).toBeHidden(); // still blocked

  await page.evaluate(() => document.getElementById('born-open-dialog')?.remove());
  await page.waitForTimeout(150);

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();
});

test('a toast (sl-alert) opening while the palette is open leaves it open', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  await page.evaluate(async () => {
    await customElements.whenDefined('sl-alert');
    const alert = document.createElement('sl-alert') as HTMLElement & {
      open?: boolean;
      updateComplete?: Promise<unknown>;
    };
    alert.textContent = 'Something happened';
    document.body.appendChild(alert);
    await alert.updateComplete;
    alert.open = true; // real toasts open the same way via sl-alert.toast()
  });
  await page.waitForTimeout(150);

  await expect(paletteDialog(page)).toBeVisible();
  const paletteOpen = await page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
  expect(paletteOpen).toBe(true);
});

test('a real modal dialog opening while the palette is open closes it', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  await page.evaluate(async () => {
    await customElements.whenDefined('sl-dialog');
    const dialog = document.createElement('sl-dialog') as HTMLElement & {
      open?: boolean;
      updateComplete?: Promise<unknown>;
    };
    dialog.setAttribute('label', 'Unrelated dialog');
    document.body.appendChild(dialog);
    await dialog.updateComplete;
    dialog.open = true;
  });

  const paletteOpenState = () =>
    page.evaluate(
      () =>
        (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
          .v2PaletteOpen
    );
  await expect.poll(paletteOpenState, { timeout: 2_000 }).toBe(false);
});

test('chat is genuinely hidden (display:none) while on /terminals — Ctrl+K with focus actually outside the terminal has zero effect', async ({
  page,
}) => {
  const { requests } = await gotoHiddenChatWithTerminal(page);
  const requestCountBefore = requests.length;

  // Actually move focus out of the terminal — `document.body.focus()` is a
  // no-op (body isn't focusable), so leaving focus on the xterm helper
  // textarea would only ever exercise the terminal-surface guard, not the
  // route/visibility guards this test targets.
  const { activeAfterBlur } = await blurDeepActiveElementAndReport(page);
  expect(activeAfterBlur).toBe('BODY');
  expect(
    await page.evaluate(() => getComputedStyle(document.getElementById('chat-outlet')!).display)
  ).toBe('none');
  expect(await page.evaluate(() => location.pathname)).toContain('/terminals/');

  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);

  // Request count, not v2PaletteOpen, is the discriminating signal here:
  // the 250ms open-palette visibility watchdog would otherwise mask a
  // guard regression by closing an already-opened palette before this
  // assertion runs.
  expect(requests.length).toBe(requestCountBefore);
});

test('a real xterm on /chat with chat visible consumes Ctrl+K before it reaches the document listener', async ({
  page,
}) => {
  // This does NOT isolate the terminal-surface (composedPath) guard in
  // chat.ts — xterm's own keydown handling stops propagation of the 'k'
  // keydown before it ever bubbles to document (capture+bubble document
  // listeners see ["capture","Control"], ["bubble","Control"],
  // ["capture","k"] — no bubble "k" reaches document), so the shortcut is
  // blocked here regardless of whether chat.ts's guard exists at all. What
  // this test actually proves: mounting
  // a terminal *alongside* chat (route stays /chat, chat stays visible)
  // and focusing its real xterm surface has zero palette effect, via
  // whichever layer causes that (xterm's own propagation stop, defense in
  // depth). See the next test for the guard itself, isolated for real.
  const requests = await setupApiMocks(page);
  const terminal = await setupTerminal(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.evaluate(() => window.chatPaletteFixture.showTerminalAlongsideChat());
  const helperTextarea = page.locator('.xterm-helper-textarea');
  await helperTextarea.waitFor({ state: 'attached' });
  await expect
    .poll(() => page.evaluate(() => window.chatPaletteFixture.terminalConnection()))
    .toBe('connected');
  expect(await page.evaluate(() => location.pathname)).toBe('/chat');
  expect(
    await page.evaluate(() => getComputedStyle(document.getElementById('chat-outlet')!).display)
  ).not.toBe('none');
  const requestCountBefore = requests.length;

  await helperTextarea.press('Control+k');
  await page.waitForTimeout(150);

  expect(terminal.input().join('')).toContain('\x0b');
  expect(requests.length).toBe(requestCountBefore);
});

test("chat.ts's terminal-surface guard itself blocks a keydown whose composedPath includes the terminal pane, on /chat with chat visible", async ({
  page,
}) => {
  // The previous test shows xterm's own propagation stop already prevents
  // Ctrl+K from a *focused xterm textarea* reaching document — which means
  // it can never discriminate whether chat.ts's own terminal-surface guard
  // (_eventFromTerminalSurface, checking composedPath for
  // SCION-TERMINAL-PANE / .xterm) does anything at all. This test bypasses
  // xterm's propagation stop entirely by dispatching a synthetic, composed,
  // bubbling keydown directly on the <scion-terminal-pane> host element
  // itself (not the xterm helper textarea) — a keydown that *does* reach
  // document, with a composedPath the guard must reject on its own.
  const requests = await setupApiMocks(page);
  await setupTerminal(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.evaluate(() => window.chatPaletteFixture.showTerminalAlongsideChat());
  await page.waitForSelector('scion-terminal-pane');
  expect(await page.evaluate(() => location.pathname)).toBe('/chat');
  expect(
    await page.evaluate(() => getComputedStyle(document.getElementById('chat-outlet')!).display)
  ).not.toBe('none');
  const requestCountBefore = requests.length;

  const dispatchCtrlKFromTerminalPane = () =>
    page.evaluate(() => {
      const pane = document.querySelector('scion-terminal-pane')!;
      const event = new KeyboardEvent('keydown', {
        key: 'k',
        ctrlKey: true,
        bubbles: true,
        composed: true,
        cancelable: true,
      });
      pane.dispatchEvent(event);
      return event.defaultPrevented;
    });

  const defaultPreventedFromTerminal = await dispatchCtrlKFromTerminalPane();
  await page.waitForTimeout(150);
  expect(defaultPreventedFromTerminal).toBe(false);
  expect(requests.length).toBe(requestCountBefore);

  // Positive control: the identical synthetic event, dispatched from
  // outside the terminal pane, does open the palette — proving the
  // document listener is alive and the rejection above is really the
  // terminal-surface guard, not some other reason nothing happened.
  const defaultPreventedElsewhere = await page.evaluate(() => {
    const event = new KeyboardEvent('keydown', {
      key: 'k',
      ctrlKey: true,
      bubbles: true,
      composed: true,
      cancelable: true,
    });
    document.body.dispatchEvent(event);
    return event.defaultPrevented;
  });
  expect(defaultPreventedElsewhere).toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
});

test('opening the palette, then a route change hiding chat, closes it without restoring focus into hidden chat', async ({
  page,
}) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  // The terminal-workspace transition hides chat via history.pushState
  // directly (main.ts) — no popstate event fires for this, only the
  // bounded-poll watchdog catches it. Poll
  // the page's own state rather than DOM visibility: `#chat-outlet` (an
  // ancestor of the palette's dialog) goes `display:none` immediately as a
  // side effect of hiding chat, which would make `toBeHidden()` pass right
  // away regardless of whether the watchdog itself ever actually ran —
  // that would prove nothing about the guard under test.
  const paletteOpenState = () =>
    page.evaluate(
      () =>
        (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
          .v2PaletteOpen
    );
  await page.evaluate(() => window.chatPaletteFixture.hideChatShowTerminal());
  await expect.poll(paletteOpenState, { timeout: 2_000 }).toBe(false);
});
