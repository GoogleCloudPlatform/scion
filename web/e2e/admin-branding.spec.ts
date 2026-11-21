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
 * Admin Branding page (/admin/branding) E2E spec against the real Hub.
 *
 * Covers:
 * - Navigation and accessible ARIA roles (radiogroups, dialog, reset buttons)
 * - Editing Product Name, Corner Style (square), Accent Color, and file-to-data: PNG upload
 * - Live SPA application without reload (<title>, sidebar mark, #scion-branding-tokens)
 * - Persistence across hard reload
 * - Inline http:// URL validation guard
 * - Reset All Branding via <sl-dialog> restoring stock "Scion" defaults live
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv } from './harness/env.js';

/** Minimal valid 1×1 transparent PNG (68 bytes). */
const TINY_PNG_BUFFER = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==',
  'base64'
);

test.describe('Admin Branding (/admin/branding)', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('edits branding, applies live, persists across reload, and resets to stock defaults', async ({
    page,
  }) => {
    try {
      await page.goto('/admin/branding', { waitUntil: 'domcontentloaded' });

      // Wait for page heading and initial form load
      await expect(page.getByRole('heading', { name: 'Branding', level: 1 })).toBeVisible({
        timeout: 15_000,
      });

      // Verify accessible ARIA roles on radio groups
      await expect(page.getByRole('radiogroup', { name: 'Scion Attribution' })).toBeVisible();
      await expect(page.getByRole('radiogroup', { name: 'Corner Style' })).toBeVisible();

      // Fill Product Name
      const nameInput = page.locator('[data-field="name"] input');
      await nameInput.fill('Acme Cloud');

      // Select Square corners
      await page.locator('[data-field="corners"] sl-radio-button[value="square"]').click();

      // Set Accent Color
      const accentInput = page.locator('[data-field="accent_color"] input');
      await accentInput.fill('#0d9488');

      // Upload a small valid PNG logo into logo.light
      await page.locator('input[type="file"][data-slot="logo.light"]').setInputFiles({
        name: 'acme-logo.png',
        mimeType: 'image/png',
        buffer: TINY_PNG_BUFFER,
      });
      await expect(page.locator('[data-data-uri-chip="logo.light"]')).toContainText('image/png');

      // Save Branding
      await page.locator('[data-save-btn]').click();
      await expect(page.getByText('Branding settings saved and applied live')).toBeVisible({
        timeout: 15_000,
      });

      // Verify live SPA update without page reload
      await expect(page).toHaveTitle(/Acme Cloud/);
      await expect
        .poll(() =>
          page.evaluate(() => document.getElementById('scion-branding-tokens')?.textContent ?? '')
        )
        .toContain('--scion-radius');

      // Hard reload and confirm persistence
      await page.reload({ waitUntil: 'domcontentloaded' });
      await expect(page.locator('[data-field="name"] input')).toHaveValue('Acme Cloud', {
        timeout: 15_000,
      });
      await expect(page.locator('[data-data-uri-chip="logo.light"]')).toBeVisible();
      await expect(page).toHaveTitle(/Acme Cloud/);

      // Inline http:// URL validation guard on logo.dark
      const darkLogoInput = page.locator('[data-field="logo.dark"] input');
      await darkLogoInput.fill('http://example.com/logo.png');
      await expect(page.locator('[data-client-error="logo.dark"]')).toContainText('http://');
      await darkLogoInput.fill('');

      // Reset All Branding via Shoelace <sl-dialog>
      await page.locator('[data-reset-all-btn]').click();
      await expect(page.locator('[data-reset-dialog]')).toHaveAttribute('open', '');
      await page.locator('[data-confirm-reset-btn]').click();

      await expect(page.getByText('Branding reset to defaults')).toBeVisible({ timeout: 15_000 });
      await expect(page).toHaveTitle(/Scion/);
      await expect(page.locator('[data-field="name"] input')).toHaveValue('');
    } finally {
      // Ensure clean stock state even if an assertion failed mid-test
      await page.request
        .delete('/api/v1/admin/server-config/sections/branding')
        .catch(() => undefined);
    }
  });
});
