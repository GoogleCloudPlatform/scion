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
 * The Delete artifact confirmation (ptone/scion#4535, mock state S7): what
 * the delete ends, the counts when the caller may list links and grants,
 * and the hub's refusals.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { Artifact } from '../../client/artifacts.js';
import { ArtifactDeleteError } from '../../client/artifacts.js';
import { deleteErrorMessage, type ScionArtifactDeleteDialog } from './artifact-delete-dialog.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const ID = '00000000-0000-4000-8000-0000000000d1';
const BASE = `/api/v1/artifacts/${ID}`;

const ARTIFACT: Artifact = {
  id: ID,
  ref: `scion://artifact/${ID}`,
  scopeKind: 'project',
  scopeRef: 'proj-1',
  ownerKind: 'agent',
  ownerRef: 'agent-1',
  title: 'Design notes',
  currentSeq: 4,
  createdAt: '2026-10-01T12:00:00Z',
  updatedAt: '2026-10-05T12:00:00Z',
};

function mockFetch(deleteStatus = 204): string[] {
  const calls: string[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      calls.push(`${method} ${url}`);
      const json = (body: unknown, status = 200): Promise<Response> =>
        Promise.resolve(new Response(JSON.stringify(body), { status }));
      if (url === `${BASE}/links`) {
        return json({
          links: [
            { id: 'l-1', createdAt: '', expiresAt: '2999-01-01T00:00:00Z' },
            { id: 'l-2', createdAt: '', expiresAt: '2999-01-01T00:00:00Z' },
          ],
        });
      }
      if (url === `${BASE}/grants`) {
        return json({
          grants: [
            {
              id: 'g-1',
              subjectKind: 'scope',
              subjectRef: 'proj-1',
              permission: 'read',
              home: true,
            },
            { id: 'g-2', subjectKind: 'principal', subjectRef: 'user:u', permission: 'write' },
          ],
          crossProjectSharing: false,
        });
      }
      if (url === BASE && method === 'DELETE') {
        if (deleteStatus === 204) return Promise.resolve(new Response(null, { status: 204 }));
        return json({ error: { code: 'x', message: 'hub says no' } }, deleteStatus);
      }
      return json({ error: { code: 'not_found' } }, 404);
    })
  );
  return calls;
}

async function settle(el: ScionArtifactDeleteDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(
  countsAvailable: boolean,
  versionCount: number | null = null
): Promise<ScionArtifactDeleteDialog> {
  const el = document.createElement('scion-artifact-delete-dialog') as ScionArtifactDeleteDialog;
  el.artifact = ARTIFACT;
  el.countsAvailable = countsAvailable;
  el.versionCount = versionCount;
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function text(el: ScionArtifactDeleteDialog): string {
  return el.shadowRoot!.textContent!.replace(/\s+/g, ' ');
}

function deleteButton(el: ScionArtifactDeleteDialog): HTMLElement {
  return Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
    (b) => b.textContent!.trim() === 'Delete'
  ) as HTMLElement;
}

describe('delete dialog', () => {
  beforeAll(async () => {
    await import('./artifact-delete-dialog.js');
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it('S7: with counts, says what the delete ends', async () => {
    mockFetch();
    const el = await mount(true, 4);
    expect(el.shadowRoot!.querySelector('sl-dialog')!.getAttribute('label')).toBe(
      'Delete artifact'
    );
    const t = text(el);
    expect(t).toContain('Delete Design notes? It stops being readable everywhere at once.');
    expect(t).toContain('2 share links stop working.');
    expect(t).toContain('2 grants to people and projects are removed.');
    expect(t).toContain('Messages that refer to it show it as unavailable.');
    expect(t).toContain(
      "All 4 versions are deleted; the files are removed from storage after the hub's grace period."
    );
    expect(t).toContain("The deletion is recorded in the artifact's history with your name.");
    expect(deleteButton(el).getAttribute('variant')).toBe('danger');
  });

  it('S7: without counts, reads no links or grants and leaves those lines out', async () => {
    const calls = mockFetch();
    const el = await mount(false);
    expect(calls).toEqual([]);
    expect(el.shadowRoot!.querySelector('li.links')).toBeNull();
    expect(el.shadowRoot!.querySelector('li.grants')).toBeNull();
    expect(text(el)).toContain('All its versions are deleted');
  });

  it('deletes and fires artifact-deleted', async () => {
    const calls = mockFetch();
    const el = await mount(false);
    const deleted = vi.fn();
    el.addEventListener('artifact-deleted', deleted);
    deleteButton(el).click();
    await settle(el);
    expect(calls).toContain(`DELETE ${BASE}`);
    expect(deleted).toHaveBeenCalledOnce();
  });

  it('a 403 keeps the dialog open with a fixed sentence', async () => {
    mockFetch(403);
    const el = await mount(false);
    const deleted = vi.fn();
    el.addEventListener('artifact-deleted', deleted);
    deleteButton(el).click();
    await settle(el);
    expect(deleted).not.toHaveBeenCalled();
    expect(el.shadowRoot!.querySelector('sl-alert[variant="danger"]')!.textContent).toContain(
      'You can no longer delete this artifact.'
    );
  });

  it('words each refusal', () => {
    expect(deleteErrorMessage(new ArtifactDeleteError('x', 403))).toBe(
      'You can no longer delete this artifact.'
    );
    expect(deleteErrorMessage(new ArtifactDeleteError('x', 404))).toContain(
      'already deleted or moved'
    );
    expect(deleteErrorMessage(new ArtifactDeleteError('hub broke', 500))).toBe('hub broke');
    expect(deleteErrorMessage('?')).toBe('Could not delete the artifact.');
  });
});
