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
 * The global Templates tab on the Hub Resources page gates its import and
 * clone controls on the `create` capability the hub reports on the
 * scope=global templates list, as it does "Refresh All from Source" (a
 * global reimport is authorized as template.create_global), and its delete
 * control on each record's own capabilities (ptone/scion#1713).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import type { ScionPageSettings } from './settings.js';
import type { ScionResourceImport } from '../shared/resource-import.js';
import type { ScionResourceList } from '../shared/resource-list.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(el: HTMLElement & { updateComplete: Promise<boolean> }): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

/**
 * Mount the settings page on the Templates tab. globalList is the body of
 * GET /api/v1/templates?scope=global (or a status code for a failure).
 */
async function mountSettings(globalList: Record<string, unknown> | number): Promise<{
  el: ScionPageSettings;
  fetchMock: ReturnType<typeof vi.fn>;
}> {
  window.history.replaceState({}, '', '/settings?tab=templates');
  const fetchMock = vi.fn((input: string) => {
    const url = String(input);
    if (url.includes('/api/v1/auth/admin-status')) {
      return Promise.resolve(
        jsonResponse({
          isAdmin: false,
          isSuperAdmin: false,
          permissions: ['template.list', 'template.read'],
        })
      );
    }
    if (url.includes('/api/v1/templates')) {
      if (typeof globalList === 'number') {
        return Promise.resolve(jsonResponse({ error: 'nope' }, globalList));
      }
      return Promise.resolve(jsonResponse(globalList));
    }
    return Promise.resolve(jsonResponse({}));
  });
  vi.stubGlobal('fetch', fetchMock);
  const el = document.createElement('scion-page-settings') as ScionPageSettings;
  document.body.appendChild(el);
  await flush(el);
  return { el, fetchMock };
}

function templatesImport(el: HTMLElement): ScionResourceImport {
  const imp = el.shadowRoot?.querySelector<ScionResourceImport>(
    'sl-tab-panel[name="templates"] scion-resource-import'
  );
  if (!imp) throw new Error('templates import not rendered');
  return imp;
}

function templatesList(el: HTMLElement): ScionResourceList {
  const list = el.shadowRoot?.querySelector<ScionResourceList>('#templates-list');
  if (!list) throw new Error('templates list not rendered');
  return list;
}

describe('settings global Templates tab capability gating', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    window.history.replaceState({}, '', '/');
  });

  it('enables import and clone when the global list reports create', async () => {
    const { el, fetchMock } = await mountSettings({
      templates: [],
      _capabilities: { actions: ['create'] },
    });
    element = el;
    expect(templatesImport(el).canImport).toBe(true);
    const list = templatesList(el);
    expect(list.canClone).toBe(true);
    expect(list.hideRefresh).toBe(false);
    expect(list.canDelete).toBe(true);
    expect(list.recordCapabilities).toBe(true);
    const capCall = fetchMock.mock.calls
      .map(([u]) => String(u))
      .find((u) => u.includes('/api/v1/templates?') && u.includes('limit=1'));
    expect(capCall).toContain('scope=global');
  });

  it('hides import and clone when the global list does not report create', async () => {
    const { el } = await mountSettings({ templates: [], _capabilities: { actions: [] } });
    element = el;
    expect(templatesImport(el).canImport).toBe(false);
    const list = templatesList(el);
    expect(list.canClone).toBe(false);
    expect(list.hideRefresh).toBe(true);
    // Delete stays gated per record.
    expect(list.recordCapabilities).toBe(true);
  });

  it('fails closed when the capability request fails', async () => {
    const { el } = await mountSettings(500);
    element = el;
    expect(templatesImport(el).canImport).toBe(false);
    expect(templatesList(el).canClone).toBe(false);
    expect(templatesList(el).hideRefresh).toBe(true);
  });
});
