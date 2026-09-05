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
 * Spec 10 — Cycle: A∈B, add B to A → inline cycle copy (AC11)
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  addGroupMember,
  uniqueSlug,
} from './groups-setup.js';

test.describe('Cycle detection (AC11)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  let groupAId: string;
  let groupBId: string;

  test.beforeAll(async () => {
    // Create two groups: A and B
    const groupA = await createGroup(env.baseURL, env.devToken, {
      name: 'Cycle Group A',
      slug: uniqueSlug('cycle-a'),
    });
    groupAId = groupA.id;

    const groupB = await createGroup(env.baseURL, env.devToken, {
      name: 'Cycle Group B',
      slug: uniqueSlug('cycle-b'),
    });
    groupBId = groupB.id;

    // Add A as a member of B (so A ∈ B)
    await addGroupMember(env.baseURL, env.devToken, groupBId, {
      memberType: 'group',
      memberId: groupAId,
      role: 'member',
    });
  });

  test('adding B to A shows cycle error inline, dialog stays open', async ({ page }) => {
    // Navigate to group A's detail page
    await page.goto(`/admin/groups/${groupAId}`, {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByRole('heading', { name: 'Cycle Group A' })).toBeVisible({
      timeout: 15_000,
    });

    // Open Add Member dialog
    await page.getByRole('button', { name: 'Add Member' }).click();

    const dialog = page.locator('sl-dialog[label="Add Member"]');
    await expect(dialog).toBeVisible({ timeout: 5_000 });

    // Switch to "Group" member type
    const typeSelect = dialog.locator('sl-select[label="Member Type"]');
    await typeSelect.click();
    await page.locator('sl-option[value="group"]').click();

    // Select group B — use the principal picker's search input
    const picker = dialog.locator('scion-principal-picker');
    await picker.waitFor({ state: 'visible', timeout: 5_000 });

    // The principal picker renders an sl-input for searching; type into it
    const pickerInput = picker.locator('sl-input').first();
    await pickerInput.evaluate((el: any, val: string) => {
      el.value = val;
      el.dispatchEvent(new Event('sl-input', { bubbles: true }));
    }, 'Cycle Group B');

    // Wait for search results dropdown and click the matching option
    const pickerOption = page.getByText('Cycle Group B').last();
    await pickerOption.click({ timeout: 10_000 }).catch(async () => {
      // Fallback: try filling the group ID directly via principal-change event
      await picker.evaluate((el: any, id: string) => {
        el.dispatchEvent(new CustomEvent('principal-change', {
          bubbles: true, composed: true,
          detail: { principalType: 'group', principalId: id, displayLabel: 'Cycle Group B' },
        }));
      }, groupBId);
    });

    // Submit
    const addBtn = dialog.getByRole('button', { name: 'Add Member' });
    await addBtn.click();

    // Should see cycle error inline
    const cycleError = dialog.getByText(/cycle/i);
    await expect(cycleError).toBeVisible({ timeout: 10_000 });

    // Dialog should stay open
    await expect(dialog).toBeVisible();
  });
});
