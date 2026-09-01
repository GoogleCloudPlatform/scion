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
 * Playwright global setup.
 *
 * 1. Builds web assets and the Go hub binary.
 * 2. Starts the hub on an ephemeral port with dev-auth + test-login enabled.
 * 3. Waits for the health endpoint.
 * 4. Seeds test data using the dev-auth token (super-admin).
 * 5. Creates an admin browser session by navigating (dev-auth auto-login).
 * 6. Writes env state for test specs.
 */

import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import { chromium } from '@playwright/test';
import { startHub } from './hub.js';
import { seedSmokeData } from './seed.js';

/** File where global-setup writes state for tests to read. */
const E2E_ENV_FILE = path.join(os.tmpdir(), 'scion-e2e-env.json');

export interface E2EEnv {
  baseURL: string;
  adminStorageState: string;
  /** Dev-auth Bearer token for direct API calls (super-admin). */
  devToken: string;
  seedData: {
    groups: Array<{ id: string; name: string; slug: string }>;
  };
}

async function globalSetup(): Promise<void> {
  console.log('\n=== E2E Global Setup ===\n');

  // 1. Build and start the hub
  const hubState = await startHub();
  const { baseURL, devToken } = hubState;

  // Expose baseURL for Playwright config
  process.env.E2E_BASE_URL = baseURL;

  // 2. Seed smoke test data using the dev-auth token (super-admin)
  console.log('\n[setup] Seeding smoke test data...');
  const seedData = await seedSmokeData(baseURL, devToken);
  console.log('[setup] Seed data ready.');

  // 3. Create admin browser session via dev-auth auto-login
  //    In dev-auth mode, navigating to any page auto-creates a session cookie.
  console.log('\n[setup] Creating admin browser session...');
  const storageDir = path.join(os.tmpdir(), 'scion-e2e-auth');
  fs.mkdirSync(storageDir, { recursive: true });
  const adminStorageState = path.join(storageDir, 'admin.json');

  const browser = await chromium.launch();
  const context = await browser.newContext();
  const page = await context.newPage();

  // Navigate to any page — dev-auth middleware sets the session cookie
  await page.goto(`${baseURL}/admin/groups`, {
    waitUntil: 'domcontentloaded',
    timeout: 15_000,
  });

  // Save the session cookies as storageState
  await context.storageState({ path: adminStorageState });
  await browser.close();
  console.log('[setup] Admin browser session saved.');

  // 4. Write env file for tests
  const env: E2EEnv = {
    baseURL,
    adminStorageState,
    devToken,
    seedData: {
      groups: seedData.groups.map((g) => ({
        id: g.id,
        name: g.name,
        slug: g.slug,
      })),
    },
  };
  fs.writeFileSync(E2E_ENV_FILE, JSON.stringify(env, null, 2));

  console.log('\n=== E2E Global Setup Complete ===\n');
  console.log(`  Hub:     ${baseURL}`);
  console.log(`  Groups:  ${seedData.groups.map((g) => g.name).join(', ')}`);
  console.log('');
}

export default globalSetup;
