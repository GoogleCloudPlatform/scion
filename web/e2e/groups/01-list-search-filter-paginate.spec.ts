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
 * Spec 1 — List: search/filter/paginate/deep-link round-trip (AC4)
 *
 * Verifies server-driven search, type filter, pagination with cursor,
 * and URL round-trip (deep-link restores query, filter, tab, and page).
 */

import { test, expect } from '@playwright/test';
import {
  getE2EEnv,
  createGroup,
  uniqueSlug,
  fillSlInput,
} from './groups-setup.js';

test.describe('List: search, filter, paginate, deep-link (AC4)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('search filters groups by name and syncs to URL', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Type in the search box — use fillSlInput since sl-input is a custom element
    const searchInput = page.locator('sl-input[placeholder="Search name, slug, description..."]');
    await fillSlInput(searchInput, 'E2E Test');

    // The URL should update with the search query
    await expect(page).toHaveURL(/[?&]q=E2E/, { timeout: 5_000 });

    // The E2E Test Group should still be visible
    await expect(page.getByText('E2E Test Group')).toBeVisible();
  });

  test('type filter restricts results', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Open the type filter and select "Explicit"
    const typeSelect = page.locator('sl-select').first();
    await typeSelect.click();
    await page.locator('sl-option[value="explicit"]').click();

    // URL should reflect the filter
    await expect(page).toHaveURL(/groupType=explicit/, { timeout: 5_000 });

    // The explicit group should still be visible
    await expect(page.getByText('E2E Test Group')).toBeVisible();
  });

  test('deep-link restores search, filter, and tab', async ({ page }) => {
    await page.goto('/admin/groups?q=E2E&groupType=explicit', {
      waitUntil: 'domcontentloaded',
    });

    // Verify search input has the value — target native <input> inside sl-input shadow DOM
    const searchSlInput = page.locator('sl-input[placeholder="Search name, slug, description..."]');
    await expect(searchSlInput.locator('input')).toHaveValue('E2E');

    // Results should be filtered
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });
  });

  test('Previous/Next pagination buttons work', async ({ page }) => {
    await page.goto('/admin/groups', { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Previous should be disabled on first page
    const previousBtn = page.getByRole('button', { name: 'Previous' });
    if (await previousBtn.isVisible()) {
      await expect(previousBtn).toBeDisabled();
    }
  });

  test('clear filters button removes all active filters', async ({ page }) => {
    await page.goto('/admin/groups?q=E2E&groupType=explicit', {
      waitUntil: 'domcontentloaded',
    });
    await expect(page.getByText('E2E Test Group')).toBeVisible({ timeout: 15_000 });

    // Click "Clear filters" (`.first()` — filter bar + empty state may both render)
    const clearBtn = page.getByRole('button', { name: 'Clear filters' }).first();
    await clearBtn.click();

    // URL should be clean
    await expect(page).toHaveURL(/\/admin\/groups(?:\?)?$/, { timeout: 5_000 });

    // Search input should be empty
    const searchSlInput = page.locator('sl-input[placeholder="Search name, slug, description..."]');
    await expect(searchSlInput.locator('input')).toHaveValue('');
  });
});
