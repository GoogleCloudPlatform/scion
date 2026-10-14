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

const TOGGLE_ERROR = 'settings store is read-only';

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
  /** Status for PUT /api/v1/admin/maintenance. */
  toggleStatus?: number;
  /** Saved maintenance state that GET returns after a failed PUT. */
  enabledAfterFailedToggle?: boolean;
}

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

function pathOf(input: string | URL | Request): string {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  return new URL(url, 'http://localhost').pathname;
}

function handler(opts: Options) {
  let savedEnabled = false;
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = pathOf(input);
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
    if (path === '/api/v1/admin/maintenance' && method === 'PUT') {
      const status = opts.toggleStatus ?? 200;
      if (status >= 400) {
        savedEnabled = opts.enabledAfterFailedToggle ?? savedEnabled;
        return json({ error: { code: 'internal', message: TOGGLE_ERROR } }, status);
      }
      const { enabled } = JSON.parse(init?.body as string) as { enabled: boolean };
      savedEnabled = enabled;
      return json({ enabled, message: '' });
    }
    if (path === '/api/v1/admin/maintenance') {
      return json({ enabled: savedEnabled, message: '' });
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
  const e = document.createElement('scion-page-admin-maintenance') as unknown as Page;
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

/** The Migrations section; other sections also render .card-list elements. */
function migrationsSection(e: Page): HTMLElement | null {
  const sections = Array.from(e.shadowRoot?.querySelectorAll<HTMLElement>('.section') ?? []);
  return (
    sections.find((s) => s.querySelector('.section-title')?.textContent?.trim() === 'Migrations') ??
    null
  );
}

function expectPageRendered(e: Page): void {
  const root = e.shadowRoot!;
  expect(root.querySelector('.error-state')).toBeNull();
  expect(root.textContent).not.toContain('Failed to Load');
  const titles = Array.from(migrationsSection(e)?.querySelectorAll('.card-title') ?? []).map((t) =>
    t.textContent?.trim()
  );
  expect(titles).toContain('Migration A');
}

function listFetchCount(): number {
  return fetchMock.mock.calls.filter(
    (call) => pathOf(call[0] as string | URL | Request) === '/api/v1/admin/maintenance/operations'
  ).length;
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
    expect(migrationsSection(el)).toBeNull();
  });
});

describe('admin maintenance mode toggle errors', () => {
  function toggleState(e: Page): { active: boolean; label: string } {
    const root = e.shadowRoot!;
    return {
      active: root.querySelector('.toggle-track')!.classList.contains('active'),
      label: root.querySelector('.toggle-label')!.textContent!.trim(),
    };
  }

  it('keeps the page, toasts the server error and shows the saved state when the toggle fails', async () => {
    // The request fails but the server saved the change, so the re-read
    // returns enabled and the toggle shows it.
    el = await mount({ toggleStatus: 500, enabledAfterFailedToggle: true });
    expect(toggleState(el)).toEqual({ active: false, label: 'Disabled' });

    el.shadowRoot!.querySelector<HTMLElement>('.toggle-track')!.click();
    await settle(el);

    expectPageRendered(el);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(vi.mocked(showToast).mock.calls[0][0]).toContain(TOGGLE_ERROR);
    expect(toggleState(el)).toEqual({ active: true, label: 'Enabled' });
  });

  it('shows the new state when the toggle succeeds', async () => {
    el = await mount({});
    el.shadowRoot!.querySelector<HTMLElement>('.toggle-track')!.click();
    await settle(el);

    expect(showToast).not.toHaveBeenCalled();
    expect(toggleState(el)).toEqual({ active: true, label: 'Enabled' });
  });
});
