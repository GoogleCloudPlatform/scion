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

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

// A failed run request on the maintenance page keeps the page rendered and
// reports the server error as a toast. Only a failed page load shows the
// whole-page Failed to Load state.

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import { showToast } from '../../utils/toast.js';

const SERVER_ERROR = 'migration lock is held by another run';

const MIGRATIONS = [
  {
    key: 'mig-a',
    title: 'Migration A',
    description: 'First migration',
    category: 'migration',
    status: 'failed',
    createdAt: '2026-01-01T00:00:00Z',
  },
];

const OPERATIONS = [
  {
    key: 'op-a',
    title: 'Operation A',
    description: 'First operation',
    category: 'operation',
    status: 'pending',
    createdAt: '2026-01-01T00:00:00Z',
  },
];

interface Options {
  /** Status for POST .../run requests. */
  runStatus?: number;
  /** Status for GET /api/v1/admin/maintenance/operations. */
  listStatus?: number;
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function handler(opts: Options) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const path = new URL(url, 'http://localhost').pathname;
    const method = (init?.method ?? 'GET').toUpperCase();

    if (method === 'POST' && path.endsWith('/run')) {
      const status = opts.runStatus ?? 200;
      if (status >= 400) {
        return json({ error: { code: 'conflict', message: SERVER_ERROR } }, status);
      }
      return json({ id: 'run-1', status: 'running' }, status);
    }
    if (path === '/api/v1/admin/maintenance/operations') {
      const status = opts.listStatus ?? 200;
      if (status >= 400) {
        return json({ error: { code: 'internal', message: 'list unavailable' } }, status);
      }
      return json({ migrations: MIGRATIONS, operations: OPERATIONS });
    }
    if (path === '/api/v1/admin/maintenance') {
      return json({ enabled: false, message: '' });
    }
    return json({});
  };
}

type Page = HTMLElement & {
  updateComplete: Promise<unknown>;
  runDialogKey: string | null;
  runDialogCategory: string;
  runInProgress: boolean;
  executeRunMigration(): Promise<void>;
  executeRunOperation(): Promise<void>;
  stopPolling(): void;
};

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

let el: Page | null = null;
let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  vi.mocked(showToast).mockClear();
});

afterEach(() => {
  el?.stopPolling();
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function settle(e: Page): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await e.updateComplete;
}

async function mount(opts: Options): Promise<Page> {
  fetchMock = vi.fn(handler(opts));
  vi.stubGlobal('fetch', fetchMock);
  const e = document.createElement('scion-page-admin-maintenance') as Page;
  document.body.appendChild(e);
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
  return e;
}

function findButton(root: ParentNode, text: string): HTMLElement | undefined {
  return Array.from(root.querySelectorAll<HTMLElement>('sl-button')).find(
    (b) => b.textContent?.trim() === text
  );
}

function expectPageRendered(e: Page): void {
  const root = e.shadowRoot!;
  expect(root.querySelector('.error-state')).toBeNull();
  expect(root.textContent).not.toContain('Failed to Load');
  expect(root.querySelector('.card-list')?.textContent).toContain('Migration A');
}

function listFetchCount(): number {
  return fetchMock.mock.calls.filter(([input]) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    return new URL(url, 'http://localhost').pathname === '/api/v1/admin/maintenance/operations';
  }).length;
}

describe('admin maintenance run request errors', () => {
  it('keeps the page and toasts the server error when a migration retry fails', async () => {
    el = await mount({ runStatus: 409 });
    const root = el.shadowRoot!;

    const retry = findButton(root, 'Retry');
    expect(retry).toBeDefined();
    retry!.click();
    await settle(el);

    const dialog = root.querySelector('sl-dialog[label="Run Migration"]');
    expect(dialog).not.toBeNull();
    const confirm = dialog!.querySelector<HTMLElement>('sl-button[variant="primary"]');
    expect(confirm?.textContent).toContain('Run Migration');
    confirm!.click();
    await settle(el);

    expectPageRendered(el);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showToast).mock.calls[0][0]).toContain(SERVER_ERROR);

    // The dialog stays open and its confirm button is usable again.
    expect(el.runInProgress).toBe(false);
    expect(el.runDialogKey).toBe('mig-a');
    const confirmAfter = root.querySelector<HTMLElement & { loading?: boolean }>(
      'sl-dialog[label="Run Migration"] sl-button[variant="primary"]'
    );
    expect(confirmAfter).not.toBeNull();
    expect(confirmAfter!.hasAttribute('loading')).toBe(false);
    expect(confirmAfter!.loading ?? false).toBe(false);
  });

  it('keeps the page and toasts the server error when an operation run fails', async () => {
    el = await mount({ runStatus: 500 });
    el.runDialogKey = 'op-a';
    el.runDialogCategory = 'operation';
    await el.executeRunOperation();
    await settle(el);

    expectPageRendered(el);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showToast).mock.calls[0][0]).toContain(SERVER_ERROR);
    expect(el.runInProgress).toBe(false);
    expect(el.runDialogKey).toBe('op-a');
  });

  it('closes the dialog and reloads the list when a migration run succeeds', async () => {
    el = await mount({});
    const before = listFetchCount();
    el.runDialogKey = 'mig-a';
    el.runDialogCategory = 'migration';
    await el.executeRunMigration();
    await settle(el);

    expectPageRendered(el);
    expect(showToast).not.toHaveBeenCalled();
    expect(el.runDialogKey).toBeNull();
    expect(el.runInProgress).toBe(false);
    expect(listFetchCount()).toBeGreaterThan(before);
  });

  it('shows the Failed to Load state when the page load fails', async () => {
    el = await mount({ listStatus: 500 });
    const root = el.shadowRoot!;
    expect(root.querySelector('.error-state')?.textContent).toContain('Failed to Load');
    expect(root.querySelector('.error-details')?.textContent).toContain('list unavailable');
    expect(root.querySelector('.card-list')).toBeNull();
  });
});
