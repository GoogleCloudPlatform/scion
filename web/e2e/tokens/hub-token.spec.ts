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
 * Access tokens page: create, list and revoke a hub-boundary user access
 * token with no project selected (ptone/scion#2125).
 *
 * The token value shown once in the reveal dialog is never read, logged or
 * asserted on beyond being non-empty.
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv } from '../harness/env.js';

interface ListedToken {
  name: string;
  boundary?: { kind: string; projectId?: string };
  projectId?: string;
  purpose?: string;
  labels?: Record<string, string>;
  revoked: boolean;
}

test.describe('Access tokens: hub boundary', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('create, list and revoke a hub token with no project selected', async ({ page }) => {
    // Unique per project: the desktop and mobile projects share one hub.
    const name = `e2e-hub-${test.info().project.name}-${Date.now()}`;

    // 1. Open the page.
    await page.goto('/profile/tokens', { waitUntil: 'domcontentloaded' });
    await page.getByRole('button', { name: 'Create Token' }).first().click();

    // 2. Choose the hub boundary: the project select goes away.
    const dialog = page.locator('sl-dialog[label="Create Access Token"]');
    await expect(dialog.locator('sl-radio[value="project"]')).toBeVisible();
    await dialog.locator('sl-radio[value="hub"]').click();
    await expect(dialog.locator('sl-select[label="Project"]')).toHaveCount(0);

    // 3. Name, one scope, purpose and a label.
    await dialog.locator('sl-input[label="Name"] input').fill(name);
    await dialog.locator('sl-input[aria-label="Filter scopes"] input').fill('agent:list');
    await dialog
      .locator('.scope-checkbox-item sl-checkbox')
      .filter({ has: page.locator('.scope-checkbox-label', { hasText: /^agent:list$/ }) })
      .click();
    await dialog.locator('sl-input[label="Purpose (optional)"] input').fill('browser journey');
    await dialog.getByRole('button', { name: 'Add label' }).click();
    await dialog.locator('sl-input[aria-label="Label key"] input').fill('suite');
    await dialog.locator('sl-input[aria-label="Label value"] input').fill('e2e');

    // 4. Create: the reveal dialog shows the token once.
    await dialog.locator('sl-button[slot="footer"][variant="primary"]').click();
    const reveal = page.locator('sl-dialog[label="Token Created"]');
    await expect(reveal.locator('.token-value')).not.toBeEmpty();
    await reveal.getByRole('button', { name: 'Done' }).click();

    // 5. The list shows the token with a Hub badge, purpose and label.
    const row = page.locator('tbody tr', { hasText: name });
    await expect(row.locator('.boundary-hub')).toHaveText('Hub');
    await expect(row.locator('.token-purpose')).toHaveText('browser journey');
    await expect(row.locator('.label-chip')).toHaveText('suite=e2e');
    await expect(row.locator('.status-active')).toBeVisible();

    // 6. Revoke it.
    await row.locator('sl-icon-button[label="Revoke"]').click();
    await page.getByRole('button', { name: 'Confirm' }).click();
    await expect(row.locator('.status-revoked')).toBeVisible();

    // The server agrees: hub boundary, no project ID, revoked.
    const res = await page.request.get('/api/v1/auth/tokens');
    expect(res.ok()).toBe(true);
    const items = ((await res.json()) as { items?: ListedToken[] }).items ?? [];
    const token = items.find((t) => t.name === name);
    expect(token?.boundary).toEqual({ kind: 'hub' });
    expect(token?.projectId).toBeUndefined();
    expect(token?.purpose).toBe('browser journey');
    expect(token?.labels).toEqual({ suite: 'e2e' });
    expect(token?.revoked).toBe(true);
  });
});
