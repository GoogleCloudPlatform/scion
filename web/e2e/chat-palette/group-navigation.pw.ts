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
 * Real-Chromium coverage for the multi-group palette's keyboard model: one
 * global best is selected across Agents/Threads/People; Tab/Shift+Tab cycle
 * in reading order and skip empties without escaping the Shoelace modal;
 * Up/Down wrap within group, Left/Right edit the input; the active option is
 * visible after expanding past ten rows; asynchronous result refresh
 * preserves manual selection by ID.
 */

import { test, expect, type Page } from '@playwright/test';
import {
  setupApiMocks,
  AGENT_WITH_DM,
  USER_WITH_DM,
  USER_WITHOUT_DM,
  USER_SUSPENDED,
  USER_SORTS_BEFORE_SELF,
  SELF_USER_ID,
  SPACE_ALPHA,
  THREAD_ALPHA,
  SPACE_BETA,
  THREAD_BETA,
} from './mock-api.js';

const THREE_GROUP_FIXTURE = {
  spaces: [SPACE_ALPHA, SPACE_BETA],
  threadsByProjectId: {
    [SPACE_ALPHA.projectId]: [THREAD_ALPHA],
    [SPACE_BETA.projectId]: [THREAD_BETA],
  },
  users: [USER_WITH_DM, USER_WITHOUT_DM],
};

async function gotoChat(
  page: Page,
  overrides: Parameters<typeof setupApiMocks>[1] = THREE_GROUP_FIXTURE
) {
  const requests = await setupApiMocks(page, overrides);
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  return requests;
}

function paletteInput(page: Page) {
  return page.locator('scion-quick-palette #palette-query-input');
}

function paletteDialog(page: Page) {
  return page.locator('scion-quick-palette sl-dialog[label="Quick switcher"]');
}

/** Deep-query into the real composer's native textarea, through the page, thread, composer and sl-textarea shadow roots. */
function composerTextarea(page: Page) {
  return page
    .locator('scion-page-chat')
    .locator('scion-chat-thread')
    .locator('scion-chat-composer')
    .locator('sl-textarea')
    .locator('textarea');
}

function activeOption(page: Page) {
  return page.locator('scion-quick-palette .palette-option.active');
}

function groupHeading(page: Page, group: string) {
  return page.locator(`scion-quick-palette #palette-heading-${group}`);
}

function groupOptions(page: Page, group: string) {
  return page.locator(
    `scion-quick-palette [aria-labelledby="palette-heading-${group}"] .palette-option`
  );
}

/**
 * Open the palette and wait for *every* group's real async load to finish
 * (not just whichever one happens to produce the current global best) before
 * returning control to the test. Threads and People fetch independently of
 * Agents, so "Agents has an active row" (Agents is the fixture's only group
 * with a real DM activity timestamp, so it finishes first) says nothing
 * about whether Threads/People have rendered their rows yet — waiting for
 * each group's real row count is what actually makes Tab's destination
 * deterministic, rather than relying on incidental timing.
 */
async function openPaletteReady(
  page: Page,
  expected: { threads: number; people: number }
): Promise<void> {
  await page.keyboard.press('Control+k');
  await expect(groupOptions(page, 'agents')).toHaveCount(2);
  await expect(groupOptions(page, 'threads')).toHaveCount(expected.threads);
  await expect(groupOptions(page, 'people')).toHaveCount(expected.people);
}

test('no row is selected on open across Agents/Threads/People until the user picks one', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteReady(page, { threads: 2, people: 2 });
  await expect(paletteInput(page)).toBeFocused();
  // Empty query: nothing is active, so the input points at no option.
  await expect(page.locator('scion-quick-palette .palette-option.active')).toHaveCount(0);
  await expect(paletteInput(page)).not.toHaveAttribute('aria-activedescendant', /.+/);
  // Recency order: AGENT_WITH_DM's DM has the only real activity timestamp
  // in the fixture (2026-09-28T12:00:00Z); everything else defaults to
  // activityMs=0, so the first ArrowDown picks it as the single active row.
  await page.keyboard.press('ArrowDown');
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);
  // Every group actually rendered — this fixture isn't accidentally still
  // Agents-only.
  await expect(groupHeading(page, 'agents')).toBeVisible();
  await expect(groupHeading(page, 'threads')).toBeVisible();
  await expect(groupHeading(page, 'people')).toBeVisible();
});

test('Tab/Shift+Tab cycle Agents -> Threads -> People in reading order, skip empties, and never escape the dialog', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteReady(page, { threads: 2, people: 2 });
  // With nothing active, the first Tab picks the first group, Agents.
  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);

  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);
  // Focus never left the query input — a Shoelace Tab trap escape would move
  // it to the dialog's close button or another focusable element instead.
  await expect(paletteInput(page)).toBeFocused();

  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(USER_WITH_DM.displayName);
  await expect(paletteInput(page)).toBeFocused();

  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);

  // Shift+Tab reverses: Agents -> People -> Threads -> Agents.
  await page.keyboard.press('Shift+Tab');
  await expect(activeOption(page)).toContainText(USER_WITH_DM.displayName);
  await page.keyboard.press('Shift+Tab');
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);
});

test('Tab skips groups with zero matches for the current query, wrapping back to the only populated one', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteReady(page, { threads: 2, people: 2 });
  // "Coder" matches only AGENT_WITH_DM ("Coder One") — General/Planning
  // (Threads) and the People fixtures have no match at all.
  await paletteInput(page).fill('Coder');
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);
  await page.keyboard.press('Tab');
  // Threads and People are both empty for this query, so Tab wraps back to
  // Agents — the only populated group — instead of landing on either.
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);
});

test('Up/Down wrap within the active group only; Left/Right keep editing the query text', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteReady(page, { threads: 2, people: 2 });
  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);
  // Threads has two rows (General/THREAD_ALPHA, Planning/THREAD_BETA), tied
  // at activityMs=0 — Tab lands on the group's best-ranked (label-ascending)
  // row, "General".
  await page.keyboard.press('Tab');
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);

  await page.keyboard.press('ArrowDown');
  await expect(activeOption(page)).toContainText(THREAD_BETA.name);

  // Wraps back to General, never crossing into People.
  await page.keyboard.press('ArrowDown');
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);

  await paletteInput(page).fill('ab');
  await page.keyboard.press('Home');
  await page.keyboard.type('x');
  await expect(paletteInput(page)).toHaveValue('xab');
  await page.keyboard.press('ArrowRight');
  await page.keyboard.type('y');
  await expect(paletteInput(page)).toHaveValue('xayb');
});

test('a suspended user is excluded from the real People group', async ({ page }) => {
  await gotoChat(page, {
    ...THREE_GROUP_FIXTURE,
    users: [USER_WITH_DM, USER_WITHOUT_DM, USER_SUSPENDED],
  });
  await openPaletteReady(page, { threads: 2, people: 2 });
  await paletteInput(page).fill(USER_SUSPENDED.displayName);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(0);
  await expect(
    page.locator('scion-quick-palette [aria-labelledby="palette-heading-people"] .palette-empty')
  ).toBeVisible();
});

test('selecting a person opens the exact sorted-user DM key, for both ID orderings', async ({
  page,
}) => {
  // Peer ID sorts *after* self ("self-user" < "user-with-dm").
  await gotoChat(page, {
    ...THREE_GROUP_FIXTURE,
    users: [USER_WITH_DM, USER_WITHOUT_DM, USER_SORTS_BEFORE_SELF],
  });
  await openPaletteReady(page, { threads: 2, people: 3 });
  await paletteInput(page).fill(USER_WITH_DM.displayName);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(`/chat/dm/${encodeURIComponent(`dm:user:${SELF_USER_ID}:user:${USER_WITH_DM.id}`)}$`)
  );

  // Peer ID sorts *before* self ("a-user-early" < "self-user") — a fresh
  // palette open (the same page, so this also proves the group's own
  // recency-based fixture data still resolves the peer correctly after a
  // prior selection). Wait for the first selection's close animation to
  // actually finish before reopening: composer focus only happens in
  // sl-after-hide, so waiting for the dialog to be hidden and the composer
  // to be focused is the reliable signal that the close has completed,
  // rather than racing a second Control+k against it.
  await expect(paletteDialog(page)).toBeHidden();
  await expect(composerTextarea(page)).toBeFocused();
  await page.keyboard.press('Control+k');
  await expect(paletteInput(page)).toBeFocused();
  await expect(groupOptions(page, 'people')).toHaveCount(3);
  await paletteInput(page).fill(USER_SORTS_BEFORE_SELF.displayName);
  await expect(page.locator('scion-quick-palette .palette-option')).toHaveCount(1);
  await page.keyboard.press('Enter');
  await expect(page).toHaveURL(
    new RegExp(
      `/chat/dm/${encodeURIComponent(`dm:user:${USER_SORTS_BEFORE_SELF.id}:user:${SELF_USER_ID}`)}$`
    )
  );
});

test('the active option is visible (in the scrollable viewport) after expanding past ten rows', async ({
  page,
}) => {
  const manyThreads = Array.from({ length: 12 }, (_, i) => ({
    id: `thread-${i}`,
    projectId: SPACE_ALPHA.projectId,
    name: `Topic ${String(i).padStart(2, '0')}`,
  }));
  await gotoChat(page, {
    spaces: [SPACE_ALPHA],
    threadsByProjectId: { [SPACE_ALPHA.projectId]: manyThreads },
  });
  // Scoped to the Threads group specifically — setupApiMocks always serves
  // its fixed 2-viable-agent fixture regardless of overrides, so Agents is
  // the first group and the first Tab picks it. Waiting for the Threads
  // group's own capped count (10, since 12 > the visible limit) is the
  // actual readiness signal here.
  await page.keyboard.press('Control+k');
  const threadOptions = groupOptions(page, 'threads');
  await expect(threadOptions).toHaveCount(10);
  await expect(groupOptions(page, 'agents')).toHaveCount(2);
  await page.keyboard.press('Tab');
  await expect(page.locator('scion-quick-palette .palette-option.active')).toBeVisible();
  await page.keyboard.press('Tab');
  await expect(page.locator('scion-quick-palette .palette-show-more')).toBeVisible();

  // All threads share activityMs=0 (tied), so ranking falls back to label
  // order: "Topic 00".."Topic 11" ascending — Tab landed on the group's
  // first row ("Topic 00"); ArrowUp wraps to the *last* (12th, index 11)
  // row, beyond the cap.
  await expect(activeOption(page)).toContainText('Topic 00');
  await page.keyboard.press('ArrowUp');
  await expect(threadOptions).toHaveCount(12);
  await expect(activeOption(page)).toContainText('Topic 11');
  // toBeVisible only checks CSS visibility, not whether the element is
  // actually scrolled into the viewport — toBeInViewport is what actually
  // discriminates a missing scrollIntoView() call.
  await expect(activeOption(page)).toBeInViewport({ ratio: 1 });
});

test('an asynchronous group refresh preserves a manually-selected row that the refresh itself pushes past the 10-row cap', async ({
  page,
}) => {
  // 9 pre-existing threads plus the target ("Target Thread", initially
  // ranked #10 — the *last visible* row, never expanded) in one project, so
  // selecting it does not itself trigger ensureGroupExpandedFor. The refresh
  // then inserts one higher-ranked thread ahead of it, pushing it to index
  // 10 (the 11th row, past the cap) — only a *second*, refresh-time call to
  // ensureGroupExpandedFor (quick-palette.ts's moveActive/reconcile path)
  // keeps it mounted and in the viewport.
  const initialThreads = [
    ...Array.from({ length: 9 }, (_, i) => ({
      id: `filler-${i}`,
      projectId: SPACE_ALPHA.projectId,
      name: `Filler ${String(i).padStart(2, '0')}`,
    })),
    { id: 'target-thread', projectId: SPACE_ALPHA.projectId, name: 'Target Thread' },
  ];
  await gotoChat(page, {
    spaces: [SPACE_ALPHA],
    threadsByProjectId: { [SPACE_ALPHA.projectId]: initialThreads },
  });
  await page.keyboard.press('Control+k');
  const threadOptions = groupOptions(page, 'threads');
  await expect(threadOptions).toHaveCount(10);
  await expect(groupOptions(page, 'agents')).toHaveCount(2);
  // The first Tab picks Agents, the second Threads.
  await page.keyboard.press('Tab');
  await page.keyboard.press('Tab');
  // Navigate to the last (10th) row without ever exceeding the cap yet.
  await page.keyboard.press('ArrowUp');
  await expect(activeOption(page)).toContainText('Target Thread');
  await expect(activeOption(page)).toBeInViewport({ ratio: 1 });

  // Simulate the page's own SSE-driven dirty-marking + 500ms debounced
  // refresh by invoking the real private reload path directly at the page
  // level, after changing the underlying fixture data so the
  // refreshed candidate list ranks "Target Thread" one row further down
  // (11th, index 10 — past the cap for the first time).
  await page.route('**/api/v1/chat/spaces/*/threads', (route) =>
    route.fulfill({
      json: {
        threads: [
          { id: 'new-top', projectId: SPACE_ALPHA.projectId, name: 'AAA New Top' },
          ...initialThreads,
        ],
      },
    })
  );
  await page.evaluate(() => {
    const el = document.querySelector('scion-page-chat') as unknown as {
      _loadPaletteThreads: () => Promise<void>;
    };
    void el._loadPaletteThreads();
  });

  await expect(threadOptions).toHaveCount(11);
  await expect(activeOption(page)).toContainText('Target Thread');
  await expect(activeOption(page)).toBeInViewport({ ratio: 1 });
});

test('an asynchronous group refresh preserves the manually-selected row by ID', async ({
  page,
}) => {
  await gotoChat(page);
  await openPaletteReady(page, { threads: 2, people: 2 });
  await page.keyboard.press('Tab'); // picks Agents' top row
  await expect(activeOption(page)).toContainText(AGENT_WITH_DM.name);
  await page.keyboard.press('Tab'); // manually select Threads' best-ranked row, General
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);

  // Simulate the page's own SSE-driven dirty-marking + 500ms debounced
  // refresh by invoking the real private retry path directly at the page
  // level — this exercises chat.ts's own reload-and-reconcile
  // machinery, not a synthetic component-level prop swap.
  await page.evaluate(() => {
    const el = document.querySelector('scion-page-chat') as unknown as {
      _loadPaletteThreads: () => Promise<void>;
    };
    void el._loadPaletteThreads();
  });

  // The refresh replaces the candidates array (new object identity) but the
  // same thread ID reappears — the manual selection must survive by ID.
  await expect(activeOption(page)).toContainText(THREAD_ALPHA.name);
});
