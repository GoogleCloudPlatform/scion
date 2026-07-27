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
 * Tests for pre-start-hook-list.ts.
 *
 * Covers the behaviours the two page surfaces depend on:
 *  1. The list is fetched from `{apiBasePath}/pre-start-hooks` (project or hub).
 *  2. The "inherited from hub" banner appears only when no project hook is active.
 *  3. Create derives the slug from the name and POSTs to the collection URL.
 *  4. The 64 KB script limit is enforced client-side (no network call).
 *  5. Activate / delete hit the documented endpoints and refresh the list.
 *  6. `readonly` suppresses all mutating affordances.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

let ScionPreStartHookList: any;

interface RecordedCall {
  url: string;
  method: string;
  body?: Record<string, unknown>;
}

const HOOK_ACTIVE = {
  id: 'hook-1',
  scope: 'project',
  projectId: 'proj-1',
  name: 'Install tools',
  slug: 'install-tools',
  description: 'installs build tools',
  script: '#!/bin/sh\necho hi\n',
  status: 'active',
  created: '2026-07-25T10:00:00Z',
  updated: '2026-07-25T10:00:00Z',
};

const HOOK_ARCHIVED = {
  ...HOOK_ACTIVE,
  id: 'hook-2',
  name: 'Old setup',
  slug: 'old-setup',
  status: 'archived',
};

/** Installs a fetch mock that serves `hooks` on GET and 200/204 on mutations. */
function installFetch(hooks: unknown[], calls: RecordedCall[]): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const method = init?.method ?? 'GET';
      const href = String(url);
      let body: Record<string, unknown> | undefined;
      if (typeof init?.body === 'string') {
        try {
          body = JSON.parse(init.body) as Record<string, unknown>;
        } catch {
          /* ignore */
        }
      }
      calls.push({ url: href, method, body });

      if (method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify({ hooks }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }
      if (method === 'DELETE') {
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      return Promise.resolve(
        new Response(JSON.stringify(HOOK_ACTIVE), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    })
  );
}

/** Creates the element, attaches it, and lets the initial load settle. */
async function createComponent(
  hooks: unknown[],
  calls: RecordedCall[],
  apiBasePath = '/api/v1/projects/proj-1'
): Promise<any> {
  installFetch(hooks, calls);
  const el = document.createElement('scion-pre-start-hook-list') as any;
  el.setAttribute('apiBasePath', apiBasePath);
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 50));
  await el.updateComplete;
  return el;
}

function text(el: any): string {
  return el.shadowRoot?.textContent ?? '';
}

describe('scion-pre-start-hook-list', () => {
  beforeAll(async () => {
    const mod = await import('./pre-start-hook-list.js');
    ScionPreStartHookList = mod.ScionPreStartHookList;
    expect(ScionPreStartHookList).toBeDefined();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('loads hooks from the scoped collection endpoint and renders them', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);

    expect(calls[0].url).toBe('/api/v1/projects/proj-1/pre-start-hooks');
    expect(el.hooks).toHaveLength(2);
    const rendered = text(el);
    expect(rendered).toContain('Install tools');
    expect(rendered).toContain('install-tools');
    expect(rendered).toContain('archived');
  });

  it('uses the hub base path when configured for hub scope', async () => {
    const calls: RecordedCall[] = [];
    await createComponent([], calls, '/api/v1');
    expect(calls[0].url).toBe('/api/v1/pre-start-hooks');
  });

  it('shows the inherited-from-hub banner only when no project hook is active', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ARCHIVED], calls);
    el.inheritedHook = {
      id: 'hub-1',
      name: 'baseline-setup',
      slug: 'baseline-setup',
      scope: 'hub',
      status: 'active',
    };
    await el.updateComplete;
    expect(text(el)).toContain('Inherited from hub: baseline-setup');

    // Once a project hook is active the banner disappears.
    el.hooks = [HOOK_ACTIVE, HOOK_ARCHIVED];
    await el.updateComplete;
    expect(text(el)).not.toContain('Inherited from hub');
  });

  it('derives the slug from the name and POSTs to the collection endpoint', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([], calls);

    el.openCreateForm();
    el.onNameInput({ target: { value: 'Install Build Tools!' } } as unknown as Event);
    expect(el.formSlug).toBe('install-build-tools');
    el.formScript = '#!/bin/sh\necho hello\n';

    await el.handleSave();

    const post = calls.find((c) => c.method === 'POST');
    expect(post?.url).toBe('/api/v1/projects/proj-1/pre-start-hooks');
    expect(post?.body?.name).toBe('Install Build Tools!');
    expect(post?.body?.slug).toBe('install-build-tools');
    expect(post?.body?.script).toBe('#!/bin/sh\necho hello\n');
    // The list is refreshed after a successful save.
    expect(calls.filter((c) => c.method === 'GET')).toHaveLength(2);
  });

  it('keeps a hand-edited slug instead of re-deriving it from the name', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([], calls);

    el.openCreateForm();
    el.onNameInput({ target: { value: 'First name' } } as unknown as Event);
    el.onSlugInput({ target: { value: 'custom-slug' } } as unknown as Event);
    el.onNameInput({ target: { value: 'Second name' } } as unknown as Event);

    expect(el.formSlug).toBe('custom-slug');
  });

  it('rejects scripts over 64 KB client-side without calling the API', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([], calls);

    el.openCreateForm();
    el.formName = 'Too big';
    el.formScript = 'x'.repeat(64 * 1024 + 1);

    await el.handleSave();

    expect(el.formError).toContain('64 KB');
    expect(calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('PUTs updates to the hook id endpoint and does not send the slug', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE], calls);

    el.openEditForm(HOOK_ACTIVE);
    el.formName = 'Renamed';
    await el.handleSave();

    const put = calls.find((c) => c.method === 'PUT');
    expect(put?.url).toBe('/api/v1/projects/proj-1/pre-start-hooks/hook-1');
    expect(put?.body?.name).toBe('Renamed');
    expect(put?.body?.slug).toBeUndefined();
  });

  it('activates an archived hook via the activate endpoint', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);

    await el.handleActivate(HOOK_ARCHIVED);

    const activate = calls.find((c) => c.url.endsWith('/activate'));
    expect(activate?.method).toBe('POST');
    expect(activate?.url).toBe('/api/v1/projects/proj-1/pre-start-hooks/hook-2/activate');
  });

  it('deletes a hook after confirmation', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true)
    );

    await el.handleDelete(HOOK_ARCHIVED);

    const del = calls.find((c) => c.method === 'DELETE');
    expect(del?.url).toBe('/api/v1/projects/proj-1/pre-start-hooks/hook-2');
  });

  it('does not delete when the confirmation is dismissed', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);
    vi.stubGlobal(
      'confirm',
      vi.fn(() => false)
    );

    await el.handleDelete(HOOK_ARCHIVED);

    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
  });

  it('offers delete for archived hooks and for a lone active hook only', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);

    // Active hook alongside others: the hub rejects the delete, so hide it.
    expect(el.canDelete(HOOK_ACTIVE)).toBe(false);
    expect(el.canDelete(HOOK_ARCHIVED)).toBe(true);

    el.hooks = [HOOK_ACTIVE];
    expect(el.canDelete(HOOK_ACTIVE)).toBe(true);
  });

  it('hides mutating affordances in readonly mode', async () => {
    const calls: RecordedCall[] = [];
    const el = await createComponent([HOOK_ACTIVE, HOOK_ARCHIVED], calls);

    // Sanity check: the editable rendering does expose these controls.
    const editableLabels = Array.from(
      (el.shadowRoot as ShadowRoot).querySelectorAll('sl-icon-button')
    ).map((b) => (b as Element).getAttribute('label'));
    expect(editableLabels).toContain('Edit');
    expect(editableLabels).toContain('Activate');

    el.readonly = true;
    await el.updateComplete;

    const shadow = el.shadowRoot as ShadowRoot;
    const labels = Array.from(shadow.querySelectorAll('sl-icon-button')).map((b) =>
      (b as Element).getAttribute('label')
    );
    expect(labels).not.toContain('Edit');
    expect(labels).not.toContain('Activate');
    expect(labels).not.toContain('Delete');
    expect(shadow.textContent).not.toContain('Create Hook');
    // The script can still be inspected read-only.
    expect(labels).toContain('View script');
  });

  it('surfaces a load failure with a retry affordance', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(new Response('boom', { status: 500 })))
    );
    const el = document.createElement('scion-pre-start-hook-list') as any;
    el.setAttribute('apiBasePath', '/api/v1');
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 50));
    await el.updateComplete;

    expect(el.error).toBeTruthy();
    expect(text(el)).toContain('Failed to Load');
  });
});
