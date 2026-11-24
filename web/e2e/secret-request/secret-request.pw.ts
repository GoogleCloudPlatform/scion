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
 * Offline Playwright E2E regression spec for secret-request masking, verbatim
 * multi-line paste, file picker, and single-click submission (ptone/scion#8748).
 *
 * Verifies in real Chromium:
 * - All row types (`environment`, `variable`, `file`) render a native
 *   `<input type="password" autocomplete="off">` while masked, hiding values
 *   both visually and from the CDP accessibility tree;
 * - Multi-line paste is captured verbatim (newlines intact) while masked,
 *   and Show reveals a `<textarea>` for editing;
 * - `file` rows load UTF-8 files via the file picker and submit newlines intact;
 * - Typing a value and clicking Submit once immediately (with the field focused)
 *   causes no blur-triggered layout shift and sends exactly one fulfill POST.
 */

import { test, expect, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

interface FulfillRecord {
  url: string;
  body: { values: Record<string, string>; surface: string; idempotencyKey: string };
}

async function installMockSecretRequestHub(
  page: Page,
  card: {
    messageId: string;
    key: string;
    secretType: 'environment' | 'file' | 'variable';
  }
): Promise<FulfillRecord[]> {
  await installEventSourceStub(page);

  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'hub.agent_secrets': true };
  });

  const fulfills: FulfillRecord[] = [];

  await page.route('**/auth/me', (route) =>
    route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        id: 'user-1',
        email: 'alice@example.test',
        name: 'Alice',
        role: 'admin',
      }),
    })
  );

  await page.route('**/api/**', async (route) => {
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
        body: JSON.stringify({ experiments: { 'hub.agent_secrets': true } }),
      });
    }
    if (p === `/api/v1/secret-requests/${card.messageId}` && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          messageId: card.messageId,
          projectId: 'proj-1',
          projectName: 'alpha-project',
          agentId: 'agent-1',
          agentName: 'release-bot',
          canRespond: true,
          reason: '',
          rows: [
            {
              rowId: 'row-1',
              messageId: card.messageId,
              projectId: 'proj-1',
              agentId: 'agent-1',
              agentName: 'release-bot',
              key: card.key,
              version: 1,
              description: 'Test secret',
              secretType: card.secretType,
              position: 0,
              status: 'pending',
              createdAt: '2026-11-24T00:00:00Z',
              expiresAt: '2026-12-01T00:00:00Z',
            },
          ],
        }),
      });
    }
    if (
      p === `/api/v1/secret-requests/${card.messageId}/fulfill` &&
      route.request().method() === 'POST'
    ) {
      const body = route.request().postDataJSON() as FulfillRecord['body'];
      fulfills.push({ url: p, body });
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          rows: [
            {
              rowId: 'row-1',
              messageId: card.messageId,
              projectId: 'proj-1',
              agentId: 'agent-1',
              agentName: 'release-bot',
              key: card.key,
              version: 1,
              description: 'Test secret',
              secretType: card.secretType,
              position: 0,
              status: 'fulfilled',
              createdAt: '2026-11-24T00:00:00Z',
              expiresAt: '2026-12-01T00:00:00Z',
              fulfilled: {
                at: '2026-11-24T00:05:00Z',
                byUserId: 'user-1',
                byName: 'Alice',
                surface: body.surface,
              },
            },
          ],
          responseMessageId: 'resp-1',
          replayed: false,
        }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  return fulfills;
}

test('environment secret row masks natively as password input and submits on a single click', async ({
  page,
}) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-env-1',
    key: 'GITHUB_TOKEN',
    secretType: 'environment',
  });

  await page.goto('/secret-requests/msg-env-1', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="GITHUB_TOKEN"]');
  await expect(input).toBeVisible();
  await expect(input).toHaveAttribute('type', 'password');

  await input.click();
  await input.fill('ghp_test_token_123');

  const submitBtn = page.locator('sl-button.submit-btn');
  await expect(submitBtn).toBeEnabled();
  await submitBtn.click();

  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({ GITHUB_TOKEN: 'ghp_test_token_123' });
});

test('file secret row captures multi-line paste while masked, hides value from AX tree, and submits newlines intact', async ({
  page,
}) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-file-1',
    key: 'DEPLOY_KEY',
    secretType: 'file',
  });

  await page.goto('/secret-requests/msg-file-1', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="DEPLOY_KEY"]');
  await expect(input).toBeVisible();
  await expect(input).toHaveAttribute('type', 'password');
  await expect(input).toHaveAttribute('autocomplete', 'off');

  const multilineSecret = '-----BEGIN PRIVATE KEY-----\nline-2-secret\n-----END PRIVATE KEY-----';

  // Dispatch a real paste event on the password input with multi-line clipboard text
  await input.evaluate((el, text) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', text);
    el.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
    );
  }, multilineSecret);

  const maskedBadge = page.locator('[data-testid="file-value-masked"]');
  await expect(maskedBadge).toBeVisible();
  await expect(page.locator('textarea[data-key="DEPLOY_KEY"]')).toHaveCount(0);

  // Verify no plaintext in DOM or CDP accessibility tree while masked
  const cardText = await page.locator('scion-chat-secret-request').innerText();
  expect(cardText).not.toContain('line-2-secret');

  const cdp = await page.context().newCDPSession(page);
  const { nodes } = (await cdp.send('Accessibility.getFullAXTree')) as {
    nodes: Array<{ value?: { value?: unknown }; name?: { value?: unknown } }>;
  };
  const leaked = nodes.some(
    (n) =>
      (typeof n.value?.value === 'string' && n.value.value.includes('line-2-secret')) ||
      (typeof n.name?.value === 'string' && n.name.value.includes('line-2-secret'))
  );
  expect(leaked).toBe(false);

  // Single click on Submit sends the verbatim multi-line value with newlines intact
  const submitBtn = page.locator('sl-button.submit-btn');
  await expect(submitBtn).toBeEnabled();
  await submitBtn.click();

  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({
    DEPLOY_KEY: multilineSecret,
  });
});

test('file secret row loads UTF-8 file via file picker, stays masked, and submits newlines intact', async ({
  page,
}) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-file-picker-1',
    key: 'TLS_CERT',
    secretType: 'file',
  });

  await page.goto('/secret-requests/msg-file-picker-1', { waitUntil: 'domcontentloaded' });

  const fileContent =
    '-----BEGIN CERTIFICATE-----\npicker-secret-line\n-----END CERTIFICATE-----\n';
  const fileInput = page.locator('input[type="file"][data-file-input="TLS_CERT"]');
  await fileInput.setInputFiles({
    name: 'cert.pem',
    mimeType: 'text/plain',
    buffer: Buffer.from(fileContent, 'utf-8'),
  });

  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
  await expect(page.locator('textarea[data-key="TLS_CERT"]')).toHaveCount(0);

  const cdp = await page.context().newCDPSession(page);
  const { nodes } = (await cdp.send('Accessibility.getFullAXTree')) as {
    nodes: Array<{ value?: { value?: unknown }; name?: { value?: unknown } }>;
  };
  const leaked = nodes.some(
    (n) =>
      (typeof n.value?.value === 'string' && n.value.value.includes('picker-secret-line')) ||
      (typeof n.name?.value === 'string' && n.name.value.includes('picker-secret-line'))
  );
  expect(leaked).toBe(false);

  const submitBtn = page.locator('sl-button.submit-btn');
  await expect(submitBtn).toBeEnabled();
  await submitBtn.click();

  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({
    TLS_CERT: fileContent,
  });
});

test('variable secret row masks natively as password input, hides value from AX tree, and reveals textarea on Show', async ({
  page,
}) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-var-1',
    key: 'API_CONFIG',
    secretType: 'variable',
  });

  await page.goto('/secret-requests/msg-var-1', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="API_CONFIG"]');
  await expect(input).toBeVisible();
  await expect(input).toHaveAttribute('type', 'password');
  await expect(input).toHaveAttribute('autocomplete', 'off');

  const multilineVar = '{\n "a": 1,\n "secret": "super-secret-variable-42"\n}';

  // Multi-line paste while masked preserves newlines and hides value from AX tree
  await input.evaluate((el, text) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', text);
    el.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
    );
  }, multilineVar);

  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();

  // Verify the CDP accessibility tree does not expose the masked value
  const cdp = await page.context().newCDPSession(page);
  const { nodes } = (await cdp.send('Accessibility.getFullAXTree')) as {
    nodes: Array<{ value?: { value?: unknown }; name?: { value?: unknown } }>;
  };
  const leaked = nodes.some(
    (n) =>
      (typeof n.value?.value === 'string' && n.value.value.includes('super-secret-variable-42')) ||
      (typeof n.name?.value === 'string' && n.name.value.includes('super-secret-variable-42'))
  );
  expect(leaked).toBe(false);

  // Show switches to textarea with newlines preserved; Hide returns to password input
  const toggleBtn = page.locator('button[data-toggle-visibility="API_CONFIG"]');
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'false');
  await toggleBtn.click();
  const textarea = page.locator('textarea[data-key="API_CONFIG"]');
  await expect(textarea).toBeVisible();
  await expect(textarea).toHaveValue(multilineVar);
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'true');

  await toggleBtn.click();
  await expect(input).toBeVisible();
  await expect(input).toHaveValue('');
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'false');

  const submitBtn = page.locator('sl-button.submit-btn');
  await submitBtn.click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({ API_CONFIG: multilineVar });
});
