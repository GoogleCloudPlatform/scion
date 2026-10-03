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
 * On touch, sending a message blurs the composer instead of re-focusing it,
 * so the on-screen keyboard retracts instead of covering the reply the user
 * is waiting to read. Desktop keeps today's re-focus. A failed send must
 * not refocus on touch either.
 */

import { test, expect, type Page } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';
import { deepActiveElementTagName } from './helpers.js';

/** Mock a successful or failing POST to the conversation-messages endpoint. */
async function mockSend(page: Page, { ok }: { ok: boolean }): Promise<void> {
  await page.route(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages$/, (route) => {
    if (route.request().method() !== 'POST') {
      void route.fallback();
      return;
    }
    if (ok) {
      void route.fulfill({ json: { id: 'sent-msg-1', content: 'hello from the test' } });
    } else {
      void route.fulfill({ status: 500, json: { error: 'boom' } });
    }
  });
}

async function typeAndSend(page: Page, text: string): Promise<void> {
  const textarea = page.locator('sl-textarea').first();
  // A pointer click lands reliably on Chromium's mobile emulation, but on
  // WebKit's iPhone device profile the actionability check sometimes finds
  // the page host element intercepting the hit point instead of the
  // composer underneath it. Focusing directly is just as good a setup step
  // here — this test is about the blur-after-send behavior, not about
  // proving a tap can focus the composer (a plain click does that
  // elsewhere, e.g. font-size.pw.ts's rename/new-thread flows).
  await textarea.evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type(text);
  await page.locator('.send-btn').first().click();
}

test('@static touch: after a successful send, the composer is not the deep active element', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch-only behavior');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSend(page, { ok: true });

  await typeAndSend(page, 'hello from the test');

  await expect(page.locator('.messages-scroll', { hasText: 'hello from the test' })).toBeVisible();
  expect(await deepActiveElementTagName(page)).not.toBe('textarea');
});

test('@static touch: a failed send restores the draft without refocusing', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch-only behavior');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSend(page, { ok: false });

  await typeAndSend(page, 'this send will fail');

  // The failed send restores the draft text into the textarea.
  const textarea = page.locator('sl-textarea').first();
  await expect(async () => {
    expect(await textarea.evaluate((el) => (el as unknown as { value: string }).value)).toBe(
      'this send will fail'
    );
  }).toPass();
  expect(await deepActiveElementTagName(page)).not.toBe('textarea');
});

test('@static touch: after saving an edit, the composer is not the deep active element', async ({
  page,
}, testInfo) => {
  test.skip(testInfo.project.name === 'desktop-1440', 'touch-only behavior');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSend(page, { ok: true });
  await page.route(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages\/[^/?]+$/, (route) => {
    if (route.request().method() !== 'PUT') {
      void route.fallback();
      return;
    }
    void route.fulfill({ json: {} });
  });

  // Send a message first, so there is an own, just-sent (and therefore
  // editable) message to open the Edit path on.
  await typeAndSend(page, 'edit me please');
  const sentMessage = page.locator('scion-chat-message', { hasText: 'edit me please' }).first();
  await expect(sentMessage).toBeVisible();

  // A tap on a message opens the same actions a desktop right-click would,
  // on devices that cannot hover — as an action sheet on a phone.
  await sentMessage.click();
  await page.locator('scion-action-sheet[open]').locator('.item', { hasText: 'Edit' }).click();

  const textarea = page.locator('sl-textarea').first();
  await expect(async () => {
    expect(await textarea.evaluate((el) => (el as unknown as { value: string }).value)).toBe(
      'edit me please'
    );
  }).toPass();

  await page.keyboard.type(' - edited');
  await page.locator('.send-btn').first().click();

  expect(await deepActiveElementTagName(page)).not.toBe('textarea');
});

test('desktop: after a successful send, the textarea keeps focus', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'desktop-1440', 'desktop-only behavior');
  await openChatRail(page);
  await openGeneralThread(page);
  await mockSend(page, { ok: true });

  await typeAndSend(page, 'hello from the test');

  await expect(page.locator('.messages-scroll', { hasText: 'hello from the test' })).toBeVisible();
  expect(await deepActiveElementTagName(page)).toBe('textarea');
});
