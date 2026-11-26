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
 * Offline Playwright E2E regression spec for create-form Secrets tab semantic
 * masking, verbatim multi-line entry, file loading, and single-click create
 * (ptone/scion#9110).
 */

import { test, expect, type Page } from '@playwright/test';
import { installEventSourceStub } from '../event-source-stub.js';

interface CreateAgentBody {
  name: string;
  projectId: string;
  secrets?: Array<{
    key: string;
    value: string;
    type: string;
    target?: string;
    injectionMode: string;
  }>;
}

async function installMockCreateAgentHub(page: Page): Promise<CreateAgentBody[]> {
  await installEventSourceStub(page);

  await page.addInitScript(() => {
    window.__SCION_FEATURES__ = { 'hub.agent_secrets': true };
  });

  const creates: CreateAgentBody[] = [];

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
    if (p === '/api/v1/projects' && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          projects: [{ id: 'proj-1', name: 'Alpha Project' }],
        }),
      });
    }
    if (p === '/api/v1/runtime-brokers' && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          brokers: [{ id: 'broker-1', name: 'local-broker', status: 'online' }],
        }),
      });
    }
    if (p === '/api/v1/templates' && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          templates: [{ id: 'tmpl-1', name: 'default', slug: 'default', scope: 'global' }],
        }),
      });
    }
    if (p === '/api/v1/harness-configs' && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          harnessConfigs: [
            { id: 'hc-1', name: 'claude', slug: 'claude', harness: 'claude', scope: 'global' },
          ],
        }),
      });
    }
    if (p === '/api/v1/projects/proj-1/settings' && route.request().method() === 'GET') {
      return route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: '{}',
      });
    }
    if (p === '/api/v1/agents' && route.request().method() === 'POST') {
      const body = route.request().postDataJSON() as CreateAgentBody;
      creates.push(body);
      return route.fulfill({
        status: 201,
        contentType: 'application/json',
        body: JSON.stringify({
          agent: { id: `agent-${creates.length}`, name: body.name, phase: 'running' },
        }),
      });
    }
    return route.fulfill({ status: 200, contentType: 'application/json', body: '{}' });
  });

  return creates;
}

async function assertNotInAXTree(page: Page, secretNeedle: string): Promise<void> {
  const cdp = await page.context().newCDPSession(page);
  try {
    const { nodes } = (await cdp.send('Accessibility.getFullAXTree')) as {
      nodes: Array<{ value?: { value?: unknown }; name?: { value?: unknown } }>;
    };
    const leaked = nodes.some(
      (n) =>
        (typeof n.value?.value === 'string' && n.value.value.includes(secretNeedle)) ||
        (typeof n.name?.value === 'string' && n.name.value.includes(secretNeedle))
    );
    expect(leaked).toBe(false);
  } finally {
    await cdp.detach();
  }
}

async function openSecretsTabAndAddRow(
  page: Page,
  opts: {
    agentName: string;
    key: string;
    type?: 'environment' | 'file' | 'on_demand';
    target?: string;
  }
): Promise<void> {
  await page.goto('/agents/new?advanced=1', { waitUntil: 'domcontentloaded' });

  const nameInput = page.locator('sl-input#name input');
  await expect(nameInput).toBeVisible();
  await nameInput.fill(opts.agentName);

  const secretsTab = page.locator('[data-testid="tab-secrets"]');
  await expect(secretsTab).toBeVisible();
  await secretsTab.click();

  const addBtn = page.locator('[data-testid="add-secret-row"]');
  await expect(addBtn).toBeVisible();
  await addBtn.click();

  const row = page.locator('[data-testid="secret-row"]').last();
  await expect(row).toBeVisible();

  await row.locator('sl-input[data-field="key"] input').fill(opts.key);

  if (opts.type && opts.type !== 'environment') {
    const typeSelect = row.locator('sl-select[data-field="type"]');
    await typeSelect.evaluate((el: HTMLElement & { value: string }, val: string) => {
      el.value = val;
      el.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
    }, opts.type);
  }

  if (opts.target !== undefined) {
    await row.locator('sl-input[data-field="target"] input').fill(opts.target);
  }
}

test('masked typing hides plaintext from DOM and AX tree and one click on Start sends exactly one POST', async ({
  page,
}) => {
  const creates = await installMockCreateAgentHub(page);
  await openSecretsTabAndAddRow(page, {
    agentName: 'typed-agent',
    key: 'GITHUB_TOKEN',
    type: 'environment',
  });

  const input = page.locator('input[data-key="GITHUB_TOKEN"]');
  await expect(input).toBeVisible();
  await expect(input).toHaveAttribute('type', 'password');
  await expect(input).toHaveAttribute('autocomplete', 'off');

  const startBtn = page.locator('.form-actions sl-button[variant="primary"]');
  await input.click();
  const startYBefore = (await startBtn.boundingBox())?.y ?? 0;

  await page.keyboard.type('ghp_typed_secret_canary_9110');

  // Verify no plaintext in DOM text or CDP AX tree while masked
  const formText = await page.locator('scion-agent-config-form').innerText();
  expect(formText).not.toContain('ghp_typed_secret_canary_9110');
  await assertNotInAXTree(page, 'ghp_typed_secret_canary_9110');

  // Verify Start button did not shift vertically; one click right after typing sends one POST
  const startYAfter = (await startBtn.boundingBox())?.y ?? 0;
  expect(Math.abs(startYAfter - startYBefore)).toBeLessThanOrEqual(1);

  await startBtn.click();
  await expect.poll(() => creates.length).toBe(1);
  expect(creates).toHaveLength(1);
  expect(creates[0].secrets).toEqual([
    {
      key: 'GITHUB_TOKEN',
      value: 'ghp_typed_secret_canary_9110',
      type: 'environment',
      injectionMode: 'always',
    },
  ]);
});

for (const [label, secretType, payload, target] of [
  [
    'LF file',
    'file',
    '-----BEGIN PRIVATE KEY-----\ncreate-lf-canary\n-----END PRIVATE KEY-----',
    '~/.ssh/id_ed25519',
  ],
  ['CRLF on_demand', 'on_demand', 'LINE1\r\ncreate-crlf-canary\r\nLINE3', undefined],
] as const) {
  test(`masked multi-line paste (${label}) hides plaintext from DOM/AX tree and submits exact bytes`, async ({
    page,
  }) => {
    const creates = await installMockCreateAgentHub(page);
    await openSecretsTabAndAddRow(page, {
      agentName: `paste-${secretType}`,
      key: 'PASTE_KEY',
      type: secretType,
      ...(target ? { target } : {}),
    });

    const input = page.locator('input[data-key="PASTE_KEY"]');
    await expect(input).toBeVisible();
    await expect(input).toHaveAttribute('type', 'password');

    const startBtn = page.locator('.form-actions sl-button[variant="primary"]');
    const startYBefore = (await startBtn.boundingBox())?.y ?? 0;

    await input.evaluate((el, text) => {
      const dt = new DataTransfer();
      dt.setData('text/plain', text);
      el.dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true })
      );
    }, payload);

    await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
    await expect(page.locator('textarea[data-key="PASTE_KEY"]')).toHaveCount(0);

    const startYAfter = (await startBtn.boundingBox())?.y ?? 0;
    expect(Math.abs(startYAfter - startYBefore)).toBeLessThanOrEqual(1);

    const needle = payload.includes('lf-canary') ? 'create-lf-canary' : 'create-crlf-canary';
    const formText = await page.locator('scion-agent-config-form').innerText();
    expect(formText).not.toContain(needle);
    await assertNotInAXTree(page, needle);

    await startBtn.click();
    await expect.poll(() => creates.length).toBe(1);
    expect(creates[0].secrets?.[0].value).toBe(payload);
  });
}

test('trusted drop of multi-line text onto masked field submits exact bytes', async ({ page }) => {
  const creates = await installMockCreateAgentHub(page);
  await openSecretsTabAndAddRow(page, {
    agentName: 'drop-agent',
    key: 'DROP_KEY',
    type: 'file',
    target: '/etc/app/drop.pem',
  });

  const input = page.locator('input[data-key="DROP_KEY"]');
  await expect(input).toBeVisible();

  await page.evaluate(() => {
    const src = document.createElement('div');
    src.id = 'drag-src';
    src.draggable = true;
    src.textContent = 'Drag secret';
    src.style.cssText = 'position:fixed;top:8px;left:8px;padding:8px;background:#ccc;z-index:9999';
    src.addEventListener('dragstart', (ev) => {
      ev.dataTransfer?.setData('text/plain', 'D1\ndrop-canary\nD3');
    });
    document.body.appendChild(src);
  });

  await page.locator('#drag-src').dragTo(input);
  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
  await assertNotInAXTree(page, 'drop-canary');

  await page.locator('.form-actions sl-button[variant="primary"]').click();
  await expect.poll(() => creates.length).toBe(1);
  expect(creates[0].secrets?.[0].value).toBe('D1\ndrop-canary\nD3');
});

test('keyboard.insertText with LF and CRLF submits exact bytes', async ({ page }) => {
  for (const [label, text] of [
    ['lf', 'X1\nX2'],
    ['crlf', 'Y1\r\nY2'],
  ] as const) {
    const creates = await installMockCreateAgentHub(page);
    await openSecretsTabAndAddRow(page, {
      agentName: `insert-${label}`,
      key: 'INSERT_KEY',
      type: 'on_demand',
    });

    const input = page.locator('input[data-key="INSERT_KEY"]');
    await input.click();
    await page.keyboard.insertText(text);

    await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
    await page.locator('.form-actions sl-button[variant="primary"]').click();
    await expect.poll(() => creates.length).toBe(1);
    expect(creates[0].secrets?.[0].value).toBe(text);
  }
});

test('Show -> edit -> Hide preserves CRLF and re-masks value from DOM and AX tree', async ({
  page,
}) => {
  const creates = await installMockCreateAgentHub(page);
  await openSecretsTabAndAddRow(page, {
    agentName: 'show-edit-hide',
    key: 'CRLF_KEY',
    type: 'file',
    target: '~/.ssh/crlf_key',
  });

  const input = page.locator('input[data-key="CRLF_KEY"]');
  await input.click();
  await page.keyboard.insertText('AAA-l1\r\nBBB-secret\r\nCCC-l3');
  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();

  const toggleBtn = page.locator('button[data-toggle-visibility="CRLF_KEY"]');
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'false');
  await toggleBtn.click();

  const textarea = page.locator('textarea[data-key="CRLF_KEY"]');
  await expect(textarea).toBeVisible();
  await expect(textarea).not.toHaveAttribute('readonly', '');
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'true');

  await textarea.evaluate((el: HTMLTextAreaElement) => {
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  });
  await page.keyboard.type('X');

  await toggleBtn.click();
  await expect(input).toBeVisible();
  await expect(textarea).toHaveCount(0);
  await expect(toggleBtn).toHaveAttribute('aria-pressed', 'false');
  await assertNotInAXTree(page, 'BBB-secret');

  await page.locator('.form-actions sl-button[variant="primary"]').click();
  await expect.poll(() => creates.length).toBe(1);
  expect(creates[0].secrets?.[0].value).toBe('AAA-l1\r\nBBB-secret\r\nCCC-l3X');
});

test('file row Load from file preserves UTF-8 BOM and CRLF verbatim while staying masked', async ({
  page,
}) => {
  const creates = await installMockCreateAgentHub(page);
  await openSecretsTabAndAddRow(page, {
    agentName: 'file-load-agent',
    key: 'TLS_CERT',
    type: 'file',
    target: '/etc/ssl/cert.pem',
  });

  const fileContent =
    '\uFEFF-----BEGIN CERTIFICATE-----\r\npicker-canary-line\r\n-----END CERTIFICATE-----\r\n';
  const fileInput = page.locator('input[type="file"][data-file-input="TLS_CERT"]');
  await fileInput.setInputFiles({
    name: 'cert.pem',
    mimeType: 'text/plain',
    buffer: Buffer.from(fileContent, 'utf-8'),
  });

  await expect(page.locator('[data-testid="file-value-masked"]')).toBeVisible();
  await expect(page.locator('textarea[data-key="TLS_CERT"]')).toHaveCount(0);

  const formText = await page.locator('scion-agent-config-form').innerText();
  expect(formText).not.toContain('picker-canary-line');
  await assertNotInAXTree(page, 'picker-canary-line');

  await page.locator('.form-actions sl-button[variant="primary"]').click();
  await expect.poll(() => creates.length).toBe(1);
  expect(creates[0].secrets).toEqual([
    {
      key: 'TLS_CERT',
      value: fileContent,
      type: 'file',
      target: '/etc/ssl/cert.pem',
    },
  ]);
});

test('create form at 1280px fits More Details tabs without horizontal scroll and enforces Secrets tab UX', async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await installMockCreateAgentHub(page);
  await page.goto('/agents/new?advanced=1', { waitUntil: 'domcontentloaded' });

  const secretsTab = page.locator('[data-testid="tab-secrets"]');
  await expect(secretsTab).toBeVisible();

  // Verify the tab bar does not scroll horizontally at 1280px.
  const tabNavMetrics = await page
    .locator('scion-agent-config-form sl-tab-group')
    .evaluate((el) => {
      const nav = el.shadowRoot?.querySelector<HTMLElement>('[part="nav"]');
      if (!nav) return null;
      return { scrollWidth: nav.scrollWidth, clientWidth: nav.clientWidth };
    });
  expect(tabNavMetrics).not.toBeNull();
  expect(tabNavMetrics!.scrollWidth).toBeLessThanOrEqual(tabNavMetrics!.clientWidth);

  await secretsTab.click();
  await page.locator('[data-testid="add-secret-row"]').click();
  const row = page.locator('[data-testid="secret-row"]').first();
  await expect(row).toBeVisible();

  // Type tooltip present, no delivery selector, Target hidden for environment.
  await expect(row.locator('[data-testid="secret-type-tooltip"]')).toBeVisible();
  await expect(row.locator('[data-field="injectionMode"]')).toHaveCount(0);
  await expect(row.locator('[data-field="target"]')).toHaveCount(0);

  // Switch to File -> Target appears.
  const typeSelect = row.locator('sl-select[data-field="type"]');
  await typeSelect.evaluate((el: HTMLElement & { value: string }) => {
    el.value = 'file';
    el.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  });
  await expect(row.locator('[data-field="target"]')).toBeVisible();

  // Switch to On demand -> Target disappears.
  await typeSelect.evaluate((el: HTMLElement & { value: string }) => {
    el.value = 'on_demand';
    el.dispatchEvent(new Event('sl-change', { bubbles: true, composed: true }));
  });
  await expect(row.locator('[data-field="target"]')).toHaveCount(0);
});
