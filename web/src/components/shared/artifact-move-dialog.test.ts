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
 * The Move dialog for an artifact whose home project was deleted: lists the
 * user's other projects, moves with PATCH {scopeRef}, and shows the hub's
 * refusal while staying open.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ScionArtifactMoveDialog } from './artifact-move-dialog.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

const ID = '00000000-0000-4000-8000-000000000009';
const ARTIFACT = { id: ID, title: 'Onboarding guide draft', scopeRef: 'gone-1' };

interface Call {
  method: string;
  url: string;
  body: unknown;
}

interface Mock {
  calls: Call[];
  /** Resolves the pending PATCH, when patch is 'hold'. */
  release: () => void;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function mockFetch(
  projects: Array<{ id: string; name?: string; slug?: string }>,
  patch: Response | 'hold'
): Mock {
  let release: () => void = () => {};
  const m: Mock = { calls: [], release: () => release() };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, opts?: RequestInit) => {
      const url = requestUrl(input);
      const method = opts?.method ?? 'GET';
      const body = typeof opts?.body === 'string' ? JSON.parse(opts.body) : undefined;
      m.calls.push({ method, url, body });
      if (url.startsWith('/api/v1/projects?')) return Promise.resolve(json({ projects }));
      if (method === 'PATCH') {
        if (patch === 'hold') {
          return new Promise<Response>((resolve) => {
            release = (): void =>
              resolve(
                json({
                  artifact: {
                    ...ARTIFACT,
                    scopeRef: 'proj-2',
                    ref: '',
                    ownerKind: 'user',
                    ownerRef: 'u',
                    scopeKind: 'project',
                    title: ARTIFACT.title,
                    currentSeq: 1,
                    createdAt: '',
                    updatedAt: '',
                  },
                })
              );
          });
        }
        return Promise.resolve(patch.clone());
      }
      return Promise.resolve(json({ error: { code: 'not_found', message: 'not found' } }, 404));
    })
  );
  return m;
}

async function settle(el: ScionArtifactMoveDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function mount(): Promise<ScionArtifactMoveDialog> {
  const el = document.createElement('scion-artifact-move-dialog') as ScionArtifactMoveDialog;
  el.artifact = ARTIFACT;
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function button(el: ScionArtifactMoveDialog, text: string): HTMLElement {
  const b = Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
    (x) => x.textContent!.trim() === text
  );
  if (!b) throw new Error(`no button ${text}`);
  return b as HTMLElement;
}

function choose(el: ScionArtifactMoveDialog, value: string): void {
  const sel = el.shadowRoot!.querySelector('sl-select') as HTMLElement & { value: string };
  sel.value = value;
  sel.dispatchEvent(new Event('sl-change'));
}

function events(el: ScionArtifactMoveDialog): { moved: unknown[]; closed: number } {
  const out = { moved: [] as unknown[], closed: 0 };
  el.addEventListener('artifact-moved', (e) => out.moved.push((e as CustomEvent).detail));
  el.addEventListener('artifact-move-closed', () => out.closed++);
  return out;
}

describe('artifact move dialog', () => {
  beforeAll(async () => {
    await import('./artifact-move-dialog.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it("lists the user's projects except the artifact's current home", async () => {
    const m = mockFetch(
      [
        { id: 'gone-1', name: 'old' },
        { id: 'proj-2', name: 'docs' },
        { id: 'proj-3', slug: 'platform' },
      ],
      json({})
    );
    const el = await mount();
    expect(m.calls[0].url).toBe('/api/v1/projects?mine=true&limit=100');
    const options = Array.from(el.shadowRoot!.querySelectorAll('sl-option')).map((o) => [
      o.getAttribute('value'),
      o.textContent!.trim(),
    ]);
    expect(options).toEqual([
      ['proj-2', 'docs'],
      ['proj-3', 'platform'],
    ]);
    expect(el.shadowRoot!.textContent).toContain('Onboarding guide draft');
    expect(button(el, 'Move').hasAttribute('disabled')).toBe(true);
  });

  it('says so when the user has no other project', async () => {
    mockFetch([{ id: 'gone-1', name: 'old' }], json({}));
    const el = await mount();
    expect(el.shadowRoot!.querySelectorAll('sl-option')).toHaveLength(0);
    expect(el.shadowRoot!.textContent).toContain('You are not a member of any other project.');
    expect(button(el, 'Move').hasAttribute('disabled')).toBe(true);
  });

  it('moves with PATCH {scopeRef} and reports the moved artifact; Cancel waits', async () => {
    const m = mockFetch([{ id: 'proj-2', name: 'docs' }], 'hold');
    const el = await mount();
    const ev = events(el);
    choose(el, 'proj-2');
    await settle(el);
    button(el, 'Move').click();
    await settle(el);
    const patch = m.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.url).toBe(`/api/v1/artifacts/${ID}`);
    expect(patch.body).toEqual({ scopeRef: 'proj-2' });
    // While the move runs, Cancel cannot be used.
    expect(button(el, 'Cancel').hasAttribute('disabled')).toBe(true);
    m.release();
    await settle(el);
    expect(ev.moved).toHaveLength(1);
    expect((ev.moved[0] as { scopeRef: string }).scopeRef).toBe('proj-2');
    expect(button(el, 'Cancel').hasAttribute('disabled')).toBe(false);
  });

  for (const [status, code, message] of [
    [409, 'home_admin_grant', 'that project holds an admin grant on the artifact'],
    [
      403,
      'cross_project_sharing_disabled',
      'moving artifacts to other projects is turned off on this hub',
    ],
    [403, 'forbidden', 'not allowed to publish into that project'],
  ] as const) {
    it(`shows the hub's ${code} refusal and stays open`, async () => {
      mockFetch([{ id: 'proj-2', name: 'docs' }], json({ error: { code, message } }, status));
      const el = await mount();
      const ev = events(el);
      choose(el, 'proj-2');
      await settle(el);
      button(el, 'Move').click();
      await settle(el);
      expect(el.shadowRoot!.querySelector('sl-alert[variant="danger"]')!.textContent).toContain(
        message
      );
      expect(ev.moved).toHaveLength(0);
      expect(ev.closed).toBe(0);
      expect(el.open).toBe(true);
      expect(button(el, 'Move').hasAttribute('disabled')).toBe(false);
    });
  }

  it('Cancel closes', async () => {
    mockFetch([{ id: 'proj-2', name: 'docs' }], json({}));
    const el = await mount();
    const ev = events(el);
    button(el, 'Cancel').click();
    expect(ev.closed).toBe(1);
  });
});
