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
 * Chromium: a space in the query matches a hyphen, so "native chat" puts the
 * native-chat agent first, with looser multi-word matches listed below it.
 */

import { test, expect } from '@playwright/test';
import { setupApiMocks } from './mock-api.js';

const agent = (name: string) => ({ id: `agent-${name}`, name, slug: name });

test('"native chat" ranks the native-chat agent first and keeps looser matches', async ({
  page,
}) => {
  await setupApiMocks(page, {
    agents: [agent('chat-native'), agent('native-agent-chat'), agent('native-chat')],
  });
  await page.goto('/e2e/chat-palette/fixture.html', { waitUntil: 'domcontentloaded' });
  await page.waitForFunction(() => !!document.querySelector('scion-page-chat'));
  await page.keyboard.press('Control+k');
  const input = page.locator('scion-quick-palette #palette-query-input');
  await expect(input).toBeFocused();
  await input.fill('native chat');

  const labels = page.locator('scion-quick-palette .palette-option > div:first-child');
  await expect(labels).toHaveText(['native-chat', 'chat-native', 'native-agent-chat']);
  const first = page.locator('scion-quick-palette .palette-option').first();
  await expect(first).toHaveAttribute('aria-selected', 'true');
  await expect(first.locator('mark')).toHaveText(['native-chat']);
});
