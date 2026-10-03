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
 * Create Agent auto-expose control (ptone/scion#2562 A2 F1): the control is
 * seeded from the hub default, and the SCION_AUTO_EXPOSE_* keys are sent only
 * when the user changed it, so an untouched create inherits the project, then
 * template, then hub default value on the hub.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface CreatePrivate {
  loading: boolean;
  autoExposePortsEnabled: boolean;
  autoExposePortsMode: string;
  updateComplete: Promise<unknown>;
  shadowRoot: ShadowRoot | null;
  buildConfig(): { env?: Record<string, string> };
}

function stubFetch(hubDefault: boolean): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      const body = url.includes('/settings/public')
        ? { autoExposePortsEnabled: hubDefault }
        : { projects: [], brokers: [], templates: [], harnessConfigs: [] };
      return Promise.resolve({ ok: true, status: 200, json: async () => body } as Response);
    })
  );
}

beforeAll(async () => {
  await import('./agent-create.js');
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

async function mountAgentCreate(hubDefault: boolean): Promise<CreatePrivate> {
  stubFetch(hubDefault);
  await import('./agent-create.js');
  const el = document.createElement('scion-page-agent-create');
  document.body.appendChild(el);
  const c = el as unknown as CreatePrivate;
  await new Promise((r) => setTimeout(r, 0));
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  return c;
}

function sentAutoExposeKeys(c: CreatePrivate): string[] {
  return Object.keys(c.buildConfig().env ?? {}).filter((k) => k.startsWith('SCION_AUTO_EXPOSE_'));
}

describe('agent-create auto-expose', () => {
  for (const hubDefault of [true, false]) {
    it(`seeds the control from the hub default (${hubDefault}) and sends no auto-expose key when untouched`, async () => {
      const c = await mountAgentCreate(hubDefault);
      expect(c.autoExposePortsEnabled).toBe(hubDefault);
      expect(sentAutoExposeKeys(c)).toEqual([]);
    });
  }

  it('sends the auto-expose keys as explicit values once the user changes the control', async () => {
    const c = await mountAgentCreate(true);
    c.autoExposePortsEnabled = false;
    expect(c.buildConfig().env).toMatchObject({ SCION_AUTO_EXPOSE_PORTS: 'false' });

    c.autoExposePortsEnabled = true;
    c.autoExposePortsMode = 'denylist';
    expect(c.buildConfig().env).toMatchObject({
      SCION_AUTO_EXPOSE_PORTS: 'true',
      SCION_AUTO_EXPOSE_MODE: 'denylist',
    });
  });

  it('labels the control as inherited until it changes', async () => {
    const c = await mountAgentCreate(false);
    const label = (): string =>
      (c.shadowRoot?.querySelector('[data-testid="auto-expose-source"]')?.textContent ?? '')
        .replace(/\s+/g, ' ')
        .trim();
    await c.updateComplete;
    expect(label()).toBe('Source: inherited');
    c.autoExposePortsEnabled = true;
    await c.updateComplete;
    expect(label()).toBe('Source: explicit');
  });
});
