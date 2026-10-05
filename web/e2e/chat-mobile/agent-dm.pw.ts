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
 * conversation, so at 320px the actions row scrolls sideways rather than
 * pushing the members button off the screen. The "Promote DM to Thread"
 * dialog opened from that header must fit inside the app frame, its text
 * field must not trigger iOS focus zoom (16px or larger), and nothing may
 * run off the side.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, currentPanel } from './fixture.js';
import { AGENT_MEMBER_ID, PROJECT_A } from './mock-api.js';
import { assertNoHorizontalOverflow } from './helpers.js';

const DM_KEY = `dm:agent:${AGENT_MEMBER_ID}:user:fixture-user`;

/** Add an agent DM to the shared mocks and open it. */
async function openAgentDM(page: Page): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.route('**/api/v1/chat/dms**', (route) => {
      if (route.request().url().includes('/unread')) {
        void route.fulfill({ json: { peerIds: [] } });
        return;
      }
      void route.fulfill({
        json: {
          dms: [
            {
              conversationKey: DM_KEY,
              peerId: AGENT_MEMBER_ID,
              peerKind: 'agent',
              peerName: 'Coder One',
              peerSlug: 'coder-one',
              projectId: PROJECT_A.id,
            },
          ],
        },
      });
    });
  });
  // A DM URL carrying the full conversation key; the page resolves the peer
  // (an agent) from the DM list.
  await page.evaluate((key) => {
    history.pushState({}, '', `/chat/dm/${encodeURIComponent(key)}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  }, DM_KEY);
  await expect(page.getByRole('button', { name: 'Promote to thread' })).toBeVisible({
    timeout: 15_000,
  });
  expect(await currentPanel(page)).toBe('center');
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
    await page.getByRole('button', { name: 'Promote to thread' }).click();
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
    await assertNoHorizontalOverflow(page, ['.header-actions']);
  });

  test('still fits when the keyboard shrinks the frame', async ({ page }) => {
    await openAgentDM(page);
    await page.getByRole('button', { name: 'Promote to thread' }).click();
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

test.describe('agent DM header', () => {
  test.beforeEach(({ page: _page }, testInfo) => {
    test.skip(testInfo.project.name === 'desktop-1440', 'phone layout');
  });

  test('every action stays reachable and nothing runs off the screen', async ({ page }) => {
    await openAgentDM(page);
    await assertNoHorizontalOverflow(page, ['.header-actions']);
    const members = page.locator('.mobile-members');
    // Scrolled into view within the actions row if it had to scroll.
    await members.scrollIntoViewIfNeeded();
    await expect(members).toBeInViewport({ ratio: 0.95 });
    await members.click();
    await expect(page.locator('.v2-panels')).toHaveAttribute('data-panel', 'right');
  });
});
