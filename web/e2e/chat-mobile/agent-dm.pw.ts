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
 * An agent DM on a phone. Its header carries the most actions of any
 * conversation, so in a narrow header everything but search and members
 * folds into a More menu (an action sheet on a phone), and the project
 * breadcrumb and the agent name each truncate to one line. The "Promote DM
 * to Thread" dialog opened from that menu must fit inside the app frame,
 * its text field must not trigger iOS focus zoom (16px or larger), and
 * nothing may run off the side.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread, currentPanel } from './fixture.js';
import { AGENT_MEMBER_ID, PROJECT_A } from './mock-api.js';
import { assertNoHorizontalOverflow } from './helpers.js';

const LONG_AGENT_NAME = 'Coder One, the agent with an exceptionally long display name';

/**
 * Open a DM with the fixture agent from the project's members list, so the
 * DM carries the project and its header shows the project breadcrumb.
 */
async function openAgentDM(page: Page, peerName = 'Coder One'): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.route(/\/api\/v1\/chat\/spaces\/([^/]+)\/members/, (route) =>
      route.fulfill({
        json: {
          humans: [],
          agents: [
            { id: AGENT_MEMBER_ID, kind: 'agent', displayName: peerName, slug: 'coder-one' },
          ],
        },
      })
    );
  });
  await openGeneralThread(page);
  const phone = (page.viewportSize()?.width ?? 0) <= 768;
  if (phone) {
    await page.locator('.mobile-members').click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  }
  await page.locator('scion-chat-members .member-item', { hasText: peerName }).first().click();
  await expect(page.locator('.v2-thread-header .conv-name')).toHaveAttribute('title', peerName, {
    timeout: 15_000,
  });
  await expect(page.locator('.v2-thread-header .conv-crumb')).toHaveAttribute(
    'title',
    PROJECT_A.slug
  );
  if (phone) {
    expect(await currentPanel(page)).toBe('center');
  }
  // Let the panel slide back to the conversation before anything measures.
  await page.waitForTimeout(400);
}

const openSheet = (page: Page) => page.locator('scion-action-sheet[open]');

/** Open the header's More menu as an action sheet and choose `label`. */
async function chooseFromMoreSheet(page: Page, label: string): Promise<void> {
  await page.getByRole('button', { name: 'More actions' }).click();
  const sheet = openSheet(page);
  await expect(sheet).toHaveCount(1);
  await sheet.locator('.item', { hasText: label }).click();
}

/** Geometry of the open dialog's panel, body and text field, in CSS px. */
async function measureDialog(page: Page) {
  return page.evaluate(() => {
    const find = (root: ParentNode, selector: string): Element | null => {
      const hit = root.querySelector(selector);
      if (hit) return hit;
      for (const el of root.querySelectorAll('*')) {
        if (el.shadowRoot) {
          const nested = find(el.shadowRoot, selector);
          if (nested) return nested;
        }
      }
      return null;
    };
    const dialog = find(document, 'sl-dialog[label="Promote DM to Thread"]')!;
    const panel = dialog.shadowRoot!.querySelector('[part~="panel"]')!;
    const input = dialog.querySelector('sl-input')!.shadowRoot!.querySelector('input')!;
    // The frame height in px: the custom property can hold a dvh length.
    const probe = document.createElement('div');
    probe.style.cssText = 'position: fixed; top: 0; height: var(--scion-app-height, 100dvh);';
    document.body.appendChild(probe);
    const frame = probe.getBoundingClientRect().height;
    probe.remove();
    const p = panel.getBoundingClientRect();
    const descendants = [...dialog.querySelectorAll('*'), panel, ...panel.querySelectorAll('*')];
    const maxRight = Math.max(...descendants.map((el) => el.getBoundingClientRect().right));
    return {
      frame,
      innerWidth: window.innerWidth,
      panelTop: p.top,
      panelBottom: p.bottom,
      panelLeft: p.left,
      panelRight: p.right,
      maxRight,
      inputFontSize: parseFloat(getComputedStyle(input).fontSize),
    };
  });
}

test.describe('promote DM to thread dialog', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'phone layout');
  });

  test('fits the frame, with a 16px field and no overflow', async ({ page }) => {
    await openAgentDM(page);
    await chooseFromMoreSheet(page, 'Promote to thread');
    const dialog = page.locator('sl-dialog[label="Promote DM to Thread"]');
    await expect(dialog).toHaveAttribute('open', '');
    // Let the open animation finish before measuring.
    await page.waitForTimeout(400);

    const m = await measureDialog(page);
    expect(m.panelTop, 'panel top inside the frame').toBeGreaterThanOrEqual(0);
    expect(m.panelBottom, 'panel bottom inside the frame').toBeLessThanOrEqual(m.frame + 0.5);
    expect(m.panelLeft, 'panel left inside the viewport').toBeGreaterThanOrEqual(0);
    expect(m.panelRight, 'panel right inside the viewport').toBeLessThanOrEqual(m.innerWidth + 0.5);
    expect(m.maxRight, 'no dialog content past the right edge').toBeLessThanOrEqual(
      m.innerWidth + 0.5
    );
    expect(m.inputFontSize, 'thread name field is 16px or larger').toBeGreaterThanOrEqual(16);

    // The action buttons are reachable.
    await expect(dialog.getByRole('button', { name: 'Promote' })).toBeInViewport();
    await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeInViewport();
    await assertNoHorizontalOverflow(page);
  });

  test('still fits when the keyboard shrinks the frame', async ({ page }) => {
    await openAgentDM(page);
    await chooseFromMoreSheet(page, 'Promote to thread');
    const dialog = page.locator('sl-dialog[label="Promote DM to Thread"]');
    await expect(dialog).toHaveAttribute('open', '');
    // What client/viewport.ts does while the keyboard is open.
    const vh = page.viewportSize()?.height ?? 812;
    const frame = Math.max(260, vh - 300);
    await page.evaluate((px) => {
      document.documentElement.style.setProperty('--scion-app-height', `${px}px`);
    }, frame);
    await page.waitForTimeout(400);
    const m = await measureDialog(page);
    expect(m.frame).toBe(frame);
    expect(m.panelTop, 'panel top inside the frame').toBeGreaterThanOrEqual(0);
    expect(m.panelBottom, 'panel bottom inside the shrunken frame').toBeLessThanOrEqual(
      frame + 0.5
    );
  });
});

/** Geometry of the conversation header, its title lines and its visible buttons. */
async function measureHeader(page: Page) {
  return page.locator('.v2-thread-header').evaluate((header) => {
    const line = (selector: string) => {
      const el = header.querySelector<HTMLElement>(selector);
      if (!el) return null;
      const cs = getComputedStyle(el);
      return {
        height: el.getBoundingClientRect().height,
        fontSize: parseFloat(cs.fontSize),
        truncated: el.scrollWidth > el.clientWidth,
        textOverflow: cs.textOverflow,
      };
    };
    const actions = header.querySelector<HTMLElement>('.header-actions')!;
    const buttons = [...header.querySelectorAll<HTMLElement>('sl-icon-button')]
      .filter((b) => b.getClientRects().length > 0)
      .map((b) => {
        const r = b.getBoundingClientRect();
        return { label: b.getAttribute('label') ?? '', left: r.left, right: r.right };
      });
    const h = header.getBoundingClientRect();
    return {
      height: h.height,
      left: h.left,
      right: h.right,
      innerWidth: window.innerWidth,
      crumb: line('.conv-crumb .conv-text'),
      name: line('.conv-name .conv-text'),
      actionsOverflowX: getComputedStyle(actions).overflowX,
      buttons,
    };
  });
}

/** One title line: a single line of text, cut with an ellipsis if too long. */
function expectOneLine(
  line: Awaited<ReturnType<typeof measureHeader>>['crumb'],
  what: string
): void {
  expect(line, `${what} is rendered`).not.toBeNull();
  expect(line!.height, `${what} stays on one line`).toBeLessThan(line!.fontSize * 1.75);
  expect(line!.textOverflow, `${what} truncates with an ellipsis`).toBe('ellipsis');
}

test.describe('agent DM header', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'phone layout');
  });

  for (const [what, peerName] of [
    ['a short name', 'Coder One'],
    ['a long name', LONG_AGENT_NAME],
  ] as const) {
    test(`stays one compact row with ${what}, with every button on screen`, async ({ page }) => {
      await openAgentDM(page, peerName);
      const m = await measureHeader(page);
      expect(m.height, 'header height').toBeLessThanOrEqual(72);
      expectOneLine(m.crumb, 'the project breadcrumb');
      expectOneLine(m.name, 'the agent name');
      expect(m.crumb!.truncated, 'the long project slug is cut short').toBe(true);
      expect(m.actionsOverflowX, 'the actions row is not a sideways scroller').toBe('visible');
      for (const b of m.buttons) {
        expect(b.left, `${b.label} starts inside the header`).toBeGreaterThanOrEqual(m.left - 0.5);
        expect(b.right, `${b.label} ends on screen`).toBeLessThanOrEqual(m.innerWidth + 0.5);
      }
      expect(m.buttons.map((b) => b.label)).toEqual([
        'Back',
        'Search messages',
        'Members',
        'More actions',
      ]);
      await assertNoHorizontalOverflow(page);
    });
  }

  test('the composer destination chip keeps a long name to one line', async ({ page }) => {
    await openAgentDM(page, LONG_AGENT_NAME);
    const chip = page.locator('scion-chat-composer .destination-chip');
    await expect(chip).toBeVisible();
    const m = await chip.evaluate((el) => {
      const name = el.querySelector('.agent-name') as HTMLElement;
      const box = el.getBoundingClientRect();
      const cs = getComputedStyle(name);
      return {
        height: name.getBoundingClientRect().height,
        lineHeight: parseFloat(cs.lineHeight) || parseFloat(cs.fontSize) * 1.5,
        truncated: name.scrollWidth > name.clientWidth,
        title: name.getAttribute('title'),
        right: box.right,
        innerWidth: window.innerWidth,
      };
    });
    expect(m.height, 'the name is one line').toBeLessThan(m.lineHeight * 1.5);
    expect(m.truncated, 'the long name is cut short').toBe(true);
    expect(m.title, 'the full name is in the title').toBe(`@${LONG_AGENT_NAME}`);
    expect(m.right, 'the chip ends on screen').toBeLessThanOrEqual(m.innerWidth + 0.5);
  });

  test('the More sheet offers the folded actions, and members is one tap away', async ({
    page,
  }) => {
    await openAgentDM(page);
    await page.getByRole('button', { name: 'More actions' }).click();
    const sheet = openSheet(page);
    await expect(sheet).toHaveCount(1);
    const labels = await sheet.locator('.item .label').allTextContents();
    for (const label of [
      'Open terminal',
      'Promote to thread',
      'Mute conversation',
      'Chime on',
      'Download as Markdown',
      'Print / Save as PDF',
      'Copy to clipboard',
    ]) {
      expect(labels.map((l) => l.trim().replace('Chime off', 'Chime on'))).toContain(label);
    }
    // Density has no effect in the mobile layout, so it is not offered.
    expect(labels.join('|')).not.toContain('view');
    await sheet.locator('.cancel').click();
    await expect(openSheet(page)).toHaveCount(0);

    const members = page.locator('.mobile-members');
    await expect(members).toBeInViewport();
    await members.click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  });

  test('closing the More sheet returns focus to the More button', async ({ page }) => {
    await openAgentDM(page);
    /** The focused element, followed down through the shadow roots, and the hosts above it. */
    const focusPath = () =>
      page.evaluate(() => {
        let el = document.activeElement;
        while (el?.shadowRoot?.activeElement) el = el.shadowRoot.activeElement;
        const path: string[] = [];
        for (let n: Node | null = el; n; ) {
          if (n instanceof Element) path.push(`${n.localName}.${[...n.classList].join('.')}`);
          n = n instanceof ShadowRoot ? n.host : n.parentNode;
        }
        return path;
      });
    const more = page.getByRole('button', { name: 'More actions' });
    await more.focus();
    await page.keyboard.press('Enter');
    await expect(openSheet(page)).toHaveCount(1);
    await page.keyboard.press('Escape');
    await expect(openSheet(page)).toHaveCount(0);
    await expect
      .poll(async () => (await focusPath()).some((n) => n.includes('header-more')))
      .toBe(true);

    // With the composer focused, opening the sheet still lowers the keyboard.
    const input = page.locator('scion-chat-composer textarea').first();
    await input.focus();
    await more.click();
    await expect(openSheet(page)).toHaveCount(1);
    expect((await focusPath())[0], 'the composer gave up focus').not.toMatch(/^textarea/);
  });

  test('a sheet action runs: Mute flips the conversation to muted', async ({ page }) => {
    await page.route('**/api/v1/chat/conversations/*/mute', (route) =>
      route.fulfill({ json: { muted: true } })
    );
    await openAgentDM(page);
    await chooseFromMoreSheet(page, 'Mute conversation');
    await expect(openSheet(page)).toHaveCount(0);
    await page.getByRole('button', { name: 'More actions' }).click();
    await expect(openSheet(page).locator('.item', { hasText: 'Unmute conversation' })).toHaveCount(
      1
    );
  });
});

/*
 * A landscape phone is wider than the mobile breakpoint, so the header sits
 * in the centre column between the rail and the members tray, a column no
 * wider than a portrait phone. The viewport is set here, so one Chromium
 * project is enough.
 */
test.describe('agent DM header in touch landscape (844x390)', () => {
  test.use({ viewport: { width: 844, height: 390 }, isMobile: true, hasTouch: true });

  test('the actions fit the centre column and the folded ones are in More', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openAgentDM(page, LONG_AGENT_NAME);
    const m = await measureHeader(page);
    expect(m.height, 'header height').toBeLessThanOrEqual(72);
    expectOneLine(m.crumb, 'the project breadcrumb');
    expectOneLine(m.name, 'the agent name');
    for (const b of m.buttons) {
      expect(b.right, `${b.label} ends inside the centre column`).toBeLessThanOrEqual(
        m.right + 0.5
      );
    }
    expect(m.buttons.map((b) => b.label)).toEqual([
      'Search messages',
      'Show/Hide members',
      'More actions',
    ]);

    const more = page.getByRole('button', { name: 'More actions' });
    const trigger = await more.boundingBox();
    expect(trigger, 'the More button has a box').not.toBeNull();
    await more.click();
    const menu = page.locator('sl-dropdown.header-more sl-menu');
    await expect(menu).toBeVisible();
    // Nine items are taller than the frame: the menu fits below the header
    // and scrolls, so the last item is still reachable.
    await expect(async () => {
      const box = await menu.boundingBox();
      expect(box, 'the menu has a box').not.toBeNull();
      expect(box!.y, 'the menu opens below its button').toBeGreaterThanOrEqual(
        trigger!.y + trigger!.height - 1
      );
      expect(box!.y + box!.height, 'the menu ends inside the frame').toBeLessThanOrEqual(390.5);
    }).toPass({ timeout: 5_000 });
    const last = menu.getByRole('menuitem', { name: 'Copy to clipboard' });
    await last.scrollIntoViewIfNeeded();
    await expect(last).toBeInViewport();
    await expect(menu.getByRole('menuitem', { name: /^Chime (on|off)$/ })).toBeVisible();
    await menu.getByRole('menuitem', { name: 'Promote to thread' }).click();
    await expect(page.locator('sl-dialog[label="Promote DM to Thread"]')).toHaveAttribute(
      'open',
      ''
    );
  });
  test('a modified or middle click on a folded link opens it in a new tab', async ({
    page,
  }, testInfo) => {
    test.skip(testInfo.project.name !== 'chromium-390', 'the viewport is fixed by this test');
    await openAgentDM(page);
    const before = page.url();
    const more = page.getByRole('button', { name: 'More actions' });
    const menu = page.locator('sl-dropdown.header-more sl-menu');
    const cases: Array<[string, Parameters<Locator['click']>[0], RegExp]> = [
      ['Open terminal', { modifiers: ['ControlOrMeta'] }, /\/terminals?\b/],
      ['Open in graph', { button: 'middle' }, /\/agents\/graph\?/],
    ];
    for (const [label, click, path] of cases) {
      await more.click();
      await expect(menu).toBeVisible();
      const popup = page.context().waitForEvent('page');
      await menu.getByRole('menuitem', { name: label }).click(click);
      const tab = await popup;
      await tab.waitForURL(path);
      expect(await tab.evaluate(() => window.opener), 'opened without an opener').toBeNull();
      await tab.close();
      expect(page.url(), 'this tab stays on the conversation').toBe(before);
      await expect(menu).toBeHidden();
    }

    // An Alt-click is left to the browser: no new tab, and no action here.
    let opened = 0;
    page.context().on('page', () => opened++);
    await more.click();
    await expect(menu).toBeVisible();
    await menu.getByRole('menuitem', { name: 'Open terminal' }).click({ modifiers: ['Alt'] });
    await page.waitForTimeout(500);
    expect(opened, 'no new tab').toBe(0);
    expect(page.url(), 'this tab stays on the conversation').toBe(before);
  });
});

test.describe('agent DM header on a wide desktop', () => {
  test('keeps the full row of actions, with no More menu', async ({ page }, testInfo) => {
    test.skip(testInfo.project.name !== 'desktop-1440', 'the wide layout');
    await openAgentDM(page, LONG_AGENT_NAME);
    const m = await measureHeader(page);
    expectOneLine(m.name, 'the agent name');
    const labels = m.buttons.map((b) => b.label);
    for (const label of [
      'Promote to thread',
      'Options',
      'Export conversation',
      'Search messages',
    ]) {
      expect(labels).toContain(label);
    }
    expect(labels).not.toContain('More actions');
    for (const b of m.buttons) {
      expect(b.right, `${b.label} ends inside the header`).toBeLessThanOrEqual(m.right + 0.5);
    }
  });
});
