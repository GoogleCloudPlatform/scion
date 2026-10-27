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
 * The Starred rail (web.chat_starred_messages), with mock data and a mocked
 * hub that serves GET /api/v1/chat/starred-conversations from its own star
 * state (sorted and paged as the hub does) and the starred-only history:
 *
 * - All / Unread / Starred; the DMs section and project headings; Recent
 *   and Alphabetical; Load more conversations; No starred conversations yet;
 * - selecting a row opens the starred-only center (on a phone the rail
 *   drawer closes); removing the last star drops the row and keeps the
 *   center open; Jump to message in conversation returns the rail to All.
 */

import { test, expect } from '@playwright/test';
import { PROJECT_A, PROJECT_B } from './mock-api.js';
import {
  setupStarRail,
  defaultConversations,
  mockThread,
  msgId,
  HUMAN_DM,
  rail,
  filterButton,
  starredRow,
  banner,
  isPhone,
  starredRowNames,
  railHeadings,
  openStarredRail,
  chooseFromMenu,
} from './star-rail-mock.js';

test.describe('Starred rail', () => {
  test('lists the starred conversations in a DMs section and project headings, Recent first', async ({
    page: pw,
  }) => {
    const hub = await setupStarRail(pw);
    await openStarredRail(pw);
    await expect
      .poll(() => railHeadings(pw))
      .toEqual(['Direct messages', PROJECT_A.name, PROJECT_B.name]);
    await expect
      .poll(() => starredRowNames(pw))
      .toEqual([
        'Mock Person',
        'Mock helper agent',
        'Mock planning',
        'mock review',
        'Mock rollout',
      ]);
    // Rows come from the endpoint, never from the loaded rail.
    await expect(rail(pw).getByText('Mock no stars')).toHaveCount(0);
    expect(hub.starredCalls[0]?.get('sort')).toBe('activity');
  });

  test('Alphabetical follows the shared sort preference', async ({ page: pw }) => {
    const hub = await setupStarRail(pw, defaultConversations(), 'alpha');
    await openStarredRail(pw);
    await expect.poll(() => hub.starredCalls.at(-1)?.get('sort')).toBe('alpha');
    await expect
      .poll(() => railHeadings(pw))
      .toEqual(['Direct messages', PROJECT_A.name, PROJECT_B.name]);
    await expect
      .poll(() => starredRowNames(pw))
      .toEqual([
        'Mock helper agent',
        'Mock Person',
        'Mock planning',
        'mock review',
        'Mock rollout',
      ]);
  });

  test('Load more conversations fills an existing heading without repeating it', async ({
    page: pw,
  }) => {
    const many = Array.from({ length: 33 }, (_, i) =>
      mockThread(
        `topic-mock-${String(i).padStart(2, '0')}`,
        `Mock thread ${String(i).padStart(2, '0')}`,
        PROJECT_B,
        new Date(Date.parse('2026-10-22T10:00:00Z') - i * 60_000).toISOString(),
        [1]
      )
    );
    const hub = await setupStarRail(pw, many);
    await openStarredRail(pw);
    await expect.poll(() => starredRowNames(pw).then((n) => n.length)).toBe(30);
    const more = rail(pw).getByRole('button', { name: 'Load more conversations' });
    await expect(more).toBeVisible();
    await more.click();
    await expect.poll(() => starredRowNames(pw).then((n) => n.length)).toBe(33);
    expect(hub.starredCalls.at(-1)?.get('cursor')).toBe('o-30');
    await expect.poll(() => railHeadings(pw)).toEqual([PROJECT_B.name]);
    await expect(more).toHaveCount(0);
  });

  test('an empty collection says No starred conversations yet and explains the action', async ({
    page: pw,
  }) => {
    await setupStarRail(pw, [
      mockThread('topic-mock-none', 'Mock none', PROJECT_A, '2026-10-22T10:00:00Z', []),
    ]);
    await openStarredRail(pw);
    await expect(rail(pw).getByText('No starred conversations yet')).toBeVisible({
      timeout: 15_000,
    });
    await expect(rail(pw).getByText('Star message')).toBeVisible();
  });

  test('selecting a row opens the starred-only center; on a phone the rail drawer closes', async ({
    page: pw,
  }) => {
    await setupStarRail(pw);
    await openStarredRail(pw);
    await starredRow(pw, 'topic-mock-planning').click();
    await expect(pw).toHaveURL(/topic-mock-planning\?messages=starred$/, { timeout: 15_000 });
    await expect(banner(pw)).toBeVisible({ timeout: 15_000 });
    await expect(pw.locator(`#msg-${msgId('topic-mock-planning', 2)}`)).toBeVisible();
    await expect(pw.locator(`#msg-${msgId('topic-mock-planning', 1)}`)).toHaveCount(0);
    if (isPhone(pw)) {
      await expect(pw.locator('.v2-panels')).toHaveAttribute('data-panel', 'center');
    } else {
      await expect(starredRow(pw, 'topic-mock-planning')).toHaveAttribute('aria-current', 'true');
    }

    // A DM row opens the DM's starred-only center too.
    if (isPhone(pw)) await pw.goBack();
    await starredRow(pw, HUMAN_DM).click();
    await expect(pw).toHaveURL(/\/chat\/dm\/.+\?messages=starred$/, { timeout: 15_000 });
    await expect(pw.locator(`#msg-${msgId(HUMAN_DM, 2)}`)).toBeVisible({ timeout: 15_000 });
  });

  test('removing the last star drops the row and keeps the center open', async ({ page: pw }) => {
    await setupStarRail(pw);
    await openStarredRail(pw);
    await starredRow(pw, 'topic-mock-review').click();
    const only = msgId('topic-mock-review', 1);
    await expect(pw.locator(`#msg-${only}`)).toBeVisible({ timeout: 15_000 });
    await chooseFromMenu(pw, only, 'Unstar message');
    await expect(pw.locator('scion-chat-thread .star-filter-empty')).toContainText(
      'No starred messages in this conversation',
      { timeout: 15_000 }
    );
    await expect(pw).toHaveURL(/topic-mock-review\?messages=starred$/);
    await expect(starredRow(pw, 'topic-mock-review')).toHaveCount(0, { timeout: 15_000 });
    await expect(filterButton(pw, 'Starred')).toHaveAttribute('aria-pressed', 'true');
  });

  test('Jump to message in conversation returns the rail to All', async ({ page: pw }) => {
    await setupStarRail(pw);
    await openStarredRail(pw);
    await starredRow(pw, 'topic-mock-planning').click();
    const target = msgId('topic-mock-planning', 3);
    await expect(pw.locator(`#msg-${target}`)).toBeVisible({ timeout: 15_000 });
    await chooseFromMenu(pw, target, 'Jump to message in conversation');
    await expect(pw).toHaveURL(new RegExp(`topic-mock-planning#msg-${target}$`), {
      timeout: 15_000,
    });
    await expect(banner(pw)).toHaveCount(0);
    await expect(filterButton(pw, 'All')).toHaveAttribute('aria-pressed', 'true');
    await expect(pw.locator(`#msg-${msgId('topic-mock-planning', 1)}`)).toBeAttached({
      timeout: 15_000,
    });
  });

  test('picking Starred with a conversation open applies the filter; All clears it', async ({
    page: pw,
  }) => {
    // Desktop: the rail and the open conversation are on screen together.
    test.skip(isPhone(pw), 'the phone shows one panel at a time');
    await setupStarRail(pw);
    await pw.goto(`/chat/space/${PROJECT_A.id}/thread/topic-mock-planning`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(pw.locator(`#msg-${msgId('topic-mock-planning', 1)}`)).toBeVisible({
      timeout: 15_000,
    });
    await expect(filterButton(pw, 'Starred')).toBeVisible({ timeout: 15_000 });
    await filterButton(pw, 'Starred').click();
    await expect(pw).toHaveURL(/\?messages=starred$/, { timeout: 15_000 });
    await filterButton(pw, 'All').click();
    await expect(pw).toHaveURL(/topic-mock-planning$/, { timeout: 15_000 });
  });
});
