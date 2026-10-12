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

// Completed migrations on the maintenance page are grouped in one collapsible
// element, collapsed by default, with the count in its summary. Pending,
// running and failed migrations stay visible outside it.

interface Migration {
  key: string;
  title: string;
  description: string;
  status: string;
  createdAt: string;
  completedAt?: string;
}

function migration(key: string, status: string): Migration {
  return {
    key,
    title: `Title ${key}`,
    description: `Description ${key}`,
    status,
    createdAt: '2026-01-01T00:00:00Z',
    ...(status === 'completed' ? { completedAt: '2026-01-02T00:00:00Z' } : {}),
  };
}

type Page = HTMLElement & {
  updateComplete: Promise<unknown>;
  loadData: (isPolling?: boolean) => Promise<void>;
};

let current: Migration[] = [];
let delayMs = 0;
let fetchMock: ReturnType<typeof vi.fn>;

function handler(input: string | URL | Request): Promise<Response> {
  const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
  const path = new URL(url, 'http://localhost').pathname;
  const body =
    path === '/api/v1/admin/maintenance'
      ? { enabled: false, message: '' }
      : path === '/api/v1/admin/maintenance/operations'
        ? { migrations: current, operations: [] }
        : {};
  const response = new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
  return delayMs > 0
    ? new Promise((resolve) => setTimeout(() => resolve(response), delayMs))
    : Promise.resolve(response);
}

beforeAll(async () => {
  await import('./admin-maintenance.js');
}, 60_000);

let el: Page | null = null;
afterEach(() => {
  delayMs = 0;
  el?.remove();
  el = null;
  vi.unstubAllGlobals();
});

async function settle(e: Page): Promise<void> {
  await e.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 200));
  await e.updateComplete;
}

async function mount(migrations: Migration[]): Promise<Page> {
  current = migrations;
  fetchMock = vi.fn(handler);
  vi.stubGlobal('fetch', fetchMock);
  const e = document.createElement('scion-page-admin-maintenance') as Page;
  document.body.appendChild(e);
  await settle(e);
  return e;
}

// Runs a non-polling reload with a delayed response, checks that the loading
// spinner replaced the element meanwhile, then waits for the reload to finish.
async function reloadThroughSpinner(e: Page): Promise<void> {
  delayMs = 50;
  const done = e.loadData();
  await e.updateComplete;
  expect(details(e)).toBeNull();
  await done;
  delayMs = 0;
  await settle(e);
}

// The migrations section only; other sections such as quick actions also use .card.
function migrationsSection(e: Page): HTMLElement | null {
  const sections = Array.from(e.shadowRoot?.querySelectorAll<HTMLElement>('.section') ?? []);
  return (
    sections.find((s) => s.querySelector('.section-title')?.textContent?.trim() === 'Migrations') ??
    null
  );
}

function details(e: Page): HTMLElement | null {
  return e.shadowRoot?.querySelector('sl-details.completed-migrations') ?? null;
}

function titles(root: ParentNode | null | undefined): string[] {
  return Array.from(root?.querySelectorAll('.card .card-title') ?? []).map(
    (n) => n.textContent?.trim() ?? ''
  );
}

const mixed = [
  migration('a', 'completed'),
  migration('b', 'pending'),
  migration('c', 'completed'),
  migration('d', 'failed'),
  migration('e', 'running'),
];

describe('admin maintenance completed migrations', () => {
  it('groups completed migrations in one element that is collapsed by default', async () => {
    el = await mount(mixed);
    const d = details(el);
    expect(d).not.toBeNull();
    expect(el.shadowRoot?.querySelectorAll('sl-details.completed-migrations').length).toBe(1);
    expect(d?.hasAttribute('open')).toBe(false);
    expect((d as HTMLElement & { open?: boolean }).open ?? false).toBe(false);
  });

  it('shows the number of completed migrations in the summary', async () => {
    el = await mount(mixed);
    expect(details(el)?.getAttribute('summary')).toBe('Completed migrations (2)');
  });

  it('puts completed cards inside the element and the others outside it', async () => {
    el = await mount(mixed);
    const d = details(el);
    expect(titles(d)).toEqual(['Title a', 'Title c']);
    const outside = Array.from(migrationsSection(el)?.querySelectorAll('.card') ?? []).filter(
      (c) => !d?.contains(c)
    );
    expect(outside.map((c) => c.querySelector('.card-title')?.textContent?.trim())).toEqual([
      'Title b',
      'Title d',
      'Title e',
    ]);
  });

  it('renders completed cards with the same card markup as the others', async () => {
    el = await mount(mixed);
    const card = details(el)?.querySelector('.card');
    expect(card?.querySelector('.card-header .card-title')?.textContent?.trim()).toBe('Title a');
    expect(card?.querySelector('.status-badge.completed')?.textContent?.trim()).toBe('completed');
    expect(card?.querySelector('.card-description')?.textContent?.trim()).toBe('Description a');
  });

  it('keeps the original relative order of completed migrations', async () => {
    el = await mount([
      migration('z', 'completed'),
      migration('p', 'pending'),
      migration('m', 'completed'),
      migration('a', 'completed'),
    ]);
    expect(titles(details(el))).toEqual(['Title z', 'Title m', 'Title a']);
  });

  it('is not shown when there are no completed migrations', async () => {
    el = await mount([migration('b', 'pending'), migration('d', 'failed')]);
    expect(details(el)).toBeNull();
    expect(titles(migrationsSection(el))).toEqual(['Title b', 'Title d']);
  });

  it('shows only the element when every migration is completed', async () => {
    el = await mount([migration('a', 'completed'), migration('c', 'completed')]);
    const d = details(el);
    expect(d).not.toBeNull();
    expect(d?.getAttribute('summary')).toBe('Completed migrations (2)');
    expect(migrationsSection(el)?.querySelector('.empty-inline')).toBeNull();
    const outside = Array.from(migrationsSection(el)?.querySelectorAll('.card') ?? []).filter(
      (c) => !d?.contains(c)
    );
    expect(outside.length).toBe(0);
  });

  it('keeps an expanded element expanded across a poll refresh', async () => {
    el = await mount(mixed);
    const before = details(el) as HTMLElement & { open?: boolean };
    before.setAttribute('open', '');
    before.open = true;

    // A poll refresh with changed data re-renders the page.
    current = [...mixed.slice(0, 4), { ...migration('e', 'completed') }];
    await el.loadData(true);
    await settle(el);

    const after = details(el) as HTMLElement & { open?: boolean };
    expect(after).toBe(before);
    expect(after.hasAttribute('open')).toBe(true);
    expect(after.open).toBe(true);
    expect(after.getAttribute('summary')).toBe('Completed migrations (3)');
    expect(titles(after)).toEqual(['Title a', 'Title c', 'Title e']);
  });

  it('keeps the open or closed state across a reload that re-creates the element', async () => {
    el = await mount(mixed);
    const first = details(el) as HTMLElement & { open?: boolean };
    first.setAttribute('open', '');
    first.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await el.updateComplete;

    // A non-polling reload shows the loading spinner, which removes the element.
    await reloadThroughSpinner(el);

    const second = details(el);
    expect(second).not.toBeNull();
    expect(second).not.toBe(first);
    expect(second?.hasAttribute('open')).toBe(true);

    second?.removeAttribute('open');
    second?.dispatchEvent(new CustomEvent('sl-hide', { bubbles: true, composed: true }));
    await el.updateComplete;
    await reloadThroughSpinner(el);

    expect(details(el)?.hasAttribute('open')).toBe(false);
  });

  it('ignores sl-show from elements inside the cards', async () => {
    el = await mount(mixed);
    const card = details(el)?.querySelector('.card') as HTMLElement;
    card.dispatchEvent(new CustomEvent('sl-show', { bubbles: true, composed: true }));
    await el.updateComplete;
    await reloadThroughSpinner(el);

    expect(details(el)?.hasAttribute('open')).toBe(false);
  });

  it('keeps the run action working on cards outside the element', async () => {
    el = await mount(mixed);
    const d = details(el);
    const failed = Array.from(migrationsSection(el)?.querySelectorAll('.card') ?? []).find(
      (c) => c.querySelector('.card-title')?.textContent?.trim() === 'Title d'
    );
    expect(failed).toBeDefined();
    expect(d?.contains(failed as Node)).toBe(false);
    (failed?.querySelector('.card-footer sl-button') as HTMLElement).click();
    await el.updateComplete;

    const confirm = el.shadowRoot?.querySelector(
      'sl-dialog sl-button[variant="primary"]'
    ) as HTMLElement | null;
    expect(confirm).not.toBeNull();
    confirm?.click();
    await settle(el);

    const posted = fetchMock.mock.calls.some(([input, init]) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      return (
        new URL(url, 'http://localhost').pathname ===
          '/api/v1/admin/maintenance/migrations/d/run' &&
        (init as RequestInit | undefined)?.method === 'POST'
      );
    });
    expect(posted).toBe(true);
  });
});
