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
 * Choice cards and response lines (ptone/scion#7673) in the real chat page,
 * with web.chat_choices on: each approved mock state (design §4.3, §4.6;
 * mocks r4) in light and dark on desktop and at 390 px. Each test asserts
 * the state's text and ARIA, then saves a screenshot of the messages it
 * shows as a test artifact (`test-results/chat-mobile/…/choices-*.png`),
 * for a reviewer to compare with the mocks. Nothing is compared to a
 * committed baseline.
 */

import { test, expect, type Locator, type Page, type TestInfo } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import {
  ASKER_NAME,
  BOB_ID,
  SELF_ID,
  answer,
  responsesThread,
  replacedThread,
  routeChoiceThread,
  setDecisions,
  setThread,
  shipDecision,
  singleThread,
  decision,
  type AskerLookup,
  type ChoiceThread,
} from './choices-mock.js';

type Theme = 'light' | 'dark';

/** Run on the desktop project and at 390 px; the other phone widths add nothing here. */
const PROJECTS = new Set(['desktop-1440', 'chromium-390']);

const isPhone = (page: Page): boolean => (page.viewportSize()?.width ?? 0) <= 768;

/**
 * Open #general showing `thread`, with web.chat_choices on and `theme`
 * applied the way index.html applies a saved choice (so the real theme
 * tokens are in effect).
 */
async function openChoices(
  page: Page,
  theme: Theme,
  thread: ChoiceThread,
  asker: AskerLookup = 'ok'
): Promise<void> {
  await openChatRail(page, async (p) => {
    await p.addInitScript((t) => {
      localStorage.setItem('scion-theme', t);
      window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, 'web.chat_choices': true };
    }, theme);
    await routeChoiceThread(p, thread, asker);
  });
  if (isPhone(page)) await openGeneralThread(page);
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
  const first = thread.messages[0]?.id as string;
  await expect(message(page, first)).toBeVisible({ timeout: 15_000 });
}

const message = (page: Page, id: string): Locator => page.locator(`[id="msg-${id}"]`);
const card = (page: Page, id: string): Locator => message(page, id).locator('scion-chat-choices');

/** Wait until the question markdown of `id`'s card has rendered (inline code). */
async function markdownReady(page: Page, id: string): Promise<void> {
  await expect(card(page, id).locator('.q code').first()).toBeVisible();
}

/**
 * Save a screenshot of the given messages (their union, padded) as
 * `choices-<project>-<theme>-<name>.png` in the test's output directory.
 */
async function shot(
  page: Page,
  testInfo: TestInfo,
  theme: Theme,
  name: string,
  ids: string[]
): Promise<void> {
  // Let Shoelace finish its transitions (button variants, tooltip hide).
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
  const file = testInfo.outputPath(`choices-${project}-${theme}-${name}.png`);
  await page.screenshot({ path: file, clip: { x, y, width: right - x, height: bottom - y } });
  await testInfo.attach(name, { path: file, contentType: 'image/png' });
}

for (const theme of ['light', 'dark'] as const) {
  test.describe(`choice cards (${theme})`, () => {
    // 2x so the saved screenshots are legible next to the mock renders.
    test.use({ colorScheme: theme, timezoneId: 'UTC', locale: 'en-US', deviceScaleFactor: 2 });

    test.beforeEach(({ page: _page }, testInfo) => {
      test.skip(!PROJECTS.has(testInfo.project.name), 'desktop and 390 px only');
    });

    test('single pending: recommended outlined, Enter hint, fallback hidden', async ({
      page,
    }, testInfo) => {
      await openChoices(page, theme, singleThread(shipDecision('prompt-1')));
      await markdownReady(page, 'prompt-1');
      const c = card(page, 'prompt-1');
      const group = c.locator('.card');
      await expect(group).toHaveAttribute('role', 'group');
      await expect(group).toHaveAttribute('aria-label', 'Ship v0.9 today?');
      await expect(group).toHaveAttribute('tabindex', '0');
      await expect(c.locator('.hint')).toHaveText(
        'Press Enter to choose the recommended answer (outlined).'
      );
      await expect(group).toHaveAttribute(
        'aria-describedby',
        (await c.locator('.hint').getAttribute('id'))!
      );
      const yes = c.locator('sl-button[data-choice="yes"]');
      await expect(yes).toHaveAttribute('variant', 'primary');
      await expect(yes).toHaveAttribute('outline', '');
      await expect(yes).toHaveAttribute('title', 'Yes (recommended)');
      await expect(c.locator('sl-button[data-choice="no"]')).toHaveAttribute('variant', 'default');
      await expect(c.locator('.row-foot')).toContainText('Awaiting your answer');
      await expect(message(page, 'prompt-1')).toContainText('One call before I tag it.');
      await expect(message(page, 'prompt-1')).not.toContainText('(fallback)');
      await shot(page, testInfo, theme, '01-single-pending', ['prompt-1']);
    });

    test('single with descriptions: stacked rows', async ({ page }, testInfo) => {
      const d = decision({
        messageId: 'prompt-desc',
        rowId: 'row-desc',
        key: '#17',
        question:
          'Which approach for the `input-needed` timeout?\n\nBoth keep the existing API; they differ in where the timer lives.',
        defaultChoiceId: 'extend',
        choices: [
          {
            id: 'extend',
            label: 'Extend input-needed',
            description: 'Add a `timeout` field; broker enforces it. ~1 day.',
          },
          {
            id: 'waiting',
            label: 'New waiting state',
            description: 'Separate state machine entry; touches web + hub. ~3 days.',
          },
        ],
      });
      await openChoices(page, theme, singleThread(d, ''));
      await markdownReady(page, 'prompt-desc');
      const c = card(page, 'prompt-desc');
      await expect(c.locator('.choices.stack')).toBeVisible();
      await expect(c.locator('button.opt.recd')).toHaveAttribute('data-choice', 'extend');
      await expect(c.locator('.desc code').first()).toHaveText('timeout');
      // The question's first paragraph is the question; the second is muted detail.
      const paragraphs = c.locator('.q p');
      await expect(paragraphs).toHaveCount(2);
      await expect(paragraphs.nth(0)).toHaveCSS('font-weight', '600');
      await expect(paragraphs.nth(1)).toHaveCSS('font-weight', '400');
      await shot(page, testInfo, theme, '03-single-descriptions', ['prompt-desc']);
    });

    test('single answered: Enter on the card picks the recommended choice', async ({
      page,
    }, testInfo) => {
      const pending = shipDecision('prompt-1');
      await openChoices(page, theme, singleThread(pending));
      await markdownReady(page, 'prompt-1');
      let body: Record<string, unknown> | null = null;
      await page.route('**/api/v1/decisions/*/answer', async (route) => {
        body = route.request().postDataJSON() as Record<string, unknown>;
        await route.fulfill({
          json: {
            decision: {
              ...pending,
              status: 'decided',
              decision: answer('yes', 'Yes', SELF_ID, 'Fixture User', 1),
            },
            responseMessageId: 'resp-1',
          },
        });
      });
      const c = card(page, 'prompt-1');
      await c.locator('.card').focus();
      await page.keyboard.press('Enter');
      await expect(c.locator('.row-foot')).toContainText('Answered by you');
      await expect(c.locator('.row-foot time')).toHaveText('15:42');
      expect(body).toMatchObject({ choice_id: 'yes' });
      const yes = c.locator('sl-button[data-choice="yes"]');
      await expect(yes).toHaveAttribute('variant', 'primary');
      await expect(yes).not.toHaveAttribute('outline', '');
      await expect(yes.locator('sl-icon[name="check-circle-fill"]')).toHaveCount(1);
      await expect(c.locator('sl-button[data-choice="no"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await expect(c.locator('.hint')).toHaveCount(0);
      await shot(page, testInfo, theme, '02-single-answered', ['prompt-1']);
    });

    test('set pending: defaults selected, "1 remaining", Submit disabled naming 17', async ({
      page,
    }, testInfo) => {
      await openChoices(page, theme, setThread('prompt-set', setDecisions('prompt-set')));
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      await expect(c.locator('.card.set')).toHaveAttribute(
        'aria-label',
        `3 decisions from ${ASKER_NAME}`
      );
      await expect(c.locator('.card-head .title')).toHaveText('3 decisions');
      await expect(c.locator('.row[role="group"]')).toHaveCount(3);
      const d234 = c.locator('.row[data-row="prompt-set-d234"]');
      await expect(d234.locator('sl-button[data-choice="yes"]')).toHaveAttribute(
        'aria-pressed',
        'true'
      );
      await expect(d234.locator('sl-button[data-choice="no"]')).toHaveAttribute(
        'aria-pressed',
        'false'
      );
      await expect(
        c.locator('.row[data-row="prompt-set-ship-now"] sl-button[data-choice="no"]')
      ).toHaveAttribute('aria-pressed', 'true');
      await expect(c.locator('.counter')).toHaveText('1 remaining');
      await expect(c.locator('.counter')).toHaveClass(/missing/);
      const submit = c.locator('sl-button.submit');
      await expect(submit).toHaveAttribute('aria-disabled', 'true');
      await expect(c.locator('sl-tooltip')).toHaveAttribute(
        'content',
        'Choose an answer for 17 first'
      );
      const hintId = await submit.getAttribute('aria-describedby');
      await expect(c.locator(`[id="${hintId}"]`)).toHaveText('Choose an answer for 17 first');
      await shot(page, testInfo, theme, '04-set-pending', ['prompt-set']);
    });

    test('set pending with a stacked row (mobile mock)', async ({ page }, testInfo) => {
      await openChoices(
        page,
        theme,
        setThread(
          'prompt-set',
          setDecisions('prompt-set', { descriptions: true }),
          'Three calls before the release.'
        )
      );
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      await expect(c.locator('.row[data-row="prompt-set-17"] .choices.stack')).toBeVisible();
      await expect(c.locator('.counter')).toHaveText('1 remaining');
      await shot(page, testInfo, theme, '13-set-pending-stacked', ['prompt-set']);
    });

    test('set ready, then submitted', async ({ page }, testInfo) => {
      const decisions = setDecisions('prompt-set');
      await openChoices(page, theme, setThread('prompt-set', decisions));
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      await c.locator('.row[data-row="prompt-set-17"] sl-button[data-choice="a"]').click();
      await expect(c.locator('.counter')).toHaveText('0 remaining');
      await expect(c.locator('.counter')).not.toHaveClass(/missing/);
      const submit = c.locator('sl-button.submit');
      await expect(submit).not.toHaveAttribute('disabled', '');
      await expect(c.locator('sl-tooltip')).toHaveCount(0);
      await shot(page, testInfo, theme, '05-set-ready', ['prompt-set']);

      let body: { answers?: { decision_row_id: string; choice_id: string }[] } | null = null;
      const picked: Record<string, [string, string]> = {
        D234: ['yes', 'Yes'],
        '17': ['a', 'a · Extend input-needed'],
        'ship-now': ['no', 'No'],
      };
      await page.route('**/api/v1/choice-prompts/*/answers', async (route) => {
        body = route.request().postDataJSON() as typeof body;
        await route.fulfill({
          json: {
            responseMessageId: 'resp-set',
            batchId: 'batch-1',
            decisions: decisions.map((d) => {
              const [id, label] = picked[d.key as string];
              return {
                ...d,
                status: 'decided',
                decision: answer(id, label, SELF_ID, 'Fixture User', 3, { batchId: 'batch-1' }),
              };
            }),
          },
        });
      });
      await submit.click();
      await expect(c.locator('.card-foot .counter')).toContainText('Submitted by you');
      await expect(c.locator('.card-foot time')).toHaveText('15:44');
      await expect(c.locator('.row .row-foot')).toHaveCount(3);
      for (const foot of await c.locator('.row .row-foot').all()) {
        await expect(foot).toContainText('Answered by you');
      }
      expect(body!.answers).toEqual([
        { decision_row_id: 'prompt-set-d234', choice_id: 'yes' },
        { decision_row_id: 'prompt-set-17', choice_id: 'a' },
        { decision_row_id: 'prompt-set-ship-now', choice_id: 'no' },
      ]);
      await expect(c.locator('sl-button.submit')).toHaveCount(0);
      await expect(c.locator('[aria-pressed]')).toHaveCount(0);
      await shot(page, testInfo, theme, '06-set-submitted', ['prompt-set']);
    });

    test('set with a replaced row: excluded, jump link, "1 replaced"', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-set', {
        overrides: {
          '17': {
            status: 'replaced',
            replacedBy: {
              rowId: 'row-17-v2',
              messageId: 'prompt-set-v2',
              version: 2,
              at: '2026-09-01T15:43:00Z',
            },
          },
        },
      });
      await openChoices(page, theme, setThread('prompt-set', decisions));
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      const replaced = c.locator('.row[data-row="prompt-set-17"]');
      await expect(replaced).toHaveClass(/replaced/);
      await expect(replaced.locator('.row-foot')).toContainText('Replaced by v2');
      await expect(replaced.locator('a.jump')).toHaveText('Jump to v2');
      await expect(replaced.locator('sl-button').first()).toHaveAttribute('aria-disabled', 'true');
      await expect(c.locator('.counter')).toHaveText('0 remaining · 1 replaced');
      await shot(page, testInfo, theme, '07-set-replaced-row', ['prompt-set']);
    });

    test('set 409 answer_set_changed: inline alert, selections kept', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-set');
      await openChoices(page, theme, setThread('prompt-set', decisions));
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      await c.locator('.row[data-row="prompt-set-d234"] sl-button[data-choice="no"]').click();
      await c.locator('.row[data-row="prompt-set-17"] sl-button[data-choice="a"]').click();
      await page.route('**/api/v1/choice-prompts/*/answers', (route) =>
        route.fulfill({
          status: 409,
          json: {
            error: {
              code: 'answer_set_changed',
              message: 'The decisions changed; review and submit again.',
              details: {
                decisions: decisions.map((d) =>
                  d.key === '17'
                    ? {
                        ...d,
                        status: 'decided',
                        decision: answer('b', 'b · New waiting state', BOB_ID, 'Bob', 2),
                      }
                    : d
                ),
                reasons: { 'prompt-set-17': 'decided' },
              },
            },
          },
        })
      );
      await c.locator('sl-button.submit').click();
      const alert = c.locator('sl-alert.changed');
      await expect(alert).toHaveAttribute('variant', 'warning');
      await expect(alert).toContainText('This changed while you were choosing:');
      await expect(alert).toContainText('17 was answered by Bob');
      await expect(alert).toContainText(
        'Your other selections are kept — review and submit again.'
      );
      await expect(
        c.locator('.row[data-row="prompt-set-d234"] sl-button[data-choice="no"]')
      ).toHaveAttribute('aria-pressed', 'true');
      await expect(c.locator('.row[data-row="prompt-set-17"] .row-foot')).toContainText(
        'Answered by Bob'
      );
      await expect(c.locator('.counter')).toHaveText('0 remaining · 1 answered by Bob');
      await shot(page, testInfo, theme, '08-set-409', ['prompt-set']);
    });

    test('replaced card with a jump to its replacement', async ({ page }, testInfo) => {
      await openChoices(page, theme, replacedThread());
      const v1 = card(page, 'prompt-v1');
      await expect(v1.locator('.row')).toHaveClass(/replaced/);
      await expect(v1.locator('.card-head .version')).toHaveText('v1');
      await expect(card(page, 'prompt-v2').locator('.card-head .version')).toHaveText('v2');
      await expect(v1.locator('.row-foot')).toContainText('Replaced by v2');
      for (const b of await v1.locator('sl-button').all()) {
        await expect(b).toHaveAttribute('aria-disabled', 'true');
      }
      const jump = v1.locator('a.jump');
      await expect(jump).toHaveText('Jump to v2');
      await expect(jump).toHaveAttribute('href', '#msg-prompt-v2');
      await shot(page, testInfo, theme, '09-replaced', ['prompt-v1', 'prompt-v2']);
      await jump.click();
      await expect(message(page, 'prompt-v2')).toHaveClass(/permalink-highlight/);
      await expect(message(page, 'prompt-v2')).toBeInViewport();
    });

    test('interpreted answer is read-only', async ({ page }, testInfo) => {
      const d = shipDecision('prompt-int', {
        status: 'decided',
        decision: answer('no', 'No', BOB_ID, 'Bob', 2, {
          provenance: 'agent_interpreted',
          sourceMessageId: 'bob-reply',
        }),
      });
      await openChoices(page, theme, singleThread(d));
      await markdownReady(page, 'prompt-int');
      const c = card(page, 'prompt-int');
      await expect(c.locator('.row-foot.interpreted')).toContainText(
        `Interpreted by ${ASKER_NAME} from Bob's reply`
      );
      await expect(c.locator('.row-foot.interpreted sl-icon[name="robot"]')).toHaveCount(1);
      await expect(c.locator('sl-button[data-choice="no"]')).toHaveAttribute('variant', 'primary');
      await expect(c.locator('sl-button[data-choice="yes"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await expect(c.locator('sl-button[data-choice="wait"]')).toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await shot(page, testInfo, theme, '09b-interpreted', ['prompt-int']);
    });

    test('blocked: the asker refuses the viewer, reason shown before clicking', async ({
      page,
    }, testInfo) => {
      const decisions = setDecisions('prompt-set').slice(0, 2);
      await openChoices(
        page,
        theme,
        setThread('prompt-set', decisions, 'Three calls before the release.'),
        'denied'
      );
      await markdownReady(page, 'prompt-set');
      const c = card(page, 'prompt-set');
      const reason = c.locator('.reason[data-reason="denied"]');
      await expect(reason).toContainText(
        `You can't answer ${ASKER_NAME}: ${ASKER_NAME} is in lineage mode and only accepts messages from users in its ancestry. You are not in its lineage.`
      );
      await expect(reason.locator('sl-icon[name="lock"]')).toHaveCount(1);
      const group = c.locator('.card.set');
      await expect(group).toHaveAttribute('aria-disabled', 'true');
      await expect(group).toHaveAttribute('aria-describedby', (await reason.getAttribute('id'))!);
      for (const b of await c.locator('.row sl-button').all()) {
        await expect(b).toHaveAttribute('aria-disabled', 'true');
      }
      await expect(c.locator('sl-button.submit')).toHaveAttribute('aria-disabled', 'true');
      await shot(page, testInfo, theme, '10-blocked', ['prompt-set']);
    });

    test('read-only thread: disabled with the read-only reason', async ({ page }, testInfo) => {
      const thread = singleThread(shipDecision('prompt-ro'));
      thread.canPost = false;
      await openChoices(page, theme, thread);
      await markdownReady(page, 'prompt-ro');
      const c = card(page, 'prompt-ro');
      const reason = c.locator('.reason[data-reason="read-only"]');
      await expect(reason).toHaveText(
        "You can read this thread but can't post in it, so you can't answer."
      );
      await expect(reason.locator('sl-icon[name="eye"]')).toHaveCount(1);
      await expect(c.locator('.card')).toHaveAttribute('aria-disabled', 'true');
      for (const b of await c.locator('sl-button').all()) {
        await expect(b).toHaveAttribute('aria-disabled', 'true');
      }
      await expect(c.locator('.hint')).toHaveCount(0);
      await shot(page, testInfo, theme, '11a-read-only', ['prompt-ro']);
    });

    test('asker gone: still answerable, with a not-delivered note', async ({ page }, testInfo) => {
      await openChoices(page, theme, singleThread(shipDecision('prompt-gone')), 'gone');
      await markdownReady(page, 'prompt-gone');
      const c = card(page, 'prompt-gone');
      const note = c.locator('.note[data-note="asker-gone"]');
      await expect(note).toHaveText(
        `${ASKER_NAME} is no longer running. Your answer will be recorded but not delivered.`
      );
      await expect(note).toHaveAttribute('role', 'note');
      await expect(c.locator('.card')).not.toHaveAttribute('aria-disabled', 'true');
      await expect(c.locator('sl-button[data-choice="no"]')).not.toHaveAttribute(
        'aria-disabled',
        'true'
      );
      await expect(c.locator('.row-foot')).toContainText('Awaiting your answer');
      await shot(page, testInfo, theme, '11b-asker-gone', ['prompt-gone']);
    });

    test('response lines: single, set collapsed and expanded, not delivered', async ({
      page,
    }, testInfo) => {
      await openChoices(page, theme, responsesThread());
      const line = (id: string): Locator => message(page, id).locator('scion-chat-choice-response');
      await expect(line('resp-own').locator('[data-kind="single"]')).toContainText(
        'you chose Yes for D234 Ship v0.9 today?'
      );
      await expect(line('resp-own').locator('time')).toHaveText('15:42');
      await expect(line('resp-bob').locator('[data-kind="single"]')).toContainText(
        'Bob chose New waiting state for 17 Which approach?'
      );
      await expect(message(page, 'resp-own').locator('.choice-response-row')).toHaveClass(/own/);
      await expect(message(page, 'resp-bob').locator('.choice-response-row')).toHaveClass(/other/);
      const set = line('resp-set').locator('details[data-kind="set"]');
      await expect(set.locator('summary')).toContainText('you answered 3 decisions');
      await expect(set.locator('summary')).toContainText('Show answers ▸');
      await expect(set).not.toHaveAttribute('open', '');
      await expect(line('resp-failed').locator('[data-state="not-delivered"]')).toHaveText(
        'not delivered (agent gone)'
      );
      await expect(line('resp-own').locator('[data-state="not-delivered"]')).toHaveCount(0);
      await shot(page, testInfo, theme, '12-responses-collapsed', [
        'resp-own',
        'resp-bob',
        'resp-set',
        'resp-failed',
      ]);

      await set.locator('summary').click();
      await expect(set).toHaveAttribute('open', '');
      await expect(set.locator('summary')).toContainText('Hide answers ▾');
      const items = set.locator('ul.answers li[data-row]');
      await expect(items).toHaveCount(3);
      await expect(items.nth(0)).toContainText('D234');
      await expect(items.nth(0)).toContainText('Yes (recommended)');
      await expect(items.nth(1)).toContainText('a · Extend input-needed');
      await expect(items.nth(2)).not.toContainText('(recommended)');
      await expect(set.locator('[data-action="jump"]')).toHaveText('Jump to question');
      await shot(page, testInfo, theme, '12-responses-expanded', [
        'resp-own',
        'resp-bob',
        'resp-set',
        'resp-failed',
      ]);

      await line('resp-own').locator('[data-action="jump"]').click();
      await expect(message(page, 'prompt-resp')).toHaveClass(/permalink-highlight/);
    });
  });
}
