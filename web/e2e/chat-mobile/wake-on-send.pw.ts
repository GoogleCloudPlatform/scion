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
 * Wake-on-send: a message to a suspended agent the user may wake gets a
 * dialog instead of a failed "Agent unreachable" bubble. "Wake and send"
 * resends the message with `wake` and the bubble shows "Waking agent…"
 * until the hub answers; Cancel leaves the draft in the composer.
 */

import { test, expect, type Page, type Request } from '@playwright/test';
import { openChatRail, openGeneralThread } from './fixture.js';

const DRAFT = 'please pick up the release notes';

/** The hub's wake offer for a suspended agent the caller may wake. */
const WAKE_OFFER = {
  error: {
    code: 'agent_not_running',
    message: 'Agent "coder-one" is suspended',
    details: { agentId: 'agent-1', agentSlug: 'coder-one', phase: 'suspended', canWake: true },
  },
};

/**
 * Mock the send endpoint: a plain send gets the wake offer; a wake send is
 * held until `release` is called, then answers 201 dispatched. Returns the
 * POST bodies seen and the release function.
 */
async function mockWakeSend(
  page: Page
): Promise<{ bodies: Array<Record<string, unknown>>; release: () => void }> {
  const bodies: Array<Record<string, unknown>> = [];
  let release!: () => void;
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route(/\/api\/v1\/chat\/conversations\/[^/?]+\/messages$/, async (route) => {
    const req: Request = route.request();
    if (req.method() !== 'POST') {
      await route.fallback();
      return;
    }
    const body = req.postDataJSON() as Record<string, unknown>;
    bodies.push(body);
    if (body.wake !== true) {
      await route.fulfill({ status: 409, json: WAKE_OFFER });
      return;
    }
    await released;
    await route.fulfill({
      status: 201,
      json: { id: 'woken-msg-1', content: DRAFT, dispatchState: 'dispatched' },
    });
  });
  return { bodies, release };
}

async function typeAndSend(page: Page, text: string): Promise<void> {
  const textarea = page.locator('sl-textarea').first();
  await textarea.evaluate((el) => (el as unknown as HTMLElement).focus());
  await page.keyboard.type(text);
  await page.locator('.send-btn').first().click();
}

function composerValue(page: Page): Promise<string> {
  return page
    .locator('sl-textarea')
    .first()
    .evaluate((el) => (el as unknown as { value: string }).value);
}

// Tagged @static: click and type only, no gestures, so they also run on
// the opt-in webkit-iphone project (PW_WEBKIT=1).
test.describe('wake on send', () => {
  test('@static "Wake and send" wakes the agent and delivers the message', async ({ page }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    const send = await mockWakeSend(page);

    await typeAndSend(page, DRAFT);

    const dialog = page.locator('sl-dialog[label="Agent is suspended"]');
    await expect(dialog).toHaveAttribute('open', '');
    await expect(dialog).toContainText('@coder-one is suspended');
    await dialog.getByRole('button', { name: 'Wake and send' }).click();

    // While the hub resumes the agent the bubble says so.
    const bubble = page.locator('scion-chat-message', { hasText: DRAFT }).first();
    await expect(bubble.locator('.delivery-state')).toContainText('Waking agent');

    send.release();
    await expect(bubble.locator('.delivery-state')).toContainText('Delivered');

    expect(send.bodies).toHaveLength(2);
    expect(send.bodies[0]).toMatchObject({ content: DRAFT, offer_wake: true });
    expect(send.bodies[0]).not.toHaveProperty('wake');
    expect(send.bodies[1]).toMatchObject({ content: DRAFT, wake: true });
    expect(send.bodies[1]).not.toHaveProperty('offer_wake');
    expect(await composerValue(page)).toBe('');
  });

  test('@static Cancel keeps the draft and sends nothing more', async ({ page }) => {
    await openChatRail(page);
    await openGeneralThread(page);
    const send = await mockWakeSend(page);

    await typeAndSend(page, DRAFT);

    const dialog = page.locator('sl-dialog[label="Agent is suspended"]');
    await expect(dialog).toHaveAttribute('open', '');
    await dialog.getByRole('button', { name: 'Cancel' }).click();
    await expect(dialog).toHaveCount(0);

    await expect(async () => {
      expect(await composerValue(page)).toBe(DRAFT);
    }).toPass();
    await expect(page.locator('scion-chat-message', { hasText: DRAFT })).toHaveCount(0);
    expect(send.bodies).toHaveLength(1);
  });
});
