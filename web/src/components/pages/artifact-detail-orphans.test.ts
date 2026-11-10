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
 * Artifact page, owner-deleted state and retirement (ptone/scion#4535,
 * approved mock states S3, S4, S7 and S8): the Owner deleted badge and the
 * struck-through owner, the Expires line, Delete… exactly when the hub
 * reports canRetire, Retention… for canShare without canManage, and the
 * delete flow.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ArtifactResponse } from '../../client/artifacts.js';
import type { ScionPageArtifactDetail } from './artifact-detail.js';
import { resetPrincipalNames } from '../../client/principal-names.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const { showToast } = vi.hoisted(() => ({ showToast: vi.fn() }));
vi.mock('../../utils/toast.js', () => ({ showToast }));

const ID = '5f1c2d3e-0000-4000-8000-0000000000aa';
const AGENT = '9d0e1f2a-3b4c-4d5e-8f6a-7b8c9d0e1f2a';

function meta(
  extra: Partial<ArtifactResponse> = {},
  art: Record<string, unknown> = {}
): ArtifactResponse {
  return {
    artifact: {
      id: ID,
      ref: `scion://artifact/${ID}`,
      scopeKind: 'project',
      scopeRef: 'p-1',
      ownerKind: 'agent',
      ownerRef: AGENT,
      title: 'Design notes',
      currentSeq: 1,
      createdAt: '2026-10-05T12:00:00Z',
      updatedAt: '2026-10-05T12:00:00Z',
      ownerDeleted: true,
      ...art,
    },
    version: {
      seq: 1,
      ref: `scion://artifact/${ID}@1`,
      kind: 'publish',
      entryPath: 'notes.md',
      totalBytes: 5,
      fileCount: 1,
      createdAt: '2026-10-05T12:00:00Z',
      state: 'ready',
      files: [{ path: 'notes.md', size: 5, sha256: 'ab', mediaType: 'text/markdown' }],
    },
    ...extra,
  };
}

interface Calls {
  urls: string[];
}

/** Mocks the hub: GET answers m, DELETE answers deleteStatus. */
function mockFetch(m: ArtifactResponse, deleteStatus = 204): Calls {
  const c: Calls = { urls: [] };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      c.urls.push(method === 'GET' ? url : `${method} ${url}`);
      const json = (body: unknown, status = 200): Promise<Response> =>
        Promise.resolve(new Response(JSON.stringify(body), { status }));
      if (method === 'DELETE') {
        if (deleteStatus === 204) return Promise.resolve(new Response(null, { status: 204 }));
        return json(
          { error: { code: 'forbidden', message: 'only the owner may delete it' } },
          deleteStatus
        );
      }
      if (url.endsWith('/links')) return json({ links: [] });
      if (url.endsWith('/grants')) return json({ grants: [], crossProjectSharing: false });
      if (/\/versions(\?|$)/.test(url)) {
        return json({ versions: [{ ...m.version, files: [] }], discarded: [] });
      }
      if (url.includes('/files/')) return Promise.resolve(new Response('# Hi', { status: 200 }));
      if (url.startsWith('/api/v1/agents/')) return json({ error: { code: 'not_found' } }, 404);
      if (url.startsWith(`/api/v1/artifacts/${ID}`)) return json(m);
      return json({ error: { code: 'not_found' } }, 404);
    })
  );
  return c;
}

async function settle(el: ScionPageArtifactDetail): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(): Promise<ScionPageArtifactDetail> {
  window.__SCION_FEATURES__ = { 'hub.artifacts': true };
  const el = document.createElement('scion-page-artifact-detail') as ScionPageArtifactDetail;
  el.pageData = { path: `/projects/p-1/artifacts/${ID}` } as ScionPageArtifactDetail['pageData'];
  document.body.appendChild(el);
  await settle(el);
  return el;
}

function buttons(el: ScionPageArtifactDetail): string[] {
  return Array.from(el.shadowRoot!.querySelectorAll('.header-actions sl-button')).map((b) =>
    b.textContent!.replace(/\s+/g, ' ').trim()
  );
}

function headerButton(el: ScionPageArtifactDetail, text: string): HTMLElement {
  const b = Array.from(el.shadowRoot!.querySelectorAll('.header-actions sl-button')).find(
    (x) => x.textContent!.trim() === text
  );
  if (!b) throw new Error(`no button ${text}`);
  return b as HTMLElement;
}

function deleteDialog(el: ScionPageArtifactDetail): HTMLElement | null {
  return el.shadowRoot!.querySelector('scion-artifact-delete-dialog');
}

describe('artifact page: owner deleted and retirement', () => {
  beforeAll(async () => {
    const hd = (
      window as unknown as { happyDOM?: { settings: { disableIframePageLoading: boolean } } }
    ).happyDOM;
    if (hd) hd.settings.disableIframePageLoading = true;
    await import('./artifact-detail.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    showToast.mockReset();
    resetPrincipalNames();
  });

  it('S3: a reader sees the badge, the struck owner and Expires, and no retirement controls', async () => {
    mockFetch(meta());
    const el = await mount();
    const root = el.shadowRoot!;
    const badge = root.querySelector('sl-badge.owner-deleted');
    expect(badge).not.toBeNull();
    expect(badge!.textContent).toContain('Owner deleted');
    expect(badge!.querySelector('sl-icon')!.getAttribute('name')).toBe('person-x');
    expect(badge!.closest('sl-tooltip')!.getAttribute('content')).toContain(
      'The agent that owned this artifact was deleted.'
    );
    // The name (here the short id: the agent cannot be looked up) is struck
    // through; the (agent) tag is not.
    const gone = root.querySelector('.meta .owner-gone');
    expect(gone!.textContent).toBe('9d0e1f2a…');
    expect(gone!.parentElement!.textContent!.replace(/\s+/g, ' ')).toContain(
      'Owner: 9d0e1f2a… (agent)'
    );
    expect(badge!.getAttribute('tabindex')).toBe('0');
    expect(badge!.getAttribute('aria-label')).toContain(
      'The agent that owned this artifact was deleted.'
    );
    expect(root.querySelector('.meta .expires')!.textContent!.replace(/\s+/g, ' ')).toContain(
      'Expires: Never'
    );
    expect(buttons(el)).not.toContain('Delete…');
    expect(buttons(el)).not.toContain('Retention…');
    expect(deleteDialog(el)).toBeNull();
  });

  it('kill check: with canRetire false the retirement controls are absent', async () => {
    mockFetch(meta({ canRetire: false, canShare: false, canManage: false }));
    const el = await mount();
    expect(buttons(el)).not.toContain('Delete…');
    expect(buttons(el)).not.toContain('Retention…');
    expect(el.shadowRoot!.querySelector('.header-actions sl-button.delete')).toBeNull();
    expect(el.shadowRoot!.querySelector('.header-actions sl-button.retention')).toBeNull();
    expect(deleteDialog(el)).toBeNull();
    expect(el.shadowRoot!.querySelector('scion-artifact-share-dialog.retention-dialog')).toBeNull();
  });

  it('kill check: canShare without canRetire never shows Delete…', async () => {
    mockFetch(meta({ canShare: true, canRetire: false }));
    const el = await mount();
    expect(buttons(el)).not.toContain('Delete…');
    expect(deleteDialog(el)).toBeNull();
  });

  it('S4: a home-project admin (canRetire only) gets Delete… and no Share, Move or Retention', async () => {
    mockFetch(meta({ canRetire: true }));
    const el = await mount();
    const b = buttons(el);
    expect(b).toContain('Delete…');
    expect(b).not.toContain('Share');
    expect(b).not.toContain('Move…');
    expect(b).not.toContain('Retention…');
    const del = headerButton(el, 'Delete…');
    expect(del.getAttribute('variant')).toBe('danger');
    expect(del.hasAttribute('outline')).toBe(true);
  });

  it('S4: a steward (canShare without canManage) gets Retention… and Delete…, not Share', async () => {
    mockFetch(meta({ canShare: true, shareAuthority: 'project_owner', canRetire: true }));
    const el = await mount();
    const b = buttons(el);
    expect(b).toContain('Retention…');
    expect(b).toContain('Delete…');
    expect(b).not.toContain('Share');
    headerButton(el, 'Retention…').click();
    await settle(el);
    const dialog = el.shadowRoot!.querySelector(
      'scion-artifact-share-dialog.retention-dialog'
    ) as HTMLElement & {
      mode: string;
      open: boolean;
    };
    expect(dialog.mode).toBe('retention');
    expect((dialog as unknown as { shareAuthority: string }).shareAuthority).toBe('project_owner');
    expect(dialog.open).toBe(true);
  });

  it('S4: an owner keeps Share and also gets Delete…, without Retention…', async () => {
    mockFetch(meta({ canManage: true, canShare: true, canRetire: true }, { ownerDeleted: false }));
    const el = await mount();
    const b = buttons(el);
    expect(b).toContain('Share');
    expect(b).toContain('Delete…');
    expect(b).not.toContain('Retention…');
    expect(el.shadowRoot!.querySelector('sl-badge.owner-deleted')).toBeNull();
    expect(el.shadowRoot!.querySelector('.meta .owner-gone')).toBeNull();
  });

  it('S3: Expires shows the expiry when one is set', async () => {
    mockFetch(meta({}, { expiresAt: '2026-11-13T09:30:00Z' }));
    const el = await mount();
    const text = el.shadowRoot!.querySelector('.meta .expires')!.textContent!;
    expect(text).toContain('Expires:');
    expect(text).not.toContain('Never');
  });

  it('S7/S8: Delete… confirms, deletes, toasts and goes back to the project', async () => {
    const calls = mockFetch(meta({ canRetire: true }));
    const el = await mount();
    const nav: string[] = [];
    const onNav = (e: Event): void => {
      nav.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      headerButton(el, 'Delete…').click();
      await settle(el);
      const dialog = deleteDialog(el) as HTMLElement & { open: boolean };
      expect(dialog.open).toBe(true);
      const confirm = Array.from(dialog.shadowRoot!.querySelectorAll('sl-button')).find(
        (b) => b.textContent!.trim() === 'Delete'
      ) as HTMLElement;
      confirm.click();
      await settle(el);
      expect(calls.urls).toContain(`DELETE /api/v1/artifacts/${ID}`);
      expect(showToast).toHaveBeenCalledWith('Deleted “Design notes”.', 'success');
      expect(nav).toEqual(['/projects/p-1']);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
  });

  it('S8: after deleting an artifact whose home project was deleted, goes to the artifacts list', async () => {
    mockFetch(meta({ canRetire: true, scopeDeleted: true }));
    const el = await mount();
    const nav: string[] = [];
    const onNav = (e: Event): void => {
      nav.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      headerButton(el, 'Delete…').click();
      await settle(el);
      const dialog = deleteDialog(el)!;
      (
        Array.from(dialog.shadowRoot!.querySelectorAll('sl-button')).find(
          (b) => b.textContent!.trim() === 'Delete'
        ) as HTMLElement
      ).click();
      await settle(el);
      expect(nav).toEqual(['/artifacts']);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
  });

  it('S7: a refused delete keeps the dialog open with the reason, and stays on the page', async () => {
    mockFetch(meta({ canRetire: true }), 403);
    const el = await mount();
    const nav: string[] = [];
    const onNav = (e: Event): void => {
      nav.push((e as CustomEvent<{ path: string }>).detail.path);
    };
    document.addEventListener('nav-click', onNav);
    try {
      headerButton(el, 'Delete…').click();
      await settle(el);
      const dialog = deleteDialog(el) as HTMLElement & { open: boolean };
      (
        Array.from(dialog.shadowRoot!.querySelectorAll('sl-button')).find(
          (b) => b.textContent!.trim() === 'Delete'
        ) as HTMLElement
      ).click();
      await settle(el);
      expect(dialog.open).toBe(true);
      expect(dialog.shadowRoot!.querySelector('sl-alert[variant="danger"]')!.textContent).toContain(
        'You can no longer delete this artifact.'
      );
      expect(showToast).not.toHaveBeenCalled();
      expect(nav).toEqual([]);
    } finally {
      document.removeEventListener('nav-click', onNav);
    }
  });
});
