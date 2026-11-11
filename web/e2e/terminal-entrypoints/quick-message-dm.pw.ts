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
 * The quick message dialog's "Open agent DM" button: from the agent detail
 * page and the agent graph it closes the dialog and lands on that agent's
 * DM in chat, with any unsent text carried into the DM composer.
 */

import { test, expect, type Page } from '@playwright/test';
import { agentA, setup, switchView } from './fixtures.js';

const DM_KEY = `dm:agent:${agentA}:user:fixture-user`;
const DM_URL = new RegExp(`/chat/dm/${encodeURIComponent(DM_KEY)}$`);

/**
 * The shared fixtures answer every agent sub-resource with `{}`, which the
 * agent detail page renders as a metrics summary and throws on, so no later
 * update (such as opening the dialog) renders. No metrics is a real state.
 */
async function setupWithoutMetrics(page: Page): Promise<void> {
  await setup(page);
  await page.route('**/api/v1/agents/*/metrics/**', (route) =>
    route.fulfill({ status: 404, json: { error: 'not found' } })
  );
}

async function openDMFromDialog(page: Page, draft: string): Promise<void> {
  const dialog = page.locator('scion-quick-message-dialog sl-dialog[open]');
  await expect(dialog).toBeVisible();
  await dialog.locator('sl-textarea textarea').fill(draft);
  const button = dialog.locator('sl-button.open-dm');
  await expect(button).toBeVisible();
  await expect(button).toHaveText(/Open agent DM/);
  await button.click();
}

async function expectAgentDM(page: Page, draft: string): Promise<void> {
  await expect(page).toHaveURL(DM_URL);
  await expect(page.locator('scion-quick-message-dialog sl-dialog[open]')).toHaveCount(0);
  await expect(page.locator('scion-chat-composer textarea').first()).toHaveValue(draft);
}

test('agent detail: Open agent DM lands on the agent DM with the draft', async ({ page }) => {
  await setupWithoutMetrics(page);
  await page.goto(`/agents/${agentA}`);
  await page.getByRole('button', { name: 'Message', exact: true }).click();
  await openDMFromDialog(page, 'from detail');
  await expectAgentDM(page, 'from detail');
});

test('agent graph: Open agent DM lands on the agent DM with the draft', async ({ page }) => {
  await setup(page);
  await page.goto('/agents');
  await switchView(page, 'scion-view-agents', 'graph');
  await page.locator('sl-icon-button.message-btn').first().click();
  await openDMFromDialog(page, 'from graph');
  await expectAgentDM(page, 'from graph');
});

test('narrow width: footer keeps Cancel and Send, DM button sits left', async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 740 });
  await setupWithoutMetrics(page);
  await page.goto(`/agents/${agentA}`);
  await page.getByRole('button', { name: 'Message', exact: true }).click();
  const dialog = page.locator('scion-quick-message-dialog sl-dialog[open]');
  await expect(dialog).toBeVisible();
  const dm = await dialog.locator('sl-button.open-dm').boundingBox();
  const cancel = await dialog.locator('sl-button', { hasText: 'Cancel' }).boundingBox();
  const send = await dialog.locator('sl-button', { hasText: 'Send' }).boundingBox();
  expect(dm && cancel && send).toBeTruthy();
  expect(dm!.x).toBeLessThan(cancel!.x);
  expect(cancel!.x).toBeLessThan(send!.x);
  expect(send!.x + send!.width).toBeLessThanOrEqual(360);

  await openDMFromDialog(page, 'from mobile');
  await expectAgentDM(page, 'from mobile');
});
