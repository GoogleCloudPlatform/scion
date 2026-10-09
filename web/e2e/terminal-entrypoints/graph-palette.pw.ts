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
 * The "Jump to agent" palette on the graph views: /agents and the project
 * page in graph mode, and /agents/graph. Picking an agent centres the graph
 * on its node in place, at the zoom that renders its name at about 16px,
 * highlights it and focuses it.
 */

import { test, expect, type Locator, type Page, type Route } from '@playwright/test';
import { projectId, setup, type AgentFixture } from './fixtures.js';
import {
  expectTapHoldsKeyboard,
  paletteInputHasFocus,
  slowPaletteModule,
} from '../palette-focus.js';

const USER = 'fixture-user';

function uuid(n: number): string {
  const hex = n.toString(16).padStart(2, '0');
  return `aaaaaa${hex}-aaaa-4aaa-8aaa-aaaaaaaaaaaa`;
}

const rootIds = Array.from({ length: 8 }, (_, i) => uuid(i + 1));
const childId = uuid(20);
const targetId = uuid(21);

function fixture(id: string, name: string, ancestry: string[]): AgentFixture {
  return { id, name, slug: name, phase: 'running', projectId, ancestry };
}

/** Eight roots side by side, and a grandchild under the first: the jump target. */
const graphAgents: Record<string, AgentFixture> = Object.fromEntries(
  [
    ...rootIds.map((id, i) => fixture(id, `root-agent-${i + 1}`, [USER])),
    fixture(childId, 'child-agent', [USER, rootIds[0]]),
    fixture(targetId, 'gamma-target', [USER, rootIds[0], childId]),
  ].map((a) => [a.id, a])
);

interface GraphHost {
  name: string;
  path: string;
  /** localStorage entries that put the page in graph mode. */
  storage: Record<string, string>;
}

const hosts: GraphHost[] = [
  { name: '/agents in graph mode', path: '/agents', storage: { 'scion-view-agents': 'graph' } },
  { name: '/agents/graph', path: '/agents/graph', storage: {} },
  {
    name: 'the project page in graph mode',
    path: `/projects/${projectId}`,
    storage: { 'scion-view-project-agents': 'graph' },
  },
];

/**
 * Mocks the API with `agents`, and seeds localStorage with `storage`. The
 * project page's metrics have no data, so its summary card stays empty.
 */
async function setupGraph(
  page: Page,
  storage: Record<string, string>,
  agents: Record<string, AgentFixture> = graphAgents
): Promise<void> {
  await setup(page, { agents });
  await page.route(/\/api\/v1\/projects\/[^/]+\/metrics/, (route) =>
    route.fulfill({ status: 404, json: { error: 'not found' } })
  );
  await page.addInitScript((entries) => {
    for (const [key, value] of Object.entries(entries)) localStorage.setItem(key, value);
  }, storage);
}

async function openHost(
  page: Page,
  path: string,
  storage: Record<string, string>,
  agents: Record<string, AgentFixture> = graphAgents
): Promise<void> {
  await setupGraph(page, storage, agents);
  await page.goto(path);
}

function graphNode(page: Page, id: string): Locator {
  return page.locator(`scion-agent-tree-view a.node[data-agent-id="${id}"]`);
}

/** The shown header's palette button (the terminal workspace has its own header). */
function paletteButton(page: Page): Locator {
  return page.locator('scion-header .palette-button:visible');
}

function paletteDialog(page: Page): Locator {
  return page.locator('scion-quick-palette sl-dialog[label="Jump to agent"]');
}

function stageTransform(page: Page): Promise<string> {
  return page
    .locator('scion-agent-tree-view .stage')
    .evaluate((el) => (el as HTMLElement).style.transform);
}

function scaleOf(transform: string): string | undefined {
  return /scale\(([^)]+)\)/.exec(transform)?.[1];
}

/** The rendered size of an agent node's name: its font size times the graph's zoom. */
async function renderedNamePx(page: Page, id: string): Promise<number> {
  const fontPx = await graphNode(page, id)
    .locator('.name')
    .evaluate((el) => parseFloat(getComputedStyle(el).fontSize));
  return fontPx * Number(scaleOf(await stageTransform(page)));
}

/** Zooms the graph out with its zoom-out button until it stops changing. */
async function zoomAllTheWayOut(page: Page): Promise<void> {
  const button = page.locator('scion-agent-tree-view .zoom-controls sl-button[title^="Zoom out"]');
  const maxClicks = 30;
  let last = '';
  for (let i = 0; i < maxClicks; i++) {
    await button.click();
    const transform = await stageTransform(page);
    if (transform === last) return;
    last = transform;
  }
  throw new Error(`the graph was still zooming out after ${maxClicks} clicks`);
}

/** How far the node's centre is from the graph canvas's centre, in px. */
function offCentre(page: Page, id: string): Promise<number> {
  return page.locator('scion-agent-tree-view').evaluate((tree, agentId) => {
    const root = tree.shadowRoot!;
    const canvas = root.querySelector('.canvas')!.getBoundingClientRect();
    const node = root.querySelector(`a.node[data-agent-id="${agentId}"]`)!.getBoundingClientRect();
    return Math.hypot(
      node.left + node.width / 2 - (canvas.left + canvas.width / 2),
      node.top + node.height / 2 - (canvas.top + canvas.height / 2)
    );
  }, id);
}

/** The deepest focused element, through open shadow roots. */
function deepActive(page: Page): Promise<{ agentId: string | null; className: string }> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
    return { agentId: el?.getAttribute('data-agent-id') ?? null, className: el?.className ?? '' };
  });
}

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

/** Whether the palette element itself is open, or opening. */
function paletteElementOpen(page: Page): Promise<boolean> {
  return page.evaluate(
    () =>
      (document.querySelector('scion-quick-palette') as { open?: boolean } | null)?.open ?? false
  );
}

/**
 * Arms {@link waitForHideSettled} for the palette's next `sl-after-hide`.
 * It resolves two tasks after that event, once a reopen deferred until the
 * close animation ends has either shown or been cancelled.
 */
async function armHideSettled(page: Page): Promise<void> {
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
}

async function waitForHideSettled(page: Page): Promise<void> {
  await page.evaluate(() => (window as unknown as { hideSettled: Promise<void> }).hideSettled);
}

/** Whether focus is inside the palette, through open shadow roots. */
function focusInPalette(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    let el: Element | null = document.activeElement;
    while (el) {
      if (el.tagName === 'SCION-QUICK-PALETTE') return true;
      el = el.shadowRoot?.activeElement ?? null;
    }
    return false;
  });
}

/** The graph's transform once the initial fit has stopped changing it. */
async function settledStageTransform(page: Page): Promise<string> {
  let last = '';
  await expect
    .poll(async () => {
      const transform = await stageTransform(page);
      const settled = transform.includes('scale(') && transform === last;
      last = transform;
      return settled;
    })
    .toBe(true);
  return last;
}

/**
 * Presses Ctrl+K and asserts that nothing handled it, so no palette is
 * opening: the graph controller calls preventDefault exactly when it acts.
 */
async function pressUnhandledShortcut(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { shortcutHandled?: Promise<boolean> };
    w.shortcutHandled = new Promise((resolve) => {
      const listener = (e: KeyboardEvent): void => {
        if (e.key.toLowerCase() !== 'k') return;
        window.removeEventListener('keydown', listener);
        resolve(e.defaultPrevented);
      };
      window.addEventListener('keydown', listener);
    });
  });
  await page.keyboard.press('Control+k');
  const handled = await page.evaluate(
    () => (window as unknown as { shortcutHandled: Promise<boolean> }).shortcutHandled
  );
  expect(handled).toBe(false);
}

/** In-app navigation, the same path a nav link takes. */
async function navClick(page: Page, path: string): Promise<void> {
  await page.evaluate((p) => {
    document.dispatchEvent(
      new CustomEvent('nav-click', { detail: { path: p }, bubbles: true, composed: true })
    );
  }, path);
}

/** Asserts the page offers no palette: no header button, and the shortcut opens nothing. */
async function expectNotOffered(page: Page): Promise<void> {
  await expect(paletteButton(page)).toHaveCount(0);
  await pressUnhandledShortcut(page);
  await expect(paletteDialog(page)).toBeHidden();
}

/**
 * Answers the requests `matcher` matches in turn: each queued step either
 * fails with a 500 or falls through to the fixture routes, optionally after
 * waiting for `release`. Requests past the end of the queue fall through.
 */
async function queueResponses(
  page: Page,
  matcher: RegExp,
  steps: { fail: boolean; until?: Promise<void> }[]
): Promise<void> {
  await page.route(matcher, async (route: Route) => {
    const step = steps.shift();
    if (step?.until) await step.until;
    if (step?.fail) await route.fulfill({ status: 500, json: { error: 'fixture failure' } });
    else await route.fallback();
  });
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = r));
  return { promise, resolve };
}

const agentsList = /\/api\/v1\/agents(\?|$)/;
const projectApi = new RegExp(`/api/v1/projects/${projectId}(\\?|$)`);

/**
 * Clicks a segment of the agents page's own view toggle. Dispatched rather
 * than clicked, so it also reaches the toggle while the modal palette is open.
 */
async function chooseView(
  page: Page,
  title: 'Graph view' | 'List view' | 'Grid view',
  pageTag = 'scion-page-agents'
): Promise<void> {
  await page
    .locator(`${pageTag} scion-view-toggle button[title="${title}"]`)
    .dispatchEvent('click');
}

async function jumpTo(page: Page, query: string): Promise<void> {
  await expect(paletteDialog(page)).toBeVisible();
  await page.locator('scion-quick-palette #palette-query-input').fill(query);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(paletteDialog(page)).toBeHidden();
}

for (const host of hosts) {
  test.describe(host.name, () => {
    test('the shortcut jumps to a picked agent in place', async ({ page }) => {
      await openHost(page, host.path, host.storage);
      await expect(graphNode(page, targetId)).toBeVisible();
      const before = await settledStageTransform(page);
      const url = page.url();
      expect(await offCentre(page, targetId)).toBeGreaterThan(50);

      await page.keyboard.press('Control+k');
      await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(10);
      await jumpTo(page, 'gamma');

      await expect.poll(() => offCentre(page, targetId)).toBeLessThan(2);
      const after = await stageTransform(page);
      expect(after).not.toBe(before);
      expect(await renderedNamePx(page, targetId)).toBeCloseTo(16, 1);
      await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
      await expect.poll(async () => (await deepActive(page)).agentId).toBe(targetId);
      expect(page.url()).toBe(url);
    });

    test('the header button opens it, and a dismiss refocuses the button', async ({ page }) => {
      await openHost(page, host.path, host.storage);
      await expect(graphNode(page, targetId)).toBeVisible();
      const button = paletteButton(page);
      await expect(button).toHaveAttribute('aria-label', 'Open Jump to agent');

      await button.click();
      await expect(paletteDialog(page)).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(paletteDialog(page)).toBeHidden();

      await expect.poll(async () => (await deepActive(page)).className).toContain('palette-button');
    });
  });
}

/**
 * After `open`, types `gam` without waiting for the palette and checks it all
 * became the query: the input has focus right after the open, and the
 * results are filtered.
 */
async function expectTypingRightAfterOpenFilters(
  page: Page,
  open: () => Promise<void>
): Promise<void> {
  await open();
  await page.keyboard.type('gam');

  await expect(paletteDialog(page)).toBeVisible();
  expect(await paletteInputHasFocus(page)).toBe(true);
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveValue('gam');
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveText([/gamma-target/]);
}

for (const host of hosts) {
  test(`${host.name}: typing straight after the shortcut becomes the query, while the palette module loads`, async ({
    page,
  }) => {
    await slowPaletteModule(page);
    await openHost(page, host.path, host.storage);
    await expect(graphNode(page, targetId)).toBeVisible();

    await expectTypingRightAfterOpenFilters(page, () => page.keyboard.press('Control+k'));
  });
}

test.describe('on a touch-primary device', () => {
  test.use({ hasTouch: true, isMobile: true });

  test('a tap on the header button holds the keyboard until the query input has focus', async ({
    page,
  }) => {
    await slowPaletteModule(page);
    await openHost(page, '/agents/graph', {});
    await expect(graphNode(page, targetId)).toBeVisible();
    expect(
      await page.evaluate(() => matchMedia('(hover: none) and (pointer: coarse)').matches)
    ).toBe(true);

    await expectTapHoldsKeyboard(page, () => paletteButton(page).tap());
    await expect(paletteDialog(page)).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(paletteDialog(page)).toBeHidden();
    await expectTapHoldsKeyboard(page, () => paletteButton(page).tap());
  });
});

test('typing straight after the header button becomes the query, while the palette module loads', async ({
  page,
}) => {
  await slowPaletteModule(page);
  await openHost(page, '/agents/graph', {});
  await expect(graphNode(page, targetId)).toBeVisible();

  await expectTypingRightAfterOpenFilters(page, () => paletteButton(page).click());
});

test('typing straight after a reopen becomes the new query', async ({ page }) => {
  await openHost(page, '/agents/graph', {});
  await expect(graphNode(page, targetId)).toBeVisible();
  await page.keyboard.press('Control+k');
  await page.keyboard.type('root');
  await expect(page.locator('scion-quick-palette #palette-query-input')).toHaveValue('root');
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();

  await expectTypingRightAfterOpenFilters(page, () => page.keyboard.press('Control+k'));
});

test('a jump expands the collapsed ancestors of the picked agent', async ({ page }) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();
  await page
    .locator(
      `scion-agent-tree-view .node-wrapper:has(a.node[data-agent-id="${rootIds[0]}"]) .collapse-chip`
    )
    .click();
  await expect(graphNode(page, targetId)).toHaveCount(0);

  await page.keyboard.press('Meta+k');
  await jumpTo(page, 'gamma');

  await expect(graphNode(page, targetId)).toBeVisible();
  await expect(graphNode(page, childId)).toBeVisible();
  await expect.poll(() => offCentre(page, targetId)).toBeLessThan(2);
  await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
});

test('a jump from a far zoomed-out graph zooms in until the name renders at about 16px', async ({
  page,
}) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();
  await settledStageTransform(page);
  await zoomAllTheWayOut(page);
  expect(await renderedNamePx(page, targetId)).toBeLessThan(8);

  await page.keyboard.press('Control+k');
  await jumpTo(page, 'gamma');

  await expect.poll(() => offCentre(page, targetId)).toBeLessThan(2);
  expect(await renderedNamePx(page, targetId)).toBeCloseTo(16, 1);
  await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
});

test('a reopen during the close animation shows a focused palette, and keeps the invoker', async ({
  page,
}) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();
  const input = page.locator('scion-quick-palette #palette-query-input');

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
    expect(await focusInPalette(page)).toBe(true);
  }
  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  await expect.poll(async () => (await deepActive(page)).className).toContain('palette-button');
});

test('an Escape during a reopen in the close animation leaves it closed, and refocuses the invoker', async ({
  page,
}) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();
  const input = page.locator('scion-quick-palette #palette-query-input');

  await paletteButton(page).click();
  await expect(input).toBeFocused();
  await armHideSettled(page);
  // All three presses land while the first close is still animating.
  await page.keyboard.press('Escape');
  await page.keyboard.press('Control+k');
  await page.keyboard.press('Escape');

  await waitForHideSettled(page);
  expect(await paletteElementOpen(page)).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();
  await expect.poll(async () => (await deepActive(page)).className).toContain('palette-button');
  // The next shortcut opens it as usual.
  await page.keyboard.press('Control+k');
  await expect.poll(() => paletteSettledOpen(page)).toBe(true);
  await expect(input).toBeFocused();
});

test('an Escape while the palette module first loads leaves it closed, and the next shortcut opens it', async ({
  page,
}) => {
  const paletteModule = deferred();
  let requested = false;
  await page.route(/\/shared\/palette\/quick-palette\.ts(\?|$)/, async (route: Route) => {
    requested = true;
    await paletteModule.promise;
    await route.fallback();
  });
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();

  await page.keyboard.press('Control+k');
  await expect.poll(() => requested).toBe(true);
  await page.keyboard.press('Escape');
  paletteModule.resolve();

  // Once the element has mounted and rendered, an open still pending would have shown.
  await page.locator('scion-quick-palette').waitFor({ state: 'attached' });
  await page.evaluate(async () => {
    await (
      document.querySelector('scion-quick-palette') as unknown as {
        updateComplete: Promise<unknown>;
      }
    ).updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  expect(await paletteElementOpen(page)).toBe(false);
  await expect(paletteDialog(page)).toBeHidden();

  await page.keyboard.press('Control+k');
  await expect.poll(() => paletteSettledOpen(page)).toBe(true);
});

test('a pick followed by a reopen during the close animation still jumps to the pick', async ({
  page,
}) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
  await expect(graphNode(page, targetId)).toBeVisible();
  await settledStageTransform(page);
  expect(await offCentre(page, targetId)).toBeGreaterThan(50);
  const input = page.locator('scion-quick-palette #palette-query-input');

  await page.keyboard.press('Control+k');
  await input.fill('gamma');
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  // The reopen lands while the pick's close is still animating.
  await page.keyboard.press('Enter');
  await page.keyboard.press('Control+k');

  await expect.poll(() => paletteSettledOpen(page)).toBe(true);
  await expect.poll(() => offCentre(page, targetId)).toBeLessThan(2);
  expect(await renderedNamePx(page, targetId)).toBeCloseTo(16, 1);
  await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
  // Focus belongs to the reopened palette, not to the picked node.
  await expect(input).toBeFocused();

  await page.keyboard.press('Escape');
  await expect(paletteDialog(page)).toBeHidden();
  expect(await offCentre(page, targetId)).toBeLessThan(2);
});

test('list mode does not offer it, and leaving graph mode closes it', async ({ page }) => {
  await openHost(page, '/agents', { 'scion-view-agents': 'list' });
  await expect(page.locator('scion-page-agents table')).toBeVisible();
  await expect(paletteButton(page)).toHaveCount(0);
  await pressUnhandledShortcut(page);
  await expect(paletteDialog(page)).toHaveCount(0);

  await chooseView(page, 'Graph view');
  await expect(paletteButton(page)).toBeVisible();
  await page.keyboard.press('Control+k');
  await expect(paletteDialog(page)).toBeVisible();

  await chooseView(page, 'List view');
  await expect(paletteDialog(page)).toBeHidden();
  await expect(paletteButton(page)).toHaveCount(0);
});

test.describe('/agents does not offer it without a graph', () => {
  const storage = { 'scion-view-agents': 'graph' };

  test('while loading, and on the error page until a Retry loads the graph', async ({ page }) => {
    const hold = deferred();
    await setupGraph(page, storage);
    await queueResponses(page, agentsList, [{ fail: true, until: hold.promise }]);
    await page.goto('/agents');
    await expect(page.getByText('Loading agents...')).toBeVisible();
    await expectNotOffered(page);

    hold.resolve();
    await expect(page.getByText('Failed to Load Agents')).toBeVisible();
    await expectNotOffered(page);

    await page.locator('scion-page-agents sl-button', { hasText: 'Retry' }).click();
    await expect(graphNode(page, targetId)).toBeVisible();
    await expect(paletteButton(page)).toBeVisible();
    await page.keyboard.press('Control+k');
    await jumpTo(page, 'gamma');
    await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
  });

  test('with no agents', async ({ page }) => {
    await openHost(page, '/agents', storage, {});
    await expect(page.locator('scion-page-agents .empty-state')).toBeVisible();
    await expectNotOffered(page);
  });
});

test.describe('the project page does not offer it without a graph', () => {
  const storage = { 'scion-view-project-agents': 'graph' };

  test('while loading, and on the error page until a Retry loads the graph', async ({ page }) => {
    const hold = deferred();
    await setupGraph(page, storage);
    await queueResponses(page, projectApi, [{ fail: true, until: hold.promise }]);
    await page.goto(`/projects/${projectId}`);
    await expect(page.getByText('Loading project...')).toBeVisible();
    await expectNotOffered(page);

    hold.resolve();
    await expect(page.getByText('Failed to Load Project')).toBeVisible();
    await expectNotOffered(page);

    await page.locator('scion-page-project-detail sl-button', { hasText: 'Retry' }).click();
    await expect(graphNode(page, targetId)).toBeVisible();
    await expect(paletteButton(page)).toBeVisible();
    await page.keyboard.press('Control+k');
    await jumpTo(page, 'gamma');
    await expect(graphNode(page, targetId)).toHaveClass(/jump-highlight/);
  });

  test('with no agents', async ({ page }) => {
    await openHost(page, `/projects/${projectId}`, storage, {});
    await expect(page.getByText('Fixture Project').first()).toBeVisible();
    await expect(page.locator('scion-agent-tree-view')).toHaveCount(0);
    await expectNotOffered(page);
  });

  test('opens again after a reload swaps the page to its error page and back', async ({ page }) => {
    // The first load succeeds; the reload fails until a Retry.
    await setupGraph(page, storage);
    await queueResponses(page, projectApi, [{ fail: false }, { fail: true }]);
    await page.goto(`/projects/${projectId}`);
    await expect(graphNode(page, targetId)).toBeVisible();
    await page.keyboard.press('Control+k');
    await expect(paletteDialog(page)).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(paletteDialog(page)).toBeHidden();

    await page
      .locator('scion-page-project-detail')
      .evaluate((el) => void (el as unknown as { loadData(): Promise<void> }).loadData());
    await expect(page.getByText('Failed to Load Project')).toBeVisible();
    await page.locator('scion-page-project-detail sl-button', { hasText: 'Retry' }).click();
    await expect(graphNode(page, targetId)).toBeVisible();

    await page.keyboard.press('Control+k');
    await jumpTo(page, 'gamma');
    await expect.poll(() => offCentre(page, targetId)).toBeLessThan(2);
  });
});

test.describe('the palette closes when its graph goes away', () => {
  test('on navigating to another page', async ({ page }) => {
    await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
    await expect(graphNode(page, targetId)).toBeVisible();
    await page.keyboard.press('Control+k');
    await expect(paletteDialog(page)).toBeVisible();

    await navClick(page, '/projects');

    await expect(page).toHaveURL(/\/projects$/);
    await expect(paletteDialog(page)).toBeHidden();
    await expect(page.locator('html')).not.toHaveClass(/\bsl-scroll-lock\b/);
  });

  test('when /terminals hides the page, and stays closed on return', async ({ page }) => {
    await openHost(page, '/agents', { 'scion-view-agents': 'graph' });
    await expect(graphNode(page, targetId)).toBeVisible();
    await page.keyboard.press('Control+k');
    await expect(paletteDialog(page)).toBeVisible();

    await navClick(page, '/terminals');

    await expect(page.locator('#terminal-workspace')).toBeVisible();
    await expect(paletteDialog(page)).toBeHidden();
    await page.goBack();
    await expect(graphNode(page, targetId)).toBeVisible();
    await expect(paletteDialog(page)).toBeHidden();
    await expect(paletteButton(page)).toBeVisible();
  });

  test("when the project page's view toggle leaves graph mode", async ({ page }) => {
    await openHost(page, `/projects/${projectId}`, { 'scion-view-project-agents': 'graph' });
    await expect(graphNode(page, targetId)).toBeVisible();
    await page.keyboard.press('Control+k');
    await expect(paletteDialog(page)).toBeVisible();

    await chooseView(page, 'List view', 'scion-page-project-detail');

    await expect(paletteDialog(page)).toBeHidden();
    await expect(paletteButton(page)).toHaveCount(0);
  });
});
