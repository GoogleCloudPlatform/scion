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
 * Test identity chip (ptone/scion#4240, phase WS1): rendered in the app
 * shell for a test identity web session only (GET /auth/me testIdentity),
 * with no close control, and its icon registered for production builds.
 */

// @vitest-environment happy-dom

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';
import { html, render } from 'lit';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

vi.mock('../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

import { renderTestIdentityChip, testIdentityChipText } from './shared/test-identity-chip.js';
import { resetTestInfraStatusForTests } from '../client/test-infra-status.js';
import type { User } from '../shared/types.js';
import type { ScionApp } from './app-shell.js';

const FIXTURE: User = {
  id: 'u-1',
  email: 'test-identity-abc@scion-fixture.invalid',
  name: 'Test identity abc',
  role: 'member',
  testIdentity: { expiresAt: '2026-10-11T22:30:00Z' },
};
const HUMAN: User = { id: 'u-2', email: 'alice@example.com', name: 'Alice', role: 'member' };

function stubFetch(): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(() =>
      Promise.resolve(
        new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
      )
    )
  );
}

afterEach(() => {
  resetTestInfraStatusForTests();
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('testIdentityChipText', () => {
  it('names the identity and its expiry', () => {
    const text = testIdentityChipText(FIXTURE)!;
    expect(text.startsWith(`Test identity ${FIXTURE.email} · expires `)).toBe(true);
    expect(text).toMatch(/\d{2}:\d{2}/);
  });

  it('is null for any other session', () => {
    expect(testIdentityChipText(HUMAN)).toBeNull();
    expect(testIdentityChipText(null)).toBeNull();
    expect(testIdentityChipText(undefined)).toBeNull();
    expect(testIdentityChipText({ ...HUMAN, testIdentity: { expiresAt: '' } })).toBeNull();
  });

  it('keeps the email when the expiry does not parse', () => {
    expect(testIdentityChipText({ ...FIXTURE, testIdentity: { expiresAt: 'not-a-date' } })).toBe(
      `Test identity ${FIXTURE.email}`
    );
  });
});

describe('renderTestIdentityChip', () => {
  it('renders a status element with no close control for a test identity', () => {
    const host = document.createElement('div');
    render(html`${renderTestIdentityChip(FIXTURE)}`, host);
    const chip = host.querySelector('[data-testid="test-identity-chip"]');
    expect(chip).not.toBeNull();
    expect(chip!.getAttribute('role')).toBe('status');
    expect(chip!.textContent).toContain(FIXTURE.email);
    expect(chip!.querySelector('sl-icon')?.getAttribute('name')).toBe('person-circle');
    expect(host.querySelector('[closable], sl-icon-button')).toBeNull();
  });

  it('renders nothing for a human session', () => {
    const host = document.createElement('div');
    render(html`${renderTestIdentityChip(HUMAN)}`, host);
    expect(host.querySelector('[data-testid="test-identity-chip"]')).toBeNull();
  });

  it('uses only registered icons', () => {
    const src = readFileSync(resolve(__dirname, '../../scripts/copy-shoelace-icons.mjs'), 'utf8');
    expect(src).toContain("'person-circle'");
  });
});

describe('app shell', () => {
  beforeAll(async () => {
    await import('./app-shell.js');
  });

  async function mountShell(user: User | null): Promise<ScionApp> {
    const shell = document.createElement('scion-app');
    shell.user = user;
    document.body.appendChild(shell);
    await shell.updateComplete;
    return shell;
  }

  it('shows the chip for a test identity session, before the header', async () => {
    stubFetch();
    const shell = await mountShell(FIXTURE);
    const chip = shell.shadowRoot!.querySelector('[data-testid="test-identity-chip"]');
    expect(chip).not.toBeNull();
    expect(chip!.nextElementSibling?.tagName.toLowerCase()).toBe('scion-header');
  });

  it('shows no chip for a human session', async () => {
    stubFetch();
    const shell = await mountShell(HUMAN);
    expect(shell.shadowRoot!.querySelector('[data-testid="test-identity-chip"]')).toBeNull();
  });
});
