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
import type { ScionResourceList } from './resource-list.js';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(el: ScionResourceList): Promise<void> {
  for (let i = 0; i < 4; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

const ITEMS = [
  { id: 'a', name: 'a', _capabilities: { actions: ['read', 'update', 'delete'] } },
  { id: 'b', name: 'b', _capabilities: { actions: ['read'] } },
  { id: 'c', name: 'c' },
];

async function createList(props: Partial<ScionResourceList>): Promise<ScionResourceList> {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse({ templates: ITEMS })))
  );
  const el = document.createElement('scion-resource-list');
  el.kind = 'template';
  el.scope = 'global';
  Object.assign(el, props);
  document.body.appendChild(el);
  await flush(el);
  return el;
}

/** Menu item labels in each row, keyed by the row's resource name. */
function rowActions(el: ScionResourceList): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const row of Array.from(el.shadowRoot?.querySelectorAll('.resource-row') ?? [])) {
    const name = row.querySelector('.resource-name')?.textContent?.trim() ?? '';
    out[name] = Array.from(row.querySelectorAll('sl-menu-item')).map((m) =>
      (m.textContent ?? '').trim()
    );
  }
  return out;
}

describe('resource list: per-record capabilities', () => {
  let element: ScionResourceList | null = null;

  beforeAll(async () => {
    await import('./resource-list.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('offers Delete only on records whose capabilities include delete', async () => {
    element = await createList({ canDelete: true, recordCapabilities: true });
    expect(rowActions(element)).toEqual({ a: ['Delete'], b: [], c: [] });
    // Rows without any action render as plain links, with no menu.
    const menus = element.shadowRoot?.querySelectorAll('sl-dropdown') ?? [];
    expect(menus.length).toBe(1);
  });

  it('keeps Clone list-level while gating Delete per record', async () => {
    element = await createList({ canClone: true, canDelete: true, recordCapabilities: true });
    expect(rowActions(element)).toEqual({
      a: ['Clone', 'Delete'],
      b: ['Clone'],
      c: ['Clone'],
    });
  });

  it('ignores record capabilities unless recordCapabilities is set', async () => {
    element = await createList({ canDelete: true });
    expect(rowActions(element)).toEqual({ a: ['Delete'], b: ['Delete'], c: ['Delete'] });
  });
});
