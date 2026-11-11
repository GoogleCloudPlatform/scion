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

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// When the hub reports break_glass, the maintenance
// page explains why turning maintenance off has no effect.

function handler(breakGlass: boolean) {
  return (input: string | URL | Request): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const path = new URL(url, 'http://localhost').pathname;
    const body =
      path === '/api/v1/admin/maintenance'
        ? { enabled: true, message: 'm', ...(breakGlass ? { break_glass: true } : {}) }
        : path.endsWith('/operations')
          ? { migrations: [], operations: [] }
          : {};
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
  };
}

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

let el: HTMLElement | null = null;
afterEach(() => {
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function mount(breakGlass: boolean): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(handler(breakGlass)));
  const e = document.createElement('scion-page-admin-maintenance') as HTMLElement & {
    updateComplete: Promise<unknown>;
  };
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

describe('admin maintenance break-glass notice', () => {
  it('is shown when the hub reports break_glass', async () => {
    el = await mount(true);
    expect(el.shadowRoot?.querySelector('.break-glass-notice')?.textContent).toContain(
      'SCION_SERVER_ADMIN_MODE'
    );
  });

  it('is not shown otherwise', async () => {
    el = await mount(false);
    expect(el.shadowRoot?.querySelector('.break-glass-notice')).toBeNull();
  });
});
