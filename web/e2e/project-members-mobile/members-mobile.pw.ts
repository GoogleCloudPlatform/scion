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

import { test, expect, type Locator } from '@playwright/test';

import {
  LONG_OTHER_NAME,
  LONG_SELF_NAME,
  OTHER_USER_ID,
  PROJECT_ID,
  SELF_USER_ID,
  setupProjectMembersHub,
} from './mock-api.js';

interface Rect {
  left: number;
  right: number;
  top: number;
  bottom: number;
  width: number;
  height: number;
}

async function rectOf(locator: Locator): Promise<Rect> {
  const box = await locator.boundingBox();
  if (!box) throw new Error(`Expected bounding box for ${locator.toString()}`);
  return {
    left: box.x,
    right: box.x + box.width,
    top: box.y,
    bottom: box.y + box.height,
    width: box.width,
    height: box.height,
  };
}

test.describe('Project Settings > Members responsive layout', () => {
  test.describe('mobile 390px viewport', () => {
    test.use({ viewport: { width: 390, height: 844 } });

    test('stacks header and keeps header buttons and all row actions unclipped', async ({
      page,
    }) => {
      await setupProjectMembersHub(page, { showVisitorsTab: true });
      await page.goto(`/projects/${PROJECT_ID}/settings`, { waitUntil: 'domcontentloaded' });

      const editor = page.locator('scion-project-members-editor');
      await expect(editor).toBeVisible({ timeout: 10_000 });
      await expect(editor.locator('tbody tr')).toHaveCount(2);

      const editorRect = await rectOf(editor);
      const headerInfo = editor.locator('.section-header-info');
      const headerActions = editor.locator('.section-header-actions');
      const description = editor.locator('.section-header-info p');
      const transferBtn = headerActions.locator('sl-button', { hasText: 'Transfer Ownership' });
      const addBtn = headerActions.locator('sl-button', { hasText: 'Add Member' });

      const infoRect = await rectOf(headerInfo);
      const actionsRect = await rectOf(headerActions);
      const descRect = await rectOf(description);
      const transferRect = await rectOf(transferBtn);
      const addRect = await rectOf(addBtn);

      // Header stacks: actions sit below title/description, and description spans full width.
      expect(actionsRect.top).toBeGreaterThanOrEqual(infoRect.bottom - 1);
      expect(descRect.width).toBeGreaterThanOrEqual(editorRect.width * 0.85);

      // Both header buttons fit within the editor and viewport without clipping.
      expect(transferRect.left).toBeGreaterThanOrEqual(editorRect.left - 1);
      expect(transferRect.right).toBeLessThanOrEqual(editorRect.right + 1);
      expect(addRect.left).toBeGreaterThanOrEqual(editorRect.left - 1);
      expect(addRect.right).toBeLessThanOrEqual(editorRect.right + 1);
      expect(addRect.right).toBeLessThanOrEqual(390);

      // Member table container and both rows keep every action visible and unclipped.
      const tableContainer = editor.locator('.table-container');
      const tableRect = await rectOf(tableContainer);
      expect(tableRect.right).toBeLessThanOrEqual(390);

      // Self row: Edit roles (pencil) + Leave button (the row shown in ptone/scion#9571).
      const selfRow = editor.locator(`tr[data-principal="user:${SELF_USER_ID}"]`);
      const selfEdit = selfRow.locator('sl-icon-button[label="Edit roles"]');
      const selfLeave = selfRow.locator('sl-button[data-self-leave]');
      await expect(selfEdit).toBeVisible();
      await expect(selfLeave).toBeVisible();
      const selfEditRect = await rectOf(selfEdit);
      const selfLeaveRect = await rectOf(selfLeave);
      expect(selfEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(390);

      // Other member row: Edit roles (pencil) + Remove member (trash).
      const otherRow = editor.locator(`tr[data-principal="user:${OTHER_USER_ID}"]`);
      const otherEdit = otherRow.locator('sl-icon-button[label="Edit roles"]');
      const otherRemove = otherRow.locator('sl-icon-button[label="Remove member"]');
      await expect(otherEdit).toBeVisible();
      await expect(otherRemove).toBeVisible();
      const otherEditRect = await rectOf(otherEdit);
      const otherRemoveRect = await rectOf(otherRemove);
      expect(otherEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(390);

      // No horizontal overflow inside the table container.
      const tableOverflow = await tableContainer.evaluate(
        (el) => el.scrollWidth > el.clientWidth + 1
      );
      expect(tableOverflow).toBe(false);
    });
  });

  test.describe('tablet 780px viewport (sidebar visible, narrow editor)', () => {
    test.use({ viewport: { width: 780, height: 800 } });

    test('uses narrow stacked layout and keeps header and all row actions unclipped', async ({
      page,
    }) => {
      await setupProjectMembersHub(page, { showVisitorsTab: true });
      await page.goto(`/projects/${PROJECT_ID}/settings`, { waitUntil: 'domcontentloaded' });

      const editor = page.locator('scion-project-members-editor');
      await expect(editor).toBeVisible({ timeout: 10_000 });
      await expect(editor.locator('tbody tr')).toHaveCount(2);

      const editorRect = await rectOf(editor);
      // Above 768px the 280px sidebar is visible, leaving the editor narrow.
      expect(editorRect.left).toBeGreaterThanOrEqual(240);
      expect(editorRect.width).toBeLessThan(520);

      const headerInfo = editor.locator('.section-header-info');
      const headerActions = editor.locator('.section-header-actions');
      const transferBtn = headerActions.locator('sl-button', { hasText: 'Transfer Ownership' });
      const addBtn = headerActions.locator('sl-button', { hasText: 'Add Member' });

      const infoRect = await rectOf(headerInfo);
      const actionsRect = await rectOf(headerActions);
      const transferRect = await rectOf(transferBtn);
      const addRect = await rectOf(addBtn);

      // Header stacks and buttons stay within the editor bounds.
      expect(actionsRect.top).toBeGreaterThanOrEqual(infoRect.bottom - 1);
      expect(transferRect.left).toBeGreaterThanOrEqual(editorRect.left - 1);
      expect(transferRect.right).toBeLessThanOrEqual(editorRect.right + 1);
      expect(addRect.left).toBeGreaterThanOrEqual(editorRect.left - 1);
      expect(addRect.right).toBeLessThanOrEqual(editorRect.right + 1);
      expect(addRect.right).toBeLessThanOrEqual(780);

      // Member table container and both rows keep every action visible and unclipped.
      const tableContainer = editor.locator('.table-container');
      const tableRect = await rectOf(tableContainer);
      expect(tableRect.right).toBeLessThanOrEqual(780);

      const selfRow = editor.locator(`tr[data-principal="user:${SELF_USER_ID}"]`);
      const selfEdit = selfRow.locator('sl-icon-button[label="Edit roles"]');
      const selfLeave = selfRow.locator('sl-button[data-self-leave]');
      await expect(selfEdit).toBeVisible();
      await expect(selfLeave).toBeVisible();
      const selfEditRect = await rectOf(selfEdit);
      const selfLeaveRect = await rectOf(selfLeave);
      expect(selfEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(780);

      const otherRow = editor.locator(`tr[data-principal="user:${OTHER_USER_ID}"]`);
      const otherEdit = otherRow.locator('sl-icon-button[label="Edit roles"]');
      const otherRemove = otherRow.locator('sl-icon-button[label="Remove member"]');
      await expect(otherEdit).toBeVisible();
      await expect(otherRemove).toBeVisible();
      const otherEditRect = await rectOf(otherEdit);
      const otherRemoveRect = await rectOf(otherRemove);
      expect(otherEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(780);

      // No horizontal overflow inside the table container.
      const tableOverflow = await tableContainer.evaluate(
        (el) => el.scrollWidth > el.clientWidth + 1
      );
      expect(tableOverflow).toBe(false);
    });
  });

  test.describe('700px viewport with long names and custom roles (no sidebar, table mode)', () => {
    test.use({ viewport: { width: 700, height: 800 } });

    for (const showVisitorsTab of [true, false]) {
      const mode = showVisitorsTab ? 'standalone (Visitors tab)' : 'compact';
      test(`keeps all row actions unclipped and full name accessible in ${mode} mode`, async ({
        page,
      }) => {
        await setupProjectMembersHub(page, { showVisitorsTab, longNames: true });
        await page.goto(`/projects/${PROJECT_ID}/settings`, { waitUntil: 'domcontentloaded' });

        const editor = page.locator('scion-project-members-editor');
        await expect(editor).toBeVisible({ timeout: 10_000 });
        await expect(editor.locator('tbody tr')).toHaveCount(2);

        const tableContainer = editor.locator('.table-container');
        const tableRect = await rectOf(tableContainer);
        expect(tableRect.right).toBeLessThanOrEqual(700);

        const selfRow = editor.locator(`tr[data-principal="user:${SELF_USER_ID}"]`);
        const selfName = selfRow.locator('.member-name');
        await expect(selfName).toHaveAttribute('title', LONG_SELF_NAME);
        await expect(selfName).toHaveText(LONG_SELF_NAME);

        const selfEdit = selfRow.locator('sl-icon-button[label="Edit roles"]');
        const selfLeave = selfRow.locator('sl-button[data-self-leave]');
        await expect(selfEdit).toBeVisible();
        await expect(selfLeave).toBeVisible();
        const selfEditRect = await rectOf(selfEdit);
        const selfLeaveRect = await rectOf(selfLeave);
        expect(selfEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
        expect(selfLeaveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
        expect(selfLeaveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
        expect(selfLeaveRect.right).toBeLessThanOrEqual(700);

        const otherRow = editor.locator(`tr[data-principal="user:${OTHER_USER_ID}"]`);
        const otherName = otherRow.locator('.member-name');
        await expect(otherName).toHaveAttribute('title', LONG_OTHER_NAME);
        await expect(otherName).toHaveText(LONG_OTHER_NAME);

        const otherEdit = otherRow.locator('sl-icon-button[label="Edit roles"]');
        const otherRemove = otherRow.locator('sl-icon-button[label="Remove member"]');
        await expect(otherEdit).toBeVisible();
        await expect(otherRemove).toBeVisible();
        const otherEditRect = await rectOf(otherEdit);
        const otherRemoveRect = await rectOf(otherRemove);
        expect(otherEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
        expect(otherRemoveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
        expect(otherRemoveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
        expect(otherRemoveRect.right).toBeLessThanOrEqual(700);

        const tableOverflow = await tableContainer.evaluate(
          (el) => el.scrollWidth > el.clientWidth + 1
        );
        expect(tableOverflow).toBe(false);
      });
    }
  });

  test.describe('960px viewport with long names and custom roles (sidebar visible, table mode)', () => {
    test.use({ viewport: { width: 960, height: 800 } });

    test('keeps all row actions unclipped and full name accessible', async ({ page }) => {
      await setupProjectMembersHub(page, { showVisitorsTab: true, longNames: true });
      await page.goto(`/projects/${PROJECT_ID}/settings`, { waitUntil: 'domcontentloaded' });

      const editor = page.locator('scion-project-members-editor');
      await expect(editor).toBeVisible({ timeout: 10_000 });
      await expect(editor.locator('tbody tr')).toHaveCount(2);

      const editorRect = await rectOf(editor);
      expect(editorRect.left).toBeGreaterThanOrEqual(240);

      const tableContainer = editor.locator('.table-container');
      const tableRect = await rectOf(tableContainer);
      expect(tableRect.right).toBeLessThanOrEqual(960);

      const selfRow = editor.locator(`tr[data-principal="user:${SELF_USER_ID}"]`);
      await expect(selfRow.locator('.member-name')).toHaveAttribute('title', LONG_SELF_NAME);
      const selfEdit = selfRow.locator('sl-icon-button[label="Edit roles"]');
      const selfLeave = selfRow.locator('sl-button[data-self-leave]');
      await expect(selfEdit).toBeVisible();
      await expect(selfLeave).toBeVisible();
      const selfEditRect = await rectOf(selfEdit);
      const selfLeaveRect = await rectOf(selfLeave);
      expect(selfEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(selfLeaveRect.right).toBeLessThanOrEqual(960);

      const otherRow = editor.locator(`tr[data-principal="user:${OTHER_USER_ID}"]`);
      await expect(otherRow.locator('.member-name')).toHaveAttribute('title', LONG_OTHER_NAME);
      const otherEdit = otherRow.locator('sl-icon-button[label="Edit roles"]');
      const otherRemove = otherRow.locator('sl-icon-button[label="Remove member"]');
      await expect(otherEdit).toBeVisible();
      await expect(otherRemove).toBeVisible();
      const otherEditRect = await rectOf(otherEdit);
      const otherRemoveRect = await rectOf(otherRemove);
      expect(otherEditRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.left).toBeGreaterThanOrEqual(tableRect.left - 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(tableRect.right + 1);
      expect(otherRemoveRect.right).toBeLessThanOrEqual(960);

      const tableOverflow = await tableContainer.evaluate(
        (el) => el.scrollWidth > el.clientWidth + 1
      );
      expect(tableOverflow).toBe(false);
    });
  });

  test.describe('desktop 1100px viewport', () => {
    test.use({ viewport: { width: 1100, height: 800 } });

    test('keeps desktop side-by-side header and single-row table layout unchanged', async ({
      page,
    }) => {
      await setupProjectMembersHub(page, { showVisitorsTab: true });
      await page.goto(`/projects/${PROJECT_ID}/settings`, { waitUntil: 'domcontentloaded' });

      const editor = page.locator('scion-project-members-editor');
      await expect(editor).toBeVisible({ timeout: 10_000 });
      await expect(editor.locator('tbody tr')).toHaveCount(2);

      // Header stays horizontal: actions sit to the right of info, not stacked below.
      const infoRect = await rectOf(editor.locator('.section-header-info'));
      const actionsRect = await rectOf(editor.locator('.section-header-actions'));
      expect(actionsRect.left).toBeGreaterThanOrEqual(infoRect.right - 1);
      expect(actionsRect.top).toBeLessThan(infoRect.bottom);

      // Table header remains visible and cells stay side-by-side on one row.
      await expect(editor.locator('thead')).toBeVisible();
      const selfRow = editor.locator(`tr[data-principal="user:${SELF_USER_ID}"]`);
      const memberCellRect = await rectOf(selfRow.locator('td').nth(0));
      const rolesCellRect = await rectOf(selfRow.locator('td').nth(1));
      const actionsCellRect = await rectOf(selfRow.locator('td.actions-cell'));
      expect(rolesCellRect.left).toBeGreaterThanOrEqual(memberCellRect.right - 1);
      expect(actionsCellRect.left).toBeGreaterThanOrEqual(rolesCellRect.right - 1);
    });
  });
});
