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
 * Create Agent auto-expose control (ptone/scion#2562 A2 F1, A5): the control
 * is seeded from the hub default, and the SCION_AUTO_EXPOSE_* keys are sent
 * only once the user operated it, so an untouched create inherits the
 * project, then template, then hub default value on the hub.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

interface CreatePrivate extends HTMLElement {
  loading: boolean;
  autoExposePortsEnabled: boolean;
  updateComplete: Promise<unknown>;
  buildConfig(): { env?: Record<string, string> };
}

let hubDefault = false;

function stubFetch(): void {
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

async function waitLoaded(c: CreatePrivate): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
  const deadline = Date.now() + 2000;
  while (c.loading && Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    await c.updateComplete;
  }
  await c.updateComplete;
}

async function mountAgentCreate(def: boolean): Promise<CreatePrivate> {
  hubDefault = def;
  stubFetch();
  const c = document.createElement('scion-page-agent-create') as CreatePrivate;
  document.body.appendChild(c);
  await waitLoaded(c);
  return c;
}

function sourceLabel(c: CreatePrivate): Element {
  const label = c.shadowRoot?.querySelector('[data-testid="auto-expose-source"]');
  expect(label).toBeTruthy();
  return label as Element;
}

function labelText(c: CreatePrivate): string {
  return (sourceLabel(c).textContent ?? '').replace(/\s+/g, ' ').trim();
}

/** Operates the auto-expose checkbox the way a user click does. */
async function clickAutoExpose(c: CreatePrivate, checked: boolean): Promise<void> {
  const box = sourceLabel(c).parentElement?.querySelector('sl-checkbox') as
    | (HTMLElement & { checked: boolean })
    | null;
  expect(box).toBeTruthy();
  box!.checked = checked;
  box!.dispatchEvent(new Event('sl-change'));
  await c.updateComplete;
}

function sentAutoExposeKeys(c: CreatePrivate): Record<string, string> {
  return Object.fromEntries(
    Object.entries(c.buildConfig().env ?? {}).filter(([k]) => k.startsWith('SCION_AUTO_EXPOSE_'))
  );
}

describe('agent-create auto-expose', () => {
  for (const def of [true, false]) {
    it(`seeds the control from the hub default (${def}) and sends no auto-expose key when untouched`, async () => {
      const c = await mountAgentCreate(def);
      expect(c.autoExposePortsEnabled).toBe(def);
      expect(sentAutoExposeKeys(c)).toEqual({});
    });
  }

  it('sends the auto-expose keys once the user operates the control', async () => {
    const c = await mountAgentCreate(true);
    await clickAutoExpose(c, false);
    expect(sentAutoExposeKeys(c)).toEqual({ SCION_AUTO_EXPOSE_PORTS: 'false' });
  });

  it('sends an explicit value equal to the hub default after unchecking and re-checking', async () => {
    const c = await mountAgentCreate(true);
    await clickAutoExpose(c, false);
    await clickAutoExpose(c, true);
    expect(sentAutoExposeKeys(c)).toMatchObject({ SCION_AUTO_EXPOSE_PORTS: 'true' });
  });

  // With the hub default true the sub-fields render untouched, so changing
  // only one of them must still send the keys.
  const subFields: [string, (root: ShadowRoot) => Element | null, string][] = [
    [
      'mode',
      (root) => root.querySelector('sl-option[value="denylist"]')?.closest('sl-select') ?? null,
      'sl-change',
    ],
    [
      'list',
      (root) => root.querySelector('sl-input[placeholder="e.g. 3000,5173,8080"]'),
      'sl-input',
    ],
    ['interval', (root) => root.querySelector('sl-input[placeholder="3s"]'), 'sl-input'],
  ];
  for (const [name, find, event] of subFields) {
    it(`sends the auto-expose keys after changing only the ${name} sub-field`, async () => {
      const c = await mountAgentCreate(true);
      const field = find(c.shadowRoot as ShadowRoot) as (HTMLElement & { value: string }) | null;
      expect(field).toBeTruthy();
      field!.value = name === 'mode' ? 'denylist' : '9';
      field!.dispatchEvent(new Event(event));
      await c.updateComplete;
      expect(sentAutoExposeKeys(c)).toMatchObject({ SCION_AUTO_EXPOSE_PORTS: 'true' });
    });
  }

  it('sends an empty port list when the user cleared it, overriding a template list', async () => {
    const c = await mountAgentCreate(true);
    const list = c.shadowRoot?.querySelector('sl-input[placeholder="e.g. 3000,5173,8080"]') as
      | (HTMLElement & { value: string })
      | null;
    expect(list).toBeTruthy();
    list!.value = '';
    list!.dispatchEvent(new Event('sl-input'));
    await c.updateComplete;
    expect(sentAutoExposeKeys(c)).toMatchObject({
      SCION_AUTO_EXPOSE_PORTS: 'true',
      SCION_AUTO_EXPOSE_PORTS_LIST: '',
    });
  });

  it('labels the control as inherited, naming the hub default, until the user operates it', async () => {
    const c = await mountAgentCreate(false);
    expect(labelText(c)).toBe(
      'Source: inherited (hub default shown; project or template may override)'
    );
    await clickAutoExpose(c, false);
    expect(labelText(c)).toBe('Source: explicit');
  });

  it('keeps a user toggle when the hub default is re-seeded by a later load', async () => {
    const c = await mountAgentCreate(false);
    await clickAutoExpose(c, true);

    // Re-attaching reruns loadFormData, which fetches the hub default again.
    hubDefault = false;
    c.remove();
    document.body.appendChild(c);
    await waitLoaded(c);

    expect(c.autoExposePortsEnabled).toBe(true);
    expect(sentAutoExposeKeys(c)).toMatchObject({ SCION_AUTO_EXPOSE_PORTS: 'true' });
  });
});
