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
 * Chromium, real <scion-chat-shell>: the header's quick-switcher button,
 * exercised through the production event path (header -> composed
 * CHAT_PALETTE_OPEN_REQUEST_EVENT -> chat.ts's document listener), not a
 * direct method call. Covers placement at desktop and touch widths, open/
 * focus/dismiss behaviour, the button's "never closes, never cancels" open
 * semantics, the touch keyboard-affordance omissions, the phone layout, the
 * 320px header-fit guard, and axe at both widths/themes.
 */

import { test, expect, type Page } from '@playwright/test';
import { AxeBuilder } from '@axe-core/playwright';
import {
  setupApiMocks,
  AGENT_WITH_DM,
  SELF_USER_ID,
  type PaletteFixtureOverrides,
  type TrackedRequest,
} from './mock-api.js';
import { CHAT_PALETTE_OPEN_REQUEST_EVENT } from '../../src/client/chat-palette-events.js';
import { paletteInputHasFocus, slowPaletteModule } from '../palette-focus.js';

const DM_KEY = `dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`;

async function gotoShell(
  page: Page,
  route?: string,
  overrides: PaletteFixtureOverrides = {}
): Promise<TrackedRequest[]> {
  const requests = await setupApiMocks(page, overrides);
  const routeParam = route ? `&route=${encodeURIComponent(route)}` : '';
  await page.goto(`/e2e/chat-palette/fixture.html?shell=1${routeParam}`, {
    waitUntil: 'domcontentloaded',
  });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  return requests;
}

function paletteButton(page: Page) {
  return page.locator('scion-header .palette-button');
}

function paletteTooltip(page: Page) {
  return page.locator('scion-header sl-tooltip:has(.palette-button)');
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

function paletteInput(page: Page) {
  return page.locator('scion-quick-palette #palette-query-input');
}

/** Deep-query into the real composer's native textarea, through both shadow roots. */
function composerTextarea(page: Page) {
  return page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
}

async function paletteOpenState(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      (document.querySelector('scion-page-chat') as unknown as { v2PaletteOpen: boolean })
        .v2PaletteOpen
  );
}

/**
 * Dispatches the exact event the header's click handler dispatches, without
 * a real pointer click — used only where a genuine `.click()`/`.tap()` would
 * be unreliable because the dialog's own overlay is mid-animation (see the
 * queued-reopen tests below). Every other test in this file uses a real
 * click or tap so the production event path (including the header's own
 * dispatch) is what's actually exercised.
 */
async function dispatchOpenRequest(page: Page): Promise<void> {
  await page.evaluate((eventName) => {
    document.dispatchEvent(new CustomEvent(eventName, { bubbles: true, composed: true }));
  }, CHAT_PALETTE_OPEN_REQUEST_EVENT);
}

async function enableDarkTheme(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.documentElement.classList.add('sl-theme-dark');
    document.documentElement.setAttribute('data-theme', 'dark');
  });
  await page.waitForTimeout(200);
}

/** Run an axe scan scoped to the header and palette subtrees and assert zero critical/serious violations. */
async function assertAxeClean(page: Page): Promise<void> {
  const results = await new AxeBuilder({ page })
    .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'])
    .include(['scion-chat-shell', 'scion-header'])
    .include(['scion-page-chat', 'scion-quick-palette'])
    .analyze();

  const serious = results.violations.filter(
    (v) => v.impact === 'serious' || v.impact === 'critical'
  );
  if (serious.length > 0) {
    const summary = serious
      .map((v) => `[${v.impact}] ${v.id}: ${v.description} (${v.nodes.length} instance(s))`)
      .join('\n');
    expect(serious, `Accessibility violations:\n${summary}`).toHaveLength(0);
  }
}

// ---------------------------------------------------------------------------
// Desktop (default 1200x800 viewport from playwright.config.ts)
// ---------------------------------------------------------------------------

test('the button renders left of the inline header actions, on a v2 chat route', async ({
  page,
}) => {
  await gotoShell(page);
  await expect(paletteButton(page)).toBeVisible();
  const buttonBox = await paletteButton(page).boundingBox();
  const actionsBox = await page.locator('scion-header .wide-right').boundingBox();
  expect(buttonBox).not.toBeNull();
  expect(actionsBox).not.toBeNull();
  expect(buttonBox!.x).toBeLessThan(actionsBox!.x);
});

test('the button is absent on a non-chat route', async ({ page }) => {
  await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html?shell=1&route=%2Fprojects%2Fabc', {
    waitUntil: 'domcontentloaded',
  });
  await expect(paletteButton(page)).toHaveCount(0);
});

test('at 900px (the medium/compact tier), the button still renders left of the mode control', async ({
  page,
}) => {
  await page.setViewportSize({ width: 900, height: 800 });
  await gotoShell(page);
  await expect(paletteButton(page)).toBeVisible();
  const buttonBox = await paletteButton(page).boundingBox();
  const compactRightBox = await page.locator('scion-header .compact-right').boundingBox();
  expect(buttonBox).not.toBeNull();
  expect(compactRightBox).not.toBeNull();
  expect(buttonBox!.x).toBeLessThan(compactRightBox!.x);
});

test('the button also renders on a DM route, not just bare /chat', async ({ page }) => {
  await gotoShell(page, `/chat/dm/${DM_KEY}`);
  await expect(paletteButton(page)).toBeVisible();
});

test('the button also renders on a thread route, not just bare /chat', async ({ page }) => {
  await gotoShell(page, '/chat/alpha/thread-alpha', {
    spaces: [{ projectId: 'project-alpha', projectName: 'Alpha', projectSlug: 'alpha' }],
    threadsByProjectId: {
      'project-alpha': [{ id: 'thread-alpha', projectId: 'project-alpha', name: 'General' }],
    },
  });
  await expect(paletteButton(page)).toBeVisible();
});

test('clicking the button opens the palette with the query input focused', async ({ page }) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await expect(paletteInput(page)).toBeFocused();
  await expect(paletteInput(page)).toHaveAttribute(
    'placeholder',
    'Search agents, threads, people, documents…'
  );
});

test('typing straight after a button click becomes the query, even while the module loads', async ({
  page,
}) => {
  await slowPaletteModule(page);
  await gotoShell(page);
  await paletteButton(page).click();
  await page.keyboard.type('coder');

  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveValue('coder');
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveText([/Coder One/]);
});

test('at desktop width, the multi-group palette lays its groups out in two columns', async ({
  page,
}) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();

  // Read in one frame, so the dialog's open animation scales every box alike.
  const layout = await page.locator('scion-quick-palette').evaluate((host) => {
    const root = host.shadowRoot!;
    const results = root.querySelector('.palette-results')!;
    const box = (group: string): DOMRect =>
      root.querySelector(`[data-palette-group="${group}"]`)!.getBoundingClientRect();
    const agents = box('agents');
    const threads = box('threads');
    return {
      columns: getComputedStyle(results).gridTemplateColumns.split(' ').length,
      sameRow: Math.abs(threads.top - agents.top) < 1,
      threadsRightOfAgents: threads.left >= agents.right - 1,
    };
  });
  expect(layout).toEqual({ columns: 2, sameRow: true, threadsRightOfAgents: true });
});

test('Escape after a button-driven open returns focus to the button', async ({ page }) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toBeFocused();
});

test('a backdrop click after a button-driven open returns focus to the button', async ({
  page,
}) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page
    .locator('scion-quick-palette sl-dialog')
    .locator('[part~="overlay"]')
    .click({ position: { x: 5, y: 5 }, force: true });
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toBeFocused();
});

test("the dialog's own X (close button) after a button-driven open returns focus to the button", async ({
  page,
}) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page.locator('scion-quick-palette sl-dialog').locator('[part~="close-button"]').click();
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toBeFocused();
});

test('a double-click before the first-open lazy import settles leaves the palette open, not toggled shut', async ({
  page,
}) => {
  // Two real, separately-dispatched Playwright clicks can't reliably land
  // inside the (likely already-cached-by-now) import's pending window in
  // this environment, and once the dialog's overlay actually appears a
  // second real pointer click can't physically reach the button at all
  // (see the "covered by the dialog overlay" test below) — exactly the
  // behaviour under test, not a flake to work around. Dispatching two
  // click events on the button synchronously, in the same task, reliably
  // reproduces the race togglePalette's 'open' mode guards against: both
  // land while `_palettePendingOpen` is still true from the first call's
  // synchronous portion, before either's `await loadQuickPalette()` (or the
  // subsequent render) has had a chance to resolve.
  await gotoShell(page);
  await page.evaluate(() => {
    const header = document
      .querySelector('scion-chat-shell')
      ?.shadowRoot?.querySelector('scion-header');
    const btn = header?.shadowRoot?.querySelector('.palette-button');
    if (!(btn instanceof HTMLElement)) throw new Error('palette button not found');
    btn.click();
    btn.click();
  });
  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteOpenState(page)).toBe(true);
});

/**
 * `document.elementFromPoint` at the button's own screen position: whatever
 * document-scope element is topmost there right now. With the palette
 * closed this is the shell itself (document-scope hit-testing does not
 * pierce into a shadow tree's own internals without `elementFromPoint`
 * being called *on* that shadow root). Only establishes the closed-state
 * baseline — see `deepHitChainAtButton` below for what actually identifies
 * the palette's own dialog once open, since this alone can't distinguish
 * the dialog's overlay from any other element inside scion-page-chat's
 * shadow tree that might happen to cover the same point.
 */
async function documentHitTagAtButton(page: Page): Promise<string | null> {
  return page.evaluate(() => {
    const header = document
      .querySelector('scion-chat-shell')
      ?.shadowRoot?.querySelector('scion-header');
    const btn = header?.shadowRoot?.querySelector('.palette-button');
    if (!(btn instanceof Element)) return null;
    const rect = btn.getBoundingClientRect();
    const el = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2);
    return el?.tagName ?? null;
  });
}

/**
 * Walks the real hit-test chain at the button's screen position through
 * every shadow boundary (`elementFromPoint` only reports the host at each
 * boundary, not what's inside it, so this calls it again on that host's own
 * `shadowRoot` and repeats), returning one `TAGNAME.class.names` string per
 * level. Identifies the palette's own `<sl-dialog class="palette-dialog">`
 * specifically, not just "some element inside scion-page-chat" — a plain
 * tag check at the document level can't tell those apart, since several
 * real dialogs (the document preview, a future modal) could equally live in
 * that shadow tree.
 */
async function deepHitChainAtButton(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const header = document
      .querySelector('scion-chat-shell')
      ?.shadowRoot?.querySelector('scion-header');
    const btn = header?.shadowRoot?.querySelector('.palette-button');
    if (!(btn instanceof Element)) return [];
    const rect = btn.getBoundingClientRect();
    const x = rect.x + rect.width / 2;
    const y = rect.y + rect.height / 2;
    const chain: string[] = [];
    let root: Document | ShadowRoot = document;
    for (let i = 0; i < 10; i++) {
      const el: Element | null = root.elementFromPoint(x, y);
      if (!el) break;
      chain.push(`${el.tagName}.${Array.from(el.classList).join('.')}`);
      if (!el.shadowRoot) break;
      root = el.shadowRoot;
    }
    return chain;
  });
}

test('once open, the palette dialog itself intercepts pointer events at the button — a second activation cannot reach it', async ({
  page,
}) => {
  await gotoShell(page);
  // Baseline: before anything opens, the button's own screen position
  // genuinely belongs to the shell, not some other always-covering element
  // — without this, "not SCION-PAGE-CHAT" after open would be meaningless.
  expect(await documentHitTagAtButton(page)).toBe('SCION-CHAT-SHELL');

  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteOpenState(page)).toBe(true);

  // The real hit-test chain at the button's position passes through the
  // palette's own dialog specifically — not just some unspecified element
  // inside scion-page-chat's shadow tree.
  const chain = await deepHitChainAtButton(page);
  expect(chain.some((tag) => tag.startsWith('SL-DIALOG.') && tag.includes('palette-dialog'))).toBe(
    true
  );
  // Independent confirmation at the Playwright-actionability level: a real
  // click attempt on the button can't even be dispatched, because another
  // element's subtree intercepts pointer events there.
  await expect(paletteButton(page).click({ trial: true, timeout: 500 })).rejects.toThrow(
    /intercepts pointer events/
  );
});

test('a button press during a still-animating close reopens once that close finishes', async ({
  page,
}) => {
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await page.evaluate(() => window.chatPaletteFixture.setPaletteDialogHideDuration(1500));
  await page.keyboard.press('Escape'); // starts the (stretched) close
  await page.waitForTimeout(200); // still animating for the whole window below
  await dispatchOpenRequest(page); // queue a reopen, same as a second button press
  await page.waitForTimeout(2200); // past the stretched 1500ms hide

  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteOpenState(page)).toBe(true);
});

test('a button press during a closing document preview reopens the palette once the preview settles', async ({
  page,
}) => {
  await gotoShell(page, `/chat/dm/${encodeURIComponent(DM_KEY)}`, {
    spaces: [],
  });
  await page.evaluate(() => {
    window.chatPaletteFixture.seedRecentFiles([
      {
        name: 'notes.txt',
        sentAt: '2026-09-28T12:00:00Z',
        target: {
          kind: 'path',
          projectId: 'project-alpha',
          containerPath: '/workspace/notes.txt',
          location: { kind: 'workspace', filePath: 'notes.txt' },
        },
      },
    ]);
  });

  await paletteButton(page).click();
  await paletteInput(page).fill('notes.txt');
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(page.locator('scion-chat-file-preview sl-dialog')).toBeVisible();

  await page.evaluate((ms) => window.chatPaletteFixture.setFilePreviewDialogHideDuration(ms), 1500);
  await page.keyboard.press('Escape'); // starts the preview's own (stretched) close
  await page.waitForTimeout(200);
  await dispatchOpenRequest(page); // queue a reopen behind the closing preview
  await page.waitForTimeout(2200);

  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteOpenState(page)).toBe(true);
});

test('a button press while an unrelated modal is open does nothing — no state change, no fetch', async ({
  page,
}) => {
  const requests = await gotoShell(page);
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
  await page.waitForTimeout(150);
  const requestCountBefore = requests.length;

  await dispatchOpenRequest(page);
  await page.waitForTimeout(150);

  expect(await paletteOpenState(page)).toBe(false);
  expect(requests.length).toBe(requestCountBefore);
});

test('a button press while the route is not chat does nothing', async ({ page }) => {
  await gotoShell(page);
  await page.evaluate(() => window.history.pushState({}, '', '/projects/abc'));
  await page.waitForTimeout(50);

  await dispatchOpenRequest(page);
  await page.waitForTimeout(150);

  expect(await paletteOpenState(page)).toBe(false);
});

test('a button press while the page is hidden does nothing', async ({ page }) => {
  await gotoShell(page);
  await page.evaluate(() => {
    Object.defineProperty(document, 'hidden', { value: true, configurable: true });
  });

  await dispatchOpenRequest(page);
  await page.waitForTimeout(150);

  expect(await paletteOpenState(page)).toBe(false);
});

test('the tooltip reads the platform-appropriate shortcut and is not disabled on desktop', async ({
  page,
}) => {
  await gotoShell(page);
  // Headless Chromium here reports a non-Mac platform.
  await expect(paletteTooltip(page)).toHaveAttribute('content', 'Quick switcher (Ctrl+K)');
  await expect(paletteTooltip(page)).not.toHaveAttribute('disabled', '');
  await expect(paletteButton(page)).toHaveAttribute('aria-keyshortcuts', 'Control+K');
});

test('aria-haspopup and aria-keyshortcuts actually reach the accessibility tree, not just the DOM', async ({
  page,
}) => {
  // A native <button> was a deliberate choice over <sl-icon-button>
  // specifically because Shoelace does not forward host-level ARIA
  // attributes to the inner element that actually takes focus and carries
  // the role — getByRole() resolves through the real accessibility tree
  // (role + accessible name), not a plain DOM attribute lookup, so finding
  // the button this way is itself proof the attributes above are exposed to
  // assistive tech, not just present as inert host attributes.
  await gotoShell(page);
  const button = page.getByRole('button', { name: 'Open quick switcher' });
  await expect(button).toBeVisible();
  await expect(button).toHaveAttribute('aria-haspopup', 'dialog');
  await expect(button).toHaveAttribute('aria-keyshortcuts', 'Control+K');
});

test('axe reports no new violations with the palette open at 1280px desktop, light and dark', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await gotoShell(page);
  await paletteButton(page).click();
  await expect(paletteDialog(page)).toBeVisible();
  await assertAxeClean(page);

  await enableDarkTheme(page);
  await assertAxeClean(page);
});

// ---------------------------------------------------------------------------
// Touch (390x844, real touch/mobile emulation)
// ---------------------------------------------------------------------------

test.describe('touch', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true });

  test('precondition: this device matches TOUCH_PRIMARY_QUERY', async ({ page }) => {
    await gotoShell(page);
    const matches = await page.evaluate(
      () => matchMedia('(hover: none) and (pointer: coarse)').matches
    );
    expect(matches).toBe(true);
  });

  test('the button renders left of the mode dropdown at 390px', async ({ page }) => {
    await gotoShell(page);
    await expect(paletteButton(page)).toBeVisible();
    const buttonBox = await paletteButton(page).boundingBox();
    const dropdownBox = await page.locator('scion-header .compact-right').boundingBox();
    expect(buttonBox).not.toBeNull();
    expect(dropdownBox).not.toBeNull();
    expect(buttonBox!.x).toBeLessThan(dropdownBox!.x);
  });

  test('tapping the button opens the palette with the query input focused', async ({ page }) => {
    await gotoShell(page);
    await paletteButton(page).tap();
    await expect(paletteDialog(page)).toBeVisible();
    await expect(paletteInput(page)).toBeFocused();
  });

  test('no keyboard-help legend, no aria-describedby, no aria-keyshortcuts, and the tooltip stays disabled', async ({
    page,
  }) => {
    await gotoShell(page);
    await expect(paletteButton(page)).not.toHaveAttribute('aria-keyshortcuts', /.+/);
    await expect(paletteTooltip(page)).toHaveAttribute('disabled', '');

    await paletteButton(page).tap();
    await expect(paletteDialog(page)).toBeVisible();

    await expect(page.locator('scion-quick-palette #palette-keyboard-help')).toHaveCount(0);
    await expect(paletteInput(page)).not.toHaveAttribute('aria-describedby', /.+/);
    // Tooltip stays disabled (never shows) even once the button has been
    // focused/tapped.
    await expect(paletteTooltip(page)).toHaveAttribute('disabled', '');
  });

  test('tap targets are at least 44x44 on touch, and the 44px rule does not inflate a two-line row', async ({
    page,
  }) => {
    // A person with no email renders a single-line row (no
    // .palette-secondary div at all — see renderPaletteGroup's
    // `secondaryLabel ? … : nothing`): a two-line row (e.g. a thread, whose
    // secondary label is always its space name) is naturally taller than
    // 44px on its own content alone, which would make the touch min-height
    // rule untestable — a single-line row's natural height (35px measured)
    // is what's actually borderline enough for the rule to matter.
    await gotoShell(page, undefined, {
      users: [{ id: 'user-no-email', displayName: 'No Email Person' }],
      spaces: [{ projectId: 'project-alpha', projectName: 'Alpha', projectSlug: 'alpha' }],
      threadsByProjectId: {
        'project-alpha': [{ id: 'thread-alpha', projectId: 'project-alpha', name: 'General' }],
      },
    });
    const buttonBox = await paletteButton(page).boundingBox();
    // Rounded: sub-pixel layout rounding can put the raw value a fraction of
    // a pixel below 44 (e.g. 43.9999...) even though the CSS rule asks for
    // exactly 44px — a real, not a visual, 44px target.
    expect(Math.round(buttonBox!.width)).toBeGreaterThanOrEqual(44);
    expect(Math.round(buttonBox!.height)).toBeGreaterThanOrEqual(44);

    await paletteButton(page).tap();
    await paletteInput(page).fill('No Email Person');
    const option = page.locator('scion-quick-palette .palette-option').first();
    await expect(option).toBeVisible();
    // `toBeVisible()` only requires a non-empty box — Shoelace's dialog show
    // animation (a scale transition) can still be mid-flight at that
    // instant, which would measure a shrunk, not-yet-settled row (see the
    // panel-geometry and Retry tests, which wait for the same reason).
    await page.waitForTimeout(300);
    const optionBox = await option.boundingBox();
    expect(Math.round(optionBox!.height)).toBeGreaterThanOrEqual(44);

    // The rule is a 44px *minimum*, not a flat addition on top of a row's
    // own padding: without `box-sizing: border-box`, the content-box default
    // adds the row's 1rem (16px) top+bottom padding on top of the 44px
    // min-height, inflating every row (including already-tall two-line ones)
    // to 60px. A real two-line row (a thread, whose secondary label is
    // always its space name) is naturally 51px on content alone; asserting
    // it stays well under 60 — with some headroom above 51 for font/line-
    // metric differences across environments — is what actually
    // distinguishes "44px minimum" from "44px plus padding" without being
    // sensitive to exactly which pixel the natural height lands on.
    await paletteInput(page).fill('General');
    const twoLineOption = page.locator('scion-quick-palette .palette-option').first();
    await expect(twoLineOption).toBeVisible();
    await page.waitForTimeout(300);
    const twoLineBox = await twoLineOption.boundingBox();
    expect(Math.round(twoLineBox!.height)).toBeLessThanOrEqual(56);
  });

  test('the Retry button is also at least 44px tall on touch, not just Show-more', async ({
    page,
  }) => {
    await setupApiMocks(page, {
      spaces: [{ projectId: 'project-alpha', projectName: 'Alpha', projectSlug: 'alpha' }],
      threadsByProjectId: {
        'project-alpha': [{ id: 'thread-alpha', projectId: 'project-alpha', name: 'General' }],
      },
    });
    // Forces the Agents group into its error state, which renders a Retry
    // button — the Show-more test above never exercises this control.
    await page.route('**/api/v1/agents*', (route) =>
      route.fulfill({ status: 500, json: { error: { code: 'internal', message: 'boom' } } })
    );
    await page.goto('/e2e/chat-palette/fixture.html?shell=1', { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));

    await paletteButton(page).tap();
    const retryButton = page.locator('scion-quick-palette .palette-group-error sl-button');
    await expect(retryButton).toBeVisible();
    // `toBeVisible()` only requires a non-empty box — Shoelace's dialog show
    // animation (a scale transition) can still be mid-flight at that
    // instant, which would measure a shrunk, not-yet-settled button (see
    // the panel-geometry test above for the same effect on the panel).
    await page.waitForTimeout(300);
    const retryBox = await retryButton.boundingBox();
    expect(Math.round(retryBox!.height)).toBeGreaterThanOrEqual(44);
  });

  test('a simulated reduced visual viewport still lets the last result scroll fully into view', async ({
    page,
  }) => {
    await gotoShell(page, undefined, {
      spaces: [{ projectId: 'project-alpha', projectName: 'Alpha', projectSlug: 'alpha' }],
      threadsByProjectId: {
        'project-alpha': Array.from({ length: 12 }, (_, i) => ({
          id: `thread-${i}`,
          projectId: 'project-alpha',
          name: `Thread Number ${i}`,
        })),
      },
    });
    await paletteButton(page).tap();
    await expect(paletteDialog(page)).toBeVisible();
    // `toBeVisible()` only requires a non-empty box — Shoelace's dialog show
    // animation (a scale transition) can still be mid-flight at that
    // instant, which would measure a shrunk, not-yet-settled scroller (see
    // the panel-geometry and Retry tests, which wait for the same reason).
    await page.waitForTimeout(300);

    // Stand in for the iOS on-screen keyboard shrinking the real
    // visualViewport, which this headless run never actually triggers.
    // scion-quick-palette lives inside scion-page-chat's shadow root, so
    // document.querySelector alone (no shadow piercing) would silently find
    // nothing here — deep-query through the host explicitly instead.
    const resultsBottom = await page.evaluate(() => {
      const switcher = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-quick-palette') as HTMLElement | undefined;
      switcher?.style.setProperty('--palette-vvh', '400px');
      const results = switcher?.shadowRoot?.querySelector('.palette-results');
      return results ? results.getBoundingClientRect().bottom : null;
    });
    // The scroller itself (not just some row inside it) must actually fit
    // inside the simulated 400px visible height — this is the direct claim
    // `max-height: calc(var(--palette-vvh, 100dvh) - var(--palette-chrome-height))`
    // makes. Checked before touching scroll position at all, so it can't be
    // satisfied by scrolling.
    expect(resultsBottom).toBeLessThanOrEqual(400);

    // Independently, the last row must be reachable *within* that fitted
    // scroller: scroll it all the way to its own end (not
    // `scrollIntoViewIfNeeded()`, which centres an out-of-view target inside
    // its scroller rather than scrolling to the end — a scroller taller than
    // the simulated viewport but shorter than about 510px would still
    // report a centred row's bottom under 400 even though real rows below it
    // stay hidden past the "keyboard"), then assert its bottom edge is also
    // within the simulated height, not just the real 844px viewport.
    const lastOptionBottom = await page.evaluate(() => {
      const switcher = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-quick-palette');
      const results = switcher?.shadowRoot?.querySelector('.palette-results');
      if (!(results instanceof HTMLElement)) return null;
      results.scrollTop = results.scrollHeight;
      const options = results.querySelectorAll('.palette-option');
      const last = options[options.length - 1];
      return last ? last.getBoundingClientRect().bottom : null;
    });
    expect(lastOptionBottom).toBeLessThanOrEqual(400);
  });

  test('the panel is top-anchored and at least viewport-width minus 1rem, at 390px', async ({
    page,
  }) => {
    await gotoShell(page);
    await paletteButton(page).tap();
    await expect(paletteDialog(page)).toBeVisible();
    // `toBeVisible()` only requires the dialog to be in the accessibility
    // tree and have a non-empty box — Shoelace's own show animation (a
    // scale transition) can still be mid-flight at that instant, which
    // would measure a shrunk, not-yet-settled panel.
    await page.waitForTimeout(300);

    const geometry = await page.evaluate(() => {
      const switcher = document
        .querySelector('scion-page-chat')
        ?.shadowRoot?.querySelector('scion-quick-palette');
      const dialog = switcher?.shadowRoot?.querySelector('sl-dialog.palette-dialog');
      const panel = dialog?.shadowRoot?.querySelector('[part~="panel"]');
      const rect = panel?.getBoundingClientRect();
      return { innerWidth: window.innerWidth, width: rect?.width, top: rect?.top };
    });

    // Shoelace's own dialog part clamps width to `calc(100% -
    // var(--sl-spacing-2x-large))` (36px by default) unless overridden —
    // the -1 tolerates sub-pixel rounding, not a real narrower panel.
    expect(geometry.width).toBeGreaterThanOrEqual(geometry.innerWidth - 16 - 1);
    // Top-anchored, not vertically centered: at most a small top margin
    // plus the safe-area inset (0 in this headless run).
    expect(geometry.top).toBeLessThanOrEqual(8);
  });

  for (const width of [320, 355]) {
    test(`at ${width}px the header has no horizontal overflow, the title doesn't wrap, and every control is fully on screen`, async ({
      page,
    }) => {
      await page.setViewportSize({ width, height: 640 });
      await gotoShell(page);
      await page.waitForTimeout(300);

      const geometry = await page.evaluate(() => {
        // scion-header lives inside scion-chat-shell's shadow root in this
        // fixture — document.querySelector alone does not pierce it.
        const header = document
          .querySelector('scion-chat-shell')!
          .shadowRoot!.querySelector('scion-header')!;
        const logo = header.shadowRoot!.querySelector('.logo') as HTMLElement;
        const title = header.shadowRoot!.querySelector('.logo-text h1') as HTMLElement;
        const button = header.shadowRoot!.querySelector('.palette-button') as HTMLElement;
        const modeDropdown = header.shadowRoot!.querySelector(
          '.compact-mode-dropdown'
        ) as HTMLElement;
        const userDropdown = header.shadowRoot!.querySelector('.user-dropdown') as HTMLElement;
        const rectOf = (el: HTMLElement) => {
          const r = el.getBoundingClientRect();
          return { left: r.left, right: r.right };
        };
        return {
          innerWidth: window.innerWidth,
          scrollWidth: header.scrollWidth,
          clientWidth: header.clientWidth,
          // A wrapped title grows the header's *height*, not its
          // scrollWidth, so the horizontal-overflow check below is
          // structurally blind to it — this is the direct check instead.
          // `title.getClientRects()` (on the block element itself) always
          // returns exactly one rect — its border box — no matter how many
          // lines of text wrap inside it, so it can't tell two lines from
          // one; a Range over the text content fares no better once
          // text-overflow: ellipsis is involved (Chromium reports more than
          // one rect for a *single*, truncated line here). The element's
          // own rendered height is what actually distinguishes them: one
          // line measures 21px, two measure 42px.
          titleHeight: title.getBoundingClientRect().height,
          titleFontSizePx: parseFloat(getComputedStyle(title).fontSize),
          // Neither the height check above nor the per-control in-viewport
          // checks below would catch the title's own box visually painting
          // over the button: that changes none of scrollWidth, the title's
          // height, or whether each control's rect individually sits inside
          // [0, innerWidth] — only a direct logo-vs-button edge comparison
          // does. `titleClips` independently guards the other way a
          // too-wide title could go unnoticed by the height check: a text
          // node that overflows its box but isn't actually clipped (no
          // ellipsis/hidden) renders past the box's edge without changing
          // the box's own reported height at all.
          logo: rectOf(logo),
          titleClips:
            title.scrollWidth <= title.clientWidth ||
            getComputedStyle(title).overflowX !== 'visible',
          button: rectOf(button),
          modeDropdown: rectOf(modeDropdown),
          userDropdown: rectOf(userDropdown),
        };
      });

      expect(geometry.scrollWidth).toBeLessThanOrEqual(geometry.clientWidth);
      // Comfortably above a real single line (21px measured, ~1.17x the
      // 18px font-size) and comfortably below a real two-line wrap (42px
      // measured, ~2.3x) — 1.5x the font-size sits in between either way.
      expect(geometry.titleHeight).toBeLessThanOrEqual(geometry.titleFontSizePx * 1.5);
      expect(geometry.titleClips).toBe(true);
      expect(geometry.logo.right).toBeLessThanOrEqual(geometry.button.left);
      for (const rect of [geometry.button, geometry.modeDropdown, geometry.userDropdown]) {
        expect(rect.left).toBeGreaterThanOrEqual(0);
        expect(rect.right).toBeLessThanOrEqual(geometry.innerWidth);
      }
    });
  }

  test('on touch, selecting a conversation does not focus the composer', async ({ page }) => {
    await gotoShell(page);
    await paletteButton(page).tap();
    await paletteInput(page).fill(AGENT_WITH_DM.name);
    await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);

    await page.locator('scion-quick-palette .palette-option').first().tap();
    await expect(paletteDialog(page)).toBeHidden();

    // The composer for the newly-opened DM must actually exist before "not
    // focused" means anything — without this wait, the assertion below
    // could pass vacuously because the composer simply hasn't mounted yet.
    const composer = composerTextarea(page);
    await composer.waitFor({ state: 'attached' });
    await expect(composer).not.toBeFocused();
    // Positive confirmation of where focus actually landed, not just where
    // it didn't: the page's own fallback target, the same place Escape/
    // backdrop/X restore focus to when there is no real invoker to restore.
    await expect(page.locator('scion-page-chat').locator('#palette-focus-fallback')).toBeFocused();
  });

  test('axe reports no new violations with the palette open at 390px touch, light and dark', async ({
    page,
  }) => {
    await gotoShell(page);
    await paletteButton(page).tap();
    await expect(paletteDialog(page)).toBeVisible();
    await assertAxeClean(page);

    await enableDarkTheme(page);
    await assertAxeClean(page);
  });
});
