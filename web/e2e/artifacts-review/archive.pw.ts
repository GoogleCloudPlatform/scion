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
 * Download all (.zip) on the Files tab (ptone/scion#7106): the browser
 * saves one zip whose entries are the version's files at their paths,
 * byte for byte.
 */

import { readFile } from 'node:fs/promises';

import { expect, test } from '@playwright/test';

import {
  DOT_PNG,
  createProject,
  openArtifact,
  publish,
  readZip,
  setExperiments,
} from './fixtures.js';

let project = '';

test.describe.configure({ mode: 'serial' });

test.beforeAll(async () => {
  await setExperiments(true);
  project = await createProject(`e2e-archive-${Date.now().toString(36)}`);
});

test.afterAll(async () => {
  await setExperiments(null);
});

test('Download all saves a zip of the version files', async ({ page }) => {
  const files: Record<string, Buffer> = {
    'index.md': Buffer.from('# Bundle\n\nSee the [notes](docs/notes.txt).\n'),
    'docs/notes.txt': Buffer.from('plain notes\n'.repeat(50)),
    'img/dot.png': DOT_PNG,
  };
  const a = await publish({ project, title: 'Zip bundle', entry: 'index.md', files });
  await openArtifact(page, project, a.id);
  await page.locator('scion-page-artifact-detail sl-tab[panel="files"]').click();
  const button = page.locator('scion-page-artifact-detail sl-tab-panel[name="files"] sl-button', {
    hasText: 'Download all (.zip)',
  });
  await expect(button).toBeVisible({ timeout: 30_000 });

  const [download] = await Promise.all([page.waitForEvent('download'), button.click()]);
  expect(download.suggestedFilename()).toBe(`zip-bundle-v${a.seq}.zip`);
  const saved = await download.path();
  const entries = readZip(await readFile(saved));
  expect([...entries.keys()]).toEqual(Object.keys(files).sort());
  for (const [name, data] of Object.entries(files)) {
    expect(entries.get(name)!.equals(data), name).toBe(true);
  }
});
