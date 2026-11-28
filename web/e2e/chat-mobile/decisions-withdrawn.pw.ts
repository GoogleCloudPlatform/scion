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
 * Withdrawn decision cards (ptone/scion#8193, parent ptone/scion#8145;
 * design-8145 r3 §4.6, mocks r2 scenes W1–W5 and W7) in the real chat page
 * with `web.decisions` on, in light and dark on desktop (1440 px) and mobile
 * (390 px), plus D7 Jump to v2 and single-answer 409 withdrawn toast.
 */

import { test, expect, type Locator, type Page, type TestInfo } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import {
  ASKER_NAME,
  answer,
  at,
  decision,
  routeDecisionThread,
  row,
  setDecisions,
  setThread,
  singleThread,
  withdrawnInfo,
  type AskerLookup,
  type DecisionThread,
} from './decisions-mock.js';

type Theme = 'light' | 'dark';

const PROJECTS = new Set(['desktop-1440', 'chromium-390']);
const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 768;

const YES_NO = [
  { id: 'yes', label: 'Yes' },
  { id: 'no', label: 'No' },
];

const message = (page: Page, id: string): Locator => page.locator(`[id="msg-${id}"]`);
const card = (page: Page, id: string): Locator => message(page, id).locator('scion-chat-decisions');

async function openDecisions(
  page: Page,
  theme: Theme,
  thread: DecisionThread,
  asker: AskerLookup = 'ok'
): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.addInitScript((t) => {
      localStorage.setItem('scion-theme', t);
      window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, 'web.decisions': true };
    }, theme);
    await routeDecisionThread(p, thread, asker);
  });
  if (isPhone(page)) await openGeneralThread(page);
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
  const first = thread.messages[0]?.id as string;
  await expect(message(page, first)).toBeVisible({ timeout: 15_000 });
}

async function markdownReady(page: Page, id: string): Promise<void> {
  await expect(card(page, id).locator('.q code').first()).toBeVisible();
}

async function shot(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string,
  ids: string[]
): Promise<void> {
  await page.waitForTimeout(250);
  const boxes = [];
  for (const id of ids) {
    const box = await message(page, id).boundingBox();
    if (box) boxes.push(box);
  }
  expect(boxes.length, `messages ${ids.join(', ')} have boxes`).toBe(ids.length);
  const viewport = page.viewportSize()!;
  const pad = 8;
  const x = Math.max(0, Math.min(...boxes.map((b) => b.x)) - pad);
  const y = Math.max(0, Math.min(...boxes.map((b) => b.y)) - pad);
  const right = Math.min(viewport.width, Math.max(...boxes.map((b) => b.x + b.width)) + pad);
  const bottom = Math.min(viewport.height, Math.max(...boxes.map((b) => b.y + b.height)) + pad);
  const project = testInfo.project.name === 'chromium-390' ? 'mobile390' : 'desktop';
  const file = testInfo.outputPath(`withdrawn-${project}-${theme}-${name}.png`);
  await page.screenshot({ path: file, clip: { x, y, width: right - x, height: bottom - y } });
  await testInfo.attach(name, { path: file, contentType: 'image/png' });
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`withdrawn decision cards (${theme})`, () => {
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('W1: single decision withdrawn with a reason', async ({ page }, testInfo) => {
      const d = decision({
        messageId: 'prompt-w1',
        rowId: 'prompt-w1-d234',
        key: 'D234',
        question: 'Ship `v0.9` today?',
        defaultChoiceId: 'yes',
        choices: YES_NO,
        status: 'withdrawn',
        withdrawn: withdrawnInfo(11, 'CI found a regression; the release moved to Monday.'),
      });
      await openDecisions(page, theme, singleThread(d, 'One call before I tag.'));
      await markdownReady(page, 'prompt-w1');

      const c = card(page, 'prompt-w1');
      await expect(c.locator('.row.withdrawn')).toBeVisible();
      await expect(c.locator('button[data-choice="yes"]')).toHaveAttribute('aria-disabled', 'true');
      await expect(c.locator('button[data-choice="no"]')).toHaveAttribute('aria-disabled', 'true');
      await expect(c.locator('.hint')).toHaveCount(0);
      await expect(c.locator('.wd sl-icon[name="slash-circle"]')).toHaveCount(1);
      await expect(c.locator('.wd')).toContainText(`No longer needed · withdrawn by ${ASKER_NAME}`);
      await expect(c.locator('.wd time')).toHaveText('15:52');
      await expect(c.locator('.wd-reason')).toHaveText(
        'CI found a regression; the release moved to Monday.'
      );

      await shot(page, testInfo, theme, 'w1-single-withdrawn', ['prompt-w1']);
    });

    test('W2: set with one row withdrawn while others wait', async ({ page }, testInfo) => {
      const decisions = setDecisions('prompt-w2', {
        overrides: {
          '17': {
            status: 'withdrawn',
            withdrawn: withdrawnInfo(11),
          },
        },
      });
      await openDecisions(
        page,
        theme,
        setThread('prompt-w2', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-w2');

      const c = card(page, 'prompt-w2');
      await c.locator('[data-row="prompt-w2-d234"] button[data-choice="yes"]').click();

      const wdRow = c.locator('[data-row="prompt-w2-17"]');
      await expect(wdRow).toHaveClass(/withdrawn/);
      await expect(wdRow.locator('.wd')).toContainText(
        `No longer needed · withdrawn by ${ASKER_NAME}`
      );
      await expect(wdRow.locator('.wd-reason')).toHaveCount(0);

      await expect(c.locator('.counter')).toHaveText('1 remaining · 1 no longer needed');
      await expect(c.locator('sl-button.submit')).toHaveAttribute('aria-disabled', 'true');

      await shot(page, testInfo, theme, 'w2-set-one-withdrawn', ['prompt-w2']);
    });

    test('W3: whole set withdrawn collapses into one card-footer line', async ({
      page,
    }, testInfo) => {
      const w = withdrawnInfo(11, "Release cancelled; I'll ask again for the next one.");
      const decisions = setDecisions('prompt-w3', {
        overrides: {
          D234: { status: 'withdrawn', withdrawn: w },
          '17': { status: 'withdrawn', withdrawn: w },
          'ship-now': { status: 'withdrawn', withdrawn: w },
        },
      });
      await openDecisions(
        page,
        theme,
        setThread('prompt-w3', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-w3');

      const c = card(page, 'prompt-w3');
      await expect(c.locator('.card.withdrawn-all')).toBeVisible();
      await expect(c.locator('.row .wd')).toHaveCount(0);
      await expect(c.locator('.card-foot .wd')).toContainText(
        `No longer needed · withdrawn by ${ASKER_NAME}`
      );
      await expect(c.locator('.card-foot .wd time')).toHaveText('15:52');
      await expect(c.locator('.card-foot .wd-reason')).toHaveText(
        "Release cancelled; I'll ask again for the next one."
      );
      await expect(c.locator('sl-button.submit')).toHaveCount(0);

      await shot(page, testInfo, theme, 'w3-set-whole-withdrawn', ['prompt-w3']);
    });

    test('W4: set with one row answered earlier and the rest withdrawn', async ({
      page,
    }, testInfo) => {
      const w = withdrawnInfo(11);
      const decisions = setDecisions('prompt-w4', {
        overrides: {
          D234: {
            status: 'decided',
            decision: answer('yes', 'Yes', 'user-alice', 'alice', 3),
          },
          '17': { status: 'withdrawn', withdrawn: w },
          'ship-now': { status: 'withdrawn', withdrawn: w },
        },
      });
      await openDecisions(page, theme, setThread('prompt-w4', decisions, ''));
      await markdownReady(page, 'prompt-w4');

      const c = card(page, 'prompt-w4');
      await expect(c.locator('[data-row="prompt-w4-d234"] .row-foot')).toContainText(
        'Answered by alice'
      );
      await expect(c.locator('.row .wd')).toHaveCount(2);
      await expect(c.locator('.counter')).toHaveText('1 answered by alice · 2 no longer needed');
      await expect(c.locator('sl-button.submit')).toHaveCount(0);

      await shot(page, testInfo, theme, 'w4-set-partly-answered', ['prompt-w4']);
    });

    test('W5: set submit refused with 409 answer_set_changed when a row was withdrawn meanwhile', async ({
      page,
    }, testInfo) => {
      const pending = setDecisions('prompt-w5');
      await openDecisions(page, theme, setThread('prompt-w5', pending, ''));
      await markdownReady(page, 'prompt-w5');

      const fresh = setDecisions('prompt-w5', {
        overrides: {
          '17': { status: 'withdrawn', withdrawn: withdrawnInfo(11) },
        },
      });
      await page.route('**/api/v1/decision-prompts/prompt-w5/answers', (route) =>
        route.fulfill({
          status: 409,
          json: {
            error: {
              code: 'answer_set_changed',
              message: 'the decisions to answer changed; refresh and submit again',
              details: {
                decisions: fresh,
                reasons: { 'prompt-w5-17': 'withdrawn' },
              },
            },
          },
        })
      );

      const c = card(page, 'prompt-w5');
      await c.locator('[data-row="prompt-w5-d234"] button[data-choice="yes"]').click();
      await c.locator('[data-row="prompt-w5-17"] button[data-choice="a"]').click();
      await c.locator('[data-row="prompt-w5-ship-now"] button[data-choice="no"]').click();
      await c.locator('sl-button.submit').click();

      await expect(c.locator('sl-alert.changed')).toContainText(
        'This changed while you were choosing: 17 is no longer needed.'
      );
      await expect(c.locator('sl-alert.changed')).toContainText('Your other selections are kept');
      await expect(c.locator('[data-row="prompt-w5-17"]')).toHaveClass(/withdrawn/);
      await expect(c.locator('.counter')).toHaveText('0 remaining · 1 no longer needed');
      await expect(c.locator('sl-button.submit')).not.toHaveAttribute('aria-disabled', 'true');

      await shot(page, testInfo, theme, 'w5-set-409-withdrawn', ['prompt-w5']);
    });

    test('W7: set with one row withdrawn (with reason) and remaining rows ready at 390 px and desktop', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-w7', {
        overrides: {
          '17': {
            status: 'withdrawn',
            withdrawn: withdrawnInfo(11, 'Settled in review.'),
          },
        },
      });
      await openDecisions(
        page,
        theme,
        setThread('prompt-w7', decisions, 'Three calls before the release.')
      );
      await markdownReady(page, 'prompt-w7');

      const c = card(page, 'prompt-w7');
      await c.locator('[data-row="prompt-w7-d234"] button[data-choice="yes"]').click();
      await c.locator('[data-row="prompt-w7-ship-now"] button[data-choice="no"]').click();

      await expect(c.locator('[data-row="prompt-w7-17"] .wd-reason')).toHaveText(
        'Settled in review.'
      );
      await expect(c.locator('.counter')).toHaveText('0 remaining · 1 no longer needed');
      await expect(c.locator('sl-button.submit')).not.toHaveAttribute('aria-disabled', 'true');

      // Ensure no horizontal overflow inside the card at 390 px mobile width.
      const overflow = await c.locator('.card').evaluate((el) => el.scrollWidth - el.clientWidth);
      expect(overflow).toBeLessThanOrEqual(1);

      await shot(page, testInfo, theme, 'w7-set-withdrawn-ready', ['prompt-w7']);
    });

    test('single 409 withdrawn toasts and D7 Jump to v2 scrolls to the re-asked version', async ({
      page,
    }) => {
      const v1Pending = decision({
        messageId: 'prompt-v1',
        rowId: 'row-d234-v1',
        key: 'D234',
        question: 'Ship `v0.9` today?',
        defaultChoiceId: 'yes',
        choices: YES_NO,
      });
      const v2Pending = decision({
        messageId: 'prompt-v2',
        rowId: 'row-d234-v2',
        key: 'D234',
        version: 2,
        question: 'Ship `v0.9.1` on Monday instead?',
        defaultChoiceId: 'yes',
        choices: YES_NO,
      });
      const thread: DecisionThread = {
        messages: [
          row('prompt-v1', 0, 'decision-prompt', 'One call before I tag.\n\n(fallback)', 'asker'),
          row('prompt-v2', 14, 'decision-prompt', 'Re-asking for Monday.\n\n(fallback)', 'asker'),
        ],
        messageDecisions: { 'prompt-v1': [v1Pending], 'prompt-v2': [v2Pending] },
        messageDecisionIntros: {
          'prompt-v1': 'One call before I tag.',
          'prompt-v2': 'Re-asking for Monday.',
        },
        messageDecisionAnswerModes: { 'prompt-v1': 'single', 'prompt-v2': 'single' },
      };
      await openDecisions(page, theme, thread);
      await markdownReady(page, 'prompt-v1');

      await page.route('**/api/v1/decisions/row-d234-v1/answer', (route) =>
        route.fulfill({
          status: 409,
          json: {
            error: {
              code: 'withdrawn',
              message: 'decision D234 v1 was withdrawn',
              details: {
                decision: {
                  ...v1Pending,
                  status: 'withdrawn',
                  withdrawn: withdrawnInfo(11, 'Moved to Monday.'),
                  replacedBy: {
                    rowId: 'row-d234-v2',
                    messageId: 'prompt-v2',
                    version: 2,
                    at: at(14),
                  },
                },
              },
            },
          },
        })
      );

      const c1 = card(page, 'prompt-v1');
      await c1.locator('button[data-choice="yes"]').click();

      await expect(page.locator('sl-alert[open]')).toContainText(
        `No longer needed: ${ASKER_NAME} withdrew this decision`
      );
      await expect(c1.locator('.row.withdrawn')).toBeVisible();
      await expect(c1.locator('.card-head .version')).toHaveText('v1');
      const jumpLink = c1.locator('.wd a.jump');
      await expect(jumpLink).toHaveText('Jump to v2');
      await jumpLink.click();
      await expect(message(page, 'prompt-v2')).toHaveClass(/permalink-highlight/);
    });
  });
}
