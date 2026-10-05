/**
 * Copyright 2026 Google LLC
 * Licensed under the Apache License, Version 2.0 (the "License");
 */

import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

// The pages must navigate via nav-click, not by importing the client entry
// module (which initialises the app and issues requests on import). Fail
// the import itself, whatever main.ts does at load time.
vi.mock('../../client/main.js', () => {
  throw new Error('access boundary pages must not import client/main');
});

const { list, get } = vi.hoisted(() => ({ list: vi.fn(), get: vi.fn() }));
vi.mock('../../client/access-boundaries-api.js', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../client/access-boundaries-api.js')>()),
  list,
  get,
}));

import { AccessBoundaryAPIError } from '../../client/access-boundaries-api.js';

const fetchSpy = vi.fn(() => Promise.reject(new Error('unexpected fetch')));

beforeAll(async () => {
  vi.stubGlobal('fetch', fetchSpy);
  await import('./admin-access-boundaries.js');
  await import('./admin-access-boundary-detail.js');
});

afterEach(() => {
  document.body.replaceChildren();
  window.history.replaceState({}, '', '/');
  list.mockReset();
  get.mockReset();
});

afterAll(() => {
  vi.unstubAllGlobals();
});

type Page = HTMLElement & { updateComplete: Promise<boolean> };

/** Mount a page and record the nav-click events that reach document. */
async function mount(tag: string, ready: (el: Page) => boolean) {
  const paths: string[] = [];
  const onNav = (e: Event) => {
    const event = e as CustomEvent<{ path: string }>;
    expect(event.bubbles).toBe(true);
    expect(event.composed).toBe(true);
    paths.push(event.detail.path);
  };
  document.addEventListener('nav-click', onNav);
  const el = document.createElement(tag) as Page;
  document.body.appendChild(el);
  await vi.waitFor(async () => {
    await el.updateComplete;
    expect(ready(el)).toBe(true);
  });
  return {
    el,
    paths,
    stop: () => document.removeEventListener('nav-click', onNav),
  };
}

function button(el: Page, label: string): HTMLElement {
  const match = Array.from(el.shadowRoot?.querySelectorAll('sl-button') ?? []).find(
    (b) => b.textContent?.trim() === label
  );
  if (!match) throw new Error(`no "${label}" button rendered`);
  return match as HTMLElement;
}

describe('admin access boundary pages', () => {
  it('load without importing client/main or making requests', () => {
    // beforeAll imported both pages; the main.js mock throws on import.
    expect(customElements.get('scion-page-admin-access-boundaries')).toBeDefined();
    expect(customElements.get('scion-page-admin-access-boundary-detail')).toBeDefined();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('inventory "Create constraint" dispatches nav-click to the create page', async () => {
    list.mockResolvedValue({
      items: [],
      totalCount: 0,
      totalCountExact: true,
      _capabilities: { actions: ['previewCreate'] },
    });
    window.history.replaceState({}, '', '/admin/access-boundaries');
    const { el, paths, stop } = await mount('scion-page-admin-access-boundaries', (page) =>
      Boolean(page.shadowRoot?.textContent?.includes('Create constraint'))
    );

    button(el, 'Create constraint').click();
    stop();

    expect(paths).toEqual(['/admin/access-boundaries/new']);
  });

  it('detail "Back to inventory" (not found) dispatches nav-click to the inventory', async () => {
    get.mockRejectedValue(
      new AccessBoundaryAPIError(404, {
        code: 'not_found',
        message: 'not found',
        retryable: false,
        correlationId: 'c-1',
      })
    );
    window.history.replaceState({}, '', '/admin/access-boundaries/missing');
    const { el, paths, stop } = await mount('scion-page-admin-access-boundary-detail', (page) =>
      Boolean(page.shadowRoot?.textContent?.includes('does not exist'))
    );

    button(el, 'Back to inventory').click();
    stop();

    expect(get).toHaveBeenCalledWith('missing');
    expect(paths).toEqual(['/admin/access-boundaries']);
  });
});
