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
 * Chromium, real xterm: the terminal workspace's "Jump to agent" palette —
 * the header button opens it with the chat palette's type scale, its
 * options spanning the full width of the results; in a multi-pane grid a
 * pick adds a pane, or replaces the focused pane when the grid is full; in
 * a single-pane view (the single preset, or any preset on
 * a narrow viewport) a pick navigates to the agent's URL like the rail; the
 * picked pane takes keyboard focus; Meta+K opens it from inside a pane, but
 * not over a pane's own open dialog, and Ctrl+K keeps reaching the PTY from
 * inside a pane while still opening the palette from outside one; leaving
 * /terminals with the palette open closes it, so the destination page keeps
 * keyboard focus and scrolling.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { DENSE_PALETTE_FONT_SIZES, paletteFontSizes } from '../palette-typography.js';

const agentA = '11111111-1111-4111-8111-111111111111';
const agentB = '22222222-2222-4222-8222-222222222222';
const agentC = '33333333-3333-4333-8333-333333333333';
const agentD = '44444444-4444-4444-8444-444444444444';
const agentE = '55555555-5555-4555-8555-555555555555';

interface AgentFixture {
  id: string;
  name: string;
  phase: string;
  projectId: string;
  project?: string;
}

async function setup(
  page: Page,
  agents: Record<string, AgentFixture>
): Promise<{ attaches: () => number; ptyInput: () => string }> {
  let attaches = 0;
  const frames: string[] = [];
  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'web.terminal_workspace': true };
    window.EventSource = class extends EventTarget {
      onopen: (() => void) | null = null;
      constructor() {
        super();
        queueMicrotask(() => this.onopen?.());
      }
      close(): void {}
    } as unknown as typeof EventSource;
  });
  await page.route('**/auth/me', (route) =>
    route.fulfill({ json: { id: 'fixture-user', email: 'fixture@example.test' } })
  );
  await page.route('**/api/v1/settings/public', (route) =>
    route.fulfill({ json: { nativeChatEnabled: true } })
  );
  // Generic agent-list route (bare array) — unrelated views' fallback.
  await page.route(/\/api\/v1\/agents(\?|$)/, (route) => {
    void route.fulfill({
      json: Object.values(agents).map((a) => ({ ...a, _capabilities: { actions: ['attach'] } })),
    });
  });
  // The palette's own paginated fetch (fetchAllPaletteAgents): registered
  // after the generic route above, so Playwright tries it first — a more
  // specific override wins over an earlier, broader registration.
  await page.route(/\/api\/v1\/agents\?limit=100(&|$)/, (route) => {
    void route.fulfill({
      json: {
        agents: Object.values(agents).map((a) => ({
          ...a,
          _capabilities: { actions: ['attach'] },
        })),
      },
    });
  });
  await page.route('**/api/v1/agents/**', (route) => {
    if (route.request().url().endsWith('/pty')) {
      void route.fulfill({ json: {} });
      return;
    }
    const id =
      route
        .request()
        .url()
        .match(/\/api\/v1\/agents\/([^/?]+)/)?.[1] ?? agentA;
    void route.fulfill({
      status: agents[id] ? 200 : 404,
      json: agents[id] ?? { error: 'not found' },
    });
  });
  await page.route('**/api/v1/system/status', (route) =>
    route.fulfill({ json: { complete: true } })
  );
  await page.routeWebSocket('**/pty?*', (socket) => {
    attaches++;
    socket.onMessage((message) => frames.push(String(message)));
    // tmux sends a redraw on attach — the client only reaches a connected
    // state on its first inbound data frame (see client/terminal-sessions.ts).
    socket.send(JSON.stringify({ type: 'data', data: Buffer.from('').toString('base64') }));
  });
  return {
    attaches: () => attaches,
    ptyInput: () =>
      frames
        .map((f) => {
          try {
            const parsed = JSON.parse(f) as { type: string; data?: string };
            return parsed.type === 'data'
              ? Buffer.from(parsed.data ?? '', 'base64').toString()
              : '';
          } catch {
            return '';
          }
        })
        .join(''),
  };
}

/**
 * Real xterm positions its input textarea off-screen with zero size (a
 * standard technique to keep it focusable but never visible) — Playwright's
 * `.click()` requires visibility and so can never land on it. `.focus()`
 * calls the DOM method directly, which is all a real user's click would
 * actually accomplish here anyway (xterm's own mousedown handler on the
 * visible terminal surface just forwards focus to this same textarea).
 */
async function focusPaneTextarea(locator: ReturnType<Page['locator']>): Promise<void> {
  await locator.focus();
}

function fixture(id: string, name: string): AgentFixture {
  return { id, name, phase: 'running', projectId: 'fixture-project' };
}

function paletteButton(page: Page): Locator {
  return page.locator('#terminal-workspace scion-header .palette-button');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Jump to agent"]');
}

function paletteOption(page: Page, name: string): Locator {
  return page.locator('scion-quick-palette .palette-option', { hasText: name });
}

function panes(page: Page): Locator {
  return page.locator('#terminal-workspace scion-terminal-pane');
}

/**
 * Panes actually placed in the active grid. Replacing a focused slot
 * (`addOrReplaceFocused`) only reassigns that layout slot — the displaced
 * agent's session and pane are retained (closable from the rail, same as
 * any other open-but-not-visible terminal), not removed from the DOM — so
 * the *total* pane count grows by one on a replace, same as on an add. Only
 * the visible count reflects "still a 2x2 grid, one slot swapped."
 */
function visiblePanes(page: Page): Locator {
  return page.locator('#terminal-workspace scion-terminal-pane:visible');
}

/** The agent ID of the pane holding keyboard focus, or null when focus is outside every pane. */
function focusedPaneAgentId(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    let node: Node | null = el;
    while (node) {
      if (node instanceof Element && node.tagName === 'SCION-TERMINAL-PANE') {
        return (node as Element & { agentId: string }).agentId;
      }
      node = node.parentNode instanceof ShadowRoot ? node.parentNode.host : node.parentNode;
    }
    return null;
  });
}

/** Whether the pane for `agentId` is rendered with a non-empty box. */
function paneIsVisible(page: Page, agentId: string): Promise<boolean> {
  return page.evaluate((id) => {
    const pane = [...document.querySelectorAll('#terminal-workspace scion-terminal-pane')].find(
      (el) => (el as Element & { agentId: string }).agentId === id
    );
    if (!(pane instanceof HTMLElement)) return false;
    const box = pane.getBoundingClientRect();
    return pane.checkVisibility() && box.width > 0 && box.height > 0;
  }, agentId);
}

function historyLength(page: Page): Promise<number> {
  return page.evaluate(() => window.history.length);
}

function pathname(page: Page): string {
  return new URL(page.url()).pathname;
}

async function pick(page: Page, name: string): Promise<void> {
  await paletteButton(page).click();
  await paletteOption(page, name).click();
  await expect(paletteDialog(page)).toBeHidden();
}

test('the header button opens the palette, labeled "Jump to agent"', async ({ page }) => {
  await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await expect(paletteDialog(page)).toBeHidden();
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteOption(page, 'Alice-bot')).toBeVisible();
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveAttribute(
    'placeholder',
    'Search agents…'
  );
});

test('a dismiss refocuses the header button that opened the palette', async ({ page }) => {
  await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await paletteButton(page).click();
  await expect(paletteOption(page, 'Alice-bot')).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();

  await expect
    .poll(() =>
      page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        return el?.classList.contains('palette-button') ?? false;
      })
    )
    .toBe(true);
});

test('the palette uses the same type scale as the chat palette', async ({ page }) => {
  await setup(page, { [agentA]: { ...fixture(agentA, 'Alice-bot'), project: 'Fixture Project' } });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await paletteButton(page).click();
  await expect(paletteOption(page, 'Alice-bot')).toBeVisible();

  expect(await paletteFontSizes(page)).toEqual(DENSE_PALETTE_FONT_SIZES);
});

test('the agents-only palette lists its options across the full width of the results', async ({
  page,
}) => {
  await setup(page, {
    [agentA]: fixture(agentA, 'Alice-bot'),
    [agentB]: fixture(agentB, 'Bob-bot-with-a-rather-long-name-for-truncation'),
    [agentC]: fixture(agentC, 'Carol-bot'),
  });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await paletteButton(page).click();
  await expect(paletteOption(page, 'Carol-bot')).toBeVisible();

  // Both widths are read in one frame, so the dialog's open animation
  // scales them alike.
  const ratio = await page.locator('scion-quick-palette').evaluate((host) => {
    const results = host.shadowRoot!.querySelector('.palette-results')!;
    const option = host.shadowRoot!.querySelector('.palette-option')!;
    return option.getBoundingClientRect().width / results.getBoundingClientRect().width;
  });
  expect(ratio).toBeGreaterThan(0.9);
});

test('in a single-pane view, picks navigate to the agent URL for new and open agents', async ({
  page,
}) => {
  await setup(page, {
    [agentA]: fixture(agentA, 'Alice-bot'),
    [agentB]: fixture(agentB, 'Bob-bot'),
  });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await pick(page, 'Bob-bot');
  await expect(page).toHaveURL(new RegExp(`/terminals/${agentB}$`));
  await expect.poll(() => paneIsVisible(page, agentB)).toBe(true);
  await expect.poll(() => paneIsVisible(page, agentA)).toBe(false);
  await expect.poll(() => focusedPaneAgentId(page)).toBe(agentB);

  await pick(page, 'Alice-bot');
  await expect(page).toHaveURL(new RegExp(`/terminals/${agentA}$`));
  await expect.poll(() => paneIsVisible(page, agentA)).toBe(true);
  await expect.poll(() => focusedPaneAgentId(page)).toBe(agentA);

  await page.reload();
  await expect(page).toHaveURL(new RegExp(`/terminals/${agentA}$`));
  await expect.poll(() => paneIsVisible(page, agentA)).toBe(true);
});

test('a reload after picking a new agent restores that agent', async ({ page }) => {
  await setup(page, {
    [agentA]: fixture(agentA, 'Alice-bot'),
    [agentB]: fixture(agentB, 'Bob-bot'),
  });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();

  await pick(page, 'Bob-bot');
  await expect(page).toHaveURL(new RegExp(`/terminals/${agentB}$`));

  await page.reload();
  await expect(page).toHaveURL(new RegExp(`/terminals/${agentB}$`));
  await expect.poll(() => paneIsVisible(page, agentB)).toBe(true);
});

test.describe('on a narrow viewport', () => {
  test.use({ viewport: { width: 600, height: 700 } });

  test('a pick from a multi-pane layout shows the picked pane, new or open', async ({ page }) => {
    await setup(page, {
      [agentA]: fixture(agentA, 'Alice-bot'),
      [agentB]: fixture(agentB, 'Bob-bot'),
    });
    await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=`);
    await expect.poll(() => paneIsVisible(page, agentA)).toBe(true);

    await pick(page, 'Bob-bot');
    await expect(page).toHaveURL(new RegExp(`/terminals/${agentB}$`));
    await expect.poll(() => paneIsVisible(page, agentB)).toBe(true);
    await expect.poll(() => paneIsVisible(page, agentA)).toBe(false);
    await expect.poll(() => focusedPaneAgentId(page)).toBe(agentB);

    await pick(page, 'Alice-bot');
    await expect(page).toHaveURL(new RegExp(`/terminals/${agentA}$`));
    await expect.poll(() => paneIsVisible(page, agentA)).toBe(true);
    await expect.poll(() => paneIsVisible(page, agentB)).toBe(false);
    await expect.poll(() => focusedPaneAgentId(page)).toBe(agentA);
  });
});

test('picking an agent adds it to the next empty pane in a multi-pane grid', async ({ page }) => {
  await setup(page, {
    [agentA]: fixture(agentA, 'Alice-bot'),
    [agentB]: fixture(agentB, 'Bob-bot'),
  });
  await page.goto(`/terminals?lv=1&lp=two-columns&s0=${agentA}&s1=`);
  await expect(panes(page)).toHaveCount(1);
  const historyBefore = await historyLength(page);

  await paletteButton(page).click();
  await paletteOption(page, 'Bob-bot').click();

  await expect(panes(page)).toHaveCount(2);
  await expect(page).toHaveURL(new RegExp(`s0=${agentA}.*s1=${agentB}|s1=${agentB}.*s0=${agentA}`));
  await expect(paletteDialog(page)).toBeHidden();
  await expect.poll(() => focusedPaneAgentId(page)).toBe(agentB);
  // The pick stays on the multi-pane URL and adds no history entry.
  expect(pathname(page)).toBe('/terminals');
  expect(await historyLength(page)).toBe(historyBefore);

  // Picking an agent already on screen moves focus to its pane.
  await pick(page, 'Alice-bot');
  await expect(visiblePanes(page)).toHaveCount(2);
  await expect.poll(() => focusedPaneAgentId(page)).toBe(agentA);
  expect(pathname(page)).toBe('/terminals');
  expect(await historyLength(page)).toBe(historyBefore);
});

test('picking an agent replaces the focused pane, not a collapse to single, when the grid is full', async ({
  page,
}) => {
  const { attaches } = await setup(page, {
    [agentA]: fixture(agentA, 'Alice-bot'),
    [agentB]: fixture(agentB, 'Bob-bot'),
    [agentC]: fixture(agentC, 'Carol-bot'),
    [agentD]: fixture(agentD, 'Dave-bot'),
    [agentE]: fixture(agentE, 'Eve-bot'),
  });
  await page.goto(`/terminals?lv=1&lp=four&s0=${agentA}&s1=${agentB}&s2=${agentC}&s3=${agentD}`);
  await expect(panes(page)).toHaveCount(4);
  await expect.poll(() => attaches()).toBeGreaterThanOrEqual(4);

  // Focus the pane showing Carol (slot 2, DOM order matches the s0..s3
  // restore order) before opening the palette.
  const carolTextarea = panes(page).nth(2).locator('.xterm-helper-textarea');
  await focusPaneTextarea(carolTextarea);
  const historyBefore = await historyLength(page);

  await paletteButton(page).click();
  await paletteOption(page, 'Eve-bot').click();

  // Still a 2x2 grid — not collapsed to a single pane — with Carol's slot
  // replaced by Eve and the other three panes untouched. Carol's own pane
  // is retained (now hidden, closable from the rail), not removed — see
  // visiblePanes' own doc comment.
  await expect(visiblePanes(page)).toHaveCount(4);
  await expect(panes(page)).toHaveCount(5);
  await expect(page).toHaveURL(/lp=four/);
  await expect(page).not.toHaveURL(new RegExp(agentC));
  await expect(page).toHaveURL(new RegExp(agentE));
  for (const id of [agentA, agentB, agentD]) {
    await expect(page).toHaveURL(new RegExp(id));
  }
  await expect.poll(() => focusedPaneAgentId(page)).toBe(agentE);
  // The pick stays on the multi-pane URL and adds no history entry.
  expect(pathname(page)).toBe('/terminals');
  expect(await historyLength(page)).toBe(historyBefore);
});

test('Meta+K opens the palette even with a terminal pane focused', async ({ page }) => {
  const { attaches } = await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => attaches()).toBeGreaterThan(0);
  const helperTextarea = page.locator('.xterm-helper-textarea').first();
  await focusPaneTextarea(helperTextarea);

  await page.keyboard.press('Meta+k');

  await expect(paletteDialog(page)).toBeVisible();
});

test("Meta+K does not open the palette over a terminal pane's own open dialog", async ({
  page,
}) => {
  const { attaches } = await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => attaches()).toBeGreaterThan(0);
  await page.locator('#terminal-workspace scion-terminal-pane').evaluate((pane) => {
    (pane as HTMLElement & { captureAuthScopeDialogOpen: boolean }).captureAuthScopeDialogOpen =
      true;
  });
  const paneDialog = page.locator(
    'scion-terminal-pane sl-dialog[label="Capture Auth — Choose Scope"]'
  );
  await expect(paneDialog).toBeVisible();
  await paneDialog.locator('sl-radio').first().focus();

  await page.keyboard.press('Meta+k');
  await page.waitForTimeout(150);

  await expect(paletteDialog(page)).toBeHidden();
  await expect(paneDialog).toBeVisible();
});

test('Ctrl+K inside a pane still reaches the PTY and does not open the palette', async ({
  page,
}) => {
  const { attaches, ptyInput } = await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect.poll(() => attaches()).toBeGreaterThan(0);
  const helperTextarea = page.locator('.xterm-helper-textarea').first();
  await focusPaneTextarea(helperTextarea);

  await page.keyboard.press('Control+k');
  await page.waitForTimeout(150);

  expect(ptyInput()).toContain('\x0b');
  await expect(paletteDialog(page)).toBeHidden();
});

test('Ctrl+K outside any pane opens the palette', async ({ page }) => {
  await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();
  // Focus somewhere outside the pane — the layout toolbar.
  await page.locator('.terminal-layout-btn').first().focus();

  await page.keyboard.press('Control+k');

  await expect(paletteDialog(page)).toBeVisible();
});

/**
 * Whether the palette's dialog is open with no show or hide animation still
 * running, so what it shows is what it settled on.
 */
function paletteSettledOpen(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const dialog = document
      .querySelector('scion-quick-palette')
      ?.shadowRoot?.querySelector('sl-dialog') as (HTMLElement & { open?: boolean }) | null;
    const animated = dialog?.shadowRoot?.querySelector('.dialog');
    return Boolean(
      dialog?.open && animated && animated.getAnimations({ subtree: true }).length === 0
    );
  });
}

test('a reopen during the close animation shows a focused palette, and keeps the invoker', async ({
  page,
}) => {
  await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();
  const input = page.locator('scion-quick-palette #palette-query-input');
  const focusInPalette = (): Promise<boolean> =>
    page.evaluate(() => {
      let el: Element | null = document.activeElement;
      while (el) {
        if (el.tagName === 'SCION-QUICK-PALETTE') return true;
        el = el.shadowRoot?.activeElement ?? null;
      }
      return false;
    });

  await paletteButton(page).click();
  await expect(input).toBeFocused();
  // The second press lands while the first close is still animating.
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+k');

  await expect.poll(() => paletteSettledOpen(page)).toBe(true);
  await expect(paletteDialog(page)).toBeVisible();
  await expect(input).toBeVisible();
  await expect(input).toBeFocused();
  for (let i = 0; i < 3; i++) {
    await page.keyboard.press('Tab');
    expect(await focusInPalette()).toBe(true);
  }
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect
    .poll(() =>
      page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        return el?.classList.contains('palette-button') ?? false;
      })
    )
    .toBe(true);
});

test('an Escape during a reopen in the close animation leaves it closed, and refocuses the invoker', async ({
  page,
}) => {
  await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
  await page.goto(`/terminals/${agentA}`);
  await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();
  const input = page.locator('scion-quick-palette #palette-query-input');

  await paletteButton(page).click();
  await expect(input).toBeFocused();
  // Resolves two tasks after the close animation ends, once a reopen
  // deferred until then has either shown or been cancelled.
  await page.evaluate(() => {
    const w = window as unknown as { hideSettled?: Promise<void> };
    w.hideSettled = new Promise((resolve) => {
      // Only the palette's own dialog: the header button's tooltip fires one too.
      const listener = (e: Event): void => {
        const dialog = document
          .querySelector('scion-quick-palette')
          ?.shadowRoot?.querySelector('sl-dialog');
        if (e.composedPath()[0] !== dialog) return;
        document.removeEventListener('sl-after-hide', listener);
        setTimeout(() => setTimeout(resolve));
      };
      document.addEventListener('sl-after-hide', listener);
    });
  });
  // All three presses land while the first close is still animating.
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+k');
  await page.keyboard.press('Escape');

  await page.evaluate(() => (window as unknown as { hideSettled: Promise<void> }).hideSettled);
  expect(
    await page.evaluate(
      () => (document.querySelector('scion-quick-palette') as { open?: boolean } | null)?.open
    )
  ).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();
  await expect
    .poll(() =>
      page.evaluate(() => {
        let el: Element | null = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        return el?.classList.contains('palette-button') ?? false;
      })
    )
    .toBe(true);
});

test.describe('leaving /terminals with the palette open', () => {
  /** In-app navigation, the same path a nav link or any other `nav-click` takes. */
  async function navClick(page: Page, path: string): Promise<void> {
    await page.evaluate((p) => {
      document.dispatchEvent(
        new CustomEvent('nav-click', { detail: { path: p }, bubbles: true, composed: true })
      );
    }, path);
  }

  async function openPaletteOnTerminal(page: Page): Promise<void> {
    await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();
    await paletteButton(page).click();
    await expect(paletteOption(page, 'Alice-bot')).toBeVisible();
  }

  /** Whether any sl-dialog, in the light DOM or any shadow root, is open. */
  function anyDialogOpen(page: Page): Promise<boolean> {
    return page.evaluate(() => {
      const walk = (root: Document | ShadowRoot): boolean =>
        [...root.querySelectorAll('*')].some(
          (el) =>
            (el.tagName === 'SL-DIALOG' && (el as Element & { open: boolean }).open) ||
            (el.shadowRoot !== null && walk(el.shadowRoot))
        );
      return walk(document);
    });
  }

  /** Whether the deep-active element sits inside the app's `scion-nav`. */
  function focusInNav(page: Page): Promise<boolean> {
    return page.evaluate(() => {
      let node: Node | null = document.activeElement;
      while (node instanceof Element && node.shadowRoot?.activeElement) {
        node = node.shadowRoot.activeElement;
      }
      while (node) {
        if (node instanceof Element && node.tagName === 'SCION-NAV') return true;
        node = node.parentNode instanceof ShadowRoot ? node.parentNode.host : node.parentNode;
      }
      return false;
    });
  }

  async function expectDestinationUsable(page: Page): Promise<void> {
    await expect(page).toHaveURL(/\/projects$/);
    await expect(page.locator('#terminal-workspace')).toBeHidden();
    await expect.poll(() => anyDialogOpen(page)).toBe(false);
    await expect(page.locator('html')).not.toHaveClass(/\bsl-scroll-lock\b/);
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
    let reached = false;
    for (let i = 0; i < 6 && !reached; i++) {
      await page.keyboard.press('Tab');
      reached = await focusInNav(page);
    }
    expect(reached).toBe(true);
  }

  async function expectPaletteClosedOnReturn(page: Page): Promise<void> {
    await expect(page).toHaveURL(new RegExp(`/terminals/${agentA}$`));
    await expect(page.locator('#terminal-workspace')).toBeVisible();
    await expect(paletteDialog(page)).toBeHidden();
    expect(
      await page.evaluate(
        () => (document.querySelector('scion-quick-palette') as { open?: boolean } | null)?.open
      )
    ).toBe(false);
  }

  test('with Back', async ({ page }) => {
    await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
    await page.goto('/projects');
    await expect(page.getByRole('link', { name: 'Dashboard' })).toBeVisible();
    await navClick(page, `/terminals/${agentA}`);
    await openPaletteOnTerminal(page);

    await page.goBack();

    await expectDestinationUsable(page);
    await page.goForward();
    await expectPaletteClosedOnReturn(page);
  });

  test('with Forward', async ({ page }) => {
    await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
    await page.goto(`/terminals/${agentA}`);
    await expect(page.locator('.xterm-helper-textarea').first()).toBeAttached();
    await navClick(page, '/projects');
    await expect(page).toHaveURL(/\/projects$/);
    await page.goBack();
    await openPaletteOnTerminal(page);

    await page.goForward();

    await expectDestinationUsable(page);
    await page.goBack();
    await expectPaletteClosedOnReturn(page);
  });

  test('with in-app navigation', async ({ page }) => {
    await setup(page, { [agentA]: fixture(agentA, 'Alice-bot') });
    await page.goto(`/terminals/${agentA}`);
    await openPaletteOnTerminal(page);

    await navClick(page, '/projects');

    await expectDestinationUsable(page);
    await page.goBack();
    await expectPaletteClosedOnReturn(page);
  });
});
