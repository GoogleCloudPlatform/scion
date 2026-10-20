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
 * E1: the Brokers area gate in a real browser (broker UI Option A, slice
 * A1). With hub.flat_runtime_brokers pinned off, /brokers and
 * /brokers/{id} render the existing pages and no Brokers area element;
 * pinned on, they render the Brokers area and no existing page element.
 */

import { test, expect } from '@playwright/test';

import { BROKER_ID, setupHub } from './mock-api.js';

const OLD_LIST = 'scion-page-brokers';
const OLD_DETAIL = 'scion-page-broker-detail';
const NEW_LIST = 'scion-page-brokers-area';
const NEW_DETAIL = 'scion-page-broker-area-detail';

test.describe('hub.flat_runtime_brokers off', () => {
  test('/brokers renders the existing list page', async ({ page }) => {
    await setupHub(page, { flatBrokers: false });
    await page.goto('/brokers', { waitUntil: 'domcontentloaded' });
    await expect(page.locator(OLD_LIST)).toHaveCount(1);
    expect(await page.locator(NEW_LIST).count()).toBe(0);
  });

  test('/brokers/{id} renders the existing detail page', async ({ page }) => {
    await setupHub(page, { flatBrokers: false });
    await page.goto(`/brokers/${BROKER_ID}`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator(OLD_DETAIL)).toHaveCount(1);
    expect(await page.locator(NEW_DETAIL).count()).toBe(0);
  });
});

test.describe('hub.flat_runtime_brokers on', () => {
  test('/brokers renders the Brokers area list', async ({ page }) => {
    await setupHub(page, { flatBrokers: true });
    await page.goto('/brokers', { waitUntil: 'domcontentloaded' });
    await expect(page.locator(NEW_LIST)).toHaveCount(1);
    await expect(page.locator(`${NEW_LIST} tr.broker-row`)).toHaveCount(1);
    expect(await page.locator(OLD_LIST).count()).toBe(0);
  });

  test('/brokers/{id} renders the Brokers area detail', async ({ page }) => {
    await setupHub(page, { flatBrokers: true });
    await page.goto(`/brokers/${BROKER_ID}`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator(NEW_DETAIL)).toHaveCount(1);
    await expect(page.locator(`${NEW_DETAIL} sl-tab[panel="overview"]`)).toHaveCount(1);
    expect(await page.locator(OLD_DETAIL).count()).toBe(0);
  });

  test('a deep link to ?tab=health opens the Health tab after health loads', async ({ page }) => {
    await setupHub(page, { flatBrokers: true, canReadHealth: true });
    await page.goto(`/brokers/${BROKER_ID}?tab=health`, { waitUntil: 'domcontentloaded' });
    await expect(page.locator(`${NEW_DETAIL} sl-tab[panel="health"]`)).toHaveCount(1);
    await expect(page.locator(`${NEW_DETAIL} sl-tab[panel="health"][active]`)).toHaveCount(1);
  });
});
