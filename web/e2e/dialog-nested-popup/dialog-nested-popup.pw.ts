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
 * Offline Playwright E2E regression spec for ptone/scion#8914.
 *
 * Exercises real `<sl-select>` dropdowns inside `<sl-dialog>` modals whose
 * lifecycle handlers listen to `sl-hide` (`/admin/quotas`) and `sl-after-hide`
 * (`/admin/skill-registries`). Without an `e.target === e.currentTarget` guard,
 * closing the nested `<sl-select>` popup bubbles `sl-hide` / `sl-after-hide` to
 * the enclosing `<sl-dialog>` and closes it prematurely.
 */

import { test, expect, type Locator, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

async function installMockAdminHub(page: Page): Promise<void> {
  await installEventSourceStub(page);

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'admin-1',
        email: 'admin@example.test',
        name: 'Test Admin',
        role: 'admin',
      }),
    })
  );

  await page.route('**/api/**', (route) => {
    const url = new URL(route.request().url());
    const p = url.pathname;

    if (p === '/api/v1/system/status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: '{"complete":true}',
      });
    }
    if (p === '/api/v1/settings/public') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ appName: 'Scion', onboardingComplete: true }),
      });
    }
    if (p === '/api/v1/experiments') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ experiments: {} }),
      });
    }
    if (p === '/api/v1/auth/admin-status') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          isAdmin: true,
          isSuperAdmin: true,
          permissions: ['*'],
        }),
      });
    }
    if (p === '/api/v1/admin/limits') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ limits: [] }),
      });
    }
    if (p === '/api/v1/admin/usage') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ usage: [] }),
      });
    }
    if (p === '/api/v1/skill-registries') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ registries: [] }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });
}

/**
 * Opens a Shoelace `<sl-select>`, clicks an `<sl-option>`, and waits for the
 * select's `sl-after-hide` event to finish bubbling before returning.
 */
async function selectOptionAndWaitAfterHide(
  dialog: Locator,
  select: Locator,
  optionValue: string
): Promise<void> {
  const afterHidePromise = select.evaluate(
    (el) =>
      new Promise<void>((resolve) => {
        el.addEventListener(
          'sl-after-hide',
          () => {
            requestAnimationFrame(() => resolve());
          },
          { once: true }
        );
      })
  );
  await select.click();
  await dialog.locator(`sl-option[value="${optionValue}"]`).click();
  await afterHidePromise;
}

test('admin quotas Create Limit dialog stays open when picking Resource Type sl-select (sl-hide)', async ({
  page,
}) => {
  await installMockAdminHub(page);

  await page.goto('/admin/quotas', { waitUntil: 'domcontentloaded' });
  await page.locator('sl-button:has-text("Create Limit")').first().click();

  const dialog = page.locator('sl-dialog[label="Create Limit Definition"]');
  await expect(dialog).toHaveAttribute('open', '');

  const nameInput = dialog.locator('sl-input[label="Name"] input');
  await nameInput.fill('max-widgets-per-project');

  await selectOptionAndWaitAfterHide(
    dialog,
    dialog.locator('sl-select[label="Resource Type"]'),
    'project'
  );

  await expect(dialog).toHaveAttribute('open', '');
  await expect(nameInput).toHaveValue('max-widgets-per-project');
});

test('admin skill registries Create Skill Registry dialog stays open when picking Type and Trust Level sl-selects (sl-after-hide)', async ({
  page,
}) => {
  await installMockAdminHub(page);

  await page.goto('/admin/skill-registries', { waitUntil: 'domcontentloaded' });
  await page.locator('sl-button:has-text("Create Registry")').first().click();

  const dialog = page.locator('sl-dialog[label="Create Skill Registry"]');
  await expect(dialog).toHaveAttribute('open', '');

  const nameInput = dialog.locator('sl-input[placeholder="production-hub"] input');
  await nameInput.fill('my-registry');

  const selects = dialog.locator('sl-select');
  await selectOptionAndWaitAfterHide(dialog, selects.nth(0), 'gcp');
  await expect(dialog).toHaveAttribute('open', '');
  await expect(nameInput).toHaveValue('my-registry');

  await selectOptionAndWaitAfterHide(dialog, selects.nth(1), 'trusted');
  await expect(dialog).toHaveAttribute('open', '');
  await expect(nameInput).toHaveValue('my-registry');
});
