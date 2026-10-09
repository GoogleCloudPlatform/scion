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
 * Tests for env-var-list.ts read-only mode: viewers with read access only
 * see the list without add, edit or delete controls.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

const ENV_VARS = [
  {
    id: 'ev-1',
    key: 'HUB_PLAIN',
    value: 'plain-value',
    scope: 'hub',
    scopeId: 'hub-1',
    injectionMode: 'always',
    created: '2026-10-01T10:00:00Z',
    updated: '2026-10-01T10:00:00Z',
  },
];

function installFetch(envVars: unknown[]): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      (): Promise<Response> =>
        Promise.resolve(
          new Response(JSON.stringify({ envVars }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
    )
  );
}

async function createComponent(envVars: unknown[], readonly: boolean): Promise<any> {
  installFetch(envVars);
  const el = document.createElement('scion-env-var-list') as any;
  el.setAttribute('scope', 'hub');
  el.setAttribute('apiBasePath', '/api/v1');
  el.setAttribute('compact', '');
  if (readonly) el.setAttribute('readonly', '');
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
  return el;
}

function iconButtonLabels(el: any): (string | null)[] {
  return Array.from((el.shadowRoot as ShadowRoot).querySelectorAll('sl-icon-button')).map((b) =>
    (b as Element).getAttribute('label')
  );
}

describe('scion-env-var-list readonly', () => {
  beforeAll(async () => {
    const mod = await import('./env-var-list.js');
    expect(mod.ScionEnvVarList).toBeDefined();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('shows add, edit and delete controls when editable', async () => {
    const el = await createComponent(ENV_VARS, false);
    const shadow = el.shadowRoot as ShadowRoot;

    expect(shadow.textContent).toContain('HUB_PLAIN');
    expect(shadow.textContent).toContain('Add Variable');
    expect(iconButtonLabels(el)).toEqual(expect.arrayContaining(['Edit', 'Delete']));
    expect(shadow.querySelector('sl-dialog')).not.toBeNull();
  });

  it('lists variables without add, edit or delete controls when readonly', async () => {
    const el = await createComponent(ENV_VARS, true);
    const shadow = el.shadowRoot as ShadowRoot;

    expect(shadow.textContent).toContain('HUB_PLAIN');
    expect(shadow.textContent).toContain('plain-value');
    expect(shadow.textContent).not.toContain('Add Variable');
    expect(iconButtonLabels(el)).not.toContain('Edit');
    expect(iconButtonLabels(el)).not.toContain('Delete');
    expect(shadow.querySelector('sl-dialog')).toBeNull();
  });

  it('shows no add prompt in the readonly empty state', async () => {
    const el = await createComponent([], true);
    const shadow = el.shadowRoot as ShadowRoot;

    expect(shadow.textContent).toContain('No Environment Variables');
    expect(shadow.textContent).not.toContain('Add Variable');
  });
});
