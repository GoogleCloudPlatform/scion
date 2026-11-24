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

  // Snapshot Submit button Y before paste to verify reserved status-line space prevents layout shift
  const submitBtn = page.locator('sl-button.submit-btn');
  const submitYBefore = (await submitBtn.boundingBox())?.y ?? 0;

  // Multi-line paste while masked preserves newlines and hides value from AX tree
  await input.evaluate((el, text) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', text);
    el.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
    );
  }, multilineVar);

  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
  const submitYAfter = (await submitBtn.boundingBox())?.y ?? 0;
  expect(Math.abs(submitYAfter - submitYBefore)).toBeLessThanOrEqual(1);

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

  await submitBtn.click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({ API_CONFIG: multilineVar });
});

for (const secretType of ['file', 'variable'] as const) {
  test(`trusted dragTo of multi-line text onto ${secretType} row preserves newlines`, async ({
    page,
  }) => {
    const fulfills = await installMockSecretRequestHub(page, {
      messageId: `msg-drop-${secretType}`,
      key: 'DROP_KEY',
      secretType,
    });

    await page.goto(`/secret-requests/msg-drop-${secretType}`, { waitUntil: 'domcontentloaded' });

    const input = page.locator('input[data-key="DROP_KEY"]');
    await expect(input).toBeVisible();

    // Create a draggable source element carrying multi-line text
    await page.evaluate(() => {
      const src = document.createElement('div');
      src.id = 'drag-src';
      src.draggable = true;
      src.textContent = 'Drag secret';
      src.style.cssText =
        'position:fixed;top:8px;left:8px;padding:8px;background:#ccc;z-index:9999';
      src.addEventListener('dragstart', (ev) => {
        ev.dataTransfer?.setData('text/plain', 'D1\nD2\nD3');
      });
      document.body.appendChild(src);
    });

    await page.locator('#drag-src').dragTo(input);

    await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
    await page.locator('sl-button.submit-btn').click();
    await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
    expect(fulfills).toHaveLength(1);
    expect(fulfills[0].body.values).toEqual({ DROP_KEY: 'D1\nD2\nD3' });
  });
}

test('keyboard.insertText with LF and CRLF preserves newlines verbatim', async ({ page }) => {
  for (const [idx, text] of [
    ['lf', 'X1\nX2'],
    ['crlf', 'Y1\r\nY2'],
  ] as const) {
    const fulfills = await installMockSecretRequestHub(page, {
      messageId: `msg-insert-${idx}`,
      key: 'INSERT_KEY',
      secretType: 'variable',
    });

    await page.goto(`/secret-requests/msg-insert-${idx}`, { waitUntil: 'domcontentloaded' });
    const input = page.locator('input[data-key="INSERT_KEY"]');
    await expect(input).toBeVisible();
    await input.click();
    await page.keyboard.insertText(text);

    await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
    await page.locator('sl-button.submit-btn').click();
    await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
    expect(fulfills).toHaveLength(1);
    expect(fulfills[0].body.values).toEqual({ INSERT_KEY: text });
  }
});

test('type-then-paste composes at caret (end and middle) for both single-line and multi-line paste', async ({
  page,
}) => {
  // 1. Single-line paste at end ("abc" + "def" -> "abcdef")
  const f1 = await installMockSecretRequestHub(page, {
    messageId: 'msg-compose-end',
    key: 'COMP_KEY',
    secretType: 'environment',
  });
  await page.goto('/secret-requests/msg-compose-end', { waitUntil: 'domcontentloaded' });
  const input1 = page.locator('input[data-key="COMP_KEY"]');
  await input1.click();
  await page.keyboard.type('abc');
  await page.keyboard.insertText('def');
  await page.locator('sl-button.submit-btn').click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(f1[0].body.values).toEqual({ COMP_KEY: 'abcdef' });

  // 2. Single-line paste in middle ("abc", caret at 1, insert "def" -> "adefbc")
  const f2 = await installMockSecretRequestHub(page, {
    messageId: 'msg-compose-mid',
    key: 'COMP_KEY',
    secretType: 'environment',
  });
  await page.goto('/secret-requests/msg-compose-mid', { waitUntil: 'domcontentloaded' });
  const input2 = page.locator('input[data-key="COMP_KEY"]');
  await input2.click();
  await page.keyboard.type('abc');
  await input2.evaluate((el: HTMLInputElement) => el.setSelectionRange(1, 1));
  await page.keyboard.insertText('def');
  await page.locator('sl-button.submit-btn').click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(f2[0].body.values).toEqual({ COMP_KEY: 'adefbc' });

  // 3. Multi-line paste in middle ("abc", caret at 1, paste "X\nY" -> "aX\nYbc")
  const f3 = await installMockSecretRequestHub(page, {
    messageId: 'msg-compose-multi',
    key: 'COMP_KEY',
    secretType: 'environment',
  });
  await page.goto('/secret-requests/msg-compose-multi', { waitUntil: 'domcontentloaded' });
  const input3 = page.locator('input[data-key="COMP_KEY"]');
  await input3.click();
  await page.keyboard.type('abc');
  await input3.evaluate((el: HTMLInputElement) => el.setSelectionRange(1, 1));
  await input3.evaluate((el) => {
    const dt = new DataTransfer();
    dt.setData('text/plain', 'X\nY');
    el.dispatchEvent(
      new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
    );
  });
  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
  await page.locator('sl-button.submit-btn').click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(f3[0].body.values).toEqual({ COMP_KEY: 'aX\nYbc' });
});

test('multi-line paste then one Backspace clears captured value and disables Submit', async ({
  page,
}) => {
  await installMockSecretRequestHub(page, {
    messageId: 'msg-clear-bs',
    key: 'CLEAR_KEY',
    secretType: 'file',
  });
  await page.goto('/secret-requests/msg-clear-bs', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="CLEAR_KEY"]');
  await input.click();
  await page.keyboard.insertText('line1\nline2');

  const badge = page.locator('[data-testid="file-value-masked"]');
  const submitBtn = page.locator('sl-button.submit-btn');
  await expect(badge).toBeVisible();
  await expect(submitBtn).toBeEnabled();

  await page.keyboard.press('Backspace');
  await expect(badge).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Submit' })).toBeDisabled();
});

for (const secretType of ['environment', 'variable', 'file'] as const) {
  test(`all-CRLF Show -> edit -> Hide -> Submit preserves CRLF on ${secretType} row`, async ({
    page,
  }) => {
    const fulfills = await installMockSecretRequestHub(page, {
      messageId: `msg-crlf-edit-${secretType}`,
      key: 'CRLF_KEY',
      secretType,
    });
    await page.goto(`/secret-requests/msg-crlf-edit-${secretType}`, {
      waitUntil: 'domcontentloaded',
    });

    const input = page.locator('input[data-key="CRLF_KEY"]');
    await input.click();
    await page.keyboard.insertText('AAA-l1\r\nBBB-l2\r\nCCC-l3');
    await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();

    const toggleBtn = page.locator('button[data-toggle-visibility="CRLF_KEY"]');
    await toggleBtn.click();

    const textarea = page.locator('textarea[data-key="CRLF_KEY"]');
    await expect(textarea).toBeVisible();
    await expect(textarea).not.toHaveAttribute('readonly', '');
    await expect(page.locator('[data-testid="mixed-line-endings-note"]')).toHaveCount(0);

    // Move caret to end and type X
    await textarea.evaluate((el: HTMLTextAreaElement) => {
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    });
    await page.keyboard.type('X');

    await toggleBtn.click();
    await expect(input).toBeVisible();

    await page.locator('sl-button.submit-btn').click();
    await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
    expect(fulfills).toHaveLength(1);
    expect(fulfills[0].body.values).toEqual({
      CRLF_KEY: 'AAA-l1\r\nBBB-l2\r\nCCC-l3X',
    });
  });
}

test('mixed line endings render revealed textarea readonly with note and submit unchanged', async ({
  page,
}) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-mixed-le',
    key: 'MIXED_KEY',
    secretType: 'file',
  });
  await page.goto('/secret-requests/msg-mixed-le', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="MIXED_KEY"]');
  await input.click();
  await page.keyboard.insertText('a\r\nb\nc');
  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();

  const toggleBtn = page.locator('button[data-toggle-visibility="MIXED_KEY"]');
  await toggleBtn.click();

  const textarea = page.locator('textarea[data-key="MIXED_KEY"]');
  await expect(textarea).toBeVisible();
  await expect(textarea).toHaveAttribute('readonly', '');
  await expect(page.locator('[data-testid="mixed-line-endings-note"]')).toHaveText(
    'This value has mixed line endings; paste or load to replace it'
  );

  // Attempt to type into the readonly textarea — must not modify the stored value
  await textarea.click();
  await page.keyboard.type('IGNORED');

  await toggleBtn.click();
  await page.locator('sl-button.submit-btn').click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({ MIXED_KEY: 'a\r\nb\nc' });
});

test('plain LF Show -> edit -> Hide -> Submit keeps LF unchanged', async ({ page }) => {
  const fulfills = await installMockSecretRequestHub(page, {
    messageId: 'msg-lf-edit',
    key: 'LF_KEY',
    secretType: 'variable',
  });
  await page.goto('/secret-requests/msg-lf-edit', { waitUntil: 'domcontentloaded' });

  const input = page.locator('input[data-key="LF_KEY"]');
  await input.click();
  await page.keyboard.insertText('L1\nL2\nL3');
  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();

  const toggleBtn = page.locator('button[data-toggle-visibility="LF_KEY"]');
  await toggleBtn.click();

  const textarea = page.locator('textarea[data-key="LF_KEY"]');
  await expect(textarea).toBeVisible();
  await expect(textarea).not.toHaveAttribute('readonly', '');
  await textarea.evaluate((el: HTMLTextAreaElement) => {
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  });
  await page.keyboard.type('X');

  await toggleBtn.click();
  await page.locator('sl-button.submit-btn').click();
  await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
  expect(fulfills).toHaveLength(1);
  expect(fulfills[0].body.values).toEqual({ LF_KEY: 'L1\nL2\nL3X' });
});

async function installDynamicMockSecretRequestHub(
  page: Page,
  secretType: 'environment' | 'file' | 'variable',
  key: string
): Promise<Map<string, FulfillRecord['body']>> {
  await installEventSourceStub(page);

  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'hub.agent_secrets': true };
  });

  const fulfillsByMsg = new Map<string, FulfillRecord['body']>();

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
    const getMatch = /^\/api\/v1\/secret-requests\/([^/]+)$/.exec(p);
    if (getMatch && route.request().method() === 'GET') {
      const messageId = getMatch[1];
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          messageId,
          projectId: 'proj-1',
          projectName: 'alpha-project',
          agentId: 'agent-1',
          agentName: 'release-bot',
          canRespond: true,
          reason: '',
          rows: [
            {
              rowId: 'row-1',
              messageId,
              projectId: 'proj-1',
              agentId: 'agent-1',
              agentName: 'release-bot',
              key,
              version: 1,
              description: 'Matrix test secret',
              secretType,
              position: 0,
              status: 'pending',
              createdAt: '2026-11-24T00:00:00Z',
              expiresAt: '2026-12-01T00:00:00Z',
            },
          ],
        }),
      });
    }
    const fulfillMatch = /^\/api\/v1\/secret-requests\/([^/]+)\/fulfill$/.exec(p);
    if (fulfillMatch && route.request().method() === 'POST') {
      const messageId = fulfillMatch[1];
      const body = route.request().postDataJSON() as FulfillRecord['body'];
      fulfillsByMsg.set(messageId, body);
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          rows: [
            {
              rowId: 'row-1',
              messageId,
              projectId: 'proj-1',
              agentId: 'agent-1',
              agentName: 'release-bot',
              key,
              version: 1,
              description: 'Matrix test secret',
              secretType,
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
          responseMessageId: `resp-${messageId}`,
          replayed: false,
        }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  return fulfillsByMsg;
}

const MATRIX_PAYLOADS = [
  { name: 'single-line', value: 'single-secret-val' },
  { name: 'lf', value: 'L1\nL2\nL3' },
  { name: 'crlf', value: 'C1\r\nC2\r\nC3' },
  { name: 'mixed', value: 'M1\r\nM2\nM3' },
] as const;

type MatrixPath =
  | 'masked-paste'
  | 'masked-drop'
  | 'masked-insertText'
  | 'revealed-paste'
  | 'revealed-drop'
  | 'revealed-insertText'
  | 'revealed-typing-edit'
  | 'file-load';

for (const secretType of ['environment', 'variable', 'file'] as const) {
  test(`exact-bytes matrix for ${secretType} row across all entry paths and line-ending payloads`, async ({
    page,
  }) => {
    const key = 'MATRIX_KEY';
    const fulfills = await installDynamicMockSecretRequestHub(page, secretType, key);

    const paths: MatrixPath[] = [
      'masked-paste',
      'masked-drop',
      'masked-insertText',
      'revealed-paste',
      'revealed-drop',
      'revealed-insertText',
      'revealed-typing-edit',
      ...(secretType === 'file' ? (['file-load'] as const) : []),
    ];

    for (const entryPath of paths) {
      for (const payload of MATRIX_PAYLOADS) {
        const msgId = `mx-${secretType}-${entryPath}-${payload.name}`;
        await page.goto(`/secret-requests/${msgId}`, { waitUntil: 'domcontentloaded' });

        const input = page.locator(`input[data-key="${key}"]`);
        const toggleBtn = page.locator(`button[data-toggle-visibility="${key}"]`);
        const textarea = page.locator(`textarea[data-key="${key}"]`);
        await expect(input).toBeVisible();

        let expectedValue: string = payload.value;

        switch (entryPath) {
          case 'masked-paste': {
            await input.evaluate((el, text) => {
              const dt = new DataTransfer();
              dt.setData('text/plain', text);
              el.dispatchEvent(
                new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
              );
            }, payload.value);
            // Single-line paste lets browser handle natively; synthetic ClipboardEvent doesn't mutate value
            if (payload.name === 'single-line') {
              await input.fill(payload.value);
            }
            break;
          }
          case 'masked-drop': {
            await page.evaluate((text) => {
              document.getElementById('mx-drag-src')?.remove();
              const src = document.createElement('div');
              src.id = 'mx-drag-src';
              src.draggable = true;
              src.textContent = 'Drag';
              src.style.cssText =
                'position:fixed;top:8px;left:8px;padding:8px;background:#ccc;z-index:9999';
              src.addEventListener('dragstart', (ev) => {
                ev.dataTransfer?.setData('text/plain', text);
              });
              document.body.appendChild(src);
            }, payload.value);
            await page.locator('#mx-drag-src').dragTo(input);
            break;
          }
          case 'masked-insertText': {
            await input.click();
            await page.keyboard.insertText(payload.value);
            break;
          }
          case 'revealed-paste': {
            await toggleBtn.click();
            await expect(textarea).toBeVisible();
            await textarea.evaluate((el, text) => {
              const dt = new DataTransfer();
              dt.setData('text/plain', text);
              el.dispatchEvent(
                new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
              );
            }, payload.value);
            if (!payload.value.includes('\r')) {
              await textarea.fill(payload.value);
            }
            if (payload.name === 'mixed') {
              await expect(textarea).toHaveAttribute('readonly', '');
            }
            await toggleBtn.click();
            break;
          }
          case 'revealed-drop': {
            await toggleBtn.click();
            await expect(textarea).toBeVisible();
            await page.evaluate((text) => {
              document.getElementById('mx-drag-src')?.remove();
              const src = document.createElement('div');
              src.id = 'mx-drag-src';
              src.draggable = true;
              src.textContent = 'Drag';
              src.style.cssText =
                'position:fixed;top:8px;left:8px;padding:8px;background:#ccc;z-index:9999';
              src.addEventListener('dragstart', (ev) => {
                ev.dataTransfer?.setData('text/plain', text);
              });
              document.body.appendChild(src);
            }, payload.value);
            await page.locator('#mx-drag-src').dragTo(textarea);
            if (payload.name === 'mixed') {
              await expect(textarea).toHaveAttribute('readonly', '');
            }
            await toggleBtn.click();
            break;
          }
          case 'revealed-insertText': {
            await toggleBtn.click();
            await expect(textarea).toBeVisible();
            await textarea.click();
            await page.keyboard.insertText(payload.value);
            if (payload.name === 'mixed') {
              await expect(textarea).toHaveAttribute('readonly', '');
            }
            await toggleBtn.click();
            break;
          }
          case 'revealed-typing-edit': {
            await input.click();
            await page.keyboard.insertText(payload.value);
            await toggleBtn.click();
            await expect(textarea).toBeVisible();
            if (payload.name === 'mixed') {
              await expect(textarea).toHaveAttribute('readonly', '');
              await textarea.click();
              await page.keyboard.type('X');
              expectedValue = payload.value;
            } else {
              await expect(textarea).not.toHaveAttribute('readonly', '');
              await textarea.evaluate((el: HTMLTextAreaElement) => {
                el.focus();
                el.setSelectionRange(el.value.length, el.value.length);
              });
              await page.keyboard.type('X');
              expectedValue = `${payload.value}X`;
            }
            await toggleBtn.click();
            break;
          }
          case 'file-load': {
            const fileInput = page.locator(`input[type="file"][data-file-input="${key}"]`);
            await fileInput.setInputFiles({
              name: `${payload.name}.txt`,
              mimeType: 'text/plain',
              buffer: Buffer.from(payload.value, 'utf-8'),
            });
            break;
          }
        }

        await page.locator('sl-button.submit-btn').click();
        await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
        expect(
          fulfills.get(msgId)?.values,
          `${secretType} / ${entryPath} / ${payload.name}`
        ).toEqual({ [key]: expectedValue });
      }
    }

    // Also verify LF value -> Show -> paste "\r\nX" transitions to mixed readonly and submits verbatim
    const mixMsgId = `mx-${secretType}-lf-then-crlf-paste`;
    await page.goto(`/secret-requests/${mixMsgId}`, { waitUntil: 'domcontentloaded' });
    const input = page.locator(`input[data-key="${key}"]`);
    const toggleBtn = page.locator(`button[data-toggle-visibility="${key}"]`);
    const textarea = page.locator(`textarea[data-key="${key}"]`);
    await input.click();
    await page.keyboard.insertText('L1\nL2');
    await toggleBtn.click();
    await expect(textarea).toBeVisible();
    await textarea.evaluate((el: HTMLTextAreaElement) => {
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
      const dt = new DataTransfer();
      dt.setData('text/plain', '\r\nX');
      el.dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
      );
    });
    await expect(textarea).toHaveAttribute('readonly', '');
    await expect(page.locator('[data-testid="mixed-line-endings-note"]')).toBeVisible();
    await toggleBtn.click();
    await page.locator('sl-button.submit-btn').click();
    await expect(page.locator('scion-chat-secret-request')).toContainText('Fulfilled by');
    expect(fulfills.get(mixMsgId)?.values).toEqual({ [key]: 'L1\nL2\r\nX' });
  });
}
