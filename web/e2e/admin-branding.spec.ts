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

      // Verify accessible ARIA roles and names on controls
      await expect(page.getByRole('radiogroup', { name: 'Scion Attribution' })).toBeVisible();
      await expect(page.getByRole('radiogroup', { name: 'Corner Style' })).toBeVisible();
      await expect(page.getByRole('combobox', { name: 'Sans-Serif Font' })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Upload Light Logo' })).toBeVisible();

      // Fill Product Name by accessible name
      const nameInput = page.getByRole('textbox', { name: 'Product Name' });
      await nameInput.fill('Acme Cloud');

      // Select Square corners
      await page.locator('[data-field="corners"] sl-radio-button[value="square"]').click();

      // Set Accent Color by accessible name
      const accentInput = page.getByRole('textbox', { name: 'Accent Color', exact: true });
      await accentInput.fill('#0d9488');

      // Verify scoped Light & Dark live preview and WCAG AA contrast badges update BEFORE saving,
      // without mutating live Hub branding tokens on document.documentElement.
      await expect(page.locator('[data-preview-pane="light"]')).toContainText('Acme Cloud');
      await expect(page.locator('[data-preview-pane="dark"]')).toContainText('Acme Cloud');
      await expect(page.locator('[data-preview-pane="light"]')).toContainText('Powered by Scion');
      await expect(page.locator('[data-preview-contrast-summary="pass"]')).toContainText(
        'WCAG AA Pass'
      );
      await expect(page.locator('[data-ramp-step]')).toHaveCount(11);
      await expect(page.locator('[data-contrast-pair="light.primary:surface"]')).toContainText(
        'AA Pass'
      );
      await expect(page.locator('[data-contrast-pair="dark.primary:surface"]')).toContainText(
        'AA Pass'
      );
      expect(
        await page.evaluate(() => document.getElementById('scion-branding-tokens'))
      ).toBeNull();

      // Upload a small valid PNG logo into logo.light
      await page.locator('input[type="file"][data-slot="logo.light"]').setInputFiles({
        name: 'acme-logo.png',
        mimeType: 'image/png',
        buffer: TINY_PNG_BUFFER,
      });
      await expect(page.locator('[data-data-uri-chip="logo.light"]')).toContainText('image/png');

      // Save Branding
      await page.getByRole('button', { name: 'Save Branding' }).click();
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
      await expect(page.getByRole('textbox', { name: 'Product Name' })).toHaveValue('Acme Cloud', {
        timeout: 15_000,
      });
      await expect(page.locator('[data-data-uri-chip="logo.light"]')).toBeVisible();
      await expect(page).toHaveTitle(/Acme Cloud/);

      // Inline http:// URL validation guard on Dark Logo (selected by accessible name)
      const darkLogoInput = page.getByRole('textbox', { name: 'Dark Logo' });
      await darkLogoInput.fill('http://example.com/logo.png');
      await expect(page.locator('[data-client-error="logo.dark"]')).toContainText('http://');
      await darkLogoInput.fill('');

      // Expand Advanced Palette Overrides and test WCAG AA 422 auto-expand + semantic & scale overrides
      const paletteDetails = page.locator('[data-advanced-palette-details]');
      const paletteToggle = page.getByRole('button', { name: /Advanced Palette Overrides/ });
      await paletteToggle.click();
      await expect(paletteDetails).toHaveAttribute('open', '');

      const lightPrimaryInput = page.getByRole('textbox', { name: 'Light Primary', exact: true });
      await expect(lightPrimaryInput).toBeVisible();

      // Failing contrast override (#eab308 on white) shows live AA Fail badge and triggers server 422
      await lightPrimaryInput.fill('#eab308');
      await expect(page.locator('[data-token-contrast="light.primary"]')).toContainText('AA Fail');

      // Collapse panel, click Save, verify 422 auto-expands the Advanced Palette panel with inline error
      await paletteToggle.click();
      await expect(paletteDetails).not.toHaveAttribute('open', '');
      await page.getByRole('button', { name: 'Save Branding' }).click();
      await expect(paletteDetails).toHaveAttribute('open', '', { timeout: 15_000 });
      await expect(
        page.locator('[data-field-error="palette.light.primary"]').first()
      ).toContainText('4.50:1');

      // Fix Light Primary to a WCAG AA passing color and save
      await lightPrimaryInput.fill('#0f766e');
      await expect(page.locator('[data-token-contrast="light.primary"]')).toContainText('AA Pass');
      await page.getByRole('button', { name: 'Save Branding' }).click();
      await expect(page.getByText('Branding settings saved and applied live')).toBeVisible({
        timeout: 15_000,
      });

      // Verify per-key reset on Light Primary and Primary Scale override + Clear overrides
      await page.getByRole('button', { name: 'Reset Light Primary to default' }).click();
      await expect(lightPrimaryInput).toHaveValue('');

      const primaryScaleDetails = page.locator('[data-scale-details="primary"]');
      await page.getByRole('button', { name: /Primary Scale/ }).click();
      await expect(primaryScaleDetails).toHaveAttribute('open', '');

      // Verify zero live status regions inside the palette editor (no screen-reader chatter)
      await expect(page.locator('scion-branding-palette-editor').getByRole('status')).toHaveCount(
        0
      );

      const primary600Input = page.getByRole('textbox', { name: 'Primary 600', exact: true });
      await primary600Input.fill('#007268');
      await page.getByRole('button', { name: 'Save Branding' }).click();
      await expect(page.getByText('Branding settings saved and applied live')).toBeVisible({
        timeout: 15_000,
      });

      await page.getByRole('button', { name: 'Clear Primary overrides' }).click();
      await expect(primary600Input).toHaveValue('');
      await page.getByRole('button', { name: 'Save Branding' }).click();
      await expect(page.getByText('Branding settings saved and applied live')).toBeVisible({
        timeout: 15_000,
      });

      // Confirm cleared scale persisted on server after reload
      await page.reload({ waitUntil: 'domcontentloaded' });
      await expect(page.getByRole('heading', { name: 'Branding', level: 1 })).toBeVisible({
        timeout: 15_000,
      });
      await expect(page.locator('[data-palette-override-count]')).toHaveCount(0);

      // Reset All Branding via Shoelace <sl-dialog>
      await page.getByRole('button', { name: 'Reset All Branding' }).click();
      await expect(page.getByRole('dialog', { name: 'Reset All Branding' })).toBeVisible();
      await page.getByRole('button', { name: 'Reset Branding' }).click();

      await expect(page.getByText('Branding reset to defaults')).toBeVisible({ timeout: 15_000 });
      await expect(page).not.toHaveTitle(/Acme Cloud/);
      await expect(page).toHaveTitle(/Scion/);
      await expect
        .poll(() => page.evaluate(() => document.getElementById('scion-branding-tokens')))
        .toBeNull();
      await expect(page.getByRole('textbox', { name: 'Product Name' })).toHaveValue('');
    } finally {
      // Ensure clean stock state even if an assertion failed mid-test
      await page.request
        .delete('/api/v1/admin/server-config/sections/branding')
        .catch(() => undefined);
    }
  });
});
