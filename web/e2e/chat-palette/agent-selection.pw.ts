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
 * Chromium: typing and arrows select an actual agent candidate; Enter opens
 * its correct DM with typed peerKind. An agent without a pre-existing DM
 * opens empty without a create request; focus moves to the new composer
 * after close.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  setupApiMocks,
  AGENT_WITH_DM,
  AGENT_WITHOUT_DM,
  AGENT_NOT_VIABLE,
  SELF_USER_ID,
} from './mock-api.js';

async function gotoChat(page: Page) {
  const requests = await setupApiMocks(page);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  return requests;
}

function paletteInput(page: Page) {
  return page.locator('scion-quick-palette #palette-query-input');
}

function paletteOptions(page: Page) {
  return page.locator('scion-quick-palette .palette-option');
}

test('typing narrows to a real agent candidate and Enter opens its correct DM', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();

  await paletteInput(page).fill(AGENT_WITH_DM.name);
  await expect(paletteOptions(page)).toHaveCount(1);
  await expect(paletteOptions(page).first()).toContainText(AGENT_WITH_DM.name);

  await page.keyboard.press('Enter');

  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
  await expect(page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]')).toBeHidden();
});

test('pressing Enter twice in a row commits only once (double-tap / commit-once protection)', async ({
  page,
}) => {
  // Isolates commitActivePaletteCandidate's own `if (this.committed)
  // return;` guard — distinct from handlePaletteKeydown's
  // `if (e.repeat) return;`, which every other
  // double-Enter test in this suite relies on instead (a real OS key
  // repeat sets `repeat: true`, which that other guard alone already
  // rejects). Two back-to-back `page.keyboard.press('Enter')` calls are
  // both ordinary, non-repeat keydowns — the shape of a real double-tap,
  // or Enter landing twice while the dialog's own hide animation is still
  // running (the switcher element, and its keydown listener, stay mounted
  // during that window) — and only the `committed` guard can reject the
  // second one.
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(AGENT_WITH_DM.name);
  await expect(paletteOptions(page)).toHaveCount(1);

  const pushStateCallsBefore = await page.evaluate(
    () => (window as unknown as { __pushStateCalls?: number }).__pushStateCalls ?? 0
  );
  await page.evaluate(() => {
    const w = window as unknown as { __pushStateCalls?: number };
    w.__pushStateCalls = 0;
    const original = history.pushState.bind(history);
    history.pushState = ((...args: Parameters<History['pushState']>) => {
      w.__pushStateCalls = (w.__pushStateCalls ?? 0) + 1;
      return original(...args);
    }) as typeof history.pushState;
  });

  await page.keyboard.press('Enter');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(300);

  const pushStateCallsAfter = await page.evaluate(
    () => (window as unknown as { __pushStateCalls?: number }).__pushStateCalls ?? 0
  );
  expect(pushStateCallsAfter - pushStateCallsBefore).toBe(1);
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITH_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
});

test('arrow keys move the active candidate before committing', async ({ page }) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  // Empty query: ranked by recency. AGENT_WITH_DM has a DM (recent activity);
  // AGENT_WITHOUT_DM has none (activityMs=0). No row is active until the
  // user picks one; the first ArrowDown picks AGENT_WITH_DM, the top row.
  // Both agent rows have loaded before the first key, which acts on them.
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toBeVisible();
  await expect(page.locator('scion-quick-palette .palette-option.active')).toHaveCount(0);
  await page.keyboard.press('ArrowDown');
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name })).toHaveClass(/active/);

  await page.keyboard.press('ArrowDown');
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toHaveClass(
    /active/
  );

  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITHOUT_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
});

test('at most one row has aria-selected="true" at a time, and it follows ArrowDown', async ({
  page,
}) => {
  // No option carries aria-selected="true" until the user picks a row, then
  // exactly one does, and it tracks the active option.
  await gotoChat(page);
  await page.keyboard.press('Control+k');

  const selected = () => page.locator('scion-quick-palette .palette-option[aria-selected="true"]');
  // Empty query: ranked by recency, same ordering as the test above.
  // Both agent rows have loaded before the first key, which acts on them.
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toBeVisible();
  await expect(selected()).toHaveCount(0);
  await page.keyboard.press('ArrowDown');
  await expect(selected()).toHaveCount(1);
  await expect(selected()).toContainText(AGENT_WITH_DM.name);

  await page.keyboard.press('ArrowDown');
  await expect(selected()).toHaveCount(1);
  await expect(selected()).toContainText(AGENT_WITHOUT_DM.name);
});

test('hovering another row does not change the keyboard-selected candidate that Enter commits', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  // Empty query: AGENT_WITH_DM is the top row. Two ArrowDowns keyboard-select
  // AGENT_WITHOUT_DM, which Enter on an empty query then commits.
  // Both agent rows have loaded before the first key, which acts on them.
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toBeVisible();
  await page.keyboard.press('ArrowDown');
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name })).toHaveClass(/active/);
  await page.keyboard.press('ArrowDown');
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toHaveClass(
    /active/
  );

  await paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name }).hover();
  // Hover is purely visual (CSS :hover) — it must not touch the
  // keyboard-selected (active) candidate.
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITHOUT_DM.name })).toHaveClass(
    /active/
  );
  await expect(paletteOptions(page).filter({ hasText: AGENT_WITH_DM.name })).not.toHaveClass(
    /active/
  );

  await page.keyboard.press('Enter');

  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:agent:${AGENT_WITHOUT_DM.id}:user:${SELF_USER_ID}`)}$`
    )
  );
});

test('a non-viable (canMessage=false) agent never appears as a candidate', async ({ page }) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(AGENT_NOT_VIABLE.name);
  // Scoped to the Agents group specifically: the palette also renders
  // Threads/People, and this fixture's default (empty) Threads/People groups
  // also show their own "No matches" for a query that matches nothing
  // anywhere.
  await expect(
    page.locator('scion-quick-palette [aria-labelledby="palette-heading-agents"] .palette-empty')
  ).toBeVisible();
  await expect(paletteOptions(page)).toHaveCount(0);
});

test('selecting an agent with no pre-existing DM opens it empty with no create request', async ({
  page,
}) => {
  const requests = await gotoChat(page);
  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(AGENT_WITHOUT_DM.name);
  // The agents/DM fetch is async, so a fill()-then-Enter without waiting
  // for the row to actually render is a real race — if Enter lands before
  // the group finishes loading, `activeId` is still null and
  // commitActivePaletteCandidate is a no-op for an empty/loading-only
  // state, leaving the URL at /chat. Waiting for the row to render here
  // avoids that race entirely.
  await expect(paletteOptions(page)).toHaveCount(1);
  await page.keyboard.press('Enter');

  const expectedKey = `dm:agent:${AGENT_WITHOUT_DM.id}:user:${SELF_USER_ID}`;
  await expect(page).toHaveURL(new RegExp(`/chat/dm/${encodeURIComponent(expectedKey)}$`));

  // The composer for the new, empty DM must be present and ready to type into.
  await page.waitForSelector('scion-chat-composer sl-textarea', {
    state: 'attached',
    timeout: 10_000,
  });

  // No DM-creation request of any kind was made — the key is constructed
  // locally and the conversation exists only once the first message sends.
  const writes = requests.filter((r) => r.method !== 'GET');
  const dmCreateAttempts = writes.filter(
    (r) => /\/api\/v1\/chat\/dms?(\/|$)/.test(new URL(r.url).pathname) && r.method === 'POST'
  );
  expect(dmCreateAttempts).toHaveLength(0);
});

test('focus moves to the new conversation composer after the palette closes on selection', async ({
  page,
}) => {
  await gotoChat(page);
  await page.keyboard.press('Control+k');
  await paletteInput(page).fill(AGENT_WITH_DM.name);
  // See the identical wait in the previous test.
  await expect(paletteOptions(page)).toHaveCount(1);
  await page.keyboard.press('Enter');

  const textarea = page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
  await expect(textarea).toBeFocused({ timeout: 10_000 });
});
