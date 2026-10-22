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
 * Publish dialog with chooseProject (the hub Artifacts page, ptone/scion#5477):
 * the Project picker lists only the projects where the user may publish,
 * preselects the global project or the only choice, and the new artifact is
 * homed in the picked project.
 */

import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest';

import type { ScionArtifactPublishDialog } from './artifact-publish-dialog.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';

interface Project {
  id: string;
  name: string;
  slug: string;
}

const GLOBAL: Project = { id: 'g-1', name: 'Global', slug: 'global' };
const WEB: Project = { id: 'w-1', name: 'Web frontend', slug: 'web-frontend' };
const BILLING: Project = { id: 'b-1', name: 'Billing', slug: 'billing' };
const RELEASE: Project = { id: 'r-1', name: 'Release tools', slug: 'release-tools' };

interface Mock {
  calls: string[];
  posted: Array<Record<string, unknown>>;
}

/**
 * Mocks fetch: the caller's projects (or projectsStatus), canCreate per
 * project id in publishable, and the create, upload and finalize calls of a
 * publish.
 */
function mockFetch(projects: Project[], publishable: Set<string>, projectsStatus = 200): Mock {
  const m: Mock = { calls: [], posted: [] };
  const json = (body: unknown, status = 200): Response =>
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = requestUrl(input);
      const method = init?.method ?? 'GET';
      m.calls.push(`${method} ${url}`);
      if (url.startsWith('/api/v1/projects?')) {
        if (projectsStatus !== 200) {
          return Promise.resolve(
            json({ error: { code: 'internal', message: 'boom' } }, projectsStatus)
          );
        }
        return Promise.resolve(json({ projects }));
      }
      if (method === 'GET' && url.startsWith('/api/v1/artifacts?')) {
        const scope = new URL(url, 'http://x').searchParams.get('scope') ?? '';
        return Promise.resolve(json({ artifacts: [], canCreate: publishable.has(scope) }));
      }
      if (method === 'POST' && url === '/api/v1/artifacts') {
        m.posted.push(
          JSON.parse((init?.body as string | undefined) ?? '{}') as Record<string, unknown>
        );
        return Promise.resolve(
          json(
            { artifact: { id: 'n-1' }, version: { seq: 1 }, upload: { required: ['a.md'] } },
            201
          )
        );
      }
      if (method === 'PUT') return Promise.resolve(new Response(null, { status: 204 }));
      return Promise.resolve(
        json({ artifact: { id: 'n-1', scopeRef: 'w-1' }, version: { seq: 1 } })
      );
    })
  );
  return m;
}

async function settle(el: ScionArtifactPublishDialog): Promise<void> {
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;
  }
}

async function openDialog(chooseProject = true): Promise<ScionArtifactPublishDialog> {
  const el = document.createElement('scion-artifact-publish-dialog') as ScionArtifactPublishDialog;
  el.chooseProject = chooseProject;
  if (!chooseProject) el.projectId = 'p-fixed';
  document.body.appendChild(el);
  el.open = true;
  await settle(el);
  return el;
}

function picker(el: ScionArtifactPublishDialog): HTMLElement & { value: string } {
  return el.shadowRoot!.querySelector('sl-select.project-select') as HTMLElement & {
    value: string;
  };
}

/** Shoelace is not registered here: read the attributes Lit sets. */
function disabled(e: Element): boolean {
  return e.hasAttribute('disabled');
}

function optionIds(el: ScionArtifactPublishDialog): string[] {
  return Array.from(picker(el).querySelectorAll('sl-option')).map(
    (o) => o.getAttribute('value') ?? ''
  );
}

function publishButton(el: ScionArtifactPublishDialog): HTMLElement {
  return Array.from(el.shadowRoot!.querySelectorAll('sl-button')).find(
    (b) => b.textContent!.trim() === 'Publish'
  ) as HTMLElement;
}

/** Picks a file and title, as the user would. */
async function fillFile(el: ScionArtifactPublishDialog): Promise<void> {
  const priv = el as unknown as {
    picked: { path: string; file: File }[];
    entry: string;
    titleValue: string;
  };
  priv.picked = [{ path: 'a.md', file: new File(['x'], 'a.md') }];
  priv.entry = 'a.md';
  priv.titleValue = 'Notes';
  await el.updateComplete;
}

describe('publish dialog project picker', () => {
  beforeAll(async () => {
    await import('./artifact-publish-dialog.js');
  }, 30_000);

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
  });

  it('lists only publishable projects, global first, and preselects global', async () => {
    mockFetch([WEB, BILLING, GLOBAL, RELEASE], new Set(['g-1', 'w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['g-1', 'r-1', 'w-1']);
    expect(picker(el).value).toBe('g-1');
    expect(disabled(picker(el))).toBe(false);
  });

  it('preselects nothing when there is no global project and several choices', async () => {
    mockFetch([WEB, RELEASE], new Set(['w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['r-1', 'w-1']);
    expect(picker(el).value).toBe('');
    await fillFile(el);
    // A file and a title are not enough without a project.
    expect(disabled(publishButton(el))).toBe(true);
  });

  it('preselects nothing when the user may not publish in the global project', async () => {
    mockFetch([GLOBAL, WEB, RELEASE], new Set(['w-1', 'r-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['r-1', 'w-1']);
    expect(picker(el).value).toBe('');
  });

  it('preselects the only publishable project', async () => {
    mockFetch([BILLING, WEB], new Set(['w-1']));
    const el = await openDialog();
    expect(optionIds(el)).toEqual(['w-1']);
    expect(picker(el).value).toBe('w-1');
  });

  it('disables the picker and says so when there is nowhere to publish', async () => {
    mockFetch([BILLING], new Set());
    const el = await openDialog();
    expect(optionIds(el)).toEqual([]);
    expect(disabled(picker(el))).toBe(true);
    expect(picker(el).getAttribute('help-text')).toContain("can't publish");
  });

  it('shows a failed project load with Retry, and Retry loads again', async () => {
    mockFetch([WEB], new Set(['w-1']), 500);
    const el = await openDialog();
    const alert = el.shadowRoot!.querySelector('.project sl-alert')!;
    expect(alert.textContent).toContain('Could not load your projects');
    expect(picker(el)).toBeNull();

    mockFetch([WEB], new Set(['w-1']));
    (alert.querySelector('sl-button') as HTMLElement).click();
    await settle(el);
    expect(optionIds(el)).toEqual(['w-1']);
  });

  it('homes the new artifact in the picked project', async () => {
    const m = mockFetch([GLOBAL, WEB], new Set(['g-1', 'w-1']));
    const el = await openDialog();
    const select = picker(el);
    select.value = 'w-1';
    select.dispatchEvent(new Event('sl-change'));
    await fillFile(el);
    const published = vi.fn();
    el.addEventListener('artifact-published', published);
    expect(disabled(publishButton(el))).toBe(false);
    publishButton(el).click();
    await settle(el);
    expect(m.posted).toHaveLength(1);
    expect(m.posted[0].scope).toBe('w-1');
    expect(published).toHaveBeenCalledTimes(1);
  });

  it('without chooseProject shows no picker and loads no projects', async () => {
    const m = mockFetch([GLOBAL], new Set(['g-1']));
    const el = await openDialog(false);
    expect(picker(el)).toBeNull();
    expect(m.calls).toEqual([]);
  });
});
