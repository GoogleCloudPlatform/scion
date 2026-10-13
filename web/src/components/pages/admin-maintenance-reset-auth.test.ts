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

// The Reset Auth panel renders each failure's plain reason with its
// category, and the per-category counts from failed_by_code.

const CREATOR_REASON =
  'creator agent parent-1 has no active delegation in this project (deleted or moved); recreate this agent';

const RESET_AUTH_RESULT = {
  succeeded: [{ id: 'ok-1', name: 'ok' }],
  failed: [
    {
      id: 'child-1',
      name: 'child',
      error: CREATOR_REASON,
      code: 'delegation_creator_missing',
      cause: 'ceiling_orphaned',
      ancestor_id: 'parent-1',
      ancestor_depth: 1,
    },
    { id: 'rt-1', name: 'rt', error: 'runtime not available', code: 'runtime_unavailable' },
    { id: 'x-1', name: 'x', error: 'broker unreachable' },
  ],
  total: 4,
  failed_by_code: { delegation_creator_missing: 1, runtime_unavailable: 1, unclassified: 1 },
};

function handler(input: string | URL | Request): Promise<Response> {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const path = new URL(url, 'http://localhost').pathname;
  const body =
    path === '/api/v1/admin/agents/reset-auth-all'
      ? RESET_AUTH_RESULT
      : path === '/api/v1/admin/maintenance'
        ? { enabled: false }
        : path.endsWith('/operations')
          ? { migrations: [], operations: [] }
          : {};
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

type MaintenancePage = HTMLElement & {
  updateComplete: Promise<unknown>;
  handleResetAuthAll(): Promise<void>;
};

let el: MaintenancePage | null = null;
afterEach(() => {
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

describe('admin maintenance reset auth result', () => {
  it('renders the plain reasons and per-category counts', async () => {
    vi.stubGlobal('fetch', vi.fn(handler));
    el = document.createElement('scion-page-admin-maintenance') as MaintenancePage;
    document.body.appendChild(el);
    await el.updateComplete;

    await el.handleResetAuthAll();
    await el.updateComplete;

    const categories = el.shadowRoot?.querySelector('.reset-auth-categories')?.textContent ?? '';
    expect(categories).toContain('Creator delegation missing: 1');
    expect(categories).toContain('Runtime unavailable: 1');
    expect(categories).toContain('Other: 1');

    const log = el.shadowRoot?.querySelector('.result-log.result-error')?.textContent ?? '';
    expect(log).toContain(`child: ${CREATOR_REASON} [Creator delegation missing]`);
    expect(log).toContain('rt: runtime not available [Runtime unavailable]');
    expect(log).toContain('x: broker unreachable');
    expect(log).not.toContain('x: broker unreachable [');
  });
});
